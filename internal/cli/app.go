package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"

	"github.com/spf13/cobra"
	"github.com/useteploy/teploy/internal/docker"
)

// newAppCmd groups commands that operate on a deployed app's running
// container(s) — distinct from `teploy exec`, which runs on the server itself.
func newAppCmd(flags *Flags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "app",
		Short: "Operate on a deployed app's running container",
	}
	cmd.AddCommand(newAppExecCmd(flags))
	return cmd
}

func newAppExecCmd(flags *Flags) *cobra.Command {
	var appName, process string
	cmd := &cobra.Command{
		Use:   "exec [command...]",
		Short: "Run a one-off command in the app's running container",
		Long: "Run a command inside the app's running container — e.g. a database migration,\n" +
			"a seed, or a maintenance task. The command runs in the existing container (not a\n" +
			"fresh one), via the container's shell, so pipes and redirects work. teploy\n" +
			"exits non-zero if the command fails.\n\n" +
			"Examples:\n" +
			"  teploy app exec -- bin/rails db:migrate\n" +
			"  teploy app exec --process worker -- ./manage.py migrate\n" +
			"  teploy app exec --app blog --host prod -- node scripts/seed.js",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAppExec(flags, appName, process, args)
		},
	}
	cmd.Flags().StringVar(&appName, "app", "", "app name — act on server state instead of teploy.yml (requires --host)")
	cmd.Flags().StringVar(&process, "process", "web", "process type to run the command in (web, worker, ...)")
	return cmd
}

func runAppExec(flags *Flags, appName, process string, args []string) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	appCfg, executor, err := resolveApp(ctx, flags, appName)
	if err != nil {
		return err
	}
	defer executor.Close()

	dk := docker.NewClient(executor)
	container, err := dk.RunningContainer(ctx, appCfg.App, process)
	if err != nil {
		return err
	}

	command := strings.Join(args, " ")
	if err := dk.ExecStream(ctx, container, command, os.Stdout, os.Stderr); err != nil {
		return fmt.Errorf("command failed in %s: %w", container, err)
	}
	return nil
}
