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
		appName   string
		accessory string
	)
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run a scheduled backup on the server (the cron entrypoint)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBackupRun(flags, appName, accessory)
		},
	}
	cmd.Flags().StringVar(&appName, "app", "", "app name (required)")
	cmd.Flags().StringVar(&accessory, "accessory", "", "back up this accessory instead of the app's volumes")
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

	detail := map[string]any{"id": res.ID, "kind": kind}
	if accessory != "" {
		detail["accessory"] = accessory
	}
	events.Record(ctx, executor, events.Event{App: appName, Kind: events.KindBackupRun, Outcome: events.Outcome(err), Detail: detail})

	if err != nil {
		// The failure alert (uploaded by `backup schedule` when the app's
		// config has a webhook) fires from here, keeping the cron line a
		// single argument-free command.
		runBackupAlertIfPresent(ctx, appName)
		return err
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
