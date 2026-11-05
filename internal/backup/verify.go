package backup

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
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
	App       string `json:"app"`
	Accessory string `json:"accessory"`
	Image     string `json:"image"`
	Kind      string `json:"kind"` // postgres|mysql|mongo|redis|valkey|dragonfly|nucleus|volume
	// Engine is the detected engine (mariadb is Kind "mysql"; generic is
	// Kind "volume") and Detection the evidence it came from.
	Engine     string `json:"engine"`
	Detection  string `json:"detection"`
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
		line = strings.TrimSuffix(line, "\r")
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
	date, size, _, err = c.latestBackup(ctx, app, name, s3)
	return date, size, err
}

// latestBackup is LatestBackupDate plus the object's file name (its
// extension says which artifact format the backup is).
func (c *Client) latestBackup(ctx context.Context, app, name string, s3 S3Config) (date string, size int64, file string, err error) {
	prefix := fmt.Sprintf("s3://%s/%s/accessories/%s/", s3.Base(), app, name)
	out, err := c.exec.Run(ctx, s3.AWS(fmt.Sprintf("s3 ls %s", ssh.ShellQuote(prefix))))
	if err != nil {
		return "", 0, "", fmt.Errorf("listing backups at %s: %w", prefix, err)
	}
	// Lines look like: 2026-07-10 04:00:01   52428800 20260710-040000.sql.gz
	// The teploy timestamp format sorts lexically, so track the max.
	var best, bestFile string
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
			best, bestFile = ts, fname
			bestSize, _ = strconv.ParseInt(fields[len(fields)-2], 10, 64)
		}
	}
	if best == "" {
		return "", 0, "", fmt.Errorf("no backups found at %s", prefix)
	}
	return best, bestSize, bestFile, nil
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
// verifyTmpDir is where a verification stages the archive and its extract.
// Disk-backed by default; see the note at its use site for why /tmp is wrong.
func verifyTmpDir() string {
	if d := strings.TrimSpace(os.Getenv("TEPLOY_VERIFY_TMPDIR")); d != "" {
		return strings.TrimRight(d, "/")
	}
	return "/var/tmp"
}

func (c *Client) VerifyBackup(ctx context.Context, app, name, date string, s3 S3Config) (retResult *VerifyResult, retErr error) {
	if err := c.ensureAWSCLI(ctx); err != nil {
		return nil, err
	}
	start := time.Now()

	image, env, err := c.InspectAccessory(ctx, app, name)
	if err != nil {
		return nil, err
	}

	det, err := c.ResolveEngine(ctx, app, name, image, env)
	if err != nil {
		return nil, err
	}

	var size int64
	var latestFile string
	if date == "" {
		date, size, latestFile, err = c.latestBackup(ctx, app, name, s3)
		if err != nil {
			return nil, err
		}
	} else if err := ValidateDate(date); err != nil {
		return nil, err
	}

	res := &VerifyResult{
		App: app, Accessory: name, Image: image, Date: date, SizeBytes: size,
		Engine: det.Engine, Detection: det.Source,
	}

	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	owner := hex.EncodeToString(nonce[:])
	scratch := "teploy-backup-verify-" + owner
	workspace, err := c.exec.Run(ctx, "umask 077; mktemp -d "+ssh.ShellQuote(verifyTmpDir()+"/teploy-verify.XXXXXXXX"))
	if err != nil {
		return nil, fmt.Errorf("creating private verification workspace: %w", err)
	}
	workspace = strings.TrimSpace(workspace)
	if !strings.HasPrefix(workspace, verifyTmpDir()+"/teploy-verify.") || strings.ContainsAny(workspace, "\n\r") || path.Clean(workspace) != workspace {
		return nil, fmt.Errorf("invalid verification workspace")
	}
	tmpBase := workspace + "/backup"
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_, cleanupErr := c.exec.Run(cleanupCtx, fmt.Sprintf("if [ \"$(docker inspect -f '{{index .Config.Labels \"teploy.verify.owner\"}}' %s 2>/dev/null)\" = %s ]; then docker rm -fv %s >/dev/null; fi; rm -rf -- %s", ssh.ShellQuote(scratch), ssh.ShellQuote(owner), ssh.ShellQuote(scratch), ssh.ShellQuote(workspace)))
		if cleanupErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("cleaning verification scratch: %w", cleanupErr))
			if retResult != nil {
				retResult.OK = false
				retResult.Detail = "scratch cleanup failed"
			}
		}
	}()
	// The unique container name and owner label keep cleanup invocation-scoped.
	env["TEPLOY_VERIFY_OWNER"] = owner

	// A database engine's backup taken before detection recognised it is
	// a raw tar of the live data directory: restorable as files, but it
	// cannot be verified as a database, and reporting it as a passing
	// "volume" was the U3 false green.
	if det.IsDB() && det.Engine != EngineNucleus {
		ext := ""
		for _, e := range artifactExts(det.Engine) {
			if strings.HasSuffix(latestFile, e) {
				ext = e
			}
		}
		if ext == "" && latestFile == "" {
			if _, e, rerr := c.resolveAccessoryKey(ctx, s3, fmt.Sprintf("s3://%s/%s/accessories/%s/", s3.Base(), app, name), date, artifactExts(det.Engine)...); rerr == nil {
				ext = e
			}
		}
		if ext == ".tar.gz" {
			res.Kind = det.Engine
			res.S3Key = fmt.Sprintf("s3://%s/%s/accessories/%s/%s.tar.gz", s3.Base(), app, name, date)
			res.DurationMs = time.Since(start).Milliseconds()
			res.Detail = "this " + det.Engine + " backup is a raw file copy of the live data directory (taken before teploy detected the engine) — it can be restored as files but not verified as a database; take a new backup"
			return res, nil
		}
	}

	// The verification kind follows the DETECTED engine: a pgvector or
	// timescale accessory is verified as postgres (restored and queried),
	// never as a "volume" that passes on files>0 (U3).
	switch det.Engine {
	case EnginePostgres:
		res.Kind = "postgres"
		err = c.verifyPostgres(ctx, res, app, name, image, env, s3, scratch, tmpBase, latestFile)
	case EngineMySQL, EngineMariaDB:
		res.Kind = "mysql"
		err = c.verifyMySQL(ctx, res, app, name, det.Engine, image, env, s3, scratch, tmpBase)
	case EngineRedis, EngineValkey:
		res.Kind = det.Engine
		err = c.verifyRedis(ctx, res, app, name, det.Engine, image, env, s3, scratch, tmpBase)
	case EngineDragonfly:
		res.Kind = "dragonfly"
		err = c.verifyDragonfly(ctx, res, app, name, image, env, s3, scratch, tmpBase)
	case EngineMongo:
		res.Kind = "mongo"
		err = c.verifyMongo(ctx, res, app, name, image, env, s3, scratch, tmpBase)
	case EngineNucleus:
		res.Kind = "nucleus"
		err = c.verifyNucleus(ctx, res, app, name, image, env, s3, scratch, tmpBase)
	case EngineGeneric:
		res.Kind = "volume"
		err = c.verifyVolume(ctx, res, app, name, s3, tmpBase)
	default:
		res.Kind = det.Engine
		err = fmt.Errorf("engine %q has no verification method", det.Engine)
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

// verifyEnv stages database environment in the private workspace; values never enter argv.
func (c *Client) verifyEnv(ctx context.Context, env map[string]string, tmpBase string, prefixes ...string) (string, error) {
	var keys []string
	for k := range env {
		for _, p := range prefixes {
			if strings.HasPrefix(k, p) {
				keys = append(keys, k)
				break
			}
		}
	}
	sort.Strings(keys)
	var data strings.Builder
	for _, k := range keys {
		if strings.ContainsAny(env[k], "\r\n\x00") {
			return "", fmt.Errorf("invalid scratch environment value")
		}
		fmt.Fprintf(&data, "%s=%s\n", k, env[k])
	}
	path := tmpBase + ".env"
	if err := c.exec.Upload(ctx, strings.NewReader(data.String()), path, "0600"); err != nil {
		return "", err
	}
	return "--label teploy.verify.owner=" + ssh.ShellQuote(env["TEPLOY_VERIFY_OWNER"]) + " --env-file " + ssh.ShellQuote(path), nil
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

func (c *Client) verifyPostgres(ctx context.Context, res *VerifyResult, app, name, image string, env map[string]string, s3 S3Config, scratch, tmpBase, latestFile string) error {
	db, user := postgresDBAndUser(app, env)
	prefix := fmt.Sprintf("s3://%s/%s/accessories/%s/", s3.Base(), app, name)
	var s3Key, ext string
	switch {
	case strings.HasSuffix(latestFile, ".pgdump"):
		s3Key, ext = prefix+latestFile, ".pgdump"
	case strings.HasSuffix(latestFile, ".sql.gz"):
		s3Key, ext = prefix+latestFile, ".sql.gz"
	default:
		var err error
		if s3Key, ext, err = c.resolveAccessoryKey(ctx, s3, prefix, res.Date, ".pgdump", ".sql.gz"); err != nil {
			return err
		}
	}
	res.S3Key = s3Key
	dump := tmpBase + ext
	if err := c.download(ctx, s3Key, dump, s3); err != nil {
		return err
	}
	// The cluster's roles (pg_dumpall --globals-only, stored beside the
	// dump) let the scratch restore keep real ownership. Older backups
	// have none.
	globals := tmpBase + ".globals.sql.gz"
	haveGlobals := c.download(ctx, prefix+"globals/"+res.Date+".sql.gz", globals, s3) == nil

	flags, err := c.verifyEnv(ctx, env, tmpBase, "POSTGRES_", "POSTGRESQL_", "ALLOW_EMPTY_PASSWORD")
	if err != nil {
		return err
	}
	sup := pgTarget{container: scratch, user: postgresBootstrapUser(env), db: db}
	if _, ok := env["POSTGRES_PASSWORD"]; !ok && env["BITNAMI_APP_NAME"] == "" {
		// Source ran without auth config (trust); scratch images require one.
		flags += " -e POSTGRES_HOST_AUTH_METHOD=trust"
	}
	if env["BITNAMI_APP_NAME"] != "" {
		// Bitnami's superuser has no password unless given one, and its
		// local socket is not trust: give the throwaway scratch one.
		sup.pwEnv = "POSTGRESQL_POSTGRES_PASSWORD"
		flags += " -e POSTGRESQL_POSTGRES_PASSWORD=teploy-scratch"
	}
	fmt.Fprintf(c.out, "Starting scratch %s...\n", scratch)
	if _, err := c.exec.Run(ctx, fmt.Sprintf(
		"docker run -d --name %s %s %s >/dev/null", ssh.ShellQuote(scratch), flags, ssh.ShellQuote(image))); err != nil {
		return fmt.Errorf("starting scratch container: %w", err)
	}
	if err := c.waitReady(ctx, scratch, postgresReadyProbe(sup.user), 30); err != nil {
		return err
	}

	// Set the scratch cluster up as the source was, then restore as the
	// accessory's OWN role through the same swap restore an in-place
	// restore runs. A gentler scratch restore (superuser, --no-owner)
	// passed backups the real restore could not load.
	if haveGlobals {
		c.applyPostgresGlobals(ctx, sup, globals)
	}
	if user != sup.user {
		ensure := "DO $$BEGIN IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = " + pgLiteral(user) + ") THEN CREATE ROLE " + pgIdent(user) + " LOGIN CREATEDB; END IF; END$$"
		if _, err := c.exec.Run(ctx, sup.psql("postgres", ensure)); err != nil {
			return fmt.Errorf("preparing the scratch role %s: %w", user, err)
		}
	}
	if err := c.pgDropDB(ctx, sup, db, 0); err != nil {
		return fmt.Errorf("preparing the scratch database: %w", err)
	}
	fmt.Fprintf(c.out, "Restoring into scratch as %s...\n", user)
	if _, err := c.restorePostgres(ctx, newPGTarget(scratch, db, user, env), dump, ext, path.Dir(tmpBase), pgRestoreOpts{logicalBytes: c.pgRecordedSize(ctx, s3, app, name, res.Date, path.Dir(tmpBase))}); err != nil {
		return fmt.Errorf("restore into scratch failed: %w", err)
	}

	out, err := c.exec.Run(ctx, sup.psql(db, "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema NOT IN ('pg_catalog','information_schema') AND table_schema NOT LIKE '\\_timescaledb%' AND table_schema NOT LIKE 'timescaledb\\_%' AND table_type = 'BASE TABLE'"))
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

func (c *Client) verifyMySQL(ctx context.Context, res *VerifyResult, app, name, engine, image string, env map[string]string, s3 S3Config, scratch, tmpBase string) error {
	db := mysqlDB(app, env)
	s3Key := fmt.Sprintf("s3://%s/%s/accessories/%s/%s.sql.gz", s3.Base(), app, name, res.Date)
	res.S3Key = s3Key
	dump := tmpBase + ".sql.gz"
	if err := c.download(ctx, s3Key, dump, s3); err != nil {
		return err
	}

	flags, err := c.verifyEnv(ctx, env, tmpBase, "MYSQL_", "MARIADB_")
	if err != nil {
		return err
	}
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
	client, _ := mysqlTools(engine)
	prelude := `MYSQL_PWD=${MYSQL_ROOT_PASSWORD:-${MARIADB_ROOT_PASSWORD:-}}; export MYSQL_PWD; `
	if err := c.waitReady(ctx, scratch, "sh -c "+ssh.ShellQuote(prelude+"exec "+client+" --protocol=TCP -h 127.0.0.1 -u root -e 'SELECT 1'"), 40); err != nil {
		return err
	}
	restore := prelude + "exec " + client + " -u root " + ssh.ShellQuote(db)
	if _, err := c.exec.Run(ctx, "gunzip -c "+ssh.ShellQuote(dump)+" > "+ssh.ShellQuote(tmpBase+".sql")+" && docker exec -i "+ssh.ShellQuote(scratch)+" sh -c "+ssh.ShellQuote(restore)+" < "+ssh.ShellQuote(tmpBase+".sql")); err != nil {
		return fmt.Errorf("restore into scratch failed: %w", err)
	}
	query := prelude + "exec " + client + " -u root -N -e " + ssh.ShellQuote("SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = '"+strings.ReplaceAll(db, "'", "''")+"'")
	out, err := c.exec.Run(ctx, "docker exec "+ssh.ShellQuote(scratch)+" sh -c "+ssh.ShellQuote(query))
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

func (c *Client) verifyRedis(ctx context.Context, res *VerifyResult, app, name, engine, image string, env map[string]string, s3 S3Config, scratch, tmpBase string) error {
	cli := redisCLI(engine)
	s3Key := fmt.Sprintf("s3://%s/%s/accessories/%s/%s.rdb.gz", s3.Base(), app, name, res.Date)
	res.S3Key = s3Key
	dump := tmpBase + ".rdb.gz"
	if err := c.download(ctx, s3Key, dump, s3); err != nil {
		return err
	}

	fmt.Fprintf(c.out, "Starting scratch %s with restored dump.rdb...\n", scratch)
	// Create stopped, seed the rdb, then start — so redis boots FROM the
	// backup (a corrupt rdb fails the readiness probe).
	cmd := fmt.Sprintf(
		"gunzip -c %s > %s.rdb && docker create --name %s --label teploy.verify.owner=%s %s >/dev/null && docker cp %s.rdb %s:/data/dump.rdb && docker start %s >/dev/null",
		ssh.ShellQuote(dump), ssh.ShellQuote(tmpBase),
		ssh.ShellQuote(scratch), ssh.ShellQuote(env["TEPLOY_VERIFY_OWNER"]), ssh.ShellQuote(image),
		ssh.ShellQuote(tmpBase), ssh.ShellQuote(scratch), ssh.ShellQuote(scratch))
	if _, err := c.exec.Run(ctx, cmd); err != nil {
		return fmt.Errorf("seeding scratch redis: %w", err)
	}
	if err := c.waitReady(ctx, scratch, cli+" ping", 20); err != nil {
		return err
	}
	out, err := c.exec.Run(ctx, fmt.Sprintf("docker exec %s %s dbsize", ssh.ShellQuote(scratch), cli))
	if err != nil {
		return fmt.Errorf("verify query failed: %w", err)
	}
	res.Metric = "keys=" + strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(out), "(integer)"))
	return nil
}

// verifyDragonfly boots a scratch dragonfly and loads the snapshot with
// DFLY LOAD (the same mechanism the in-place restore uses).
func (c *Client) verifyDragonfly(ctx context.Context, res *VerifyResult, app, name, image string, env map[string]string, s3 S3Config, scratch, tmpBase string) error {
	s3Key := fmt.Sprintf("s3://%s/%s/accessories/%s/%s.rdb.gz", s3.Base(), app, name, res.Date)
	res.S3Key = s3Key
	dump := tmpBase + ".rdb.gz"
	if err := c.download(ctx, s3Key, dump, s3); err != nil {
		return err
	}
	if _, err := c.exec.Run(ctx, "gunzip -c "+ssh.ShellQuote(dump)+" > "+ssh.ShellQuote(tmpBase+".rdb")); err != nil {
		return fmt.Errorf("decompressing the snapshot: %w", err)
	}
	fmt.Fprintf(c.out, "Starting scratch %s...\n", scratch)
	if _, err := c.exec.Run(ctx, fmt.Sprintf("docker run -d --name %s --label teploy.verify.owner=%s %s --proactor_threads=2 >/dev/null",
		ssh.ShellQuote(scratch), ssh.ShellQuote(env["TEPLOY_VERIFY_OWNER"]), ssh.ShellQuote(image))); err != nil {
		return fmt.Errorf("starting scratch container: %w", err)
	}
	keys, err := c.dragonflyScratchLoad(ctx, scratch, tmpBase+".rdb")
	if err != nil {
		return err
	}
	res.Metric = "keys=" + keys
	return nil
}

func (c *Client) verifyMongo(ctx context.Context, res *VerifyResult, app, name, image string, env map[string]string, s3 S3Config, scratch, tmpBase string) error {
	s3Key := fmt.Sprintf("s3://%s/%s/accessories/%s/%s.archive.gz", s3.Base(), app, name, res.Date)
	res.S3Key = s3Key
	dump := tmpBase + ".archive.gz"
	if err := c.download(ctx, s3Key, dump, s3); err != nil {
		return err
	}

	flags, err := c.verifyEnv(ctx, env, tmpBase, "MONGO_")
	if err != nil {
		return err
	}
	fmt.Fprintf(c.out, "Starting scratch %s...\n", scratch)
	if _, err := c.exec.Run(ctx, fmt.Sprintf(
		"docker run -d --name %s %s %s >/dev/null",
		ssh.ShellQuote(scratch), flags, ssh.ShellQuote(image))); err != nil {
		return fmt.Errorf("starting scratch container: %w", err)
	}
	// mongosh on 5.x+, legacy mongo shell before that.
	probe := "sh -c 'mongosh --quiet --eval 1 || mongo --quiet --eval 1'"
	if err := c.waitReady(ctx, scratch, probe, 30); err != nil {
		return err
	}

	fmt.Fprintf(c.out, "Restoring into scratch...\n")
	if _, err := c.exec.Run(ctx, fmt.Sprintf(
		"docker exec -i %s mongorestore --archive --gzip --quiet --nsExclude=%s < %s",
		ssh.ShellQuote(scratch), ssh.ShellQuote(mongoCheckExclude), ssh.ShellQuote(dump))); err != nil {
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
	s3Key := fmt.Sprintf("s3://%s/%s/accessories/%s/%s.tar.gz", s3.Base(), app, name, res.Date)
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

	flags, err := c.verifyEnv(ctx, env, tmpBase, "NUCLEUS_")
	if err != nil {
		return err
	}
	fmt.Fprintf(c.out, "Booting scratch %s on the restored data dir...\n", scratch)
	if _, err := c.exec.Run(ctx, fmt.Sprintf(
		"docker run -d --name %s %s -v %s/nucleus-data:/data %s >/dev/null",
		ssh.ShellQuote(scratch), flags, ssh.ShellQuote(extractDir), ssh.ShellQuote(image))); err != nil {
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
	s3Key := fmt.Sprintf("s3://%s/%s/accessories/%s/%s.tar.gz", s3.Base(), app, name, res.Date)
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
