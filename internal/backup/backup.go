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

	"github.com/useteploy/teploy/internal/confighome"
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

// S3Config holds S3 bucket and credentials info.
//
// Endpoint (optional) points at an S3-compatible server instead of AWS —
// e.g. a self-hosted MinIO accessory (`http://<app>-minio:9000`) or
// B2/R2/Bunny. AccessKey/SecretKey (optional) are passed inline to the aws
// CLI call as env vars for that command only, never written to disk on the
// server. StoredTarget (optional) names the server's backup-target file
// (D3): when set, the encrypted credentials it carries are decrypted and
// exported inside the invocation itself and Prefix is part of the key
// layout (s3://<bucket>/<prefix>/<app>/...).
type S3Config struct {
	Bucket    string
	Region    string
	Endpoint  string
	AccessKey string
	SecretKey string
	// Prefix is an optional key-layout prefix under the bucket.
	Prefix string
	// StoredTarget is the server path of backup-target.json whose encrypted
	// credentials should back this config (set when it was resolved from
	// the server's stored target rather than flags).
	StoredTarget string
}

// Base is the bucket-root-relative key prefix every backup key is built
// from: the bare bucket, or bucket/prefix when a prefix is configured.
func (s3 S3Config) Base() string {
	if s3.Prefix == "" {
		return s3.Bucket
	}
	return s3.Bucket + "/" + s3.Prefix
}

// AWS builds an aws-CLI invocation for this config. Inline credential env
// (if set) or the stored-target decrypt prelude comes first, then the
// subcommand, then --region and --endpoint-url as configured.
func (s3 S3Config) AWS(args string) string {
	var b strings.Builder
	awsCmd := func() string {
		s := "aws " + args + " --region " + ssh.ShellQuote(s3.Region)
		if s3.Endpoint != "" {
			s += " --endpoint-url " + ssh.ShellQuote(s3.Endpoint)
		}
		return s
	}
	switch {
	case s3.AccessKey != "":
		if s3.AccessKey != "" {
			b.WriteString("AWS_ACCESS_KEY_ID=" + ssh.ShellQuote(s3.AccessKey) + " ")
		}
		if s3.SecretKey != "" {
			b.WriteString("AWS_SECRET_ACCESS_KEY=" + ssh.ShellQuote(s3.SecretKey) + " ")
		}
		b.WriteString(awsCmd())
	case s3.StoredTarget != "":
		b.WriteString(StoredCredsPrefix(s3.StoredTarget))
		b.WriteString(awsCmd())
		b.WriteString(" )")
	default:
		b.WriteString(awsCmd())
	}
	return b.String()
}

// Client performs backup and restore operations on a remote server via SSH.
type Client struct {
	exec ssh.Executor
	out  io.Writer
	// retryDelay spaces retries of a postgres rename refused because
	// sessions were still connected (0 = one second; tests shorten it).
	retryDelay time.Duration
	// loadDeadline overrides the deadlines on dump loads and their cleanup
	// (0 = scaled to the dump; tests shorten it).
	loadDeadline time.Duration
}

// NewClient creates a new backup client.
func NewClient(exec ssh.Executor, out io.Writer) *Client {
	return &Client{exec: exec, out: out}
}

// BackupResult describes one backup as created (D3's backup-json-v1 rows).
type BackupResult struct {
	ID        string
	Key       string
	SizeBytes int64
	// TeployKey is the sibling server-config object for volume backups,
	// empty when the app has no .teploy directory (or for accessories).
	TeployKey string
	// Engine is the detected accessory engine ("volumes" for an app
	// volume archive); Consistency is the guarantee the artifact can
	// honestly claim (engine-consistent, engine-snapshot, crash-consistent).
	Engine      string
	Consistency string
	DurationMs  int64
	// UncoveredDatabases are databases in the postgres cluster this backup
	// does not contain; Warnings are non-fatal coverage problems.
	UncoveredDatabases []string
	Warnings           []string
}

// BackupVolumes archives the app's volumes (and .env) to S3, reporting the
// created backup's identity and size. The .env rides inside the archive as
// a top-level member which restore puts back beside the volumes directory;
// the archive is built inside a private 0700 mktemp workspace (audit F34).
func (c *Client) BackupVolumes(ctx context.Context, app string, s3 S3Config) (res BackupResult, err error) {
	start := time.Now()
	res = BackupResult{Engine: "volumes", Consistency: consistencyCrash}
	defer func() { res.DurationMs = time.Since(start).Milliseconds() }()
	if err := c.ensureAWSCLI(ctx); err != nil {
		return res, err
	}

	timestamp, err := newBackupID(time.Now())
	if err != nil {
		return res, err
	}
	res.ID = timestamp
	volumesDir := fmt.Sprintf("%s/%s/volumes", deploymentsDir, app)
	appDir := fmt.Sprintf("%s/%s", deploymentsDir, app)

	runOut, err := c.exec.Run(ctx, "umask 077; mktemp -d /tmp/teploy-backup.XXXXXXXX")
	if err != nil {
		return res, fmt.Errorf("creating backup workspace: %w", err)
	}
	workDir := strings.TrimSpace(runOut)
	cleanup := func() {
		c.exec.Run(context.WithoutCancel(ctx), "rm -rf "+ssh.ShellQuote(workDir))
	}
	archivePath := workDir + "/backup.tar.gz"
	s3Key := fmt.Sprintf("s3://%s/%s/volumes/%s.tar.gz", s3.Base(), app, timestamp)
	res.Key = s3Key

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
		return res, fmt.Errorf("creating archive: %w", err)
	}
	if size, err := c.remoteSize(ctx, archivePath); err == nil {
		res.SizeBytes = size
	}

	// Upload to S3.
	fmt.Fprintf(c.out, "Uploading to %s...\n", s3Key)
	uploadCmd := s3.AWS(fmt.Sprintf("s3 cp %s %s", ssh.ShellQuote(archivePath), ssh.ShellQuote(s3Key)))
	if _, err := c.exec.Run(ctx, uploadCmd); err != nil {
		cleanup()
		return res, fmt.Errorf("uploading to S3: %w", err)
	}

	// The app's server data (Teploy config repository and event history)
	// travels as a sibling object of the same timestamp, never inside the
	// volume archive, whose root is the volumes directory.
	teployDir := confighome.Dir(app)
	if present, err := c.remoteExists(ctx, teployDir); err != nil {
		cleanup()
		return res, fmt.Errorf("checking %s: %w", teployDir, err)
	} else if present {
		teployArchive := workDir + "/teploy.tar.gz"
		teployKey := fmt.Sprintf("s3://%s/%s/teploy/%s.tar.gz", s3.Base(), app, timestamp)
		res.TeployKey = teployKey
		tarCmd := fmt.Sprintf("tar -czf %s --exclude=./.write.lock --exclude=./events.lock .", ssh.ShellQuote(teployArchive))
		if _, err := c.exec.Run(ctx, fmt.Sprintf("cd %s && if [ -d config.git ] && command -v flock >/dev/null 2>&1; then flock .write.lock %s; else %s; fi", ssh.ShellQuote(teployDir), tarCmd, tarCmd)); err != nil {
			cleanup()
			return res, fmt.Errorf("archiving %s: %w", teployDir, err)
		}
		fmt.Fprintf(c.out, "Uploading Teploy config and history to %s...\n", teployKey)
		if _, err := c.exec.Run(ctx, s3.AWS(fmt.Sprintf("s3 cp %s %s", ssh.ShellQuote(teployArchive), ssh.ShellQuote(teployKey)))); err != nil {
			cleanup()
			return res, fmt.Errorf("uploading Teploy config to S3: %w", err)
		}
	}

	cleanup()
	fmt.Fprintf(c.out, "Backup complete: %s\n", s3Key)
	return res, nil
}

// remoteSize stats a file on the server. A stat failure is not fatal to the
// backup — the size is reporting, not correctness.
func (c *Client) remoteSize(ctx context.Context, path string) (int64, error) {
	out, err := c.exec.Run(ctx, "stat -c %s "+ssh.ShellQuote(path))
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(out), 10, 64)
}

// RestoreVolumes downloads and extracts a volume backup from S3. If the
// archive carries the app-level .env (see BackupVolumes), it is restored
// beside the volumes directory in the app directory — the pre-restore file
// is kept as .env.pre-restore so overwriting credentials is recoverable.
func (c *Client) RestoreVolumes(ctx context.Context, app, date string, s3 S3Config) error {
	recoveryDir, err := c.RestoreVolumesKeep(ctx, app, date, s3, "")
	if err == nil && recoveryDir != "" {
		c.exec.Run(context.WithoutCancel(ctx), "rm -rf "+ssh.ShellQuote(recoveryDir))
	}
	return err
}

// RestoreVolumesKeep is RestoreVolumes where the caller owns the recovery
// directory: on success it is returned (empty when none was created) instead
// of being deleted, so a caller with post-restore steps of its own (D3's
// quiesced restore health check) can keep the pre-restore copy until those
// steps also pass. quiesced, when non-empty, names the containers that were
// stopped for this restore — recorded into failure messages so the operator
// knows what is down.
//
// Every scratch path (download, staging, staged env) lives under one private
// mktemp run directory: the old fixed /tmp names were shared by concurrent
// restores and by remnants of interrupted ones, so a failed restore followed
// by an archive without an .env could consume the previous run's stale
// staged env (audit F33). The promotion's recovery directory is retained
// until the .env commit also succeeds, so volume data and env are replaced
// as one recoverable unit (audit F35).
func (c *Client) RestoreVolumesKeep(ctx context.Context, app, date string, s3 S3Config, quiesced string) (string, error) {
	if err := ValidateDate(date); err != nil {
		return "", err
	}
	if err := c.ensureAWSCLI(ctx); err != nil {
		return "", err
	}

	volumesDir := fmt.Sprintf("%s/%s/volumes", deploymentsDir, app)
	appDir := fmt.Sprintf("%s/%s", deploymentsDir, app)
	envPath := appDir + "/.env"
	s3Key := fmt.Sprintf("s3://%s/%s/volumes/%s.tar.gz", s3.Base(), app, date)

	runOut, err := c.exec.Run(ctx, "umask 077; mktemp -d /tmp/teploy-volume-restore.XXXXXXXX")
	if err != nil {
		return "", fmt.Errorf("creating restore workspace: %w", err)
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
		return "", fmt.Errorf("downloading from S3: %w", err)
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
		if quiesced != "" {
			err = fmt.Errorf("%w (containers stopped for the restore: %s — restart them or re-run after recovering)", err, quiesced)
		}
		return recoveryDir, err
	}
	// The volumes promotion succeeded but is not yet committed: the
	// recovery directory is returned to the caller, which releases it only
	// once its own post-restore steps also succeed.

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
		return recoveryDir, fmt.Errorf("restoring .env: %w — volume recovery copy kept at %s, restore files kept in %s", err, recoveryDir, runDir)
	} else if strings.TrimSpace(out) != "" {
		fmt.Fprint(c.out, out+"\n")
	}
	// The run directory is spent; the recovery directory stays alive for
	// the caller (the RestoreVolumes wrapper drops it once the whole
	// command succeeds).
	cleanupRun()
	fmt.Fprintln(c.out, "Restore complete")
	return recoveryDir, nil
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

	recoveryDir, err := promoteStaged(ctx, exec, stageDir, liveDir, out, stageDir, archivePath)
	if err != nil {
		// A recovery-incomplete failure retains its artifacts INSIDE the
		// staged tree — the caller's cleanup must not delete what the
		// error just described as kept (TCL-43).
		if !errors.Is(err, errRecoveryIncomplete) {
			cleanup()
		}
		return recoveryDir, err
	}
	cleanup()
	return recoveryDir, nil
}

// promoteStaged copies an already-extracted staged tree over liveDir's
// contents using the two-phase recovery discipline (see
// extractToStagingThenPromote): originals are moved aside first, so a
// failure at ANY point is recoverable and never destroys the only copy.
// retainedPaths name the staged tree (and archive) that a
// recovery-incomplete error must tell the operator about; the CALLER owns
// their cleanup. Shared by volume-archive restore and the DR cutover path.
func promoteStaged(ctx context.Context, exec ssh.Executor, stageDir, liveDir string, out io.Writer, retainedPaths ...string) (string, error) {
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
				errRecoveryIncomplete, liveDir, err, liveDir, recoveryDir, strings.Join(retainedPaths, ", "))
		}
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
				errRecoveryIncomplete, liveDir, err, recoveryDir, strings.Join(retainedPaths, ", "))
		}
		exec.Run(context.WithoutCancel(ctx), "rm -rf "+ssh.ShellQuote(recoveryDir))
		return "", fmt.Errorf("promoting staged restore into %s: %w — previous contents restored, staged restore discarded",
			liveDir, err)
	}

	return recoveryDir, nil
}

// ListBackups lists available backups from S3.
func (c *Client) ListBackups(ctx context.Context, app, prefix string, s3 S3Config) ([]string, error) {
	if err := c.ensureAWSCLI(ctx); err != nil {
		return nil, err
	}

	s3Path := fmt.Sprintf("s3://%s/%s/%s/", s3.Base(), app, prefix)
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
