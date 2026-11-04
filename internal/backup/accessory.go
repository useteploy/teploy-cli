package backup

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/useteploy/teploy/internal/ssh"
)

// AccessoryBackup performs a database-aware backup for an accessory.
//
// All intermediate artifacts (dumps, snapshots, scratch tars) are built
// inside one private 0700 mktemp workspace and removed on every exit — the
// old predictable /tmp/<app>-… paths were shared across accessories of the
// same app and exposed database dumps to other local users (audit F34).
func (c *Client) AccessoryBackup(ctx context.Context, app, name, image string, env map[string]string, s3 S3Config) (res BackupResult, err error) {
	start := time.Now()
	defer func() { res.DurationMs = time.Since(start).Milliseconds() }()
	if err := c.ensureAWSCLI(ctx); err != nil {
		return res, err
	}
	det, err := c.ResolveEngine(ctx, app, name, image, env)
	if err != nil {
		return res, err
	}
	res.Engine = det.Engine

	timestamp, err := newBackupID(time.Now())
	if err != nil {
		return res, err
	}
	res.ID = timestamp
	containerName := app + "-" + name

	runOut, err := c.exec.Run(ctx, "umask 077; mktemp -d /tmp/teploy-backup.XXXXXXXX")
	if err != nil {
		return res, fmt.Errorf("creating backup workspace: %w", err)
	}
	workDir := strings.TrimSpace(runOut)
	cleanup := func() {
		c.exec.Run(context.WithoutCancel(ctx), "rm -rf "+ssh.ShellQuote(workDir))
	}

	// Consistency classification + the dump command come from the shared
	// planner (also used by the DR bundle path) so the two snapshot paths
	// can never drift apart.
	plan, err := planAccessoryDump(app, name, det, env, workDir)
	if err != nil {
		cleanup()
		return res, err
	}
	res.Consistency = plan.consistency
	s3Key := fmt.Sprintf("s3://%s/%s/accessories/%s/%s%s", s3.Base(), app, name, timestamp, plan.ext)
	res.Key = s3Key
	if err := plan.stage(ctx, c.exec); err != nil {
		cleanup()
		return res, fmt.Errorf("staging the mysql credential file for %s (no dump was run): %w", name, err)
	}
	fmt.Fprintf(c.out, "Backing up %s (%s, detected from %s)...\n", containerName, det.Engine, det.Source)
	if _, err := c.exec.Run(ctx, plan.cmd); err != nil {
		// Never leave partial backups around: they're disk-fillers at best,
		// restore-bait at worst.
		cleanup()
		return res, fmt.Errorf("dumping %s: %w", name, err)
	}
	size, err := c.checkArtifactSize(ctx, plan.artifactPath, plan.minBytes)
	res.SizeBytes = size
	if err != nil {
		cleanup()
		return res, fmt.Errorf("dumping %s: %w", name, err)
	}

	fmt.Fprintf(c.out, "Uploading to %s...\n", s3Key)
	if _, err := c.exec.Run(ctx, s3.AWS(fmt.Sprintf("s3 cp %s %s", ssh.ShellQuote(plan.artifactPath), ssh.ShellQuote(s3Key)))); err != nil {
		cleanup()
		return res, fmt.Errorf("uploading to S3: %w", err)
	}

	cleanup()
	fmt.Fprintf(c.out, "Backup complete: %s\n", s3Key)
	return res, nil
}

// checkArtifactSize measures a freshly written artifact and refuses one
// below the engine's plausibility floor. A wrong password or a dump tool
// that errored to stderr used to leave a 20-byte gzip of an empty stream
// that every later step accepted as a backup (U11 on deploy-ovh: fourteen
// of them).
func (c *Client) checkArtifactSize(ctx context.Context, path string, minBytes int64) (int64, error) {
	out, err := c.exec.Run(ctx, "wc -c < "+ssh.ShellQuote(path))
	if err != nil {
		return 0, fmt.Errorf("measuring the backup artifact: %w", err)
	}
	size, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("measuring the backup artifact: unreadable size %q", strings.TrimSpace(out))
	}
	if size < minBytes {
		return size, fmt.Errorf("backup artifact is implausibly small (%d bytes, minimum %d) — the dump almost certainly failed (wrong credentials, empty output); refusing to store it", size, minBytes)
	}
	return size, nil
}

// AccessoryRestore restores a database backup from S3.
func (c *Client) AccessoryRestore(ctx context.Context, app, name, image, date string, env map[string]string, s3 S3Config) error {
	recoveryDir, err := c.AccessoryRestoreKeep(ctx, app, name, image, date, env, s3)
	if err == nil && recoveryDir != "" {
		c.exec.Run(context.WithoutCancel(ctx), "rm -rf "+ssh.ShellQuote(recoveryDir))
	}
	return err
}

// RestoreStopsContainer reports whether restoring this engine replaces the
// accessory's files on disk (so the container must be stopped first). The
// engine branches restore through the running engine instead; redis and
// valkey stop and restart themselves inside their restore script.
func RestoreStopsContainer(engine string) bool {
	return engine == EngineGeneric || engine == EngineNucleus
}

// AccessoryRestoreKeep is AccessoryRestore where the caller owns the
// recovery directory — see RestoreVolumesKeep. For postgres and dragonfly it
// holds the safety snapshot of the database as it was just before the
// restore; for file-tree engines it holds the previous files.
func (c *Client) AccessoryRestoreKeep(ctx context.Context, app, name, image, date string, env map[string]string, s3 S3Config) (string, error) {
	if err := ValidateDate(date); err != nil {
		return "", err
	}
	if err := c.ensureAWSCLI(ctx); err != nil {
		return "", err
	}
	det, err := c.ResolveEngine(ctx, app, name, image, env)
	if err != nil {
		return "", err
	}
	container := app + "-" + name
	accPrefix := fmt.Sprintf("s3://%s/%s/accessories/%s/", s3.Base(), app, name)

	// All restore scratch files live under one per-invocation temp dir. The
	// old fixed names (/tmp/restore.sql.gz, …, and the generic branch's
	// /tmp/<app>-<name>-restore-stage) were shared by every restore: two
	// concurrent restores (or a crash mid-restore followed by a retry)
	// could read or stage the previous run's files. On failure the dir is
	// kept and named in the error for inspection; on success it is removed.
	tmpOut, err := c.exec.Run(ctx, "mktemp -d "+ssh.ShellQuote("/tmp/teploy-restore.XXXXXX"))
	if err != nil {
		return "", fmt.Errorf("creating restore temp dir: %w", err)
	}
	tmpdir := strings.TrimSpace(tmpOut)
	keepTmp := func(err error) (string, error) {
		return "", fmt.Errorf("%w — restore files kept in %s", err, tmpdir)
	}
	download := func(key, dest string) error {
		fmt.Fprintf(c.out, "Downloading %s...\n", key)
		if _, err := c.exec.Run(ctx, s3.AWS(fmt.Sprintf("s3 cp %s %s", ssh.ShellQuote(key), ssh.ShellQuote(dest)))); err != nil {
			return fmt.Errorf("downloading backup: %w", err)
		}
		return nil
	}
	done := func(recovery string) (string, error) {
		c.exec.Run(ctx, "rm -rf "+ssh.ShellQuote(tmpdir))
		fmt.Fprintln(c.out, "Restore complete")
		return recovery, nil
	}

	switch det.Engine {
	case EnginePostgres:
		key, ext, err := c.resolveAccessoryKey(ctx, s3, accPrefix, date, ".pgdump", ".sql.gz")
		if err != nil {
			return keepTmp(err)
		}
		artifact := tmpdir + "/restore" + ext
		if err := download(key, artifact); err != nil {
			return keepTmp(err)
		}
		db, user := postgresDBAndUser(app, env)
		fmt.Fprintf(c.out, "Restoring %s (postgres database %s)...\n", name, db)
		recovery, err := c.restorePostgresInPlace(ctx, pgTarget{container: container, user: user, db: db}, artifact, ext, tmpdir)
		if err != nil {
			if recovery != "" {
				return recovery, fmt.Errorf("restoring %s: %w — restore files kept in %s", name, err, tmpdir)
			}
			return keepTmp(fmt.Errorf("restoring %s: %w", name, err))
		}
		return done(recovery)

	case EngineDragonfly:
		key := accPrefix + date + ".rdb.gz"
		artifact := tmpdir + "/restore.rdb.gz"
		if err := download(key, artifact); err != nil {
			return keepTmp(err)
		}
		recOut, err := c.exec.Run(ctx, "umask 077; mktemp -d /var/tmp/teploy-pre-restore.XXXXXX")
		if err != nil {
			return keepTmp(fmt.Errorf("creating the pre-restore snapshot directory: %w", err))
		}
		recovery := strings.TrimSpace(recOut)
		fmt.Fprintf(c.out, "Restoring %s (dragonfly)...\n", name)
		if _, err := c.exec.Run(ctx, dragonflyLoadScript(container, artifact, tmpdir, recovery+"/pre-restore.rdb")); err != nil {
			return recovery, fmt.Errorf("restoring %s: %w — the pre-restore snapshot (when taken) is at %s/pre-restore.rdb, restore files kept in %s", name, err, recovery, tmpdir)
		}
		return done(recovery)
	}

	// Determine file type and restore command based on DB type. The file
	// branches (generic, nucleus) leave restoreCmd empty and instead set
	// accDir, since they go through the staging helper below rather than a
	// single shell command.
	var s3Key, restorePath, restoreCmd, accDir string

	switch det.Engine {
	case EngineMySQL, EngineMariaDB:
		// The old branch never assigned s3Key/restorePath, so the download
		// and gunzip below operated on empty arguments — every MySQL/MariaDB
		// restore was deterministically broken (audit F31).
		s3Key = accPrefix + date + ".sql.gz"
		restorePath = tmpdir + "/restore.sql.gz"
		qContainer := ssh.ShellQuote(container)
		db := mysqlDB(app, env)
		// Same 0600 env-file credential transport as the backup path
		// (see there) — the password rides in no argv. The restore
		// failure path DELIBERATELY keeps tmpdir for inspection, so the
		// credential file must be removed by name there: keep the SQL,
		// never the secret (C08).
		client, _ := mysqlTools(det.Engine)
		sqlPath := tmpdir + "/restore.sql"
		receiver := "docker exec -i " + qContainer + " " + client + " -u root " + ssh.ShellQuote(db) + " < " + ssh.ShellQuote(sqlPath)
		if pwd := mysqlRootPassword(env); pwd != "" {
			envFile := tmpdir + "/mysql.env"
			if err := c.exec.Upload(ctx, strings.NewReader("MYSQL_PWD="+pwd+"\n"), envFile, "0600"); err != nil {
				return keepTmp(err)
			}
			receiver = pipefail(
				"cat "+ssh.ShellQuote(envFile)+" && cat "+ssh.ShellQuote(sqlPath),
				"docker exec -i "+qContainer+" sh -c "+ssh.ShellQuote(mysqlPasswordPrelude+"exec "+client+" -u root "+ssh.ShellQuote(db)),
				tmpdir+"/producer.failed")
		}
		restoreCmd = "gunzip -c " + ssh.ShellQuote(restorePath) + " > " + ssh.ShellQuote(sqlPath) + " && " + receiver

		// The kept-on-failure scratch dir must never keep the credential.
		innerKeep := keepTmp
		keepTmp = func(err error) (string, error) {
			c.exec.Run(context.WithoutCancel(ctx), "rm -f "+ssh.ShellQuote(tmpdir+"/mysql.env"))
			return innerKeep(err)
		}
	case EngineMongo:
		s3Key = accPrefix + date + ".archive.gz"
		restorePath = tmpdir + "/restore.archive.gz"
		restoreCmd = mongoRestoreCmd(container, restorePath)
	case EngineRedis, EngineValkey:
		// AccessoryBackup stores redis as <date>.rdb.gz; without this case
		// the default branch looked for a .tar.gz that doesn't exist, so
		// redis restores always failed. Stop redis first so its shutdown
		// save can't overwrite the snapshot we copy in, then start so it
		// loads dump.rdb (full reasoning on redisRestoreScript).
		s3Key = accPrefix + date + ".rdb.gz"
		restorePath = tmpdir + "/restore.rdb.gz"
		if err := c.redisAOFPreflight(ctx, det.Engine, app, name); err != nil {
			return keepTmp(err)
		}
		restoreCmd = redisRestoreScript(container, restorePath, tmpdir+"/restore.rdb", tmpdir+"/old-dump.rdb")
	case EngineGeneric, EngineNucleus:
		// File engines: extract the tar into the accessory directory
		// through the staging helper.
		s3Key = accPrefix + date + ".tar.gz"
		restorePath = tmpdir + "/restore.tar.gz"
		accDir = fmt.Sprintf("%s/%s/accessories/%s", deploymentsDir, app, name)
	default:
		return keepTmp(fmt.Errorf("engine %q has no restore path", det.Engine))
	}

	// Every engine branch must produce a complete artifact specification —
	// an empty s3Key/restorePath means a branch forgot its convention and
	// would otherwise download from/gunzip an empty string (the class of
	// bug F31 fixed for MySQL).
	if s3Key == "" || restorePath == "" {
		return keepTmp(fmt.Errorf("internal error: incomplete backup artifact specification for %s (image %s)", name, image))
	}

	if err := download(s3Key, restorePath); err != nil {
		return keepTmp(err)
	}

	fmt.Fprintf(c.out, "Restoring %s...\n", name)
	var fileRecovery string
	if accDir != "" {
		stageDir := tmpdir + "/restore-stage"
		recoveryDir, err := extractToStagingThenPromote(ctx, c.exec, restorePath, stageDir, accDir, c.out, nil)
		if err != nil {
			// Recovery-incomplete failures retain their artifacts in
			// tmpdir/stage — keepTmp's contract already preserves tmpdir,
			// so the retained paths stay inspectable (TCL-43).
			return keepTmp(fmt.Errorf("restoring %s: %w", name, err))
		}
		// The recovery directory is returned to the caller (the
		// AccessoryRestore wrapper releases it once the whole command
		// succeeds); the staged tree in tmpdir is spent either way.
		fileRecovery = recoveryDir
	} else if _, err := c.exec.Run(ctx, restoreCmd); err != nil {
		return keepTmp(fmt.Errorf("restoring %s: %w", name, err))
	}
	return done(fileRecovery)
}

// resolveAccessoryKey finds the stored object for backup date among the
// extensions an engine has used over time (postgres: custom-format
// .pgdump since G7, plain .sql.gz before), by listing the date's prefix.
func (c *Client) resolveAccessoryKey(ctx context.Context, s3 S3Config, prefix, date string, exts ...string) (key, ext string, err error) {
	out, lsErr := c.exec.Run(ctx, s3.AWS("s3 ls "+ssh.ShellQuote(prefix+date+".")))
	names := map[string]bool{}
	if lsErr == nil {
		for _, line := range strings.Split(out, "\n") {
			if f := strings.Fields(line); len(f) > 0 {
				names[f[len(f)-1]] = true
			}
		}
	}
	for _, e := range exts {
		if names[date+e] {
			return prefix + date + e, e, nil
		}
	}
	detail := ""
	if lsErr != nil {
		detail = fmt.Sprintf(" (listing: %v)", lsErr)
	}
	return "", "", fmt.Errorf("no backup %s found under %s (looked for %s)%s", date, prefix, strings.Join(exts, ", "), detail)
}

// pipefail joins producer | consumer so that a failure of EITHER side fails
// the whole command. POSIX sh reports only the consumer's status (and
// Debian's dash has no `set -o pipefail`), so the producer records its own
// failure in a marker file that is checked after the pipe.
func pipefail(producer, consumer, marker string) string {
	q := ssh.ShellQuote(marker)
	return fmt.Sprintf("rm -f %s; { %s || : > %s; } | %s && [ ! -e %s ]", q, producer, q, consumer, q)
}

// accessoryDumpPlan is the single source of truth for how one accessory is
// snapshotted: the engine classification, the consistency the artifact can
// honestly claim, and the exact server-side command that produces it.
// AccessoryBackup (single-accessory S3 backups) and the DR bundle path both
// build on it, so the two flows cannot drift into different dump semantics.
type accessoryDumpPlan struct {
	engine      string // one of the Engine* constants
	detection   string // EngineDetection.Source
	method      string // pg_dump|mysqldump|mariadb-dump|mongodump|redis-bgsave|dragonfly-save|tar
	consistency string // engine-consistent | engine-snapshot | crash-consistent
	ext         string // .pgdump | .sql.gz | .archive.gz | .rdb.gz | .tar.gz
	// artifactPath is where the plan's command writes its artifact.
	artifactPath string
	cmd          string
	// minBytes is the smallest artifact that can be a real backup of this
	// engine; anything smaller is refused (checkArtifactSize).
	minBytes int64
	// credential, when non-nil, is a secret the plan's command needs via a
	// 0600 env-file (--env-file) rather than argv: the CALLER must upload
	// it before running cmd and ensure it never survives a kept scratch
	// dir (C08 transport contract).
	credential *planCredential
}

// planCredential is the staged env-file contract between the planner and
// its callers: content uploaded at path with mode 0600.
type planCredential struct {
	path    string
	content string
}

// stage uploads the plan's credential file (no-op when the plan carries
// none). Errors are pre-effect: no dump command has run yet.
func (p accessoryDumpPlan) stage(ctx context.Context, exec ssh.Executor) error {
	if p.credential == nil {
		return nil
	}
	return exec.Upload(ctx, strings.NewReader(p.credential.content), p.credential.path, "0600")
}

// Consistency level labels recorded in DR bundle manifests. A raw tar of a
// live database volume is NOT a consistent backup, and these labels are how
// the code refuses to present it as one (C07).
const (
	consistencyEngineDump = "engine-consistent" // engine-native dump tooling (pg_dump, mysqldump, mongodump)
	consistencySnapshot   = "engine-snapshot"   // engine-acknowledged point-in-time file snapshot (redis BGSAVE)
	consistencyCrash      = "crash-consistent"  // raw file copy of a live writer; only crash recovery is guaranteed
	consistencyQuiesced   = "quiesced"          // writer stopped before the copy (operator or teploy did it)
)

// Plausibility floors per artifact (bytes). The structural checks inside
// each command (custom-format magic, mysqldump completion trailer, RDB
// magic) are the precise guard; these floors catch the empty-output class
// even where no structural check exists. Measured against empty databases
// of each engine (see engines_integration_test.go): the smallest real
// artifacts are several times larger.
const (
	minPostgresDump = 200 // pg_dump -Fc of an empty postgres:17 database: 865 B
	minMySQLDump    = 200 // gzip'd mysqldump of an empty mysql:8 database: 468 B
	minMongoDump    = 64
	minRDBSnapshot  = 64 // gzip'd empty redis:7 RDB: 111 B; gzip of nothing: 23 B
	minTarArchive   = 40 // gzip'd tar of one entry
)

// planAccessoryDump builds the command that dumps an accessory into
// workDir for its DETECTED engine. A recognised database never takes the
// raw-tar branch: generic is chosen only when detection found no engine
// (or the teploy.engine=generic label asserts it).
func planAccessoryDump(app, name string, det EngineDetection, env map[string]string, workDir string) (accessoryDumpPlan, error) {
	containerName := app + "-" + name
	qContainer := ssh.ShellQuote(containerName)

	// Dumps are redirected into files with `>`, never piped: a shell
	// pipeline's exit status is its LAST command's (gzip "succeeds"
	// compressing an empty stream even when the dump errored) — that is
	// how a wrong password produced 20-byte "backups".
	dumpPath := workDir + "/dump.out.gz"
	dumpTmp := workDir + "/dump.out"
	base := accessoryDumpPlan{engine: det.Engine, detection: det.Source, artifactPath: dumpPath}

	switch det.Engine {
	case EnginePostgres:
		db, user := postgresDBAndUser(app, env)
		p := ssh.ShellQuote(workDir + "/dump.pgdump")
		base.method, base.consistency, base.ext, base.minBytes = "pg_dump", consistencyEngineDump, ".pgdump", minPostgresDump
		base.artifactPath = workDir + "/dump.pgdump"
		base.cmd = fmt.Sprintf("docker exec %s pg_dump -Fc -U %s %s > %s && { [ \"$(head -c 5 %s)\" = PGDMP ] || { echo 'pg_dump did not produce a custom-format archive' >&2; exit 1; }; }",
			qContainer, ssh.ShellQuote(user), ssh.ShellQuote(db), p, p)
		return base, nil

	case EngineMySQL, EngineMariaDB:
		client := "mysqldump"
		if det.Engine == EngineMariaDB {
			client = "mariadb-dump"
		}
		// A consistent InnoDB snapshot without locking writers, plus the
		// stored routines and triggers a plain dump omits (routines) or a
		// --skip-triggers default elsewhere would drop.
		dumpArgs := client + " --single-transaction --routines --triggers -u root " + ssh.ShellQuote(mysqlDB(app, env))
		// Root password rides a 0600 env-file consumed by --env-file, never
		// argv (mysqldump argv is visible in `ps` inside the container, and
		// -e MYSQL_PWD puts the secret in the docker CLI's own argv on the
		// host). Absent = current behavior (passwordless root).
		command := "docker exec " + qContainer + " " + dumpArgs
		if pwd := mysqlRootPassword(env); pwd != "" {
			envFile := workDir + "/mysql.env"
			base.credential = &planCredential{path: envFile, content: "MYSQL_PWD=" + pwd + "\n"}
			command = "docker exec -i " + qContainer + " sh -c " + ssh.ShellQuote(mysqlPasswordPrelude+"exec "+dumpArgs) + " < " + ssh.ShellQuote(envFile)
		}
		qTmp := ssh.ShellQuote(dumpTmp)
		base.method, base.consistency, base.ext, base.minBytes = client, consistencyEngineDump, ".sql.gz", minMySQLDump
		base.cmd = command + " > " + qTmp +
			" && { tail -c 512 " + qTmp + " | grep -q 'Dump completed' || { echo '" + client + " output has no completion trailer (truncated or failed dump)' >&2; exit 1; }; }" +
			" && gzip -c " + qTmp + " > " + ssh.ShellQuote(dumpPath)
		return base, nil

	case EngineMongo:
		base.method, base.consistency, base.ext, base.minBytes = "mongodump", consistencyEngineDump, ".archive.gz", minMongoDump
		base.cmd = fmt.Sprintf("docker exec %s mongodump --archive --gzip > %s", qContainer, ssh.ShellQuote(dumpPath))
		return base, nil

	case EngineRedis, EngineValkey:
		// Trigger BGSAVE, wait for an ACKNOWLEDGED new save, then copy
		// dump.rdb. The script fails closed: BGSAVE refusal (other than an
		// already-running save, whose completion still moves LASTSAVE), a
		// poll timeout, a failed copy or a non-RDB file all abort before
		// an artifact is produced (audit F36, TCL-42, A40).
		cli := redisCLI(det.Engine)
		redisTmp := workDir + "/dump.rdb"
		base.method, base.consistency, base.ext, base.minBytes = "redis-bgsave", consistencySnapshot, ".rdb.gz", minRDBSnapshot
		base.cmd = strings.Join([]string{
			"set -eu",
			// AOF-enabled Redis persists to the append-only file; backing
			// up only dump.rdb captures a stale or empty dataset — the
			// reply must be a proven `appendonly no`.
			fmt.Sprintf("aof=$(docker exec %s %s --raw config get appendonly)", qContainer, cli),
			`set -- $aof`,
			`if [ "${1:-}" != appendonly ] || [ "${2:-}" != no ]; then echo 'cannot confirm ` + det.Engine + ` appendonly=no (got: '"$aof"') — teploy backup captures dump.rdb only; refusing' >&2; exit 1; fi`,
			fmt.Sprintf("ls=$(docker exec %s %s lastsave)", qContainer, cli),
			fmt.Sprintf("bgs=$(docker exec %s %s bgsave 2>&1) || true", qContainer, cli),
			`case "$bgs" in *ERR*) case "$bgs" in *"in progress"*) ;; *) printf 'BGSAVE failed: %s\n' "$bgs" >&2; exit 1;; esac;; esac`,
			"saved=no; i=0",
			`while [ "$i" -lt 60 ]; do`,
			fmt.Sprintf("cur=$(docker exec %s %s lastsave)", qContainer, cli),
			`if [ "$cur" != "$ls" ]; then saved=yes; break; fi`,
			"sleep 1; i=$((i+1))",
			"done",
			`if [ "$saved" != yes ]; then echo 'timed out waiting for BGSAVE to complete' >&2; exit 1; fi`,
			fmt.Sprintf("docker cp %s:/data/dump.rdb %s", qContainer, ssh.ShellQuote(redisTmp)),
			rdbMagicCheck(redisTmp),
			fmt.Sprintf("gzip -c %s > %s", ssh.ShellQuote(redisTmp), ssh.ShellQuote(dumpPath)),
			fmt.Sprintf("rm -f %s", ssh.ShellQuote(redisTmp)),
		}, "\n")
		return base, nil

	case EngineDragonfly:
		// Dragonfly's BGSAVE writes its own multi-file DF format
		// (dump-<ts>-summary.dfs + shards), never dump.rdb, so the redis
		// branch would copy nothing. `SAVE RDB <name>` writes a single
		// Redis-compatible RDB synchronously into the working dir; it is
		// copied out and removed from the container.
		snap := "teploy-snapshot-" + safeSuffix(workDir)
		rdb := workDir + "/dump.rdb"
		base.method, base.consistency, base.ext, base.minBytes = "dragonfly-save-rdb", consistencySnapshot, ".rdb.gz", minRDBSnapshot
		base.cmd = strings.Join([]string{
			"set -eu",
			dragonflyDirPrelude(qContainer),
			fmt.Sprintf("out=$(docker exec %s redis-cli save rdb %s 2>&1) || true", qContainer, snap),
			`if [ "$out" != OK ]; then printf 'dragonfly SAVE RDB failed: %s\n' "$out" >&2; exit 1; fi`,
			fmt.Sprintf(`cp_ok=yes; docker cp %s:"$dir"/%s.rdb %s || cp_ok=no`, qContainer, snap, ssh.ShellQuote(rdb)),
			fmt.Sprintf(`docker exec -u 0 %s rm -f "$dir"/%s.rdb || true`, qContainer, snap),
			`if [ "$cp_ok" != yes ]; then echo 'copying the dragonfly snapshot out of the container failed' >&2; exit 1; fi`,
			rdbMagicCheck(rdb),
			fmt.Sprintf("gzip -c %s > %s", ssh.ShellQuote(rdb), ssh.ShellQuote(dumpPath)),
			fmt.Sprintf("rm -f %s", ssh.ShellQuote(rdb)),
		}, "\n")
		return base, nil

	case EngineNucleus, EngineGeneric:
		// File engines: tar the accessory directory. For Nucleus (and any
		// live writer) this is a crash-consistent snapshot by design —
		// Nucleus's recovery is built for it (torn-tail truncation + CRC
		// skip) and verify-backup boots a scratch engine on the result.
		// GNU tar exits 1 with "file changed" warnings when a WAL rotates
		// mid-read, so exit 1 is tolerated; real failures exit 2. The
		// generic branch is never chosen for a detected database (G6).
		accDir := fmt.Sprintf("%s/%s/accessories/%s", deploymentsDir, app, name)
		base.method, base.consistency, base.ext, base.minBytes = "tar", consistencyCrash, ".tar.gz", minTarArchive
		base.artifactPath = workDir + "/dump.tar.gz"
		base.cmd = fmt.Sprintf("tar -czf %s -C %s . || [ $? -eq 1 ]", ssh.ShellQuote(workDir+"/dump.tar.gz"), ssh.ShellQuote(accDir))
		return base, nil
	}
	return accessoryDumpPlan{}, fmt.Errorf("accessory %s: engine %q has no backup method — refusing to fall back to a raw file copy of a database", name, det.Engine)
}

// safeSuffix derives a filename-safe unique token from a mktemp path.
func safeSuffix(p string) string {
	b := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' {
			return r
		}
		return '-'
	}, path.Base(p))
	return strings.Trim(b, "-")
}

// rdbMagicCheck refuses a file that is not an RDB snapshot (Redis and
// Dragonfly write "REDIS", Valkey 9+ "VALKEY").
func rdbMagicCheck(p string) string {
	return fmt.Sprintf(`case "$(head -c 5 %s)" in REDIS|VALKE) ;; *) echo 'snapshot is not an RDB file' >&2; exit 1;; esac`, ssh.ShellQuote(p))
}

// dragonflyDirPrelude sets $dir to the dragonfly snapshot directory:
// `CONFIG GET dir`, else the container's working directory (the image
// default is /data).
func dragonflyDirPrelude(qContainer string) string {
	return fmt.Sprintf(`dir=$(docker exec %s redis-cli --raw config get dir | sed -n 2p); [ -n "$dir" ] || dir=$(docker inspect -f '{{.Config.WorkingDir}}' %s); [ -n "$dir" ] || dir=/data`, qContainer, qContainer)
}

// dragonflyLoadScript restores a dragonfly snapshot ONLINE: the current
// dataset is saved as a pre-restore RDB first (copied to preRestore),
// the backup is copied into the snapshot dir and loaded with DFLY LOAD
// (which replaces the dataset), and a normal SAVE then persists the
// restored state in dragonfly's own format so a restart cannot bring the
// old snapshot back. A failed load reloads the pre-restore snapshot.
func dragonflyLoadScript(container, artifactGz, workDir, preRestore string) string {
	q := ssh.ShellQuote(container)
	rdb := workDir + "/restore.rdb"
	tag := safeSuffix(workDir)
	in, pre := "teploy-restore-"+tag+".rdb", "teploy-pre-restore-"+tag+".rdb"
	return strings.Join([]string{
		"set -eu",
		fmt.Sprintf("gunzip -c %s > %s", ssh.ShellQuote(artifactGz), ssh.ShellQuote(rdb)),
		rdbMagicCheck(rdb),
		dragonflyDirPrelude(q),
		fmt.Sprintf(`out=$(docker exec %s redis-cli save rdb %s 2>&1) || true`, q, strings.TrimSuffix(pre, ".rdb")),
		`if [ "$out" != OK ]; then printf 'pre-restore snapshot failed (dataset untouched): %s\n' "$out" >&2; exit 1; fi`,
		fmt.Sprintf(`docker cp %s:"$dir"/%s %s`, q, pre, ssh.ShellQuote(preRestore)),
		fmt.Sprintf(`docker cp %s %s:"$dir"/%s`, ssh.ShellQuote(rdb), q, in),
		// docker cp keeps the host uid; the engine must be able to read it.
		fmt.Sprintf(`docker exec -u 0 %s chmod 644 "$dir"/%s`, q, in),
		fmt.Sprintf(`out=$(docker exec %s redis-cli dfly load %s 2>&1) || true`, q, in),
		`if [ "$out" != OK ]; then`,
		fmt.Sprintf(`  back=$(docker exec %s redis-cli dfly load %s 2>&1) || true`, q, pre),
		fmt.Sprintf(`  docker exec -u 0 %s rm -f "$dir"/%s "$dir"/%s || true`, q, in, pre),
		`  if [ "$back" = OK ]; then printf 'DFLY LOAD of the backup failed (%s); the pre-restore dataset was reloaded\n' "$out" >&2; else printf 'DFLY LOAD of the backup failed (%s) and reloading the pre-restore snapshot failed too (%s)\n' "$out" "$back" >&2; fi`,
		`  exit 1`,
		`fi`,
		fmt.Sprintf(`docker exec -u 0 %s rm -f "$dir"/%s "$dir"/%s`, q, in, pre),
		fmt.Sprintf(`out=$(docker exec %s redis-cli save 2>&1) || true`, q),
		`if [ "$out" != OK ]; then printf 'restored, but persisting the restored dataset failed: %s\n' "$out" >&2; exit 1; fi`,
		fmt.Sprintf("rm -f %s", ssh.ShellQuote(rdb)),
	}, "\n")
}

// redisCLI is the engine's command-line client.
func redisCLI(engine string) string {
	if engine == EngineValkey {
		return "valkey-cli"
	}
	return "redis-cli"
}

// ---- postgres ----

// pgTarget is one database inside a postgres container.
type pgTarget struct {
	container, user, db string
}

// maintDB is the database the DROP/CREATE runs from: never the target.
func (t pgTarget) maintDB() string {
	if t.db == "postgres" {
		return "template1"
	}
	return "postgres"
}

func (t pgTarget) psql(db string, sqls ...string) string {
	cmd := "docker exec " + ssh.ShellQuote(t.container) + " psql -X -qtA -v ON_ERROR_STOP=1 -U " + ssh.ShellQuote(t.user) + " -d " + ssh.ShellQuote(db)
	for _, s := range sqls {
		cmd += " -c " + ssh.ShellQuote(s)
	}
	return cmd
}

func pgIdent(s string) string   { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
func pgLiteral(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// pgDBProps is what CREATE DATABASE needs to recreate a database as it was.
type pgDBProps struct {
	version                      int
	exists                       bool
	owner, encoding, collate, ct string
}

func (c *Client) pgDatabaseProps(ctx context.Context, t pgTarget) (pgDBProps, error) {
	q := "SELECT current_setting('server_version_num') || coalesce((SELECT '|' || pg_get_userbyid(datdba) || '|' || pg_encoding_to_char(encoding) || '|' || datcollate || '|' || datctype FROM pg_database WHERE datname = " + pgLiteral(t.db) + "), '')"
	out, err := c.exec.Run(ctx, t.psql(t.maintDB(), q))
	if err != nil {
		return pgDBProps{}, fmt.Errorf("reading database %s properties: %w", t.db, err)
	}
	f := strings.Split(strings.TrimSpace(out), "|")
	v, err := strconv.Atoi(f[0])
	if err != nil {
		return pgDBProps{}, fmt.Errorf("reading database %s properties: unexpected reply %q", t.db, strings.TrimSpace(out))
	}
	p := pgDBProps{version: v}
	if len(f) == 5 {
		p.exists, p.owner, p.encoding, p.collate, p.ct = true, f[1], f[2], f[3], f[4]
	}
	return p, nil
}

var timescaleTOC = regexp.MustCompile(`(?m) EXTENSION - timescaledb( |$)`)

// pgValidateArtifact proves the artifact is readable before anything
// destructive runs, and reports whether it carries TimescaleDB (whose
// restore must be bracketed by timescaledb_pre_restore/post_restore).
func (c *Client) pgValidateArtifact(ctx context.Context, t pgTarget, artifact, ext string) (timescale bool, err error) {
	if ext == ".sql.gz" {
		if _, err := c.exec.Run(ctx, "gzip -t "+ssh.ShellQuote(artifact)); err != nil {
			return false, fmt.Errorf("legacy plain dump is corrupt: %w", err)
		}
		return false, nil
	}
	toc, err := c.exec.Run(ctx, "docker exec -i "+ssh.ShellQuote(t.container)+" pg_restore -l < "+ssh.ShellQuote(artifact))
	if err != nil {
		return false, fmt.Errorf("pg_restore cannot read the archive: %w", err)
	}
	return timescaleTOC.MatchString(toc), nil
}

// pgRecreateAndLoad replaces the target database with the artifact's
// contents: DROP (forcing out connections) and CREATE from template0 with
// the original owner/encoding/locale, then pg_restore (or psql for a
// legacy plain dump) in a single transaction. There is no --clean path:
// dropping the database is the only way the result equals the backup
// (objects created after the backup do not survive), and a populated
// database no longer makes the first CREATE fail (U4). scratch relaxes
// ownership/ACLs for verification copies whose roles do not exist.
func (c *Client) pgRecreateAndLoad(ctx context.Context, t pgTarget, props pgDBProps, artifact, ext, workDir string, timescale, scratch bool) error {
	maint := t.maintDB()
	if props.version >= 130000 {
		if _, err := c.exec.Run(ctx, t.psql(maint, "DROP DATABASE IF EXISTS "+pgIdent(t.db)+" WITH (FORCE)")); err != nil {
			return fmt.Errorf("dropping database %s: %w", t.db, err)
		}
	} else {
		terminate := "SELECT count(pg_terminate_backend(pid)) FROM pg_stat_activity WHERE datname = " + pgLiteral(t.db) + " AND pid <> pg_backend_pid()"
		if _, err := c.exec.Run(ctx, t.psql(maint, terminate, "DROP DATABASE IF EXISTS "+pgIdent(t.db))); err != nil {
			return fmt.Errorf("dropping database %s: %w", t.db, err)
		}
	}
	owner := props.owner
	if owner == "" || scratch {
		owner = t.user
	}
	create := "CREATE DATABASE " + pgIdent(t.db) + " OWNER " + pgIdent(owner) + " TEMPLATE template0"
	if props.exists && !scratch {
		create += " ENCODING " + pgLiteral(props.encoding) + " LC_COLLATE " + pgLiteral(props.collate) + " LC_CTYPE " + pgLiteral(props.ct)
	}
	if _, err := c.exec.Run(ctx, t.psql(maint, create)); err != nil {
		return fmt.Errorf("creating database %s: %w", t.db, err)
	}
	if timescale {
		if _, err := c.exec.Run(ctx, t.psql(t.db, "CREATE EXTENSION IF NOT EXISTS timescaledb", "SELECT timescaledb_pre_restore()")); err != nil {
			return fmt.Errorf("preparing timescaledb restore: %w", err)
		}
	}
	q := ssh.ShellQuote
	if ext == ".sql.gz" {
		sqlPath := workDir + "/restore.sql"
		cmd := "gunzip -c " + q(artifact) + " > " + q(sqlPath) + " && docker exec -i " + q(t.container) +
			" psql -X -q -v ON_ERROR_STOP=1 --single-transaction -U " + q(t.user) + " -d " + q(t.db) + " < " + q(sqlPath) + " >/dev/null"
		if _, err := c.exec.Run(ctx, cmd); err != nil {
			return fmt.Errorf("loading the plain dump: %w", err)
		}
	} else {
		flags := " --exit-on-error --single-transaction"
		if scratch {
			flags += " --no-owner --no-acl"
		}
		cmd := "docker exec -i " + q(t.container) + " pg_restore" + flags + " -U " + q(t.user) + " -d " + q(t.db) + " < " + q(artifact)
		if _, err := c.exec.Run(ctx, cmd); err != nil {
			return fmt.Errorf("pg_restore: %w", err)
		}
	}
	if timescale {
		if _, err := c.exec.Run(ctx, t.psql(t.db, "SELECT timescaledb_post_restore()")); err != nil {
			return fmt.Errorf("finishing timescaledb restore: %w", err)
		}
	}
	return nil
}

// restorePostgresInPlace is the G7 in-place sequence (the caller has
// already quiesced the app): validate the artifact, take a safety dump of
// the current database, drop + recreate it, load the backup. A failed load
// reloads the safety dump; when that also fails the error is
// recovery-incomplete (the app must not be restarted against it) and names
// the safety dump. On success the safety dump's directory is returned as
// the caller-owned recovery directory.
func (c *Client) restorePostgresInPlace(ctx context.Context, t pgTarget, artifact, ext, workDir string) (string, error) {
	timescale, err := c.pgValidateArtifact(ctx, t, artifact, ext)
	if err != nil {
		return "", fmt.Errorf("backup artifact failed validation (database untouched): %w", err)
	}
	props, err := c.pgDatabaseProps(ctx, t)
	if err != nil {
		return "", fmt.Errorf("%w (database untouched)", err)
	}
	recDir, safety := "", ""
	if props.exists {
		out, err := c.exec.Run(ctx, "umask 077; mktemp -d /var/tmp/teploy-pre-restore.XXXXXX")
		if err != nil {
			return "", fmt.Errorf("creating the safety-dump directory (database untouched): %w", err)
		}
		recDir = strings.TrimSpace(out)
		safety = recDir + "/pre-restore.pgdump"
		qs := ssh.ShellQuote(safety)
		cmd := fmt.Sprintf("docker exec %s pg_dump -Fc -U %s %s > %s && [ \"$(head -c 5 %s)\" = PGDMP ]",
			ssh.ShellQuote(t.container), ssh.ShellQuote(t.user), ssh.ShellQuote(t.db), qs, qs)
		if _, err := c.exec.Run(ctx, cmd); err != nil {
			c.exec.Run(context.WithoutCancel(ctx), "rm -rf "+ssh.ShellQuote(recDir))
			return "", fmt.Errorf("safety dump of the current database failed (database untouched): %w", err)
		}
		fmt.Fprintf(c.out, "Safety dump of the current %s database: %s\n", t.db, safety)
	}

	loadErr := c.pgRecreateAndLoad(ctx, t, props, artifact, ext, workDir, timescale, false)
	if loadErr == nil {
		return recDir, nil
	}
	if safety == "" {
		return "", loadErr
	}
	fmt.Fprintf(c.out, "Restore failed (%v); reloading the safety dump...\n", loadErr)
	wctx := context.WithoutCancel(ctx)
	safetyTS, err := c.pgValidateArtifact(wctx, t, safety, ".pgdump")
	if err == nil {
		err = c.pgRecreateAndLoad(wctx, t, props, safety, ".pgdump", workDir, safetyTS, false)
	}
	if err != nil {
		return recDir, fmt.Errorf("%w: %v — reloading the safety dump also failed: %v; the pre-restore database is saved at %s", errRecoveryIncomplete, loadErr, err, safety)
	}
	c.exec.Run(wctx, "rm -rf "+ssh.ShellQuote(recDir))
	return "", fmt.Errorf("%w — the previous database was reloaded from the safety dump", loadErr)
}

// postgresRestoreCmd is the legacy plain-dump loader for DR bundles taken
// before G7 (.sql.gz members), restored into a freshly created accessory
// (an empty database, so no recreate is needed). gunzip writes to a plain
// file first: a pipeline reports only the LAST command's status.
func postgresRestoreCmd(container, user, db, dumpPath, sqlPath string) string {
	return fmt.Sprintf("gunzip -c %s > %s && docker exec -i %s psql -v ON_ERROR_STOP=1 -U %s %s < %s",
		ssh.ShellQuote(dumpPath), ssh.ShellQuote(sqlPath), ssh.ShellQuote(container), ssh.ShellQuote(user), ssh.ShellQuote(db), ssh.ShellQuote(sqlPath))
}

// ---- mysql / mongo / redis ----

// restoreMySQL decompresses successfully before streaming a credential header and SQL.
// The password is confined to SSH stdin and the client environment, never command text.
func (c *Client) restoreMySQL(ctx context.Context, engine, container, db, pwd, dumpPath, sqlPath string) error {
	if strings.ContainsAny(pwd, "\r\n") {
		return fmt.Errorf("database credential contains a line break")
	}
	if _, err := c.exec.Run(ctx, "umask 077; gunzip -c "+ssh.ShellQuote(dumpPath)+" > "+ssh.ShellQuote(sqlPath)); err != nil {
		return err
	}
	client, _ := mysqlTools(engine)
	receiver := pipefail("cat && cat "+ssh.ShellQuote(sqlPath),
		"docker exec -i "+ssh.ShellQuote(container)+" sh -c "+ssh.ShellQuote(mysqlPasswordPrelude+"exec "+client+" -u root "+ssh.ShellQuote(db)),
		sqlPath+".producer-failed")
	return c.exec.RunInput(ctx, receiver, strings.NewReader("MYSQL_PWD="+pwd+"\n"))
}

func mysqlTools(engine string) (string, string) {
	if engine == EngineMariaDB {
		return "mariadb", "mariadb-admin"
	}
	return "mysql", "mysqladmin"
}

// mysqlReadyProbe waits for the FINAL server with a real authenticated
// query over TCP. `mysqladmin ping` exits 0 even on "access denied" and
// answers the entrypoint's temporary init server (socket-only, root not
// yet configured), so a restore could start mid-init and fail with
// ERROR 1045 once the real server came up.
func mysqlReadyProbe(engine string) string {
	client, _ := mysqlTools(engine)
	return "sh -c " + ssh.ShellQuote("MYSQL_PWD=${MARIADB_ROOT_PASSWORD:-${MYSQL_ROOT_PASSWORD:-${MYSQL_PASSWORD:-}}}; export MYSQL_PWD; exec "+client+" --protocol=TCP -h 127.0.0.1 -u root -e 'SELECT 1'")
}

// mongoRestoreCmd streams a mongodump archive into mongorestore.
func mongoRestoreCmd(container, dumpPath string) string {
	return fmt.Sprintf("docker exec -i %s mongorestore --archive --gzip --drop < %s", ssh.ShellQuote(container), ssh.ShellQuote(dumpPath))
}

// redisRestoreScript stops the container (so its shutdown save cannot
// overwrite the snapshot), captures the current dump as a baseline, copies
// the restored one in, and restarts — restoring the original on any failure
// after the stop (audits F38/A40/A41, T37/T38; see AccessoryRestore history).
// Shared by redis and valkey (both load /data/dump.rdb at start).
func redisRestoreScript(container, dumpPath, rdbPath, oldRdb string) string {
	qContainer := ssh.ShellQuote(container)
	return strings.Join([]string{
		"set -eu",
		fmt.Sprintf("gunzip -c %s > %s", ssh.ShellQuote(dumpPath), ssh.ShellQuote(rdbPath)),
		rdbMagicCheck(rdbPath),
		"had=no",
		fmt.Sprintf(`restore_original() { if [ "$had" = yes ] && [ -f %s ]; then docker cp %s %s:/data/dump.rdb || true; fi; docker start %s || true; }`,
			ssh.ShellQuote(oldRdb), ssh.ShellQuote(oldRdb), qContainer, qContainer),
		fmt.Sprintf("docker stop %s", qContainer),
		// Post-stop baseline (docker cp works on a stopped container):
		// success -> had=yes; a proven not-found -> nothing to preserve;
		// anything else -> restart + abort.
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
}

// redisAOFPreflight proves the accessory is appendonly=no before any
// stop/copy (A40): "not proven yes" is not "proven no", and an AOF-enabled
// redis/valkey would ignore a dump.rdb restore on restart.
func (c *Client) redisAOFPreflight(ctx context.Context, engine, app, name string) error {
	containerName := app + "-" + name
	aofOut, aofErr := c.exec.Run(ctx, fmt.Sprintf("docker exec %s %s --raw config get appendonly", ssh.ShellQuote(containerName), redisCLI(engine)))
	if aofErr != nil {
		return fmt.Errorf("cannot establish the %s persistence mode for %s: %w", engine, containerName, aofErr)
	}
	if aofFields := strings.Fields(aofOut); len(aofFields) != 2 || aofFields[0] != "appendonly" || aofFields[1] != "no" {
		return fmt.Errorf("cannot confirm appendonly=no for %s (got %q) — an AOF-enabled %s would load the append-only file on restart and teploy's dump.rdb restore would be a no-op; an explicit restore plan is required", containerName, strings.TrimSpace(aofOut), engine)
	}
	return nil
}

// isDBType reports whether image's repository name is dbType. Detection
// proper is DetectEngine; this remains the image-name primitive.
func isDBType(image, dbType string) bool {
	return imageName(image) == dbType
}

// imageName returns the repository name of an image reference:
// "postgres:16" → "postgres", "library/postgres" → "postgres",
// "registry.example:5000/postgres:16" → "postgres",
// "postgres@sha256:..." → "postgres". The registry port's colon sits
// BEFORE the last slash, so it is never taken for the tag separator.
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
// / POSTGRES_USER, defaulting to the app name / "postgres"); Bitnami images
// name them POSTGRESQL_DATABASE / POSTGRESQL_USERNAME. Backup/restore
// previously hardcoded db=app unconditionally — silently wrong for any
// accessory setting a custom POSTGRES_DB.
func postgresDBAndUser(app string, env map[string]string) (db, user string) {
	db = env["POSTGRES_DB"]
	if db == "" {
		db = env["POSTGRESQL_DATABASE"]
	}
	if db == "" {
		db = app
	}
	user = env["POSTGRES_USER"]
	if user == "" {
		user = env["POSTGRESQL_USERNAME"]
	}
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
	db := env["MARIADB_DATABASE"]
	if db == "" {
		db = env["MYSQL_DATABASE"]
	}
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
	if pwd := env["MARIADB_ROOT_PASSWORD"]; pwd != "" {
		return pwd
	}
	if pwd := env["MYSQL_ROOT_PASSWORD"]; pwd != "" {
		return pwd
	}
	return env["MYSQL_PASSWORD"]
}

const mysqlPasswordPrelude = `IFS= read -r teploy_credential || exit 1; MYSQL_PWD=${teploy_credential#MYSQL_PWD=}; export MYSQL_PWD; `

// postgresReadyProbe waits for the FINAL server: the official entrypoint
// runs a temporary socket-only server while it initialises, and a socket
// probe that answers during init let a restore race the restart. The
// temporary server never listens on TCP.
func postgresReadyProbe(user string) string {
	return "pg_isready -h 127.0.0.1 -U " + ssh.ShellQuote(user)
}

// pgLoadArtifact loads a DR-bundle postgres member into a freshly started
// engine: custom-format members through the recreate+pg_restore path,
// legacy plain members (bundles taken before G7) through psql.
func (c *Client) pgLoadArtifact(ctx context.Context, t pgTarget, artifact, workDir string, scratch bool) error {
	if strings.HasSuffix(artifact, ".sql.gz") {
		_, err := c.exec.Run(ctx, postgresRestoreCmd(t.container, t.user, t.db, artifact, strings.TrimSuffix(artifact, ".gz")))
		return err
	}
	timescale, err := c.pgValidateArtifact(ctx, t, artifact, ".pgdump")
	if err != nil {
		return err
	}
	props, err := c.pgDatabaseProps(ctx, t)
	if err != nil {
		return err
	}
	return c.pgRecreateAndLoad(ctx, t, props, artifact, ".pgdump", workDir, timescale, scratch)
}

// dragonflyScratchLoad loads an RDB into a running scratch dragonfly with
// DFLY LOAD and returns its key count.
func (c *Client) dragonflyScratchLoad(ctx context.Context, scratch, rdb string) (string, error) {
	if err := c.waitReady(ctx, scratch, "redis-cli ping", 20); err != nil {
		return "", err
	}
	q := ssh.ShellQuote(scratch)
	out, err := c.exec.Run(ctx, strings.Join([]string{
		"set -eu",
		dragonflyDirPrelude(q),
		fmt.Sprintf(`docker cp %s %s:"$dir"/teploy-verify.rdb`, ssh.ShellQuote(rdb), q),
		fmt.Sprintf(`docker exec -u 0 %s chmod 644 "$dir"/teploy-verify.rdb`, q),
		fmt.Sprintf(`out=$(docker exec %s redis-cli dfly load teploy-verify.rdb 2>&1) || true`, q),
		`if [ "$out" != OK ]; then printf 'DFLY LOAD of the backup failed: %s\n' "$out" >&2; exit 1; fi`,
		fmt.Sprintf("docker exec %s redis-cli dbsize", q),
	}, "\n"))
	if err != nil {
		return "", fmt.Errorf("loading the snapshot into scratch dragonfly: %w", err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	return strings.TrimSpace(lines[len(lines)-1]), nil
}
