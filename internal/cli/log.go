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
			return runLog(flags, appName, last, cmd.OutOrStdout())
		},
	}

	cmd.Flags().IntVar(&last, "last", 20, "number of entries to show")
	cmd.Flags().StringVar(&appName, "app", "", "app name — act on server state instead of teploy.yml (requires --host)")

	return cmd
}

func runLog(flags *Flags, appName string, last int, out io.Writer) error {
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
		return writeLogEntries(out, nil, flags.JSON, "No deploy history")
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
		return writeLogEntries(out, nil, flags.JSON, fmt.Sprintf("No deploy history for %s", appCfg.App))
	}

	// Take the last N entries.
	if last > 0 && len(entries) > last {
		entries = entries[len(entries)-last:]
	}

	return writeLogEntries(out, entries, flags.JSON, "")
}

func writeLogEntries(out io.Writer, entries []state.LogEntry, jsonOutput bool, emptyMessage string) error {
	if jsonOutput {
		if entries == nil {
			entries = []state.LogEntry{}
		}
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(entries)
	}
	if len(entries) == 0 {
		fmt.Fprintln(out, emptyMessage)
		return nil
	}

	fmt.Fprintf(out, "%-20s  %-10s  %-8s  %-7s  %s\n", "TIMESTAMP", "TYPE", "VERSION", "STATUS", "DURATION")
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
		fmt.Fprintf(out, "%-20s  %-10s  %-8s  %-7s  %s\n", ts, e.Type, hash, status, dur)
	}
	return nil
}
