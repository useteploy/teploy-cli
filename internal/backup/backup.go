package backup

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/useteploy/teploy/internal/ssh"
)

const deploymentsDir = "/deployments"

// errRecoveryIncomplete marks restore failures whose recovery paths are
// RETAINED on the server (recovery dir and/or staged tree) — the callers
// must not run their broad run-directory cleanup over those artifacts, and
// the error messages name them as kept. Without this marker the callers'
// deferred cleanup deleted the very directories the errors had just
// described as retained (TCL-43).
var errRecoveryIncomplete = errors.New("recovery incomplete")

// safeName matches safe values for shell interpolation: alphanumeric, hyphens, dots.
var safeName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// ValidateBucket checks that an S3 bucket name is safe for shell use.
func ValidateBucket(bucket string) error {
	if !safeName.MatchString(bucket) || len(bucket) > 63 {
		return fmt.Errorf("invalid bucket name %q — must be alphanumeric with hyphens/dots (max 63 chars)", bucket)
	}
	return nil
}

// ValidateRegion checks that an AWS region string is safe for shell use.
func ValidateRegion(region string) error {
	if !safeName.MatchString(region) || len(region) > 25 {
		return fmt.Errorf("invalid region %q — must be alphanumeric with hyphens", region)
	}
	return nil
}

// backupIDRE matches a backup identity: the historical timestamp form
// (20060102-150405) or the collision-proof form newBackupID writes
// (timestamp + 16 hex of randomness, audit A44) — two backups of the same
// app inside one second used to target the same S3 key, silently
// replacing each other.
var backupIDRE = regexp.MustCompile(`^[0-9]{8}-[0-9]{6}(-[0-9a-f]{16})?$`)

// newBackupID mints a backup identity that is timestamp-ordered AND
// unique within the same second (A44).
func newBackupID(now time.Time) (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generating backup id: %w", err)
	}
	return now.UTC().Format("20060102-150405") + "-" + hex.EncodeToString(b[:]), nil
}

// ValidateDate checks that a date/timestamp backup identity is exactly the
// expected grammar (legacy timestamp or newBackupID's collision-proof
// form) and that its timestamp part parses — anything else is rejected
// before it reaches a shell or an S3 key.
func ValidateDate(date string) error {
	if !backupIDRE.MatchString(date) {
		return fmt.Errorf("invalid backup id %q — expected format like 20060102-150405[-0123456789abcdef]", date)
	}
	if _, err := time.Parse("20060102-150405", date[:15]); err != nil {
		return fmt.Errorf("invalid backup id %q — timestamp part is not a real date", date)
	}
	return nil
}

// ValidateSchedule checks that a cron schedule is a complete, well-formed
// five-field expression. The old check was character-set only, so a
// four/six-field string or an out-of-range value passed validation and
// produced a crontab entry cron silently never ran.
func ValidateSchedule(schedule string) error {
	fields := strings.Fields(schedule)
	if len(fields) != 5 {
		return fmt.Errorf("invalid cron schedule %q — expected five fields (minute hour day-of-month month day-of-week)", schedule)
	}
	ranges := []struct {
		name     string
		min, max int
	}{
		{"minute", 0, 59},
		{"hour", 0, 23},
		{"day-of-month", 1, 31},
		{"month", 1, 12},
		{"day-of-week", 0, 7}, // 0 and 7 are both Sunday
	}
	for i, field := range fields {
		r := ranges[i]
		for _, part := range strings.Split(field, ",") {
			bounds, step, hasStep := strings.Cut(part, "/")
			if hasStep {
				if !unsignedDecimal(step) {
					return fmt.Errorf("invalid cron schedule %q — step %q in %s is not a positive number", schedule, step, r.name)
				}
				stepN, err := strconv.Atoi(step)
				if err != nil || stepN < 1 {
					return fmt.Errorf("invalid cron schedule %q — step %q in %s is not a positive number", schedule, step, r.name)
				}
			}
			lo, hi, isRange := strings.Cut(bounds, "-")
			if !validCronValue(lo, r.min, r.max) {
				return fmt.Errorf("invalid cron schedule %q — %s value %q out of range %d-%d", schedule, r.name, lo, r.min, r.max)
			}
			if isRange {
				// A range needs two in-range NUMERIC endpoints in ascending
				// order: '5-1', '5-*', and '*-5' all used to pass because
				// '*' and each bare number are individually valid values
				// (TCL-46).
				if lo == "*" || hi == "*" {
					return fmt.Errorf("invalid cron schedule %q — wildcards are not range endpoints in %s (%q)", schedule, r.name, part)
				}
				if !validCronValue(hi, r.min, r.max) {
					return fmt.Errorf("invalid cron schedule %q — %s range end %q out of range %d-%d", schedule, r.name, hi, r.min, r.max)
				}
				loN, loErr := strconv.Atoi(lo)
				hiN, hiErr := strconv.Atoi(hi)
				if loErr != nil || hiErr != nil || loN > hiN {
					return fmt.Errorf("invalid cron schedule %q — %s range %q must be ascending numeric bounds", schedule, r.name, part)
				}
			}
		}
	}
	return nil
}

// validCronValue accepts "*" or a bare UNSIGNED number within range.
// Empty strings (e.g. from "1,,2") and signed forms like "+1" are
// rejected — strconv.Atoi accepted the plus sign, which is not the
// decimal-field grammar cron itself accepts (audit A46).
func validCronValue(v string, min, max int) bool {
	if v == "*" {
		return true
	}
	if !unsignedDecimal(v) {
		return false
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return false
	}
	return n >= min && n <= max
}

// unsignedDecimal reports whether s is one or more ASCII digits.
func unsignedDecimal(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// S3Config holds S3 bucket and credentials info (stored on server).
//
// Endpoint (optional) points at an S3-compatible server instead of AWS —
// e.g. a self-hosted MinIO accessory (`http://<app>-minio:9000`) or
// B2/R2/Bunny. AccessKey/SecretKey (optional) are passed inline to the aws
// CLI call as env vars for that command only, never written to disk on the
// server — omit them to use the server's ambient AWS credentials as before.
type S3Config struct {
	Bucket    string
	Region    string
	Endpoint  string
	AccessKey string
	SecretKey string
}

// AWS builds an aws-CLI invocation for this config: inline credential env
// (if set), the subcommand, then --region and --endpoint-url as configured.
func (s3 S3Config) AWS(args string) string {
	var b strings.Builder
	if s3.AccessKey != "" {
		b.WriteString("AWS_ACCESS_KEY_ID=" + ssh.ShellQuote(s3.AccessKey) + " ")
	}
	if s3.SecretKey != "" {
		b.WriteString("AWS_SECRET_ACCESS_KEY=" + ssh.ShellQuote(s3.SecretKey) + " ")
	}
	b.WriteString("aws " + args + " --region " + ssh.ShellQuote(s3.Region))
	if s3.Endpoint != "" {
		b.WriteString(" --endpoint-url " + ssh.ShellQuote(s3.Endpoint))
	}
	return b.String()
}

// Client performs backup and restore operations on a remote server via SSH.
type Client struct {
	exec ssh.Executor
	out  io.Writer
}

// NewClient creates a new backup client.
func NewClient(exec ssh.Executor, out io.Writer) *Client {
	return &Client{exec: exec, out: out}
}

// The app's .env rides inside the archive as a top-level `.env` member,
// which RestoreVolumes puts back beside the volumes directory. The old
// command passed the absolute .env path as a second tar argument: tar
// strips the leading slash, so restore unpacked it to
// volumes/deployments/<app>/.env — a nested host-path artifact nobody
// ever read back, while the live .env stayed stale.
//
// The archive is built inside a private 0700 mktemp workspace: it contains
// the app's .env and volume data, and the old predictable /tmp path exposed
// it to other local users under the default umask (audit F34).
func (c *Client) BackupVolumes(ctx context.Context, app string, s3 S3Config) error {
	if err := c.ensureAWSCLI(ctx); err != nil {
		return err
	}

	timestamp, err := newBackupID(time.Now())
	if err != nil {
		return err
	}
	volumesDir := fmt.Sprintf("%s/%s/volumes", deploymentsDir, app)
	appDir := fmt.Sprintf("%s/%s", deploymentsDir, app)

	runOut, err := c.exec.Run(ctx, "umask 077; mktemp -d /tmp/teploy-backup.XXXXXXXX")
	if err != nil {
		return fmt.Errorf("creating backup workspace: %w", err)
	}
	workDir := strings.TrimSpace(runOut)
	cleanup := func() {
		c.exec.Run(context.WithoutCancel(ctx), "rm -rf "+ssh.ShellQuote(workDir))
	}
	archivePath := workDir + "/backup.tar.gz"
	s3Key := fmt.Sprintf("s3://%s/%s/volumes/%s.tar.gz", s3.Bucket, app, timestamp)

	// An explicit if/else, not `tar ... || tar ...`: the old fallback
	// re-ran the tar without .env whenever the first tar failed for ANY
	// reason, masking real archive failures as successes.
	fmt.Fprintf(c.out, "Archiving volumes for %s...\n", app)
	cmd := fmt.Sprintf(
		"if [ -f %s ]; then tar -czf %s -C %s . -C %s .env; else tar -czf %s -C %s .; fi",
		ssh.ShellQuote(appDir+"/.env"),
		ssh.ShellQuote(archivePath), ssh.ShellQuote(volumesDir), ssh.ShellQuote(appDir),
		ssh.ShellQuote(archivePath), ssh.ShellQuote(volumesDir),
	)
	if _, err := c.exec.Run(ctx, cmd); err != nil {
		cleanup()
		return fmt.Errorf("creating archive: %w", err)
	}

	// Upload to S3.
	fmt.Fprintf(c.out, "Uploading to %s...\n", s3Key)
	uploadCmd := s3.AWS(fmt.Sprintf("s3 cp %s %s", ssh.ShellQuote(archivePath), ssh.ShellQuote(s3Key)))
	if _, err := c.exec.Run(ctx, uploadCmd); err != nil {
		cleanup()
		return fmt.Errorf("uploading to S3: %w", err)
	}

	cleanup()
	fmt.Fprintf(c.out, "Backup complete: %s\n", s3Key)
	return nil
}

// RestoreVolumes downloads and extracts a volume backup from S3. If the
// archive carries the app-level .env (see BackupVolumes), it is restored
// beside the volumes directory in the app directory — the pre-restore file
// is kept as .env.pre-restore so overwriting credentials is recoverable.
//
// Every scratch path (download, staging, staged env) lives under one private
// mktemp run directory: the old fixed /tmp names were shared by concurrent
// restores and by remnants of interrupted ones, so a failed restore followed
// by an archive without an .env could consume the previous run's stale
// staged env (audit F33). The promotion's recovery directory is retained
// until the .env commit also succeeds, so volume data and env are replaced
// as one recoverable unit (audit F35).
func (c *Client) RestoreVolumes(ctx context.Context, app, date string, s3 S3Config) error {
	if err := ValidateDate(date); err != nil {
		return err
	}
	if err := c.ensureAWSCLI(ctx); err != nil {
		return err
	}

	volumesDir := fmt.Sprintf("%s/%s/volumes", deploymentsDir, app)
	appDir := fmt.Sprintf("%s/%s", deploymentsDir, app)
	envPath := appDir + "/.env"
	s3Key := fmt.Sprintf("s3://%s/%s/volumes/%s.tar.gz", s3.Bucket, app, date)

	runOut, err := c.exec.Run(ctx, "umask 077; mktemp -d /tmp/teploy-volume-restore.XXXXXXXX")
	if err != nil {
		return fmt.Errorf("creating restore workspace: %w", err)
	}
	runDir := strings.TrimSpace(runOut)
	cleanupRun := func() {
		c.exec.Run(context.WithoutCancel(ctx), "rm -rf "+ssh.ShellQuote(runDir))
	}
	archivePath := runDir + "/backup.tar.gz"
	stageDir := runDir + "/stage"
	stagedEnv := runDir + "/new.env"

	fmt.Fprintf(c.out, "Downloading %s...\n", s3Key)
	if _, err := c.exec.Run(ctx, s3.AWS(fmt.Sprintf("s3 cp %s %s", ssh.ShellQuote(s3Key), ssh.ShellQuote(archivePath)))); err != nil {
		cleanupRun()
		return fmt.Errorf("downloading from S3: %w", err)
	}

	// Before the staged tree is promoted into the volumes directory, pull
	// the backup's .env member out of it (it must land beside volumes/,
	// not inside). The old archive layout stored it under
	// deployments/<app>/.env — recognize and clear that artifact too. The
	// legacy branch used to test `-f <stage>/deployments` (a DIRECTORY, so
	// never true) and silently left the env behind in staging (audit F32).
	setAsideEnv := func(stage string) error {
		cmd := fmt.Sprintf(
			"if [ -f %s ]; then mv %s %s; elif [ -f %s ]; then mv %s %s 2>/dev/null; rm -rf %s; fi",
			ssh.ShellQuote(stage+"/.env"), ssh.ShellQuote(stage+"/.env"), ssh.ShellQuote(stagedEnv),
			ssh.ShellQuote(stage+"/deployments/"+app+"/.env"), ssh.ShellQuote(stage+"/deployments/"+app+"/.env"), ssh.ShellQuote(stagedEnv),
			ssh.ShellQuote(stage+"/deployments"),
		)
		if _, err := c.exec.Run(ctx, cmd); err != nil {
			return fmt.Errorf("setting aside backed-up .env: %w", err)
		}
		return nil
	}

	recoveryDir, err := extractToStagingThenPromote(ctx, c.exec, archivePath, stageDir, volumesDir, c.out, setAsideEnv)
	if err != nil {
		// A recovery-incomplete failure retains its artifacts INSIDE this
		// run directory (and the recovery dir beside the live tree) — the
		// broad run cleanup must not delete what the error just described
		// as kept (TCL-43).
		if !errors.Is(err, errRecoveryIncomplete) {
			cleanupRun()
		}
		return err
	}
	// The volumes promotion succeeded but is not yet committed: keep the
	// recovery directory until the .env step below also succeeds, then
	// release both together.
	dropRecovery := func() {
		if recoveryDir != "" {
			c.exec.Run(context.WithoutCancel(ctx), "rm -rf "+ssh.ShellQuote(recoveryDir))
		}
		cleanupRun()
	}

	// Install the backed-up .env (if the archive carried one — older
	// backups and volumes-only schedules don't) while keeping the previous
	// file recoverable.
	// The env commit runs as a set -eu script (audit A39): the old .env's
	// recovery copy is MANDATORY when one exists (the old `cp -p` inside an
	// if-without-chaining was skipped on failure and the destructive mv ran
	// anyway), and both files are staged as private siblings ON THE
	// DESTINATION FILESYSTEM (mktemp in the app dir) so publication is an
	// atomic same-filesystem rename with 0600 applied BEFORE the file is
	// live — not a cross-filesystem mv from /tmp chmod'd after the fact.
	envCmd := strings.Join([]string{
		"set -eu",
		fmt.Sprintf("if [ -f %s ]; then", ssh.ShellQuote(stagedEnv)),
		fmt.Sprintf("  if [ -f %s ]; then", ssh.ShellQuote(envPath)),
		`    old=$(mktemp ` + ssh.ShellQuote(appDir+"/.env-old.XXXXXXXX") + `)`,
		fmt.Sprintf(`    cat %s > "$old"`, ssh.ShellQuote(envPath)),
		`    chmod 600 "$old"`,
		fmt.Sprintf(`    mv -fT -- "$old" %s`, ssh.ShellQuote(envPath+".pre-restore")),
		"  fi",
		`  new=$(mktemp ` + ssh.ShellQuote(appDir+"/.env-new.XXXXXXXX") + `)`,
		fmt.Sprintf(`  cat %s > "$new"`, ssh.ShellQuote(stagedEnv)),
		`  chmod 600 "$new"`,
		fmt.Sprintf(`  mv -fT -- "$new" %s`, ssh.ShellQuote(envPath)),
		"  echo 'Restored app .env'",
		"fi",
	}, "\n")
	if out, err := c.exec.Run(ctx, envCmd); err != nil {
		// Env commit failed: keep the recovery dir + run dir so the mixed
		// state is manually recoverable, and say exactly that.
		return fmt.Errorf("restoring .env: %w — volume recovery copy kept at %s, restore files kept in %s", err, recoveryDir, runDir)
	} else if strings.TrimSpace(out) != "" {
		fmt.Fprint(c.out, out+"\n")
	}
	dropRecovery()
	fmt.Fprintln(c.out, "Restore complete")
	return nil
}

// extractToStagingThenPromote extracts archivePath into a fresh, isolated
// staging directory (wiped first, in case a prior interrupted restore left it
// behind), and only replaces liveDir's contents once that extraction
// succeeds. A truncated, corrupt, or otherwise malformed archive therefore
// fails without ever touching the live directory — previously `tar -xzf`
// extracted directly into it, so a failure partway through could leave a
// half-old/half-new mix of files with no way to tell which is which.
//
// The promotion itself is two DISTINCT phases with different recovery
// procedures:
//
//  1. Move the live entries aside into a sibling recovery dir. If this move
//     fails partway (some entries moved, some still live), recovery moves the
//     saved entries back WITHOUT touching the still-live ones. The previous
//     single `&&`-chained form deleted everything still in live before moving
//     the recovery dir back — a partial first move therefore destroyed the
//     only copy of the unmoved originals (audit F02).
//
//  2. Copy the staged tree in. Only at this point is every original safely in
//     the recovery dir, so a failed copy may clear the partial replacement
//     and move the originals back.
//
// The recovery directory is NOT deleted on success — it is returned so the
// caller can remove it only after every dependent step (e.g. restoring the
// backed-up .env) has also committed (audit F35). On failure it is kept and
// named in the error.
//
// This does not make restore fully atomic or safe against every hostile
// archive: extraction still runs the remote host's own `tar`, so a symlink
// planted early in the archive can still be written through by a later entry
// in the SAME tar invocation, before staging can intervene — that class of
// attack needs either a remote validation helper or downloading the archive
// locally and inspecting entries with Go's archive/tar before ever invoking
// a shell tar. What this DOES close is the truncated/corrupt-archive case,
// the "partially overwrites a live, in-use directory" failure mode, and the
// partial-move data loss.
//
// prePromote, when non-nil, runs against the extracted staging directory
// between extraction and promotion; a failure there aborts with the live
// directory untouched. RestoreVolumes uses it to lift the backed-up app
// .env out of the tree before the volumes promotion copies it in.
func extractToStagingThenPromote(ctx context.Context, exec ssh.Executor, archivePath, stageDir, liveDir string, out io.Writer, prePromote func(stageDir string) error) (string, error) {
	cleanup := func() {
		exec.Run(context.WithoutCancel(ctx), "rm -rf "+ssh.ShellQuote(stageDir)+" "+ssh.ShellQuote(archivePath))
	}

	fmt.Fprintln(out, "Extracting to staging area...")
	extractCmd := fmt.Sprintf("rm -rf %s && mkdir -p %s && tar -xzf %s -C %s",
		ssh.ShellQuote(stageDir), ssh.ShellQuote(stageDir), ssh.ShellQuote(archivePath), ssh.ShellQuote(stageDir))
	if _, err := exec.Run(ctx, extractCmd); err != nil {
		cleanup()
		return "", fmt.Errorf("extracting archive to staging (live directory untouched): %w", err)
	}

	if prePromote != nil {
		if err := prePromote(stageDir); err != nil {
			cleanup()
			return "", fmt.Errorf("preparing staged restore: %w", err)
		}
	}

	fmt.Fprintf(out, "Restoring to %s...\n", liveDir)
	if _, err := exec.Run(ctx, "mkdir -p "+ssh.ShellQuote(liveDir)); err != nil {
		return "", fmt.Errorf("preparing %s: %w", liveDir, err)
	}
	recoveryOut, err := exec.Run(ctx, "mktemp -d "+ssh.ShellQuote(liveDir)+".restore-old.XXXXXX")
	if err != nil {
		return "", fmt.Errorf("creating recovery directory for %s: %w", liveDir, err)
	}
	recoveryDir := strings.TrimSpace(recoveryOut)

	// Phase 1: move the current contents aside. liveDir itself is never
	// removed or moved, which matters when it's a docker bind-mount target.
	moveCmd := fmt.Sprintf("find %s -mindepth 1 -maxdepth 1 -exec mv -t %s -- {} +",
		ssh.ShellQuote(liveDir), ssh.ShellQuote(recoveryDir))
	if _, err := exec.Run(ctx, moveCmd); err != nil {
		// PARTIAL move possible: recovery holds some originals, liveDir may
		// still hold the rest. NEVER delete anything still in liveDir here —
		// just move the saved entries back. Names cannot collide (each entry
		// is either moved or not), so a plain move-back is lossless.
		moveBack := fmt.Sprintf("find %s -mindepth 1 -maxdepth 1 -exec mv -t %s -- {} +",
			ssh.ShellQuote(recoveryDir), ssh.ShellQuote(liveDir))
		if _, rbErr := exec.Run(context.WithoutCancel(ctx), moveBack); rbErr != nil {
			return recoveryDir, fmt.Errorf("%w: moving current contents aside for %s: %v — original entries preserved split across %s and %s, staged restore kept in %s",
				errRecoveryIncomplete, liveDir, err, liveDir, recoveryDir, stageDir)
		}
		cleanup()
		exec.Run(context.WithoutCancel(ctx), "rm -rf "+ssh.ShellQuote(recoveryDir))
		return "", fmt.Errorf("moving current contents aside for %s: %w — previous contents restored, live directory untouched",
			liveDir, err)
	}

	// Phase 2: copy the staged contents in. Every original is now safely in
	// the recovery dir, so a failed copy may clear the partial replacement
	// and restore the originals.
	copyCmd := fmt.Sprintf("cp -a %s/. %s/", ssh.ShellQuote(stageDir), ssh.ShellQuote(liveDir))
	if _, err := exec.Run(ctx, copyCmd); err != nil {
		rollbackCmd := fmt.Sprintf("find %s -mindepth 1 -delete && find %s -mindepth 1 -maxdepth 1 -exec mv -t %s -- {} +",
			ssh.ShellQuote(liveDir), ssh.ShellQuote(recoveryDir), ssh.ShellQuote(liveDir))
		if _, rbErr := exec.Run(context.WithoutCancel(ctx), rollbackCmd); rbErr != nil {
			return recoveryDir, fmt.Errorf("%w: promoting staged restore into %s: %v — rollback failed too; previous contents kept in %s, staged restore kept in %s",
				errRecoveryIncomplete, liveDir, err, recoveryDir, stageDir)
		}
		cleanup()
		exec.Run(context.WithoutCancel(ctx), "rm -rf "+ssh.ShellQuote(recoveryDir))
		return "", fmt.Errorf("promoting staged restore into %s: %w — previous contents restored, staged restore discarded",
			liveDir, err)
	}

	cleanup()
	return recoveryDir, nil
}

// ListBackups lists available backups from S3.
func (c *Client) ListBackups(ctx context.Context, app, prefix string, s3 S3Config) ([]string, error) {
	if err := c.ensureAWSCLI(ctx); err != nil {
		return nil, err
	}

	s3Path := fmt.Sprintf("s3://%s/%s/%s/", s3.Bucket, app, prefix)
	output, err := c.exec.Run(ctx, s3.AWS(fmt.Sprintf("s3 ls %s", ssh.ShellQuote(s3Path))))
	if err != nil {
		return nil, fmt.Errorf("listing backups: %w", err)
	}

	var backups []string
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		backups = append(backups, line)
	}
	return backups, nil
}

// AccessoryBackup performs a database-aware backup for an accessory.
//
// All intermediate artifacts (dumps, snapshots, scratch tars) are built
// inside one private 0700 mktemp workspace and removed on every exit — the
// old predictable /tmp/<app>-… paths were shared across accessories of the
// same app and exposed database dumps to other local users (audit F34).
func (c *Client) AccessoryBackup(ctx context.Context, app, name, image string, env map[string]string, s3 S3Config) error {
	if err := c.ensureAWSCLI(ctx); err != nil {
		return err
	}

	timestamp, err := newBackupID(time.Now())
	if err != nil {
		return err
	}
	containerName := app + "-" + name
	qContainer := ssh.ShellQuote(containerName)

	runOut, err := c.exec.Run(ctx, "umask 077; mktemp -d /tmp/teploy-backup.XXXXXXXX")
	if err != nil {
		return fmt.Errorf("creating backup workspace: %w", err)
	}
	workDir := strings.TrimSpace(runOut)
	cleanup := func() {
		c.exec.Run(context.WithoutCancel(ctx), "rm -rf "+ssh.ShellQuote(workDir))
	}

	dumpPath := workDir + "/dump.out.gz"
	// dumpTmp is redirected into with `>`, not piped into gzip: a shell
	// pipeline's exit status is its LAST command's (gzip, which "succeeds"
	// compressing an empty stream even when pg_dump/mysqldump errored to
	// stderr) — `| gzip > path` would silently swallow a real dump
	// failure. Confirmed live: a wrong db name (see postgresDBAndUser)
	// produced a 20-byte gzip of nothing while the old `| gzip` version of
	// this command reported "Backup complete". Redirecting to a plain
	// file with `>` preserves the dump command's own exit code, which
	// c.exec.Run already surfaces (with captured stderr) as a real error.
	dumpTmp := workDir + "/dump.out"
	var dumpCmd string
	s3Key := ""
	switch {
	case isDBType(image, "postgres"):
		db, user := postgresDBAndUser(app, env)
		s3Key = fmt.Sprintf("s3://%s/%s/accessories/%s/%s.sql.gz", s3.Bucket, app, name, timestamp)
		dumpCmd = fmt.Sprintf("docker exec %s pg_dump -U %s %s > %s && gzip -c %s > %s",
			qContainer, ssh.ShellQuote(user), ssh.ShellQuote(db), ssh.ShellQuote(dumpTmp), ssh.ShellQuote(dumpTmp), ssh.ShellQuote(dumpPath))
	case isDBType(image, "mysql"), isDBType(image, "mariadb"):
		db := mysqlDB(app, env)
		s3Key = fmt.Sprintf("s3://%s/%s/accessories/%s/%s.sql.gz", s3.Bucket, app, name, timestamp)
		// Root password via a 0600 env-file consumed by `docker exec
		// --env-file`, never on any argv. The old `-e MYSQL_PWD=<pwd>`
		// form kept the secret off the mysqldump argv but left it on the
		// DOCKER EXEC argv — visible in the host's `ps` for the life of
		// the dump (C08). The file lives in the 0700 backup workspace
		// and dies with it on every path (cleanup removes the workspace
		// on failure and success alike). Absent = current behavior
		// (passwordless root).
		execEnv := ""
		if pwd := mysqlRootPassword(env); pwd != "" {
			envFile := workDir + "/mysql.env"
			if err := c.exec.Upload(ctx, strings.NewReader("MYSQL_PWD="+pwd+"\n"), envFile, "0600"); err != nil {
				cleanup()
				return fmt.Errorf("staging the mysql credential file for %s (no dump was run): %w", name, err)
			}
			execEnv = " --env-file " + ssh.ShellQuote(envFile)
		}
		dumpCmd = fmt.Sprintf("docker exec%s %s mysqldump -u root %s > %s && gzip -c %s > %s",
			execEnv, qContainer, ssh.ShellQuote(db), ssh.ShellQuote(dumpTmp), ssh.ShellQuote(dumpTmp), ssh.ShellQuote(dumpPath))
	case isDBType(image, "mongo"):
		s3Key = fmt.Sprintf("s3://%s/%s/accessories/%s/%s.archive.gz", s3.Bucket, app, name, timestamp)
		dumpCmd = fmt.Sprintf("docker exec %s mongodump --archive --gzip > %s", qContainer, ssh.ShellQuote(dumpPath))
	case isDBType(image, "redis"):
		s3Key = fmt.Sprintf("s3://%s/%s/accessories/%s/%s.rdb.gz", s3.Bucket, app, name, timestamp)
		// Redis: trigger bgsave, wait for an ACKNOWLEDGED new save, then
		// copy dump.rdb. The old one-liner polled LASTSAVE in a loop whose
		// exhaustion still exited 0 (the last `sleep` won), so docker cp
		// ran with the PREVIOUS dump and uploaded it as a fresh backup
		// (audit F36). This script fails closed: BGSAVE refusal (other
		// than an already-running save, whose completion still moves
		// LASTSAVE), a poll timeout, or a failed copy all abort before
		// anything is uploaded.
		redisTmp := workDir + "/dump.rdb"
		dumpCmd = strings.Join([]string{
			"set -eu",
			// AOF-enabled Redis persists to the append-only file; backing
			// up only dump.rdb captures a stale or empty dataset. Fail
			// closed rather than uploading a wrong-point-in-time artifact
			// (TCL-42).
			// AOF gate (A40): the old `config get appendonly | tail -n 1`
			// pipeline masked a failed docker exec (empty output fell
			// through as "not yes") and only refused on a substring
			// match. The reply must be a proven `appendonly no` —
			// anything else (auth error, empty, unexpected) refuses.
			fmt.Sprintf("aof=$(docker exec %s redis-cli --raw config get appendonly)", qContainer),
			`set -- $aof`,
			`if [ "${1:-}" != appendonly ] || [ "${2:-}" != no ]; then echo 'cannot confirm redis appendonly=no (got: '"$aof"') — teploy backup captures dump.rdb only; refusing' >&2; exit 1; fi`,
			fmt.Sprintf("ls=$(docker exec %s redis-cli lastsave)", qContainer),
			fmt.Sprintf("bgs=$(docker exec %s redis-cli bgsave 2>&1) || true", qContainer),
			`case "$bgs" in *ERR*) case "$bgs" in *"in progress"*) ;; *) printf 'redis BGSAVE failed: %s\n' "$bgs" >&2; exit 1;; esac;; esac`,
			"saved=no; i=0",
			`while [ "$i" -lt 60 ]; do`,
			fmt.Sprintf("cur=$(docker exec %s redis-cli lastsave)", qContainer),
			`if [ "$cur" != "$ls" ]; then saved=yes; break; fi`,
			"sleep 1; i=$((i+1))",
			"done",
			`if [ "$saved" != yes ]; then echo 'timed out waiting for Redis BGSAVE to complete' >&2; exit 1; fi`,
			fmt.Sprintf("docker cp %s:/data/dump.rdb %s", qContainer, ssh.ShellQuote(redisTmp)),
			fmt.Sprintf("gzip -c %s > %s", ssh.ShellQuote(redisTmp), ssh.ShellQuote(dumpPath)),
			fmt.Sprintf("rm -f %s", ssh.ShellQuote(redisTmp)),
		}, "\n")
	default:
		// Generic: tar the volume directory. For a LIVE engine (nucleus and
		// anything else that mutates its files during the read) this is a
		// crash-consistent snapshot: GNU tar exits 1 with "file changed" /
		// "file shrank" warnings when a WAL rotates or checkpoints mid-read.
		// That shape is exactly what crash recovery is built for (torn-tail
		// truncation + CRC skip), so tolerate exit 1 — real failures
		// (unreadable dir, ENOSPC) exit 2. `accessory verify-backup` is the
		// correctness gate: it boots the archive in a scratch container.
		accDir := fmt.Sprintf("%s/%s/accessories/%s", deploymentsDir, app, name)
		dumpPath = workDir + "/dump.tar.gz"
		s3Key = fmt.Sprintf("s3://%s/%s/accessories/%s/%s.tar.gz", s3.Bucket, app, name, timestamp)
		dumpCmd = fmt.Sprintf("tar -czf %s -C %s . || [ $? -eq 1 ]", ssh.ShellQuote(dumpPath), ssh.ShellQuote(accDir))
	}

	fmt.Fprintf(c.out, "Backing up %s...\n", containerName)
	if _, err := c.exec.Run(ctx, dumpCmd); err != nil {
		// Never leave partial backups around: they're disk-fillers at best,
		// restore-bait at worst.
		cleanup()
		return fmt.Errorf("dumping %s: %w", name, err)
	}

	fmt.Fprintf(c.out, "Uploading to %s...\n", s3Key)
	if _, err := c.exec.Run(ctx, s3.AWS(fmt.Sprintf("s3 cp %s %s", ssh.ShellQuote(dumpPath), ssh.ShellQuote(s3Key)))); err != nil {
		cleanup()
		return fmt.Errorf("uploading to S3: %w", err)
	}

	cleanup()
	fmt.Fprintf(c.out, "Backup complete: %s\n", s3Key)
	return nil
}

// AccessoryRestore restores a database backup from S3.
func (c *Client) AccessoryRestore(ctx context.Context, app, name, image, date string, env map[string]string, s3 S3Config) error {
	if err := c.ensureAWSCLI(ctx); err != nil {
		return err
	}

	containerName := app + "-" + name
	qContainer := ssh.ShellQuote(containerName)

	// All restore scratch files live under one per-invocation temp dir. The
	// old fixed names (/tmp/restore.sql.gz, …, and the generic branch's
	// /tmp/<app>-<name>-restore-stage) were shared by every restore: two
	// concurrent restores (or a crash mid-restore followed by a retry)
	// could read or stage the previous run's files. On failure the dir is
	// kept and named in the error for inspection; on success it is removed.
	tmpOut, err := c.exec.Run(ctx, "mktemp -d "+ssh.ShellQuote("/tmp/teploy-restore.XXXXXX"))
	if err != nil {
		return fmt.Errorf("creating restore temp dir: %w", err)
	}
	tmpdir := strings.TrimSpace(tmpOut)
	keepTmp := func(err error) error {
		return fmt.Errorf("%w — restore files kept in %s", err, tmpdir)
	}

	// Determine file type and restore command based on DB type. The generic
	// (tar) branch leaves restoreCmd empty and instead sets accDir, since it
	// goes through the staging helper below rather than a single shell command.
	var s3Key, restorePath, restoreCmd, accDir string

	switch {
	case isDBType(image, "postgres"):
		db, user := postgresDBAndUser(app, env)
		s3Key = fmt.Sprintf("s3://%s/%s/accessories/%s/%s.sql.gz", s3.Bucket, app, name, date)
		restorePath = tmpdir + "/restore.sql.gz"
		// Decompress to a file first and feed psql via stdin redirect: a
		// pipeline reports only the LAST command's status, so `gunzip | psql`
		// succeeded on a corrupt archive (empty stdin) and — without
		// ON_ERROR_STOP — on SQL errors too. Same convention as verify.go.
		sqlPath := tmpdir + "/restore.sql"
		restoreCmd = fmt.Sprintf("gunzip -c %s > %s && docker exec -i %s psql -v ON_ERROR_STOP=1 -U %s %s < %s",
			ssh.ShellQuote(restorePath), ssh.ShellQuote(sqlPath), qContainer, ssh.ShellQuote(user), ssh.ShellQuote(db), ssh.ShellQuote(sqlPath))
	case isDBType(image, "mysql"), isDBType(image, "mariadb"):
		db := mysqlDB(app, env)
		// The old branch never assigned s3Key/restorePath, so the download
		// and gunzip below operated on empty arguments — every MySQL/MariaDB
		// restore was deterministically broken (audit F31).
		s3Key = fmt.Sprintf("s3://%s/%s/accessories/%s/%s.sql.gz", s3.Bucket, app, name, date)
		restorePath = tmpdir + "/restore.sql.gz"
		// Same 0600 env-file credential transport as the backup path
		// (see there) — the password rides in no argv. The restore
		// failure path DELIBERATELY keeps tmpdir for inspection, so the
		// credential file must be removed by name there: keep the SQL,
		// never the secret (C08).
		execEnv := ""
		if pwd := mysqlRootPassword(env); pwd != "" {
			envFile := tmpdir + "/mysql.env"
			if err := c.exec.Upload(ctx, strings.NewReader("MYSQL_PWD="+pwd+"\n"), envFile, "0600"); err != nil {
				return keepTmp(fmt.Errorf("staging the mysql credential file for %s: %w", name, err))
			}
			execEnv = " --env-file " + ssh.ShellQuote(envFile)
		}
		// Same pipeline-to-redirect shape as postgres (mysql itself exits
		// nonzero on SQL errors when reading a script, but gunzip's failure
		// must not be masked either).
		sqlPath := tmpdir + "/restore.sql"
		restoreCmd = fmt.Sprintf("gunzip -c %s > %s && docker exec -i%s %s mysql -u root %s < %s",
			ssh.ShellQuote(restorePath), ssh.ShellQuote(sqlPath), execEnv, qContainer, ssh.ShellQuote(db), ssh.ShellQuote(sqlPath))
		// The kept-on-failure scratch dir must never keep the credential.
		innerKeep := keepTmp
		keepTmp = func(err error) error {
			c.exec.Run(context.WithoutCancel(ctx), "rm -f "+ssh.ShellQuote(tmpdir+"/mysql.env"))
			return innerKeep(err)
		}
	case isDBType(image, "mongo"):
		s3Key = fmt.Sprintf("s3://%s/%s/accessories/%s/%s.archive.gz", s3.Bucket, app, name, date)
		restorePath = tmpdir + "/restore.archive.gz"
		restoreCmd = fmt.Sprintf("docker exec -i %s mongorestore --archive --gzip --drop < %s", qContainer, ssh.ShellQuote(restorePath))
	case isDBType(image, "redis"):
		// AccessoryBackup stores redis as <date>.rdb.gz; without this case the
		// default branch looked for a .tar.gz that doesn't exist, so redis
		// restores always failed. Stop redis first so its shutdown save can't
		// overwrite the snapshot we copy in, then start so it loads dump.rdb.
		//
		// The old `gunzip && stop && cp && start` chain could leave the
		// accessory STOPPED forever when docker cp failed after a successful
		// stop (audit F38). This script decompresses FIRST (no downtime while
		// validating the artifact), saves the current dump, and restores +
		// restarts the original on any failure after the stop.
		s3Key = fmt.Sprintf("s3://%s/%s/accessories/%s/%s.rdb.gz", s3.Bucket, app, name, date)
		restorePath = tmpdir + "/restore.rdb.gz"
		rdbPath := tmpdir + "/restore.rdb"
		oldRdb := tmpdir + "/old-dump.rdb"
		// A40: the AOF gate is a Go-level preflight so a failed or
		// unexpected reply (auth error, empty output, anything but a
		// proven `appendonly no`) refuses BEFORE any stop or copy — the
		// old `config get appendonly | tail -n 1` pipeline masked a failed
		// docker exec as "not yes" and fell through to the destructive
		// replacement. ("Not proven yes" is not "proven no".)
		aofOut, aofErr := c.exec.Run(ctx, fmt.Sprintf("docker exec %s redis-cli --raw config get appendonly", qContainer))
		if aofErr != nil {
			return keepTmp(fmt.Errorf("cannot establish the Redis persistence mode for %s: %w", containerName, aofErr))
		}
		if aofFields := strings.Fields(aofOut); len(aofFields) != 2 || aofFields[0] != "appendonly" || aofFields[1] != "no" {
			return keepTmp(fmt.Errorf("cannot confirm appendonly=no for %s (got %q) — an AOF-enabled Redis would load the append-only file on restart and teploy's dump.rdb restore would be a no-op; an explicit restore plan is required", containerName, strings.TrimSpace(aofOut)))
		}
		// A41 ordering + T37/T38 arming: restore_original is defined (and
		// the old-dump capture attempted) AFTER the stop — a graceful redis
		// shutdown writes a final RDB, so the pre-stop existence flag could
		// miss data present at shutdown. The baseline copy itself
		// distinguishes "no such file" (nothing to preserve) from every
		// other failure, and ANY failure after the stop restarts the
		// container before aborting: the old script's `set -e` exit on a
		// failed docker cp left Redis stopped with no recovery attempt.
		restoreCmd = strings.Join([]string{
			"set -eu",
			fmt.Sprintf("gunzip -c %s > %s", ssh.ShellQuote(restorePath), ssh.ShellQuote(rdbPath)),
			"had=no",
			fmt.Sprintf(`restore_original() { if [ "$had" = yes ] && [ -f %s ]; then docker cp %s %s:/data/dump.rdb || true; fi; docker start %s || true; }`,
				ssh.ShellQuote(oldRdb), ssh.ShellQuote(oldRdb), qContainer, qContainer),
			fmt.Sprintf("docker stop %s", qContainer),
			// Post-stop baseline (docker cp works on a stopped container):
			// success -> had=yes; a proven not-found -> nothing to
			// preserve; anything else -> restart + abort.
			`cperr=$(mktemp)`,
			fmt.Sprintf(`if docker cp %s:/data/dump.rdb %s 2>"$cperr"; then had=yes; elif grep -qi 'no such' "$cperr"; then had=no; else cat "$cperr" >&2; rm -f "$cperr"; restore_original; echo 'capturing the pre-restore dump failed; the container was restarted' >&2; exit 1; fi`,
				qContainer, ssh.ShellQuote(oldRdb)),
			`rm -f "$cperr"`,
			"ok=yes",
			fmt.Sprintf("docker cp %s %s:/data/dump.rdb || ok=no", ssh.ShellQuote(rdbPath), qContainer),
			`if [ "$ok" != yes ]; then`,
			`  restore_original`,
			"  echo 'redis restore failed after stopping the container; the original dump was restored when available' >&2",
			"  exit 1",
			"fi",
			fmt.Sprintf("if ! docker start %s; then", qContainer),
			`  restore_original`,
			"  echo 'redis container failed to start after the restore; the original dump was put back — verify the accessory' >&2",
			"  exit 1",
			"fi",
			fmt.Sprintf("rm -f %s", ssh.ShellQuote(rdbPath)),
		}, "\n")
	default:
		// Generic: extract tar to accessory directory.
		s3Key = fmt.Sprintf("s3://%s/%s/accessories/%s/%s.tar.gz", s3.Bucket, app, name, date)
		restorePath = tmpdir + "/restore.tar.gz"
		accDir = fmt.Sprintf("%s/%s/accessories/%s", deploymentsDir, app, name)
	}

	// Every engine branch must produce a complete artifact specification —
	// an empty s3Key/restorePath means a branch forgot its convention and
	// would otherwise download from/gunzip an empty string (the class of
	// bug F31 fixed for MySQL).
	if s3Key == "" || restorePath == "" {
		return keepTmp(fmt.Errorf("internal error: incomplete backup artifact specification for %s (image %s)", name, image))
	}

	fmt.Fprintf(c.out, "Downloading %s...\n", s3Key)
	if _, err := c.exec.Run(ctx, s3.AWS(fmt.Sprintf("s3 cp %s %s", ssh.ShellQuote(s3Key), ssh.ShellQuote(restorePath)))); err != nil {
		return keepTmp(fmt.Errorf("downloading backup: %w", err))
	}

	fmt.Fprintf(c.out, "Restoring %s...\n", name)
	if accDir != "" {
		stageDir := tmpdir + "/restore-stage"
		recoveryDir, err := extractToStagingThenPromote(ctx, c.exec, restorePath, stageDir, accDir, c.out, nil)
		if err != nil {
			// Recovery-incomplete failures retain their artifacts in
			// tmpdir/stage — keepTmp's contract already preserves tmpdir,
			// so the retained paths stay inspectable (TCL-43).
			return keepTmp(fmt.Errorf("restoring %s: %w", name, err))
		}
		// No dependent steps follow a generic accessory restore, so the
		// retained recovery directory can be released immediately.
		if recoveryDir != "" {
			c.exec.Run(context.WithoutCancel(ctx), "rm -rf "+ssh.ShellQuote(recoveryDir))
		}
	} else if _, err := c.exec.Run(ctx, restoreCmd); err != nil {
		return keepTmp(fmt.Errorf("restoring %s: %w", name, err))
	}

	c.exec.Run(ctx, "rm -rf "+ssh.ShellQuote(tmpdir))
	fmt.Fprintln(c.out, "Restore complete")
	return nil
}

// SetSchedule creates (or replaces) a cron job for scheduled backups. marker is
// a stable per-target tag (e.g. "teploy-backup:<app>") appended as a trailing
// comment so the entry can be found and replaced on reschedule.
func (c *Client) SetSchedule(ctx context.Context, schedule, command, marker string) error {
	// The sink enforces the grammar itself (A46): direct callers used to be
	// able to bypass ValidateSchedule, and a command or marker containing a
	// line break would silently install multiple unintended crontab lines.
	if err := ValidateSchedule(schedule); err != nil {
		return err
	}
	if strings.ContainsAny(command+marker, "\r\n\x00") {
		return fmt.Errorf("cron command and marker must each be a single line")
	}
	// Dedup on the marker with grep -vF (fixed string). The previous grep -v
	// matched the whole COMMAND as a regex — backup commands are full of regex
	// metacharacters (. * $ ( ) /), so the dedup matched the wrong lines or none
	// at all, leaving duplicate cron entries piling up on every reschedule.
	// Single-quoting via ShellQuote keeps the command's literal $(date …) intact
	// (cron evaluates it at run time).
	// Bracket the marker so the fixed-string dedup can't collide across apps
	// whose names are prefixes of one another. grep -vF is an UNanchored
	// substring match, so a bare marker `teploy-backup:web` would also match
	// (and wrongly delete) the line for `teploy-backup:web-staging`. The
	// brackets make the token uniquely terminable: `[teploy-backup:web]` is not
	// a substring of `[teploy-backup:web-staging]`.
	tag := "[" + marker + "]"
	// cron treats the first unescaped % in the command as the start of the
	// job's stdin (effectively a newline): a command containing
	// `$(date +%Y%m%d-…)` was truncated at the first % and never ran past
	// it. Escape every % in the COMMAND portion — the schedule and the
	// marker are % free — so cron passes them through literally.
	line := fmt.Sprintf("%s %s # %s", schedule, strings.ReplaceAll(command, "%", "\\%"), tag)
	// The current crontab is read with its exit status CHECKED (TCL-46):
	// the old `crontab -l 2>/dev/null | grep -vF …` pipeline masked any
	// real read failure as empty input and then installed only the new
	// line — silently deleting every unrelated job. Only the canonical
	// "no crontab for <user>" failure means "start from empty"; every
	// other failure aborts without invoking the install.
	cmd := fmt.Sprintf(
		`raw=$(crontab -l 2>&1); rc=$?; `+
			`if [ "$rc" -ne 0 ]; then case "$raw" in *"no crontab"*) raw="";; *) `+
			`echo "reading crontab failed: $raw" >&2; exit 1;; esac; fi; `+
			`(printf '%%s\n' "$raw" | grep -vF %s; printf '%%s\n' %s) | crontab -`,
		ssh.ShellQuote(tag), ssh.ShellQuote(line),
	)
	if _, err := c.exec.Run(ctx, cmd); err != nil {
		return fmt.Errorf("setting cron schedule: %w", err)
	}
	return nil
}

func (c *Client) ensureAWSCLI(ctx context.Context) error {
	if _, err := c.exec.Run(ctx, "which aws"); err != nil {
		return fmt.Errorf("aws CLI not found on server — install with: apt install awscli")
	}
	return nil
}

// isDBType reports whether image refers to the given database engine. The
// repository's final path component is compared, with tag/digest and
// registry host stripped — including a registry port: its colon sits
// BEFORE the last slash, so treating the first colon as the tag separator
// turned "registry.example:5000/postgres:16" into "registry.example" and
// silently routed real databases through the generic tar backup/restore
// branch.
func isDBType(image, dbType string) bool {
	return imageName(image) == dbType
}

// imageName returns the repository name of an image reference:
// "postgres:16" → "postgres", "library/postgres" → "postgres",
// "registry.example:5000/postgres:16" → "postgres",
// "postgres@sha256:..." → "postgres".
func imageName(image string) string {
	if i := strings.Index(image, "@"); i >= 0 {
		image = image[:i]
	}
	lastSlash := strings.LastIndex(image, "/")
	lastColon := strings.LastIndex(image, ":")
	if lastColon > lastSlash {
		image = image[:lastColon]
	}
	if i := strings.LastIndex(image, "/"); i >= 0 {
		image = image[i+1:]
	}
	return image
}

// postgresDBAndUser resolves the actual database name and superuser a
// postgres accessory is running with, mirroring
// internal/accessories.connectionEnvVars' exact fallback logic (POSTGRES_DB
// / POSTGRES_USER, defaulting to the app name / "postgres"). Backup/restore
// previously hardcoded db=app unconditionally — silently wrong (and
// silently UNDETECTED, since `pg_dump | gzip` masked the resulting error —
// see AccessoryBackup) for any accessory setting a custom POSTGRES_DB.
func postgresDBAndUser(app string, env map[string]string) (db, user string) {
	db = env["POSTGRES_DB"]
	if db == "" {
		db = app
	}
	user = env["POSTGRES_USER"]
	if user == "" {
		user = "postgres"
	}
	return db, user
}

// mysqlDB mirrors connectionEnvVars' MYSQL_DATABASE fallback (see
// postgresDBAndUser). mysql/mariadb backup/restore always uses the "root"
// user, matching connectionEnvVars' own hardcoded assumption there — no
// equivalent MYSQL_USER override exists to resolve.
func mysqlDB(app string, env map[string]string) string {
	db := env["MYSQL_DATABASE"]
	if db == "" {
		db = app
	}
	return db
}

// mysqlRootPassword resolves the root password the mysql/mariadb containers
// themselves honor (MYSQL_ROOT_PASSWORD, falling back to MYSQL_PASSWORD).
// Used to stage MYSQL_PWD in a 0600 env-file consumed by `docker exec
// --env-file` — never on any argv, which would expose the password in
// the host's `ps` output. Empty means no password configured; callers
// keep the bare command.
func mysqlRootPassword(env map[string]string) string {
	if pwd := env["MYSQL_ROOT_PASSWORD"]; pwd != "" {
		return pwd
	}
	return env["MYSQL_PASSWORD"]
}
