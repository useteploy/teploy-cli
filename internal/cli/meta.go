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
	"github.com/useteploy/teploy/internal/docker"
	"github.com/useteploy/teploy/internal/releasemeta"
	"github.com/useteploy/teploy/internal/ssh"
)

func newMetaCmd(flags *Flags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "meta",
		Short: "Inspect and maintain per-release deploy metadata on a server",
	}
	cmd.AddCommand(newMetaScrubCmd(flags))
	return cmd
}

func newMetaScrubCmd(flags *Flags) *cobra.Command {
	var app string
	var apply bool
	cmd := &cobra.Command{
		Use:   "scrub",
		Short: "Remove secret env values from release metadata written by older teploy versions",
		Long: "Release records (/deployments/<app>/meta/<release>.json) written before this\n" +
			"version stored every container env value, secrets included. scrub rewrites\n" +
			"them to the current shape: plain values whose source is provably non-secret\n" +
			"stay, every other value is replaced by its key name and source. It also\n" +
			"removes leftover attempt env files (meta/att/*/env, plaintext copies of\n" +
			"resolved env older than 30 minutes).\n\n" +
			"Dry run by default: reports what would change and changes nothing. --apply\n" +
			"rewrites, taking each app's deploy lock (a locked app is skipped and\n" +
			"reported). Output names keys and paths, never values. Safe to re-run.\n\n" +
			"Rollback is unaffected: it recreates containers from docker's own record\n" +
			"of them, not from release metadata.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMetaScrub(flags, app, apply, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&app, "app", "", "only this app (default: every app on the host)")
	cmd.Flags().BoolVar(&apply, "apply", false, "rewrite records and remove attempt env files (default: dry run)")
	return cmd
}

func runMetaScrub(flags *Flags, app string, apply bool, out io.Writer) error {
	if flags.Host == "" {
		return fmt.Errorf("--host is required")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	host, user, key, err := config.ResolveServer(flags.Host, flags.Host, flags.User, flags.Key)
	if err != nil {
		return fmt.Errorf("resolving host: %w", err)
	}
	executor, err := ssh.Connect(ctx, ssh.ConnectConfig{Host: host, User: user, KeyPath: key})
	if err != nil {
		return fmt.Errorf("connecting to %s: %w", host, err)
	}
	defer executor.Close()

	report, err := releasemeta.Scrub(ctx, executor, docker.NewClient(executor), releasemeta.ScrubOptions{App: app, Apply: apply})
	if err != nil {
		return err
	}
	if flags.JSON {
		return json.NewEncoder(out).Encode(report)
	}
	writeScrubReport(out, report)
	if len(report.Skipped) > 0 && apply {
		return fmt.Errorf("%d item(s) skipped — see above", len(report.Skipped))
	}
	return nil
}

func writeScrubReport(out io.Writer, r *releasemeta.ScrubReport) {
	mode := "Dry run (nothing changed; rerun with --apply)"
	if r.Apply {
		mode = "Applied"
	}
	fmt.Fprintf(out, "%s: %d release record(s) scanned, %d carry secret-capable env values, %d attempt env file(s)\n",
		mode, r.RecordsScanned, len(r.Records), len(r.AttemptEnvFiles))
	for _, rec := range r.Records {
		state := "would rewrite"
		if rec.Rewritten {
			state = "rewrote"
		}
		fmt.Fprintf(out, "  %s %s — %d key(s): %s\n", state, rec.Path, len(rec.RedactedKeys), strings.Join(rec.RedactedKeys, ", "))
	}
	for _, f := range r.AttemptEnvFiles {
		verb := "would remove"
		if r.Apply {
			verb = "removed"
		}
		fmt.Fprintf(out, "  %s %s\n", verb, f)
	}
	for _, s := range r.Skipped {
		fmt.Fprintf(out, "  skipped: %s\n", s)
	}
}
