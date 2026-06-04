package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/signal"

	"github.com/spf13/cobra"
	"github.com/useteploy/teploy/internal/autodeploy"
	"github.com/useteploy/teploy/internal/config"
)

func newAutoDeployCmd(flags *Flags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "autodeploy",
		Short: "Manage webhook-triggered and scheduled auto-deploys",
	}

	cmd.AddCommand(newAutoDeploySetupCmd(flags))
	cmd.AddCommand(newAutoDeployStatusCmd(flags))
	cmd.AddCommand(newAutoDeployRemoveCmd(flags))
	cmd.AddCommand(newAutoDeployScheduleCmd(flags))
	cmd.AddCommand(newAutoDeployUnscheduleCmd(flags))

	return cmd
}

func newAutoDeploySetupCmd(flags *Flags) *cobra.Command {
	var (
		branch string
		secret string
	)

	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Set up webhook auto-deploy",
		Long:  "Installs a webhook listener on the server that triggers deploys on git push.\nConfigure your Git provider to POST to https://yourdomain.com/teploy-webhook/<app>",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAutoDeploySetup(flags, branch, secret)
		},
	}

	cmd.Flags().StringVar(&branch, "branch", "main", "branch to watch for pushes")
	cmd.Flags().StringVar(&secret, "secret", "", "webhook secret for request validation")

	return cmd
}

func runAutoDeploySetup(flags *Flags, branch, secret string) error {
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

	mgr := autodeploy.NewManager(executor, os.Stdout)

	// A secret is mandatory — without it the listener is an open deploy trigger.
	// Generate one when the user didn't supply --secret, and surface it so they
	// can configure it in their Git provider's webhook settings.
	generated := false
	if secret == "" {
		buf := make([]byte, 32)
		if _, err := rand.Read(buf); err != nil {
			return fmt.Errorf("generating webhook secret: %w", err)
		}
		secret = hex.EncodeToString(buf)
		generated = true
	}

	cfg := autodeploy.Config{
		App:    appCfg.App,
		Branch: branch,
		Secret: secret,
	}

	if err := mgr.Setup(ctx, cfg); err != nil {
		return err
	}

	if err := mgr.SetupCaddyRoute(ctx, appCfg.App, appCfg.Domain); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not add Caddy route: %v\n", err)
		fmt.Fprintf(os.Stderr, "  You may need to add the webhook route manually\n")
	}

	fmt.Printf("\nAuto-deploy configured for %s\n", appCfg.App)
	fmt.Printf("  Webhook URL: https://%s/teploy-webhook/%s\n", appCfg.Domain, appCfg.App)
	fmt.Printf("  Branch: %s\n", branch)
	if generated {
		fmt.Printf("  Secret (generated — add this to your Git provider's webhook):\n    %s\n", secret)
	} else {
		fmt.Printf("  Secret: configured\n")
	}
	fmt.Printf("\nAdd this URL to your Git provider's webhook settings (push events only).\n")
	return nil
}

func newAutoDeployStatusCmd(flags *Flags) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Check auto-deploy status",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAutoDeployStatus(flags)
		},
	}
}

func runAutoDeployStatus(flags *Flags) error {
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

	mgr := autodeploy.NewManager(executor, os.Stdout)
	active, status, err := mgr.Status(ctx, appCfg.App)
	if err != nil {
		return err
	}

	fmt.Println("Webhook auto-deploy:")
	if active {
		fmt.Printf("  status:      active (%s)\n", status)
		fmt.Printf("  webhook URL: https://%s/teploy-webhook/%s\n", appCfg.Domain, appCfg.App)
	} else {
		fmt.Println("  status:      not configured")
		fmt.Println("  enable with: teploy autodeploy setup")
	}

	schedule, err := mgr.ScheduleStatus(ctx, appCfg.App)
	if err != nil {
		return err
	}
	fmt.Println("Scheduled redeploy:")
	if schedule != "" {
		fmt.Printf("  status:      active\n")
		fmt.Printf("  cron:        %s\n", schedule)
		fmt.Printf("  log:         /deployments/%s/scheduled-redeploy.log\n", appCfg.App)
	} else {
		fmt.Println("  status:      not configured")
		fmt.Println("  enable with: teploy autodeploy schedule \"<cron>\"")
	}
	return nil
}

func newAutoDeployScheduleCmd(flags *Flags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "schedule <cron>",
		Short: "Schedule periodic redeploys to refresh the image",
		Long: `Installs a cron job on the server that periodically pulls the image
referenced by the running container and redeploys only if a newer
digest is available. No-op when the image is already current.

Use this when the image tag is pinned to a major version (e.g. :14)
and you want to receive its patch releases automatically.

Examples:
  teploy autodeploy schedule "0 4 * * 0"      # Sundays at 4am
  teploy autodeploy schedule "0 */6 * * *"    # every 6 hours`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAutoDeploySchedule(flags, args[0])
		},
	}
	return cmd
}

func runAutoDeploySchedule(flags *Flags, schedule string) error {
	appCfg, err := config.LoadApp(".")
	if err != nil {
		return err
	}
	if err := autodeploy.ValidateSchedule(schedule); err != nil {
		return err
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	executor, err := connectForApp(ctx, flags, appCfg)
	if err != nil {
		return err
	}
	defer executor.Close()

	mgr := autodeploy.NewManager(executor, os.Stdout)
	return mgr.Schedule(ctx, appCfg.App, schedule)
}

func newAutoDeployUnscheduleCmd(flags *Flags) *cobra.Command {
	return &cobra.Command{
		Use:   "unschedule",
		Short: "Remove the scheduled redeploy cron entry",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAutoDeployUnschedule(flags)
		},
	}
}

func runAutoDeployUnschedule(flags *Flags) error {
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

	mgr := autodeploy.NewManager(executor, os.Stdout)
	return mgr.Unschedule(ctx, appCfg.App)
}

func newAutoDeployRemoveCmd(flags *Flags) *cobra.Command {
	return &cobra.Command{
		Use:   "remove",
		Short: "Remove auto-deploy webhook",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAutoDeployRemove(flags)
		},
	}
}

func runAutoDeployRemove(flags *Flags) error {
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

	mgr := autodeploy.NewManager(executor, os.Stdout)
	return mgr.Remove(ctx, appCfg.App)
}
