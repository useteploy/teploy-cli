package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strconv"

	"github.com/spf13/cobra"
	"github.com/useteploy/teploy/internal/caddy"
	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/deploy"
	"github.com/useteploy/teploy/internal/multideploy"
	"github.com/useteploy/teploy/internal/ssh"
)

func newScaleCmd(flags *Flags) *cobra.Command {
	var parallel int

	cmd := &cobra.Command{
		Use:   "scale <count>",
		Short: "Deploy app to N app-role servers and update load balancer",
		Long:  "Deploys the app to the specified number of app-role servers from servers.yml, then updates the LB server's Caddy upstream list.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			count, err := strconv.Atoi(args[0])
			if err != nil || count < 1 {
				return fmt.Errorf("count must be a positive integer (got %q)", args[0])
			}
			return runScale(flags, count, parallel)
		},
	}

	cmd.Flags().IntVar(&parallel, "parallel", 0, "max concurrent deploys (default: from teploy.yml or 1)")

	return cmd
}

func runScale(flags *Flags, count, parallel int) error {
	// 1. Load app config.
	appCfg, err := config.LoadApp(".")
	if err != nil {
		return err
	}

	// 2. Get app-role servers.
	serversPath, err := config.DefaultServersPath()
	if err != nil {
		return fmt.Errorf("determining servers path: %w", err)
	}

	appServers, err := config.GetServersByRole(serversPath, "app")
	if err != nil {
		return fmt.Errorf("listing app servers: %w", err)
	}

	if len(appServers) == 0 {
		return fmt.Errorf("no servers with role 'app' found — use 'teploy server add <name> <host> --role app'")
	}

	if count > len(appServers) {
		return fmt.Errorf("requested %d servers but only %d app servers available", count, len(appServers))
	}

	// Sort names for deterministic selection.
	names := make([]string, 0, len(appServers))
	for name := range appServers {
		names = append(names, name)
	}
	sort.Strings(names)

	// Select the first `count` servers.
	//
	// Resolve each through config.ResolveServer (matching runMultiDeploy's
	// pattern) instead of hand-building the target from the Server struct
	// directly — the hand-built version never resolved a key at all,
	// meaning --key/TEPLOY_SSH_KEY could never apply to `teploy scale`,
	// only the hardcoded ~/.ssh/id_ed25519 default path. Also thread
	// through Tags (per-host env vars) — deployApp already expects
	// target.Tags to be populated (scale.go's deploySingleServer passes it
	// straight through), but the old code left it nil, silently dropping
	// tag-based env injection for every `teploy scale` deploy. Both found
	// live while testing scale against an isolated servers.yml.
	targets := make([]multideploy.ServerTarget, count)
	for i := 0; i < count; i++ {
		srv := appServers[names[i]]
		host, user, key, err := config.ResolveServer(names[i], flags.Host, flags.User, flags.Key)
		if err != nil {
			return fmt.Errorf("resolving server %s: %w", names[i], err)
		}
		targets[i] = multideploy.ServerTarget{
			Name: names[i],
			Host: host,
			User: user,
			Key:  key,
			Role: srv.Role,
			Tags: srv.Tags,
		}
	}

	// Resolve parallel setting.
	if parallel <= 0 {
		parallel = appCfg.Parallel
	}
	if parallel <= 0 {
		parallel = 1
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	fmt.Printf("Scaling %s to %d servers (parallel=%d)...\n", appCfg.App, count, parallel)

	// 3. Run parallel deploys.
	results := multideploy.ParallelDeploy(ctx, targets, parallel, func(ctx context.Context, target multideploy.ServerTarget, out io.Writer) error {
		// scale doesn't expose --migrate-volumes; detect + abort on mismatch (safe default).
		return deploySingleServer(ctx, appCfg, target, out, false)
	}, os.Stdout)

	// 4. Print results summary.
	fmt.Print(multideploy.FormatResults(results))

	// 5. Check for any failures.
	var failCount int
	var successTargets []multideploy.ServerTarget
	for i, r := range results {
		if !r.Success {
			failCount++
		} else {
			successTargets = append(successTargets, targets[i])
		}
	}

	// 6. Update LB if any servers succeeded.
	if len(successTargets) > 0 {
		if err := updateLoadBalancer(ctx, flags, appCfg, serversPath, successTargets); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: LB update failed: %v\n", err)
		}
	}

	if failCount > 0 {
		return fmt.Errorf("%d of %d servers failed", failCount, count)
	}

	return nil
}

// deploySingleServer connects to a single server and runs the existing deploy flow.
// This is a simplified version that runs the core deploy.Deploy for a single target.
func deploySingleServer(ctx context.Context, appCfg *config.AppConfig, target multideploy.ServerTarget, out io.Writer, migrateVolumes bool) error {
	fmt.Fprintf(out, "Connecting to %s@%s...\n", target.User, target.Host)

	executor, err := ssh.Connect(ctx, ssh.ConnectConfig{
		Host:    target.Host,
		User:    target.User,
		KeyPath: target.Key,
	})
	if err != nil {
		return fmt.Errorf("connecting to %s: %w", target.Name, err)
	}
	defer executor.Close()

	fmt.Fprintf(out, "Connected to %s\n", target.Name)

	// Use the existing single-server deploy orchestration.
	deployer := newSingleServerDeployer(executor, out, target.Key, migrateVolumes)
	return deployer.deployApp(ctx, appCfg, target.Tags)
}

// rollbackSingleServer connects to one server and rolls it back to its
// previous version. Used by runMultiDeploy's rollback-all-on-partial-
// failure path (see internal/multideploy: it reuses ParallelDeploy itself
// to run this across every server that deployed successfully, so the
// fleet ends up consistent instead of split-brain).
func rollbackSingleServer(ctx context.Context, appCfg *config.AppConfig, target multideploy.ServerTarget, out io.Writer) error {
	fmt.Fprintf(out, "Connecting to %s@%s...\n", target.User, target.Host)

	executor, err := ssh.Connect(ctx, ssh.ConnectConfig{
		Host:    target.Host,
		User:    target.User,
		KeyPath: target.Key,
	})
	if err != nil {
		return fmt.Errorf("connecting to %s: %w", target.Name, err)
	}
	defer executor.Close()

	// Preserve custom TLS termination across rollback, same as the
	// interactive `teploy rollback` path (internal/cli/rollback.go).
	tlsCert, tlsKey, tlsInternal, err := resolveAppTLS(ctx, executor, appCfg)
	if err != nil {
		return err
	}

	return deploy.Rollback(ctx, executor, out, deploy.RollbackConfig{
		App:         appCfg.App,
		Domain:      appCfg.Domain,
		StopTimeout: appCfg.StopTimeout,
		TLSCert:     tlsCert,
		TLSKey:      tlsKey,
		TLSInternal: tlsInternal,
		CaddyExtra:  appCfg.CaddyExtra,
		Firewall:    caddyFirewall(appCfg.Firewall),
		Ingress:     appCfg.Ingress,
	})
}

// updateLoadBalancer updates the LB server's Caddy upstream list with the successful targets.
func updateLoadBalancer(ctx context.Context, flags *Flags, appCfg *config.AppConfig, serversPath string, targets []multideploy.ServerTarget) error {
	lbServers, err := config.GetServersByRole(serversPath, "lb")
	if err != nil {
		return fmt.Errorf("listing LB servers: %w", err)
	}

	if len(lbServers) == 0 {
		// No LB configured — skip.
		return nil
	}

	// Build upstream list from successful targets.
	upstreams := make([]caddy.Upstream, len(targets))
	for i, t := range targets {
		// Default HTTP port for app containers.
		upstreams[i] = caddy.Upstream{Dial: fmt.Sprintf("%s:80", t.Host)}
	}

	// Update each LB server.
	for name, srv := range lbServers {
		user := srv.User
		if user == "" {
			user = "root"
		}

		fmt.Printf("Updating LB %s (%s)...\n", name, srv.Host)

		keyPath := flags.Key
		if keyPath == "" {
			keyPath = os.Getenv("TEPLOY_SSH_KEY")
		}
		executor, err := ssh.Connect(ctx, ssh.ConnectConfig{
			Host:    srv.Host,
			User:    user,
			KeyPath: keyPath,
		})
		if err != nil {
			return fmt.Errorf("connecting to LB %s: %w", name, err)
		}

		// Upload + reference the app's custom TLS cert on this LB host so the
		// load balancer terminates HTTPS the same way the app servers do.
		cert, key, internal, tlsErr := resolveAppTLS(ctx, executor, appCfg)
		if tlsErr != nil {
			executor.Close()
			return fmt.Errorf("uploading TLS to LB %s: %w", name, tlsErr)
		}
		tls := caddy.TLS{Cert: cert, Key: key, Internal: internal}

		client := caddy.NewClient(executor)
		err = client.SetLoadBalancer(ctx, appCfg.App, appCfg.Domain, upstreams, tls, appCfg.CaddyExtra, caddyFirewall(appCfg.Firewall))
		executor.Close()
		if err != nil {
			return fmt.Errorf("updating LB %s: %w", name, err)
		}

		fmt.Printf("  LB %s updated with %d upstreams\n", name, len(upstreams))
	}

	return nil
}
