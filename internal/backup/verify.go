package backup

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/useteploy/teploy/internal/ssh"
)

// VerifyResult reports the outcome of a backup verification: the backup that
// was tested, what kind of restore was exercised, and the evidence that the
// restored copy is actually usable (not just that a file exists in S3).
type VerifyResult struct {
	App        string `json:"app"`
	Accessory  string `json:"accessory"`
	Image      string `json:"image"`
	Kind       string `json:"kind"` // postgres|mysql|mongo|redis|nucleus|volume
	Date       string `json:"date"`
	S3Key      string `json:"s3_key"`
	SizeBytes  int64  `json:"size_bytes"`
	Metric     string `json:"metric"` // e.g. "tables=42", "keys=310", "files=12"
	DurationMs int64  `json:"duration_ms"`
	OK         bool   `json:"ok"`
	Detail     string `json:"detail,omitempty"`
}

// InspectAccessory reads the image and environment of the RUNNING accessory
// container from server state (docker inspect). This is what lets
// verify-backup work without teploy.yml — the container itself is the source
// of truth for image + env, and cloning its env into the scratch container
// gives auth parity with the backup commands (same POSTGRES_USER, same
// passwordless-root mysql, etc.).
func (c *Client) InspectAccessory(ctx context.Context, app, name string) (image string, env map[string]string, err error) {
	containerName := app + "-" + name
	img, err := c.exec.Run(ctx, fmt.Sprintf(
		"docker inspect -f '{{.Config.Image}}' %s", ssh.ShellQuote(containerName)))
	if err != nil {
		return "", nil, fmt.Errorf("accessory %s not found on server (docker inspect): %w", containerName, err)
	}
	rawEnv, err := c.exec.Run(ctx, fmt.Sprintf(
		"docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' %s", ssh.ShellQuote(containerName)))
	if err != nil {
		return "", nil, fmt.Errorf("reading accessory env: %w", err)
	}
	env = make(map[string]string)
	for _, line := range strings.Split(rawEnv, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if ok {
			env[k] = v
		}
	}
	return strings.TrimSpace(img), env, nil
}

// LatestBackupDate resolves the most recent backup timestamp for an accessory
// by listing the S3 prefix. Returns the bare timestamp (the <date> restore
// arg) and the object's size in bytes.
func (c *Client) LatestBackupDate(ctx context.Context, app, name string, s3 S3Config) (date string, size int64, err error) {
	prefix := fmt.Sprintf("s3://%s/%s/accessories/%s/", s3.Bucket, app, name)
	out, err := c.exec.Run(ctx, fmt.Sprintf("aws s3 ls %s --region %s", prefix, s3.Region))
	if err != nil {
		return "", 0, fmt.Errorf("listing backups at %s: %w", prefix, err)
	}
	// Lines look like: 2026-07-10 04:00:01   52428800 20260710-040000.sql.gz
	// The teploy timestamp format sorts lexically, so track the max.
	var best string
	var bestSize int64
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		fname := fields[len(fields)-1]
		ts, _, ok := strings.Cut(fname, ".")
		if !ok || ValidateDate(ts) != nil {
			continue
		}
		if ts > best {
			best = ts
			bestSize, _ = strconv.ParseInt(fields[len(fields)-2], 10, 64)
		}
	}
	if best == "" {
		return "", 0, fmt.Errorf("no backups found at %s", prefix)
	}
	return best, bestSize, nil
}

// VerifyBackup downloads a backup (latest when date is empty), restores it
// into a throwaway scratch container (or scratch directory for plain volume
// archives), and checks the restored copy is actually usable. The running
// accessory is never touched. The scratch container/directory is always torn
// down, pass or fail.
//
// Image and env come from the running container (InspectAccessory), so this
// works from server state alone — no teploy.yml required, which makes it
// callable from teploy-dash and from cron.
func (c *Client) VerifyBackup(ctx context.Context, app, name, date string, s3 S3Config) (*VerifyResult, error) {
	if err := c.ensureAWSCLI(ctx); err != nil {
		return nil, err
	}
	start := time.Now()

	image, env, err := c.InspectAccessory(ctx, app, name)
	if err != nil {
		return nil, err
	}

	var size int64
	if date == "" {
		date, size, err = c.LatestBackupDate(ctx, app, name, s3)
		if err != nil {
			return nil, err
		}
	} else if err := ValidateDate(date); err != nil {
		return nil, err
	}

	res := &VerifyResult{
		App: app, Accessory: name, Image: image, Date: date, SizeBytes: size,
	}

	scratch := app + "-" + name + "-verify"
	tmpBase := fmt.Sprintf("/tmp/teploy-verify-%s-%s", app, name)

	// Always clean up, pass or fail — a leftover scratch container must never
	// survive a verification run.
	defer func() {
		_, _ = c.exec.Run(context.WithoutCancel(ctx), fmt.Sprintf(
			"docker rm -f %s >/dev/null 2>&1; rm -rf %s %s.d",
			ssh.ShellQuote(scratch), ssh.ShellQuote(tmpBase), ssh.ShellQuote(tmpBase)))
	}()
	// Remove any stale scratch from a previous interrupted run.
	_, _ = c.exec.Run(ctx, fmt.Sprintf("docker rm -f %s >/dev/null 2>&1 || true", ssh.ShellQuote(scratch)))

	switch {
	case isDBType(image, "postgres"):
		res.Kind = "postgres"
		err = c.verifyPostgres(ctx, res, app, name, image, env, s3, scratch, tmpBase)
	case isDBType(image, "mysql"), isDBType(image, "mariadb"):
		res.Kind = "mysql"
		err = c.verifyMySQL(ctx, res, app, name, image, env, s3, scratch, tmpBase)
	case isDBType(image, "redis"):
		res.Kind = "redis"
		err = c.verifyRedis(ctx, res, app, name, image, env, s3, scratch, tmpBase)
	case isDBType(image, "mongo"):
		res.Kind = "mongo"
		err = c.verifyMongo(ctx, res, app, name, image, env, s3, scratch, tmpBase)
	case isDBType(image, "nucleus"):
		res.Kind = "nucleus"
		err = c.verifyNucleus(ctx, res, app, name, image, env, s3, scratch, tmpBase)
	default:
		res.Kind = "volume"
		err = c.verifyVolume(ctx, res, app, name, s3, tmpBase)
	}

	res.DurationMs = time.Since(start).Milliseconds()
	if err != nil {
		res.OK = false
		res.Detail = err.Error()
		// A failed verification is a RESULT, not an operational error: the
		// caller gets ok=false + detail and decides how to alert.
		return res, nil
	}
	res.OK = true
	return res, nil
}

// envFlags renders `-e K=V` docker-run flags for the env keys relevant to a
// DB image, cloning the source accessory's configuration so the scratch
// container authenticates exactly like the one the backup was taken from.
func envFlags(env map[string]string, prefixes ...string) string {
	var parts []string
	for k, v := range env {
		for _, p := range prefixes {
			if strings.HasPrefix(k, p) {
				parts = append(parts, "-e "+ssh.ShellQuote(k+"="+v))
				break
			}
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, " ")
}

func (c *Client) download(ctx context.Context, s3Key, dest string, s3 S3Config) error {
	fmt.Fprintf(c.out, "Downloading %s...\n", s3Key)
	_, err := c.exec.Run(ctx, s3.AWS(fmt.Sprintf("s3 cp %s %s",
		ssh.ShellQuote(s3Key), ssh.ShellQuote(dest))))
	if err != nil {
		return fmt.Errorf("downloading backup: %w", err)
	}
	return nil
}

// waitReady polls a readiness probe inside the scratch container.
func (c *Client) waitReady(ctx context.Context, scratch, probe string, attempts int) error {
	cmd := fmt.Sprintf(
		"for i in $(seq 1 %d); do docker exec %s %s >/dev/null 2>&1 && exit 0; sleep 3; done; echo 'scratch container never became ready' >&2; docker logs --tail 20 %s >&2; exit 1",
		attempts, ssh.ShellQuote(scratch), probe, ssh.ShellQuote(scratch))
	if _, err := c.exec.Run(ctx, cmd); err != nil {
		return fmt.Errorf("scratch %s not ready: %w", scratch, err)
	}
	return nil
}

func (c *Client) verifyPostgres(ctx context.Context, res *VerifyResult, app, name, image string, env map[string]string, s3 S3Config, scratch, tmpBase string) error {
	db, user := postgresDBAndUser(app, env)
	s3Key := fmt.Sprintf("s3://%s/%s/accessories/%s/%s.sql.gz", s3.Bucket, app, name, res.Date)
	res.S3Key = s3Key
	dump := tmpBase + ".sql.gz"
	if err := c.download(ctx, s3Key, dump, s3); err != nil {
		return err
	}

	flags := envFlags(env, "POSTGRES_")
	if _, ok := env["POSTGRES_PASSWORD"]; !ok {
		// Source ran without auth config (trust); scratch images require one.
		flags += " -e POSTGRES_HOST_AUTH_METHOD=trust"
	}
	fmt.Fprintf(c.out, "Starting scratch %s...\n", scratch)
	if _, err := c.exec.Run(ctx, fmt.Sprintf(
		"docker run -d --name %s %s %s >/dev/null", ssh.ShellQuote(scratch), flags, ssh.ShellQuote(image))); err != nil {
		return fmt.Errorf("starting scratch container: %w", err)
	}
	if err := c.waitReady(ctx, scratch, fmt.Sprintf("pg_isready -U %s", ssh.ShellQuote(user)), 30); err != nil {
		return err
	}

	fmt.Fprintf(c.out, "Restoring into scratch...\n")
	if _, err := c.exec.Run(ctx, fmt.Sprintf(
		"gunzip -c %s | docker exec -i %s psql -q -v ON_ERROR_STOP=1 -U %s %s >/dev/null",
		ssh.ShellQuote(dump), ssh.ShellQuote(scratch), ssh.ShellQuote(user), ssh.ShellQuote(db))); err != nil {
		return fmt.Errorf("restore into scratch failed: %w", err)
	}

	out, err := c.exec.Run(ctx, fmt.Sprintf(
		"docker exec %s psql -tA -U %s %s -c \"SELECT COUNT(*) FROM information_schema.tables WHERE table_schema='public'\"",
		ssh.ShellQuote(scratch), ssh.ShellQuote(user), ssh.ShellQuote(db)))
	if err != nil {
		return fmt.Errorf("verify query failed: %w", err)
	}
	n, _ := strconv.Atoi(strings.TrimSpace(out))
	res.Metric = fmt.Sprintf("tables=%d", n)
	if n == 0 {
		return fmt.Errorf("restored database has zero tables")
	}
	return nil
}

func (c *Client) verifyMySQL(ctx context.Context, res *VerifyResult, app, name, image string, env map[string]string, s3 S3Config, scratch, tmpBase string) error {
	db := mysqlDB(app, env)
	s3Key := fmt.Sprintf("s3://%s/%s/accessories/%s/%s.sql.gz", s3.Bucket, app, name, res.Date)
	res.S3Key = s3Key
	dump := tmpBase + ".sql.gz"
	if err := c.download(ctx, s3Key, dump, s3); err != nil {
		return err
	}

	flags := envFlags(env, "MYSQL_", "MARIADB_")
	if _, ok := env["MYSQL_ROOT_PASSWORD"]; !ok {
		if _, ok2 := env["MARIADB_ROOT_PASSWORD"]; !ok2 {
			flags += " -e MYSQL_ALLOW_EMPTY_PASSWORD=yes"
		}
	}
	fmt.Fprintf(c.out, "Starting scratch %s...\n", scratch)
	if _, err := c.exec.Run(ctx, fmt.Sprintf(
		"docker run -d --name %s %s %s >/dev/null", ssh.ShellQuote(scratch), flags, ssh.ShellQuote(image))); err != nil {
		return fmt.Errorf("starting scratch container: %w", err)
	}
	// mysqladmin ping via socket mirrors the backup's `mysqldump -u root`
	// (socket auth, no password on argv).
	if err := c.waitReady(ctx, scratch, "mysqladmin ping -u root --silent", 40); err != nil {
		return err
	}

	fmt.Fprintf(c.out, "Restoring into scratch...\n")
	if _, err := c.exec.Run(ctx, fmt.Sprintf(
		"gunzip -c %s | docker exec -i %s mysql -u root %s",
		ssh.ShellQuote(dump), ssh.ShellQuote(scratch), ssh.ShellQuote(db))); err != nil {
		return fmt.Errorf("restore into scratch failed: %w", err)
	}

	out, err := c.exec.Run(ctx, fmt.Sprintf(
		"docker exec %s sh -c %s", ssh.ShellQuote(scratch),
		ssh.ShellQuote(fmt.Sprintf("mysql -u root -N -e \"SHOW TABLES\" %s | wc -l", db))))
	if err != nil {
		return fmt.Errorf("verify query failed: %w", err)
	}
	n, _ := strconv.Atoi(strings.TrimSpace(out))
	res.Metric = fmt.Sprintf("tables=%d", n)
	if n == 0 {
		return fmt.Errorf("restored database has zero tables")
	}
	return nil
}

func (c *Client) verifyRedis(ctx context.Context, res *VerifyResult, app, name, image string, env map[string]string, s3 S3Config, scratch, tmpBase string) error {
	s3Key := fmt.Sprintf("s3://%s/%s/accessories/%s/%s.rdb.gz", s3.Bucket, app, name, res.Date)
	res.S3Key = s3Key
	dump := tmpBase + ".rdb.gz"
	if err := c.download(ctx, s3Key, dump, s3); err != nil {
		return err
	}

	fmt.Fprintf(c.out, "Starting scratch %s with restored dump.rdb...\n", scratch)
	// Create stopped, seed the rdb, then start — so redis boots FROM the
	// backup (a corrupt rdb fails the readiness probe).
	cmd := fmt.Sprintf(
		"gunzip -c %s > %s.rdb && docker create --name %s %s >/dev/null && docker cp %s.rdb %s:/data/dump.rdb && docker start %s >/dev/null",
		ssh.ShellQuote(dump), ssh.ShellQuote(tmpBase),
		ssh.ShellQuote(scratch), ssh.ShellQuote(image),
		ssh.ShellQuote(tmpBase), ssh.ShellQuote(scratch), ssh.ShellQuote(scratch))
	if _, err := c.exec.Run(ctx, cmd); err != nil {
		return fmt.Errorf("seeding scratch redis: %w", err)
	}
	if err := c.waitReady(ctx, scratch, "redis-cli ping", 20); err != nil {
		return err
	}
	out, err := c.exec.Run(ctx, fmt.Sprintf("docker exec %s redis-cli dbsize", ssh.ShellQuote(scratch)))
	if err != nil {
		return fmt.Errorf("verify query failed: %w", err)
	}
	res.Metric = "keys=" + strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(out), "(integer)"))
	return nil
}

func (c *Client) verifyMongo(ctx context.Context, res *VerifyResult, app, name, image string, env map[string]string, s3 S3Config, scratch, tmpBase string) error {
	s3Key := fmt.Sprintf("s3://%s/%s/accessories/%s/%s.archive.gz", s3.Bucket, app, name, res.Date)
	res.S3Key = s3Key
	dump := tmpBase + ".archive.gz"
	if err := c.download(ctx, s3Key, dump, s3); err != nil {
		return err
	}

	fmt.Fprintf(c.out, "Starting scratch %s...\n", scratch)
	if _, err := c.exec.Run(ctx, fmt.Sprintf(
		"docker run -d --name %s %s %s >/dev/null",
		ssh.ShellQuote(scratch), envFlags(env, "MONGO_"), ssh.ShellQuote(image))); err != nil {
		return fmt.Errorf("starting scratch container: %w", err)
	}
	// mongosh on 5.x+, legacy mongo shell before that.
	probe := "sh -c 'mongosh --quiet --eval 1 || mongo --quiet --eval 1'"
	if err := c.waitReady(ctx, scratch, probe, 30); err != nil {
		return err
	}

	fmt.Fprintf(c.out, "Restoring into scratch...\n")
	if _, err := c.exec.Run(ctx, fmt.Sprintf(
		"cat %s | docker exec -i %s mongorestore --archive --gzip --quiet",
		ssh.ShellQuote(dump), ssh.ShellQuote(scratch))); err != nil {
		return fmt.Errorf("restore into scratch failed: %w", err)
	}

	out, err := c.exec.Run(ctx, fmt.Sprintf(
		"docker exec %s sh -c 'mongosh --quiet --eval \"db.getMongo().getDBNames().length\" || mongo --quiet --eval \"db.getMongo().getDBNames().length\"'",
		ssh.ShellQuote(scratch)))
	if err != nil {
		return fmt.Errorf("verify query failed: %w", err)
	}
	res.Metric = "dbs=" + strings.TrimSpace(out)
	return nil
}

// verifyNucleus boot-verifies a Nucleus engine on the restored data
// directory: the generic accessory tar contains the accessory's volume dirs
// (nucleus-data/), and a scratch engine must come up, replay/restore its
// tables, and stay running. The "Restored N table(s)" boot line is the
// verification metric.
func (c *Client) verifyNucleus(ctx context.Context, res *VerifyResult, app, name, image string, env map[string]string, s3 S3Config, scratch, tmpBase string) error {
	s3Key := fmt.Sprintf("s3://%s/%s/accessories/%s/%s.tar.gz", s3.Bucket, app, name, res.Date)
	res.S3Key = s3Key
	dump := tmpBase + ".tar.gz"
	if err := c.download(ctx, s3Key, dump, s3); err != nil {
		return err
	}

	extractDir := tmpBase + ".d"
	if _, err := c.exec.Run(ctx, fmt.Sprintf(
		"mkdir -p %s && tar -xzf %s -C %s && test -d %s/nucleus-data",
		ssh.ShellQuote(extractDir), ssh.ShellQuote(dump), ssh.ShellQuote(extractDir), ssh.ShellQuote(extractDir))); err != nil {
		return fmt.Errorf("archive extract failed (or no nucleus-data dir inside): %w", err)
	}

	fmt.Fprintf(c.out, "Booting scratch %s on the restored data dir...\n", scratch)
	if _, err := c.exec.Run(ctx, fmt.Sprintf(
		"docker run -d --name %s %s -v %s/nucleus-data:/data %s >/dev/null",
		ssh.ShellQuote(scratch), envFlags(env, "NUCLEUS_"), ssh.ShellQuote(extractDir), ssh.ShellQuote(image))); err != nil {
		return fmt.Errorf("starting scratch container: %w", err)
	}

	// Wait for the boot-restore line, then confirm the engine stayed up.
	cmd := fmt.Sprintf(
		"for i in $(seq 1 30); do docker logs %s 2>&1 | grep -q 'Restored' && break; sleep 3; done; "+
			"docker logs %s 2>&1 | grep -o 'Restored [0-9]* table' | tail -1; "+
			"test \"$(docker inspect -f '{{.State.Status}}' %s)\" = running",
		ssh.ShellQuote(scratch), ssh.ShellQuote(scratch), ssh.ShellQuote(scratch))
	out, err := c.exec.Run(ctx, cmd)
	if err != nil {
		return fmt.Errorf("scratch engine failed to boot from the backup: %w", err)
	}
	line := strings.TrimSpace(out)
	if line == "" {
		return fmt.Errorf("engine booted but never logged a table restore")
	}
	res.Metric = "restored " + strings.TrimPrefix(line, "Restored ") + "s"
	return nil
}

// verifyVolume checks a generic volume archive: integrity (a truncated or
// corrupt tar fails extraction) and non-emptiness.
func (c *Client) verifyVolume(ctx context.Context, res *VerifyResult, app, name string, s3 S3Config, tmpBase string) error {
	s3Key := fmt.Sprintf("s3://%s/%s/accessories/%s/%s.tar.gz", s3.Bucket, app, name, res.Date)
	res.S3Key = s3Key
	dump := tmpBase + ".tar.gz"
	if err := c.download(ctx, s3Key, dump, s3); err != nil {
		return err
	}
	extractDir := tmpBase + ".d"
	out, err := c.exec.Run(ctx, fmt.Sprintf(
		"mkdir -p %s && tar -xzf %s -C %s && find %s -type f | wc -l",
		ssh.ShellQuote(extractDir), ssh.ShellQuote(dump), ssh.ShellQuote(extractDir), ssh.ShellQuote(extractDir)))
	if err != nil {
		return fmt.Errorf("archive extract failed: %w", err)
	}
	n, _ := strconv.Atoi(strings.TrimSpace(out))
	res.Metric = fmt.Sprintf("files=%d", n)
	if n == 0 {
		return fmt.Errorf("archive extracted but contains zero files")
	}
	return nil
}
