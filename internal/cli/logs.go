package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/useteploy/teploy/internal/docker"
	"github.com/useteploy/teploy/internal/state"
)

func newLogsCmd(flags *Flags) *cobra.Command {
	var (
		process  string
		lines    int
		appName  string
		noFollow bool
	)

	cmd := &cobra.Command{
		Use:   "logs",
		Short: "Tail container logs",
		Long:  "Stream Docker logs from the running container. Defaults to the web process.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLogs(flags, appName, process, lines, !noFollow)
		},
	}

	cmd.Flags().StringVar(&process, "process", "web", "process type to view logs for")
	cmd.Flags().IntVar(&lines, "lines", 50, "number of historical log lines (--tail is an alias)")
	cmd.Flags().BoolVar(&noFollow, "no-follow", false, "print the requested lines and exit")
	cmd.Flags().StringVar(&appName, "app", "", "app name — act on server state instead of teploy.yml (requires --host)")
	cmd.Flags().SetNormalizeFunc(tailToLines)

	return cmd
}

// tailToLines lets `--tail N` be used as a shorthand for `--lines N`,
// matching the muscle memory of `docker logs --tail`.
func tailToLines(_ *pflag.FlagSet, name string) pflag.NormalizedName {
	if name == "tail" {
		return "lines"
	}
	return pflag.NormalizedName(name)
}

func runLogs(flags *Flags, appName, process string, lines int, follow bool) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	appCfg, executor, err := resolveApp(ctx, flags, appName)
	if err != nil {
		return err
	}
	defer executor.Close()

	// Read current state to find the running version.
	current, err := state.Read(ctx, executor, appCfg.App)
	if err != nil || current == nil {
		return fmt.Errorf("no deploy state found for %s — deploy first", appCfg.App)
	}

	containerName := docker.ContainerName(appCfg.App, process, current.CurrentHash)
	cmd := logsCommand(containerName, lines, follow)
	return executor.RunStream(ctx, cmd, os.Stdout, os.Stderr)
}

func logsCommand(containerName string, lines int, follow bool) string {
	followFlag := ""
	if follow {
		followFlag = " -f"
	}
	return fmt.Sprintf("docker logs%s --tail %d %s", followFlag, lines, containerName)
}
