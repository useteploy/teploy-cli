package backup

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/useteploy/teploy/internal/ssh"
)

// pgTarget is one database inside a postgres container, reached as user.
type pgTarget struct {
	container, user, db string
	// pwEnv names the container env var holding this role's password, for
	// images whose local socket is not trust-authenticated (Bitnami). The
	// password is read inside the container: it never reaches any argv.
	pwEnv string
}

// tool is `docker exec` of a postgres client tool, ready for its arguments.
// stdin keeps stdin open for a dump or SQL file redirected into it.
func (t pgTarget) tool(tool string, stdin bool) string {
	cmd := "docker exec "
	if stdin {
		cmd = "docker exec -i "
	}
	cmd += ssh.ShellQuote(t.container) + " "
	if t.pwEnv != "" {
		cmd += "sh -c " + ssh.ShellQuote("PGPASSWORD=$"+t.pwEnv+"; export PGPASSWORD; exec \"$@\"") + " sh "
	}
	return cmd + tool
}

// newPGTarget is the accessory's own role on database db.
func newPGTarget(container, db, user string, env map[string]string) pgTarget {
	t := pgTarget{container: container, db: db, user: user}
	if env["BITNAMI_APP_NAME"] != "" {
		t.pwEnv = "POSTGRESQL_PASSWORD"
	}
	return t
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
	cmd := t.tool("psql", false) + " -X -qtA -v ON_ERROR_STOP=1 -U " + ssh.ShellQuote(t.user) + " -d " + ssh.ShellQuote(db)
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
	toc, err := c.exec.Run(ctx, t.tool("pg_restore", true)+" -C -l < "+ssh.ShellQuote(artifact))
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

// pgSwapStatus is what pg_database shows about a swap's three names.
type pgSwapStatus struct {
	live, replacement, previous bool
}

// pgSwap atomically renames the live database aside and the replacement
// into its place (one transaction, so no failure can leave neither), first
// ending the sessions connected to either. A swap refused because sessions
// remain (one the role may not end, a prepared transaction's) is retried
// briefly, then returned.
func (c *Client) pgSwap(ctx context.Context, t pgTarget, newDB, oldDB string, exists bool) error {
	stmt := "ALTER DATABASE " + pgIdent(newDB) + " RENAME TO " + pgIdent(t.db)
	sqls := []string{c.pgTerminate(newDB)}
	if exists {
		stmt = "ALTER DATABASE " + pgIdent(t.db) + " RENAME TO " + pgIdent(oldDB) + "; " + stmt
		sqls = append(sqls, c.pgTerminate(t.db))
	}
	sqls = append(sqls, stmt)
	var err error
	for i := 0; i < 5; i++ {
		if _, err = c.exec.Run(ctx, t.psql(t.maintDB(), sqls...)); err == nil {
			return nil
		}
		if !strings.Contains(err.Error(), "being accessed by other users") {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(c.delay()):
		}
	}
	return err
}

func (c *Client) delay() time.Duration {
	if c.retryDelay > 0 {
		return c.retryDelay
	}
	return time.Second
}

// pgSwapState re-reads pg_database after a failed swap: its exit status can
// be lost (Ctrl-C, a dropped SSH session) after the transaction committed,
// so the error alone says nothing about what happened.
func (c *Client) pgSwapState(ctx context.Context, t pgTarget, newDB, oldDB string) (pgSwapStatus, error) {
	q := "SELECT datname FROM pg_database WHERE datname IN (" + pgLiteral(t.db) + ", " + pgLiteral(newDB) + ", " + pgLiteral(oldDB) + ")"
	var err error
	for i := 0; i < 5; i++ {
		var out string
		if out, err = c.exec.Run(ctx, t.psql(t.maintDB(), q)); err == nil {
			var st pgSwapStatus
			// Line by line: a database name may contain a space.
			for _, name := range strings.Split(out, "\n") {
				switch strings.TrimRight(name, "\r") {
				case t.db:
					st.live = true
				case newDB:
					st.replacement = true
				case oldDB:
					st.previous = true
				}
			}
			return st, nil
		}
		select {
		case <-ctx.Done():
			return pgSwapStatus{}, err
		case <-time.After(c.delay()):
		}
	}
	return pgSwapStatus{}, err
}

// pgAlive reports whether the server answers, allowing for crash recovery
// after a failure such as a full disk.
func (c *Client) pgAlive(ctx context.Context, t pgTarget) bool {
	for i := 0; i < 10; i++ {
		if _, err := c.exec.Run(ctx, t.psql(t.maintDB(), "SELECT 1")); err == nil {
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
		cmd := "gunzip -c " + q(artifact) + " > " + q(sqlPath) + " && " + t.tool("psql", true) +
			" -X -q -v ON_ERROR_STOP=1 -U " + q(t.user) + " -d " + q(db) + " < " + q(sqlPath) + " >/dev/null"
		if _, err := c.exec.Run(ctx, cmd); err != nil {
			return fmt.Errorf("loading the plain dump: %w", err)
		}
	} else {
		cmd := t.tool("pg_restore", true) + " --exit-on-error --no-password -U " + q(t.user) + " -d " + q(db) + " < " + q(artifact)
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
	cmd := "docker cp " + q(list) + " " + q(t.container+":"+inContainer) + " && " + t.tool("pg_restore", true) + " -C -L " + q(inContainer) + " -f - < " + q(artifact)
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
	out, err := c.exec.Run(ctx, t.tool("psql", true)+" -X -q -U "+q(t.user)+" -d "+q(t.db)+" < "+q(sqlFile)+" 2>&1")
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

// pgRestoreOpts tune a swap restore.
type pgRestoreOpts struct {
	// logicalBytes is the database size recorded at backup time (0 when
	// the backup predates it): what the replacement will occupy.
	logicalBytes int64
	// ignoreSpace skips the free-space preflight (--ignore-space-check).
	ignoreSpace bool
}

// pgArtifactSizeFactor estimates a database's size from a compressed dump
// when the backup recorded none: text and index data commonly grow 3-5x.
const pgArtifactSizeFactor = 4

// pgRequiredBytes is the room the replacement needs beside the live
// database. The backup decides it: a nearly empty live database can still
// need gigabytes, and a bloated one being recovered from a runaway table
// needs only what the backup holds. Live size only raises an estimate; a
// recorded size is known, so it stands alone.
func (c *Client) pgRequiredBytes(ctx context.Context, t pgTarget, artifact string, cl pgCluster, opts pgRestoreOpts) (int64, string) {
	withWAL := func(size int64) int64 {
		// Loading writes WAL too, on this volume, up to max_wal_size before
		// checkpoints recycle it. Running out of it is a PANIC.
		return size + size/10 + min(size, c.pgMaxWALBytes(ctx, t))
	}
	if opts.logicalBytes > 0 {
		return withWAL(opts.logicalBytes), "the size recorded at backup time, plus WAL"
	}
	var est int64
	if out, err := c.exec.Run(ctx, "wc -c < "+ssh.ShellQuote(artifact)); err == nil {
		if n, perr := strconv.ParseInt(strings.TrimSpace(out), 10, 64); perr == nil {
			est = n * pgArtifactSizeFactor
		}
	}
	if cl.exists && cl.sizeBytes > est {
		est = cl.sizeBytes
	}
	return withWAL(est), fmt.Sprintf("an estimate (%dx the artifact, at least the live database), plus WAL", pgArtifactSizeFactor)
}

// pgMaxWALBytes reads max_wal_size from the server (its default 1 GB when it
// cannot be read).
func (c *Client) pgMaxWALBytes(ctx context.Context, t pgTarget) int64 {
	const defaultWAL = 1 << 30
	out, err := c.exec.Run(ctx, t.psql(t.maintDB(), "SELECT setting FROM pg_settings WHERE name = 'max_wal_size'"))
	if err != nil {
		return defaultWAL
	}
	mb, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil || mb <= 0 {
		return defaultWAL
	}
	return mb << 20
}

// pgLocale is a database's collation provider, for CREATE DATABASE.
type pgLocale struct {
	Provider string `json:"provider"`
	Locale   string `json:"locale"`
	Rules    string `json:"rules"`
}

// pgDatabaseLocale reads the provider columns, whose names depend on the
// server version (ICU arrived in 15, rules in 16, datlocale in 17).
func (c *Client) pgDatabaseLocale(ctx context.Context, t pgTarget, version int) (pgLocale, error) {
	var cols string
	switch {
	case version >= 170000:
		cols = "datlocprovider AS provider, datlocale AS locale, daticurules AS rules"
	case version >= 160000:
		cols = "datlocprovider AS provider, daticulocale AS locale, daticurules AS rules"
	case version >= 150000:
		cols = "datlocprovider AS provider, daticulocale AS locale"
	default:
		return pgLocale{}, nil
	}
	out, err := c.exec.Run(ctx, t.psql(t.maintDB(), "SELECT row_to_json(x) FROM (SELECT "+cols+" FROM pg_database WHERE datname = "+pgLiteral(t.db)+") x"))
	if err != nil {
		return pgLocale{}, err
	}
	var loc pgLocale
	if s := strings.TrimSpace(out); s != "" {
		if err := json.Unmarshal([]byte(s), &loc); err != nil {
			return pgLocale{}, fmt.Errorf("unexpected reply %q", s)
		}
	}
	return loc, nil
}

// createClause is the CREATE DATABASE locale-provider clause. Without it an
// ICU database comes back as libc, silently changing its sort order.
func (l pgLocale) createClause(version int) string {
	switch l.Provider {
	case "i":
		clause := " LOCALE_PROVIDER icu ICU_LOCALE " + pgLiteral(l.Locale)
		if l.Rules != "" && version >= 160000 {
			clause += " ICU_RULES " + pgLiteral(l.Rules)
		}
		return clause
	case "b":
		return " LOCALE_PROVIDER builtin BUILTIN_LOCALE " + pgLiteral(l.Locale)
	case "c":
		return " LOCALE_PROVIDER libc"
	}
	return ""
}

// truncBytes shortens s to at most n bytes on a rune boundary: postgres
// identifiers are limited to 63 BYTES.
func truncBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// restorePostgres replaces t.db with the artifact without ever leaving it
// empty: the backup is loaded into a NEW database, and only once that load
// has fully succeeded are the two swapped in one transaction. The previous
// database is kept, renamed, as the recovery copy (returned; the caller
// drops it with ReleaseRecovery after its health check). Every failure
// before the swap leaves the live database untouched, and says so only if
// it is still true: a load that took the server down is recovery-incomplete.
//
// The old sequence (DROP, CREATE, pg_restore --single-transaction, reload a
// safety dump on failure) emptied the database whenever the load failed
// for a reason the safety reload shared: lock-table exhaustion on a large
// schema, or a non-superuser owner restoring an extension only a superuser
// may create.
func (c *Client) restorePostgres(ctx context.Context, t pgTarget, artifact, ext, workDir string, opts pgRestoreOpts) (RestoreRecovery, error) {
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
	if !opts.ignoreSpace {
		// The replacement is built beside the live database on the same
		// data volume, so that volume must hold it.
		need, basis := c.pgRequiredBytes(ctx, t, artifact, cl, opts)
		pgdata, ok := c.containerEnv(ctx, t.container, "PGDATA")
		if !ok || pgdata == "" {
			pgdata = "/var/lib/postgresql/data"
			if app, _ := c.containerEnv(ctx, t.container, "BITNAMI_APP_NAME"); app != "" {
				pgdata = "/bitnami/postgresql"
			}
		}
		free, known := c.freeBytes(ctx, "docker exec "+ssh.ShellQuote(t.container)+" ", pgdata)
		if err := requireSpace("the replacement database on the data volume (needed: "+basis+"; --ignore-space-check overrides)", free, known, need); err != nil {
			return none, err
		}
	}

	var warnings []string
	// A plain dump carries no database-level entries, so they come from
	// the database being replaced; the old one is dropped after the swap
	// and they would be lost with it.
	dbLevelArchive, dbLevel := artifact, a
	if ext == ".sql.gz" && cl.exists {
		dbLevelArchive = workDir + "/live-dblevel.pgdump"
		cmd := t.tool("pg_dump", false) + " -Fc --schema-only -U " + ssh.ShellQuote(t.user) + " " + ssh.ShellQuote(t.db) + " > " + ssh.ShellQuote(dbLevelArchive)
		if _, err := c.exec.Run(ctx, cmd); err != nil {
			warnings = append(warnings, fmt.Sprintf("could not read the database-level settings of %s before replacing it: %v", t.db, err))
			dbLevel = pgArchive{}
		} else if dbLevel, err = c.pgInspectArtifact(ctx, t, dbLevelArchive, ".pgdump"); err != nil {
			warnings = append(warnings, fmt.Sprintf("could not read the database-level settings of %s before replacing it: %v", t.db, err))
			dbLevel = pgArchive{}
		}
	}

	sfx := randSuffix()
	prefix := truncBytes(t.db, 40)
	newDB := prefix + "_teploy_new_" + sfx
	oldDB := prefix + "_teploy_old_" + sfx
	owner := t.user
	if cl.exists {
		owner = cl.owner
	}
	create := "CREATE DATABASE " + pgIdent(newDB) + " OWNER " + pgIdent(owner) + " TEMPLATE template0"
	if cl.exists {
		create += " ENCODING " + pgLiteral(cl.encoding) + " LC_COLLATE " + pgLiteral(cl.collate) + " LC_CTYPE " + pgLiteral(cl.ctype)
		loc, err := c.pgDatabaseLocale(ctx, t, cl.version)
		if err != nil {
			return none, fmt.Errorf("reading the collation provider of %s (database untouched): %w", t.db, err)
		}
		create += loc.createClause(cl.version)
	}
	wctx := context.WithoutCancel(ctx)
	if _, err := c.exec.Run(ctx, t.psql(t.maintDB(), create)); err != nil {
		return none, fmt.Errorf("creating the replacement database (database untouched): %w", err)
	}
	fmt.Fprintf(c.out, "Loading the backup into %s...\n", newDB)
	if err := c.pgLoadInto(ctx, t, newDB, artifact, a, workDir); err != nil {
		return none, c.pgLoadFailed(wctx, t, newDB, cl.version, err)
	}

	if err := c.pgSwap(ctx, t, newDB, oldDB, cl.exists); err != nil {
		st, stErr := c.pgSwapState(wctx, t, newDB, oldDB)
		kept := RestoreRecovery{PGDatabase: oldDB, pg: t, pgVersion: cl.version}
		switch {
		case stErr != nil:
			return kept, fmt.Errorf("%w: the swap failed (%v) and the server no longer answers (%v), so whether %s or %s holds the data is unknown; neither was dropped", errRecoveryIncomplete, err, stErr, t.db, newDB)
		case st.live && st.replacement && !st.previous:
			// Rolled back: nothing changed.
			if dropErr := c.pgDropDB(wctx, t, newDB, cl.version); dropErr != nil {
				return none, fmt.Errorf("taking %s offline for the swap failed (database untouched, but the replacement %s could not be dropped: %v): %w", t.db, newDB, dropErr, err)
			}
			return none, fmt.Errorf("taking %s offline for the swap failed (database untouched; the replacement was dropped): %w", t.db, err)
		case st.replacement && !st.live && !cl.exists:
			if dropErr := c.pgDropDB(wctx, t, newDB, cl.version); dropErr != nil {
				return none, fmt.Errorf("renaming the restored database into place failed and the replacement %s could not be dropped (%v): %w", newDB, dropErr, err)
			}
			return none, fmt.Errorf("renaming the restored database into place failed: %w", err)
		case st.live && !st.replacement && (st.previous || !cl.exists):
			// Committed: only the reply was lost. The restore succeeded.
			warnings = append(warnings, fmt.Sprintf("the swap reported an error (%v) but pg_database shows it completed", err))
			ctx = wctx
		default:
			return kept, fmt.Errorf("%w: the swap failed (%v) and pg_database is in an unexpected state (%s present: %v, %s present: %v, %s present: %v); nothing was dropped", errRecoveryIncomplete, err, t.db, st.live, newDB, st.replacement, oldDB, st.previous)
		}
	}
	warnings = append(warnings, c.pgApplyDBLevel(ctx, t, dbLevelArchive, dbLevel, workDir)...)
	for _, w := range warnings {
		fmt.Fprintf(c.out, "Warning: %s\n", w)
	}
	if !cl.exists {
		return RestoreRecovery{Warnings: warnings}, nil
	}
	fmt.Fprintf(c.out, "Previous database kept as %s until the restore is confirmed\n", oldDB)
	return RestoreRecovery{PGDatabase: oldDB, pg: t, pgVersion: cl.version, Warnings: warnings}, nil
}

// pgLoadFailed classifies a failed load truthfully. A load can take the
// server down with it (a full disk panics postgres, and it may not come
// back): then the live database is not "untouched" in any useful sense, and
// the caller must not restart the app against a dead server. A replacement
// that could not be dropped is named, not reported as gone.
func (c *Client) pgLoadFailed(ctx context.Context, t pgTarget, newDB string, version int, loadErr error) error {
	dead := func() error {
		return fmt.Errorf("%w: loading the backup failed (%v) and the postgres server in %s no longer answers; the data volume is probably full. The partial replacement %s remains; free space, start the accessory and drop it", errRecoveryIncomplete, loadErr, t.container, newDB)
	}
	if !c.pgAlive(ctx, t) {
		return dead()
	}
	if err := c.pgDropDB(ctx, t, newDB, version); err != nil {
		// DROP DATABASE checkpoints, and on a full disk that is what takes
		// the server down: look again before saying anything is untouched.
		if !c.pgAlive(ctx, t) {
			return dead()
		}
		return fmt.Errorf("%w (database untouched, but the partial replacement %s could not be dropped: %v)", loadErr, newDB, err)
	}
	return fmt.Errorf("%w (database untouched; the partial replacement was dropped)", loadErr)
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
	// A DR target has no live data to protect, and a guessed size must not
	// refuse a recovery: a load that does not fit fails honestly.
	rec, err := c.restorePostgres(ctx, t, artifact, ".pgdump", workDir, pgRestoreOpts{ignoreSpace: true})
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

// BackupDatabasesLabel is the container label `backup_databases:` in
// teploy.yml sets: the extra databases a postgres backup also dumps, comma
// separated, or "all".
const BackupDatabasesLabel = "teploy.backup-databases"

var (
	pgLeftoverName = regexp.MustCompile(`^(.*)_teploy_(old|new)_[0-9a-f]{8}$`)
)

// pgCoverage says what a backup of t.db leaves out of the cluster.
type pgCoverage struct {
	Extra      []string // listed in backup_databases: dumped beside the primary
	Uncovered  []string // in the cluster, not backed up
	KeptOld    []string // <db>_teploy_old_*: a previous database kept by a restore
	PartialNew []string // <db>_teploy_new_*: a replacement left by a killed restore
	// Missing are databases listed by name that the server does not have.
	Missing []string
	// PrimaryMissing says the accessory's own database does not exist (with
	// AllDatabases, every database then has a series of its own).
	PrimaryMissing bool
	// PrimaryIsSingle says a SingleDatabase backup is of the primary database.
	PrimaryIsSingle bool
	// Throwaway are mysql dry-run databases a killed restore left.
	Throwaway []string
	// DroppedThrowaway are mongo dry-run databases this backup cleared.
	DroppedThrowaway []string
}

// pgReadCoverage lists the cluster's databases. A "postgres" maintenance
// database counts only when it holds tables of its own (an extension's
// catalog tables, such as TimescaleDB's, do not count). Names teploy's own
// restore left behind are reported separately, never as uncovered.
func (c *Client) pgReadCoverage(ctx context.Context, t pgTarget, listed []string) (pgCoverage, error) {
	var cov pgCoverage
	names, err := c.pgDatabaseNames(ctx, t)
	if err != nil {
		return cov, err
	}
	all := false
	want := map[string]bool{}
	for _, l := range listed {
		if l == "all" {
			all = true
		}
		want[l] = true
	}
	primaryFound := false
	for _, name := range names {
		switch {
		case name == t.db:
			primaryFound = true
		case pgLeftoverName.MatchString(name):
			if strings.Contains(name, "_teploy_old_") {
				cov.KeptOld = append(cov.KeptOld, name)
			} else {
				cov.PartialNew = append(cov.PartialNew, name)
			}
		case want[name] && addressableDB(name):
			cov.Extra = append(cov.Extra, name)
		case name == "postgres":
			tables := "SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE c.relkind IN ('r','p') AND n.nspname NOT IN ('pg_catalog','information_schema') AND n.nspname NOT LIKE 'pg\\_%' AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_class'::regclass AND d.objid = c.oid AND d.deptype = 'e')"
			if n, err := c.exec.Run(ctx, t.psql("postgres", tables)); err == nil && strings.TrimSpace(n) != "0" && strings.TrimSpace(n) != "" {
				if all {
					cov.Extra = append(cov.Extra, name)
				} else {
					cov.Uncovered = append(cov.Uncovered, name)
				}
			}
		case all && addressableDB(name):
			cov.Extra = append(cov.Extra, name)
		default:
			cov.Uncovered = append(cov.Uncovered, name)
		}
	}
	cov.PrimaryMissing = t.db != "" && !primaryFound
	for _, l := range listed {
		if l != "all" && !slices.Contains(names, l) && l != t.db {
			cov.Missing = append(cov.Missing, l)
		}
	}
	return cov, nil
}

// pgDatabaseNames lists the cluster's connectable non-template databases.
func (c *Client) pgDatabaseNames(ctx context.Context, t pgTarget) ([]string, error) {
	out, err := c.exec.Run(ctx, t.psql(t.maintDB(), "SELECT datname FROM pg_database WHERE NOT datistemplate AND datallowconn ORDER BY datname"))
	if err != nil {
		return nil, err
	}
	return splitLines(out), nil
}

// warnings renders the coverage problems for messages and events.
func (cov pgCoverage) warnings(db string) []string {
	var w []string
	if len(cov.Uncovered) > 0 {
		w = append(w, fmt.Sprintf("this backup covers only %s: the cluster also holds %s, not backed up (list them in the accessory's backup_databases: to include them)", strings.Join(append([]string{db}, cov.Extra...), ", "), strings.Join(cov.Uncovered, ", ")))
	}
	if len(cov.KeptOld) > 0 {
		w = append(w, fmt.Sprintf("a restore kept the previous database %s: confirm with `teploy backup restore --accessory <name> --release` or undo with --rollback", strings.Join(cov.KeptOld, ", ")))
	}
	if len(cov.PartialNew) > 0 {
		w = append(w, fmt.Sprintf("a killed restore left the partial database %s: `teploy backup restore --accessory <name> --release` removes it", strings.Join(cov.PartialNew, ", ")))
	}
	return w
}

// pgBackupMeta is the small sidecar stored beside a postgres dump.
type pgBackupMeta struct {
	LogicalBytes int64 `json:"logical_bytes"`
}

// pgRecordedSize reads the size recorded at backup time (0 when absent), for
// the primary database or one with its own series.
func (c *Client) pgRecordedSize(ctx context.Context, s3 S3Config, app, name, db, id, workDir string) int64 {
	local := workDir + "/meta.json"
	if !c.fetchSidecar(ctx, s3, seriesPrefix(s3, app, name, db)+"meta/"+id+".json", local) {
		return 0
	}
	out, err := c.exec.Run(ctx, "cat "+ssh.ShellQuote(local))
	if err != nil {
		return 0
	}
	var m pgBackupMeta
	if json.Unmarshal([]byte(out), &m) != nil {
		return 0
	}
	return m.LogicalBytes
}

// AccessoryReport is what doctor shows about one running database accessory.
type AccessoryReport struct {
	App, Accessory string
	Uncovered      []string // databases its backups leave out
	Leftovers      []string // databases a restore left behind
	Err            error
}

// AccessoryProbe lists the running accessories (read-only).
const AccessoryProbe = `docker ps --filter label=teploy.role=accessory --format '{{.Label "teploy.app"}}|{{.Label "teploy.accessory"}}'`

// AccessoryReports inspects every running database accessory on the server for
// uncovered databases and restore leftovers (read-only).
func (c *Client) AccessoryReports(ctx context.Context) ([]AccessoryReport, error) {
	out, err := c.exec.Run(ctx, AccessoryProbe)
	if err != nil {
		return nil, err
	}
	var reports []AccessoryReport
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		app, name, ok := strings.Cut(strings.TrimSpace(line), "|")
		if !ok || app == "" || name == "" {
			continue
		}
		image, env, err := c.InspectAccessory(ctx, app, name)
		if err != nil {
			continue
		}
		det, err := c.ResolveEngine(ctx, app, name, image, env)
		if err != nil {
			continue
		}
		container := ContainerName(app, name)
		r := AccessoryReport{App: app, Accessory: name}
		switch det.Engine {
		case EnginePostgres:
			db, user := postgresDBAndUser(app, env)
			labels, _ := c.inspectEngineHints(ctx, container)
			cov, err := c.pgReadCoverage(ctx, newPGTarget(container, db, user, env), listedDatabases(labels))
			r.Err, r.Uncovered = err, cov.Uncovered
			r.Leftovers = append(append(r.Leftovers, cov.KeptOld...), cov.PartialNew...)
		case EngineMySQL, EngineMariaDB:
			r.Uncovered, r.Leftovers, r.Err = c.mysqlReadCoverage(ctx, det.Engine, container, mysqlDB(app, env))
			labels, _ := c.inspectEngineHints(ctx, container)
			if listed := listedDatabases(labels); len(listed) > 0 && r.Err == nil {
				cov, err := c.mysqlCoverage(ctx, det.Engine, container, mysqlDB(app, env), listed)
				r.Err, r.Uncovered = err, cov.Uncovered
			}
		case EngineMongo:
			r.Leftovers, r.Err = c.mongoCheckDatabases(ctx, container)
		default:
			continue
		}
		reports = append(reports, r)
	}
	return reports, nil
}

// listedDatabases parses the backup_databases label.
func listedDatabases(labels map[string]string) []string {
	var out []string
	for _, d := range strings.Split(labels[BackupDatabasesLabel], ",") {
		if d = strings.TrimSpace(d); d != "" {
			out = append(out, d)
		}
	}
	return out
}
