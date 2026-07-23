package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"

	"github.com/spf13/cobra"
	"github.com/useteploy/teploy/internal/autodeploy"
	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/ssh"
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
	cmd.AddCommand(newAutoDeployServeCmd())

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
		Long: "Installs the teploy binary and a real webhook listener (teploy autodeploy serve, run as a\n" +
			"systemd unit) on the server, which triggers deploys on git push using the same deploy\n" +
			"code `teploy deploy` uses. Also clones this repo's 'origin' remote into the server-side\n" +
			"build directory if it isn't already a checkout there — a private repo needs credentials\n" +
			"(e.g. an SSH deploy key) already configured for the server's user, or this step is\n" +
			"skipped with a warning and the first real webhook will fail to fetch until it's set up\n" +
			"manually. Configure your Git provider to POST to\n" +
			"https://yourdomain.com/teploy-webhook/<app>.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAutoDeploySetup(flags, branch, secret)
		},
	}

	cmd.Flags().StringVar(&branch, "branch", "main", "branch to watch for pushes")
	cmd.Flags().StringVar(&secret, "secret", "", "webhook secret for request validation")

	return cmd
}

func runAutoDeploySetup(flags *Flags, branch, secret string) error {
	if err := autodeploy.ValidateBranch(branch); err != nil {
		return err
	}

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

	// `teploy autodeploy serve` needs to run ON the server as a resident
	// process — upload the matching-platform teploy binary before wiring
	// the systemd unit up to run it. Reuses the same fetch/verify/extract
	// pipeline as `teploy update` (see binarydist.go).
	const teployBinaryPath = "/deployments/.bin/teploy"
	fmt.Println("Installing teploy binary on server...")
	binVersion, err := deployTeployBinaryToServer(ctx, executor, teployBinaryPath)
	if err != nil {
		return fmt.Errorf("installing teploy binary: %w", err)
	}
	fmt.Printf("  Installed teploy %s\n", binVersion)

	// `teploy autodeploy serve` fetches from an existing git checkout at
	// autodeploy.BuildDir(app) on every trigger — it has no operator
	// machine to rsync from the way `teploy deploy`'s server-build mode
	// does (and that path excludes .git from the rsync anyway, see
	// internal/build/ignore.go, so it wouldn't leave a fetchable checkout
	// even if it ran first). Clone it now, once, using the operator's own
	// git remote — best-effort: a private repo needing credentials the
	// server doesn't have yet will fail here with a clear message instead
	// of a cryptic one on the first real webhook.
	if err := ensureServerBuildCheckout(ctx, executor, appCfg.App); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: %v\n", err)
		fmt.Fprintf(os.Stderr, "  teploy autodeploy serve will fail to fetch until %s is a valid, credentialed git checkout on the server.\n", autodeploy.BuildDir(appCfg.App))
	}

	cfg := autodeploy.Config{
		App:              appCfg.App,
		Branch:           branch,
		Secret:           secret,
		TeployBinaryPath: teployBinaryPath,
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

// ensureServerBuildCheckout clones the operator's local git remote into
// autodeploy.BuildDir(app) on the server if a git checkout isn't already
// there, so `teploy autodeploy serve`'s `git fetch` on the first
// webhook-triggered deploy has something to fetch into. No-ops if a
// checkout already exists (idempotent — safe to call on every `setup`).
func ensureServerBuildCheckout(ctx context.Context, executor ssh.Executor, app string) error {
	buildDir := autodeploy.BuildDir(app)

	out, _ := executor.Run(ctx, fmt.Sprintf("test -d %s && echo yes || true", ssh.ShellQuote(buildDir+"/.git")))
	if strings.TrimSpace(out) == "yes" {
		return nil // already a checkout — nothing to do
	}

	remoteURL, err := localGitRemoteURL()
	if err != nil {
		return fmt.Errorf("determining local git remote (run this from the app's repo, with an 'origin' remote configured): %w", err)
	}

	if _, err := executor.Run(ctx, "mkdir -p "+ssh.ShellQuote(buildDir)); err != nil {
		return fmt.Errorf("creating build directory: %w", err)
	}
	cloneCmd := fmt.Sprintf("git clone %s %s", ssh.ShellQuote(remoteURL), ssh.ShellQuote(buildDir))
	if _, err := executor.Run(ctx, cloneCmd); err != nil {
		return fmt.Errorf("cloning %s on the server (a private repo needs credentials — e.g. an SSH deploy key — already configured for the server's user): %w", remoteURL, err)
	}
	fmt.Printf("  Cloned %s into %s on the server\n", remoteURL, buildDir)
	return nil
}

// localGitRemoteURL runs `git remote get-url origin` in the current
// directory (the operator's local checkout — same directory teploy.yml
// lives in).
func localGitRemoteURL() (string, error) {
	out, err := exec.Command("git", "remote", "get-url", "origin").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
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

	schedule, err := mgr.ScheduleStatus(ctx, appCfg.App)
	if err != nil {
		return err
	}
	if flags.JSON {
		return json.NewEncoder(os.Stdout).Encode(struct {
			App           string `json:"app"`
			Host          string `json:"host"`
			WebhookActive bool   `json:"webhook_active"`
			WebhookStatus string `json:"webhook_status"`
			WebhookURL    string `json:"webhook_url"`
			Scheduled     bool   `json:"scheduled"`
			Cron          string `json:"cron"`
			Log           string `json:"log"`
		}{
			App: appCfg.App, Host: executor.Host(), WebhookActive: active, WebhookStatus: status,
			WebhookURL: "https://" + appCfg.Domain + "/teploy-webhook/" + appCfg.App,
			Scheduled:  schedule != "", Cron: schedule,
			Log: "/deployments/" + appCfg.App + "/scheduled-redeploy.log",
		})
	}
	fmt.Println("Webhook auto-deploy:")
	if active {
		fmt.Printf("  status:      active (%s)\n", status)
		fmt.Printf("  webhook URL: https://%s/teploy-webhook/%s\n", appCfg.Domain, appCfg.App)
	} else {
		fmt.Println("  status:      not configured")
		fmt.Println("  enable with: teploy autodeploy setup")
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
