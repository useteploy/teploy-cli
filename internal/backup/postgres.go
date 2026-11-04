package backup

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/useteploy/teploy/internal/ssh"
)

// pgTarget is one database inside a postgres container, reached as user.
type pgTarget struct {
	container, user, db string
}

// maintDB is the database administrative statements run from: never the
// target (a database cannot be renamed or dropped from inside itself).
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

// pgArchive is what a backup artifact says about itself.
type pgArchive struct {
	ext       string
	timescale bool
	dbname    string   // the database the archive was dumped from
	dbLevel   []string // TOC lines of the database's COMMENT / ACL / properties
}

var (
	timescaleTOC = regexp.MustCompile(`(?m) EXTENSION - timescaledb( |$)`)
	dbLevelTOC   = regexp.MustCompile(`^\d+; \d+ \d+ (COMMENT - DATABASE |ACL - DATABASE |DATABASE PROPERTIES - )`)
)

// pgInspectArtifact proves the artifact is readable before anything
// changes and reads its table of contents (pg_restore -C -l also lists the
// database-level entries: its comment, ACL and ALTER DATABASE/ROLE SET).
func (c *Client) pgInspectArtifact(ctx context.Context, t pgTarget, artifact, ext string) (pgArchive, error) {
	a := pgArchive{ext: ext}
	if ext == ".sql.gz" {
		if _, err := c.exec.Run(ctx, "gzip -t "+ssh.ShellQuote(artifact)); err != nil {
			return a, fmt.Errorf("legacy plain dump is corrupt: %w", err)
		}
		return a, nil
	}
	toc, err := c.exec.Run(ctx, "docker exec -i "+ssh.ShellQuote(t.container)+" pg_restore -C -l < "+ssh.ShellQuote(artifact))
	if err != nil {
		return a, fmt.Errorf("pg_restore cannot read the archive: %w", err)
	}
	a.timescale = timescaleTOC.MatchString(toc)
	for _, line := range strings.Split(toc, "\n") {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, "; dbname: "); ok {
			a.dbname = strings.TrimSpace(v)
		}
		if dbLevelTOC.MatchString(line) {
			a.dbLevel = append(a.dbLevel, line)
		}
	}
	return a, nil
}

// pgCluster is the state a restore decides on.
type pgCluster struct {
	version         int
	super, createdb bool
	exists          bool
	owner           string
	encoding        string
	collate, ctype  string
	sizeBytes       int64
}

func (c *Client) pgClusterState(ctx context.Context, t pgTarget) (pgCluster, error) {
	q := "SELECT current_setting('server_version_num') || '|' || r.rolsuper || '|' || r.rolcreatedb || coalesce((SELECT '|' || pg_get_userbyid(d.datdba) || '|' || pg_encoding_to_char(d.encoding) || '|' || d.datcollate || '|' || d.datctype || '|' || CASE WHEN has_database_privilege(d.oid, 'CONNECT') THEN pg_database_size(d.oid) ELSE -1 END FROM pg_database d WHERE d.datname = " + pgLiteral(t.db) + "), '') FROM pg_roles r WHERE r.rolname = current_user"
	out, err := c.exec.Run(ctx, t.psql(t.maintDB(), q))
	if err != nil {
		return pgCluster{}, fmt.Errorf("reading database %s: %w", t.db, err)
	}
	f := strings.Split(strings.TrimSpace(out), "|")
	if len(f) < 3 {
		return pgCluster{}, fmt.Errorf("reading database %s: unexpected reply %q", t.db, strings.TrimSpace(out))
	}
	v, err := strconv.Atoi(f[0])
	if err != nil {
		return pgCluster{}, fmt.Errorf("reading database %s: unexpected reply %q", t.db, strings.TrimSpace(out))
	}
	cl := pgCluster{version: v, super: f[1] == "t" || f[1] == "true", createdb: f[2] == "t" || f[2] == "true"}
	if len(f) == 8 {
		cl.exists, cl.owner, cl.encoding, cl.collate, cl.ctype = true, f[3], f[4], f[5], f[6]
		cl.sizeBytes, _ = strconv.ParseInt(f[7], 10, 64)
	}
	return cl, nil
}

// pgPreflight refuses before anything changes when the restoring role
// cannot complete the swap: it must be a superuser, or own the database
// and hold CREATEDB (to create the replacement and rename both).
func pgPreflight(t pgTarget, cl pgCluster) error {
	if cl.super {
		return nil
	}
	if !cl.createdb {
		return fmt.Errorf("role %s is not a superuser and lacks CREATEDB, so it cannot build the replacement database (nothing was changed) — restore as a superuser or grant CREATEDB", t.user)
	}
	if cl.exists && cl.owner != t.user {
		return fmt.Errorf("role %s does not own database %s (owner %s) and is not a superuser, so it cannot swap it (nothing was changed)", t.user, t.db, cl.owner)
	}
	return nil
}

func randSuffix() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func (c *Client) pgTerminate(db string) string {
	return "SELECT count(pg_terminate_backend(pid)) FROM pg_stat_activity WHERE datname = " + pgLiteral(db) + " AND pid <> pg_backend_pid()"
}

// pgDropDB drops a database the restore created or kept aside.
func (c *Client) pgDropDB(ctx context.Context, t pgTarget, db string, version int) error {
	stmt := "DROP DATABASE IF EXISTS " + pgIdent(db)
	if version >= 130000 {
		stmt += " WITH (FORCE)"
	}
	_, err := c.exec.Run(ctx, t.psql(t.maintDB(), c.pgTerminate(db), stmt))
	return err
}

// pgRename renames a database, first ending the sessions connected to it.
// A rename refused because sessions remain (one the role may not end —
// a superuser's, a prepared transaction's) is retried briefly, then
// returned: the database is still intact under its old name.
func (c *Client) pgRename(ctx context.Context, t pgTarget, from, to string) error {
	var err error
	for i := 0; i < 5; i++ {
		if _, err = c.exec.Run(ctx, t.psql(t.maintDB(), c.pgTerminate(from), "ALTER DATABASE "+pgIdent(from)+" RENAME TO "+pgIdent(to))); err == nil {
			return nil
		}
		if !strings.Contains(err.Error(), "being accessed by other users") {
			return err
		}
		delay := c.retryDelay
		if delay == 0 {
			delay = time.Second
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(delay):
		}
	}
	return err
}

// pgLoadInto loads the artifact into db (an empty, freshly created
// database). No --single-transaction: one transaction over a large schema
// exhausts the lock table (max_locks_per_transaction), and the target is a
// throwaway database until the swap, so atomicity comes from the swap.
func (c *Client) pgLoadInto(ctx context.Context, t pgTarget, db, artifact string, a pgArchive, workDir string) error {
	q := ssh.ShellQuote
	if a.timescale {
		if _, err := c.exec.Run(ctx, t.psql(db, "CREATE EXTENSION IF NOT EXISTS timescaledb", "SELECT timescaledb_pre_restore()")); err != nil {
			return fmt.Errorf("preparing timescaledb restore: %w", err)
		}
	}
	if a.ext == ".sql.gz" {
		sqlPath := workDir + "/restore.sql"
		cmd := "gunzip -c " + q(artifact) + " > " + q(sqlPath) + " && docker exec -i " + q(t.container) +
			" psql -X -q -v ON_ERROR_STOP=1 -U " + q(t.user) + " -d " + q(db) + " < " + q(sqlPath) + " >/dev/null"
		if _, err := c.exec.Run(ctx, cmd); err != nil {
			return fmt.Errorf("loading the plain dump: %w", err)
		}
	} else {
		cmd := "docker exec -i " + q(t.container) + " pg_restore --exit-on-error --no-password -U " + q(t.user) + " -d " + q(db) + " < " + q(artifact)
		if _, err := c.exec.Run(ctx, cmd); err != nil {
			return fmt.Errorf("pg_restore: %w", err)
		}
	}
	if a.timescale {
		if _, err := c.exec.Run(ctx, t.psql(db, "SELECT timescaledb_post_restore()")); err != nil {
			return fmt.Errorf("finishing timescaledb restore: %w", err)
		}
	}
	return nil
}

// pgApplyDBLevel re-applies the archive's database-level entries (COMMENT
// ON DATABASE, GRANT/REVOKE ON DATABASE, ALTER DATABASE ... SET, ALTER ROLE
// ... IN DATABASE ... SET) to the restored database. A plain pg_restore
// never applies them, so search_path settings and REVOKE CONNECT FROM
// PUBLIC were silently lost. Statements the role may not run are reported,
// not fatal: the data is already in place.
func (c *Client) pgApplyDBLevel(ctx context.Context, t pgTarget, artifact string, a pgArchive, workDir string) []string {
	if len(a.dbLevel) == 0 {
		return nil
	}
	if a.dbname != "" && a.dbname != t.db {
		return []string{fmt.Sprintf("the backup was taken from database %s, not %s; its database-level settings were not applied", a.dbname, t.db)}
	}
	q := ssh.ShellQuote
	list := workDir + "/dblevel.list"
	inContainer := "/tmp/teploy-dblevel-" + randSuffix() + ".list"
	if err := c.exec.Upload(ctx, strings.NewReader(strings.Join(a.dbLevel, "\n")+"\n"), list, "0600"); err != nil {
		return []string{"database-level settings not applied: " + err.Error()}
	}
	cmd := "docker cp " + q(list) + " " + q(t.container+":"+inContainer) + " && docker exec -i " + q(t.container) + " pg_restore -C -L " + q(inContainer) + " -f - < " + q(artifact)
	sql, err := c.exec.Run(ctx, cmd)
	c.exec.Run(context.WithoutCancel(ctx), "docker exec -u 0 "+q(t.container)+" rm -f "+q(inContainer))
	if err != nil {
		return []string{"database-level settings not applied: " + err.Error()}
	}
	var keep []string
	for _, line := range strings.Split(sql, "\n") {
		trim := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trim, `\`):
			continue // psql meta-commands (\connect, \restrict)
		case strings.HasPrefix(trim, "CREATE DATABASE "):
			continue
		case strings.HasPrefix(trim, "ALTER DATABASE ") && strings.Contains(trim, " OWNER TO "):
			continue
		}
		keep = append(keep, line)
	}
	sqlFile := workDir + "/dblevel.sql"
	if err := c.exec.Upload(ctx, strings.NewReader(strings.Join(keep, "\n")+"\n"), sqlFile, "0600"); err != nil {
		return []string{"database-level settings not applied: " + err.Error()}
	}
	out, err := c.exec.Run(ctx, "docker exec -i "+q(t.container)+" psql -X -q -U "+q(t.user)+" -d "+q(t.db)+" < "+q(sqlFile)+" 2>&1")
	var warns []string
	if err != nil {
		warns = append(warns, "database-level settings not applied: "+err.Error())
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "ERROR:") {
			warns = append(warns, "database-level setting not applied: "+strings.TrimSpace(line))
		}
	}
	return warns
}

// restorePostgres replaces t.db with the artifact without ever leaving it
// empty: the backup is loaded into a NEW database, and only once that load
// has fully succeeded is the live database renamed aside and the new one
// renamed into its place. The previous database is kept, renamed, as the
// recovery copy (returned; the caller drops it with ReleaseRecovery after
// its health check). Every failure before the swap leaves the live
// database untouched; only a failed swap-back is recovery-incomplete.
//
// The old sequence (DROP, CREATE, pg_restore --single-transaction, reload a
// safety dump on failure) emptied the database whenever the load failed
// for a reason the safety reload shared: lock-table exhaustion on a large
// schema, or a non-superuser owner restoring an extension only a superuser
// may create.
func (c *Client) restorePostgres(ctx context.Context, t pgTarget, artifact, ext, workDir string) (RestoreRecovery, error) {
	var none RestoreRecovery
	a, err := c.pgInspectArtifact(ctx, t, artifact, ext)
	if err != nil {
		return none, fmt.Errorf("backup artifact failed validation (database untouched): %w", err)
	}
	cl, err := c.pgClusterState(ctx, t)
	if err != nil {
		return none, fmt.Errorf("%w (database untouched)", err)
	}
	if err := pgPreflight(t, cl); err != nil {
		return none, err
	}
	if cl.exists && cl.sizeBytes > 0 {
		// The replacement is built beside the live database, so the data
		// volume must hold both for a while.
		pgdata, ok := c.containerEnv(ctx, t.container, "PGDATA")
		if !ok || pgdata == "" {
			pgdata = "/var/lib/postgresql/data"
		}
		free, known := c.freeBytes(ctx, "docker exec "+ssh.ShellQuote(t.container)+" ", pgdata)
		if err := requireSpace("building the replacement database beside the live one", free, known, cl.sizeBytes+cl.sizeBytes/10); err != nil {
			return none, err
		}
	}

	sfx := randSuffix()
	newDB := fmt.Sprintf("%.40s_teploy_new_%s", t.db, sfx)
	oldDB := fmt.Sprintf("%.40s_teploy_old_%s", t.db, sfx)
	owner := t.user
	if cl.exists {
		owner = cl.owner
	}
	create := "CREATE DATABASE " + pgIdent(newDB) + " OWNER " + pgIdent(owner) + " TEMPLATE template0"
	if cl.exists {
		create += " ENCODING " + pgLiteral(cl.encoding) + " LC_COLLATE " + pgLiteral(cl.collate) + " LC_CTYPE " + pgLiteral(cl.ctype)
	}
	wctx := context.WithoutCancel(ctx)
	dropNew := func() {
		if err := c.pgDropDB(wctx, t, newDB, cl.version); err != nil {
			fmt.Fprintf(c.out, "Warning: could not drop the partial replacement database %s: %v\n", newDB, err)
		}
	}
	if _, err := c.exec.Run(ctx, t.psql(t.maintDB(), create)); err != nil {
		return none, fmt.Errorf("creating the replacement database (database untouched): %w", err)
	}
	fmt.Fprintf(c.out, "Loading the backup into %s...\n", newDB)
	if err := c.pgLoadInto(ctx, t, newDB, artifact, a, workDir); err != nil {
		dropNew()
		return none, fmt.Errorf("%w (database untouched; the partial replacement was dropped)", err)
	}

	if cl.exists {
		if err := c.pgRename(ctx, t, t.db, oldDB); err != nil {
			dropNew()
			return none, fmt.Errorf("taking %s offline for the swap failed (database untouched; the replacement was dropped): %w", t.db, err)
		}
	}
	if err := c.pgRename(ctx, t, newDB, t.db); err != nil {
		if !cl.exists {
			dropNew()
			return none, fmt.Errorf("renaming the restored database into place: %w", err)
		}
		if backErr := c.pgRename(wctx, t, oldDB, t.db); backErr != nil {
			return RestoreRecovery{PGDatabase: oldDB, pg: t, pgVersion: cl.version}, fmt.Errorf("%w: renaming the restored database %s into place failed (%v) and renaming the previous database %s back failed too (%v) — %s does not exist; both copies are kept", errRecoveryIncomplete, newDB, err, oldDB, backErr, t.db)
		}
		dropNew()
		return none, fmt.Errorf("renaming the restored database into place failed (the previous database was renamed back, untouched): %w", err)
	}
	for _, w := range c.pgApplyDBLevel(ctx, t, artifact, a, workDir) {
		fmt.Fprintf(c.out, "Warning: %s\n", w)
	}
	if !cl.exists {
		return none, nil
	}
	fmt.Fprintf(c.out, "Previous database kept as %s until the restore is confirmed\n", oldDB)
	return RestoreRecovery{PGDatabase: oldDB, pg: t, pgVersion: cl.version}, nil
}

// containerEnv reads one environment variable of a container.
func (c *Client) containerEnv(ctx context.Context, container, key string) (string, bool) {
	out, err := c.exec.Run(ctx, "docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "+ssh.ShellQuote(container))
	if err != nil {
		return "", false
	}
	for _, line := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), key+"="); ok {
			return v, true
		}
	}
	return "", false
}

// postgresRestoreCmd is the legacy plain-dump loader for DR bundles taken
// before G7 (.sql.gz members), restored into a freshly created accessory.
func postgresRestoreCmd(container, user, db, dumpPath, sqlPath string) string {
	return fmt.Sprintf("gunzip -c %s > %s && docker exec -i %s psql -v ON_ERROR_STOP=1 -U %s %s < %s",
		ssh.ShellQuote(dumpPath), ssh.ShellQuote(sqlPath), ssh.ShellQuote(container), ssh.ShellQuote(user), ssh.ShellQuote(db), ssh.ShellQuote(sqlPath))
}

// postgresReadyProbe waits for the FINAL server: the official entrypoint
// runs a temporary socket-only server while it initialises, and a socket
// probe that answers during init let a restore race the restart.
func postgresReadyProbe(user string) string {
	return "pg_isready -h 127.0.0.1 -U " + ssh.ShellQuote(user)
}

// pgLoadArtifact loads a DR-bundle postgres member into a freshly started
// engine: custom-format members through the swap restore (the fresh empty
// database it replaces is dropped at once), legacy plain members through
// psql.
func (c *Client) pgLoadArtifact(ctx context.Context, t pgTarget, artifact, workDir string) error {
	if strings.HasSuffix(artifact, ".sql.gz") {
		_, err := c.exec.Run(ctx, postgresRestoreCmd(t.container, t.user, t.db, artifact, strings.TrimSuffix(artifact, ".gz")))
		return err
	}
	rec, err := c.restorePostgres(ctx, t, artifact, ".pgdump", workDir)
	if err != nil {
		return err
	}
	return c.ReleaseRecovery(ctx, rec)
}

// postgresDBAndUser resolves the database name and role a postgres
// accessory runs with, mirroring internal/accessories.connectionEnvVars
// (POSTGRES_DB / POSTGRES_USER, defaulting to the app name / "postgres");
// Bitnami images name them POSTGRESQL_DATABASE / POSTGRESQL_USERNAME.
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

// postgresBootstrapUser is the superuser a fresh official-image container
// creates (POSTGRES_USER, default postgres) — the role verify uses to set a
// scratch cluster up before restoring as the accessory's own role.
func postgresBootstrapUser(env map[string]string) string {
	if u := env["POSTGRES_USER"]; u != "" {
		return u
	}
	return "postgres"
}
