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
type S3Config struct {
	Bucket string
	Region string
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
	uploadCmd := fmt.Sprintf("aws s3 cp %s %s --region %s", archivePath, s3Key, s3.Region)
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
	if _, err := c.exec.Run(ctx, fmt.Sprintf("aws s3 cp %s %s --region %s", s3Key, archivePath, s3.Region)); err != nil {
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
	output, err := c.exec.Run(ctx, fmt.Sprintf("aws s3 ls %s --region %s", s3Path, s3.Region))
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
func (c *Client) AccessoryBackup(ctx context.Context, app, name, image string, s3 S3Config) error {
	if err := c.ensureAWSCLI(ctx); err != nil {
		return err
	}

	timestamp := time.Now().UTC().Format("20060102-150405")
	containerName := app + "-" + name
	dumpPath := fmt.Sprintf("/tmp/%s-%s-%s.sql.gz", app, name, timestamp)
	s3Key := fmt.Sprintf("s3://%s/%s/accessories/%s/%s.sql.gz", s3.Bucket, app, name, timestamp)

	var dumpCmd string
	switch {
	case isDBType(image, "postgres"):
		db := app // Default DB name
		dumpCmd = fmt.Sprintf("docker exec %s pg_dump -U postgres %s | gzip > %s", containerName, db, dumpPath)
	case isDBType(image, "mysql"), isDBType(image, "mariadb"):
		db := app
		dumpCmd = fmt.Sprintf("docker exec %s mysqldump -u root %s | gzip > %s", containerName, db, dumpPath)
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
		// Generic: tar the volume directory.
		accDir := fmt.Sprintf("%s/%s/accessories/%s", deploymentsDir, app, name)
		dumpPath = fmt.Sprintf("/tmp/%s-%s-%s.tar.gz", app, name, timestamp)
		dumpCmd = fmt.Sprintf("tar -czf %s -C %s .", dumpPath, accDir)
		s3Key = fmt.Sprintf("s3://%s/%s/accessories/%s/%s.tar.gz", s3.Bucket, app, name, timestamp)
	}

	fmt.Fprintf(c.out, "Backing up %s...\n", containerName)
	if _, err := c.exec.Run(ctx, dumpCmd); err != nil {
		return fmt.Errorf("dumping %s: %w", name, err)
	}

	fmt.Fprintf(c.out, "Uploading to %s...\n", s3Key)
	if _, err := c.exec.Run(ctx, fmt.Sprintf("aws s3 cp %s %s --region %s", dumpPath, s3Key, s3.Region)); err != nil {
		return fmt.Errorf("uploading to S3: %w", err)
	}

	c.exec.Run(ctx, "rm -f "+dumpPath)
	fmt.Fprintf(c.out, "Backup complete: %s\n", s3Key)
	return nil
}

// AccessoryRestore restores a database backup from S3.
func (c *Client) AccessoryRestore(ctx context.Context, app, name, image, date string, s3 S3Config) error {
	if err := c.ensureAWSCLI(ctx); err != nil {
		return err
	}

	containerName := app + "-" + name

	// Determine file type and restore command based on DB type.
	var s3Key, restorePath, restoreCmd string

	switch {
	case isDBType(image, "postgres"):
		s3Key = fmt.Sprintf("s3://%s/%s/accessories/%s/%s.sql.gz", s3.Bucket, app, name, date)
		restorePath = "/tmp/restore.sql.gz"
		restoreCmd = fmt.Sprintf("gunzip -c %s | docker exec -i %s psql -U postgres %s", restorePath, containerName, app)
	case isDBType(image, "mysql"), isDBType(image, "mariadb"):
		s3Key = fmt.Sprintf("s3://%s/%s/accessories/%s/%s.sql.gz", s3.Bucket, app, name, date)
		restorePath = "/tmp/restore.sql.gz"
		restoreCmd = fmt.Sprintf("gunzip -c %s | docker exec -i %s mysql -u root %s", restorePath, containerName, app)
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
	if _, err := c.exec.Run(ctx, fmt.Sprintf("aws s3 cp %s %s --region %s", s3Key, restorePath, s3.Region)); err != nil {
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
	line := fmt.Sprintf("%s %s # %s", schedule, command, marker)
	cmd := fmt.Sprintf(
		`(crontab -l 2>/dev/null | grep -vF %s; printf '%%s\n' %s) | crontab -`,
		ssh.ShellQuote(marker), ssh.ShellQuote(line),
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
