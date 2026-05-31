package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strings"

	"github.com/spf13/cobra"
	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/state"
)

func newLogCmd(flags *Flags) *cobra.Command {
	var last int
	var appName string

	cmd := &cobra.Command{
		Use:   "log",
		Short: "Show deploy history",
		Long:  "Display the deploy log from the server — deploys, rollbacks, restarts, and failures.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLog(flags, appName, last)
		},
	}

	cmd.Flags().IntVar(&last, "last", 20, "number of entries to show")
	cmd.Flags().StringVar(&appName, "app", "", "app name — act on server state instead of teploy.yml (requires --host)")

	return cmd
}

func runLog(flags *Flags, appName string, last int) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	appCfg, executor, err := resolveApp(ctx, flags, appName)
	if err != nil {
		return err
	}
	defer executor.Close()

	// Read the log file and filter by app.
	output, err := executor.Run(ctx, "cat /deployments/teploy.log 2>/dev/null")
	if err != nil || strings.TrimSpace(output) == "" {
		fmt.Println("No deploy history")
		return nil
	}

	var entries []state.LogEntry
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var entry state.LogEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		if entry.App == appCfg.App {
			entries = append(entries, entry)
		}
	}

	if len(entries) == 0 {
		fmt.Printf("No deploy history for %s\n", appCfg.App)
		return nil
	}

	// Take the last N entries.
	if last > 0 && len(entries) > last {
		entries = entries[len(entries)-last:]
	}

	// Output.
	if flags.JSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(entries)
	}

	fmt.Printf("%-20s  %-10s  %-8s  %-7s  %s\n", "TIMESTAMP", "TYPE", "VERSION", "STATUS", "DURATION")
	for _, e := range entries {
		status := "ok"
		if !e.Success {
			status = "FAILED"
		}
		ts := e.Timestamp.Format("2006-01-02 15:04:05")
		hash := e.Hash
		if hash == "" {
			hash = "-"
		}
		dur := fmt.Sprintf("%dms", e.DurationMs)
		if e.DurationMs == 0 {
			dur = "-"
		}
		fmt.Printf("%-20s  %-10s  %-8s  %-7s  %s\n", ts, e.Type, hash, status, dur)
	}
	return nil
}
