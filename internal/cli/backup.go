package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"

	"github.com/spf13/cobra"
	"github.com/useteploy/teploy/internal/backup"
	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/notify"
	"github.com/useteploy/teploy/internal/ssh"
)

func newBackupCmd(flags *Flags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "backup",
		Short: "Back up app volumes to S3",
	}

	cmd.AddCommand(newBackupCreateCmd(flags))
	cmd.AddCommand(newBackupListCmd(flags))
	cmd.AddCommand(newBackupRestoreCmd(flags))
	cmd.AddCommand(newBackupPruneCmd(flags))
	cmd.AddCommand(newBackupScheduleCmd(flags))

	return cmd
}

// addRetentionFlags registers the shared --keep-last / --max-age-days flags.
func addRetentionFlags(cmd *cobra.Command, keepLast, maxAgeDays *int) {
	cmd.Flags().IntVar(keepLast, "keep-last", 0, "Keep only the N most recent backups (0 = keep all)")
	cmd.Flags().IntVar(maxAgeDays, "max-age-days", 0, "Delete backups older than N days (0 = no age limit; pair with --keep-last as a floor)")
}

func newBackupCreateCmd(flags *Flags) *cobra.Command {
	var (
		bucket     string
		region     string
		endpoint   string
		keepLast   int
		maxAgeDays int
	)

	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a volume backup",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBackupCreate(flags, bucket, region, endpoint, keepLast, maxAgeDays)
		},
	}

	cmd.Flags().StringVar(&bucket, "bucket", "", "S3 bucket name")
	cmd.Flags().StringVar(&region, "region", "us-east-1", "AWS region")
	cmd.Flags().StringVar(&endpoint, "endpoint", "", "S3-compatible endpoint URL (MinIO/B2/R2); creds from TEPLOY_S3_ACCESS_KEY/SECRET_KEY or AWS_* env")
	addRetentionFlags(cmd, &keepLast, &maxAgeDays)

	return cmd
}

// s3Config assembles the backup target. --endpoint switches the server-side
// aws CLI to an S3-compatible endpoint (a MinIO accessory, B2, R2, Bunny).
// Credentials come from the LOCAL environment — TEPLOY_S3_ACCESS_KEY /
// TEPLOY_S3_SECRET_KEY, falling back to AWS_ACCESS_KEY_ID /
// AWS_SECRET_ACCESS_KEY — and ride inline on the remote aws command for
// that invocation only; nothing is persisted server-side. Unset = the
// server's ambient AWS credentials, exactly as before.
func s3Config(bucket, region, endpoint string) backup.S3Config {
	access := os.Getenv("TEPLOY_S3_ACCESS_KEY")
	if access == "" {
		access = os.Getenv("AWS_ACCESS_KEY_ID")
	}
	secret := os.Getenv("TEPLOY_S3_SECRET_KEY")
	if secret == "" {
		secret = os.Getenv("AWS_SECRET_ACCESS_KEY")
	}
	// Only pass creds inline when a custom endpoint is in play — for plain
	// AWS the server's own credential chain keeps working untouched.
	if endpoint == "" {
		return backup.S3Config{Bucket: bucket, Region: region}
	}
	return backup.S3Config{Bucket: bucket, Region: region, Endpoint: endpoint, AccessKey: access, SecretKey: secret}
}

func runBackupCreate(flags *Flags, bucket, region, endpoint string, keepLast, maxAgeDays int) error {
	appCfg, err := config.LoadApp(".")
	if err != nil {
		return err
	}

	if bucket == "" {
		return fmt.Errorf("--bucket is required")
	}
	if err := backup.ValidateBucket(bucket); err != nil {
		return err
	}
	if err := backup.ValidateRegion(region); err != nil {
		return err
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	executor, err := connectForApp(ctx, flags, appCfg)
	if err != nil {
		return err
	}
	defer executor.Close()

	client := backup.NewClient(executor, os.Stdout)
	cfg := s3Config(bucket, region, endpoint)
	err = client.BackupVolumes(ctx, appCfg.App, cfg)

	// Enforce retention only after a successful backup — never prune when the
	// fresh backup failed (that could leave zero good copies).
	var pruned []string
	var pruneErr error
	policy := backup.RetentionPolicy{KeepLast: keepLast, MaxAgeDays: maxAgeDays}
	if err == nil && !policy.IsZero() {
		pruned, pruneErr = client.PruneBackups(ctx, appCfg.App, "volumes", cfg, policy)
	}

	// Fire notification (fire-and-forget). The backup itself succeeding is what
	// Success reflects; a prune failure is surfaced in the message + exit code
	// but does not flip the backup to "failed" (the data is safely stored).
	if n := buildNotifier(appCfg); n != nil {
		msg := fmt.Sprintf("Backup created for %s", appCfg.App)
		switch {
		case err != nil:
			msg = fmt.Sprintf("Backup failed for %s: %s", appCfg.App, err)
		case pruneErr != nil:
			msg = fmt.Sprintf("Backup created for %s, but prune failed: %s", appCfg.App, pruneErr)
		case len(pruned) > 0:
			msg = fmt.Sprintf("Backup created for %s (pruned %d old backup(s))", appCfg.App, len(pruned))
		}
		n.Send(ctx, notify.Payload{
			App:     appCfg.App,
			Server:  executor.Host(),
			Type:    "backup",
			Success: err == nil,
			Message: msg,
		})
	}

	if err != nil {
		return err
	}
	return pruneErr
}

func newBackupListCmd(flags *Flags) *cobra.Command {
	var (
		bucket   string
		region   string
		endpoint string
	)

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List available backups",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBackupList(flags, bucket, region, endpoint)
		},
	}

	cmd.Flags().StringVar(&bucket, "bucket", "", "S3 bucket name")
	cmd.Flags().StringVar(&region, "region", "us-east-1", "AWS region")
	cmd.Flags().StringVar(&endpoint, "endpoint", "", "S3-compatible endpoint URL (MinIO/B2/R2); creds from TEPLOY_S3_ACCESS_KEY/SECRET_KEY or AWS_* env")

	return cmd
}

func runBackupList(flags *Flags, bucket, region, endpoint string) error {
	appCfg, err := config.LoadApp(".")
	if err != nil {
		return err
	}

	if bucket == "" {
		return fmt.Errorf("--bucket is required")
	}
	if err := backup.ValidateBucket(bucket); err != nil {
		return err
	}
	if err := backup.ValidateRegion(region); err != nil {
		return err
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	executor, err := connectForApp(ctx, flags, appCfg)
	if err != nil {
		return err
	}
	defer executor.Close()

	client := backup.NewClient(executor, os.Stdout)
	backups, err := client.ListBackups(ctx, appCfg.App, "volumes", s3Config(bucket, region, endpoint))
	if err != nil {
		return err
	}
	if flags.JSON {
		type backupDTO struct {
			Name string `json:"name"`
		}
		result := make([]backupDTO, 0, len(backups))
		for _, item := range backups {
			result = append(result, backupDTO{Name: item})
		}
		return json.NewEncoder(os.Stdout).Encode(result)
	}

	if len(backups) == 0 {
		fmt.Println("No backups found")
		return nil
	}

	for _, b := range backups {
		fmt.Println(b)
	}
	return nil
}

func newBackupRestoreCmd(flags *Flags) *cobra.Command {
	var (
		bucket   string
		region   string
		endpoint string
	)

	cmd := &cobra.Command{
		Use:   "restore <date>",
		Short: "Restore a volume backup",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBackupRestore(flags, args[0], bucket, region, endpoint)
		},
	}

	cmd.Flags().StringVar(&bucket, "bucket", "", "S3 bucket name")
	cmd.Flags().StringVar(&region, "region", "us-east-1", "AWS region")
	cmd.Flags().StringVar(&endpoint, "endpoint", "", "S3-compatible endpoint URL (MinIO/B2/R2); creds from TEPLOY_S3_ACCESS_KEY/SECRET_KEY or AWS_* env")

	return cmd
}

func newBackupScheduleCmd(flags *Flags) *cobra.Command {
	var (
		bucket   string
		region   string
		endpoint string
		keepLast int
	)

	cmd := &cobra.Command{
		Use:   "schedule <cron>",
		Short: "Set up automated backups on a cron schedule",
		Long:  "Creates a cron job on the server to run backups automatically.\nExample: teploy backup schedule \"0 3 * * *\" --bucket my-backups --keep-last 7",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBackupSchedule(flags, args[0], bucket, region, endpoint, keepLast)
		},
	}

	cmd.Flags().StringVar(&bucket, "bucket", "", "S3 bucket name")
	cmd.Flags().StringVar(&region, "region", "us-east-1", "AWS region")
	cmd.Flags().StringVar(&endpoint, "endpoint", "", "S3-compatible endpoint URL (MinIO/B2/R2); creds from TEPLOY_S3_ACCESS_KEY/SECRET_KEY or AWS_* env")
	cmd.Flags().IntVar(&keepLast, "keep-last", 0, "Keep only the N most recent backups on each run (0 = keep all)")

	return cmd
}

func runBackupSchedule(flags *Flags, schedule, bucket, region, endpoint string, keepLast int) error {
	appCfg, err := config.LoadApp(".")
	if err != nil {
		return err
	}

	if bucket == "" {
		return fmt.Errorf("--bucket is required")
	}
	if err := backup.ValidateBucket(bucket); err != nil {
		return err
	}
	if err := backup.ValidateRegion(region); err != nil {
		return err
	}
	if err := backup.ValidateSchedule(schedule); err != nil {
		return err
	}
	if keepLast < 0 {
		return fmt.Errorf("--keep-last must be >= 0")
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	executor, err := connectForApp(ctx, flags, appCfg)
	if err != nil {
		return err
	}
	defer executor.Close()

	webhook := firstWebhookURL(appCfg.Notifications)
	backupCmd := buildScheduledBackupCmd(appCfg.App, executor.Host(), bucket, region, keepLast, webhook)

	client := backup.NewClient(executor, os.Stdout)
	if err := client.SetSchedule(ctx, schedule, backupCmd, "teploy-backup:"+appCfg.App); err != nil {
		return err
	}

	fmt.Printf("Backup scheduled: %s\n", schedule)
	fmt.Printf("  App: %s\n", appCfg.App)
	fmt.Printf("  Bucket: s3://%s/%s/volumes/\n", bucket, appCfg.App)
	if keepLast > 0 {
		fmt.Printf("  Retention: keep last %d\n", keepLast)
	}
	if webhook != "" {
		fmt.Println("  Alerts: webhook notified on failure")
	}
	return nil
}

// firstWebhookURL returns the first usable webhook URL from the app's
// notifications config (legacy single webhook, else the first webhook channel),
// or "" if none. Only webhook channels work from the headless cron path — SMTP
// and other channels need the CLI's notifier, which isn't present server-side.
func firstWebhookURL(n config.NotificationsConfig) string {
	if n.Webhook != "" {
		return n.Webhook
	}
	for _, ch := range n.Channels {
		if ch.Type == "webhook" && ch.URL != "" {
			return ch.URL
		}
	}
	return ""
}

// buildScheduledBackupCmd assembles the shell command cron runs for a scheduled
// backup: archive → upload → clean up, then optional keep-last retention, and
// (if a webhook is set) a failure alert wrapping the whole chain.
func buildScheduledBackupCmd(app, server, bucket, region string, keepLast int, webhook string) string {
	cmd := fmt.Sprintf(
		"tar -czf /tmp/%s-backup-$(date +%%Y%%m%%d-%%H%%M%%S).tar.gz -C /deployments/%s/volumes . && "+
			"aws s3 cp /tmp/%s-backup-*.tar.gz s3://%s/%s/volumes/ --region %s && "+
			"rm -f /tmp/%s-backup-*.tar.gz",
		app, app,
		app, bucket, app, region,
		app,
	)

	// Bake keep-last retention into the same cron job: after the fresh upload,
	// list the timestamped keys, keep the newest N, delete the rest. The names
	// sort chronologically (20060102-150405), so `head -n -N` yields exactly the
	// stale ones. No `%` in this clause, so no cron %-escaping concerns. Only
	// keep-last is supported in the scheduled shell path; --max-age-days lives on
	// `backup create`/`backup prune` (shell date math is fragile).
	if keepLast > 0 {
		cmd += fmt.Sprintf(
			" && for f in $(aws s3 ls s3://%s/%s/volumes/ --region %s | sed 's/.* //' | grep -E '[0-9]{8}-[0-9]{6}' | sort | head -n -%d); do "+
				"[ -n \"$f\" ] && aws s3 rm s3://%s/%s/volumes/$f --region %s; done",
			bucket, app, region, keepLast,
			bucket, app, region,
		)
	}

	// Failure alerting: a scheduled backup runs headless, so it can't use the
	// CLI notifier (teploy.yml + its config aren't on the server). If a webhook
	// is configured, POST a failure payload — matching notify.Payload's schema —
	// when any step of the chain fails. Deliberately `%`-free (no $(date) with a
	// format) so it can't trip cron's %-escaping; the receiver stamps its own
	// receipt time. `; false` preserves the non-zero exit so cron still logs it.
	if webhook != "" {
		payload := fmt.Sprintf(
			`{"app":%q,"server":%q,"type":"backup","success":false,"message":"Scheduled backup failed","duration_ms":0,"timestamp":""}`,
			app, server,
		)
		alert := fmt.Sprintf(
			"curl -sf -m 10 -X POST -H 'Content-Type: application/json' -d %s %s",
			ssh.ShellQuote(payload), ssh.ShellQuote(webhook),
		)
		cmd = "( " + cmd + " ) || { " + alert + "; false; }"
	}

	return cmd
}

func runBackupRestore(flags *Flags, date, bucket, region, endpoint string) error {
	appCfg, err := config.LoadApp(".")
	if err != nil {
		return err
	}

	if bucket == "" {
		return fmt.Errorf("--bucket is required")
	}
	if err := backup.ValidateBucket(bucket); err != nil {
		return err
	}
	if err := backup.ValidateRegion(region); err != nil {
		return err
	}
	if err := backup.ValidateDate(date); err != nil {
		return err
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	executor, err := connectForApp(ctx, flags, appCfg)
	if err != nil {
		return err
	}
	defer executor.Close()

	client := backup.NewClient(executor, os.Stdout)
	return client.RestoreVolumes(ctx, appCfg.App, date, s3Config(bucket, region, endpoint))
}

func newBackupPruneCmd(flags *Flags) *cobra.Command {
	var (
		bucket     string
		region     string
		endpoint   string
		accessory  string
		keepLast   int
		maxAgeDays int
	)

	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Delete old backups per a retention policy",
		Long:  "Removes backups outside the retention policy.\nExample: teploy backup prune --bucket my-backups --keep-last 7\n         teploy backup prune --bucket my-backups --max-age-days 30 --keep-last 3",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBackupPrune(flags, bucket, region, endpoint, accessory, keepLast, maxAgeDays)
		},
	}

	cmd.Flags().StringVar(&bucket, "bucket", "", "S3 bucket name")
	cmd.Flags().StringVar(&region, "region", "us-east-1", "AWS region")
	cmd.Flags().StringVar(&endpoint, "endpoint", "", "S3-compatible endpoint URL (MinIO/B2/R2); creds from TEPLOY_S3_ACCESS_KEY/SECRET_KEY or AWS_* env")
	cmd.Flags().StringVar(&accessory, "accessory", "", "Prune an accessory's backups instead of volumes (accessory name)")
	addRetentionFlags(cmd, &keepLast, &maxAgeDays)

	return cmd
}

func runBackupPrune(flags *Flags, bucket, region, endpoint, accessory string, keepLast, maxAgeDays int) error {
	appCfg, err := config.LoadApp(".")
	if err != nil {
		return err
	}

	if bucket == "" {
		return fmt.Errorf("--bucket is required")
	}
	if err := backup.ValidateBucket(bucket); err != nil {
		return err
	}
	if err := backup.ValidateRegion(region); err != nil {
		return err
	}

	policy := backup.RetentionPolicy{KeepLast: keepLast, MaxAgeDays: maxAgeDays}
	if policy.IsZero() {
		return fmt.Errorf("nothing to prune: set --keep-last and/or --max-age-days")
	}

	prefix := "volumes"
	if accessory != "" {
		// Accessory name lands in an unquoted S3 path (ListBackups) — reuse the
		// same safe-name check as buckets to keep it shell-safe.
		if err := backup.ValidateBucket(accessory); err != nil {
			return fmt.Errorf("invalid accessory name %q: %w", accessory, err)
		}
		prefix = "accessories/" + accessory
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	executor, err := connectForApp(ctx, flags, appCfg)
	if err != nil {
		return err
	}
	defer executor.Close()

	client := backup.NewClient(executor, os.Stdout)
	deleted, err := client.PruneBackups(ctx, appCfg.App, prefix, s3Config(bucket, region, endpoint), policy)
	if err != nil {
		return err
	}

	if len(deleted) == 0 {
		fmt.Println("No backups pruned (all within retention policy)")
	} else {
		fmt.Printf("Pruned %d backup(s)\n", len(deleted))
	}
	return nil
}
