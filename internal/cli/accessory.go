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
	"github.com/useteploy/teploy/internal/accessories"
	"github.com/useteploy/teploy/internal/backup"
	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/docker"
	"github.com/useteploy/teploy/internal/ssh"
)

// destination holds the -d / --destination flag value, set as a persistent
// flag on the accessory parent command so every subcommand can layer a
// destination overlay (teploy.<dest>.yml) on top of teploy.yml. Without
// this, accessory commands couldn't read overlay-supplied env values such
// as accessory POSTGRES_PASSWORD.
var accessoryDestination string

// loadAppCfgForAccessory loads teploy.yml and merges teploy.<dest>.yml on
// top when -d is supplied. Falls back to plain LoadApp otherwise.
func loadAppCfgForAccessory() (*config.AppConfig, error) {
	if accessoryDestination != "" {
		return config.LoadAppWithDestination(".", accessoryDestination)
	}
	return config.LoadApp(".")
}

// resolveAppForAccessory resolves the app config + an SSH connection for an
// accessory subcommand. With --app it reads server-side state (no teploy.yml
// needed — the path teploy-dash uses); without, it loads teploy.yml honoring
// the -d destination overlay. Only stop/start/logs use this: they address
// {app}-{accessory} containers by name. upgrade/backup/restore need accessory
// image config from teploy.yml and stay cwd-bound.
func resolveAppForAccessory(ctx context.Context, flags *Flags, appName string) (*config.AppConfig, ssh.Executor, error) {
	if appName != "" {
		return resolveApp(ctx, flags, appName)
	}
	appCfg, err := loadAppCfgForAccessory()
	if err != nil {
		return nil, nil, err
	}
	ex, err := connectForApp(ctx, flags, appCfg)
	if err != nil {
		return nil, nil, err
	}
	return appCfg, ex, nil
}

func newAccessoryCmd(flags *Flags) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "accessory",
		Aliases: []string{"acc"},
		Short:   "Manage accessories (databases, caches)",
	}

	cmd.PersistentFlags().StringVarP(&accessoryDestination, "destination", "d", "", "destination overlay (e.g. prod merges teploy.prod.yml)")

	cmd.AddCommand(newAccessoryListCmd(flags))
	cmd.AddCommand(newAccessoryExecCmd(flags))
	cmd.AddCommand(newAccessoryStopCmd(flags))
	cmd.AddCommand(newAccessoryStartCmd(flags))
	cmd.AddCommand(newAccessoryLogsCmd(flags))
	cmd.AddCommand(newAccessoryUpgradeCmd(flags))
	cmd.AddCommand(newAccessoryBackupCmd(flags))
	cmd.AddCommand(newAccessoryRestoreCmd(flags))
	cmd.AddCommand(newAccessoryVerifyBackupCmd(flags))

	return cmd
}

func newAccessoryExecCmd(flags *Flags) *cobra.Command {
	var appName string
	cmd := &cobra.Command{
		Use:   "exec <name> [command...]",
		Short: "Run a command in an accessory container",
		Long: "Run a one-off command in an accessory's running container — e.g. a query\n" +
			"against a database accessory. The command runs via the container's shell, and a\n" +
			"non-zero exit reports failure.\n\n" +
			"Examples:\n" +
			"  teploy accessory exec db -- psql -U postgres -c 'SELECT 1'\n" +
			"  teploy accessory exec cache -- redis-cli INFO",
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAccessoryExec(flags, appName, args[0], args[1:])
		},
	}
	cmd.Flags().StringVar(&appName, "app", "", "app name — act on server state instead of teploy.yml (requires --host)")
	return cmd
}

func runAccessoryExec(flags *Flags, appName, name string, args []string) error {
	if err := config.ValidateIdentifier("accessory", name); err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	appCfg, executor, err := resolveAppForAccessory(ctx, flags, appName)
	if err != nil {
		return err
	}
	defer executor.Close()

	dk := docker.NewClient(executor)
	container := accessories.ContainerName(appCfg.App, name)
	command := quoteExecArgs(args)
	if err := dk.ExecStream(ctx, container, command, os.Stdout, os.Stderr); err != nil {
		return fmt.Errorf("command failed in %s: %w", container, err)
	}
	return nil
}

// quoteExecArgs rebuilds a shell command line from cobra-parsed args,
// preserving the argument boundaries cobra already resolved. A bare
// strings.Join(args, " ") loses those boundaries (e.g. "-c" "SELECT 1" as
// two argv entries collapse into one ambiguous string), so when the joined
// string later gets re-split by the remote `sh -c` in
// docker.Client.ExecStream, "SELECT 1" silently becomes two words
// ("SELECT" and "1") instead of staying one argument. Found live running
// exactly the example in this command's own --help text
// (`psql -c 'SELECT 1'`) — psql reported "extra command-line argument
// \"1\" ignored". Trades away shell-operator passthrough (pipes,
// redirects) for correctness on quoted multi-word arguments — the right
// tradeoff here since this command's own documented use case is
// single-command invocations with flag values like -c, not pipelines
// (unlike `teploy app exec`, which explicitly promises pipe/redirect
// support and is intentionally NOT quoted this way).
func quoteExecArgs(args []string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = ssh.ShellQuote(a)
	}
	return strings.Join(quoted, " ")
}

func newAccessoryListCmd(flags *Flags) *cobra.Command {
	var appName string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List accessory containers",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAccessoryList(flags, appName)
		},
	}
	// `list` only needs the app name (read-only container listing), so it
	// supports --app for use outside an app directory (e.g. teploy-dash).
	// The other accessory subcommands need accessory config from teploy.yml
	// and remain cwd-bound.
	cmd.Flags().StringVar(&appName, "app", "", "app name — act on server state instead of teploy.yml (requires --host)")
	return cmd
}

func runAccessoryList(flags *Flags, appName string) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	appCfg, executor, err := resolveApp(ctx, flags, appName)
	if err != nil {
		return err
	}
	defer executor.Close()

	mgr := accessories.NewManager(executor, os.Stdout)
	containers, err := mgr.List(ctx, appCfg.App)
	if err != nil {
		return err
	}

	if len(containers) == 0 {
		fmt.Println("No accessories running")
		return nil
	}

	for _, c := range containers {
		fmt.Printf("%-30s %-20s %-10s %s\n", c.Name, c.Image, c.State, c.Status)
	}
	return nil
}

func newAccessoryStopCmd(flags *Flags) *cobra.Command {
	var appName string
	cmd := &cobra.Command{
		Use:   "stop <name>",
		Short: "Stop an accessory container",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAccessoryStop(flags, appName, args[0])
		},
	}
	cmd.Flags().StringVar(&appName, "app", "", "app name — act on server state instead of teploy.yml (requires --host)")
	return cmd
}

func runAccessoryStop(flags *Flags, appName, name string) error {
	if err := config.ValidateIdentifier("accessory", name); err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	appCfg, executor, err := resolveAppForAccessory(ctx, flags, appName)
	if err != nil {
		return err
	}
	defer executor.Close()

	mgr := accessories.NewManager(executor, os.Stdout)
	fmt.Printf("Stopping %s...\n", accessories.ContainerName(appCfg.App, name))
	if err := mgr.Stop(ctx, appCfg.App, name); err != nil {
		return err
	}
	fmt.Println("Stopped")
	return nil
}

func newAccessoryStartCmd(flags *Flags) *cobra.Command {
	var appName string
	cmd := &cobra.Command{
		Use:   "start <name>",
		Short: "Start a stopped accessory container",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAccessoryStart(flags, appName, args[0])
		},
	}
	cmd.Flags().StringVar(&appName, "app", "", "app name — act on server state instead of teploy.yml (requires --host)")
	return cmd
}

func runAccessoryStart(flags *Flags, appName, name string) error {
	if err := config.ValidateIdentifier("accessory", name); err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	appCfg, executor, err := resolveAppForAccessory(ctx, flags, appName)
	if err != nil {
		return err
	}
	defer executor.Close()

	mgr := accessories.NewManager(executor, os.Stdout)
	fmt.Printf("Starting %s...\n", accessories.ContainerName(appCfg.App, name))
	if err := mgr.Start(ctx, appCfg.App, name); err != nil {
		return err
	}
	fmt.Println("Started")
	return nil
}

func newAccessoryLogsCmd(flags *Flags) *cobra.Command {
	var lines int
	var appName string

	cmd := &cobra.Command{
		Use:   "logs <name>",
		Short: "Show accessory container logs",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAccessoryLogs(flags, appName, args[0], lines)
		},
	}

	cmd.Flags().IntVar(&lines, "lines", 50, "number of log lines to show (--tail is an alias)")
	cmd.Flags().StringVar(&appName, "app", "", "app name — act on server state instead of teploy.yml (requires --host)")
	cmd.Flags().SetNormalizeFunc(tailToLines)

	return cmd
}

func runAccessoryLogs(flags *Flags, appName, name string, lines int) error {
	if err := config.ValidateIdentifier("accessory", name); err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	appCfg, executor, err := resolveAppForAccessory(ctx, flags, appName)
	if err != nil {
		return err
	}
	defer executor.Close()

	mgr := accessories.NewManager(executor, os.Stdout)
	return mgr.Logs(ctx, appCfg.App, name, lines)
}

func newAccessoryUpgradeCmd(flags *Flags) *cobra.Command {
	return &cobra.Command{
		Use:   "upgrade <name> <new_image>",
		Short: "Upgrade an accessory to a new image",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAccessoryUpgrade(flags, args[0], args[1])
		},
	}
}

func runAccessoryUpgrade(flags *Flags, name, newImage string) error {
	appCfg, err := loadAppCfgForAccessory()
	if err != nil {
		return err
	}

	accCfg, ok := appCfg.Accessories[name]
	if !ok {
		return fmt.Errorf("accessory %q not found in teploy.yml", name)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	executor, err := connectForApp(ctx, flags, appCfg)
	if err != nil {
		return err
	}
	defer executor.Close()

	mgr := accessories.NewManager(executor, os.Stdout)
	return mgr.Upgrade(ctx, appCfg.App, name, newImage, accCfg)
}

func newAccessoryBackupCmd(flags *Flags) *cobra.Command {
	var (
		bucket   string
		region   string
		endpoint string
		schedule string
	)

	cmd := &cobra.Command{
		Use:   "backup <name>",
		Short: "Back up an accessory (database-aware dump)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAccessoryBackup(flags, args[0], bucket, region, endpoint, schedule)
		},
	}

	cmd.Flags().StringVar(&bucket, "bucket", "", "S3 bucket name")
	cmd.Flags().StringVar(&region, "region", "us-east-1", "AWS region")
	cmd.Flags().StringVar(&endpoint, "endpoint", "", "S3-compatible endpoint URL (MinIO/B2/R2); creds from TEPLOY_S3_ACCESS_KEY/SECRET_KEY or AWS_* env")
	cmd.Flags().StringVar(&schedule, "schedule", "", "cron schedule for automated backups")

	return cmd
}

func runAccessoryBackup(flags *Flags, name, bucket, region, endpoint, schedule string) error {
	appCfg, err := loadAppCfgForAccessory()
	if err != nil {
		return err
	}

	accCfg, ok := appCfg.Accessories[name]
	if !ok {
		return fmt.Errorf("accessory %q not found in teploy.yml", name)
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
	if schedule != "" {
		if err := backup.ValidateSchedule(schedule); err != nil {
			return err
		}
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	executor, err := connectForApp(ctx, flags, appCfg)
	if err != nil {
		return err
	}
	defer executor.Close()

	client := backup.NewClient(executor, os.Stdout)
	s3Cfg := s3Config(bucket, region, endpoint)

	if schedule != "" {
		backupCmd := fmt.Sprintf("teploy accessory backup %s --bucket %s --region %s", name, bucket, region)
		if endpoint != "" {
			// The cron job runs server-side where the local TEPLOY_S3_* env
			// doesn't exist — embed endpoint + creds in the crontab line
			// (root-only readable, same trust class as ~/.aws/credentials).
			backupCmd = fmt.Sprintf("TEPLOY_S3_ACCESS_KEY=%s TEPLOY_S3_SECRET_KEY=%s %s --endpoint %s",
				ssh.ShellQuote(s3Cfg.AccessKey), ssh.ShellQuote(s3Cfg.SecretKey), backupCmd, ssh.ShellQuote(endpoint))
		}
		if err := client.SetSchedule(ctx, schedule, backupCmd, "teploy-accessory-backup:"+appCfg.App+":"+name); err != nil {
			return err
		}
		fmt.Printf("Scheduled backup: %s\n", schedule)
		return nil
	}

	return client.AccessoryBackup(ctx, appCfg.App, name, accCfg.Image, accCfg.Env, s3Cfg)
}

func newAccessoryRestoreCmd(flags *Flags) *cobra.Command {
	var (
		bucket   string
		region   string
		endpoint string
	)

	cmd := &cobra.Command{
		Use:   "restore <name> <date>",
		Short: "Restore an accessory from backup",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAccessoryRestore(flags, args[0], args[1], bucket, region, endpoint)
		},
	}

	cmd.Flags().StringVar(&bucket, "bucket", "", "S3 bucket name")
	cmd.Flags().StringVar(&region, "region", "us-east-1", "AWS region")
	cmd.Flags().StringVar(&endpoint, "endpoint", "", "S3-compatible endpoint URL (MinIO/B2/R2); creds from TEPLOY_S3_ACCESS_KEY/SECRET_KEY or AWS_* env")

	return cmd
}

func runAccessoryRestore(flags *Flags, name, date, bucket, region, endpoint string) error {
	appCfg, err := loadAppCfgForAccessory()
	if err != nil {
		return err
	}

	accCfg, ok := appCfg.Accessories[name]
	if !ok {
		return fmt.Errorf("accessory %q not found in teploy.yml", name)
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
	s3Cfg := s3Config(bucket, region, endpoint)
	return client.AccessoryRestore(ctx, appCfg.App, name, accCfg.Image, date, accCfg.Env, s3Cfg)
}

func newAccessoryVerifyBackupCmd(flags *Flags) *cobra.Command {
	var (
		appName  string
		bucket   string
		region   string
		date     string
		endpoint string
	)

	cmd := &cobra.Command{
		Use:   "verify-backup <name>",
		Short: "Restore a backup into a scratch container and verify it's usable",
		Long: "Downloads the latest backup (or --date) for an accessory, restores it into a\n" +
			"throwaway scratch container, and verifies the restored copy is actually usable\n" +
			"(tables exist, engine boots, keys load). The running accessory is never touched;\n" +
			"the scratch container is always removed. Proves backups aren't write-only.\n\n" +
			"Reads the accessory's image and env from the running container, so it works from\n" +
			"server state alone: pass --app + --host to run without a teploy.yml (the mode\n" +
			"teploy-dash and cron use).\n\n" +
			"Examples:\n" +
			"  teploy accessory verify-backup postgres --bucket my-backups\n" +
			"  teploy accessory verify-backup db --app myapp --host 1.2.3.4 --bucket b --json",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAccessoryVerifyBackup(flags, appName, args[0], bucket, region, date, endpoint)
		},
	}
	cmd.Flags().StringVar(&appName, "app", "", "app name — act on server state instead of teploy.yml (requires --host)")
	cmd.Flags().StringVar(&bucket, "bucket", "", "S3 bucket name")
	cmd.Flags().StringVar(&region, "region", "us-east-1", "AWS region")
	cmd.Flags().StringVar(&endpoint, "endpoint", "", "S3-compatible endpoint URL (MinIO/B2/R2); creds from TEPLOY_S3_ACCESS_KEY/SECRET_KEY or AWS_* env")
	cmd.Flags().StringVar(&date, "date", "", "backup timestamp to verify (default: latest)")
	return cmd
}

func runAccessoryVerifyBackup(flags *Flags, appName, name, bucket, region, date, endpoint string) error {
	if bucket == "" {
		return fmt.Errorf("--bucket is required")
	}
	if err := backup.ValidateBucket(bucket); err != nil {
		return err
	}
	if err := backup.ValidateRegion(region); err != nil {
		return err
	}
	if date != "" {
		if err := backup.ValidateDate(date); err != nil {
			return err
		}
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	appCfg, executor, err := resolveAppForAccessory(ctx, flags, appName)
	if err != nil {
		return err
	}
	defer executor.Close()

	out := io.Writer(os.Stdout)
	if flags.JSON {
		// Keep stdout clean for the JSON result; progress goes to stderr.
		out = os.Stderr
	}
	client := backup.NewClient(executor, out)
	res, err := client.VerifyBackup(ctx, appCfg.App, name, date, s3Config(bucket, region, endpoint))
	if err != nil {
		return err
	}

	if flags.JSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(res); err != nil {
			return err
		}
	} else if res.OK {
		fmt.Printf("OK: %s/%s backup %s verified (%s, %s in %.1fs)\n",
			appCfg.App, name, res.Date, res.Kind, res.Metric, float64(res.DurationMs)/1000)
	} else {
		fmt.Printf("FAILED: %s/%s backup %s did not verify: %s\n", appCfg.App, name, res.Date, res.Detail)
	}
	if !res.OK {
		return fmt.Errorf("backup verification failed")
	}
	return nil
}
