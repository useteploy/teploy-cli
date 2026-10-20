package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"

	"github.com/spf13/cobra"
	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/deploy"
	"github.com/useteploy/teploy/internal/notify"
	"github.com/useteploy/teploy/internal/ssh"
	"github.com/useteploy/teploy/internal/state"
)

func newRollbackCmd(flags *Flags) *cobra.Command {
	var appName string
	var toHash string

	cmd := &cobra.Command{
		Use:   "rollback",
		Short: "Roll back to the previous deploy",
		Long: `Start the target version's containers (or, for type:static apps, flip the
release symlink), health check, re-route traffic, and stop the current containers.

--to <hash> rolls back to a specific version instead of just the immediately
previous one — for containers, whatever version's containers are still on the
server (see keep_versions retention, and ` + "`teploy releases`" + ` for what is
still available); for type:static apps, a specific retained release (see
keep_releases). Without --to, the previous deploy is used.

--json prints the machine result {app, server, from, to, ok} on stdout;
progress goes to stderr and failures keep the error-envelope convention.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
			defer cancel()
			// Under --json, stdout carries only the result document; the
			// engine's progress goes to stderr.
			out := io.Writer(os.Stdout)
			if flags.JSON {
				out = os.Stderr
			}
			var err error
			var serverName, app string
			var from, to string
			if appName != "" {
				app = appName
				serverName = flags.Host
				from, to, err = runRollbackByApp(ctx, flags, appName, toHash, out)
			} else {
				var appCfg *config.AppConfig
				appCfg, err = config.LoadApp(".")
				if err == nil {
					app = appCfg.App
					serverName = appCfg.Server
					if serverName == "" && len(appCfg.Servers) > 0 {
						serverName = appCfg.Servers[0]
					}
					from, to, err = runRollback(ctx, flags, appCfg, toHash, out)
				}
			}
			if err != nil {
				return err
			}
			if flags.JSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(rollbackResult{
					MachineInterface: MachineInterface,
					App:              app,
					Server:           serverName,
					From:             from,
					To:               to,
					OK:               true,
				})
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&appName, "app", "", "app name — reads state from server instead of teploy.yml (requires --host)")
	cmd.Flags().StringVar(&toHash, "to", "", "specific version/release hash to roll back to")

	return cmd
}

// rollbackResult is the `rollback --json` success payload (token
// rollback-json-v1). Failures never emit it: they keep the structured
// error-envelope-on-stderr convention.
type rollbackResult struct {
	MachineInterface int    `json:"machine_interface"`
	App              string `json:"app"`
	Server           string `json:"server"`
	From             string `json:"from"`
	To               string `json:"to"`
	OK               bool   `json:"ok"`
}

// rollbackLanded reads the release hashes a successful rollback moved
// between: "to" is what now runs, "from" is what it replaced (recorded by
// the rollback itself as the previous release).
func rollbackLanded(ctx context.Context, executor ssh.Executor, app string) (from, to string) {
	st, err := state.Read(ctx, executor, app)
	if err != nil || st == nil {
		return "", ""
	}
	return st.PreviousHash, st.CurrentHash
}

func runRollback(ctx context.Context, flags *Flags, appCfg *config.AppConfig, toHash string, out io.Writer) (from, to string, retErr error) {
	executor, err := connectForApp(ctx, flags, appCfg)
	if err != nil {
		return "", "", err
	}
	defer executor.Close()

	// Static apps take the file-based rollback path.
	if appCfg.IsStatic() {
		d := deploy.NewStaticDeployer(executor, out)
		return "", "", d.Rollback(ctx, deploy.StaticRollbackConfig{
			App:         appCfg.App,
			Domain:      appCfg.Domain,
			ToHash:      toHash,
			SPA:         appCfg.SPA,
			SPAFallback: appCfg.SPAFallback,
			Cache:       appCfg.Cache,
			Headers:     appCfg.Headers,
			CaddyExtra:  appCfg.CaddyExtra,
		})
	}

	// Preserve custom TLS termination across rollback. The cert is already
	// on the server from the last deploy; re-upload to be safe (idempotent).
	// Legacy shared path (nil attempt): the rollback target is not known
	// here, and F14-recorded releases override these paths from the record
	// with the target's own attempt-scoped cert (F08).
	tlsCert, tlsKey, tlsInternal, err := resolveAppTLS(ctx, executor, appCfg, nil)
	if err != nil {
		return "", "", err
	}

	rollbackErr := deploy.Rollback(ctx, executor, out, deploy.RollbackConfig{
		App:          appCfg.App,
		Domain:       appCfg.Domain,
		StopTimeout:  appCfg.StopTimeout,
		DrainSeconds: appCfg.DrainSeconds,
		ToHash:       toHash,
		Health:       healthConfigFrom(appCfg.Health),
		TLSCert:      tlsCert,
		TLSKey:       tlsKey,
		TLSInternal:  tlsInternal,
		CaddyExtra:   appCfg.CaddyExtra,
		Cache:        appCfg.Cache,
		Firewall:     caddyFirewall(appCfg.Firewall),
		Access:       caddyAccess(appCfg.Access),
		Ingress:      appCfg.Ingress,
	})

	// Fire notification (best-effort). buildNotifier, not NewNotifier: this path
	// used to read only the legacy `notifications.webhook` key, so an install
	// configured with `notifications.channels` received deploy events (which go
	// through buildNotifier) but never rollback events — the one event you most
	// want to hear about.
	if notifier := buildNotifier(appCfg); notifier != nil {
		msg := fmt.Sprintf("Rolled back %s", appCfg.App)
		if rollbackErr != nil {
			msg = fmt.Sprintf("Rollback failed for %s: %s", appCfg.App, rollbackErr)
		}
		for _, nErr := range notifier.Send(ctx, notify.Payload{
			App:     appCfg.App,
			Server:  executor.Host(),
			Type:    "rollback",
			Success: rollbackErr == nil,
			Message: msg,
		}) {
			fmt.Fprintf(os.Stderr, "Warning: notification failed: %v\n", nErr)
		}
	}

	// Audit the rollback (fire-and-forget; no-op unless `audit:` is configured).
	target := toHash
	if target == "" {
		target = "previous"
	}
	emitDeployAudit(ctx, appCfg, "deploy.rollback", target, executor.Host(), rollbackErr)

	if rollbackErr != nil {
		return "", "", rollbackErr
	}
	from, to = rollbackLanded(ctx, executor, appCfg.App)
	return from, to, nil
}

// runRollbackByApp rolls back using server-side state instead of a local
// teploy.yml. Used by teploy-dash and for running rollback outside of an
// app directory.
//
// Both deploy types are complete on this path since F13/F14: container
// rollbacks restore from the recorded release spec (deploy.Rollback reads
// the F14 record, backfilling from live containers for pre-store releases),
// and static rollbacks read the recorded serving config
// (StaticDeployer.RollbackStateOnly).
func runRollbackByApp(ctx context.Context, flags *Flags, appName, toHash string, out io.Writer) (from, to string, retErr error) {
	if err := config.ValidateName(appName); err != nil {
		return "", "", err
	}
	if toHash != "" {
		if err := validateVersionArg(toHash); err != nil {
			return "", "", fmt.Errorf("invalid --to %q for a container rollback", toHash)
		}
	}
	if flags.Host == "" {
		return "", "", fmt.Errorf("--host is required when using --app")
	}

	host, user, key, err := config.ResolveServer(flags.Host, flags.Host, flags.User, flags.Key)
	if err != nil {
		return "", "", err
	}
	if !flags.JSON {
		fmt.Printf("Connecting to %s@%s...\n", user, host)
	}

	executor, err := ssh.Connect(ctx, ssh.ConnectConfig{Host: host, User: user, KeyPath: key})
	if err != nil {
		return "", "", err
	}
	defer executor.Close()

	appState, err := state.Read(ctx, executor, appName)
	if err != nil {
		return "", "", fmt.Errorf("reading state for %q: %w", appName, err)
	}
	if appState == nil {
		return "", "", fmt.Errorf("no state found for app %q on %s — has it been deployed?", appName, host)
	}
	if appState.DeploymentType == "static" {
		// Static rollback reads the serving config (SPA, headers, cache…)
		// from the recorded release metadata (F13/F14). Releases deployed
		// before the store existed have no record — that is a redeploy-first
		// request, not a guess at how the release was served.
		d := deploy.NewStaticDeployer(executor, out)
		if err := d.RollbackStateOnly(ctx, appName, toHash); err != nil {
			return "", "", err
		}
		from, to = rollbackLanded(ctx, executor, appName)
		return from, to, nil
	}
	ingress := appState.IngressMode
	if ingress == "" {
		ingress = config.IngressCaddy
	}
	// Only Caddy-managed routing needs a domain: host ingress publishes a
	// raw port and external ingress is the user's own front.
	if ingress == config.IngressCaddy && appState.Domain == "" {
		return "", "", fmt.Errorf("no domain in state for %q — redeploy once to update state", appName)
	}

	if err := deploy.Rollback(ctx, executor, out, deploy.RollbackConfig{
		App:     appName,
		Domain:  appState.Domain,
		ToHash:  toHash,
		Ingress: ingress,
	}); err != nil {
		return "", "", err
	}
	from, to = rollbackLanded(ctx, executor, appName)
	return from, to, nil
}
