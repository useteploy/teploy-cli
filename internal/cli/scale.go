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
	targets := make([]multideploy.ServerTarget, count)
	for i := 0; i < count; i++ {
		srv := appServers[names[i]]
		user := srv.User
		if user == "" {
			user = "root"
		}
		targets[i] = multideploy.ServerTarget{
			Name: names[i],
			Host: srv.Host,
			User: user,
			Role: srv.Role,
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
		return deploySingleServer(ctx, appCfg, target, out)
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
func deploySingleServer(ctx context.Context, appCfg *config.AppConfig, target multideploy.ServerTarget, out io.Writer) error {
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
	deployer := newSingleServerDeployer(executor, out, target.Key)
	return deployer.deployApp(ctx, appCfg, target.Tags)
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

		executor, err := ssh.Connect(ctx, ssh.ConnectConfig{
			Host:    srv.Host,
			User:    user,
			KeyPath: flags.Key,
		})
		if err != nil {
			return fmt.Errorf("connecting to LB %s: %w", name, err)
		}

		// Upload + reference the app's custom TLS cert on this LB host so the
		// load balancer terminates HTTPS the same way the app servers do.
		var tls caddy.TLS
		if appCfg.TLS != nil {
			cert, key, upErr := uploadAppTLS(ctx, executor, appCfg.App, appCfg.TLS)
			if upErr != nil {
				executor.Close()
				return fmt.Errorf("uploading TLS to LB %s: %w", name, upErr)
			}
			tls = caddy.TLS{Cert: cert, Key: key}
		}

		client := caddy.NewClient(executor)
		err = client.SetLoadBalancer(ctx, appCfg.App, appCfg.Domain, upstreams, tls, appCfg.CaddyExtra)
		executor.Close()
		if err != nil {
			return fmt.Errorf("updating LB %s: %w", name, err)
		}

		fmt.Printf("  LB %s updated with %d upstreams\n", name, len(upstreams))
	}

	return nil
}
