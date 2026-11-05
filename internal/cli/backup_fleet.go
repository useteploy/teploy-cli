package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"filippo.io/age"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/useteploy/teploy/internal/backup"
	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/events"
	"github.com/useteploy/teploy/internal/secret"
	"github.com/useteploy/teploy/internal/ssh"
)

// The fleet engine's commands: `backup run --all` (the one policy cron line),
// `server backup-policy`, `backup status`, `backup fetch`, `backup keygen`.

// fleetRunDeadline bounds a whole `backup run --all`; each target has its own
// backup.BackupDeadline inside it. A holder older than this plus an hour is stuck.
const fleetRunDeadline = backup.MaxDeadline

// serverToolPaths are directories cron's minimal PATH leaves out, where aws
// (the AWS installer's symlink) and age may live.
var serverToolPaths = []string{"/usr/local/sbin", "/usr/local/bin", "/usr/sbin", "/usr/bin", "/sbin", "/bin", "/snap/bin"}

// ensureServerPath completes cron's PATH: the job finding no aws would fail every
// night with a misleading message, on a server where the command works by hand.
func ensureServerPath() {
	have := map[string]bool{}
	cur := os.Getenv("PATH")
	for _, d := range filepath.SplitList(cur) {
		have[d] = true
	}
	for _, d := range serverToolPaths {
		if !have[d] {
			cur += string(os.PathListSeparator) + d
		}
	}
	os.Setenv("PATH", cur)
}

// backupRunEnvelope is `backup run --all --json` (backup-fleet-v1).
type backupRunEnvelope struct {
	MachineInterface int `json:"machine_interface"`
	backup.FleetRunResult
}

func runBackupRunAll(flags *Flags, ignoreSpace bool) error {
	ensureServerPath()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, fleetRunDeadline)
	defer cancel()
	executor := ssh.NewLocalExecutor()
	defer executor.Close()

	progress := io.Writer(os.Stdout)
	if flags.JSON {
		progress = os.Stderr
	}
	res, err := runFleet(ctx, executor, progress, ignoreSpace, nil)
	if flags.JSON {
		encErr := json.NewEncoder(os.Stdout).Encode(backupRunEnvelope{MachineInterface: MachineInterface, FleetRunResult: res})
		return errors.Join(err, encErr)
	}
	return err
}

// fleetFailed is the run's error once a result exists: nonzero exit for anything
// but a clean run.
func fleetFailed(res backup.FleetRunResult) error {
	switch res.Status {
	case backup.RunOK, "skipped":
		return nil
	case backup.RunPartial:
		n := 0
		for _, t := range res.Targets {
			if t.Status == backup.TargetFailed {
				n++
			}
		}
		return fmt.Errorf("backup run --all: %d target(s) failed", n)
	}
	if res.Error != "" {
		return fmt.Errorf("backup run --all failed: %s", res.Error)
	}
	return fmt.Errorf("backup run --all failed: no target succeeded")
}

// runFleet is the whole run, against a given executor (the seam tests use).
func runFleet(ctx context.Context, exec ssh.Executor, out io.Writer, ignoreSpace bool, hc *http.Client) (backup.FleetRunResult, error) {
	started := time.Now().UTC()
	res := backup.FleetRunResult{StartedAt: started, Targets: []backup.FleetTargetResult{}}

	policy, err := backup.ReadFleetPolicy(ctx, exec)
	if err != nil {
		res.Status, res.Error = backup.RunFailed, err.Error()
		return res, fleetFailed(res)
	}
	if !policy.Configured() {
		res.Status, res.Error = backup.RunFailed, "no backup policy on this server: set one with `teploy server backup-policy set`"
		return res, fleetFailed(res)
	}
	res.Encrypted = policy.Recipient != ""
	if policy.TmpDir != "" {
		os.Setenv("TEPLOY_BACKUP_TMPDIR", policy.TmpDir)
	}

	lock, holder, lerr := backup.AcquireRunLock(backup.RunLockPath(policy.Path), started, fleetRunDeadline+time.Hour)
	if lerr != nil {
		if errors.Is(lerr, backup.ErrRunLocked) {
			// The running one reports; a stuck one stops reporting, and the
			// missing heartbeat is the alert.
			fmt.Fprintf(out, "Another backup run --all is running (pid %d, since %s); this one stands down\n", holder.PID, holder.StartedAt.Format(time.RFC3339))
			res.Status, res.Error = "skipped", "another run holds the lock"
			return res, nil
		}
		return finishFleet(ctx, exec, out, res, policy, started, lerr, hc)
	}
	defer lock.Release()

	target, err := backup.ReadTarget(ctx, exec)
	if err == nil && !TargetConfigured(target) {
		err = fmt.Errorf("the policy has no backup target to write to: set one with `teploy server backup-target set`")
	}
	if err != nil {
		return finishFleet(ctx, exec, out, res, policy, started, err, hc)
	}
	cfg := TargetS3(target)
	cfg.OwnerRecipient = policy.Recipient
	client := backup.NewClient(exec, out)
	if policy.TargetDeadline != "" {
		if d, derr := time.ParseDuration(policy.TargetDeadline); derr == nil {
			client.SetDeadline(d)
		}
	}
	if err := client.EnsureAWSCLIPublic(ctx); err != nil {
		return finishFleet(ctx, exec, out, res, policy, started, err, hc)
	}
	if err := client.PrepareEncryption(ctx, cfg); err != nil {
		return finishFleet(ctx, exec, out, res, policy, started, err, hc)
	}
	if !cfg.Encrypted() {
		res.Warnings = append(res.Warnings, "no recovery key is configured, so objects are stored unencrypted and app .env files are left out of volume backups (teploy backup keygen; teploy server backup-policy set --recipient)")
	}

	ectx, ecancel := context.WithTimeout(ctx, 10*time.Minute)
	targets, uncovered, enumWarnings, err := client.EnumerateFleet(ectx, policy)
	ecancel()
	if err != nil {
		return finishFleet(ctx, exec, out, res, policy, started, err, hc)
	}
	res.UncoveredContainers = uncovered
	res.Warnings = append(res.Warnings, enumWarnings...)
	// Zero targets with a policy: a lost accessory and an empty docker daemon look
	// exactly like this, and reporting ok would hide them. A server meant to have
	// nothing yet says so.
	if len(targets) == 0 && !policy.AllowEmpty {
		return finishFleet(ctx, exec, out, res, policy, started, fmt.Errorf("the policy found nothing to back up on this server: no accessory, app volume or adopted container. If that is right, set allow_empty (teploy server backup-policy set --allow-empty)"), hc)
	}
	if len(targets) > 0 && policy.AllowEmpty {
		res.Warnings = append(res.Warnings, fmt.Sprintf("allow_empty is set and this server has %d target(s): if they all disappear, a run reports ok instead of failing (teploy server backup-policy set --allow-empty=false)", len(targets)))
	}

	// A killed run's leftovers, now that this run owns the lock. Bounded: a wedged
	// janitor must not cost the backups.
	jctx, jcancel := context.WithTimeout(ctx, 5*time.Minute)
	res.CleanedWorkspaces = len(client.CleanStaleWorkspaces(jctx))
	nsSeen := map[string]bool{}
	var namespaces []string
	for _, t := range targets {
		if t.App != "" && !nsSeen[t.App] {
			nsSeen[t.App] = true
			namespaces = append(namespaces, t.App)
		}
	}
	res.AbortedUploads = client.AbortStaleUploads(jctx, cfg, namespaces, started)
	jcancel()

	for _, t := range targets {
		if ctx.Err() != nil {
			r := backup.FleetTargetResult{Kind: t.Kind, App: t.App, Accessory: t.Name, Container: t.Container, Status: backup.TargetFailed, Reason: "the run was stopped before this target: " + ctx.Err().Error()}
			res.Targets = append(res.Targets, r)
			recordFleetTargetEvent(context.WithoutCancel(ctx), exec, r)
			continue
		}
		r := client.RunFleetTarget(ctx, t, cfg, backup.BackupOptions{IgnoreSpace: ignoreSpace})
		res.Targets = append(res.Targets, r)
		recordFleetTargetEvent(ctx, exec, r)
		// A refused prune is a stored backup and a retention that has stopped: not
		// something to find only in one target's JSON.
		for _, w := range r.Warnings {
			res.Warnings = append(res.Warnings, fleetLabel(r)+": "+w)
		}
		switch r.Status {
		case backup.TargetOK:
			fmt.Fprintf(out, "OK      %s (%d bytes)\n", fleetLabel(r), r.SizeBytes)
		case backup.TargetFailed:
			fmt.Fprintf(out, "FAILED  %s: %s\n", fleetLabel(r), r.Reason)
		default:
			fmt.Fprintf(out, "SKIPPED %s: %s\n", fleetLabel(r), r.Reason)
		}
	}
	return finishFleet(ctx, exec, out, res, policy, started, nil, hc)
}

func fleetLabel(r backup.FleetTargetResult) string {
	if r.Container != "" {
		return r.Container
	}
	return r.App + "/volumes"
}

// recordFleetTargetEvent writes the target's backup.run: in the app's history, or
// the adopted namespace's. Retired per-app alert script: a policy run signals
// through its heartbeat (plan 2.3), and legacy `backup run --app` keeps the script.
func recordFleetTargetEvent(ctx context.Context, exec ssh.Executor, r backup.FleetTargetResult) {
	if r.Status == backup.TargetOwnCron || (r.Status == backup.TargetExcluded && !r.SizeGuard) {
		return
	}
	kind := backup.KindAccessory
	if r.Kind == backup.TargetVolumes {
		kind = backup.KindVolumes
	}
	detail := backupRunDetail(r.Result, kind, r.Accessory)
	detail["policy_managed"] = true
	detail["pruned"] = r.Pruned
	if len(r.Warnings) > 0 {
		detail["warnings"] = r.Warnings
	}
	var dbs []string
	for _, o := range r.Objects {
		if o.Database != "" {
			dbs = append(dbs, o.Database)
		}
	}
	if len(dbs) > 0 {
		detail["databases"] = dbs
	}
	if len(r.FailedDatabases) > 0 {
		names := make([]string, 0, len(r.FailedDatabases))
		for db := range r.FailedDatabases {
			names = append(names, db)
		}
		detail["failed_databases"] = names
	}
	ev := events.Event{App: r.App, Kind: events.KindBackupRun, Outcome: events.Succeeded, Detail: detail}
	switch r.Status {
	case backup.TargetFailed:
		ev.Outcome, ev.Detail = events.Failed, events.ErrDetail(detail, errors.New(r.Reason))
	case backup.TargetExcluded:
		ev.Outcome = events.Refused
		detail["reason"] = r.Reason
		detail["size_bytes"] = r.SizeBytes
	}
	events.Record(ctx, exec, ev)
}

// finishFleet settles the status, sends the heartbeat and writes the summary
// event. It runs on a fresh deadline: a run that hit its own must still report.
func finishFleet(ctx context.Context, exec ssh.Executor, out io.Writer, res backup.FleetRunResult, policy backup.FleetPolicy, started time.Time, fatal error, hc *http.Client) (backup.FleetRunResult, error) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()
	res.DurationMs = time.Since(started).Milliseconds()
	backup.SummarizeFleet(&res)
	if fatal != nil {
		res.Status, res.Error = backup.RunFailed, fatal.Error()
	}
	for _, o := range res.Omissions {
		fmt.Fprintf(out, "NOT BACKED UP: %s\n", o)
	}
	for _, w := range res.Warnings {
		fmt.Fprintf(out, "Warning: %s\n", w)
	}
	// up = every target that should have run did: an omission is reported, but a
	// deliberate exclusion is not an outage.
	res.Heartbeats = backup.SendHeartbeats(rctx, policy.HeartbeatURLs, res.Status == backup.RunOK, hc)
	hbOK, hbFailed, tOK, tFailed := 0, 0, 0, 0
	for _, h := range res.Heartbeats {
		if h.OK {
			hbOK++
		} else {
			hbFailed++
			// A dead receiver is worth saying, but never fails the backup.
			fmt.Fprintf(out, "Warning: heartbeat to %s failed: %s\n", h.Host, h.Error)
		}
	}
	for _, t := range res.Targets {
		switch t.Status {
		case backup.TargetOK:
			tOK++
		case backup.TargetFailed:
			tFailed++
		}
	}
	detail := map[string]any{
		"status": res.Status, "targets_ok": tOK, "targets_failed": tFailed, "encrypted": res.Encrypted,
		"duration_ms": res.DurationMs, "heartbeats_ok": hbOK, "heartbeats_failed": hbFailed,
	}
	if len(res.Omissions) > 0 {
		detail["omissions"] = res.Omissions
	}
	if len(res.Warnings) > 0 {
		detail["warnings"] = res.Warnings
	}
	outcome := events.Succeeded
	if res.Status != backup.RunOK {
		outcome = events.Failed
		if res.Error != "" {
			detail["error"] = res.Error
		} else {
			detail["error"] = fmt.Sprintf("%d target(s) failed", tFailed)
		}
	}
	events.Record(rctx, exec, events.Event{App: backup.FleetApp, Kind: events.KindBackupAll, Outcome: outcome, Detail: detail})
	fmt.Fprintf(out, "backup run --all: %s (%d ok, %d failed, %d not backed up)\n", res.Status, tOK, tFailed, len(res.Omissions))
	return res, fleetFailed(res)
}

// ---- server backup-policy ----

// backupPolicyDTO is `server backup-policy show --json`: heartbeat URLs carry
// tokens, so they appear only as hosts.
type backupPolicyDTO struct {
	MachineInterface int                   `json:"machine_interface"`
	Host             string                `json:"host"`
	Configured       bool                  `json:"configured"`
	Path             string                `json:"path,omitempty"`
	Interval         string                `json:"interval,omitempty"`
	Cron             string                `json:"cron,omitempty"`
	Retention        backup.FleetRetention `json:"retention"`
	Excludes         []string              `json:"excludes,omitempty"`
	Adopted          []backup.AdoptedEntry `json:"adopted,omitempty"`
	Volumes          bool                  `json:"volumes"`
	VolumeMaxGB      float64               `json:"volume_max_gb,omitempty"`
	HeartbeatCount   int                   `json:"heartbeat_count"`
	HeartbeatHosts   []string              `json:"heartbeat_hosts,omitempty"`
	Recipient        string                `json:"recipient,omitempty"`
	Encrypted        bool                  `json:"encrypted"`
	AllowEmpty       bool                  `json:"allow_empty,omitempty"`
	TmpDir           string                `json:"tmp_dir,omitempty"`
	TargetDeadline   string                `json:"target_deadline,omitempty"`
}

func newServerBackupPolicyCmd(flags *Flags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "backup-policy",
		Short: "Manage the server's backup policy (the fleet engine's one cron line)",
		Long: "One policy per server, at /etc/teploy/backup-policy.json (0600; a non-root\n" +
			"user goes through passwordless sudo, and one with neither write access nor sudo\n" +
			"keeps ~/.config/teploy/backup-policy.json unless a policy in /etc would shadow it).\n" +
			"A single cron line runs `teploy backup run --all`, which works out at run\n" +
			"time what to back up: every labelled accessory (each of its databases), app\n" +
			"volumes when enabled, and adopted containers. A per-app schedule stays as an\n" +
			"override for that target.",
	}
	cmd.AddCommand(newServerBackupPolicySetCmd(flags))
	cmd.AddCommand(newServerBackupPolicyShowCmd(flags))
	cmd.AddCommand(newServerBackupPolicyUnsetCmd(flags))
	return cmd
}

type policySetFlags struct {
	stdin, heartbeatStdin, noRecipient, clearHeartbeats, volumes, allowEmpty bool
	interval, recipient, tmpDir, targetDeadline                              string
	keepLast, keepDaily, keepWeekly, keepMonthly, maxAgeDays                 int
	volumeMaxGB                                                              float64
	excludes, adopts, removeAdopted                                          []string
}

func newServerBackupPolicySetCmd(flags *Flags) *cobra.Command {
	var f policySetFlags
	cmd := &cobra.Command{
		Use:   "set",
		Short: "Store the policy and install its one cron line",
		Long: "Flags change only what they name; everything else keeps its stored value.\n" +
			"--stdin instead reads the whole policy as JSON on stdin (the machine path).\n" +
			"Heartbeat URLs carry tokens, so they only ever arrive on stdin: with\n" +
			"--heartbeat-stdin, one URL per line.\n\n" +
			"--max-age-days keeps everything younger than that (restic keep-within);\n" +
			"--keep-daily/--keep-weekly/--keep-monthly keep the newest of the last N days,\n" +
			"ISO weeks and months that have a backup. The rules are a union, and the\n" +
			"newest good backup always stays.\n\n" +
			"Examples:\n" +
			"  teploy server backup-policy set --host prod --interval 1h --max-age-days 2 \\\n" +
			"    --keep-daily 7 --keep-weekly 4 --keep-monthly 12 --recipient age1...\n" +
			"  teploy server backup-policy set --host prod --adopt omni_postgres:postgres:all \\\n" +
			"    --adopt wordpress-shared-mysql:mysql:all\n" +
			"  printf 'https://dash/api/push/TOKEN\\n' | teploy server backup-policy set --host prod --heartbeat-stdin",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runServerBackupPolicySet(cmd, flags, &f)
		},
	}
	cmd.Flags().BoolVar(&f.stdin, "stdin", false, "read the whole policy as JSON on stdin")
	cmd.Flags().StringVar(&f.interval, "interval", "", "15m, 30m, 1h, 2h, 3h, 4h, 6h, 8h, 12h, 24h, 7d (or hourly, daily, weekly); none removes the cron line")
	cmd.Flags().IntVar(&f.keepLast, "keep-last", 0, "keep the N most recent backups")
	cmd.Flags().IntVar(&f.keepDaily, "keep-daily", 0, "keep the newest backup of the last N days that have one")
	cmd.Flags().IntVar(&f.keepWeekly, "keep-weekly", 0, "keep the newest backup of the last N ISO weeks that have one")
	cmd.Flags().IntVar(&f.keepMonthly, "keep-monthly", 0, "keep the newest backup of the last N months that have one")
	cmd.Flags().IntVar(&f.maxAgeDays, "max-age-days", 0, "keep everything younger than N days")
	cmd.Flags().StringSliceVar(&f.excludes, "exclude", nil, "leave out: <app>, <app>/<accessory>, <app>/volumes or a container name (replaces the list)")
	cmd.Flags().StringArrayVar(&f.adopts, "adopt", nil, "adopt an unmanaged container: <container>[:<engine>[:all|db1,db2]] (default: detected engine, all databases)")
	cmd.Flags().StringSliceVar(&f.removeAdopted, "remove-adopted", nil, "stop adopting these containers (their backups stay)")
	cmd.Flags().BoolVar(&f.volumes, "volumes", false, "back up app volumes (each archive is a full copy)")
	cmd.Flags().Float64Var(&f.volumeMaxGB, "volume-max-gb", 0, "warn and exclude a volume archive over this many GB")
	cmd.Flags().StringVar(&f.recipient, "recipient", "", "owner recovery public key (`teploy backup keygen`): encrypts every object from now on")
	cmd.Flags().BoolVar(&f.noRecipient, "no-recipient", false, "stop encrypting new objects")
	cmd.Flags().BoolVar(&f.heartbeatStdin, "heartbeat-stdin", false, "read heartbeat URLs from stdin, one per line (replaces the list)")
	cmd.Flags().BoolVar(&f.clearHeartbeats, "clear-heartbeats", false, "remove every heartbeat URL")
	cmd.Flags().BoolVar(&f.allowEmpty, "allow-empty", false, "a run with nothing to back up is not a failure (a server with nothing on it yet)")
	cmd.Flags().StringVar(&f.tmpDir, "tmp-dir", "", "build backups on this disk instead of /var/tmp")
	cmd.Flags().StringVar(&f.targetDeadline, "target-deadline", "", "deadline of one target (default 6h)")
	return cmd
}

// parseAdoptFlag parses <container>[:<engine>[:all|db1,db2]].
func parseAdoptFlag(s string) (backup.AdoptedEntry, error) {
	parts := strings.SplitN(s, ":", 3)
	e := backup.AdoptedEntry{Container: parts[0], Databases: backup.DatabaseSelector{All: true}}
	if len(parts) > 1 {
		e.Engine = parts[1]
	}
	if len(parts) > 2 && parts[2] != "all" {
		e.Databases = backup.DatabaseSelector{Names: strings.Split(parts[2], ",")}
	}
	return e, nil
}

// applyPolicyFlags merges the changed flags over a policy.
func applyPolicyFlags(cmd *cobra.Command, p *backup.FleetPolicy, f *policySetFlags) error {
	ch := cmd.Flags().Changed
	if ch("interval") {
		p.Interval = f.interval
		if f.interval == "none" {
			p.Interval = ""
		}
	}
	if ch("keep-last") {
		p.Retention.KeepLast = f.keepLast
	}
	if ch("keep-daily") {
		p.Retention.KeepDaily = f.keepDaily
	}
	if ch("keep-weekly") {
		p.Retention.KeepWeekly = f.keepWeekly
	}
	if ch("keep-monthly") {
		p.Retention.KeepMonthly = f.keepMonthly
	}
	if ch("max-age-days") {
		p.Retention.MaxAgeDays = f.maxAgeDays
	}
	if ch("exclude") {
		p.Excludes = f.excludes
	}
	if ch("volumes") {
		p.Volumes = f.volumes
	}
	if ch("volume-max-gb") {
		p.VolumeMaxGB = f.volumeMaxGB
	}
	if ch("allow-empty") {
		p.AllowEmpty = f.allowEmpty
	}
	if ch("tmp-dir") {
		p.TmpDir = f.tmpDir
	}
	if ch("target-deadline") {
		p.TargetDeadline = f.targetDeadline
	}
	if ch("recipient") && ch("no-recipient") {
		return fmt.Errorf("--recipient and --no-recipient are mutually exclusive")
	}
	if ch("recipient") {
		p.Recipient = f.recipient
	}
	if f.noRecipient {
		p.Recipient = ""
	}
	for _, name := range f.removeAdopted {
		kept := p.Adopted[:0]
		for _, a := range p.Adopted {
			if a.Container != name {
				kept = append(kept, a)
			}
		}
		p.Adopted = kept
	}
	for _, s := range f.adopts {
		e, err := parseAdoptFlag(s)
		if err != nil {
			return err
		}
		replaced := false
		for i := range p.Adopted {
			if p.Adopted[i].Container == e.Container {
				p.Adopted[i], replaced = e, true
			}
		}
		if !replaced {
			p.Adopted = append(p.Adopted, e)
		}
	}
	if f.clearHeartbeats {
		p.HeartbeatURLs = nil
	}
	return nil
}

func readHeartbeatLines(r io.Reader) ([]string, error) {
	b, err := io.ReadAll(io.LimitReader(r, 1<<16))
	if err != nil {
		return nil, err
	}
	var urls []string
	for _, line := range strings.Split(string(b), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			urls = append(urls, line)
		}
	}
	return urls, nil
}

func runServerBackupPolicySet(cmd *cobra.Command, flags *Flags, f *policySetFlags) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	var err error
	if f.stdin && f.heartbeatStdin {
		return fmt.Errorf("--stdin and --heartbeat-stdin both read stdin: put heartbeat_urls in the JSON")
	}
	if f.stdin {
		// LocalNonPersistentFlags: --host and friends are not policy flags.
		cmd.LocalNonPersistentFlags().Visit(func(fl *pflag.Flag) {
			if fl.Name != "stdin" && fl.Name != "no-recipient" && err == nil {
				err = fmt.Errorf("--stdin takes the whole policy: combine it with no policy flag but --no-recipient")
			}
		})
		if err != nil {
			return err
		}
	}
	var incoming *backup.FleetPolicy
	var heartbeats []string
	if f.stdin {
		dec := json.NewDecoder(io.LimitReader(os.Stdin, 1<<20))
		dec.DisallowUnknownFields()
		var p backup.FleetPolicy
		if err := dec.Decode(&p); err != nil {
			return fmt.Errorf("reading the policy from stdin: %w", err)
		}
		incoming = &p
	} else if f.heartbeatStdin {
		var err error
		if heartbeats, err = readHeartbeatLines(os.Stdin); err != nil {
			return fmt.Errorf("reading heartbeat URLs from stdin: %w", err)
		}
		if len(heartbeats) == 0 {
			return fmt.Errorf("--heartbeat-stdin: no URLs on stdin")
		}
	}

	host, user, key, err := configResolveServerForTarget(flags)
	if err != nil {
		return err
	}
	if !flags.JSON {
		fmt.Printf("Connecting to %s@%s...\n", user, host)
	}
	executor, err := ssh.Connect(ctx, ssh.ConnectConfig{Host: host, User: user, KeyPath: key})
	if err != nil {
		return err
	}
	defer executor.Close()
	return setBackupPolicyOn(ctx, cmd, executor, flags, f, incoming, heartbeats)
}

func setBackupPolicyOn(ctx context.Context, cmd *cobra.Command, executor ssh.Executor, flags *Flags, f *policySetFlags, incoming *backup.FleetPolicy, heartbeats []string) error {
	stored, err := backup.ReadFleetPolicy(ctx, executor)
	if errors.Is(err, backup.ErrPolicyHidden) {
		// This user cannot see /etc/teploy, and is setting a policy of its own in
		// ~/.config: that is the way out of the refusal, not another refusal.
		fmt.Fprintf(os.Stderr, "Warning: %v\nThis user's own policy is stored in ~/.config/teploy and applies to its own runs.\n", err)
		stored, err = backup.FleetPolicy{}, nil
	}
	if err != nil {
		return err
	}
	p := stored
	if incoming != nil {
		p = *incoming
		// A machine-written policy that omits the key must not switch encryption
		// off as a side effect.
		if stored.Recipient != "" && p.Recipient == "" && !f.noRecipient {
			return fmt.Errorf("this policy has no recipient, which would stop encrypting new backups on a server that encrypts them; nothing was changed. Send the recipient, or pass --no-recipient if that is the intent")
		}
	} else {
		if err := applyPolicyFlags(cmd, &p, f); err != nil {
			return err
		}
		if heartbeats != nil {
			p.HeartbeatURLs = heartbeats
		}
	}
	p.V = "1"
	if err := p.Validate(); err != nil {
		return err
	}
	if stored.Recipient != "" && p.Recipient == "" {
		fmt.Fprintln(os.Stderr, "Warning: encryption is now OFF: new backups are stored in plaintext, and app .env files are left out of volume backups")
	}
	if p.Retention.Policy().IsZero() && p.Interval != "" {
		fmt.Fprintln(os.Stderr, "Warning: this policy has no retention rule, so backups are never pruned")
	}

	// Everything that can refuse does so before the policy is stored.
	if p.Interval != "" {
		if err := backup.RequireScheduler(ctx, executor); err != nil {
			return err
		}
	}
	if p.Recipient != "" {
		if err := secret.NewManager(executor).EnsureAge(ctx); err != nil {
			return err
		}
	}
	bin := ""
	if p.Interval != "" {
		if bin, err = ensureFleetBinary(ctx, executor); err != nil {
			return err
		}
	}
	target, err := backup.ReadTarget(ctx, executor)
	if err != nil {
		return err
	}
	if !TargetConfigured(target) {
		fmt.Fprintln(os.Stderr, "Warning: this server has no backup target yet, so runs will fail until `teploy server backup-target set` stores one")
	}

	path, fallback, err := backup.WriteFleetPolicy(ctx, executor, p)
	if err != nil {
		return err
	}
	p.Path = path

	client := backup.NewClient(executor, backupProgressOut(flags))
	cronSpec := ""
	if p.Interval == "" {
		if err := client.RemoveSchedule(ctx, backup.FleetCronMarker); err != nil {
			return err
		}
	} else {
		cronSpec, err = backup.IntervalCron(p.Interval, executor.Host())
		if err != nil {
			return err
		}
		if err := client.SetSchedule(ctx, cronSpec, bin+" backup run --all", backup.FleetCronMarker); err != nil {
			return err
		}
	}
	events.Record(ctx, executor, events.Event{App: backup.FleetApp, Kind: events.KindBackupPolicy, Outcome: events.Succeeded, Detail: map[string]any{
		"interval": p.Interval, "encrypted": p.Recipient != "", "volumes": p.Volumes, "adopted": len(p.Adopted),
	}})
	if flags.JSON {
		return json.NewEncoder(os.Stdout).Encode(policyDTO(executor.Host(), p, cronSpec))
	}
	fmt.Printf("Backup policy stored at %s\n", path)
	if fallback {
		fmt.Println("  /etc/teploy is not writable by this user — the policy lives in ~/.config/teploy instead")
	}
	printPolicy(p, cronSpec)
	return nil
}

func policyDTO(host string, p backup.FleetPolicy, cronSpec string) backupPolicyDTO {
	d := backupPolicyDTO{
		MachineInterface: MachineInterface, Host: host, Configured: p.Configured(), Path: p.Path,
		Interval: p.Interval, Cron: cronSpec, Retention: p.Retention, Excludes: p.Excludes, Adopted: p.Adopted,
		Volumes: p.Volumes, VolumeMaxGB: p.VolumeMaxGB, HeartbeatCount: len(p.HeartbeatURLs),
		Recipient: p.Recipient, Encrypted: p.Recipient != "",
		AllowEmpty: p.AllowEmpty, TmpDir: p.TmpDir, TargetDeadline: p.TargetDeadline,
	}
	for _, h := range backup.MaskedHeartbeats(p.HeartbeatURLs) {
		d.HeartbeatHosts = append(d.HeartbeatHosts, strings.TrimSuffix(h, "/..."))
	}
	return d
}

func printPolicy(p backup.FleetPolicy, cronSpec string) {
	if p.Interval != "" {
		fmt.Printf("  Interval: %s (cron %q: teploy backup run --all)\n", p.Interval, cronSpec)
	} else {
		fmt.Println("  Interval: none (no cron line; manual runs only)")
	}
	r := p.Retention
	fmt.Printf("  Retention: keep last %d, daily %d, weekly %d, monthly %d, everything younger than %d days\n", r.KeepLast, r.KeepDaily, r.KeepWeekly, r.KeepMonthly, r.MaxAgeDays)
	if p.Recipient != "" {
		fmt.Printf("  Encryption: on, to %s and the server's own identity\n", p.Recipient)
	} else {
		fmt.Println("  Encryption: OFF (no recovery key); app .env files are left out of volume backups")
	}
	fmt.Printf("  Volumes: %v", p.Volumes)
	if p.VolumeMaxGB > 0 {
		fmt.Printf(" (cap %.2f GB per archive)", p.VolumeMaxGB)
	}
	fmt.Println()
	if p.AllowEmpty {
		fmt.Println("  Nothing to back up: allowed (--allow-empty)")
	}
	if p.TmpDir != "" {
		fmt.Printf("  Builds backups in: %s\n", p.TmpDir)
	}
	if p.TargetDeadline != "" {
		fmt.Printf("  Deadline per target: %s\n", p.TargetDeadline)
	}
	for _, a := range p.Adopted {
		dbs := "all"
		if !a.Databases.All {
			dbs = strings.Join(a.Databases.Names, ", ")
		}
		fmt.Printf("  Adopted: %s (%s; databases: %s)\n", a.Container, orDetected(a.Engine), dbs)
	}
	if len(p.Excludes) > 0 {
		fmt.Printf("  Excludes: %s\n", strings.Join(p.Excludes, ", "))
	}
	for _, h := range backup.MaskedHeartbeats(p.HeartbeatURLs) {
		fmt.Printf("  Heartbeat: %s\n", h)
	}
}

func orDetected(engine string) string {
	if engine == "" {
		return "engine detected"
	}
	return engine
}

// ensureFleetBinary is ensureScheduledBackupBinary for the policy line: the
// server's teploy must speak `backup run --all`, and an older one is upgraded.
func ensureFleetBinary(ctx context.Context, executor ssh.Executor) (string, error) {
	bin := scheduledTeployPath()
	supports := func() bool {
		return !ssh.RunDetailed(ctx, executor, ssh.ShellQuote(bin)+" backup run --help 2>/dev/null | grep -q -- --all").Failed()
	}
	if supports() {
		return bin, nil
	}
	if bin != serverTeployPath {
		return "", fmt.Errorf("%s does not support `backup run --all` (check TEPLOY_SERVER_BINARY)", bin)
	}
	if _, err := deployTeployBinaryToServer(ctx, executor, serverTeployPath, "backup run"); err != nil {
		return "", fmt.Errorf("the server's teploy binary does not support `backup run --all` and installing one failed: %w", err)
	}
	if !supports() {
		return "", fmt.Errorf("the installed teploy binary still does not support `backup run --all` — the latest release may predate it")
	}
	return bin, nil
}

func newServerBackupPolicyShowCmd(flags *Flags) *cobra.Command {
	return &cobra.Command{
		Use:   "show",
		Short: "Show the stored policy (heartbeat URLs only as hosts)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
			defer cancel()
			host, user, key, err := configResolveServerForTarget(flags)
			if err != nil {
				return err
			}
			executor, err := ssh.Connect(ctx, ssh.ConnectConfig{Host: host, User: user, KeyPath: key})
			if err != nil {
				return err
			}
			defer executor.Close()
			p, err := backup.ReadFleetPolicy(ctx, executor)
			if err != nil {
				return err
			}
			cronSpec := ""
			if lines, err := backup.ReadCrontab(ctx, executor); err == nil {
				for _, line := range lines {
					if m := backup.FindCronMarker(line); m != nil && m.Kind == backup.KindFleet {
						cronSpec = m.Cron
					}
				}
			}
			if flags.JSON {
				return json.NewEncoder(os.Stdout).Encode(policyDTO(executor.Host(), p, cronSpec))
			}
			if !p.Configured() {
				fmt.Println("No backup policy configured — set one with `teploy server backup-policy set`")
				return nil
			}
			fmt.Printf("Backup policy (%s)\n", p.Path)
			printPolicy(p, cronSpec)
			return nil
		},
	}
}

func newServerBackupPolicyUnsetCmd(flags *Flags) *cobra.Command {
	return &cobra.Command{
		Use:   "unset",
		Short: "Remove the policy and its cron line (stored backups are kept)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
			defer cancel()
			host, user, key, err := configResolveServerForTarget(flags)
			if err != nil {
				return err
			}
			executor, err := ssh.Connect(ctx, ssh.ConnectConfig{Host: host, User: user, KeyPath: key})
			if err != nil {
				return err
			}
			defer executor.Close()
			// The policy first: it can refuse (a policy this user cannot remove stays in
			// force), and that must not leave it in force with no line running it. A line
			// left behind by a failure below fails loudly every interval instead.
			removed, err := backup.UnsetFleetPolicy(ctx, executor)
			if err != nil {
				return err
			}
			if err := backup.NewClient(executor, io.Discard).RemoveSchedule(ctx, backup.FleetCronMarker); err != nil {
				return err
			}
			if len(removed) == 0 {
				fmt.Println("No backup policy configured")
				return nil
			}
			for _, p := range removed {
				fmt.Printf("Removed %s\n", p)
			}
			fmt.Println("The cron line is gone and new backups stop. Stored backups were not touched; nothing else on this server encrypts them any more.")
			return nil
		},
	}
}

// ---- backup status ----

// backupStatusDTO is `backup status --json` (backup-status-envelope).
type backupStatusDTO struct {
	MachineInterface int       `json:"machine_interface"`
	Host             string    `json:"host"`
	ObservedAt       time.Time `json:"observed_at"`
	backup.FleetStatus
}

func newBackupStatusCmd(flags *Flags) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Every database on the server: covered, stale, failing, uncovered, excluded or adopted",
		Long: "One read-only call for a whole server: every DB-like container (labelled or\n" +
			"not), every app's volumes, what covers each, its last success and failure,\n" +
			"its newest object and last verify, every database nothing backs up, and the\n" +
			"target and policy. No secrets. --host names the server.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
			defer cancel()
			host, user, key, err := configResolveServerForTarget(flags)
			if err != nil {
				return err
			}
			executor, err := ssh.Connect(ctx, ssh.ConnectConfig{Host: host, User: user, KeyPath: key})
			if err != nil {
				return err
			}
			defer executor.Close()
			now := time.Now().UTC()
			sctx, scancel := context.WithTimeout(ctx, 10*time.Minute)
			defer scancel()
			st, err := backup.ReadFleetStatus(sctx, executor, now)
			if err != nil {
				return err
			}
			if flags.JSON {
				return json.NewEncoder(os.Stdout).Encode(backupStatusDTO{MachineInterface: MachineInterface, Host: executor.Host(), ObservedAt: now, FleetStatus: st})
			}
			printBackupStatus(st)
			return nil
		},
	}
}

func printBackupStatus(st backup.FleetStatus) {
	if st.Target.Configured {
		fmt.Printf("Target:  s3://%s", st.Target.Bucket)
		if st.Target.Prefix != "" {
			fmt.Printf("/%s", st.Target.Prefix)
		}
		if !st.Target.Reachable {
			fmt.Printf(" (NOT REACHABLE: %s)", st.Target.Error)
		}
		fmt.Println()
	} else {
		fmt.Println("Target:  none configured")
	}
	if st.Policy.Configured {
		fmt.Printf("Policy:  every %s, encryption %v, %d heartbeat(s)\n", st.Policy.Interval, st.Encrypted, st.Policy.HeartbeatCount)
	} else {
		fmt.Println("Policy:  none configured")
	}
	if st.LastRun != nil {
		fmt.Printf("Last run: %s at %s (%d ok, %d failed)\n", st.LastRun.Status, st.LastRun.At.Format(time.RFC3339), st.LastRun.TargetsOK, st.LastRun.TargetsFailed)
	}
	for _, e := range st.Entries {
		newest := "-"
		if e.Newest != nil {
			newest = fmt.Sprintf("%s %d bytes", e.Newest.CreatedAt, e.Newest.SizeBytes)
			if e.Newest.Encrypted {
				newest += " encrypted"
			}
		}
		fmt.Printf("%-36s %-10s %-10s %s", e.Name, e.Engine, e.Coverage, newest)
		if e.Reason != "" {
			fmt.Printf("  (%s)", e.Reason)
		}
		fmt.Println()
	}
	if len(st.UncoveredDatabases) > 0 {
		fmt.Printf("NOT BACKED UP: %s\n", strings.Join(st.UncoveredDatabases, ", "))
	}
	for _, w := range st.Warnings {
		fmt.Printf("Warning: %s\n", w)
	}
}

// ---- backup keygen ----

func newBackupKeygenCmd(flags *Flags) *cobra.Command {
	return &cobra.Command{
		Use:   "keygen",
		Short: "Generate a recovery keypair, printed once and stored nowhere",
		Long: "Generates the owner recovery key backups are encrypted to. The private key\n" +
			"is printed once, here, and is not written anywhere: store it offline. Without\n" +
			"it, a lost server's backups cannot be read. Give the public key to\n" +
			"`teploy server backup-policy set --recipient`, then prove the private key\n" +
			"works: `teploy backup fetch <key> --identity <file>`.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return writeBackupKeygen(os.Stdout, flags.JSON)
		},
	}
}

func writeBackupKeygen(w io.Writer, jsonOut bool) error {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		return fmt.Errorf("generating a keypair: %w", err)
	}
	if jsonOut {
		return json.NewEncoder(w).Encode(map[string]any{"machine_interface": MachineInterface, "public_key": id.Recipient().String(), "private_key": id.String()})
	}
	fmt.Fprintln(w, "# Teploy backup recovery key. Shown once; stored nowhere. Keep it offline.")
	fmt.Fprintf(w, "# public key: %s\n", id.Recipient())
	fmt.Fprintln(w, id.String())
	return nil
}

// ---- backup fetch ----

// backupFetchDTO is `backup fetch --json`.
type backupFetchDTO struct {
	MachineInterface int    `json:"machine_interface"`
	Key              string `json:"key"`
	Output           string `json:"output"`
	SizeBytes        int64  `json:"size_bytes"`
	Encrypted        bool   `json:"encrypted"`
}

func newBackupFetchCmd(flags *Flags) *cobra.Command {
	var identity, output, region, endpoint string
	cmd := &cobra.Command{
		Use:   "fetch <key>",
		Short: "Download and decrypt one backup object here, with no server involved",
		Long: "Runs on THIS machine: the owner's recovery drill, and the way back when the\n" +
			"server is gone. It needs the aws CLI, the bucket's credentials\n" +
			"(TEPLOY_S3_ACCESS_KEY/SECRET_KEY or AWS_* in the environment) and, for a .age\n" +
			"object, the recovery key (--identity, the file `teploy backup keygen`'s output\n" +
			"was saved to). The key is the s3:// URI from `teploy backup list`. A partial\n" +
			"or wrong result is never left behind: the output appears only once it is\n" +
			"complete and authenticated.\n\n" +
			"Example:\n" +
			"  teploy backup fetch s3://b/prod/app/accessories/db/20261104-030000-ab.pgdump.age \\\n" +
			"    --identity recovery.txt --endpoint https://s3.example.com",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
			defer cancel()
			dto, err := runBackupFetch(ctx, args[0], identity, output, region, endpoint)
			if err != nil {
				return err
			}
			if flags.JSON {
				return json.NewEncoder(os.Stdout).Encode(dto)
			}
			fmt.Printf("Fetched %s (%d bytes)%s\n", dto.Output, dto.SizeBytes, map[bool]string{true: ", decrypted with the given identity", false: ""}[dto.Encrypted])
			return nil
		},
	}
	cmd.Flags().StringVar(&identity, "identity", "", "age identity file (required for .age objects)")
	cmd.Flags().StringVarP(&output, "output", "o", "", "where to write the result (default: the object's name, minus .age, in this directory)")
	cmd.Flags().StringVar(&region, "region", "", "AWS region (default: us-east-1)")
	cmd.Flags().StringVar(&endpoint, "endpoint", "", "S3-compatible endpoint URL (MinIO/B2/R2)")
	return cmd
}

func runBackupFetch(ctx context.Context, key, identity, output, region, endpoint string) (backupFetchDTO, error) {
	var dto backupFetchDTO
	if !strings.HasPrefix(key, "s3://") || strings.ContainsAny(key, "\r\n\x00") {
		return dto, fmt.Errorf("the key must be an s3:// URI (see `teploy backup list`)")
	}
	encrypted := strings.HasSuffix(key, ".age")
	var ids []age.Identity
	if encrypted {
		if identity == "" {
			return dto, fmt.Errorf("%s is encrypted: pass --identity <recovery key file>", filepath.Base(key))
		}
		f, err := os.Open(identity)
		if err != nil {
			return dto, fmt.Errorf("opening the identity file: %w", err)
		}
		ids, err = age.ParseIdentities(f)
		f.Close()
		if err != nil {
			return dto, fmt.Errorf("reading the identity file: %w", err)
		}
	}
	if endpoint != "" {
		if err := backup.ValidateEndpoint(endpoint); err != nil {
			return dto, err
		}
	}
	if region == "" {
		region = "us-east-1"
	}
	if err := backup.ValidateRegion(region); err != nil {
		return dto, err
	}
	if output == "" {
		output = strings.TrimSuffix(filepath.Base(key), ".age")
	}
	if _, err := os.Lstat(output); err == nil {
		return dto, fmt.Errorf("%s already exists: refusing to overwrite it (choose another --output)", output)
	}
	dir := filepath.Dir(output)
	tmp, err := os.MkdirTemp(dir, ".teploy-fetch-")
	if err != nil {
		return dto, fmt.Errorf("creating a private work directory beside %s: %w", output, err)
	}
	defer os.RemoveAll(tmp)
	raw := filepath.Join(tmp, "object")
	if err := localAWSCopy(ctx, key, raw, region, endpoint); err != nil {
		return dto, err
	}
	result := raw
	if encrypted {
		result = filepath.Join(tmp, "plain")
		if err := decryptFile(raw, result, ids); err != nil {
			return dto, fmt.Errorf("decrypting %s with %s: %w (a recovery key from before a rotation is needed for older objects)", filepath.Base(key), identity, err)
		}
	}
	if err := os.Chmod(result, 0o600); err != nil {
		return dto, err
	}
	st, err := os.Stat(result)
	if err != nil {
		return dto, err
	}
	if err := os.Rename(result, output); err != nil {
		return dto, err
	}
	return backupFetchDTO{MachineInterface: MachineInterface, Key: key, Output: output, SizeBytes: st.Size(), Encrypted: encrypted}, nil
}

func decryptFile(src, dst string, ids []age.Identity) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	r, err := age.Decrypt(in, ids...)
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, r); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// ---- adopted addressing ----

// resolveBackupApp is resolveApp plus the adopted namespace: with adopted, the
// target is the server named by --host, and the app is the namespace.
func resolveBackupApp(ctx context.Context, flags *Flags, appName string, adopted bool) (*config.AppConfig, ssh.Executor, error) {
	if !adopted {
		return resolveApp(ctx, flags, appName)
	}
	if appName != "" {
		return nil, nil, fmt.Errorf("--adopted and --app are mutually exclusive: an adopted container belongs to no app")
	}
	host, user, key, err := configResolveServerForTarget(flags)
	if err != nil {
		return nil, nil, err
	}
	executor, err := ssh.Connect(ctx, ssh.ConnectConfig{Host: host, User: user, KeyPath: key})
	if err != nil {
		return nil, nil, err
	}
	return &config.AppConfig{App: backup.AdoptedApp}, executor, nil
}

// applyAdoptedEngine gives the client the policy entry's engine: for an adopted
// container that override has no label to live on.
func applyAdoptedEngine(ctx context.Context, client *backup.Client, exec ssh.Executor, app, container string) error {
	if app != backup.AdoptedApp {
		return nil
	}
	if err := backup.ValidateContainerName(container); err != nil {
		return err
	}
	p, err := backup.ReadFleetPolicy(ctx, exec)
	if err != nil {
		return err
	}
	for _, a := range p.Adopted {
		if a.Container == container {
			return client.SetEngineOverride(container, a.Engine)
		}
	}
	return nil
}

// ---- backup before destructive operations ----

// preBackupScope says what a safety backup must cover.
type preBackupScope struct {
	// Database scopes a restore's backup to what it replaces ("" = the primary).
	Database string
	// All is for an operation that destroys the whole accessory: purge and upgrade.
	// Every database must be in the backup, or the operation is refused.
	All bool
	// IgnoreSpace carries --ignore-space-check: a restore is sometimes how a full
	// disk gets fixed.
	IgnoreSpace bool
}

const noBackupHint = "Fix it, or pass --no-backup to go ahead without one (a restore over corrupt data, where the dump cannot succeed, is what that flag is for)"

// noSuchContainer is the one inspect failure that means there is nothing there. A
// docker blip must not read as nothing to protect.
func noSuchContainer(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no such object") || strings.Contains(msg, "no such container")
}

// preOperationBackup stores a copy of what op is about to destroy. A failure
// refuses the operation and says exactly what is at stake; --no-backup is the
// operator's explicit way past it (a restore is sometimes the fix for what is
// breaking the backup, and the local safety copy still exists). Nothing to protect
// (no such container, a database already gone) is not a failure.
func preOperationBackup(ctx context.Context, client *backup.Client, exec ssh.Executor, app, accessory string, cfg backup.S3Config, op string, scope preBackupScope) (string, error) {
	var res backup.BackupResult
	var err error
	if accessory == "" {
		// An empty or missing tree has nothing to protect, and restoring into one
		// is exactly what must not be refused. An unreadable one is not empty.
		d := ssh.ShellQuote("/deployments/" + app + "/volumes")
		out, lsErr := exec.Run(ctx, "if [ ! -e "+d+" ]; then echo empty; elif x=$(ls -A "+d+"); then if [ -n \"$x\" ]; then echo yes; else echo empty; fi; else exit 1; fi")
		if lsErr != nil {
			return "", fmt.Errorf("could not read %s's volumes before %s, so nothing was changed: %w\n%s", app, op, lsErr, noBackupHint)
		}
		if strings.TrimSpace(out) != "yes" {
			return "", nil
		}
		res, err = client.BackupVolumesWith(ctx, app, cfg, backup.BackupOptions{IgnoreSpace: scope.IgnoreSpace, SafetyCopy: true})
		recordPreBackupEvent(ctx, exec, app, accessory, res, err, op)
		if err != nil {
			return "", fmt.Errorf("the automatic backup before %s failed, so nothing was changed: %w\n%s", op, err, noBackupHint)
		}
		return res.ID, nil
	}
	image, env, inspErr := client.InspectAccessory(ctx, app, accessory)
	if inspErr != nil {
		if noSuchContainer(inspErr) {
			return "", nil
		}
		return "", fmt.Errorf("could not inspect %s before %s, so nothing was changed: %w\n%s", backup.ContainerName(app, accessory), op, inspErr, noBackupHint)
	}
	opts := backup.BackupOptions{IgnoreSpace: scope.IgnoreSpace, SafetyCopy: true}
	if scope.All {
		opts.AllDatabases = true
	} else {
		// Only what op will replace, by name: a database that is already gone has
		// nothing to protect, and restoring it is exactly what must not be refused.
		opts.SingleDatabase = scope.Database
		if opts.SingleDatabase == "" {
			opts.SingleDatabase = client.PrimaryDatabase(ctx, app, accessory, image, env)
		}
	}
	res, err = client.AccessoryBackupWith(ctx, app, accessory, image, env, cfg, opts)
	if err != nil || len(res.Objects) > 0 {
		recordPreBackupEvent(ctx, exec, app, accessory, res, err, op)
	}
	if err != nil {
		return "", fmt.Errorf("the automatic backup of %s before %s failed, so nothing was changed: %w\n%s", backup.ContainerName(app, accessory), op, err, noBackupHint)
	}
	if scope.All && len(res.UncoveredDatabases) > 0 {
		return "", fmt.Errorf("the automatic backup of %s before %s does not cover %s (names the dump tools cannot address), so nothing was changed\n%s", backup.ContainerName(app, accessory), op, strings.Join(res.UncoveredDatabases, ", "), noBackupHint)
	}
	if len(res.Objects) == 0 {
		return "", nil
	}
	return res.ID, nil
}

func recordPreBackupEvent(ctx context.Context, exec ssh.Executor, app, accessory string, res backup.BackupResult, err error, op string) {
	kind := backup.KindAccessory
	if accessory == "" {
		kind = backup.KindVolumes
	}
	detail := backupRunDetail(res, kind, accessory)
	detail["trigger"] = op
	recordAppEvent(ctx, exec, app, events.KindBackupRun, err, detail)
}

// placeIdentity puts a local identity file on the server for one operation, in a
// private directory removed by the returned cleanup. A backup encrypted by another
// host can be read no other way; that is the one time the recovery key leaves the
// owner's machine, and only when asked for by flag. The janitor and doctor know the
// directory, if a kill leaves it behind.
func placeIdentity(ctx context.Context, exec ssh.Executor, identity string) (string, func(), error) {
	none := func() {}
	if identity == "" {
		return "", none, nil
	}
	body, err := os.ReadFile(identity)
	if err != nil {
		return "", none, fmt.Errorf("reading the identity file: %w", err)
	}
	if _, err := age.ParseIdentities(strings.NewReader(string(body))); err != nil {
		return "", none, fmt.Errorf("%s is not an age identity file: %w", identity, err)
	}
	out, err := exec.Run(ctx, "umask 077; mktemp -d /var/tmp/teploy-dr-identity.XXXXXXXX")
	if err != nil {
		return "", none, fmt.Errorf("creating a private directory for the identity: %w", err)
	}
	dir := strings.TrimSpace(out)
	cleanup := func() { exec.Run(context.WithoutCancel(ctx), "rm -rf "+ssh.ShellQuote(dir)) }
	path := dir + "/identity"
	if err := exec.Upload(ctx, strings.NewReader(string(body)), path, "0600"); err != nil {
		cleanup()
		return "", none, fmt.Errorf("placing the identity: %w", err)
	}
	return path, cleanup, nil
}

// resolvePreBackupTarget is the S3 config the safety backups use: the server's
// stored target. No target is a refusal naming the way out.
func resolvePreBackupTarget(ctx context.Context, exec ssh.Executor, op string) (backup.S3Config, error) {
	target, err := backup.ReadTarget(ctx, exec)
	if err != nil {
		return backup.S3Config{}, err
	}
	if !TargetConfigured(target) {
		return backup.S3Config{}, fmt.Errorf("%s destroys data and this server has no backup target to save a copy to: set one with `teploy server backup-target set`, or pass --no-backup to go ahead without one", op)
	}
	cfg := TargetS3(target)
	if err := backup.ApplyPolicyEncryption(ctx, exec, &cfg); err != nil {
		return backup.S3Config{}, err
	}
	return cfg, nil
}

// deployNoBackup is `deploy --no-backup`.
var deployNoBackup bool

// backupBeforeDeployHook backs up each running database accessory before a deploy
// whose pre_deploy hook is about to run. A first deploy has nothing to protect.
func backupBeforeDeployHook(ctx context.Context, exec ssh.Executor, appCfg *config.AppConfig, out io.Writer) error {
	client := backup.NewClient(exec, out)
	var cfg backup.S3Config
	haveCfg := false
	for _, name := range sortedAccessoryNames(appCfg.Accessories) {
		image, env, err := client.InspectAccessory(ctx, appCfg.App, name)
		if err != nil {
			if noSuchContainer(err) {
				continue
			}
			return fmt.Errorf("could not inspect %s before this deploy's pre_deploy hook: %w\n%s", backup.ContainerName(appCfg.App, name), err, noBackupHint)
		}
		det, err := client.ResolveEngine(ctx, appCfg.App, name, image, env)
		if err != nil || !det.IsDB() {
			continue
		}
		if !haveCfg {
			if cfg, err = resolvePreBackupTarget(ctx, exec, "a deploy with backup_before_hooks"); err != nil {
				return err
			}
			haveCfg = true
		}
		id, err := preOperationBackup(ctx, client, exec, appCfg.App, name, cfg, "this deploy's pre_deploy hook", preBackupScope{All: true})
		if err != nil {
			return err
		}
		if id != "" {
			fmt.Fprintf(out, "Backed up %s before the pre_deploy hook: %s\n", backup.ContainerName(appCfg.App, name), id)
		}
	}
	return nil
}
