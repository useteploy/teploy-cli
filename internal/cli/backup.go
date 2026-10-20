package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"

	"github.com/spf13/cobra"
	"github.com/useteploy/teploy/internal/backup"
	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/deploy"
	"github.com/useteploy/teploy/internal/docker"
	"github.com/useteploy/teploy/internal/events"
	"github.com/useteploy/teploy/internal/notify"
	secretstore "github.com/useteploy/teploy/internal/secret"
	"github.com/useteploy/teploy/internal/ssh"
	"github.com/useteploy/teploy/internal/state"
)

// The backup family (D3): one target per server, read at run time.
//
// Every command resolves its S3 target the same way — explicit legacy
// flags (--bucket and friends) win, else the server's stored target
// (`teploy server backup-target set`). Folderless operation (--app X
// --host S) works everywhere: volumes and .teploy data live on the
// server, and accessory facts come from the running container.

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
	cmd.AddCommand(newBackupUnscheduleCmd(flags))
	cmd.AddCommand(newBackupRunCmd(flags))

	return cmd
}

// backupTargetFlags carries the shared S3-target flags. An empty Bucket
// means "not given" (resolve from the server's stored target).
type backupTargetFlags struct {
	bucket   string
	region   string
	endpoint string
	prefix   string
}

// addBackupTargetFlags registers the legacy target flags. They override
// the stored server target when given (existing users' scripts keep
// working unchanged).
func addBackupTargetFlags(cmd *cobra.Command, f *backupTargetFlags) {
	cmd.Flags().StringVar(&f.bucket, "bucket", "", "S3 bucket name (default: the server's stored backup target)")
	cmd.Flags().StringVar(&f.region, "region", "", "AWS region (default: the stored target's region, else us-east-1)")
	cmd.Flags().StringVar(&f.endpoint, "endpoint", "", "S3-compatible endpoint URL (MinIO/B2/R2); creds from TEPLOY_S3_ACCESS_KEY/SECRET_KEY or AWS_* env")
	cmd.Flags().StringVar(&f.prefix, "prefix", "", "key prefix under the bucket (default: the stored target's prefix)")
}

// resolveBackupS3 builds the S3Config for a backup command. Legacy flags
// win (unchanged legacy behavior including env-credential gating); with no
// flags the server's stored target is used, stored credentials and all.
func resolveBackupS3(ctx context.Context, exec ssh.Executor, f *backupTargetFlags) (backup.S3Config, error) {
	if f.bucket != "" {
		if err := backup.ValidateBucket(f.bucket); err != nil {
			return backup.S3Config{}, err
		}
		region := f.region
		if region == "" {
			region = "us-east-1"
		}
		if err := backup.ValidateRegion(region); err != nil {
			return backup.S3Config{}, err
		}
		if f.endpoint != "" {
			if err := backup.ValidateEndpoint(f.endpoint); err != nil {
				return backup.S3Config{}, err
			}
		}
		if err := backup.ValidatePrefix(f.prefix); err != nil {
			return backup.S3Config{}, err
		}
		cfg := s3Config(f.bucket, region, f.endpoint)
		cfg.Prefix = f.prefix
		return cfg, nil
	}
	target, err := backup.ReadTarget(ctx, exec)
	if err != nil {
		return backup.S3Config{}, err
	}
	if !TargetConfigured(target) {
		return backup.S3Config{}, fmt.Errorf("no backup target is configured for this server — set one with `teploy server backup-target set --host S --bucket B` (or pass the legacy --bucket flag)")
	}
	return TargetS3(target), nil
}

// TargetConfigured reports whether a stored target exists.
func TargetConfigured(t backup.Target) bool {
	return t.Bucket != ""
}

// TargetS3 maps a stored target to an S3Config. Stored credentials apply
// whether or not an endpoint is set (D3); the ambient AWS chain is used
// only when none are stored.
func TargetS3(t backup.Target) backup.S3Config {
	region := t.Region
	if region == "" {
		region = "us-east-1"
	}
	return backup.S3Config{
		Bucket:       t.Bucket,
		Region:       region,
		Endpoint:     t.Endpoint,
		Prefix:       t.Prefix,
		StoredTarget: t.Path,
	}
}

// s3Config assembles the legacy flag-driven target. --endpoint switches the
// server-side aws CLI to an S3-compatible endpoint (a MinIO accessory, B2,
// R2, Bunny). Credentials come from the LOCAL environment —
// TEPLOY_S3_ACCESS_KEY / TEPLOY_S3_SECRET_KEY, falling back to
// AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY — and ride inline on the
// remote aws command for that invocation only; nothing is persisted
// server-side. Unset = the server's ambient AWS credentials, exactly as
// before.
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

// addRetentionFlags registers the shared --keep-last / --max-age-days flags.
func addRetentionFlags(cmd *cobra.Command, keepLast, maxAgeDays *int) {
	cmd.Flags().IntVar(keepLast, "keep-last", 0, "Keep only the N most recent backups (0 = keep all)")
	cmd.Flags().IntVar(maxAgeDays, "max-age-days", 0, "Delete backups older than N days (0 = no age limit; pair with --keep-last as a floor)")
}

// backupProgressOut keeps stdout clean for the JSON envelope.
func backupProgressOut(flags *Flags) io.Writer {
	if flags.JSON {
		return os.Stderr
	}
	return os.Stdout
}

// backupCreatedAt renders a backup id's timestamp as RFC3339 (best effort).
func backupCreatedAt(id string) string {
	if len(id) >= 15 {
		if ts, err := backup.ParseIDTimestamp(id[:15]); err == nil {
			return ts
		}
	}
	return ""
}

// backupCreateDTO is the `backup create --json` envelope (backup-json-v1).
type backupCreateDTO struct {
	MachineInterface int    `json:"machine_interface"`
	Host             string `json:"host"`
	App              string `json:"app"`
	ID               string `json:"id"`
	Kind             string `json:"kind"`
	Accessory        string `json:"accessory,omitempty"`
	SizeBytes        int64  `json:"size_bytes"`
	CreatedAt        string `json:"created_at"`
	Key              string `json:"key"`
	TeployKey        string `json:"teploy_key,omitempty"`
	Pruned           int    `json:"pruned"`
}

func newBackupCreateCmd(flags *Flags) *cobra.Command {
	var (
		appName    string
		accessory  string
		tf         backupTargetFlags
		keepLast   int
		maxAgeDays int
	)

	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a volume or accessory backup",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBackupCreate(flags, appName, accessory, &tf, keepLast, maxAgeDays)
		},
	}

	cmd.Flags().StringVar(&appName, "app", "", "app name — act on server state instead of teploy.yml (requires --host)")
	cmd.Flags().StringVar(&accessory, "accessory", "", "back up this accessory instead of the app's volumes")
	addBackupTargetFlags(cmd, &tf)
	addRetentionFlags(cmd, &keepLast, &maxAgeDays)

	return cmd
}

func runBackupCreate(flags *Flags, appName, accessory string, tf *backupTargetFlags, keepLast, maxAgeDays int) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	appCfg, executor, err := resolveApp(ctx, flags, appName)
	if err != nil {
		return err
	}
	defer executor.Close()

	cfg, err := resolveBackupS3(ctx, executor, tf)
	if err != nil {
		return err
	}

	client := backup.NewClient(executor, backupProgressOut(flags))

	var res backup.BackupResult
	var kind string
	if accessory != "" {
		if err := backup.ValidateBucket(accessory); err != nil {
			return fmt.Errorf("invalid accessory name %q: %w", accessory, err)
		}
		image, env, accErr := client.InspectAccessory(ctx, appCfg.App, accessory)
		if accErr != nil {
			return fmt.Errorf("inspecting accessory %s: %w", accessory, accErr)
		}
		res, err = client.AccessoryBackup(ctx, appCfg.App, accessory, image, env, cfg)
		kind = backup.KindAccessory
	} else {
		res, err = client.BackupVolumes(ctx, appCfg.App, cfg)
		kind = backup.KindVolumes
	}

	// Enforce retention only after a successful backup — never prune when the
	// fresh backup failed (that could leave zero good copies).
	var pruned int
	var pruneErr error
	policy := backup.RetentionPolicy{KeepLast: keepLast, MaxAgeDays: maxAgeDays}
	if err == nil && !policy.IsZero() {
		prefix := "volumes"
		if accessory != "" {
			prefix = "accessories/" + accessory
		}
		var deleted []string
		deleted, pruneErr = client.PruneBackups(ctx, appCfg.App, prefix, cfg, policy)
		pruned = len(deleted)
	}

	detail := map[string]any{"id": res.ID, "kind": kind}
	if accessory != "" {
		detail["accessory"] = accessory
	}
	recordAppEvent(ctx, executor, appCfg.App, events.KindBackupRun, err, detail)

	// Fire notification (fire-and-forget). The backup itself succeeding is
	// what Success reflects; a prune failure is surfaced in the message +
	// exit code but does not flip the backup to "failed" (the data is
	// safely stored).
	if n := buildNotifier(appCfg); n != nil {
		msg := fmt.Sprintf("Backup created for %s", appCfg.App)
		switch {
		case err != nil:
			msg = fmt.Sprintf("Backup failed for %s: %s", appCfg.App, err)
		case pruneErr != nil:
			msg = fmt.Sprintf("Backup created for %s, but prune failed: %s", appCfg.App, pruneErr)
		case pruned > 0:
			msg = fmt.Sprintf("Backup created for %s (pruned %d old backup(s))", appCfg.App, pruned)
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
	if pruneErr != nil {
		return pruneErr
	}

	if flags.JSON {
		return json.NewEncoder(os.Stdout).Encode(backupCreateDTO{
			MachineInterface: MachineInterface,
			Host:             executor.Host(),
			App:              appCfg.App,
			ID:               res.ID,
			Kind:             kind,
			Accessory:        accessory,
			SizeBytes:        res.SizeBytes,
			CreatedAt:        backupCreatedAt(res.ID),
			Key:              res.Key,
			TeployKey:        res.TeployKey,
			Pruned:           pruned,
		})
	}
	fmt.Printf("Backup %s created (%s, %d bytes)\n", res.ID, kind, res.SizeBytes)
	if res.TeployKey != "" {
		fmt.Printf("  Server data: %s\n", res.TeployKey)
	}
	if pruned > 0 {
		fmt.Printf("  Pruned %d old backup(s)\n", pruned)
	}
	return nil
}

// backupListDTO is the `backup list --json` envelope (backup-json-v1).
type backupListDTO struct {
	MachineInterface int                   `json:"machine_interface"`
	Host             string                `json:"host"`
	App              string                `json:"app"`
	Target           *backupTargetSummary  `json:"target,omitempty"`
	Backups          []backup.ListedBackup `json:"backups"`
	Schedules        []backupScheduleDTO   `json:"schedules"`
}

type backupTargetSummary struct {
	Bucket         string `json:"bucket"`
	Region         string `json:"region"`
	Endpoint       string `json:"endpoint,omitempty"`
	Prefix         string `json:"prefix,omitempty"`
	HasCredentials bool   `json:"has_credentials"`
}

// backupScheduleDTO is one discovered schedule (from teploy cron markers,
// retention joined from the per-app schedule store).
type backupScheduleDTO struct {
	Kind       string `json:"kind"`
	Accessory  string `json:"accessory,omitempty"`
	Cron       string `json:"cron"`
	KeepLast   int    `json:"keep_last,omitempty"`
	MaxAgeDays int    `json:"max_age_days,omitempty"`
	Form       string `json:"form"`
}

func newBackupListCmd(flags *Flags) *cobra.Command {
	var (
		appName string
		tf      backupTargetFlags
	)

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List available backups and schedules",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBackupList(flags, appName, &tf)
		},
	}

	cmd.Flags().StringVar(&appName, "app", "", "app name — act on server state instead of teploy.yml (requires --host)")
	addBackupTargetFlags(cmd, &tf)

	return cmd
}

func runBackupList(flags *Flags, appName string, tf *backupTargetFlags) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	appCfg, executor, err := resolveApp(ctx, flags, appName)
	if err != nil {
		return err
	}
	defer executor.Close()

	cfg, err := resolveBackupS3(ctx, executor, tf)
	if err != nil {
		return err
	}

	client := backup.NewClient(executor, backupProgressOut(flags))
	rows, err := client.ListBackupsRecursive(ctx, appCfg.App, cfg)
	if err != nil {
		return err
	}
	if rows == nil {
		rows = []backup.ListedBackup{}
	}
	schedules, err := discoverBackupSchedules(ctx, executor, appCfg.App)
	if err != nil {
		return err
	}
	if schedules == nil {
		schedules = []backupScheduleDTO{}
	}

	if flags.JSON {
		target, _ := backup.ReadTarget(ctx, executor)
		var summary *backupTargetSummary
		if TargetConfigured(target) {
			summary = &backupTargetSummary{
				Bucket:         target.Bucket,
				Region:         target.Region,
				Endpoint:       target.Endpoint,
				Prefix:         target.Prefix,
				HasCredentials: target.HasCredentials,
			}
		}
		return json.NewEncoder(os.Stdout).Encode(backupListDTO{
			MachineInterface: MachineInterface,
			Host:             executor.Host(),
			App:              appCfg.App,
			Target:           summary,
			Backups:          rows,
			Schedules:        schedules,
		})
	}

	if len(rows) == 0 && len(schedules) == 0 {
		fmt.Println("No backups or schedules found")
		return nil
	}
	for _, s := range schedules {
		label := s.Kind
		if s.Accessory != "" {
			label += "/" + s.Accessory
		}
		retention := ""
		if s.KeepLast > 0 {
			retention = fmt.Sprintf(", keep last %d", s.KeepLast)
		}
		if s.MaxAgeDays > 0 {
			retention += fmt.Sprintf(", max age %dd", s.MaxAgeDays)
		}
		fmt.Printf("schedule %-20s %s (%s%s)\n", label, s.Cron, s.Form, retention)
	}
	for _, b := range rows {
		label := b.Kind
		if b.Accessory != "" {
			label += "/" + b.Accessory
		}
		fmt.Printf("%s  %-18s %10d bytes  %s\n", b.CreatedAt, label, b.SizeBytes, b.Key)
	}
	return nil
}

// discoverBackupSchedules reads the crontab's teploy backup markers and
// joins each with the retention stored in the app's schedule store.
func discoverBackupSchedules(ctx context.Context, exec ssh.Executor, app string) ([]backupScheduleDTO, error) {
	lines, err := backup.ReadCrontab(ctx, exec)
	if err != nil {
		return nil, err
	}
	policy, err := backup.ReadPolicy(ctx, exec, app)
	if err != nil {
		return nil, err
	}
	var out []backupScheduleDTO
	for _, line := range lines {
		m := backup.FindCronMarker(line)
		if m == nil || m.App != app {
			continue
		}
		s := backupScheduleDTO{Kind: m.Kind, Accessory: m.Accessory, Cron: m.Cron, Form: m.Form}
		var kp *backup.KindPolicy
		if m.Accessory == "" {
			kp = policy.Volumes
		} else if policy.Accessories != nil {
			kp = policy.Accessories[m.Accessory]
		}
		if kp != nil {
			s.KeepLast, s.MaxAgeDays = kp.KeepLast, kp.MaxAgeDays
		}
		out = append(out, s)
	}
	return out, nil
}

// backupRestoreDTO is the `backup restore --json` envelope (backup-json-v1).
type backupRestoreDTO struct {
	MachineInterface int      `json:"machine_interface"`
	Host             string   `json:"host"`
	App              string   `json:"app"`
	ID               string   `json:"id"`
	Kind             string   `json:"kind"`
	Accessory        string   `json:"accessory,omitempty"`
	OK               bool     `json:"ok"`
	Stopped          []string `json:"stopped"`
	Started          []string `json:"started"`
	Health           string   `json:"health"`
	RecoveryDir      string   `json:"recovery_dir,omitempty"`
	Error            string   `json:"error,omitempty"`
}

func newBackupRestoreCmd(flags *Flags) *cobra.Command {
	var (
		appName   string
		accessory string
		tf        backupTargetFlags
		yes       bool
	)

	cmd := &cobra.Command{
		Use:   "restore <date>",
		Short: "Restore a volume or accessory backup (quiesced)",
		Long: "Restores a backup under the app's fenced lock: the app's containers\n" +
			"are stopped (and a generic accessory's container, for an accessory\n" +
			"restore), the backup is promoted with the two-phase recovery\n" +
			"discipline, the containers are started again and health-checked. On\n" +
			"failure the recovery copy of the previous data is kept and named.\n" +
			"Non-interactive runs must pass --yes.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBackupRestore(flags, appName, accessory, args[0], &tf, yes)
		},
	}

	cmd.Flags().StringVar(&appName, "app", "", "app name — act on server state instead of teploy.yml (requires --host)")
	cmd.Flags().StringVar(&accessory, "accessory", "", "restore this accessory's backup instead of the app's volumes")
	addBackupTargetFlags(cmd, &tf)
	cmd.Flags().BoolVar(&yes, "yes", false, "proceed without interactive confirmation")

	return cmd
}

func runBackupRestore(flags *Flags, appName, accessory, date string, tf *backupTargetFlags, yes bool) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	appCfg, executor, err := resolveApp(ctx, flags, appName)
	if err != nil {
		return err
	}
	defer executor.Close()

	cfg, err := resolveBackupS3(ctx, executor, tf)
	if err != nil {
		return err
	}
	if err := backup.ValidateDate(date); err != nil {
		return err
	}
	if accessory != "" {
		if err := backup.ValidateBucket(accessory); err != nil {
			return fmt.Errorf("invalid accessory name %q: %w", accessory, err)
		}
	}

	if !yes {
		if !stdinIsTerminal() {
			return fmt.Errorf("--yes is required to restore without an interactive terminal")
		}
		if !confirm(os.Stdout, fmt.Sprintf("Restore %s backup %s for %s (stops the app while it runs)?", restoreKindLabel(accessory), date, appCfg.App)) {
			return fmt.Errorf("aborted")
		}
	}

	// The fenced lock serializes the restore against deploys and every
	// other mutating command; every stop/start below runs guarded by it.
	lk, err := state.AcquireLockFenced(ctx, executor, appCfg.App)
	if err != nil {
		return err
	}
	lk.StartRenewal(executor)
	defer state.ReleaseLockFenced(executor, lk, appCfg.App)

	progress := backupProgressOut(flags)
	client := backup.NewClient(executor, progress)
	lc := deploy.NewLifecycle(executor, progress)

	dto := backupRestoreDTO{
		MachineInterface: MachineInterface,
		Host:             executor.Host(),
		App:              appCfg.App,
		ID:               date,
		Kind:             backup.KindVolumes,
		Accessory:        accessory,
		Stopped:          []string{},
		Started:          []string{},
	}
	if accessory != "" {
		dto.Kind = backup.KindAccessory
	}

	// Quiesce: stop the app's running containers; a generic accessory's
	// container too (engine restores run through the live engine; redis
	// manages its own stop/start inside its restore script).
	stopTimeout := appCfg.StopTimeout
	if stopTimeout == 0 {
		stopTimeout = 10
	}
	stopped, qErr := lc.QuiesceApp(ctx, lk, appCfg.App, stopTimeout)
	for _, c := range stopped {
		dto.Stopped = append(dto.Stopped, c.Name)
	}
	accessoryStopped := false
	if qErr == nil && accessory != "" {
		accessoryImage, _ := inspectAccessoryForRestore(ctx, client, appCfg.App, accessory, appCfg)
		if backup.EngineOf(accessoryImage) == "generic" {
			container := appCfg.App + "-" + accessory
			if _, sErr := lk.Guarded(ctx, executor, fmt.Sprintf("docker stop --time %d %s", stopTimeout, ssh.ShellQuote(container))); sErr == nil {
				accessoryStopped = true
				dto.Stopped = append(dto.Stopped, container)
			} else {
				qErr = sErr
			}
		}
	}
	if qErr != nil {
		// Quiesce failures leave the app down only when a stop failed
		// midway — restart whatever QuiesceApp managed to stop.
		return finishRestoreFailure(flags, ctx, executor, lc, lk, dto, accessoryStopped, stopped, qErr)
	}

	// Restore, keeping ownership of the recovery directory until the
	// post-restore health check also passes.
	var recoveryDir string
	quiescedNote := strings.Join(dto.Stopped, ", ")
	if accessory != "" {
		accessoryImage, env := inspectAccessoryForRestore(ctx, client, appCfg.App, accessory, appCfg)
		recoveryDir, err = client.AccessoryRestoreKeep(ctx, appCfg.App, accessory, accessoryImage, date, env, cfg)
	} else {
		recoveryDir, err = client.RestoreVolumesKeep(ctx, appCfg.App, date, cfg, quiescedNote)
	}
	if err != nil {
		return finishRestoreFailure(flags, ctx, executor, lc, lk, dto, accessoryStopped, stopped, err)
	}

	// Resume: start what was stopped and health-check the app.
	resume := lc.ResumeApp(ctx, lk, appCfg.App, stopped)
	dto.Started = resume.Started
	if dto.Started == nil {
		dto.Started = []string{}
	}
	if accessoryStopped {
		container := appCfg.App + "-" + accessory
		if _, sErr := lk.Guarded(ctx, executor, "docker start "+ssh.ShellQuote(container)); sErr == nil {
			dto.Started = append(dto.Started, container)
		} else if resume.HealthErr == nil {
			resume.HealthErr = sErr
		}
	}

	if resume.HealthErr != nil {
		// The restored data is in place but did not come up healthy: keep
		// the recovery copy so the operator can fall back.
		dto.Health = "unhealthy"
		dto.RecoveryDir = recoveryDir
		dto.Error = fmt.Sprintf("health check after restore failed: %v — pre-restore data kept in %s", resume.HealthErr, recoveryDir)
		recordAppEvent(ctx, executor, appCfg.App, events.KindBackupRestore, resume.HealthErr, restoreEventDetail(dto))
		if flags.JSON {
			return errors.Join(resume.HealthErr, json.NewEncoder(os.Stdout).Encode(dto))
		}
		return resume.HealthErr
	}
	dto.Health = resume.HealthStatus()

	if recoveryDir != "" {
		executor.Run(context.WithoutCancel(ctx), "rm -rf "+ssh.ShellQuote(recoveryDir))
	}
	dto.OK = true
	recordAppEvent(ctx, executor, appCfg.App, events.KindBackupRestore, nil, restoreEventDetail(dto))
	if flags.JSON {
		return json.NewEncoder(os.Stdout).Encode(dto)
	}
	fmt.Printf("Restore complete: %s (started %s)\n", date, strings.Join(dto.Started, ", "))
	return nil
}

// inspectAccessoryForRestore resolves the accessory's image and env for a
// restore: the running container first (the resolved credentials only
// exist there, audit F68), the folder config only when it carries no
// unresolved references.
func inspectAccessoryForRestore(ctx context.Context, client *backup.Client, app, accessory string, appCfg *config.AppConfig) (string, map[string]string) {
	if image, env, err := client.InspectAccessory(ctx, app, accessory); err == nil {
		return image, env
	}
	if accCfg, ok := appCfg.Accessories[accessory]; ok {
		if err := requireResolvedAccessoryEnv(accessory, accCfg.Env); err == nil {
			return accCfg.Image, accCfg.Env
		}
	}
	return "", nil
}

// finishRestoreFailure restarts what was stopped (unless the failure left
// the data tree in an indeterminate state — recovery-incomplete — where
// serving from it is worse than staying down), reports, and records the
// event.
func finishRestoreFailure(flags *Flags, ctx context.Context, executor ssh.Executor, lc *deploy.Lifecycle, lk *state.Lock, dto backupRestoreDTO, accessoryStopped bool, stopped []docker.Container, rerr error) error {
	if accessoryStopped {
		container := dto.App + "-" + dto.Accessory
		if _, err := lk.Guarded(ctx, executor, "docker start "+ssh.ShellQuote(container)); err == nil {
			dto.Started = append(dto.Started, container)
		}
	}
	if !backup.IsRecoveryIncomplete(rerr) {
		resume := lc.ResumeApp(ctx, lk, dto.App, stopped)
		dto.Started = append(dto.Started, resume.Started...)
	}
	dto.OK = false
	dto.Error = rerr.Error()
	recordAppEvent(ctx, executor, dto.App, events.KindBackupRestore, rerr, restoreEventDetail(dto))
	if flags.JSON {
		return errors.Join(rerr, json.NewEncoder(os.Stdout).Encode(dto))
	}
	return rerr
}

func restoreEventDetail(dto backupRestoreDTO) map[string]any {
	detail := map[string]any{"id": dto.ID, "kind": dto.Kind}
	if dto.Accessory != "" {
		detail["accessory"] = dto.Accessory
	}
	return detail
}

func restoreKindLabel(accessory string) string {
	if accessory != "" {
		return "accessory " + accessory
	}
	return "volume"
}

func newBackupPruneCmd(flags *Flags) *cobra.Command {
	var (
		appName    string
		tf         backupTargetFlags
		accessory  string
		keepLast   int
		maxAgeDays int
	)

	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Delete old backups per a retention policy",
		Long:  "Removes backups outside the retention policy.\nExample: teploy backup prune --keep-last 7\n         teploy backup prune --max-age-days 30 --keep-last 3",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBackupPrune(flags, appName, &tf, accessory, keepLast, maxAgeDays)
		},
	}

	cmd.Flags().StringVar(&appName, "app", "", "app name — act on server state instead of teploy.yml (requires --host)")
	addBackupTargetFlags(cmd, &tf)
	cmd.Flags().StringVar(&accessory, "accessory", "", "Prune an accessory's backups instead of volumes (accessory name)")
	addRetentionFlags(cmd, &keepLast, &maxAgeDays)

	return cmd
}

func runBackupPrune(flags *Flags, appName string, tf *backupTargetFlags, accessory string, keepLast, maxAgeDays int) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	appCfg, executor, err := resolveApp(ctx, flags, appName)
	if err != nil {
		return err
	}
	defer executor.Close()

	cfg, err := resolveBackupS3(ctx, executor, tf)
	if err != nil {
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

	client := backup.NewClient(executor, backupProgressOut(flags))
	deleted, err := client.PruneBackups(ctx, appCfg.App, prefix, cfg, policy)
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

func newBackupScheduleCmd(flags *Flags) *cobra.Command {
	var (
		appName    string
		tf         backupTargetFlags
		keepLast   int
		maxAgeDays int
	)

	cmd := &cobra.Command{
		Use:   "schedule <cron>",
		Short: "Set up automated backups on a cron schedule",
		Long: "Creates a cron job on the server that runs `teploy backup run` — the\n" +
			"job carries no bucket and no credentials; both are read from the\n" +
			"server's stored backup target at run time.\n" +
			"Example: teploy backup schedule \"0 3 * * *\" --app myapp --keep-last 7",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBackupSchedule(flags, args[0], appName, &tf, keepLast, maxAgeDays)
		},
	}

	cmd.Flags().StringVar(&appName, "app", "", "app name — act on server state instead of teploy.yml (requires --host)")
	addBackupTargetFlags(cmd, &tf)
	addRetentionFlags(cmd, &keepLast, &maxAgeDays)

	return cmd
}

func runBackupSchedule(flags *Flags, schedule, appName string, tf *backupTargetFlags, keepLast, maxAgeDays int) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	appCfg, executor, err := resolveApp(ctx, flags, appName)
	if err != nil {
		return err
	}
	defer executor.Close()

	if err := backup.ValidateSchedule(schedule); err != nil {
		return err
	}
	if keepLast < 0 || maxAgeDays < 0 {
		return fmt.Errorf("--keep-last and --max-age-days must be >= 0")
	}

	target, err := resolveScheduleTarget(ctx, executor, tf)
	if err != nil {
		return err
	}

	// The scheduled job runs ON the server and needs a teploy binary that
	// speaks `backup run`.
	binPath, err := ensureScheduledBackupBinary(ctx, executor)
	if err != nil {
		return err
	}

	// Failure alerting stays a script beside the app; `backup run` invokes
	// it on failure, so the cron line itself stays argument-free.
	webhook := firstWebhookURL(appCfg.Notifications)
	signed := false
	if webhook != "" {
		secret := appCfg.Notifications.SigningSecret()
		script := buildBackupAlertScript(appCfg.App, executor.Host(), webhook, secret)
		if err := executor.Upload(ctx, strings.NewReader(script), backupAlertScriptPath(appCfg.App), "0700"); err != nil {
			return fmt.Errorf("uploading the backup alert script: %w", err)
		}
		signed = secret != ""
	}

	// The retention half of the schedule lives in the per-app store, read
	// at run time.
	policy, err := backup.ReadPolicy(ctx, executor, appCfg.App)
	if err != nil {
		return err
	}
	policy.Volumes = &backup.KindPolicy{Cron: schedule, KeepLast: keepLast, MaxAgeDays: maxAgeDays}
	if err := backup.WritePolicy(ctx, executor, appCfg.App, policy); err != nil {
		return err
	}

	client := backup.NewClient(executor, backupProgressOut(flags))
	if err := client.SetSchedule(ctx, schedule, buildScheduledBackupRunCmd(binPath, appCfg.App, ""), "teploy-backup:"+appCfg.App); err != nil {
		return err
	}

	fmt.Printf("Backup scheduled: %s\n", schedule)
	fmt.Printf("  App: %s\n", appCfg.App)
	fmt.Printf("  Target: s3://%s (read at run time from the server's stored target)\n", backupTargetLabel(target))
	if keepLast > 0 || maxAgeDays > 0 {
		fmt.Printf("  Retention: keep last %d, max age %dd\n", keepLast, maxAgeDays)
	}
	if webhook != "" {
		if signed {
			fmt.Println("  Alerts: webhook notified on failure (signed)")
		} else {
			// Said plainly, because a receiver that verifies signatures will
			// reject this and the rejection looks like a wrong secret.
			fmt.Printf("  Alerts: webhook notified on failure (UNSIGNED — set %s to sign)\n", config.WebhookSecretEnv)
		}
	}
	return nil
}

// storeTargetFromLegacyFlags persists the legacy flag values as the
// server's backup target (credentials from the local environment, when an
// endpoint is in play). Used only by the schedule bootstrap path.
func storeTargetFromLegacyFlags(ctx context.Context, executor ssh.Executor, tf *backupTargetFlags) (backup.Target, error) {
	region := tf.region
	if region == "" {
		region = "us-east-1"
	}
	if err := backup.ValidateBucket(tf.bucket); err != nil {
		return backup.Target{}, err
	}
	if err := backup.ValidateRegion(region); err != nil {
		return backup.Target{}, err
	}
	if tf.endpoint != "" {
		if err := backup.ValidateEndpoint(tf.endpoint); err != nil {
			return backup.Target{}, err
		}
	}
	if err := backup.ValidatePrefix(tf.prefix); err != nil {
		return backup.Target{}, err
	}
	t := backup.Target{Bucket: tf.bucket, Region: region, Endpoint: tf.endpoint, Prefix: tf.prefix}
	credsArmor := ""
	if tf.endpoint != "" {
		access := os.Getenv("TEPLOY_S3_ACCESS_KEY")
		if access == "" {
			access = os.Getenv("AWS_ACCESS_KEY_ID")
		}
		secret := os.Getenv("TEPLOY_S3_SECRET_KEY")
		if secret == "" {
			secret = os.Getenv("AWS_SECRET_ACCESS_KEY")
		}
		if access != "" && secret != "" {
			if err := (secretstore.NewManager(executor)).EnsureAge(ctx); err != nil {
				return backup.Target{}, err
			}
			armor, err := backup.EncryptCredentials(ctx, executor, access, secret)
			if err != nil {
				return backup.Target{}, err
			}
			credsArmor = armor
		}
	}
	path, fallback, err := backup.WriteTarget(ctx, executor, t, credsArmor)
	if err != nil {
		return backup.Target{}, err
	}
	t.Path = path
	t.HasCredentials = credsArmor != ""
	fmt.Printf("Stored the server backup target s3://%s at %s\n", backupTargetLabel(t), path)
	if fallback {
		fmt.Println("  /etc/teploy is not writable by this user — the target lives in ~/.config/teploy instead")
	}
	return t, nil
}

// backupTargetLabel renders a target as bucket[/prefix].
func backupTargetLabel(t backup.Target) string {
	if t.Prefix == "" {
		return t.Bucket
	}
	return t.Bucket + "/" + t.Prefix
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

func newBackupUnscheduleCmd(flags *Flags) *cobra.Command {
	var appName string
	cmd := &cobra.Command{
		Use:   "unschedule",
		Short: "Remove the scheduled volume-backup cron entry",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBackupUnschedule(flags, appName)
		},
	}
	cmd.Flags().StringVar(&appName, "app", "", "app name — act on server state instead of teploy.yml (requires --host)")
	return cmd
}

func runBackupUnschedule(flags *Flags, appName string) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	appCfg, executor, err := resolveApp(ctx, flags, appName)
	if err != nil {
		return err
	}
	defer executor.Close()

	client := backup.NewClient(executor, backupProgressOut(flags))
	if err := client.RemoveSchedule(ctx, "teploy-backup:"+appCfg.App); err != nil {
		return err
	}
	policy, err := backup.ReadPolicy(ctx, executor, appCfg.App)
	if err != nil {
		return err
	}
	if policy.Volumes != nil {
		policy.Volumes = nil
		if err := backup.WritePolicy(ctx, executor, appCfg.App, policy); err != nil {
			return err
		}
	}
	fmt.Printf("Backup schedule removed for %s\n", appCfg.App)
	return nil
}
