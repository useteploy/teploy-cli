package backup

import (
	"bytes"
	"context"
	"encoding/json"
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

// BackupOptions tune an accessory backup.
type BackupOptions struct {
	// AllDatabases backs up every database on the server, each as its own
	// object: what a policy-managed run does, so a database created tomorrow is
	// covered with no action.
	AllDatabases bool
	// Databases names exactly the extra databases to back up (an adopted
	// entry's databases: [...]). A listed database that is missing is a failure.
	Databases []string
	// SingleDatabase backs up only this database (the safety backup before a
	// `restore --database`).
	SingleDatabase string
	// IgnoreSpace skips the free-space preflight.
	IgnoreSpace bool
	// SafetyCopy marks what this backup stores as a destructive operation's way
	// back: retention leaves it alone for safetyWindow.
	SafetyCopy bool
}

// PrimaryDatabase names the database a default restore of this accessory would
// replace ("" for an engine without named databases).
func (c *Client) PrimaryDatabase(ctx context.Context, app, name, image string, env map[string]string) string {
	det, err := c.ResolveEngine(ctx, app, name, image, env)
	if err != nil {
		return ""
	}
	switch det.Engine {
	case EnginePostgres:
		db, _ := postgresDBAndUser(app, env)
		return db
	case EngineMySQL, EngineMariaDB:
		return mysqlDB(app, env)
	}
	return ""
}

// BackupDeadline bounds one backup of one target, so a wedged dump or upload
// cannot hold a run (and its lock) forever.
const BackupDeadline = 6 * time.Hour

func (c *Client) backupDeadline() time.Duration {
	if c.deadline > 0 {
		return c.deadline
	}
	return BackupDeadline
}

// backupTmpDir is where a backup builds its artifacts: disk, not the tmpfs /tmp
// is on many hosts, which a database dump would fill along with the RAM.
func backupTmpDir() string {
	if d := strings.TrimSpace(os.Getenv("TEPLOY_BACKUP_TMPDIR")); d != "" {
		return strings.TrimRight(d, "/")
	}
	return "/var/tmp"
}

func (c *Client) newBackupWorkspace(ctx context.Context) (string, func(), error) {
	runOut, err := c.exec.Run(ctx, "umask 077; mktemp -d "+ssh.ShellQuote(backupTmpDir()+"/teploy-backup.XXXXXXXX"))
	if err != nil {
		return "", nil, fmt.Errorf("creating backup workspace: %w", err)
	}
	workDir := strings.TrimSpace(runOut)
	// Says how long this run may live, for a janitor that does not know its deadline.
	// Best effort: without it the fixed leftover age applies.
	c.exec.Run(ctx, fmt.Sprintf(`printf '%%s\n' "$(( $(date +%%s) + %d ))" > %s`, int64((c.backupDeadline()+time.Hour).Seconds()), ssh.ShellQuote(workDir+"/"+workspaceExpiry)))
	return workDir, func() {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		c.exec.Run(rctx, "rm -rf "+ssh.ShellQuote(workDir))
	}, nil
}

// AccessoryBackup performs a database-aware backup for an accessory.
func (c *Client) AccessoryBackup(ctx context.Context, app, name, image string, env map[string]string, s3 S3Config) (BackupResult, error) {
	return c.AccessoryBackupWith(ctx, app, name, image, env, s3, BackupOptions{})
}

// AccessoryBackupWith is AccessoryBackup with options.
//
// All intermediate artifacts (dumps, snapshots, scratch tars) are built
// inside one private 0700 mktemp workspace and removed on every exit — the
// old predictable /tmp/<app>-… paths were shared across accessories of the
// same app and exposed database dumps to other local users (audit F34).
//
// Objects are stored sidecars first, primary last (see store.go), and one
// database failing does not stop the others: the result names it and the
// returned error makes the backup a failure, so nothing is pruned.
func (c *Client) AccessoryBackupWith(ctx context.Context, app, name, image string, env map[string]string, s3 S3Config, opts BackupOptions) (res BackupResult, err error) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, c.backupDeadline())
	defer cancel()
	defer func() { res.DurationMs = time.Since(start).Milliseconds() }()
	if err := c.ensureAWSCLI(ctx); err != nil {
		return res, err
	}
	det, err := c.ResolveEngine(ctx, app, name, image, env)
	if err != nil {
		return res, err
	}
	res.Engine = det.Engine
	res.Encrypted = s3.Encrypted()
	if err := c.prepareEncryption(ctx, s3); err != nil {
		return res, err
	}
	adopted := app == AdoptedApp
	if adopted && !det.IsDB() || adopted && det.Engine == EngineNucleus {
		return res, fmt.Errorf("adopted container %s: engine %q has no adopted backup method (adopted containers need a database engine; set engine: in the policy entry if detection is wrong)", name, det.Engine)
	}

	timestamp, err := newBackupID(time.Now())
	if err != nil {
		return res, err
	}
	res.ID = timestamp
	containerName := ContainerName(app, name)

	workDir, cleanup, err := c.newBackupWorkspace(ctx)
	if err != nil {
		return res, err
	}
	defer cleanup()

	// What this backup covers. A server with more databases than it covers is
	// still backed up: a partial backup and a loud warning beat none.
	var pgT pgTarget
	if det.Engine == EnginePostgres {
		db, user := postgresDBAndUser(app, env)
		if adopted {
			db = ""
		}
		pgT = newPGTarget(containerName, db, user, env)
	}
	coverage, cerr := c.coverageWith(ctx, app, name, det, pgT, env, opts)
	res.UncoveredDatabases = coverage.Uncovered
	res.Warnings = coverage.warningsFor(app, name, det, env)
	multi := opts.AllDatabases || len(opts.Databases) > 0 || opts.SingleDatabase != "" || adopted
	if cerr != nil {
		if multi && (det.Engine == EnginePostgres || det.Engine == EngineMySQL || det.Engine == EngineMariaDB) {
			return res, fmt.Errorf("listing the databases of %s, so what to back up is unknown: %w", containerName, cerr)
		}
		res.Warnings = append(res.Warnings, cerr.Error())
	}
	for _, w := range res.Warnings {
		fmt.Fprintf(c.out, "Warning: %s\n", w)
	}
	fmt.Fprintf(c.out, "Backing up %s (%s, detected from %s)...\n", containerName, det.Engine, det.Source)

	failed := map[string]string{}
	for _, missing := range coverage.Missing {
		failed[missing] = "listed for backup but not on the server"
	}

	primary := true
	switch {
	case opts.SingleDatabase != "" && (!coverage.PrimaryIsSingle || coverage.PrimaryMissing):
		primary = false
	case (det.Engine == EnginePostgres || det.Engine == EngineMySQL || det.Engine == EngineMariaDB) && (adopted || (multi && coverage.PrimaryMissing)):
		// Nothing named this container's primary database: every database has
		// its own series. Pruning sees that as one series per database.
		primary = false
	}

	// Sidecars and extra databases first: the primary object commits the backup.
	// No database, no sidecar: a roles file alone is not a backup of anything.
	if det.Engine == EnginePostgres && (primary || len(coverage.Extra) > 0) {
		if w := c.backupPostgresGlobals(ctx, pgT, timestamp, workDir, s3, app, name); w != "" {
			res.Warnings = append(res.Warnings, w)
			fmt.Fprintf(c.out, "Warning: %s\n", w)
		}
	}
	for i, db := range coverage.Extra {
		obj, derr := c.backupOneDatabase(ctx, app, name, det, env, db, workDir, i, timestamp, s3, opts)
		if derr != nil {
			failed[db] = derr.Error()
			fmt.Fprintf(c.out, "Backing up database %s FAILED: %v\n", db, derr)
			continue
		}
		res.Objects = append(res.Objects, obj)
		res.SizeBytes += obj.Size
		fmt.Fprintf(c.out, "Backed up database %s\n", db)
	}

	if primary {
		obj, consistency, perr := c.backupPrimary(ctx, app, name, det, env, pgT, workDir, timestamp, s3, opts)
		res.Consistency = consistency
		if perr != nil {
			return res, joinDatabaseFailures(failed, perr)
		}
		res.Key = obj.Key
		res.SizeBytes += obj.Size
		res.Objects = append(res.Objects, obj)
	} else if plan, perr := planAccessoryDump(app, name, det, env, workDir); perr == nil {
		res.Consistency = plan.consistency
	}
	if res.Key == "" && len(res.Objects) > 0 {
		res.Key = res.Objects[0].Key
	}
	if len(res.Objects) == 0 && len(failed) == 0 {
		// An empty server (no schema yet) has nothing to store. Not a failure,
		// but never a silent success either.
		res.Warnings = append(res.Warnings, containerName+" holds no database yet, so nothing was stored")
		return res, nil
	}
	res.FailedDatabases = failed
	c.markNamespaceOwner(ctx, s3, app)
	if opts.SafetyCopy {
		c.markSafetyCopy(ctx, s3, seriesPrefix(s3, app, name, ""), timestamp, &res)
	}
	if len(failed) > 0 {
		return res, joinDatabaseFailures(failed, nil)
	}
	fmt.Fprintf(c.out, "Backup complete: %s\n", res.Key)
	return res, nil
}

// markSafetyCopy records an id as a safety copy. Failing to is reported, not fatal:
// the copy exists, and only its protection from retention is lost.
func (c *Client) markSafetyCopy(ctx context.Context, s3 S3Config, prefix, id string, res *BackupResult) {
	key := prefix + safetyDir + "/" + id + ".safety"
	if _, err := c.exec.Run(ctx, "printf '%s\\n' safety | "+s3.AWS("s3 cp - "+ssh.ShellQuote(key))); err != nil {
		res.Warnings = append(res.Warnings, fmt.Sprintf("could not protect safety copy %s from retention: %v", id, err))
	}
}

// joinDatabaseFailures renders per-database failures into one error.
func joinDatabaseFailures(failed map[string]string, primary error) error {
	if len(failed) == 0 {
		return primary
	}
	names := make([]string, 0, len(failed))
	for db := range failed {
		names = append(names, db)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, db := range names {
		parts = append(parts, db+": "+failed[db])
	}
	msg := "database(s) not backed up: " + strings.Join(parts, "; ")
	if primary != nil {
		return fmt.Errorf("%w; %s", primary, msg)
	}
	return errors.New(msg)
}

// backupPrimary dumps and stores the accessory's primary object: the backup's
// commit point. It is also the only object a single-database engine has.
func (c *Client) backupPrimary(ctx context.Context, app, name string, det EngineDetection, env map[string]string, pgT pgTarget, workDir, id string, s3 S3Config, opts BackupOptions) (BackupObject, string, error) {
	var obj BackupObject
	plan, err := planAccessoryDump(app, name, det, env, workDir)
	if err != nil {
		return obj, "", err
	}
	if err := c.preflightDumpSpace(ctx, app, name, det, env, pgT, workDir, opts); err != nil {
		return obj, plan.consistency, err
	}
	if err := plan.stage(ctx, c.exec); err != nil {
		return obj, plan.consistency, fmt.Errorf("staging the mysql credential file for %s (no dump was run): %w", name, err)
	}
	if _, err := c.exec.Run(ctx, plan.cmd); err != nil {
		// Never leave partial backups around: they're disk-fillers at best,
		// restore-bait at worst.
		return obj, plan.consistency, fmt.Errorf("dumping %s: %w", name, err)
	}
	size, err := c.checkArtifactSize(ctx, plan.artifactPath, plan.minBytes)
	if err != nil {
		return obj, plan.consistency, fmt.Errorf("dumping %s: %w", name, err)
	}
	if det.Engine == EnginePostgres {
		c.storeDatabaseMeta(ctx, pgT, pgT.db, s3, seriesPrefix(s3, app, name, "")+"meta/"+id+".json", workDir)
	}
	key := seriesPrefix(s3, app, name, "") + id + plan.ext
	fmt.Fprintf(c.out, "Uploading to %s...\n", key)
	stored, err := c.put(ctx, s3, plan.artifactPath, key)
	if err != nil {
		return obj, plan.consistency, fmt.Errorf("storing %s: %w", name, err)
	}
	_ = size
	return BackupObject{Key: stored.Key, Size: stored.Size, Encrypted: stored.Encrypted}, plan.consistency, nil
}

// backupOneDatabase dumps and stores one more database as its own object.
// Series objects sit below the accessory's backups, so primary listings see one
// backup per id; `backup restore --database` restores one.
func (c *Client) backupOneDatabase(ctx context.Context, app, name string, det EngineDetection, env map[string]string, db, workDir string, n int, id string, s3 S3Config, opts BackupOptions) (BackupObject, error) {
	obj := BackupObject{Database: db}
	sub := fmt.Sprintf("%s/db-%d", workDir, n)
	if _, err := c.exec.Run(ctx, "mkdir -p "+ssh.ShellQuote(sub)); err != nil {
		return obj, err
	}
	defer c.exec.Run(context.WithoutCancel(ctx), "rm -rf "+ssh.ShellQuote(sub))
	envDB := envForDatabase(env, det.Engine, db)
	plan, err := planAccessoryDump(app, name, det, envDB, sub)
	if err != nil {
		return obj, err
	}
	var pgT pgTarget
	if det.Engine == EnginePostgres {
		_, user := postgresDBAndUser(app, env)
		pgT = newPGTarget(ContainerName(app, name), db, user, env)
	}
	if err := c.preflightDumpSpace(ctx, app, name, det, envDB, pgT, sub, opts); err != nil {
		return obj, err
	}
	if err := plan.stage(ctx, c.exec); err != nil {
		return obj, fmt.Errorf("staging the credential file (no dump was run): %w", err)
	}
	if _, err := c.exec.Run(ctx, plan.cmd); err != nil {
		return obj, err
	}
	if _, err := c.checkArtifactSize(ctx, plan.artifactPath, plan.minBytes); err != nil {
		return obj, err
	}
	series := seriesPrefix(s3, app, name, db)
	if det.Engine == EnginePostgres {
		c.storeDatabaseMeta(ctx, pgT, db, s3, series+"meta/"+id+".json", sub)
	}
	stored, err := c.put(ctx, s3, plan.artifactPath, series+id+plan.ext)
	if err != nil {
		return obj, err
	}
	obj.Key, obj.Size, obj.Encrypted = stored.Key, stored.Size, stored.Encrypted
	return obj, nil
}

// envForDatabase is the accessory's env with its database variables naming db,
// so the shared planner dumps that database with the same flags and checks.
func envForDatabase(env map[string]string, engine, db string) map[string]string {
	out := make(map[string]string, len(env)+2)
	for k, v := range env {
		out[k] = v
	}
	switch engine {
	case EnginePostgres:
		out["POSTGRES_DB"] = db
		delete(out, "POSTGRESQL_DATABASE")
	case EngineMySQL, EngineMariaDB:
		out["MYSQL_DATABASE"] = db
		delete(out, "MARIADB_DATABASE")
	}
	return out
}

// dumpReserveBytes is free space a dump must leave: a database on the same
// filesystem as the workspace panics when the disk fills.
const dumpReserveBytes = 256 << 20

// preflightDumpSpace refuses a dump that would fill the workspace's disk: on a
// small host the same disk holds the live database. It measures what the backup
// will occupy, plaintext and ciphertext at once, not the size of the live data
// alone. A size it cannot measure is not a refusal.
func (c *Client) preflightDumpSpace(ctx context.Context, app, name string, det EngineDetection, env map[string]string, pgT pgTarget, workDir string, opts BackupOptions) error {
	if opts.IgnoreSpace {
		return nil
	}
	q := ssh.ShellQuote
	container := ContainerName(app, name)
	var live int64
	var known bool
	readInt := func(out string, err error) (int64, bool) {
		if err != nil {
			return 0, false
		}
		f := strings.Fields(out)
		if len(f) == 0 {
			return 0, false
		}
		n, perr := strconv.ParseInt(f[0], 10, 64)
		return n, perr == nil
	}
	var need int64
	switch det.Engine {
	case EnginePostgres:
		live, known = readInt(c.exec.Run(ctx, pgT.psql(pgT.db, "SELECT pg_database_size(current_database())")))
		// A compressed custom-format archive, plus its ciphertext.
		need = live
	case EngineMySQL, EngineMariaDB:
		sql := "SELECT COALESCE(SUM(data_length + index_length), 0) FROM information_schema.TABLES WHERE table_schema = '" + strings.ReplaceAll(mysqlDB(app, env), "'", "''") + "'"
		live, known = readInt(c.exec.Run(ctx, mysqlEnvClient(det.Engine, container, sql, "-N", "-B")))
		// The plain SQL and its gzip side by side, then the gzip and its ciphertext.
		need = live + live/2
	case EngineMongo:
		live, known = readInt(c.exec.Run(ctx, "docker exec "+q(container)+" du -sk /data/db"))
		live *= 1024
		need = live
	case EngineRedis, EngineValkey, EngineDragonfly:
		live, known = readInt(c.exec.Run(ctx, "docker exec "+q(container)+" du -sk /data"))
		live *= 1024
		need = live
	default:
		live, known = readInt(c.exec.Run(ctx, "du -sk "+q(fmt.Sprintf("%s/%s/accessories/%s", deploymentsDir, app, name))))
		live *= 1024
		need = live
	}
	if !known {
		return nil
	}
	free, freeKnown := c.freeBytes(ctx, "", workDir)
	if freeKnown && free < need+dumpReserveBytes {
		return fmt.Errorf("not enough free space in %s for this dump: %d bytes free, about %d needed plus a %d byte reserve (live size %d); nothing was dumped. Set TEPLOY_BACKUP_TMPDIR to a larger disk or pass --ignore-space-check", workDir, free, need, int64(dumpReserveBytes), live)
	}
	return nil
}

// postgresGlobalsKey is where a postgres backup's cluster globals (roles,
// tablespaces: pg_dumpall --globals-only) are stored beside the dump. It
// sits one directory below the accessory's backups, so listings, latest
// resolution and retention counting never see it as a backup of its own.
func postgresGlobalsKey(s3 S3Config, app, name, id string) string {
	return seriesPrefix(s3, app, name, "") + "globals/" + id + ".sql.gz"
}

// backupPostgresGlobals stores the cluster's roles beside the dump so a
// restore into a fresh cluster (and verify-backup's scratch) can recreate
// object ownership. Best effort: the database dump is the backup. A failure
// comes back as a warning, so it is reported and not only printed.
func (c *Client) backupPostgresGlobals(ctx context.Context, t pgTarget, id, workDir string, s3 S3Config, app, name string) string {
	gz := workDir + "/globals.sql.gz"
	if err := c.dumpPostgresGlobals(ctx, t, workDir, gz); err != nil {
		return fmt.Sprintf("the cluster roles (pg_dumpall --globals-only) were not saved: %v", err)
	}
	if _, err := c.put(ctx, s3, gz, postgresGlobalsKey(s3, app, name, id)); err != nil {
		return fmt.Sprintf("uploading the cluster roles failed: %v", err)
	}
	return ""
}

// dumpPostgresGlobals writes pg_dumpall --globals-only, gzipped, to gz. A
// role that may not read pg_authid gets it without role passwords.
func (c *Client) dumpPostgresGlobals(ctx context.Context, t pgTarget, workDir, gz string) error {
	q := ssh.ShellQuote
	sql := workDir + "/globals.sql"
	dumpall := t.tool("pg_dumpall", false) + " --globals-only"
	cmd := "{ " + dumpall + " -U " + q(t.user) + " > " + q(sql) + " 2>/dev/null || " + dumpall + " --no-role-passwords -U " + q(t.user) + " > " + q(sql) + "; } && grep -q 'CREATE ROLE' " + q(sql) + " && gzip -c " + q(sql) + " > " + q(gz)
	_, err := c.exec.Run(ctx, cmd)
	return err
}

// applyPostgresGlobals loads a gzipped globals file as super. Errors for
// roles that already exist are expected, so it never stops on them; a dump
// that needs a missing role still fails, loudly, when it loads.
func (c *Client) applyPostgresGlobals(ctx context.Context, super pgTarget, gz string) {
	q := ssh.ShellQuote
	sql := strings.TrimSuffix(gz, ".gz")
	c.exec.Run(ctx, "gunzip -c "+q(gz)+" > "+q(sql)+" && "+super.tool("psql", true)+" -X -q -U "+q(super.user)+" -d postgres < "+q(sql)+" >/dev/null 2>&1; true")
}

// backupCoverage says what a default backup of this accessory leaves out, per
// engine (the DR bundle's question; it stores one object per accessory).
func (c *Client) backupCoverage(ctx context.Context, app, name string, det EngineDetection, t pgTarget, env map[string]string) (pgCoverage, []string, []string) {
	cov, err := c.coverageWith(ctx, app, name, det, t, env, BackupOptions{})
	w := cov.warningsFor(app, name, det, env)
	if err != nil {
		w = append(w, err.Error())
	}
	return cov, cov.Uncovered, w
}

// coverageWith works out which databases a backup with these options stores.
// Mongo dumps the whole server, so its killed-restore throwaways are dropped
// first: they would otherwise land in this backup silently.
func (c *Client) coverageWith(ctx context.Context, app, name string, det EngineDetection, t pgTarget, env map[string]string, opts BackupOptions) (pgCoverage, error) {
	container := ContainerName(app, name)
	listed := func() []string {
		switch {
		case opts.SingleDatabase != "":
			return []string{opts.SingleDatabase}
		case opts.AllDatabases:
			return []string{"all"}
		case len(opts.Databases) > 0:
			return opts.Databases
		}
		labels, _ := c.inspectEngineHints(ctx, container)
		return listedDatabases(labels)
	}
	switch det.Engine {
	case EnginePostgres:
		cov, err := c.pgReadCoverage(ctx, t, listed())
		if err != nil {
			return cov, fmt.Errorf("could not list the cluster's databases, so coverage is unknown: %w", err)
		}
		cov.applySingle(t.db, opts)
		return cov, nil
	case EngineMySQL, EngineMariaDB:
		db := mysqlDB(app, env)
		if app == AdoptedApp {
			db = ""
		}
		cov, err := c.mysqlCoverage(ctx, det.Engine, container, db, listed())
		if err != nil {
			return cov, fmt.Errorf("could not list the server's schemas, so coverage is unknown: %w", err)
		}
		cov.applySingle(db, opts)
		return cov, nil
	case EngineMongo:
		dropped, err := c.dropMongoCheckDatabases(ctx, container)
		if err != nil {
			return pgCoverage{}, fmt.Errorf("could not clear dry-run databases, so this backup may include them: %w", err)
		}
		return pgCoverage{DroppedThrowaway: dropped}, nil
	}
	return pgCoverage{}, nil
}

// applySingle narrows a coverage to the one database a safety backup is for.
func (cov *pgCoverage) applySingle(primary string, opts BackupOptions) {
	if opts.SingleDatabase == "" {
		return
	}
	// A database that does not exist has nothing to protect: restoring it after
	// it was dropped is the point, not a failure of its safety backup.
	cov.Uncovered, cov.Missing = nil, nil
	cov.PrimaryIsSingle = opts.SingleDatabase == primary
	if cov.PrimaryIsSingle {
		cov.Extra = nil
	}
}

// warningsFor renders the coverage problems for messages and events, by engine.
func (cov pgCoverage) warningsFor(app, name string, det EngineDetection, env map[string]string) []string {
	switch det.Engine {
	case EnginePostgres:
		db, _ := postgresDBAndUser(app, env)
		return cov.warnings(db)
	case EngineMySQL, EngineMariaDB:
		var w []string
		if len(cov.Uncovered) > 0 {
			w = append(w, fmt.Sprintf("this backup covers only %s: the server also holds %s, not backed up", strings.Join(append([]string{mysqlDB(app, env)}, cov.Extra...), ", "), strings.Join(cov.Uncovered, ", ")))
		}
		if len(cov.Throwaway) > 0 {
			w = append(w, fmt.Sprintf("a killed restore left the dry-run database %s: `teploy backup restore --accessory <name> --release` removes it", strings.Join(cov.Throwaway, ", ")))
		}
		return w
	case EngineMongo:
		if len(cov.DroppedThrowaway) > 0 {
			return []string{fmt.Sprintf("dropped the dry-run databases a killed restore left: %s", strings.Join(cov.DroppedThrowaway, ", "))}
		}
	}
	return nil
}

// storeDatabaseMeta stores a database's logical size beside its dump: a
// restore's free-space check needs what the BACKUP will occupy, which
// neither the compressed artifact nor the live database says. Best effort.
func (c *Client) storeDatabaseMeta(ctx context.Context, t pgTarget, db string, s3 S3Config, key, workDir string) {
	out, err := c.exec.Run(ctx, t.psql(db, "SELECT pg_database_size(current_database())"))
	if err != nil {
		return
	}
	size, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil || size <= 0 {
		return
	}
	local := workDir + "/meta.json"
	body, _ := json.Marshal(pgBackupMeta{LogicalBytes: size})
	if err := c.exec.Upload(ctx, bytes.NewReader(body), local, "0600"); err != nil {
		return
	}
	c.put(ctx, s3, local, key)
}

func pgExtraKey(s3 S3Config, app, name, db, id string) string {
	return seriesPrefix(s3, app, name, db) + id + ".pgdump"
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
	containerName := ContainerName(app, name)
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
		t := newPGTarget(containerName, db, user, env)
		p := ssh.ShellQuote(workDir + "/dump.pgdump")
		base.method, base.consistency, base.ext, base.minBytes = "pg_dump", consistencyEngineDump, ".pgdump", minPostgresDump
		base.artifactPath = workDir + "/dump.pgdump"
		base.cmd = fmt.Sprintf("%s -Fc -U %s %s > %s && { [ \"$(head -c 5 %s)\" = PGDMP ] || { echo 'pg_dump did not produce a custom-format archive' >&2; exit 1; }; }",
			t.tool("pg_dump", false), ssh.ShellQuote(user), ssh.ShellQuote(db), p, p)
		return base, nil

	case EngineMySQL, EngineMariaDB:
		client := "mysqldump"
		if det.Engine == EngineMariaDB {
			client = "mariadb-dump"
		}
		dumpArgs := client + " " + mysqlDumpArgs + " -u root " + ssh.ShellQuote(mysqlDB(app, env))
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
			" && gzip -c " + qTmp + " > " + ssh.ShellQuote(dumpPath) + " && rm -f " + qTmp
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
			// The in-container snapshot never outlives the script, on any
			// exit path (a failed copy, a dropped SSH session).
			fmt.Sprintf(`cleanup_snapshot() { docker exec -u 0 %s rm -f "$dir"/%s.rdb >/dev/null 2>&1 || true; }`, qContainer, snap),
			`trap cleanup_snapshot EXIT HUP INT TERM`,
			fmt.Sprintf("out=$(docker exec %s redis-cli save rdb %s 2>&1) || true", qContainer, snap),
			`if [ "$out" != OK ]; then printf 'dragonfly SAVE RDB failed: %s\n' "$out" >&2; exit 1; fi`,
			fmt.Sprintf(`docker cp %s:"$dir"/%s.rdb %s`, qContainer, snap, ssh.ShellQuote(rdb)),
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
		tarCmd := fmt.Sprintf("tar -czf %s -C %s . || [ $? -eq 1 ]", ssh.ShellQuote(workDir+"/dump.tar.gz"), ssh.ShellQuote(accDir))
		if det.Engine == EngineGeneric && det.Source != "label" && det.Source != "config" {
			// Nothing recognised the container, but its files may still be
			// a database teploy has no method for (ClickHouse, Elastic,
			// Qdrant, KeyDB, a postgres fork). A live tar of those is not a
			// backup, so a data-directory marker refuses it while the
			// container runs; a stopped container's files are quiescent.
			base.cmd = liveDBGuard(accDir, containerName) + " && { " + tarCmd + "; }"
		} else {
			base.cmd = tarCmd
		}
		return base, nil
	}
	return accessoryDumpPlan{}, fmt.Errorf("accessory %s: engine %q has no backup method — refusing to fall back to a raw file copy of a database", name, det.Engine)
}

// liveDBMarkers are file names only database data directories contain.
var liveDBMarkers = []string{
	"PG_VERSION",                               // postgres and forks
	"ibdata1", "mysql.ibd", "aria_log_control", // mysql / mariadb
	"WiredTiger", "WiredTiger.wt", // mongo
	"*.rdb", "appendonlydir", "appendonly.aof", "*.dfs", // redis family, dragonfly
	"raft_state.json",                          // qdrant
	"preprocessed_configs", "metadata_dropped", // clickhouse
	"node.lock", // elasticsearch / opensearch
}

// liveDBGuard is the shell check that refuses a raw tar of what looks like
// a running database's data directory.
func liveDBGuard(accDir, container string) string {
	var names []string
	for _, m := range liveDBMarkers {
		names = append(names, "-name "+ssh.ShellQuote(m))
	}
	find := "find " + ssh.ShellQuote(accDir) + " -maxdepth 4 \\( " + strings.Join(names, " -o ") + " \\) -print 2>/dev/null | head -n 1"
	return "m=$(" + find + "); if [ -n \"$m\" ] && [ \"$(docker inspect -f '{{.State.Running}}' " + ssh.ShellQuote(container) + " 2>/dev/null)\" = true ]; then " +
		"echo \"refusing a raw copy of a running database: $m marks a database data directory, and teploy has no engine method for this container — stop the accessory first, or set engine: in teploy.yml (teploy.engine label; generic asserts it is plain files)\" >&2; exit 1; fi"
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
		// Neither in-container snapshot outlives the script, on any exit.
		fmt.Sprintf(`cleanup_snapshots() { docker exec -u 0 %s rm -f "$dir"/%s "$dir"/%s >/dev/null 2>&1 || true; }`, q, in, pre),
		`trap cleanup_snapshots EXIT HUP INT TERM`,
		fmt.Sprintf(`out=$(docker exec %s redis-cli save rdb %s 2>&1) || true`, q, strings.TrimSuffix(pre, ".rdb")),
		`if [ "$out" != OK ]; then printf 'pre-restore snapshot failed (dataset untouched): %s\n' "$out" >&2; exit 1; fi`,
		fmt.Sprintf(`docker cp %s:"$dir"/%s %s`, q, pre, ssh.ShellQuote(preRestore)),
		fmt.Sprintf(`docker cp %s %s:"$dir"/%s`, ssh.ShellQuote(rdb), q, in),
		// docker cp keeps the host uid; the engine must be able to read it.
		fmt.Sprintf(`docker exec -u 0 %s chmod 644 "$dir"/%s`, q, in),
		fmt.Sprintf(`out=$(docker exec %s redis-cli dfly load %s 2>&1) || true`, q, in),
		`if [ "$out" != OK ]; then`,
		fmt.Sprintf(`  back=$(docker exec %s redis-cli dfly load %s 2>&1) || true`, q, pre),
		`  if [ "$back" = OK ]; then printf 'DFLY LOAD of the backup failed (%s); the pre-restore dataset was reloaded\n' "$out" >&2; exit 1; fi`,
		`  printf '` + recoveryIncompleteMarker + `: DFLY LOAD of the backup failed (%s) and reloading the pre-restore snapshot failed too (%s)\n' "$out" "$back" >&2`,
		`  exit 3`,
		`fi`,
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
	return fmt.Sprintf("docker exec -i %s mongorestore --archive --gzip --drop --nsExclude=%s < %s", ssh.ShellQuote(container), ssh.ShellQuote(mongoCheckExclude), ssh.ShellQuote(dumpPath))
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
	containerName := ContainerName(app, name)
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
