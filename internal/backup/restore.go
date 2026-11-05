package backup

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

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
	// Loc is the location the object was found in (the current prefix or a
	// previous one); its sidecars live beside it.
	Loc S3Config
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
	key, ext, loc, err := c.findObject(ctx, s3, app+"/accessories/"+name+"/", date, artifactExts(det.Engine)...)
	if err != nil {
		return AccessoryArtifact{}, err
	}
	return AccessoryArtifact{Key: key, Ext: ext, Engine: det, Loc: loc, FileRestore: ext == ".tar.gz"}, nil
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
	// Database restores one database with its own series (backup_databases:,
	// policy-managed runs, adopted containers) instead of the primary.
	Database string
	// NoPreBackup skips the automatic backup the CLI takes before a restore.
	NoPreBackup bool
	// IdentityFile is a local age identity (--identity) the CLI places on the
	// server for this restore.
	IdentityFile string
	// Deadline overrides the operation deadline (--deadline).
	Deadline time.Duration
}

// AccessoryRestoreWith is AccessoryRestoreKeep with options.
func (c *Client) AccessoryRestoreWith(ctx context.Context, app, name, image, date string, env map[string]string, s3 S3Config, opts RestoreOptions) (RestoreRecovery, error) {
	var none RestoreRecovery
	// A wedged download or load must not keep the app down indefinitely; each
	// engine's restore already says, on cancel, what state it left.
	ctx, cancel := context.WithTimeout(ctx, c.backupDeadline())
	defer cancel()
	if err := c.ensureAWSCLI(ctx); err != nil {
		return none, err
	}
	art, err := c.ResolveRestoreArtifact(ctx, app, name, image, date, env, s3, opts)
	if err != nil {
		return none, err
	}
	container := ContainerName(app, name)
	if art.FileRestore {
		out, _ := c.exec.Run(ctx, "docker inspect -f '{{.State.Running}}' "+ssh.ShellQuote(container)+" 2>/dev/null")
		if strings.TrimSpace(out) == "true" {
			return none, fmt.Errorf("backup %s of %s is a file archive (%s); restoring it replaces files on disk, so the accessory must be stopped first (teploy backup restore --accessory does that)", date, name, art.Ext)
		}
	}

	// All restore scratch files live under one per-invocation temp dir
	// (shared fixed names let concurrent or retried restores read each
	// other's files). On failure it is kept and named; on success removed.
	tmpOut, err := c.exec.Run(ctx, "mktemp -d "+ssh.ShellQuote(backupTmpDir()+"/teploy-restore.XXXXXX"))
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
	if err := c.fetch(ctx, art.Loc, art.Key, artifact); err != nil {
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
			pgOpts.logicalBytes = c.pgRecordedSize(ctx, art.Loc, app, name, db, date, tmpdir)
		} else {
			pgOpts.logicalBytes = c.pgRecordedSize(ctx, art.Loc, app, name, "", date, tmpdir)
		}
		rec, err = c.restorePostgres(ctx, newPGTarget(container, db, user, env), artifact, art.Ext, tmpdir, pgOpts)
		if err != nil {
			return fail(rec, err)
		}

	case art.Engine.Engine == EngineMySQL || art.Engine.Engine == EngineMariaDB:
		m := mysqlTarget{engine: art.Engine.Engine, container: container, db: mysqlDB(app, env)}
		if opts.Database != "" {
			m.db = opts.Database
		}
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
		rec, err = c.restoreMongoInPlace(ctx, container, artifact, recoveryPrefix(app, name), opts.IgnoreSpaceCheck)
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
		recOut, err := c.exec.Run(ctx, "umask 077; mktemp -d "+ssh.ShellQuote(recoveryPrefix(app, name)+"XXXXXX"))
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
		c.markRestoreCompleted(ctx, &rec)

	default:
		return fail(none, fmt.Errorf("engine %q has no restore path for a %s artifact", art.Engine.Engine, art.Ext))
	}

	c.exec.Run(ctx, "rm -rf "+ssh.ShellQuote(tmpdir))
	fmt.Fprintln(c.out, "Restore complete")
	return rec, nil
}

// recoveryPrefix names an accessory's pre-restore safety copies, so doctor
// and `backup restore --release` can attribute them. "@" appears in neither
// an app nor an accessory name: with "-", app a + accessory b-c and app a-b +
// accessory c shared a prefix, and releasing one deleted the other's copies.
func recoveryPrefix(app, name string) string {
	return "/var/tmp/teploy-pre-restore." + app + "@" + name + "@"
}

// restoreCompletedMarker sits beside a safety copy once its restore finished.
// A copy without one may be the only copy of the data: the restore was killed
// after it dropped the live database.
const restoreCompletedMarker = "restore-completed"

// markRestoreCompleted records that the restore a safety copy belongs to
// finished. Failing to write it only costs an explicit --force later.
func (c *Client) markRestoreCompleted(ctx context.Context, rec *RestoreRecovery) {
	if rec.Path == "" {
		return
	}
	if _, err := c.exec.Run(ctx, ": > "+ssh.ShellQuote(rec.Path+"/"+restoreCompletedMarker)); err != nil {
		rec.Warnings = append(rec.Warnings, fmt.Sprintf("could not mark %s as completed, so --release will need --force: %v", rec.Path, err))
	}
}

// resolveExtraDatabase finds one database's object: its own series, or the
// accessory's primary series when it is the primary database.
func (c *Client) resolveExtraDatabase(ctx context.Context, app, name, image, date string, env map[string]string, s3 S3Config, db string) (AccessoryArtifact, error) {
	if err := ValidateDate(date); err != nil {
		return AccessoryArtifact{}, err
	}
	if !addressableDB(db) {
		return AccessoryArtifact{}, fmt.Errorf("invalid database name %q", db)
	}
	det, err := c.ResolveEngine(ctx, app, name, image, env)
	if err != nil {
		return AccessoryArtifact{}, err
	}
	var primary string
	var exts []string
	switch det.Engine {
	case EnginePostgres:
		primary, _ = postgresDBAndUser(app, env)
		exts = []string{".pgdump"}
	case EngineMySQL, EngineMariaDB:
		primary = mysqlDB(app, env)
		exts = []string{".sql.gz"}
	default:
		return AccessoryArtifact{}, fmt.Errorf("--database applies to postgres and mysql/mariadb accessories; %s is %s", name, det.Engine)
	}
	if db == primary && app != AdoptedApp {
		if art, err := c.ResolveAccessoryArtifact(ctx, app, name, image, date, env, s3); err == nil {
			return art, nil
		}
	}
	key, ext, loc, err := c.findObject(ctx, s3, app+"/accessories/"+name+"/databases/"+dbSegment(db)+"/", date, exts...)
	if err != nil {
		return AccessoryArtifact{}, err
	}
	return AccessoryArtifact{Key: key, Ext: ext, Engine: det, Loc: loc}, nil
}

// ResolveRestoreArtifact is the artifact a restore with opts would use.
// Resolving it before the app is stopped means a restore with nothing to
// restore leaves the app running.
func (c *Client) ResolveRestoreArtifact(ctx context.Context, app, name, image, date string, env map[string]string, s3 S3Config, opts RestoreOptions) (AccessoryArtifact, error) {
	if opts.Database != "" {
		return c.resolveExtraDatabase(ctx, app, name, image, date, env, s3, opts.Database)
	}
	if app == AdoptedApp {
		if det, err := c.ResolveEngine(ctx, app, name, image, env); err == nil && (det.Engine == EnginePostgres || det.Engine == EngineMySQL || det.Engine == EngineMariaDB) {
			return AccessoryArtifact{}, fmt.Errorf("%s holds several databases, each with its own backups: name one with --database", name)
		}
	}
	return c.ResolveAccessoryArtifact(ctx, app, name, image, date, env, s3)
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
	loadPrelude                     string
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
// file read or the client fails. loadPrelude is SQL sent first, in the same
// session.
func (m mysqlTarget) load(sqlPath, marker string) string {
	client, _ := mysqlTools(m.engine)
	prelude := ""
	if m.loadPrelude != "" {
		prelude = "printf '%s\\n' " + ssh.ShellQuote(m.loadPrelude) + " && "
	}
	if m.credFile == "" && prelude == "" {
		return "docker exec -i " + ssh.ShellQuote(m.container) + " " + client + " -u root " + ssh.ShellQuote(m.db) + " < " + ssh.ShellQuote(sqlPath)
	}
	if m.credFile == "" {
		return pipefail(prelude+"cat "+ssh.ShellQuote(sqlPath), "docker exec -i "+ssh.ShellQuote(m.container)+" "+client+" -u root "+ssh.ShellQuote(m.db), marker)
	}
	return pipefail("cat "+ssh.ShellQuote(m.credFile)+" && "+prelude+"cat "+ssh.ShellQuote(sqlPath),
		"docker exec -i "+ssh.ShellQuote(m.container)+" sh -c "+ssh.ShellQuote(mysqlPasswordPrelude+"exec "+client+" -u root "+ssh.ShellQuote(m.db)),
		marker)
}

// mysqlDryRunMarker is the table that proves a <db>_teploy_check database is a
// teploy dry run, not somebody's real database.
const mysqlDryRunMarker = "teploy_dry_run_marker"

// loadTimeout bounds a dump load: long enough for a slow disk (1 MB/s), short
// enough that a wedged server does not keep the app down indefinitely.
func loadTimeout(bytes int64) time.Duration {
	return 10*time.Minute + time.Duration(bytes/(1<<20))*time.Second
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
	check := truncBytes(m.db, 40) + "_teploy_check"
	checkLit := "'" + strings.ReplaceAll(check, "'", "''") + "'"
	// charset | collation | size | the throwaway exists | it is teploy's.
	query := "SELECT CONCAT_WS('|', COALESCE((SELECT DEFAULT_CHARACTER_SET_NAME FROM information_schema.SCHEMATA WHERE SCHEMA_NAME = " + lit + "), ''), " +
		"COALESCE((SELECT DEFAULT_COLLATION_NAME FROM information_schema.SCHEMATA WHERE SCHEMA_NAME = " + lit + "), ''), " +
		"(SELECT COALESCE(SUM(data_length + index_length), 0) FROM information_schema.TABLES WHERE table_schema = " + lit + "), " +
		"(SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME = " + checkLit + "), " +
		"(SELECT COUNT(*) FROM information_schema.TABLES WHERE table_schema = " + checkLit + " AND table_name = '" + mysqlDryRunMarker + "'))"
	out, err := c.exec.Run(ctx, m.client(client, "-N -B -e "+q(query)))
	if err != nil {
		return none, fmt.Errorf("reading database %s (database untouched): %w", m.db, err)
	}
	props := strings.Split(strings.TrimSpace(out), "|")
	exists := len(props) == 5 && props[0] != ""
	charset := ""
	if exists {
		charset = " CHARACTER SET " + props[0] + " COLLATE " + props[1]
	}
	checkExists, checkOurs := len(props) == 5 && props[3] != "0", len(props) == 5 && props[4] != "0"
	if checkExists && !checkOurs {
		return none, fmt.Errorf("a database named %s already exists and is not a teploy dry run; rename it, or it would be overwritten (database untouched)", check)
	}
	liveBytes := int64(0)
	if exists {
		liveBytes, _ = strconv.ParseInt(props[2], 10, 64)
	}
	recreate := func(db string) string {
		return m.client(client, "-e "+q("DROP DATABASE IF EXISTS "+mysqlIdent(db)+"; CREATE DATABASE "+mysqlIdent(db)+charset))
	}

	// A dry run is not replication data: it must not fill the binlog (or
	// reach replicas). Without the privilege to skip it, the binlog needs
	// room for a second copy.
	logBin := false
	if out, err := c.exec.Run(ctx, m.client(client, "-N -B -e "+q("SELECT @@log_bin"))); err == nil {
		logBin = strings.TrimSpace(out) == "1"
	}
	noBinlog := ""
	if _, err := c.exec.Run(ctx, m.client(client, "-e "+q("SET sql_log_bin=0"))); err == nil {
		noBinlog = "SET sql_log_bin=0; "
	}

	var sqlBytes int64
	if out, err := c.exec.Run(ctx, "wc -c < "+q(sqlPath)); err == nil {
		sqlBytes, _ = strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	}
	if !ignoreSpace {
		// A table takes more room than its dump text; a binlogged load takes
		// about a dump's worth again. The dry run happens beside the live
		// database and is dropped before the real load, which replaces it.
		data := sqlBytes + sqlBytes/4
		dryNeed := data
		if logBin && noBinlog == "" {
			dryNeed += sqlBytes
		}
		realNeed := data - liveBytes
		if logBin {
			realNeed += sqlBytes
		}
		need := max(dryNeed, realNeed)
		free, known := c.freeBytes(ctx, "docker exec "+q(m.container)+" ", "/var/lib/mysql")
		if err := requireSpace("loading the dump on the data volume (--ignore-space-check overrides)", free, known, need); err != nil {
			return none, err
		}
	}

	// The dry run: nothing of the live database is touched until the server
	// has accepted the whole dump. Deadlines, because a full disk does not
	// fail MySQL: it waits forever for someone to free space.
	checkT := m
	checkT.db = check
	checkT.loadPrelude = noBinlog
	wctx := context.WithoutCancel(ctx)
	bounded := func(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
		if c.loadDeadline > 0 {
			d = c.loadDeadline
		}
		return context.WithTimeout(parent, d)
	}
	dropCheck := func() error {
		dctx, cancel := bounded(wctx, 2*time.Minute)
		defer cancel()
		_, err := c.exec.Run(dctx, m.client(client, "-e "+q(noBinlog+"DROP DATABASE IF EXISTS "+mysqlIdent(check))))
		return err
	}
	if checkExists {
		// A killed restore's leftover: the marker proves it is ours.
		if err := dropCheck(); err != nil {
			return none, fmt.Errorf("dropping the leftover dry-run database %s (database untouched): %w", check, err)
		}
	}
	fmt.Fprintf(c.out, "Checking that the dump loads (into %s)...\n", check)
	dctx, cancelDry := bounded(ctx, loadTimeout(sqlBytes))
	defer cancelDry()
	// Plain CREATE (no IF NOT EXISTS, no DROP): a database that appeared since
	// the check above makes this fail rather than be overwritten.
	_, dryErr := c.exec.Run(dctx, m.client(client, "-e "+q(noBinlog+"CREATE DATABASE "+mysqlIdent(check)+charset+"; CREATE TABLE "+mysqlIdent(check)+"."+mysqlDryRunMarker+" (id int)")))
	created := dryErr == nil
	if dryErr == nil {
		_, dryErr = c.exec.Run(dctx, checkT.load(sqlPath, workDir+"/check.failed"))
	}
	var dropErr error
	if created {
		dropErr = dropCheck()
	}
	if dryErr != nil {
		// A dry run that takes the server down is not "untouched".
		pctx, cancelPing := bounded(wctx, 30*time.Second)
		defer cancelPing()
		if _, pingErr := c.exec.Run(pctx, m.client(client, "-e "+q("SELECT 1"))); pingErr != nil {
			return none, fmt.Errorf("%w: the dry run failed (%v) and the server no longer answers (%v); the data volume may be full", errRecoveryIncomplete, dryErr, pingErr)
		}
		if dropErr != nil {
			return none, fmt.Errorf("this server cannot load the dump (database untouched, but the dry-run database %s could not be dropped and may be holding the disk: %v): %w", check, dropErr, dryErr)
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
			free, known := c.freeBytes(ctx, "", "/var/tmp")
			if err := requireSpace("the pre-restore safety dump in /var/tmp (--ignore-space-check overrides)", free, known, liveBytes); err != nil {
				return none, err
			}
		}
		dir, err := c.exec.Run(ctx, "umask 077; mktemp -d "+q(recPrefix+"XXXXXX"))
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
		lctx, cancel := bounded(ctx, loadTimeout(sqlBytes))
		defer cancel()
		if _, err := c.exec.Run(lctx, recreate(m.db)); err != nil {
			return fmt.Errorf("recreating database %s: %w", m.db, err)
		}
		if _, err := c.exec.Run(lctx, m.load(sqlPath, workDir+"/load.failed")); err != nil {
			return fmt.Errorf("loading the dump: %w", err)
		}
		return nil
	}()
	if loadErr == nil {
		c.markRestoreCompleted(ctx, &rec)
		return rec, nil
	}
	if safety == "" {
		return none, loadErr
	}
	fmt.Fprintf(c.out, "Restore failed (%v); reloading the safety dump...\n", loadErr)
	rctx, cancelReload := bounded(wctx, loadTimeout(liveBytes))
	defer cancelReload()
	if _, err := c.exec.Run(rctx, recreate(m.db)); err == nil {
		_, err = c.exec.Run(rctx, m.load(safety, workDir+"/reload.failed"))
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

// mongoCheckPrefix names the throwaway database of a restore's dry run.
const mongoCheckPrefix = "teploy_check_"

// mongoCheckExclude keeps a killed restore's throwaway out of restores.
const mongoCheckExclude = mongoCheckPrefix + "*.*"

// mongoShell runs a script in the container's shell (mongosh on 5.x+, the
// legacy mongo shell before that).
func mongoShell(container, script string) string {
	return "docker exec " + ssh.ShellQuote(container) + " sh -c " + ssh.ShellQuote("mongosh --quiet --eval "+ssh.ShellQuote(script)+" || mongo --quiet --eval "+ssh.ShellQuote(script))
}

// mongoCheckDatabases lists leftover dry-run databases.
func (c *Client) mongoCheckDatabases(ctx context.Context, container string) ([]string, error) {
	script := "print(db.getMongo().getDBNames().filter(function(n){return n.indexOf('" + mongoCheckPrefix + "')===0}).join('\\n'))"
	out, err := c.exec.Run(ctx, mongoShell(container, script))
	if err != nil {
		return nil, err
	}
	return splitLines(out), nil
}

// dropMongoCheckDatabases removes dry-run databases, returning what it found.
func (c *Client) dropMongoCheckDatabases(ctx context.Context, container string) ([]string, error) {
	names, err := c.mongoCheckDatabases(ctx, container)
	if err != nil || len(names) == 0 {
		return nil, err
	}
	script := "db.getMongo().getDBNames().filter(function(n){return n.indexOf('" + mongoCheckPrefix + "')===0}).forEach(function(n){db.getSiblingDB(n).dropDatabase()})"
	_, err = c.exec.Run(ctx, mongoShell(container, script))
	return names, err
}

func (c *Client) mongoAlive(ctx context.Context, container string) bool {
	for i := 0; i < 5; i++ {
		if _, err := c.exec.Run(ctx, mongoShell(container, "db.runCommand({ping:1})")); err == nil {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(c.delay()):
		}
	}
	return false
}

// mongoSizeFactor estimates a data set's size from its gzipped archive.
const mongoSizeFactor = 3

// restoreMongoInPlace first proves the archive loads, into one throwaway
// database (its collections named <db>.<coll>, so any database name fits the
// 63-byte limit; admin and config are left out, as they cannot be renamed), so
// an archive the server refuses leaves the data untouched, and a dry run that
// takes mongod down is reported as that. Then a safety mongodump and
// mongorestore --drop; a failed restore reloads the safety archive
// (recovery-incomplete when that fails too). --drop replaces the collections
// in the archive; collections created after the backup survive.
func (c *Client) restoreMongoInPlace(ctx context.Context, container, artifact, recPrefix string, ignoreSpace bool) (RestoreRecovery, error) {
	var none RestoreRecovery
	q := ssh.ShellQuote
	if _, err := c.exec.Run(ctx, "gzip -t "+q(artifact)); err != nil {
		return none, fmt.Errorf("the archive is corrupt (database untouched): %w", err)
	}
	var archiveBytes int64
	if out, err := c.exec.Run(ctx, "wc -c < "+q(artifact)); err == nil {
		archiveBytes, _ = strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	}
	if !ignoreSpace {
		free, known := c.freeBytes(ctx, "docker exec "+q(container)+" ", "/data/db")
		if err := requireSpace("loading the archive on the data volume (--ignore-space-check overrides)", free, known, archiveBytes*mongoSizeFactor); err != nil {
			return none, err
		}
	}
	wctx := context.WithoutCancel(ctx)
	bounded := func(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
		if c.loadDeadline > 0 {
			d = c.loadDeadline
		}
		return context.WithTimeout(parent, d)
	}
	dropCheck := func() {
		dctx, cancel := bounded(wctx, 2*time.Minute)
		defer cancel()
		if _, err := c.dropMongoCheckDatabases(dctx, container); err != nil {
			fmt.Fprintf(c.out, "Warning: could not drop the dry-run databases (%s*): %v\n", mongoCheckPrefix, err)
		}
	}
	dropCheck() // a killed restore's leftover
	fmt.Fprintln(c.out, "Checking that the archive loads...")
	checkDB := mongoCheckPrefix + randSuffix()
	dry := "docker exec -i " + q(container) + " mongorestore --archive --gzip --drop --nsExclude=" + q("admin.*") + " --nsExclude=" + q("config.*") + " --nsExclude=" + q(mongoCheckExclude) +
		" --nsFrom=" + q("$db$.$coll$") + " --nsTo=" + q(checkDB+".$db$.$coll$") + " < " + q(artifact)
	dctx, cancelDry := bounded(ctx, loadTimeout(archiveBytes*mongoSizeFactor))
	defer cancelDry()
	_, dryErr := c.exec.Run(dctx, dry)
	if dryErr != nil && !c.mongoAlive(wctx, container) {
		return none, fmt.Errorf("%w: the dry run failed (%v) and mongod no longer answers; the data volume may be full. Free space and start the accessory; the dry-run database %s remains", errRecoveryIncomplete, dryErr, checkDB)
	}
	dropCheck()
	if dryErr != nil {
		return none, fmt.Errorf("this server cannot load the archive, so nothing was changed (database untouched): %w", dryErr)
	}
	if !ignoreSpace {
		free, known := c.freeBytes(ctx, "", "/var/tmp")
		if err := requireSpace("the pre-restore safety dump in /var/tmp (--ignore-space-check overrides)", free, known, archiveBytes); err != nil {
			return none, err
		}
	}
	dir, err := c.exec.Run(ctx, "umask 077; mktemp -d "+q(recPrefix+"XXXXXX"))
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
		c.markRestoreCompleted(ctx, &rec)
		return rec, nil
	} else {
		if _, rbErr := c.exec.Run(wctx, mongoRestoreCmd(container, safety)); rbErr != nil {
			return rec, fmt.Errorf("%w: mongorestore: %v — reloading the safety dump also failed: %v; the pre-restore data is saved at %s", errRecoveryIncomplete, err, rbErr, safety)
		}
		c.exec.Run(wctx, "rm -rf "+q(rec.Path))
		return none, fmt.Errorf("mongorestore: %w — the previous data was reloaded from the safety dump", err)
	}
}

func splitLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}

// ---- kept recovery copies ----

// RecoveryCopies is what earlier restores of an accessory left behind.
type RecoveryCopies struct {
	Previous []string // postgres databases renamed aside (<db>_teploy_old_*)
	Partial  []string // replacements a killed restore left (<db>_teploy_new_*)
	// Throwaway are dry-run databases a killed restore left (mysql, mongo).
	Throwaway []string
	Paths     []string // pre-restore safety copies in /var/tmp
	// Unconfirmed are the Paths whose restore never finished: each may be
	// the only copy of the data.
	Unconfirmed []string
	// MissingLive are databases that do not exist while copies of them do:
	// the kept copy may be all that is left.
	MissingLive []string
	engine      string
	container   string
	pg          pgTarget
	version     int
}

// All lists every kept copy.
func (rc RecoveryCopies) All() []string {
	var out []string
	out = append(out, rc.Previous...)
	out = append(out, rc.Partial...)
	out = append(out, rc.Throwaway...)
	return append(out, rc.Paths...)
}

// FindRecoveryCopies lists an accessory's kept recovery copies; database
// scopes postgres copies to one database ("" means every database in the
// cluster, as a `--database` restore keeps copies of its own).
func (c *Client) FindRecoveryCopies(ctx context.Context, app, name, image, database string, env map[string]string) (RecoveryCopies, error) {
	rc := RecoveryCopies{container: ContainerName(app, name)}
	det, err := c.ResolveEngine(ctx, app, name, image, env)
	if err != nil {
		return rc, err
	}
	rc.engine = det.Engine
	switch det.Engine {
	case EnginePostgres:
		db, user := postgresDBAndUser(app, env)
		rc.pg = newPGTarget(rc.container, db, user, env)
		cl, err := c.pgClusterState(ctx, rc.pg)
		if err != nil {
			return rc, err
		}
		rc.version = cl.version
		names, err := c.pgDatabaseNames(ctx, rc.pg)
		if err != nil {
			return rc, err
		}
		live := map[string]bool{}
		for _, n := range names {
			if !pgLeftoverName.MatchString(n) {
				live[truncBytes(n, 40)] = true
			}
		}
		missing := map[string]bool{}
		for _, n := range names {
			m := pgLeftoverName.FindStringSubmatch(n)
			if m == nil || (database != "" && m[1] != truncBytes(database, 40)) {
				continue
			}
			if m[2] == "old" {
				rc.Previous = append(rc.Previous, n)
			} else {
				rc.Partial = append(rc.Partial, n)
			}
			if !live[m[1]] && !missing[m[1]] {
				missing[m[1]] = true
				rc.MissingLive = append(rc.MissingLive, m[1])
			}
		}
	case EngineMySQL, EngineMariaDB:
		_, left, err := c.mysqlReadCoverage(ctx, det.Engine, rc.container, mysqlDB(app, env))
		if err != nil {
			return rc, err
		}
		rc.Throwaway = left
	case EngineMongo:
		left, err := c.mongoCheckDatabases(ctx, rc.container)
		if err != nil {
			return rc, err
		}
		rc.Throwaway = left
	}
	out, err := c.exec.Run(ctx, "for d in "+ssh.ShellQuote(recoveryPrefix(app, name))+"*; do [ -d \"$d\" ] || continue; if [ -e \"$d/"+restoreCompletedMarker+"\" ]; then printf 'done %s\\n' \"$d\"; else printf 'open %s\\n' \"$d\"; fi; done; true")
	if err == nil {
		for _, line := range splitLines(out) {
			state, path, ok := strings.Cut(line, " ")
			if !ok {
				continue
			}
			rc.Paths = append(rc.Paths, path)
			if state == "open" {
				rc.Unconfirmed = append(rc.Unconfirmed, path)
			}
		}
	}
	return rc, nil
}

// ReleaseRecoveryCopies confirms a restore: it discards the kept copies. It
// refuses when one may be the only copy of the data: while a database is
// missing (a killed restore, a failed swap back), or, without force, when a
// safety copy's restore never recorded that it finished.
func (c *Client) ReleaseRecoveryCopies(ctx context.Context, rc RecoveryCopies, force bool) ([]string, error) {
	if err := rc.CheckRelease(force); err != nil {
		return nil, err
	}
	var released []string
	var errs []error
	for _, db := range append(append([]string{}, rc.Previous...), rc.Partial...) {
		if err := c.pgDropDB(ctx, rc.pg, db, rc.version); err != nil {
			errs = append(errs, fmt.Errorf("dropping %s: %w", db, err))
			continue
		}
		released = append(released, db)
	}
	for _, db := range rc.Throwaway {
		var err error
		if rc.engine == EngineMongo {
			_, err = c.exec.Run(ctx, mongoShell(rc.container, "db.getSiblingDB('"+db+"').dropDatabase()"))
		} else {
			_, err = c.exec.Run(ctx, mysqlEnvClient(rc.engine, rc.container, "DROP DATABASE IF EXISTS "+mysqlIdent(db)))
		}
		if err != nil {
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

// CheckRelease says why releasing these copies would be unsafe.
func (rc RecoveryCopies) CheckRelease(force bool) error {
	if len(rc.MissingLive) > 0 {
		var copies []string
		for _, n := range append(append([]string{}, rc.Previous...), rc.Partial...) {
			if m := pgLeftoverName.FindStringSubmatch(n); m != nil && slices.Contains(rc.MissingLive, m[1]) {
				copies = append(copies, n)
			}
		}
		return fmt.Errorf("refusing to release: no database named %s exists, so %s may be all that is left of it; use --rollback", strings.Join(rc.MissingLive, ", "), strings.Join(copies, ", "))
	}
	if len(rc.Unconfirmed) > 0 && !force {
		return fmt.Errorf("refusing to release: the restore that made %s never recorded finishing, so each may be the only copy of the data. Check the live database; --force discards them anyway", strings.Join(rc.Unconfirmed, ", "))
	}
	return nil
}

// RollbackPlan is a checked rollback, decided before the app is stopped.
type RollbackPlan struct {
	Previous string // the kept database to swap back
	Database string // the database it becomes
	t        pgTarget
	version  int
}

// PlanRollback picks the kept previous database to swap back into database
// ("" = the accessory's primary); pick selects one when several are kept. No
// app is stopped for a rollback with nothing to do.
func (rc RecoveryCopies) PlanRollback(database, pick string) (RollbackPlan, error) {
	if rc.engine != EnginePostgres || len(rc.Previous) == 0 {
		if len(rc.Paths) > 0 {
			return RollbackPlan{}, fmt.Errorf("no previous database is kept; the pre-restore safety copies are %s (load one with the engine's client)", strings.Join(rc.Paths, ", "))
		}
		return RollbackPlan{}, fmt.Errorf("no previous database is kept for this accessory")
	}
	t := rc.pg
	if database != "" {
		t.db = database
	}
	prefix := truncBytes(t.db, 40)
	var candidates []string
	for _, n := range rc.Previous {
		if m := pgLeftoverName.FindStringSubmatch(n); m != nil && m[1] == prefix {
			candidates = append(candidates, n)
		}
	}
	switch {
	case pick != "":
		if !slices.Contains(candidates, pick) {
			return RollbackPlan{}, fmt.Errorf("%s is not a kept previous database of %s (kept: %s)", pick, t.db, strings.Join(rc.Previous, ", "))
		}
		return RollbackPlan{Previous: pick, Database: t.db, t: t, version: rc.version}, nil
	case len(candidates) == 1:
		return RollbackPlan{Previous: candidates[0], Database: t.db, t: t, version: rc.version}, nil
	case len(candidates) == 0:
		return RollbackPlan{}, fmt.Errorf("no previous database of %s is kept (kept: %s); name the one to roll back with --database", t.db, strings.Join(rc.Previous, ", "))
	}
	return RollbackPlan{}, fmt.Errorf("several previous databases of %s are kept (%s): pick one with --recovery-database", t.db, strings.Join(candidates, ", "))
}

// RollbackRestore swaps the planned previous database back into place. The
// database it displaces stays, renamed as a partial copy, until --release. The
// caller has quiesced the app.
func (c *Client) RollbackRestore(ctx context.Context, plan RollbackPlan) (displaced string, err error) {
	t := plan.t
	displaced = truncBytes(t.db, 40) + "_teploy_new_" + randSuffix()
	st, err := c.pgSwapState(ctx, t, plan.Previous, displaced)
	if err != nil {
		return "", fmt.Errorf("reading the databases: %w", err)
	}
	// pgSwap's roles reversed: previous -> live, live -> displaced. A live
	// database may be missing after a recovery-incomplete restore.
	if err := c.pgSwap(ctx, t, plan.Previous, displaced, st.live); err != nil {
		wctx := context.WithoutCancel(ctx)
		after, stErr := c.pgSwapState(wctx, t, plan.Previous, displaced)
		switch {
		case stErr != nil:
			return "", fmt.Errorf("%w: the rollback failed (%v) and the server no longer answers (%v)", errRecoveryIncomplete, err, stErr)
		case after.live && !after.replacement && (after.previous || !st.live):
			// Committed: only the reply was lost.
		default:
			return "", fmt.Errorf("the rollback did not happen, nothing changed: %w", err)
		}
	}
	if !st.live {
		displaced = ""
	}
	return displaced, nil
}

// ---- mysql coverage ----

// mysqlEnvClient runs one SQL statement with the password the container's own
// env holds (never argv); no credential file is needed for read-only checks.
func mysqlEnvClient(engine, container, sql string, flags ...string) string {
	client, _ := mysqlTools(engine)
	return "docker exec " + ssh.ShellQuote(container) + " sh -c " + ssh.ShellQuote("MYSQL_PWD=${MARIADB_ROOT_PASSWORD:-${MYSQL_ROOT_PASSWORD:-${MYSQL_PASSWORD:-}}}; export MYSQL_PWD; exec "+client+" -u root "+strings.Join(flags, " ")+" -e "+ssh.ShellQuote(sql))
}

// mysqlDumpableDB is addressableDB minus what mysqldump itself mangles: it
// backslash-escapes a quote in the schema name inside its own USE statement and
// then fails with "Unknown database" (seen on mysql 8 and mariadb-dump). Such a
// schema is reported as not backed up, loudly.
func mysqlDumpableDB(db string) bool {
	return addressableDB(db) && !strings.ContainsAny(db, `'\`)
}

// mysqlCoverage decides which of the server's schemas a backup with this listing
// stores, the shape pgReadCoverage gives postgres. A schema it cannot hand to
// the tools literally stays uncovered, whatever the listing says.
func (c *Client) mysqlCoverage(ctx context.Context, engine, container, db string, listed []string) (pgCoverage, error) {
	var cov pgCoverage
	others, leftovers, err := c.mysqlReadCoverage(ctx, engine, container, db)
	if err != nil {
		return cov, err
	}
	cov.Throwaway = leftovers
	all := false
	want := map[string]bool{}
	for _, l := range listed {
		if l == "all" {
			all = true
		}
		want[l] = true
	}
	for _, name := range others {
		if (all || want[name]) && mysqlDumpableDB(name) {
			cov.Extra = append(cov.Extra, name)
		} else {
			cov.Uncovered = append(cov.Uncovered, name)
		}
	}
	if db != "" {
		exists, err := c.mysqlSchemaExists(ctx, engine, container, db)
		if err != nil {
			return cov, err
		}
		cov.PrimaryMissing = !exists
	}
	for _, l := range listed {
		if l != "all" && l != db && !slices.Contains(others, l) {
			cov.Missing = append(cov.Missing, l)
		}
	}
	return cov, nil
}

func (c *Client) mysqlSchemaExists(ctx context.Context, engine, container, db string) (bool, error) {
	out, err := c.exec.Run(ctx, mysqlEnvClient(engine, container, "SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME = '"+strings.ReplaceAll(db, "'", "''")+"'", "-N", "-B"))
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) != "0", nil
}

// mysqlReadCoverage lists the server's other schemas: the dump holds one, so
// the rest are not backed up. A schema carrying the dry-run marker is a killed
// restore's leftover, reported apart.
func (c *Client) mysqlReadCoverage(ctx context.Context, engine, container, db string) (uncovered, leftovers []string, err error) {
	q := "SELECT s.schema_name, (SELECT COUNT(*) FROM information_schema.TABLES t WHERE t.table_schema = s.schema_name AND t.table_name = '" + mysqlDryRunMarker + "') FROM information_schema.SCHEMATA s WHERE s.schema_name NOT IN ('mysql', 'information_schema', 'performance_schema', 'sys')"
	out, err := c.exec.Run(ctx, mysqlEnvClient(engine, container, q, "-N", "-B"))
	if err != nil {
		return nil, nil, err
	}
	for _, line := range splitLines(out) {
		f := strings.Split(line, "\t")
		if len(f) != 2 || f[0] == db {
			continue
		}
		if f[1] != "0" {
			leftovers = append(leftovers, f[0])
		} else {
			uncovered = append(uncovered, f[0])
		}
	}
	return uncovered, leftovers, nil
}
