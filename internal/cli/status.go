package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strings"

	"github.com/spf13/cobra"
	"github.com/useteploy/teploy/internal/docker"
	"github.com/useteploy/teploy/internal/state"
)

func newStatusCmd(flags *Flags) *cobra.Command {
	var appName string
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show what's running for the app",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStatus(flags, appName)
		},
	}
	cmd.Flags().StringVar(&appName, "app", "", "app name — act on server state instead of teploy.yml (requires --host)")
	return cmd
}

func runStatus(flags *Flags, appName string) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	appCfg, executor, err := resolveApp(ctx, flags, appName)
	if err != nil {
		return err
	}
	defer executor.Close()

	// Read deploy state.
	current, _ := state.Read(ctx, executor, appCfg.App)

	// List containers.
	dk := docker.NewClient(executor)
	containers, err := dk.ListContainers(ctx, appCfg.App)
	if err != nil {
		return err
	}

	if flags.JSON {
		return json.NewEncoder(os.Stdout).Encode(map[string]interface{}{
			"app":        appCfg.App,
			"server":     executor.Host(),
			"state":      current,
			"containers": containers,
		})
	}

	fmt.Printf("App:     %s\n", appCfg.App)
	fmt.Printf("Server:  %s\n", executor.Host())
	if current != nil {
		fmt.Printf("Version: %s (port %d)\n", current.CurrentHash, current.CurrentPort)
		if current.PreviousHash != "" {
			fmt.Printf("Previous: %s (port %d)\n", current.PreviousHash, current.PreviousPort)
		}
	} else {
		fmt.Println("Version: not deployed")
	}

	if len(containers) == 0 {
		fmt.Println("\nNo containers")
		return nil
	}

	fmt.Printf("\n%-35s  %-25s  %-10s  %s\n", "CONTAINER", "IMAGE", "STATE", "STATUS")
	for _, c := range containers {
		fmt.Printf("%-35s  %-25s  %-10s  %s\n", c.Name, c.Image, c.State, c.Status)
	}
	return nil
}

func newStatsCmd(flags *Flags) *cobra.Command {
	var appName string
	cmd := &cobra.Command{
		Use:   "stats",
		Short: "Show CPU/RAM per container",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStats(flags, appName)
		},
	}
	cmd.Flags().StringVar(&appName, "app", "", "app name — act on server state instead of teploy.yml (requires --host)")
	return cmd
}

func runStats(flags *Flags, appName string) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	appCfg, executor, err := resolveApp(ctx, flags, appName)
	if err != nil {
		return err
	}
	defer executor.Close()

	// Get container names for this app (docker stats doesn't support --filter).
	dk := docker.NewClient(executor)
	containers, err := dk.ListContainers(ctx, appCfg.App)
	if err != nil {
		return err
	}
	if len(containers) == 0 {
		fmt.Println("No containers running")
		return nil
	}

	names := make([]string, len(containers))
	for i, c := range containers {
		names[i] = c.Name
	}

	format := "'table {{.Name}}\t{{.CPUPerc}}\t{{.MemUsage}}\t{{.MemPerc}}\t{{.NetIO}}\t{{.BlockIO}}'"
	if flags.JSON {
		format = "'{{json .}}'"
	}

	cmd := fmt.Sprintf("docker stats --no-stream --format %s %s", format, strings.Join(names, " "))
	return executor.RunStream(ctx, cmd, os.Stdout, os.Stderr)
}
