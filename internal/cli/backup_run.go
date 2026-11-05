package cli

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/useteploy/teploy/internal/backup"
	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/events"
	"github.com/useteploy/teploy/internal/ssh"
)

// `teploy backup run` — the cron entrypoint (D3). It runs ON the server
// (like `autodeploy redeploy` and `accessory backup --local`): docker, aws
// and the stored backup target are all local, so there is no SSH hop and
// no teploy.yml. Cron lines carry only the app (and accessory); the
// target, retention and the failure-alert script are read at run time.

func newBackupRunCmd(flags *Flags) *cobra.Command {
	var (
		appName     string
		accessory   string
		all         bool
		ignoreSpace bool
	)
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run a scheduled backup on the server (the cron entrypoint)",
		Long: "Runs ON the server. --app backs up one app's volumes or one accessory (a\n" +
			"per-app schedule). --all is the policy line: it reads the server's backup\n" +
			"policy and works out at run time what to back up (every labelled accessory\n" +
			"and each of its databases, app volumes when enabled, adopted containers), one\n" +
			"target at a time. One target failing does not stop the others; the exit status\n" +
			"is nonzero for anything but a clean run, and nothing of a failed target is\n" +
			"pruned. A run that finds another one running stands down.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if all {
				if appName != "" || accessory != "" {
					return fmt.Errorf("--all takes no --app or --accessory: it works out its own targets")
				}
				return runBackupRunAll(flags, ignoreSpace)
			}
			return runBackupRun(flags, appName, accessory)
		},
	}
	cmd.Flags().StringVar(&appName, "app", "", "app name (required without --all)")
	cmd.Flags().StringVar(&accessory, "accessory", "", "back up this accessory instead of the app's volumes")
	cmd.Flags().BoolVar(&all, "all", false, "back up everything the server's backup policy covers")
	cmd.Flags().BoolVar(&ignoreSpace, "ignore-space-check", false, "with --all: skip the free-space preflights")
	return cmd
}

func runBackupRun(flags *Flags, appName, accessory string) error {
	if err := config.ValidateName(appName); err != nil {
		return err
	}
	if accessory != "" {
		if err := backup.ValidateBucket(accessory); err != nil {
			return fmt.Errorf("invalid accessory name %q: %w", accessory, err)
		}
	}

	// Runs on the server itself: no SSH hop, local docker and aws.
	ensureServerPath()
	executor := ssh.NewLocalExecutor()
	defer executor.Close()
	return runBackupRunOn(context.Background(), executor, appName, accessory)
}

// runBackupRunOn is runBackupRun against a given executor (the seam tests
// use; production passes the local executor).
func runBackupRunOn(ctx context.Context, executor ssh.Executor, appName, accessory string) error {

	target, err := backup.ReadTarget(ctx, executor)
	if err != nil {
		return err
	}
	if !TargetConfigured(target) {
		return fmt.Errorf("no backup target is configured on this server — run `teploy server backup-target set` from a machine that manages it")
	}
	cfg := TargetS3(target)
	if err := backup.ApplyPolicyEncryption(ctx, executor, &cfg); err != nil {
		return err
	}

	client := backup.NewClient(executor, os.Stdout)
	policy, err := backup.ReadPolicy(ctx, executor, appName)
	if err != nil {
		return err
	}

	var res backup.BackupResult
	var kind string
	if accessory != "" {
		image, env, inspErr := client.InspectAccessory(ctx, appName, accessory)
		if inspErr != nil {
			err = fmt.Errorf("inspecting accessory %s: %w", accessory, inspErr)
			kind = backup.KindAccessory
		} else {
			res, err = client.AccessoryBackup(ctx, appName, accessory, image, env, cfg)
			kind = backup.KindAccessory
		}
	} else {
		res, err = client.BackupVolumes(ctx, appName, cfg)
		kind = backup.KindVolumes
	}

	// Retention from the per-app store (written by `backup schedule` /
	// `accessory backup --schedule`), never baked into the cron line.
	var pruned int
	if err == nil {
		var kp *backup.KindPolicy
		if accessory == "" {
			kp = policy.Volumes
		} else if policy.Accessories != nil {
			kp = policy.Accessories[accessory]
		}
		if kp != nil {
			rp := backup.RetentionPolicy{KeepLast: kp.KeepLast, MaxAgeDays: kp.MaxAgeDays}
			if !rp.IsZero() {
				prefix := "volumes"
				if accessory != "" {
					prefix = "accessories/" + accessory
				}
				var deleted []string
				deleted, pruneErr := client.PruneBackups(ctx, appName, prefix, cfg, rp)
				pruned = len(deleted)
				if pruneErr != nil {
					fmt.Fprintf(os.Stderr, "Warning: prune failed: %v\n", pruneErr)
				}
			}
		}
	}

	events.Record(ctx, executor, events.Event{App: appName, Kind: events.KindBackupRun, Outcome: events.Outcome(err), Detail: events.ErrDetail(backupRunDetail(res, kind, accessory), err)})

	if err != nil {
		// The failure alert (uploaded by `backup schedule` when the app's
		// config has a webhook) fires from here, keeping the cron line a
		// single argument-free command.
		runBackupAlertIfPresent(ctx, appName)
		return err
	}
	if len(res.UncoveredDatabases) > 0 {
		// Succeeded, but something on the server is not backed up. Nobody
		// reads events unprompted; until Dash alerts on this (Slice 4) the
		// failure channel carries it, as a warning.
		runBackupWarningIfPresent(ctx, appName)
	}
	fmt.Printf("Backup %s complete (%s, %d bytes", res.ID, kind, res.SizeBytes)
	if pruned > 0 {
		fmt.Printf(", pruned %d", pruned)
	}
	fmt.Println(")")
	return nil
}

// runBackupAlertIfPresent executes the app's uploaded failure-alert script
// when one exists. Its own failures are reported but never mask the backup
// error that triggered it.
func runBackupAlertIfPresent(ctx context.Context, app string) {
	script := backupAlertScriptPath(app)
	exec := ssh.NewLocalExecutor()
	defer exec.Close()
	res := ssh.RunDetailed(ctx, exec, "if [ -x "+ssh.ShellQuote(script)+" ]; then "+ssh.ShellQuote(script)+"; fi")
	if res.Failed() {
		fmt.Fprintf(os.Stderr, "Warning: the backup alert script failed: %s\n", res.ExitErrorText())
	}
}

// backupWarningCmd runs the alert script as a warning, only if it is one that
// understands the argument: an older script ignores it and would report this
// successful backup as failed. `backup schedule` rewrites it.
func backupWarningCmd(app string) string {
	script := ssh.ShellQuote(backupAlertScriptPath(app))
	return "if [ -x " + script + " ] && grep -qF " + ssh.ShellQuote(backupAlertWarningMarker) + " " + script + "; then " + script + " warning; fi"
}

func runBackupWarningIfPresent(ctx context.Context, app string) {
	exec := ssh.NewLocalExecutor()
	defer exec.Close()
	res := ssh.RunDetailed(ctx, exec, backupWarningCmd(app))
	if res.Failed() {
		fmt.Fprintf(os.Stderr, "Warning: the backup alert script failed: %s\n", res.ExitErrorText())
	}
}

// buildScheduledBackupRunCmd assembles the command every teploy-managed
// backup cron line runs: the binary by absolute path, the app, nothing
// else. Bucket, credentials and retention are all read at run time.
func buildScheduledBackupRunCmd(binPath, app, accessory string) string {
	cmd := binPath + " backup run --app " + ssh.ShellQuote(app)
	if accessory != "" {
		cmd += " --accessory " + ssh.ShellQuote(accessory)
	}
	return cmd
}

// scheduledTeployPath is the binary cron lines invoke. TEPLOY_SERVER_BINARY
// overrides it for testing against a pre-release build (the release
// pipeline must publish `backup run` before the default path is usable).
func scheduledTeployPath() string {
	if p := strings.TrimSpace(os.Getenv("TEPLOY_SERVER_BINARY")); p != "" {
		return p
	}
	return serverTeployPath
}

// ensureScheduledBackupBinary makes sure the server has a teploy binary
// that speaks `backup run`, returning the path cron lines should invoke.
// An existing binary that already supports it is left untouched (no
// reinstall); otherwise the release pipeline installs the latest one and
// the candidate is capability-checked before publication.
func ensureScheduledBackupBinary(ctx context.Context, executor ssh.Executor) (string, error) {
	bin := scheduledTeployPath()
	if serverBinarySupportsBackupRun(ctx, executor, bin) {
		return bin, nil
	}
	if bin != serverTeployPath {
		return "", fmt.Errorf("%s does not support `backup run` (check TEPLOY_SERVER_BINARY)", bin)
	}
	if _, err := deployTeployBinaryToServer(ctx, executor, serverTeployPath, "backup run"); err != nil {
		return "", fmt.Errorf("the server's teploy binary does not support `backup run` and installing one failed: %w", err)
	}
	if !serverBinarySupportsBackupRun(ctx, executor, bin) {
		return "", fmt.Errorf("the installed teploy binary still does not support `backup run` — the latest release may predate it")
	}
	return bin, nil
}

// serverBinarySupportsBackupRun probes the binary's own help.
func serverBinarySupportsBackupRun(ctx context.Context, executor ssh.Executor, bin string) bool {
	res := ssh.RunDetailed(ctx, executor, ssh.ShellQuote(bin)+" backup run --help >/dev/null 2>&1")
	return !res.Failed()
}
