package backup

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/useteploy/teploy/internal/ssh"
)

const deploymentsDir = "/deployments"

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

// ValidateDate checks that a date/timestamp string is safe for shell use (e.g. 20060102-150405).
func ValidateDate(date string) error {
	if !safeName.MatchString(date) || len(date) > 30 {
		return fmt.Errorf("invalid date %q — expected format like 20060102-150405", date)
	}
	return nil
}

// ValidateSchedule checks that a cron schedule expression is safe.
func ValidateSchedule(schedule string) error {
	// Cron fields only contain: digits, spaces, *, /, -, commas.
	for _, c := range schedule {
		if !((c >= '0' && c <= '9') || c == ' ' || c == '*' || c == '/' || c == '-' || c == ',') {
			return fmt.Errorf("invalid cron schedule %q — unexpected character %q", schedule, string(c))
		}
	}
	return nil
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
	b.WriteString("aws " + args + " --region " + s3.Region)
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

// BackupVolumes creates a tar.gz archive of all app volumes and uploads to S3.
func (c *Client) BackupVolumes(ctx context.Context, app string, s3 S3Config) error {
	if err := c.ensureAWSCLI(ctx); err != nil {
		return err
	}

	timestamp := time.Now().UTC().Format("20060102-150405")
	volumesDir := fmt.Sprintf("%s/%s/volumes", deploymentsDir, app)
	envFile := fmt.Sprintf("%s/%s/.env", deploymentsDir, app)
	archivePath := fmt.Sprintf("/tmp/%s-volumes-%s.tar.gz", app, timestamp)
	s3Key := fmt.Sprintf("s3://%s/%s/volumes/%s.tar.gz", s3.Bucket, app, timestamp)

	// Create archive.
	fmt.Fprintf(c.out, "Archiving volumes for %s...\n", app)
	cmd := fmt.Sprintf(
		"tar -czf %s -C %s . %s 2>/dev/null || tar -czf %s -C %s .",
		archivePath, volumesDir, envFile, archivePath, volumesDir,
	)
	if _, err := c.exec.Run(ctx, cmd); err != nil {
		return fmt.Errorf("creating archive: %w", err)
	}

	// Upload to S3.
	fmt.Fprintf(c.out, "Uploading to %s...\n", s3Key)
	uploadCmd := s3.AWS(fmt.Sprintf("s3 cp %s %s", archivePath, s3Key))
	if _, err := c.exec.Run(ctx, uploadCmd); err != nil {
		return fmt.Errorf("uploading to S3: %w", err)
	}

	// Clean up local archive.
	c.exec.Run(ctx, "rm -f "+archivePath)
	fmt.Fprintf(c.out, "Backup complete: %s\n", s3Key)
	return nil
}

// RestoreVolumes downloads and extracts a volume backup from S3.
func (c *Client) RestoreVolumes(ctx context.Context, app, date string, s3 S3Config) error {
	if err := c.ensureAWSCLI(ctx); err != nil {
		return err
	}

	volumesDir := fmt.Sprintf("%s/%s/volumes", deploymentsDir, app)
	s3Key := fmt.Sprintf("s3://%s/%s/volumes/%s.tar.gz", s3.Bucket, app, date)
	archivePath := fmt.Sprintf("/tmp/%s-volumes-restore.tar.gz", app)

	fmt.Fprintf(c.out, "Downloading %s...\n", s3Key)
	if _, err := c.exec.Run(ctx, s3.AWS(fmt.Sprintf("s3 cp %s %s", s3Key, archivePath))); err != nil {
		return fmt.Errorf("downloading from S3: %w", err)
	}

	fmt.Fprintf(c.out, "Restoring to %s...\n", volumesDir)
	if _, err := c.exec.Run(ctx, fmt.Sprintf("mkdir -p %s && tar -xzf %s -C %s", volumesDir, archivePath, volumesDir)); err != nil {
		return fmt.Errorf("extracting archive: %w", err)
	}

	c.exec.Run(ctx, "rm -f "+archivePath)
	fmt.Fprintln(c.out, "Restore complete")
	return nil
}

// ListBackups lists available backups from S3.
func (c *Client) ListBackups(ctx context.Context, app, prefix string, s3 S3Config) ([]string, error) {
	if err := c.ensureAWSCLI(ctx); err != nil {
		return nil, err
	}

	s3Path := fmt.Sprintf("s3://%s/%s/%s/", s3.Bucket, app, prefix)
	output, err := c.exec.Run(ctx, s3.AWS(fmt.Sprintf("s3 ls %s", s3Path)))
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
func (c *Client) AccessoryBackup(ctx context.Context, app, name, image string, env map[string]string, s3 S3Config) error {
	if err := c.ensureAWSCLI(ctx); err != nil {
		return err
	}

	timestamp := time.Now().UTC().Format("20060102-150405")
	containerName := app + "-" + name
	dumpPath := fmt.Sprintf("/tmp/%s-%s-%s.sql.gz", app, name, timestamp)
	s3Key := fmt.Sprintf("s3://%s/%s/accessories/%s/%s.sql.gz", s3.Bucket, app, name, timestamp)

	// dumpTmp is redirected into with `>`, not piped into gzip: a shell
	// pipeline's exit status is its LAST command's (gzip, which "succeeds"
	// compressing an empty stream even when pg_dump/mysqldump errored to
	// stderr) — `| gzip > path` would silently swallow a real dump
	// failure. Confirmed live: a wrong db name (see postgresDBAndUser)
	// produced a 20-byte gzip of nothing while the old `| gzip` version of
	// this command reported "Backup complete". Redirecting to a plain
	// file with `>` preserves the dump command's own exit code, which
	// c.exec.Run already surfaces (with captured stderr) as a real error.
	dumpTmp := fmt.Sprintf("/tmp/%s-%s-%s.sql", app, name, timestamp)
	var dumpCmd string
	switch {
	case isDBType(image, "postgres"):
		db, user := postgresDBAndUser(app, env)
		dumpCmd = fmt.Sprintf("docker exec %s pg_dump -U %s %s > %s && gzip -c %s > %s && rm -f %s",
			containerName, ssh.ShellQuote(user), ssh.ShellQuote(db), dumpTmp, dumpTmp, dumpPath, dumpTmp)
	case isDBType(image, "mysql"), isDBType(image, "mariadb"):
		db := mysqlDB(app, env)
		dumpCmd = fmt.Sprintf("docker exec %s mysqldump -u root %s > %s && gzip -c %s > %s && rm -f %s",
			containerName, ssh.ShellQuote(db), dumpTmp, dumpTmp, dumpPath, dumpTmp)
	case isDBType(image, "mongo"):
		dumpCmd = fmt.Sprintf("docker exec %s mongodump --archive --gzip > %s", containerName, dumpPath)
		s3Key = fmt.Sprintf("s3://%s/%s/accessories/%s/%s.archive.gz", s3.Bucket, app, name, timestamp)
	case isDBType(image, "redis"):
		// Redis: trigger bgsave then copy dump.rdb.
		dumpCmd = fmt.Sprintf(
			"docker exec %s redis-cli bgsave && sleep 2 && docker cp %s:/data/dump.rdb /tmp/%s-redis.rdb && gzip -c /tmp/%s-redis.rdb > %s && rm -f /tmp/%s-redis.rdb",
			containerName, containerName, app, app, dumpPath, app,
		)
		s3Key = fmt.Sprintf("s3://%s/%s/accessories/%s/%s.rdb.gz", s3.Bucket, app, name, timestamp)
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
		dumpPath = fmt.Sprintf("/tmp/%s-%s-%s.tar.gz", app, name, timestamp)
		dumpCmd = fmt.Sprintf("tar -czf %s -C %s . || [ $? -eq 1 ]", dumpPath, accDir)
		s3Key = fmt.Sprintf("s3://%s/%s/accessories/%s/%s.tar.gz", s3.Bucket, app, name, timestamp)
	}

	fmt.Fprintf(c.out, "Backing up %s...\n", containerName)
	if _, err := c.exec.Run(ctx, dumpCmd); err != nil {
		return fmt.Errorf("dumping %s: %w", name, err)
	}

	fmt.Fprintf(c.out, "Uploading to %s...\n", s3Key)
	if _, err := c.exec.Run(ctx, s3.AWS(fmt.Sprintf("s3 cp %s %s", dumpPath, s3Key))); err != nil {
		return fmt.Errorf("uploading to S3: %w", err)
	}

	c.exec.Run(ctx, "rm -f "+dumpPath)
	fmt.Fprintf(c.out, "Backup complete: %s\n", s3Key)
	return nil
}

// AccessoryRestore restores a database backup from S3.
func (c *Client) AccessoryRestore(ctx context.Context, app, name, image, date string, env map[string]string, s3 S3Config) error {
	if err := c.ensureAWSCLI(ctx); err != nil {
		return err
	}

	containerName := app + "-" + name

	// Determine file type and restore command based on DB type.
	var s3Key, restorePath, restoreCmd string

	switch {
	case isDBType(image, "postgres"):
		db, user := postgresDBAndUser(app, env)
		s3Key = fmt.Sprintf("s3://%s/%s/accessories/%s/%s.sql.gz", s3.Bucket, app, name, date)
		restorePath = "/tmp/restore.sql.gz"
		restoreCmd = fmt.Sprintf("gunzip -c %s | docker exec -i %s psql -U %s %s",
			restorePath, containerName, ssh.ShellQuote(user), ssh.ShellQuote(db))
	case isDBType(image, "mysql"), isDBType(image, "mariadb"):
		db := mysqlDB(app, env)
		s3Key = fmt.Sprintf("s3://%s/%s/accessories/%s/%s.sql.gz", s3.Bucket, app, name, date)
		restorePath = "/tmp/restore.sql.gz"
		restoreCmd = fmt.Sprintf("gunzip -c %s | docker exec -i %s mysql -u root %s",
			restorePath, containerName, ssh.ShellQuote(db))
	case isDBType(image, "mongo"):
		s3Key = fmt.Sprintf("s3://%s/%s/accessories/%s/%s.archive.gz", s3.Bucket, app, name, date)
		restorePath = "/tmp/restore.archive.gz"
		restoreCmd = fmt.Sprintf("cat %s | docker exec -i %s mongorestore --archive --gzip --drop", restorePath, containerName)
	case isDBType(image, "redis"):
		// AccessoryBackup stores redis as <date>.rdb.gz; without this case the
		// default branch looked for a .tar.gz that doesn't exist, so redis
		// restores always failed. Stop redis first so its shutdown save can't
		// overwrite the snapshot we copy in, then start so it loads dump.rdb.
		s3Key = fmt.Sprintf("s3://%s/%s/accessories/%s/%s.rdb.gz", s3.Bucket, app, name, date)
		restorePath = "/tmp/restore.rdb.gz"
		restoreCmd = fmt.Sprintf(
			"gunzip -c %s > /tmp/restore.rdb && docker stop %s && docker cp /tmp/restore.rdb %s:/data/dump.rdb && docker start %s && rm -f /tmp/restore.rdb",
			restorePath, containerName, containerName, containerName,
		)
	default:
		// Generic: extract tar to volume directory.
		s3Key = fmt.Sprintf("s3://%s/%s/accessories/%s/%s.tar.gz", s3.Bucket, app, name, date)
		restorePath = "/tmp/restore.tar.gz"
		accDir := fmt.Sprintf("%s/%s/accessories/%s", deploymentsDir, app, name)
		restoreCmd = fmt.Sprintf("tar -xzf %s -C %s", restorePath, accDir)
	}

	fmt.Fprintf(c.out, "Downloading %s...\n", s3Key)
	if _, err := c.exec.Run(ctx, s3.AWS(fmt.Sprintf("s3 cp %s %s", s3Key, restorePath))); err != nil {
		return fmt.Errorf("downloading backup: %w", err)
	}

	fmt.Fprintf(c.out, "Restoring %s...\n", name)
	if _, err := c.exec.Run(ctx, restoreCmd); err != nil {
		return fmt.Errorf("restoring %s: %w", name, err)
	}

	c.exec.Run(ctx, "rm -f "+restorePath)
	fmt.Fprintln(c.out, "Restore complete")
	return nil
}

// SetSchedule creates (or replaces) a cron job for scheduled backups. marker is
// a stable per-target tag (e.g. "teploy-backup:<app>") appended as a trailing
// comment so the entry can be found and replaced on reschedule.
func (c *Client) SetSchedule(ctx context.Context, schedule, command, marker string) error {
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
	line := fmt.Sprintf("%s %s # %s", schedule, command, tag)
	cmd := fmt.Sprintf(
		`(crontab -l 2>/dev/null | grep -vF %s; printf '%%s\n' %s) | crontab -`,
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

func isDBType(image, dbType string) bool {
	base := strings.Split(image, ":")[0]
	parts := strings.Split(base, "/")
	name := parts[len(parts)-1]
	return name == dbType
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
