package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"

	"github.com/spf13/cobra"
	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/deploy"
	"github.com/useteploy/teploy/internal/state"
)

// newReleasesCmd lists retained releases for a static deploy. The list comes
// from /deployments/<app>/releases/ on the target server, ordered by mtime
// (newest first). The current release is starred.
func newReleasesCmd(flags *Flags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "releases",
		Short: "List retained releases for a type:static app",
		Long: `Lists release directories on the server for the current app, newest first.
The currently-active release is marked with *. Use 'teploy rollback --to <hash>'
to flip to a specific past release.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runReleases(flags)
		},
	}
	return cmd
}

func runReleases(flags *Flags) error {
	appCfg, err := config.LoadApp(".")
	if err != nil {
		return err
	}
	if !appCfg.IsStatic() {
		return fmt.Errorf("'teploy releases' is only available for type:static apps (this app is type:%q)", appCfg.Type)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	executor, err := connectForApp(ctx, flags, appCfg)
	if err != nil {
		return err
	}
	defer executor.Close()

	d := deploy.NewStaticDeployer(executor, os.Stdout)
	releases, err := d.ListReleases(ctx, appCfg.App, "")
	if err != nil {
		return fmt.Errorf("listing releases: %w", err)
	}
	if len(releases) == 0 {
		fmt.Println("No releases yet — run 'teploy deploy' first.")
		return nil
	}

	current := ""
	if s, _ := state.Read(ctx, executor, appCfg.App); s != nil {
		current = s.CurrentHash
	}

	for _, r := range releases {
		marker := "  "
		if r == current {
			marker = " *"
		}
		fmt.Printf("%s %s\n", marker, r)
	}
	return nil
}
