package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"

	"github.com/spf13/cobra"
	"github.com/useteploy/teploy/internal/caddy"
	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/state"
)

func newMaintenanceCmd(flags *Flags) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "maintenance",
		Aliases: []string{"maint"},
		Short:   "Toggle maintenance mode",
	}

	cmd.AddCommand(newMaintenanceOnCmd(flags))
	cmd.AddCommand(newMaintenanceOffCmd(flags))

	return cmd
}

func newMaintenanceOnCmd(flags *Flags) *cobra.Command {
	var appName string
	cmd := &cobra.Command{
		Use:   "on",
		Short: "Enable maintenance mode (returns 503 to all visitors)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMaintenanceToggle(flags, appName, true)
		},
	}
	cmd.Flags().StringVar(&appName, "app", "", "app name — act on server state instead of teploy.yml (requires --host)")
	return cmd
}

func newMaintenanceOffCmd(flags *Flags) *cobra.Command {
	var appName string
	cmd := &cobra.Command{
		Use:   "off",
		Short: "Disable maintenance mode (restore normal traffic)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMaintenanceToggle(flags, appName, false)
		},
	}
	cmd.Flags().StringVar(&appName, "app", "", "app name — act on server state instead of teploy.yml (requires --host)")
	return cmd
}

func runMaintenanceToggle(flags *Flags, appName string, enable bool) error {
	// With --app there's no teploy.yml to check ingress against, so the
	// AUTHORITATIVE server state decides (audit T62): the old shape assumed
	// caddy ingress on that path, and a maintenance toggle against a
	// host/external-ingress app happily rewrote routes nothing serves.
	if appName == "" {
		appCfg, err := config.LoadApp(".")
		if err != nil {
			return err
		}
		// Maintenance mode is served by Caddy as a 503 route block. With
		// external ingress, Teploy doesn't control the routing layer, so
		// there's nothing to swap. Surface this clearly rather than silently
		// no-op'ing or pretending it worked.
		if !appCfg.UsesCaddy() {
			return fmt.Errorf("'teploy maintenance' requires Teploy-managed Caddy; this app uses ingress: %s — route traffic away from the container via your external ingress instead", appCfg.Ingress)
		}
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	appCfg, executor, err := resolveApp(ctx, flags, appName)
	if err != nil {
		return err
	}
	defer executor.Close()

	if appName != "" {
		st, err := state.Read(ctx, executor, appName)
		if err != nil {
			return fmt.Errorf("reading server state for %s: %w", appName, err)
		}
		if st != nil && st.IngressMode != "" && st.IngressMode != "caddy" {
			return fmt.Errorf("'teploy maintenance' requires Teploy-managed Caddy; %s uses ingress: %s (per its server state) — route traffic away via that ingress instead", appName, st.IngressMode)
		}
	}

	// Maintenance is serialized with deploys under the SAME fenced app lock
	// (audit T62): the toggle used to run unlocked, so a deploy during
	// maintenance re-rendered the app's route while the stash held a route
	// for the now-stopped release — and maintenance-off then restored that
	// stale route over the deploy's live one.
	if err := state.EnsureAppDir(ctx, executor, appCfg.App); err != nil {
		return fmt.Errorf("creating app directory: %w", err)
	}
	lk, err := state.AcquireLockFenced(ctx, executor, appCfg.App)
	if err != nil {
		return fmt.Errorf("acquiring deploy lock: %w", err)
	}
	defer state.ReleaseLockFenced(executor, lk, appCfg.App)

	client := caddy.NewClient(executor)

	if enable {
		fmt.Printf("Enabling maintenance mode for %s (%s)...\n", appCfg.App, appCfg.Domain)
		if err := client.SetMaintenance(ctx, appCfg.App, appCfg.Domain); err != nil {
			return fmt.Errorf("enabling maintenance mode: %w", err)
		}
		fmt.Println("Maintenance mode enabled — all visitors see 503 page")
	} else {
		fmt.Printf("Disabling maintenance mode for %s...\n", appCfg.App)
		if err := client.RemoveMaintenance(ctx, appCfg.App); err != nil {
			return fmt.Errorf("disabling maintenance mode: %w", err)
		}
		fmt.Println("Maintenance mode disabled — traffic restored")
	}

	return nil
}
