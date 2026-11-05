package backup

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/useteploy/teploy/internal/ssh"
)

// RestoreRecovery is what a restore keeps so it can be undone until the
// caller's post-restore checks pass: a server path (the previous files, or a
// pre-restore snapshot) and/or the previous postgres database, renamed aside.
// ReleaseRecovery discards it; until then it is the way back.
type RestoreRecovery struct {
	Path string
	// PGDatabase is the previous database, renamed aside in pg's container.
	PGDatabase string
	pg         pgTarget
	pgVersion  int
	// Warnings are non-fatal problems the restore met (database-level
	// settings it could not re-apply, a lost swap status).
	Warnings []string
}

// IsZero reports whether the restore kept nothing.
func (r RestoreRecovery) IsZero() bool { return r.Path == "" && r.PGDatabase == "" }

// String names what was kept, for messages and the restore --json envelope.
func (r RestoreRecovery) String() string {
	var parts []string
	if r.PGDatabase != "" {
		parts = append(parts, fmt.Sprintf("database %s in container %s", r.PGDatabase, r.pg.container))
	}
	if r.Path != "" {
		parts = append(parts, r.Path)
	}
	return strings.Join(parts, " and ")
}

// ReleaseRecovery discards a kept recovery copy.
func (c *Client) ReleaseRecovery(ctx context.Context, r RestoreRecovery) error {
	var errs []error
	if r.PGDatabase != "" {
		if err := c.pgDropDB(ctx, r.pg, r.PGDatabase, r.pgVersion); err != nil {
			errs = append(errs, fmt.Errorf("dropping the pre-restore database %s: %w", r.PGDatabase, err))
		}
	}
	if r.Path != "" {
		if _, err := c.exec.Run(ctx, "rm -rf "+ssh.ShellQuote(r.Path)); err != nil {
			errs = append(errs, fmt.Errorf("removing %s: %w", r.Path, err))
		}
	}
	return errors.Join(errs...)
}

// AccessoryArtifact is the stored object a restore of <date> would use.
type AccessoryArtifact struct {
	Key, Ext string
	Engine   EngineDetection
	// FileRestore is true when the artifact is a file archive (.tar.gz):
	// restoring it replaces files on disk, so the accessory container
	// must be stopped first. Every backup a pre-G6 CLI took of a
	// pgvector/timescale/postgis/bitnami/valkey/dragonfly accessory is one.
	FileRestore bool
}

// artifactExts lists, per engine, the extensions its backups have used:
// the engine-native form first, then the raw .tar.gz the generic branch
// wrote before detection recognised the engine.
func artifactExts(engine string) []string {
	switch engine {
	case EnginePostgres:
		return []string{".pgdump", ".sql.gz", ".tar.gz"}
	case EngineMySQL, EngineMariaDB:
		return []string{".sql.gz", ".tar.gz"}
	case EngineMongo:
		return []string{".archive.gz", ".tar.gz"}
	case EngineRedis, EngineValkey, EngineDragonfly:
		return []string{".rdb.gz", ".tar.gz"}
	}
	return []string{".tar.gz"}
}

// ResolveAccessoryArtifact finds the object for backup date and says
// whether restoring it is a file restore (decided by the artifact, not the
// engine: a reclassified engine's old backups are tarballs).
func (c *Client) ResolveAccessoryArtifact(ctx context.Context, app, name, image, date string, env map[string]string, s3 S3Config) (AccessoryArtifact, error) {
	if err := ValidateDate(date); err != nil {
		return AccessoryArtifact{}, err
	}
	det, err := c.ResolveEngine(ctx, app, name, image, env)
	if err != nil {
		return AccessoryArtifact{}, err
	}
	prefix := fmt.Sprintf("s3://%s/%s/accessories/%s/", s3.Base(), app, name)
	key, ext, err := c.resolveAccessoryKey(ctx, s3, prefix, date, artifactExts(det.Engine)...)
	if err != nil {
		return AccessoryArtifact{}, err
	}
	return AccessoryArtifact{Key: key, Ext: ext, Engine: det, FileRestore: ext == ".tar.gz"}, nil
}

// AccessoryRestore restores an accessory backup and releases the recovery
// copy on success.
func (c *Client) AccessoryRestore(ctx context.Context, app, name, image, date string, env map[string]string, s3 S3Config) error {
	rec, err := c.AccessoryRestoreKeep(ctx, app, name, image, date, env, s3)
	if err == nil && !rec.IsZero() {
		if relErr := c.ReleaseRecovery(context.WithoutCancel(ctx), rec); relErr != nil {
			fmt.Fprintf(c.out, "Warning: %v\n", relErr)
		}
	}
	return err
}

// AccessoryRestoreKeep is AccessoryRestore where the caller owns the
// recovery copy (released with ReleaseRecovery once its own post-restore
// checks pass). A file restore (.tar.gz artifact) requires the accessory
// container to be stopped; the engine restores run through the live engine.
//
// Every error says whether the live data was touched. Only a failure that
// leaves the data in an indeterminate state is recovery-incomplete
// (IsRecoveryIncomplete), which tells the caller not to restart the app.
func (c *Client) AccessoryRestoreKeep(ctx context.Context, app, name, image, date string, env map[string]string, s3 S3Config) (RestoreRecovery, error) {
	return c.AccessoryRestoreWith(ctx, app, name, image, date, env, s3, RestoreOptions{})
}

// RestoreOptions tune an accessory restore.
type RestoreOptions struct {
	// IgnoreSpaceCheck skips the free-space preflights: when the estimate
	// is known to be wrong, or the disk is being recovered by this restore.
	IgnoreSpaceCheck bool
	// Database restores one extra database stored by backup_databases:
	// instead of the accessory's primary (postgres only).
	Database string
}

// AccessoryRestoreWith is AccessoryRestoreKeep with options.
func (c *Client) AccessoryRestoreWith(ctx context.Context, app, name, image, date string, env map[string]string, s3 S3Config, opts RestoreOptions) (RestoreRecovery, error) {
	var none RestoreRecovery
	if err := c.ensureAWSCLI(ctx); err != nil {
		return none, err
	}
	var art AccessoryArtifact
	var err error
	if opts.Database != "" {
		art, err = c.resolveExtraDatabase(ctx, app, name, image, date, env, s3, opts.Database)
	} else {
		art, err = c.ResolveAccessoryArtifact(ctx, app, name, image, date, env, s3)
	}
	if err != nil {
		return none, err
	}
	container := app + "-" + name
	if art.FileRestore {
		out, _ := c.exec.Run(ctx, "docker inspect -f '{{.State.Running}}' "+ssh.ShellQuote(container)+" 2>/dev/null")
		if strings.TrimSpace(out) == "true" {
			return none, fmt.Errorf("backup %s of %s is a file archive (%s); restoring it replaces files on disk, so the accessory must be stopped first (teploy backup restore --accessory does that)", date, name, art.Ext)
		}
	}

	// All restore scratch files live under one per-invocation temp dir
	// (shared fixed names let concurrent or retried restores read each
	// other's files). On failure it is kept and named; on success removed.
	tmpOut, err := c.exec.Run(ctx, "mktemp -d "+ssh.ShellQuote("/tmp/teploy-restore.XXXXXX"))
	if err != nil {
		return none, fmt.Errorf("creating restore temp dir: %w", err)
	}
	tmpdir := strings.TrimSpace(tmpOut)
	credFile := tmpdir + "/mysql.env"
	fail := func(rec RestoreRecovery, err error) (RestoreRecovery, error) {
		// The kept-for-inspection scratch dir never keeps a credential.
		c.exec.Run(context.WithoutCancel(ctx), "rm -f "+ssh.ShellQuote(credFile))
		return rec, fmt.Errorf("restoring %s: %w — restore files kept in %s", name, err, tmpdir)
	}
	artifact := tmpdir + "/restore" + art.Ext
	fmt.Fprintf(c.out, "Downloading %s...\n", art.Key)
	if _, err := c.exec.Run(ctx, s3.AWS(fmt.Sprintf("s3 cp %s %s", ssh.ShellQuote(art.Key), ssh.ShellQuote(artifact)))); err != nil {
		return fail(none, fmt.Errorf("downloading backup: %w", err))
	}

	fmt.Fprintf(c.out, "Restoring %s (%s, %s)...\n", name, art.Engine.Engine, art.Ext)
	var rec RestoreRecovery
	switch {
	case art.FileRestore:
		accDir := fmt.Sprintf("%s/%s/accessories/%s", deploymentsDir, app, name)
		dir, err := extractToStagingThenPromote(ctx, c.exec, artifact, tmpdir+"/restore-stage", accDir, c.out, nil)
		if err != nil {
			return fail(none, err)
		}
		rec = RestoreRecovery{Path: dir}

	case art.Engine.Engine == EnginePostgres:
		db, user := postgresDBAndUser(app, env)
		pgOpts := pgRestoreOpts{ignoreSpace: opts.IgnoreSpaceCheck}
		if opts.Database != "" && opts.Database != db {
			db = opts.Database
		} else {
			pgOpts.logicalBytes = c.pgRecordedSize(ctx, s3, app, name, date, tmpdir)
		}
		rec, err = c.restorePostgres(ctx, newPGTarget(container, db, user, env), artifact, art.Ext, tmpdir, pgOpts)
		if err != nil {
			return fail(rec, err)
		}

	case art.Engine.Engine == EngineMySQL || art.Engine.Engine == EngineMariaDB:
		m := mysqlTarget{engine: art.Engine.Engine, container: container, db: mysqlDB(app, env)}
		if pwd := mysqlRootPassword(env); pwd != "" {
			if err := c.exec.Upload(ctx, strings.NewReader("MYSQL_PWD="+pwd+"\n"), credFile, "0600"); err != nil {
				return fail(none, err)
			}
			m.credFile = credFile
		}
		rec, err = c.restoreMySQLInPlace(ctx, m, artifact, tmpdir, opts.IgnoreSpaceCheck, recoveryPrefix(app, name))
		if err != nil {
			return fail(rec, err)
		}

	case art.Engine.Engine == EngineMongo:
		rec, err = c.restoreMongoInPlace(ctx, container, artifact, recoveryPrefix(app, name))
		if err != nil {
			return fail(rec, err)
		}

	case art.Engine.Engine == EngineRedis || art.Engine.Engine == EngineValkey:
		// Stop first so the shutdown save cannot overwrite the snapshot
		// copied in, then start so it loads dump.rdb (redisRestoreScript).
		if err := c.redisAOFPreflight(ctx, art.Engine.Engine, app, name); err != nil {
			return fail(none, err)
		}
		if _, err := c.exec.Run(ctx, redisRestoreScript(container, artifact, tmpdir+"/restore.rdb", tmpdir+"/old-dump.rdb")); err != nil {
			return fail(none, err)
		}

	case art.Engine.Engine == EngineDragonfly:
		recOut, err := c.exec.Run(ctx, "umask 077; mktemp -d "+ssh.ShellQuote(recoveryPrefix(app, name)+".XXXXXX"))
		if err != nil {
			return fail(none, fmt.Errorf("creating the pre-restore snapshot directory (dataset untouched): %w", err))
		}
		rec = RestoreRecovery{Path: strings.TrimSpace(recOut)}
		if _, err := c.exec.Run(ctx, dragonflyLoadScript(container, artifact, tmpdir, rec.Path+"/pre-restore.rdb")); err != nil {
			if strings.Contains(err.Error(), recoveryIncompleteMarker) {
				err = fmt.Errorf("%w: %v — the pre-restore snapshot is at %s/pre-restore.rdb", errRecoveryIncomplete, err, rec.Path)
			}
			return fail(rec, err)
		}

	default:
		return fail(none, fmt.Errorf("engine %q has no restore path for a %s artifact", art.Engine.Engine, art.Ext))
	}

	c.exec.Run(ctx, "rm -rf "+ssh.ShellQuote(tmpdir))
	fmt.Fprintln(c.out, "Restore complete")
	return rec, nil
}

// recoveryPrefix names an accessory's pre-restore safety copies, so doctor
// and `backup restore --release` can attribute them.
func recoveryPrefix(app, name string) string {
	return "/var/tmp/teploy-pre-restore." + app + "-" + name
}

// resolveExtraDatabase is the stored dump of a backup_databases: entry.
func (c *Client) resolveExtraDatabase(ctx context.Context, app, name, image, date string, env map[string]string, s3 S3Config, db string) (AccessoryArtifact, error) {
	if err := ValidateDate(date); err != nil {
		return AccessoryArtifact{}, err
	}
	if !pgSafeDBName.MatchString(db) {
		return AccessoryArtifact{}, fmt.Errorf("invalid database name %q", db)
	}
	det, err := c.ResolveEngine(ctx, app, name, image, env)
	if err != nil {
		return AccessoryArtifact{}, err
	}
	if det.Engine != EnginePostgres {
		return AccessoryArtifact{}, fmt.Errorf("--database applies to postgres accessories; %s is %s", name, det.Engine)
	}
	if primary, _ := postgresDBAndUser(app, env); db == primary {
		return c.ResolveAccessoryArtifact(ctx, app, name, image, date, env, s3)
	}
	return AccessoryArtifact{Key: pgExtraKey(s3, app, name, db, date), Ext: ".pgdump", Engine: det}, nil
}

// recoveryIncompleteMarker is what a restore script prints when both the
// restore and its own rollback failed (the data is in an unknown state).
const recoveryIncompleteMarker = "TEPLOY_RECOVERY_INCOMPLETE"

// freeBytes reads the free space of the filesystem holding path (df -Pk);
// ok is false when it cannot be read.
func (c *Client) freeBytes(ctx context.Context, cmdPrefix, path string) (int64, bool) {
	out, err := c.exec.Run(ctx, cmdPrefix+"df -Pk "+ssh.ShellQuote(path))
	if err != nil {
		return 0, false
	}
	return parseDFAvail(out)
}

func parseDFAvail(out string) (int64, bool) {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 2 {
		return 0, false
	}
	f := strings.Fields(lines[len(lines)-1])
	if len(f) < 4 {
		return 0, false
	}
	kb, err := strconv.ParseInt(f[3], 10, 64)
	if err != nil {
		return 0, false
	}
	return kb * 1024, true
}

// requireSpace refuses when the free space is known to be below need.
func requireSpace(what string, free int64, known bool, need int64) error {
	if known && free < need {
		return fmt.Errorf("not enough free space for %s: %d bytes free, %d needed (nothing was changed)", what, free, need)
	}
	return nil
}

// ---- mysql / mariadb ----

// mysqlTarget is one database in a mysql/mariadb container. credFile, when
// set, is the 0600 MYSQL_PWD=... file every client call reads its password
// from (never argv).
type mysqlTarget struct {
	engine, container, db, credFile string
}

// client builds `docker exec` of the engine client with args, reading the
// credential (when any) from credFile.
func (m mysqlTarget) client(tool, args string) string {
	q := ssh.ShellQuote(m.container)
	if m.credFile == "" {
		return "docker exec -i " + q + " " + tool + " -u root " + args
	}
	return "docker exec -i " + q + " sh -c " + ssh.ShellQuote(mysqlPasswordPrelude+"exec "+tool+" -u root "+args) + " < " + ssh.ShellQuote(m.credFile)
}

// load streams a SQL file into the database, failing when either the
// file read or the client fails.
func (m mysqlTarget) load(sqlPath, marker string) string {
	client, _ := mysqlTools(m.engine)
	if m.credFile == "" {
		return "docker exec -i " + ssh.ShellQuote(m.container) + " " + client + " -u root " + ssh.ShellQuote(m.db) + " < " + ssh.ShellQuote(sqlPath)
	}
	return pipefail("cat "+ssh.ShellQuote(m.credFile)+" && cat "+ssh.ShellQuote(sqlPath),
		"docker exec -i "+ssh.ShellQuote(m.container)+" sh -c "+ssh.ShellQuote(mysqlPasswordPrelude+"exec "+client+" -u root "+ssh.ShellQuote(m.db)),
		marker)
}

func mysqlIdent(s string) string { return "`" + strings.ReplaceAll(s, "`", "``") + "`" }

// mysqlDumpArgs are the dump flags every mysql/mariadb dump uses: a
// consistent InnoDB snapshot without locking writers, plus the stored
// routines, triggers and events a plain dump omits.
const mysqlDumpArgs = "--single-transaction --routines --triggers --events"

// restoreMySQLInPlace replaces the database with the dump. MySQL has no
// RENAME DATABASE, and RENAME TABLE across schemas does not carry views,
// routines, events or triggers, so a Postgres-style swap is not available.
// Instead the dump must first prove it loads: into a throwaway
// <db>_teploy_check on this server, under this server's settings. A dump the
// server refuses (a table created under innodb_strict_mode=0 hits error 1118
// on reload) is refused here with the database untouched; DROPping first
// left only the tables before the bad one, and the safety dump of the same
// table failed the same way. Then: safety dump, DROP + CREATE with the
// database's charset (so tables created after the backup do not survive),
// load. A failed load reloads the safety dump; if that fails too the error is
// recovery-incomplete.
func (c *Client) restoreMySQLInPlace(ctx context.Context, m mysqlTarget, artifact, workDir string, ignoreSpace bool, recPrefix string) (RestoreRecovery, error) {
	var none RestoreRecovery
	client, _ := mysqlTools(m.engine)
	dumpTool := "mysqldump"
	if m.engine == EngineMariaDB {
		dumpTool = "mariadb-dump"
	}
	sqlPath := workDir + "/restore.sql"
	q := ssh.ShellQuote
	if _, err := c.exec.Run(ctx, "umask 077; gunzip -c "+q(artifact)+" > "+q(sqlPath)+" && tail -c 512 "+q(sqlPath)+" | grep -q 'Dump completed'"); err != nil {
		return none, fmt.Errorf("the dump is corrupt or truncated (database untouched): %w", err)
	}
	lit := "'" + strings.ReplaceAll(m.db, "'", "''") + "'"
	out, err := c.exec.Run(ctx, m.client(client, "-N -B -e "+q("SELECT CONCAT(DEFAULT_CHARACTER_SET_NAME, '|', DEFAULT_COLLATION_NAME, '|', (SELECT COALESCE(SUM(data_length + index_length), 0) FROM information_schema.TABLES WHERE table_schema = "+lit+")) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME = "+lit)))
	if err != nil {
		return none, fmt.Errorf("reading database %s (database untouched): %w", m.db, err)
	}
	props := strings.Split(strings.TrimSpace(out), "|")
	exists := len(props) == 3
	charset := ""
	if exists {
		charset = " CHARACTER SET " + props[0] + " COLLATE " + props[1]
	}
	recreate := func(db string) string {
		return m.client(client, "-e "+q("DROP DATABASE IF EXISTS "+mysqlIdent(db)+"; CREATE DATABASE "+mysqlIdent(db)+charset))
	}

	if !ignoreSpace {
		// The dry run and the real load each need about the dump's size on
		// the data volume; the dry run's copy is dropped before the load.
		var sqlBytes int64
		if out, err := c.exec.Run(ctx, "wc -c < "+q(sqlPath)); err == nil {
			sqlBytes, _ = strconv.ParseInt(strings.TrimSpace(out), 10, 64)
		}
		free, known := c.freeBytes(ctx, "docker exec "+q(m.container)+" ", "/var/lib/mysql")
		if err := requireSpace("loading the dump on the data volume (--ignore-space-check overrides)", free, known, sqlBytes); err != nil {
			return none, err
		}
	}

	// The dry run: nothing of the live database is touched until the server
	// has accepted the whole dump.
	check := truncBytes(m.db, 40) + "_teploy_check"
	checkT := m
	checkT.db = check
	wctx := context.WithoutCancel(ctx)
	dropCheck := func() error {
		_, err := c.exec.Run(wctx, m.client(client, "-e "+q("DROP DATABASE IF EXISTS "+mysqlIdent(check))))
		return err
	}
	fmt.Fprintf(c.out, "Checking that the dump loads (into %s)...\n", check)
	_, dryErr := c.exec.Run(ctx, recreate(check))
	if dryErr == nil {
		_, dryErr = c.exec.Run(ctx, checkT.load(sqlPath, workDir+"/check.failed"))
	}
	dropErr := dropCheck()
	if dryErr != nil {
		// A dry run that takes the server down is not "untouched".
		if _, pingErr := c.exec.Run(wctx, m.client(client, "-e "+q("SELECT 1"))); pingErr != nil {
			return none, fmt.Errorf("%w: the dry run failed (%v) and the server no longer answers (%v)", errRecoveryIncomplete, dryErr, pingErr)
		}
		return none, fmt.Errorf("this server cannot load the dump, so nothing was changed (database untouched): %w", dryErr)
	}
	if dropErr != nil {
		fmt.Fprintf(c.out, "Warning: could not drop the dry-run database %s: %v\n", check, dropErr)
	}

	var rec RestoreRecovery
	safety := ""
	if exists {
		if !ignoreSpace {
			size, _ := strconv.ParseInt(props[2], 10, 64)
			free, known := c.freeBytes(ctx, "", "/var/tmp")
			if err := requireSpace("the pre-restore safety dump in /var/tmp (--ignore-space-check overrides)", free, known, size); err != nil {
				return none, err
			}
		}
		dir, err := c.exec.Run(ctx, "umask 077; mktemp -d "+q(recPrefix+".XXXXXX"))
		if err != nil {
			return none, fmt.Errorf("creating the safety-dump directory (database untouched): %w", err)
		}
		rec.Path = strings.TrimSpace(dir)
		safety = rec.Path + "/pre-restore.sql"
		if _, err := c.exec.Run(ctx, m.client(dumpTool, mysqlDumpArgs+" "+q(m.db))+" > "+q(safety)+" && tail -c 512 "+q(safety)+" | grep -q 'Dump completed'"); err != nil {
			c.exec.Run(wctx, "rm -rf "+q(rec.Path))
			return none, fmt.Errorf("safety dump of the current database failed (database untouched): %w", err)
		}
		fmt.Fprintf(c.out, "Safety dump of the current %s database: %s\n", m.db, safety)
	}

	loadErr := func() error {
		if _, err := c.exec.Run(ctx, recreate(m.db)); err != nil {
			return fmt.Errorf("recreating database %s: %w", m.db, err)
		}
		if _, err := c.exec.Run(ctx, m.load(sqlPath, workDir+"/load.failed")); err != nil {
			return fmt.Errorf("loading the dump: %w", err)
		}
		return nil
	}()
	if loadErr == nil {
		return rec, nil
	}
	if safety == "" {
		return none, loadErr
	}
	fmt.Fprintf(c.out, "Restore failed (%v); reloading the safety dump...\n", loadErr)
	if _, err := c.exec.Run(wctx, recreate(m.db)); err == nil {
		_, err = c.exec.Run(wctx, m.load(safety, workDir+"/reload.failed"))
		if err == nil {
			c.exec.Run(wctx, "rm -rf "+q(rec.Path))
			return none, fmt.Errorf("%w — the previous database was reloaded from the safety dump", loadErr)
		}
		loadErr = fmt.Errorf("%v — reloading the safety dump also failed: %v", loadErr, err)
	} else {
		loadErr = fmt.Errorf("%v — recreating the database for the safety reload failed: %v", loadErr, err)
	}
	return rec, fmt.Errorf("%w: %v; the pre-restore database is saved at %s", errRecoveryIncomplete, loadErr, safety)
}

// ---- mongo ----

// mongoCheckPrefix names the throwaway databases of a restore's dry run.
const mongoCheckPrefix = "teploy_check_"

// mongoShell runs a script in the container's shell (mongosh on 5.x+, the
// legacy mongo shell before that).
func mongoShell(container, script string) string {
	return "docker exec " + ssh.ShellQuote(container) + " sh -c " + ssh.ShellQuote("mongosh --quiet --eval "+ssh.ShellQuote(script)+" || mongo --quiet --eval "+ssh.ShellQuote(script))
}

// restoreMongoInPlace first proves the archive loads, into throwaway
// teploy_check_* databases (mongorestore --nsFrom/--nsTo; admin and config
// are left out, as they cannot be renamed), so an archive the server refuses
// leaves the data untouched. Then a safety mongodump and mongorestore --drop;
// a failed restore reloads the safety archive (recovery-incomplete when that
// fails too). --drop replaces the collections in the archive; collections
// created after the backup survive.
func (c *Client) restoreMongoInPlace(ctx context.Context, container, artifact, recPrefix string) (RestoreRecovery, error) {
	var none RestoreRecovery
	q := ssh.ShellQuote
	if _, err := c.exec.Run(ctx, "gzip -t "+q(artifact)); err != nil {
		return none, fmt.Errorf("the archive is corrupt (database untouched): %w", err)
	}
	wctx := context.WithoutCancel(ctx)
	dropCheck := func() {
		script := "db.getMongo().getDBNames().filter(function(n){return n.indexOf(" + "'" + mongoCheckPrefix + "'" + ")===0}).forEach(function(n){db.getSiblingDB(n).dropDatabase()})"
		if _, err := c.exec.Run(wctx, mongoShell(container, script)); err != nil {
			fmt.Fprintf(c.out, "Warning: could not drop the dry-run databases (%s*): %v\n", mongoCheckPrefix, err)
		}
	}
	fmt.Fprintln(c.out, "Checking that the archive loads...")
	dry := "docker exec -i " + q(container) + " mongorestore --archive --gzip --drop --nsExclude=" + q("admin.*") + " --nsExclude=" + q("config.*") +
		" --nsFrom=" + q("$db$.$coll$") + " --nsTo=" + q(mongoCheckPrefix+"$db$.$coll$") + " < " + q(artifact)
	_, dryErr := c.exec.Run(ctx, dry)
	dropCheck()
	if dryErr != nil {
		return none, fmt.Errorf("this server cannot load the archive, so nothing was changed (database untouched): %w", dryErr)
	}
	dir, err := c.exec.Run(ctx, "umask 077; mktemp -d "+q(recPrefix+".XXXXXX"))
	if err != nil {
		return none, fmt.Errorf("creating the safety-dump directory (database untouched): %w", err)
	}
	rec := RestoreRecovery{Path: strings.TrimSpace(dir)}
	safety := rec.Path + "/pre-restore.archive.gz"
	if _, err := c.exec.Run(ctx, "docker exec "+q(container)+" mongodump --archive --gzip > "+q(safety)); err != nil {
		c.exec.Run(wctx, "rm -rf "+q(rec.Path))
		return none, fmt.Errorf("safety dump of the current data failed (database untouched): %w", err)
	}
	if _, err := c.exec.Run(ctx, mongoRestoreCmd(container, artifact)); err == nil {
		return rec, nil
	} else {
		if _, rbErr := c.exec.Run(wctx, mongoRestoreCmd(container, safety)); rbErr != nil {
			return rec, fmt.Errorf("%w: mongorestore: %v — reloading the safety dump also failed: %v; the pre-restore data is saved at %s", errRecoveryIncomplete, err, rbErr, safety)
		}
		c.exec.Run(wctx, "rm -rf "+q(rec.Path))
		return none, fmt.Errorf("mongorestore: %w — the previous data was reloaded from the safety dump", err)
	}
}

// ---- kept recovery copies ----

// RecoveryCopies is what earlier restores of an accessory left behind.
type RecoveryCopies struct {
	Previous []string // postgres databases renamed aside (<db>_teploy_old_*)
	Partial  []string // replacements a killed restore left (<db>_teploy_new_*)
	Paths    []string // pre-restore safety copies in /var/tmp
	pg       pgTarget
	version  int
}

// FindRecoveryCopies lists an accessory's kept recovery copies.
func (c *Client) FindRecoveryCopies(ctx context.Context, app, name, image string, env map[string]string) (RecoveryCopies, error) {
	var rc RecoveryCopies
	det, err := c.ResolveEngine(ctx, app, name, image, env)
	if err != nil {
		return rc, err
	}
	if det.Engine == EnginePostgres {
		db, user := postgresDBAndUser(app, env)
		rc.pg = newPGTarget(app+"-"+name, db, user, env)
		cl, err := c.pgClusterState(ctx, rc.pg)
		if err != nil {
			return rc, err
		}
		rc.version = cl.version
		cov, err := c.pgReadCoverage(ctx, rc.pg, nil)
		if err != nil {
			return rc, err
		}
		// Only this database's copies (a truncated name is still its prefix).
		prefix := truncBytes(db, 40) + "_teploy_"
		for _, n := range cov.KeptOld {
			if strings.HasPrefix(n, prefix) {
				rc.Previous = append(rc.Previous, n)
			}
		}
		for _, n := range cov.PartialNew {
			if strings.HasPrefix(n, prefix) {
				rc.Partial = append(rc.Partial, n)
			}
		}
	}
	out, err := c.exec.Run(ctx, "for d in "+ssh.ShellQuote(recoveryPrefix(app, name))+".*; do [ -d \"$d\" ] && printf '%s\\n' \"$d\"; done; true")
	if err == nil {
		rc.Paths = strings.Fields(out)
	}
	return rc, nil
}

// ReleaseRecoveryCopies confirms a restore: it discards every kept copy.
func (c *Client) ReleaseRecoveryCopies(ctx context.Context, rc RecoveryCopies) ([]string, error) {
	var released []string
	var errs []error
	for _, db := range append(append([]string{}, rc.Previous...), rc.Partial...) {
		if err := c.pgDropDB(ctx, rc.pg, db, rc.version); err != nil {
			errs = append(errs, fmt.Errorf("dropping %s: %w", db, err))
			continue
		}
		released = append(released, db)
	}
	for _, p := range rc.Paths {
		if _, err := c.exec.Run(ctx, "rm -rf "+ssh.ShellQuote(p)); err != nil {
			errs = append(errs, fmt.Errorf("removing %s: %w", p, err))
			continue
		}
		released = append(released, p)
	}
	return released, errors.Join(errs...)
}

// RollbackRestore undoes a restore by swapping the kept previous postgres
// database back into place. The database it displaces stays, renamed as a
// partial copy, until --release. pick selects one when several are kept. The
// caller has quiesced the app. Safety-dump copies of other engines are not
// rolled back automatically; the error names them.
func (c *Client) RollbackRestore(ctx context.Context, rc RecoveryCopies, pick string) (restored, displaced string, err error) {
	switch {
	case pick != "":
		if !slices.Contains(rc.Previous, pick) {
			return "", "", fmt.Errorf("%s is not a kept previous database (kept: %s)", pick, strings.Join(rc.Previous, ", "))
		}
		restored = pick
	case len(rc.Previous) == 1:
		restored = rc.Previous[0]
	case len(rc.Previous) == 0:
		if len(rc.Paths) > 0 {
			return "", "", fmt.Errorf("no previous database is kept; the pre-restore safety copies are %s (load one with the engine's client)", strings.Join(rc.Paths, ", "))
		}
		return "", "", fmt.Errorf("no previous database is kept for this accessory")
	default:
		return "", "", fmt.Errorf("several previous databases are kept (%s): pick one with --recovery-database", strings.Join(rc.Previous, ", "))
	}
	t := rc.pg
	displaced = truncBytes(t.db, 40) + "_teploy_new_" + randSuffix()
	st, err := c.pgSwapState(ctx, t, restored, displaced)
	if err != nil {
		return "", "", fmt.Errorf("reading the databases: %w", err)
	}
	// pgSwap's roles reversed: previous -> live, live -> displaced. A live
	// database may be missing after a recovery-incomplete restore.
	if err := c.pgSwap(ctx, t, restored, displaced, st.live); err != nil {
		wctx := context.WithoutCancel(ctx)
		after, stErr := c.pgSwapState(wctx, t, restored, displaced)
		switch {
		case stErr != nil:
			return "", "", fmt.Errorf("%w: the rollback failed (%v) and the server no longer answers (%v)", errRecoveryIncomplete, err, stErr)
		case after.live && !after.replacement && (after.previous || !st.live):
			// Committed: only the reply was lost.
		default:
			return "", "", fmt.Errorf("the rollback did not happen, nothing changed: %w", err)
		}
	}
	if !st.live {
		displaced = ""
	}
	return restored, displaced, nil
}
