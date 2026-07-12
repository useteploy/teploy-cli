package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"

	"github.com/spf13/cobra"
	"github.com/useteploy/teploy/internal/backup"
	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/notify"
)

func newBackupCmd(flags *Flags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "backup",
		Short: "Back up app volumes to S3",
	}

	cmd.AddCommand(newBackupCreateCmd(flags))
	cmd.AddCommand(newBackupListCmd(flags))
	cmd.AddCommand(newBackupRestoreCmd(flags))
	cmd.AddCommand(newBackupScheduleCmd(flags))

	return cmd
}

func newBackupCreateCmd(flags *Flags) *cobra.Command {
	var (
		bucket   string
		region   string
		endpoint string
	)

	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a volume backup",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBackupCreate(flags, bucket, region, endpoint)
		},
	}

	cmd.Flags().StringVar(&bucket, "bucket", "", "S3 bucket name")
	cmd.Flags().StringVar(&region, "region", "us-east-1", "AWS region")
	cmd.Flags().StringVar(&endpoint, "endpoint", "", "S3-compatible endpoint URL (MinIO/B2/R2); creds from TEPLOY_S3_ACCESS_KEY/SECRET_KEY or AWS_* env")

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

func runBackupCreate(flags *Flags, bucket, region, endpoint string) error {
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
	err = client.BackupVolumes(ctx, appCfg.App, s3Config(bucket, region, endpoint))

	// Fire notification (fire-and-forget).
	if n := buildNotifier(appCfg); n != nil {
		msg := fmt.Sprintf("Backup created for %s", appCfg.App)
		if err != nil {
			msg = fmt.Sprintf("Backup failed for %s: %s", appCfg.App, err)
		}
		n.Send(ctx, notify.Payload{
			App:     appCfg.App,
			Server:  executor.Host(),
			Type:    "backup",
			Success: err == nil,
			Message: msg,
		})
	}

	return err
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
	)

	cmd := &cobra.Command{
		Use:   "schedule <cron>",
		Short: "Set up automated backups on a cron schedule",
		Long:  "Creates a cron job on the server to run backups automatically.\nExample: teploy backup schedule \"0 3 * * *\" --bucket my-backups",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBackupSchedule(flags, args[0], bucket, region, endpoint)
		},
	}

	cmd.Flags().StringVar(&bucket, "bucket", "", "S3 bucket name")
	cmd.Flags().StringVar(&region, "region", "us-east-1", "AWS region")
	cmd.Flags().StringVar(&endpoint, "endpoint", "", "S3-compatible endpoint URL (MinIO/B2/R2); creds from TEPLOY_S3_ACCESS_KEY/SECRET_KEY or AWS_* env")

	return cmd
}

func runBackupSchedule(flags *Flags, schedule, bucket, region, endpoint string) error {
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

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	executor, err := connectForApp(ctx, flags, appCfg)
	if err != nil {
		return err
	}
	defer executor.Close()

	// Build the backup command that cron will run.
	backupCmd := fmt.Sprintf(
		"tar -czf /tmp/%s-backup-$(date +%%Y%%m%%d-%%H%%M%%S).tar.gz -C /deployments/%s/volumes . && "+
			"aws s3 cp /tmp/%s-backup-*.tar.gz s3://%s/%s/volumes/ --region %s && "+
			"rm -f /tmp/%s-backup-*.tar.gz",
		appCfg.App, appCfg.App,
		appCfg.App, bucket, appCfg.App, region,
		appCfg.App,
	)

	client := backup.NewClient(executor, os.Stdout)
	if err := client.SetSchedule(ctx, schedule, backupCmd, "teploy-backup:"+appCfg.App); err != nil {
		return err
	}

	fmt.Printf("Backup scheduled: %s\n", schedule)
	fmt.Printf("  App: %s\n", appCfg.App)
	fmt.Printf("  Bucket: s3://%s/%s/volumes/\n", bucket, appCfg.App)
	return nil
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
