package backup

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy/internal/ssh"
)

const pgTOC = `;
; Archive created at 2026-11-03 04:00:00 UTC
;     dbname: myapp
;
3470; 1262 16385 DATABASE - myapp appowner
3471; 0 0 COMMENT - DATABASE myapp appowner
3472; 0 0 ACL - DATABASE myapp appowner
3473; 0 0 DATABASE PROPERTIES - myapp appowner
220; 1259 16424 TABLE public t appowner
`

const pgDBLevelSQL = `SET statement_timeout = 0;
\restrict abc
CREATE DATABASE myapp WITH TEMPLATE = template0 ENCODING = 'UTF8';
ALTER DATABASE myapp OWNER TO appowner;
\connect myapp
COMMENT ON DATABASE myapp IS 'the app';
ALTER DATABASE myapp SET search_path TO 'app', 'public';
REVOKE CONNECT,TEMPORARY ON DATABASE myapp FROM PUBLIC;
\unrestrict abc
`

// pgRestoreMock answers the swap restore of database myapp in container
// myapp-postgres as the superuser postgres; overrides match first.
func pgRestoreMock(overrides ...ssh.MockCommand) *ssh.MockExecutor {
	cmds := append([]ssh.MockCommand{}, overrides...)
	cmds = append(cmds,
		ssh.MockCommand{Match: "which aws", Output: "/usr/bin/aws\n"},
		ssh.MockCommand{Match: "aws s3 ls", Output: "2026-01-01 00:00:00 2048 20260101-000000.pgdump\n"},
		ssh.MockCommand{Match: "aws s3 cp", Output: "download: done\n"},
		ssh.MockCommand{Match: "mktemp -d '/tmp/teploy-restore.XXXXXX'", Output: "/tmp/teploy-restore.abc123\n"},
		ssh.MockCommand{Match: "gzip -t", Output: ""},
		ssh.MockCommand{Match: "docker exec -i 'myapp-postgres' pg_restore -C -l", Output: pgTOC},
		ssh.MockCommand{Match: "docker exec 'myapp-postgres' psql -X -qtA -v ON_ERROR_STOP=1 -U 'postgres' -d 'postgres' -c 'SELECT current_setting", Output: "170004|t|t|appowner|UTF8|en_US.utf8|en_US.utf8|1000000\n"},
		ssh.MockCommand{Match: "docker inspect -f '{{range .Config.Env}}", Output: "PGDATA=/var/lib/postgresql/data\n"},
		ssh.MockCommand{Match: "docker exec 'myapp-postgres' df -Pk", Output: "Filesystem 1024-blocks Used Available Capacity Mounted on\n/dev/vdb1 100000000 10 90000000 1% /var/lib/postgresql/data\n"},
		ssh.MockCommand{Match: "docker exec 'myapp-postgres' psql", Output: ""},
		ssh.MockCommand{Match: "docker exec -i 'myapp-postgres' pg_restore --exit-on-error", Output: ""},
		ssh.MockCommand{Match: "docker cp", Output: pgDBLevelSQL},
		ssh.MockCommand{Match: "docker exec -u 0", Output: ""},
		ssh.MockCommand{Match: "docker exec -i 'myapp-postgres' psql -X -q -U", Output: ""},
		ssh.MockCommand{Match: "gunzip -c", Output: ""},
		ssh.MockCommand{Match: "rm -rf", Output: ""},
	)
	return ssh.NewMockExecutor("1.2.3.4", cmds...)
}

func pgClient(mock *ssh.MockExecutor) *Client {
	c := NewClient(mock, io.Discard)
	c.retryDelay = time.Millisecond
	return c
}

func pgRestore(t *testing.T, mock *ssh.MockExecutor) (RestoreRecovery, error) {
	t.Helper()
	return pgClient(mock).AccessoryRestoreKeep(context.Background(), "myapp", "postgres", "postgres:16",
		"20260101-000000", nil, S3Config{Bucket: "my-bucket", Region: "us-east-1"})
}

// pgSteps indexes the first call of each step (-1 = absent).
func pgSteps(calls []string) map[string]int {
	o := map[string]int{"validate": -1, "create": -1, "load": -1, "rename-old": -1, "rename-new": -1, "dblevel": -1, "drop-live": -1, "drop-new": -1}
	set := func(k string, i int) {
		if o[k] < 0 {
			o[k] = i
		}
	}
	for i, c := range calls {
		switch {
		case strings.Contains(c, "pg_restore -C -l") || strings.HasPrefix(c, "gzip -t"):
			set("validate", i)
		case strings.Contains(c, `CREATE DATABASE "myapp_teploy_new_`):
			set("create", i)
		case strings.Contains(c, "pg_restore --exit-on-error") || (strings.Contains(c, "gunzip -c") && strings.Contains(c, " psql ")):
			set("load", i)
		case strings.Contains(c, `ALTER DATABASE "myapp" RENAME TO "myapp_teploy_old_`):
			set("rename-old", i)
		case strings.Contains(c, `RENAME TO "myapp"`) && strings.Contains(c, `ALTER DATABASE "myapp_teploy_new_`):
			set("rename-new", i)
		case strings.HasPrefix(c, "docker exec -i 'myapp-postgres' psql -X -q -U"):
			set("dblevel", i)
		case strings.Contains(c, `DROP DATABASE IF EXISTS "myapp"`) && !strings.Contains(c, "teploy_"):
			set("drop-live", i)
		case strings.Contains(c, `DROP DATABASE IF EXISTS "myapp_teploy_new_`):
			set("drop-new", i)
		}
	}
	return o
}

// The swap restore never leaves the live database empty: the backup loads
// into a NEW database, and only a fully loaded copy is renamed into place,
// with the previous database kept aside (U4 + the review's emptied-db
// blocker). No --single-transaction (lock-table exhaustion on big schemas).
func TestPostgresRestore_SwapSequence(t *testing.T) {
	mock := pgRestoreMock()
	rec, err := pgRestore(t, mock)
	if err != nil {
		t.Fatal(err)
	}
	o := pgSteps(mock.Calls)
	for _, k := range []string{"validate", "create", "load", "rename-old", "rename-new", "dblevel"} {
		if o[k] < 0 {
			t.Fatalf("step %s missing; calls: %v", k, mock.Calls)
		}
	}
	if !(o["validate"] < o["create"] && o["create"] < o["load"] && o["load"] < o["rename-old"] && o["rename-old"] < o["rename-new"] && o["rename-new"] < o["dblevel"]) {
		t.Fatalf("steps out of order: %v", o)
	}
	if o["drop-live"] >= 0 {
		t.Fatal("the live database must never be dropped by the restore")
	}
	for _, c := range mock.Calls {
		if strings.Contains(c, "--single-transaction") || strings.Contains(c, "--no-owner") {
			t.Errorf("no single-transaction load and no ownership shortcuts: %s", c)
		}
		if strings.Contains(c, `CREATE DATABASE "myapp_teploy_new_`) && !strings.Contains(c, `OWNER "appowner" TEMPLATE template0 ENCODING `) {
			t.Errorf("the replacement keeps the owner, encoding and locale: %s", c)
		}
	}
	if !strings.HasPrefix(rec.PGDatabase, "myapp_teploy_old_") || rec.Path != "" {
		t.Fatalf("the previous database must be the recovery copy, got %+v", rec)
	}
	// Database-level entries are re-applied, minus CREATE/OWNER/meta lines.
	sql := string(mock.Files["/tmp/teploy-restore.abc123/dblevel.sql"])
	if !strings.Contains(sql, "ALTER DATABASE myapp SET search_path") || !strings.Contains(sql, "REVOKE CONNECT") || !strings.Contains(sql, "COMMENT ON DATABASE") {
		t.Errorf("database-level settings and ACL must be re-applied: %q", sql)
	}
	if strings.Contains(sql, "CREATE DATABASE") || strings.Contains(sql, `\connect`) || strings.Contains(sql, "OWNER TO") {
		t.Errorf("CREATE DATABASE / OWNER / psql meta lines must be filtered: %q", sql)
	}
	list := string(mock.Files["/tmp/teploy-restore.abc123/dblevel.list"])
	if strings.Contains(list, "TABLE") || !strings.Contains(list, "DATABASE PROPERTIES") || !strings.Contains(list, "ACL - DATABASE") {
		t.Errorf("only the database-level TOC entries are selected: %q", list)
	}

	// The wrapper releases the recovery copy (drops the previous db) only
	// after a successful restore.
	mock = pgRestoreMock()
	if err := pgClient(mock).AccessoryRestore(context.Background(), "myapp", "postgres", "postgres:16", "20260101-000000", nil, S3Config{Bucket: "my-bucket", Region: "us-east-1"}); err != nil {
		t.Fatal(err)
	}
	last := ""
	for _, c := range mock.Calls {
		if strings.Contains(c, "DROP DATABASE") {
			last = c
		}
	}
	if !strings.Contains(last, `DROP DATABASE IF EXISTS "myapp_teploy_old_`) || !strings.Contains(last, "WITH (FORCE)") {
		t.Fatalf("release must drop the kept previous database, got %q", last)
	}
}

// Reviewer's blocker: the load fails for a reason a safety reload would
// share (lock exhaustion, a non-superuser owner creating a superuser-only
// extension). The live database is untouched; only the partial
// replacement is dropped, and the error is NOT recovery-incomplete.
func TestPostgresRestore_LoadFailureLeavesDatabaseUntouched(t *testing.T) {
	mock := pgRestoreMock(ssh.MockCommand{Match: "docker exec -i 'myapp-postgres' pg_restore --exit-on-error", Err: errors.New("exit status 1: pg_restore: error: could not execute query: ERROR:  out of shared memory")})
	_, err := pgRestore(t, mock)
	if err == nil || !strings.Contains(err.Error(), "database untouched") || IsRecoveryIncomplete(err) {
		t.Fatalf("want an untouched, recoverable failure, got %v", err)
	}
	o := pgSteps(mock.Calls)
	if o["rename-old"] >= 0 || o["drop-live"] >= 0 {
		t.Fatalf("the live database was touched: %v", mock.Calls)
	}
	if o["drop-new"] < 0 {
		t.Fatal("the partial replacement must be dropped")
	}
}

// A refused rename (a session the role may not end: a superuser's, a
// prepared transaction's) leaves the live database intact under its name:
// "database untouched", not recovery-incomplete, so the app is restarted.
func TestPostgresRestore_RefusedSwapIsUntouched(t *testing.T) {
	mock := pgRestoreMock(ssh.MockCommand{Match: "docker exec 'myapp-postgres' psql -X -qtA -v ON_ERROR_STOP=1 -U 'postgres' -d 'postgres' -c 'SELECT count(pg_terminate_backend(pid)) FROM pg_stat_activity WHERE datname = '\"'\"'myapp'\"'\"'",
		Err: errors.New(`exit status 1: ERROR:  database "myapp" is being accessed by other users`)})
	_, err := pgRestore(t, mock)
	if err == nil || !strings.Contains(err.Error(), "database untouched") || IsRecoveryIncomplete(err) {
		t.Fatalf("want an untouched failure, got %v", err)
	}
	renames := 0
	for _, c := range mock.Calls {
		if strings.Contains(c, `ALTER DATABASE "myapp" RENAME TO`) {
			renames++
		}
	}
	if renames < 2 {
		t.Errorf("a rename refused for open sessions is retried, got %d attempts", renames)
	}
	if pgSteps(mock.Calls)["drop-new"] < 0 {
		t.Fatal("the unused replacement must be dropped")
	}
}

// The only recovery-incomplete case: the new database cannot be renamed
// into place AND the previous one cannot be renamed back.
func TestPostgresRestore_SwapBackFailureIsRecoveryIncomplete(t *testing.T) {
	mock := pgRestoreMock(
		ssh.MockCommand{Match: "docker exec 'myapp-postgres' psql -X -qtA -v ON_ERROR_STOP=1 -U 'postgres' -d 'postgres' -c 'SELECT count(pg_terminate_backend(pid)) FROM pg_stat_activity WHERE datname = '\"'\"'myapp_teploy_", Err: errors.New("exit status 1: ERROR:  permission denied")},
	)
	rec, err := pgRestore(t, mock)
	if err == nil || !IsRecoveryIncomplete(err) {
		t.Fatalf("want recovery-incomplete, got %v", err)
	}
	if !strings.HasPrefix(rec.PGDatabase, "myapp_teploy_old_") || !strings.Contains(err.Error(), rec.PGDatabase) {
		t.Fatalf("both copies are kept and named: rec=%+v err=%v", rec, err)
	}
}

// A role that is neither superuser nor owner-with-CREATEDB is refused
// before anything is created.
func TestPostgresRestore_PreflightRefusesUnprivilegedRole(t *testing.T) {
	for _, state := range []string{
		"170004|f|t|someoneelse|UTF8|C|C|100\n", // not the owner
		"170004|f|f|postgres|UTF8|C|C|100\n",    // owner without CREATEDB
	} {
		mock := pgRestoreMock(ssh.MockCommand{Match: "docker exec 'myapp-postgres' psql -X -qtA -v ON_ERROR_STOP=1 -U 'postgres' -d 'postgres' -c 'SELECT current_setting", Output: state})
		_, err := pgRestore(t, mock)
		if err == nil || !strings.Contains(err.Error(), "nothing was changed") {
			t.Fatalf("state %q: want a preflight refusal, got %v", state, err)
		}
		if pgSteps(mock.Calls)["create"] >= 0 {
			t.Fatalf("state %q: refused restore created a database", state)
		}
	}
	// Owner with CREATEDB (a Bitnami POSTGRESQL_USERNAME role) may restore.
	mock := pgRestoreMock(ssh.MockCommand{Match: "docker exec 'myapp-postgres' psql -X -qtA -v ON_ERROR_STOP=1 -U 'postgres' -d 'postgres' -c 'SELECT current_setting", Output: "170004|f|t|postgres|UTF8|C|C|100\n"})
	if _, err := pgRestore(t, mock); err != nil {
		t.Fatalf("owner with CREATEDB: %v", err)
	}
}

func TestPostgresRestore_RefusesWithoutRoomForTheReplacement(t *testing.T) {
	mock := pgRestoreMock(ssh.MockCommand{Match: "docker exec 'myapp-postgres' df -Pk", Output: "Filesystem 1024-blocks Used Available Capacity Mounted on\n/dev/vdb1 1000 990 10 99% /var/lib/postgresql/data\n"})
	_, err := pgRestore(t, mock)
	if err == nil || !strings.Contains(err.Error(), "not enough free space") {
		t.Fatalf("want a space refusal, got %v", err)
	}
	if pgSteps(mock.Calls)["create"] >= 0 {
		t.Fatal("refused before creating anything")
	}
}

// Legacy plain .sql.gz backups (pre-G7) load with psql into the
// replacement, then swap.
func TestPostgresRestore_LegacyPlainDump(t *testing.T) {
	mock := pgRestoreMock(ssh.MockCommand{Match: "aws s3 ls", Output: "2026-01-01 00:00:00 99 20260101-000000.sql.gz\n"})
	if _, err := pgRestore(t, mock); err != nil {
		t.Fatal(err)
	}
	o := pgSteps(mock.Calls)
	if o["load"] < 0 || o["rename-new"] < 0 {
		t.Fatalf("legacy restore must load then swap: %v", mock.Calls)
	}
	for _, c := range mock.Calls {
		if strings.Contains(c, "gunzip -c") && strings.Contains(c, " psql ") && !strings.Contains(c, "-v ON_ERROR_STOP=1") {
			t.Errorf("psql must stop on errors: %s", c)
		}
	}
}

func TestPostgresRestore_CorruptArtifactTouchesNothing(t *testing.T) {
	mock := pgRestoreMock(
		ssh.MockCommand{Match: "aws s3 ls", Output: "2026-01-01 00:00:00 99 20260101-000000.sql.gz\n"},
		ssh.MockCommand{Match: "gzip -t", Err: errors.New("exit status 1: gzip: stdin: not in gzip format")},
	)
	_, err := pgRestore(t, mock)
	if err == nil || !strings.Contains(err.Error(), "not in gzip format") || !strings.Contains(err.Error(), "database untouched") || !strings.Contains(err.Error(), "/tmp/teploy-restore.abc123") {
		t.Fatalf("want an untouched failure naming the kept temp dir, got %v", err)
	}
	if pgSteps(mock.Calls)["create"] >= 0 {
		t.Fatal("a corrupt artifact must be refused before anything is created")
	}
	for _, c := range mock.Calls {
		if strings.HasPrefix(c, "rm -rf '/tmp/teploy-restore.") {
			t.Fatalf("failure keeps the temp dir: %s", c)
		}
	}
}

// Every pre-G6 backup of a reclassified engine (here pgvector) is a raw
// .tar.gz: it is still restorable — as files, with the accessory stopped —
// and the artifact (not the engine) is what says so.
func TestLegacyTarOfReclassifiedEngineIsAFileRestore(t *testing.T) {
	ctx := context.Background()
	s3 := S3Config{Bucket: "b", Region: "us-east-1"}
	env := map[string]string{"PG_MAJOR": "17"}
	listing := ssh.MockCommand{Match: "aws s3 ls", Output: "2026-01-01 00:00:00 4096 20260101-000000.tar.gz\n"}

	art, err := NewClient(ssh.NewMockExecutor("h", listing), io.Discard).ResolveAccessoryArtifact(ctx, "myapp", "db", "pgvector/pgvector:pg17", "20260101-000000", env, s3)
	if err != nil || !art.FileRestore || art.Engine.Engine != EnginePostgres || art.Ext != ".tar.gz" {
		t.Fatalf("artifact = %+v, %v", art, err)
	}
	native, err := NewClient(ssh.NewMockExecutor("h", ssh.MockCommand{Match: "aws s3 ls", Output: "2026-01-01 00:00:00 4096 20260101-000000.pgdump\n"}), io.Discard).ResolveAccessoryArtifact(ctx, "myapp", "db", "pgvector/pgvector:pg17", "20260101-000000", env, s3)
	if err != nil || native.FileRestore {
		t.Fatalf("a .pgdump is an engine restore: %+v %v", native, err)
	}

	running := ssh.NewMockExecutor("h", listing,
		ssh.MockCommand{Match: "which aws", Output: "/usr/bin/aws\n"},
		ssh.MockCommand{Match: "docker inspect -f '{{.State.Running}}'", Output: "true\n"})
	if _, err := NewClient(running, io.Discard).AccessoryRestoreKeep(ctx, "myapp", "db", "pgvector/pgvector:pg17", "20260101-000000", env, s3); err == nil || !strings.Contains(err.Error(), "must be stopped") {
		t.Fatalf("a file restore into a running accessory must be refused, got %v", err)
	}

	stopped := ssh.NewMockExecutor("h", listing,
		ssh.MockCommand{Match: "which aws", Output: "/usr/bin/aws\n"},
		ssh.MockCommand{Match: "docker inspect -f '{{.State.Running}}'", Output: "false\n"},
		ssh.MockCommand{Match: "mktemp -d '/tmp/teploy-restore.XXXXXX'", Output: "/tmp/teploy-restore.abc123\n"},
		ssh.MockCommand{Match: "aws s3 cp", Output: ""},
		ssh.MockCommand{Match: "rm -rf", Output: ""},
		ssh.MockCommand{Match: "mkdir -p", Output: ""},
		ssh.MockCommand{Match: "mktemp -d", Output: "/deployments/myapp/accessories/db.restore-old.x\n"},
		ssh.MockCommand{Match: "find", Output: ""},
		ssh.MockCommand{Match: "cp -a", Output: ""},
	)
	rec, err := NewClient(stopped, io.Discard).AccessoryRestoreKeep(ctx, "myapp", "db", "pgvector/pgvector:pg17", "20260101-000000", env, s3)
	if err != nil || rec.Path != "/deployments/myapp/accessories/db.restore-old.x" {
		t.Fatalf("file restore = %+v, %v", rec, err)
	}
	extracted := false
	for _, c := range stopped.Calls {
		if strings.Contains(c, "tar -xzf '/tmp/teploy-restore.abc123/restore.tar.gz'") {
			extracted = true
		}
		if strings.Contains(c, "pg_restore") {
			t.Fatalf("a tarball is not fed to pg_restore: %s", c)
		}
	}
	if !extracted {
		t.Fatalf("the tarball was not staged: %v", stopped.Calls)
	}
}

// ---- mysql ----

func mysqlRestoreMock(overrides ...ssh.MockCommand) *ssh.MockExecutor {
	cmds := append([]ssh.MockCommand{}, overrides...)
	cmds = append(cmds,
		ssh.MockCommand{Match: "which aws", Output: "/usr/bin/aws\n"},
		ssh.MockCommand{Match: "aws s3 ls", Output: "2026-01-01 00:00:00 2048 20260101-000000.sql.gz\n"},
		ssh.MockCommand{Match: "aws s3 cp", Output: ""},
		ssh.MockCommand{Match: "mktemp -d '/tmp/teploy-restore.XXXXXX'", Output: "/tmp/teploy-restore.abc123\n"},
		ssh.MockCommand{Match: "umask 077; gunzip -c", Output: ""},
		ssh.MockCommand{Match: "docker exec -i 'myapp-db' sh -c 'IFS= read -r teploy_credential || exit 1; MYSQL_PWD=${teploy_credential#MYSQL_PWD=}; export MYSQL_PWD; exec mysql -u root -N -B", Output: "utf8mb4|utf8mb4_0900_ai_ci|16384\n"},
		ssh.MockCommand{Match: "df -Pk '/var/tmp'", Output: "Filesystem 1024-blocks Used Available Capacity Mounted on\n/dev/vda1 100000 10 90000 1% /\n"},
		ssh.MockCommand{Match: "umask 077; mktemp -d /var/tmp/teploy-pre-restore.XXXXXX", Output: "/var/tmp/teploy-pre-restore.m\n"},
		ssh.MockCommand{Match: "docker exec -i 'myapp-db' sh -c 'IFS= read -r teploy_credential || exit 1; MYSQL_PWD=${teploy_credential#MYSQL_PWD=}; export MYSQL_PWD; exec mysqldump", Output: ""},
		ssh.MockCommand{Match: "docker exec -i 'myapp-db' sh -c 'IFS= read -r teploy_credential || exit 1; MYSQL_PWD=${teploy_credential#MYSQL_PWD=}; export MYSQL_PWD; exec mysql -u root -e", Output: ""},
		ssh.MockCommand{Match: "rm -f ", Output: ""},
		ssh.MockCommand{Match: "rm -rf", Output: ""},
	)
	return ssh.NewMockExecutor("1.2.3.4", cmds...)
}

func mysqlRestore(mock *ssh.MockExecutor) (RestoreRecovery, error) {
	return NewClient(mock, io.Discard).AccessoryRestoreKeep(context.Background(), "myapp", "db", "mysql:8",
		"20260101-000000", map[string]string{"MYSQL_VERSION": "8.4", "MYSQL_ROOT_PASSWORD": "s3cret", "MYSQL_DATABASE": "appdb"}, S3Config{Bucket: "b", Region: "us-east-1"})
}

// MySQL: safety dump, DROP + CREATE with the original charset (so a table
// created after the backup does not survive), load; the safety dump is the
// recovery copy.
func TestMySQLRestore_SafetyDumpThenRecreate(t *testing.T) {
	mock := mysqlRestoreMock()
	rec, err := mysqlRestore(mock)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Path != "/var/tmp/teploy-pre-restore.m" {
		t.Fatalf("the safety dump is the recovery copy: %+v", rec)
	}
	safety, recreate, load := -1, -1, -1
	for i, c := range mock.Calls {
		switch {
		case strings.Contains(c, "exec mysqldump -u root --single-transaction --routines --triggers --events"):
			safety = i
		case strings.Contains(c, "DROP DATABASE IF EXISTS `appdb`; CREATE DATABASE `appdb` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci"):
			recreate = i
		case strings.Contains(c, "cat '/tmp/teploy-restore.abc123/restore.sql'"):
			load = i
		}
		if strings.Contains(c, "s3cret") {
			t.Fatalf("password in a command: %s", c)
		}
	}
	if !(safety >= 0 && safety < recreate && recreate < load) {
		t.Fatalf("want safety < recreate < load, got %d %d %d: %v", safety, recreate, load, mock.Calls)
	}
}

// A failed load reloads the safety dump (recoverable); when that fails
// too the error is recovery-incomplete so the app is not restarted on a
// half-restored database.
func TestMySQLRestore_FailureClassification(t *testing.T) {
	loadFails := ssh.MockCommand{Match: "rm -f '/tmp/teploy-restore.abc123/load.failed'", Err: errors.New("exit status 1: ERROR 1064 at line 9")}
	rec, err := mysqlRestore(mysqlRestoreMock(loadFails))
	if err == nil || IsRecoveryIncomplete(err) || !strings.Contains(err.Error(), "reloaded from the safety dump") || !rec.IsZero() {
		t.Fatalf("want a recoverable failure, got %v (%+v)", err, rec)
	}
	reloadFails := ssh.MockCommand{Match: "rm -f '/tmp/teploy-restore.abc123/reload.failed'", Err: errors.New("exit status 1: disk full")}
	rec, err = mysqlRestore(mysqlRestoreMock(loadFails, reloadFails))
	if err == nil || !IsRecoveryIncomplete(err) || rec.Path != "/var/tmp/teploy-pre-restore.m" || !strings.Contains(err.Error(), "pre-restore.sql") {
		t.Fatalf("want recovery-incomplete naming the safety dump, got %v (%+v)", err, rec)
	}
}

func TestMySQLRestore_TruncatedDumpTouchesNothing(t *testing.T) {
	mock := mysqlRestoreMock(ssh.MockCommand{Match: "umask 077; gunzip -c", Err: errors.New("exit status 1")})
	_, err := mysqlRestore(mock)
	if err == nil || !strings.Contains(err.Error(), "database untouched") {
		t.Fatalf("got %v", err)
	}
	for _, c := range mock.Calls {
		if strings.Contains(c, "DROP DATABASE") {
			t.Fatalf("dropped after a bad dump: %s", c)
		}
	}
}

// ---- mongo ----

func TestMongoRestore_FailureClassification(t *testing.T) {
	base := func(o ...ssh.MockCommand) *ssh.MockExecutor {
		cmds := append(o,
			ssh.MockCommand{Match: "which aws", Output: "/usr/bin/aws\n"},
			ssh.MockCommand{Match: "aws s3 ls", Output: "2026-01-01 00:00:00 2048 20260101-000000.archive.gz\n"},
			ssh.MockCommand{Match: "aws s3 cp", Output: ""},
			ssh.MockCommand{Match: "mktemp -d '/tmp/teploy-restore.XXXXXX'", Output: "/tmp/teploy-restore.abc123\n"},
			ssh.MockCommand{Match: "gzip -t", Output: ""},
			ssh.MockCommand{Match: "umask 077; mktemp -d /var/tmp/teploy-pre-restore.XXXXXX", Output: "/var/tmp/teploy-pre-restore.g\n"},
			ssh.MockCommand{Match: "docker exec 'myapp-db' mongodump", Output: ""},
			ssh.MockCommand{Match: "docker exec -i 'myapp-db' mongorestore", Output: ""},
			ssh.MockCommand{Match: "rm -", Output: ""},
		)
		return ssh.NewMockExecutor("h", cmds...)
	}
	run := func(m *ssh.MockExecutor) (RestoreRecovery, error) {
		return NewClient(m, io.Discard).AccessoryRestoreKeep(context.Background(), "myapp", "db", "mongo:7", "20260101-000000", map[string]string{"MONGO_VERSION": "7.0"}, S3Config{Bucket: "b", Region: "us-east-1"})
	}
	if rec, err := run(base()); err != nil || rec.Path != "/var/tmp/teploy-pre-restore.g" {
		t.Fatalf("success keeps the safety dump as recovery: %+v %v", rec, err)
	}
	first := ssh.MockCommand{Match: "docker exec -i 'myapp-db' mongorestore --archive --gzip --drop < '/tmp/", Err: errors.New("exit status 1")}
	if _, err := run(base(first)); err == nil || IsRecoveryIncomplete(err) {
		t.Fatalf("a reloaded safety dump is recoverable: %v", err)
	}
	both := ssh.MockCommand{Match: "docker exec -i 'myapp-db' mongorestore --archive --gzip --drop < '/var/tmp/", Err: errors.New("exit status 1")}
	if _, err := run(base(first, both)); err == nil || !IsRecoveryIncomplete(err) {
		t.Fatalf("a failed reload is recovery-incomplete: %v", err)
	}
}

// Dragonfly: when DFLY LOAD and the pre-restore reload both fail the
// script's marker makes the error recovery-incomplete.
func TestDragonflyDoubleFailureIsRecoveryIncomplete(t *testing.T) {
	mock := ssh.NewMockExecutor("h",
		ssh.MockCommand{Match: "which aws", Output: "/usr/bin/aws\n"},
		ssh.MockCommand{Match: "aws s3 ls", Output: "2026-01-01 00:00:00 2048 20260101-000000.rdb.gz\n"},
		ssh.MockCommand{Match: "aws s3 cp", Output: ""},
		ssh.MockCommand{Match: "mktemp -d '/tmp/teploy-restore.XXXXXX'", Output: "/tmp/teploy-restore.abc123\n"},
		ssh.MockCommand{Match: "umask 077; mktemp -d /var/tmp/teploy-pre-restore.XXXXXX", Output: "/var/tmp/teploy-pre-restore.d\n"},
		ssh.MockCommand{Match: "set -eu", Err: errors.New("Process exited with status 3: " + recoveryIncompleteMarker + ": DFLY LOAD of the backup failed")},
		ssh.MockCommand{Match: "rm -", Output: ""},
	)
	rec, err := NewClient(mock, io.Discard).AccessoryRestoreKeep(context.Background(), "myapp", "cache", "docker.dragonflydb.io/dragonflydb/dragonfly:latest", "20260101-000000", nil, S3Config{Bucket: "b", Region: "us-east-1"})
	if err == nil || !IsRecoveryIncomplete(err) || rec.Path != "/var/tmp/teploy-pre-restore.d" {
		t.Fatalf("got %v (%+v)", err, rec)
	}
	s := dragonflyLoadScript("c", "/tmp/r/a.rdb.gz", "/tmp/r", "/var/tmp/p/pre.rdb")
	if !strings.Contains(s, "trap 'docker exec -u 0 'c' rm -f") || strings.Index(s, "trap ") > strings.Index(s, "save rdb teploy-pre-restore-") {
		t.Errorf("in-container snapshots are trap-cleaned from before the first SAVE:\n%s", s)
	}
}
