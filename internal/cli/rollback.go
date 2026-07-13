package cli

import (
	"context"
	"fmt"
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
server (see keep_versions retention); for type:static apps, a specific retained
release (see keep_releases). Without --to, the previous deploy is used.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if appName != "" {
				return runRollbackByApp(flags, appName, toHash)
			}
			return runRollback(flags, toHash)
		},
	}

	cmd.Flags().StringVar(&appName, "app", "", "app name — reads state from server instead of teploy.yml (requires --host)")
	cmd.Flags().StringVar(&toHash, "to", "", "specific version/release hash to roll back to")

	return cmd
}

func runRollback(flags *Flags, toHash string) error {
	appCfg, err := config.LoadApp(".")
	if err != nil {
		return err
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	executor, err := connectForApp(ctx, flags, appCfg)
	if err != nil {
		return err
	}
	defer executor.Close()

	// Static apps take the file-based rollback path.
	if appCfg.IsStatic() {
		d := deploy.NewStaticDeployer(executor, os.Stdout)
		return d.Rollback(ctx, deploy.StaticRollbackConfig{
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
	tlsCert, tlsKey, tlsInternal, err := resolveAppTLS(ctx, executor, appCfg)
	if err != nil {
		return err
	}

	rollbackErr := deploy.Rollback(ctx, executor, os.Stdout, deploy.RollbackConfig{
		App:         appCfg.App,
		Domain:      appCfg.Domain,
		StopTimeout: appCfg.StopTimeout,
		ToHash:      toHash,
		TLSCert:     tlsCert,
		TLSKey:      tlsKey,
		TLSInternal: tlsInternal,
		CaddyExtra:  appCfg.CaddyExtra,
		Firewall:    caddyFirewall(appCfg.Firewall),
		Ingress:     appCfg.Ingress,
	})

	// Fire notification (best-effort).
	if notifier := notify.NewNotifier(appCfg.Notifications.Webhook); notifier != nil {
		msg := fmt.Sprintf("Rolled back %s", appCfg.App)
		if rollbackErr != nil {
			msg = fmt.Sprintf("Rollback failed for %s: %s", appCfg.App, rollbackErr)
		}
		if nErr := notifier.Send(ctx, notify.Payload{
			App:     appCfg.App,
			Server:  executor.Host(),
			Type:    "rollback",
			Success: rollbackErr == nil,
			Message: msg,
		}); nErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: notification failed: %v\n", nErr)
		}
	}

	return rollbackErr
}

// runRollbackByApp rolls back using server-side state instead of a local
// teploy.yml. Used by teploy-dash and for running rollback outside of an
// app directory.
//
// Known gap, not addressed here: this always calls the container
// deploy.Rollback — it never checks whether the app is actually
// type:static (that requires teploy.yml, which this path deliberately
// doesn't need) and branches to StaticDeployer.Rollback the way
// runRollback does. Calling `teploy rollback --app <static-app> --host
// ...` will fail rather than correctly flip the release symlink.
func runRollbackByApp(flags *Flags, appName, toHash string) error {
	if flags.Host == "" {
		return fmt.Errorf("--host is required when using --app")
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	host, user, key, err := config.ResolveServer(flags.Host, flags.Host, flags.User, flags.Key)
	if err != nil {
		return err
	}

	executor, err := ssh.Connect(ctx, ssh.ConnectConfig{Host: host, User: user, KeyPath: key})
	if err != nil {
		return err
	}
	defer executor.Close()

	appState, err := state.Read(ctx, executor, appName)
	if err != nil {
		return fmt.Errorf("reading state for %q: %w", appName, err)
	}
	if appState == nil {
		return fmt.Errorf("no state found for app %q on %s — has it been deployed?", appName, host)
	}
	if appState.Domain == "" {
		return fmt.Errorf("no domain in state for %q — redeploy once to update state", appName)
	}

	return deploy.Rollback(ctx, executor, os.Stdout, deploy.RollbackConfig{
		App:    appName,
		Domain: appState.Domain,
		ToHash: toHash,
	})
}
