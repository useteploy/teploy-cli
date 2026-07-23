package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"

	"github.com/spf13/cobra"
	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/ssh"
	"github.com/useteploy/teploy/internal/state"
)

func newPinCmd(flags *Flags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pin [version]",
		Short: "Protect a version from keep_versions auto-pruning",
		Long: `Pin a deployed version so it is never removed by keep_versions cleanup,
even once it falls outside the retention window — a durable rollback target.

With no argument, pins the currently-deployed version. Pins are stored on the
server, so the CLI, dashboard, and auto-deploy all honor the same set.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			version := ""
			if len(args) > 0 {
				version = args[0]
			}
			return runPin(flags, version)
		},
	}
	return cmd
}

func newUnpinCmd(flags *Flags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "unpin <version>",
		Short: "Remove a version's pin (it becomes eligible for pruning again)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runUnpin(flags, args[0])
		},
	}
	return cmd
}

func newPinsCmd(flags *Flags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pins",
		Short: "List pinned versions for the current app",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPins(flags)
		},
	}
	return cmd
}

// pinExecutor loads the app config, rejects static apps (pins gate the
// container image/version prune, which static release dirs don't use), and
// opens a connection.
func pinExecutor(flags *Flags) (context.Context, context.CancelFunc, ssh.Executor, *config.AppConfig, error) {
	appCfg, err := config.LoadApp(".")
	if err != nil {
		return nil, nil, nil, nil, err
	}
	if appCfg.IsStatic() {
		return nil, nil, nil, nil, fmt.Errorf("pins apply to container versions; type:static apps use release directories (see 'teploy releases')")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	executor, err := connectForApp(ctx, flags, appCfg)
	if err != nil {
		cancel()
		return nil, nil, nil, nil, err
	}
	return ctx, cancel, executor, appCfg, nil
}

func runPin(flags *Flags, version string) error {
	ctx, cancel, executor, appCfg, err := pinExecutor(flags)
	if err != nil {
		return err
	}
	defer cancel()
	defer executor.Close()

	if version == "" {
		s, err := state.Read(ctx, executor, appCfg.App)
		if err != nil || s == nil || s.CurrentHash == "" {
			return fmt.Errorf("no version given and no current deployment to pin — deploy first or pass a version")
		}
		version = s.CurrentHash
	}

	if err := state.AddPin(ctx, executor, appCfg.App, version); err != nil {
		return err
	}
	if !flags.JSON {
		fmt.Printf("Pinned %s\n", version)
	}
	return printPins(ctx, executor, appCfg.App, flags.JSON)
}

func runUnpin(flags *Flags, version string) error {
	ctx, cancel, executor, appCfg, err := pinExecutor(flags)
	if err != nil {
		return err
	}
	defer cancel()
	defer executor.Close()

	if err := state.RemovePin(ctx, executor, appCfg.App, version); err != nil {
		return err
	}
	if !flags.JSON {
		fmt.Printf("Unpinned %s\n", version)
	}
	return printPins(ctx, executor, appCfg.App, flags.JSON)
}

func runPins(flags *Flags) error {
	ctx, cancel, executor, appCfg, err := pinExecutor(flags)
	if err != nil {
		return err
	}
	defer cancel()
	defer executor.Close()
	return printPins(ctx, executor, appCfg.App, flags.JSON)
}

func printPins(ctx context.Context, executor ssh.Executor, app string, jsonOutput bool) error {
	pins, err := state.ReadPins(ctx, executor, app)
	if err != nil {
		return err
	}
	if jsonOutput {
		type pinDTO struct {
			Version string `json:"version"`
		}
		result := make([]pinDTO, 0, len(pins))
		for _, pin := range pins {
			result = append(result, pinDTO{Version: pin})
		}
		return json.NewEncoder(os.Stdout).Encode(result)
	}
	if len(pins) == 0 {
		fmt.Println("No pinned versions.")
		return nil
	}
	fmt.Println("Pinned versions:")
	for _, p := range pins {
		fmt.Printf("  %s\n", p)
	}
	return nil
}
