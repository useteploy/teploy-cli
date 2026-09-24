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
	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/deploy"
	"github.com/useteploy/teploy/internal/docker"
	"github.com/useteploy/teploy/internal/ssh"
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
	return writeStatus(ctx, flags, appCfg, executor, os.Stdout)
}

// formatRepairDebt renders the operator-facing sentence for outstanding
// release-record repair debt (C01-6), or "" when there is none — absence
// must be silent.
func formatRepairDebt(app string, debt *deploy.RepairDebt) string {
	if debt == nil {
		return ""
	}
	return fmt.Sprintf("release record for %s@%s is missing (record write failed %d attempt(s): %s) — the next deploy rebuilds it", app, debt.Release, debt.Attempts, debt.Reason)
}

// writeStatus renders the app's server-side state. Split from runStatus so
// the state-read surface (state, containers, and now repair debt) is
// testable against a mock executor.
func writeStatus(ctx context.Context, flags *Flags, appCfg *config.AppConfig, executor ssh.Executor, out io.Writer) error {
	// Read deploy state.
	current, _ := state.Read(ctx, executor, appCfg.App)

	// Outstanding release-record repair debt (C01-6): a previous deploy
	// whose record write failed after the live commit. Unreadable markers
	// are visible too — an unhealable debt must not be an invisible one.
	debt, debtErr := deploy.ReadRepairDebt(ctx, executor, appCfg.App)

	// List containers.
	dk := docker.NewClient(executor)
	containers, err := dk.ListContainers(ctx, appCfg.App)
	if err != nil {
		return err
	}
	containers = dk.ResolveImageTags(ctx, containers)

	if flags.JSON {
		return json.NewEncoder(out).Encode(map[string]interface{}{
			"app":         appCfg.App,
			"server":      executor.Host(),
			"state":       current,
			"repair_debt": debt,
			"containers":  containers,
		})
	}

	fmt.Fprintf(out, "App:     %s\n", appCfg.App)
	fmt.Fprintf(out, "Server:  %s\n", executor.Host())
	if current != nil {
		fmt.Fprintf(out, "Version: %s (port %d)\n", current.CurrentHash, current.CurrentPort)
		if current.PreviousHash != "" {
			fmt.Fprintf(out, "Previous: %s (port %d)\n", current.PreviousHash, current.PreviousPort)
		}
	} else {
		fmt.Fprintln(out, "Version: not deployed")
	}
	if debtErr != nil {
		fmt.Fprintf(out, "Repair debt: marker could not be read — %v\n", debtErr)
	} else if line := formatRepairDebt(appCfg.App, debt); line != "" {
		fmt.Fprintf(out, "Repair debt: %s\n", line)
	}

	if len(containers) == 0 {
		fmt.Fprintln(out, "\nNo containers")
		return nil
	}

	fmt.Fprintf(out, "\n%-35s  %-35s  %-10s  %s\n", "CONTAINER", "IMAGE", "STATE", "STATUS")
	for _, c := range containers {
		fmt.Fprintf(out, "%-35s  %-35s  %-10s  %s\n", c.Name, c.Image, c.State, c.Status)
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
