package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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
			return runStats(flags, appName, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&appName, "app", "", "app name — act on server state instead of teploy.yml (requires --host)")
	return cmd
}

type containerStats struct {
	Name          string `json:"name"`
	CPUPercent    string `json:"cpu_percent"`
	MemoryUsage   string `json:"memory_usage"`
	MemoryPercent string `json:"memory_percent"`
	NetworkIO     string `json:"network_io"`
	BlockIO       string `json:"block_io"`
}

type dockerStatsEntry struct {
	Name     string `json:"Name"`
	CPUPerc  string `json:"CPUPerc"`
	MemUsage string `json:"MemUsage"`
	MemPerc  string `json:"MemPerc"`
	NetIO    string `json:"NetIO"`
	BlockIO  string `json:"BlockIO"`
}

func runStats(flags *Flags, appName string, out io.Writer) error {
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
		if flags.JSON {
			return writeContainerStats(out, nil)
		}
		fmt.Fprintln(out, "No containers running")
		return nil
	}

	names := make([]string, len(containers))
	for i, c := range containers {
		names[i] = c.Name
	}

	if flags.JSON {
		cmd := fmt.Sprintf("docker stats --no-stream --format '{{json .}}' %s", strings.Join(names, " "))
		output, err := executor.Run(ctx, cmd)
		if err != nil {
			return err
		}
		stats, err := parseContainerStats(output)
		if err != nil {
			return err
		}
		return writeContainerStats(out, stats)
	}

	format := "'table {{.Name}}\t{{.CPUPerc}}\t{{.MemUsage}}\t{{.MemPerc}}\t{{.NetIO}}\t{{.BlockIO}}'"
	cmd := fmt.Sprintf("docker stats --no-stream --format %s %s", format, strings.Join(names, " "))
	return executor.RunStream(ctx, cmd, out, os.Stderr)
}

func writeContainerStats(out io.Writer, stats []containerStats) error {
	if stats == nil {
		stats = []containerStats{}
	}
	return json.NewEncoder(out).Encode(stats)
}

func parseContainerStats(output string) ([]containerStats, error) {
	stats := []containerStats{}
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var entry dockerStatsEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			return nil, fmt.Errorf("parsing container stats: %w", err)
		}
		stats = append(stats, containerStats{
			Name:          entry.Name,
			CPUPercent:    entry.CPUPerc,
			MemoryUsage:   entry.MemUsage,
			MemoryPercent: entry.MemPerc,
			NetworkIO:     entry.NetIO,
			BlockIO:       entry.BlockIO,
		})
	}
	return stats, nil
}
