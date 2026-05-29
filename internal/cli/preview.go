package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"time"

	"github.com/spf13/cobra"
	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/notify"
	"github.com/useteploy/teploy/internal/preview"
)

func newPreviewCmd(flags *Flags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "preview",
		Short: "Manage preview environments",
	}

	cmd.AddCommand(newPreviewDeployCmd(flags))
	cmd.AddCommand(newPreviewListCmd(flags))
	cmd.AddCommand(newPreviewDestroyCmd(flags))
	cmd.AddCommand(newPreviewPruneCmd(flags))

	return cmd
}

func newPreviewDeployCmd(flags *Flags) *cobra.Command {
	var ttl string

	cmd := &cobra.Command{
		Use:   "deploy <branch>",
		Short: "Deploy a preview environment for a branch",
		Long: `Deploy a preview environment on a temporary <branch>.<domain> subdomain.

Uses the current working directory's code (whatever branch is checked out
locally) and the image already configured in teploy.yml. To preview a
different branch, check it out first and build the image — preview deploy
does not run its own build or git checkout.

Example:
  git checkout feat/new-landing
  teploy deploy --image registry/myapp:feat-new-landing
  teploy preview deploy feat-new-landing --ttl 24h`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPreviewDeploy(flags, args[0], ttl)
		},
	}

	cmd.Flags().StringVar(&ttl, "ttl", "72h", "time-to-live before auto-expiry")

	return cmd
}

func runPreviewDeploy(flags *Flags, branch, ttlStr string) error {
	appCfg, err := config.LoadApp(".")
	if err != nil {
		return err
	}

	// Preview environments need Teploy-managed Caddy to provision a
	// branch.app.example.com subdomain route on demand. With external
	// ingress (CF Tunnel, etc.), preview hostnames have to be added at
	// the external layer — out of scope for Teploy. Reject explicitly so
	// the failure mode is "useful error" not "silently broken preview".
	if !appCfg.UsesCaddy() {
		return fmt.Errorf("'teploy preview' requires Teploy-managed Caddy; this app uses ingress: %s — add the preview hostname route at your external ingress and deploy the branch with -d instead", appCfg.Ingress)
	}

	ttl, err := time.ParseDuration(ttlStr)
	if err != nil {
		return fmt.Errorf("invalid TTL: %w", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	executor, err := connectForApp(ctx, flags, appCfg)
	if err != nil {
		return err
	}
	defer executor.Close()

	version, err := gitShortHash()
	if err != nil {
		return fmt.Errorf("could not determine version from git: %w", err)
	}

	// Use the app's current image or build tag.
	image := appCfg.Image
	if image == "" {
		image = appCfg.App + "-build-" + version
	}

	mgr := preview.NewManager(executor, os.Stdout)
	err = mgr.Deploy(ctx, preview.DeployConfig{
		App:     appCfg.App,
		Domain:  appCfg.Domain,
		Branch:  branch,
		Image:   image,
		Version: version,
		TTL:     ttl,
	})

	if n := buildNotifier(appCfg); n != nil {
		msg := fmt.Sprintf("Preview %s deployed for %s", branch, appCfg.App)
		if err != nil {
			msg = fmt.Sprintf("Preview %s failed for %s: %s", branch, appCfg.App, err)
		}
		n.Send(ctx, notify.Payload{
			App:     appCfg.App,
			Server:  executor.Host(),
			Type:    "preview",
			Success: err == nil,
			Hash:    version,
			Message: msg,
		})
	}

	return err
}

func newPreviewListCmd(flags *Flags) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List active previews",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPreviewList(flags)
		},
	}
}

func runPreviewList(flags *Flags) error {
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

	mgr := preview.NewManager(executor, os.Stdout)
	previews, err := mgr.List(ctx, appCfg.App)
	if err != nil {
		return err
	}

	if len(previews) == 0 {
		fmt.Println("No active previews")
		return nil
	}

	for _, p := range previews {
		expired := ""
		if time.Now().UTC().After(p.ExpiresAt) {
			expired = " (expired)"
		}
		fmt.Printf("  %s → https://%s%s\n", p.Branch, p.Domain, expired)
		fmt.Printf("    Container: %s  Port: %d  Expires: %s\n",
			p.Container, p.Port, p.ExpiresAt.Format(time.RFC3339))
	}
	return nil
}

func newPreviewDestroyCmd(flags *Flags) *cobra.Command {
	return &cobra.Command{
		Use:   "destroy <branch>",
		Short: "Tear down a preview environment",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPreviewDestroy(flags, args[0])
		},
	}
}

func runPreviewDestroy(flags *Flags, branch string) error {
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

	mgr := preview.NewManager(executor, os.Stdout)
	return mgr.Destroy(ctx, appCfg.App, branch)
}

func newPreviewPruneCmd(flags *Flags) *cobra.Command {
	return &cobra.Command{
		Use:   "prune",
		Short: "Remove expired previews",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPreviewPrune(flags)
		},
	}
}

func runPreviewPrune(flags *Flags) error {
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

	mgr := preview.NewManager(executor, os.Stdout)
	n, err := mgr.Prune(ctx, appCfg.App)
	if err != nil {
		return err
	}
	if n == 0 {
		fmt.Println("No expired previews to prune")
	} else {
		fmt.Printf("Pruned %d expired preview(s)\n", n)
	}
	return nil
}
