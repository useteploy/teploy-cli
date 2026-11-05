package backup

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

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
		ssh.MockCommand{Match: "mktemp -d '/var/tmp/teploy-restore.XXXXXX'", Output: "/tmp/teploy-restore.abc123\n"},
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
	return newStoreMock("1.2.3.4", cmds...)
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
	o := map[string]int{"validate": -1, "create": -1, "load": -1, "swap": -1, "dblevel": -1, "drop-live": -1, "drop-new": -1}
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
		case strings.Contains(c, `RENAME TO "myapp"`):
			set("swap", i)
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
	for _, k := range []string{"validate", "create", "load", "swap", "dblevel"} {
		if o[k] < 0 {
			t.Fatalf("step %s missing; calls: %v", k, mock.Calls)
		}
	}
	if !(o["validate"] < o["create"] && o["create"] < o["load"] && o["load"] < o["swap"] && o["swap"] < o["dblevel"]) {
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
	if o["swap"] >= 0 || o["drop-live"] >= 0 {
		t.Fatalf("the live database was touched: %v", mock.Calls)
	}
	if o["drop-new"] < 0 {
		t.Fatal("the partial replacement must be dropped")
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
	if o["load"] < 0 || o["swap"] < 0 {
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

	art, err := NewClient(newStoreMock("h", listing), io.Discard).ResolveAccessoryArtifact(ctx, "myapp", "db", "pgvector/pgvector:pg17", "20260101-000000", env, s3)
	if err != nil || !art.FileRestore || art.Engine.Engine != EnginePostgres || art.Ext != ".tar.gz" {
		t.Fatalf("artifact = %+v, %v", art, err)
	}
	native, err := NewClient(newStoreMock("h", ssh.MockCommand{Match: "aws s3 ls", Output: "2026-01-01 00:00:00 4096 20260101-000000.pgdump\n"}), io.Discard).ResolveAccessoryArtifact(ctx, "myapp", "db", "pgvector/pgvector:pg17", "20260101-000000", env, s3)
	if err != nil || native.FileRestore {
		t.Fatalf("a .pgdump is an engine restore: %+v %v", native, err)
	}

	running := newStoreMock("h", listing,
		ssh.MockCommand{Match: "which aws", Output: "/usr/bin/aws\n"},
		ssh.MockCommand{Match: "docker inspect -f '{{.State.Running}}'", Output: "true\n"})
	if _, err := NewClient(running, io.Discard).AccessoryRestoreKeep(ctx, "myapp", "db", "pgvector/pgvector:pg17", "20260101-000000", env, s3); err == nil || !strings.Contains(err.Error(), "must be stopped") {
		t.Fatalf("a file restore into a running accessory must be refused, got %v", err)
	}

	stopped := newStoreMock("h", listing,
		ssh.MockCommand{Match: "which aws", Output: "/usr/bin/aws\n"},
		ssh.MockCommand{Match: "docker inspect -f '{{.State.Running}}'", Output: "false\n"},
		ssh.MockCommand{Match: "mktemp -d '/var/tmp/teploy-restore.XXXXXX'", Output: "/tmp/teploy-restore.abc123\n"},
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
		ssh.MockCommand{Match: "mktemp -d '/var/tmp/teploy-restore.XXXXXX'", Output: "/tmp/teploy-restore.abc123\n"},
		ssh.MockCommand{Match: "umask 077; gunzip -c", Output: ""},
		ssh.MockCommand{Match: "docker exec -i 'myapp-db' sh -c 'IFS= read -r teploy_credential || exit 1; MYSQL_PWD=${teploy_credential#MYSQL_PWD=}; export MYSQL_PWD; exec mysql -u root -N -B", Output: "utf8mb4|utf8mb4_0900_ai_ci|16384|0|0\n"},
		ssh.MockCommand{Match: "df -Pk '/var/tmp'", Output: "Filesystem 1024-blocks Used Available Capacity Mounted on\n/dev/vda1 100000 10 90000 1% /\n"},
		ssh.MockCommand{Match: "umask 077; mktemp -d '/var/tmp/teploy-pre-restore.myapp@db@XXXXXX'", Output: "/var/tmp/teploy-pre-restore.m\n"},
		ssh.MockCommand{Match: "docker exec -i 'myapp-db' sh -c 'IFS= read -r teploy_credential || exit 1; MYSQL_PWD=${teploy_credential#MYSQL_PWD=}; export MYSQL_PWD; exec mysqldump", Output: ""},
		ssh.MockCommand{Match: "docker exec -i 'myapp-db' sh -c 'IFS= read -r teploy_credential || exit 1; MYSQL_PWD=${teploy_credential#MYSQL_PWD=}; export MYSQL_PWD; exec mysql -u root -e", Output: ""},
		ssh.MockCommand{Match: "rm -f ", Output: ""},
		ssh.MockCommand{Match: "rm -rf", Output: ""},
	)
	return newStoreMock("1.2.3.4", cmds...)
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
			ssh.MockCommand{Match: "mktemp -d '/var/tmp/teploy-restore.XXXXXX'", Output: "/tmp/teploy-restore.abc123\n"},
			ssh.MockCommand{Match: "gzip -t", Output: ""},
			ssh.MockCommand{Match: "umask 077; mktemp -d '/var/tmp/teploy-pre-restore.myapp@db@XXXXXX'", Output: "/var/tmp/teploy-pre-restore.g\n"},
			ssh.MockCommand{Match: "docker exec 'myapp-db' mongodump", Output: ""},
			ssh.MockCommand{Match: "docker exec 'myapp-db' sh -c", Output: ""},
			ssh.MockCommand{Match: "docker exec -i 'myapp-db' mongorestore", Output: ""},
			ssh.MockCommand{Match: "rm -", Output: ""},
		)
		return newStoreMock("h", cmds...)
	}
	run := func(m *ssh.MockExecutor) (RestoreRecovery, error) {
		return NewClient(m, io.Discard).AccessoryRestoreKeep(context.Background(), "myapp", "db", "mongo:7", "20260101-000000", map[string]string{"MONGO_VERSION": "7.0"}, S3Config{Bucket: "b", Region: "us-east-1"})
	}
	if rec, err := run(base()); err != nil || rec.Path != "/var/tmp/teploy-pre-restore.g" {
		t.Fatalf("success keeps the safety dump as recovery: %+v %v", rec, err)
	}
	first := ssh.MockCommand{Match: "docker exec -i 'myapp-db' mongorestore --archive --gzip --drop --nsExclude='teploy_check_*.*' < '/tmp/", Err: errors.New("exit status 1")}
	if _, err := run(base(first)); err == nil || IsRecoveryIncomplete(err) {
		t.Fatalf("a reloaded safety dump is recoverable: %v", err)
	}
	both := ssh.MockCommand{Match: "docker exec -i 'myapp-db' mongorestore --archive --gzip --drop --nsExclude='teploy_check_*.*' < '/var/tmp/", Err: errors.New("exit status 1")}
	if _, err := run(base(first, both)); err == nil || !IsRecoveryIncomplete(err) {
		t.Fatalf("a failed reload is recovery-incomplete: %v", err)
	}
}

// Dragonfly: when DFLY LOAD and the pre-restore reload both fail the
// script's marker makes the error recovery-incomplete.
func TestDragonflyDoubleFailureIsRecoveryIncomplete(t *testing.T) {
	mock := newStoreMock("h",
		ssh.MockCommand{Match: "which aws", Output: "/usr/bin/aws\n"},
		ssh.MockCommand{Match: "aws s3 ls", Output: "2026-01-01 00:00:00 2048 20260101-000000.rdb.gz\n"},
		ssh.MockCommand{Match: "aws s3 cp", Output: ""},
		ssh.MockCommand{Match: "mktemp -d '/var/tmp/teploy-restore.XXXXXX'", Output: "/tmp/teploy-restore.abc123\n"},
		ssh.MockCommand{Match: "umask 077; mktemp -d '/var/tmp/teploy-pre-restore.myapp@cache@XXXXXX'", Output: "/var/tmp/teploy-pre-restore.d\n"},
		ssh.MockCommand{Match: "set -eu", Err: errors.New("Process exited with status 3: " + recoveryIncompleteMarker + ": DFLY LOAD of the backup failed")},
		ssh.MockCommand{Match: "rm -", Output: ""},
	)
	rec, err := NewClient(mock, io.Discard).AccessoryRestoreKeep(context.Background(), "myapp", "cache", "docker.dragonflydb.io/dragonflydb/dragonfly:latest", "20260101-000000", nil, S3Config{Bucket: "b", Region: "us-east-1"})
	if err == nil || !IsRecoveryIncomplete(err) || rec.Path != "/var/tmp/teploy-pre-restore.d" {
		t.Fatalf("got %v (%+v)", err, rec)
	}
	s := dragonflyLoadScript("c", "/tmp/r/a.rdb.gz", "/tmp/r", "/var/tmp/p/pre.rdb")
	if !strings.Contains(s, "cleanup_snapshots() { docker exec -u 0 'c' rm -f") || strings.Index(s, "trap ") > strings.Index(s, "save rdb teploy-pre-restore-") {
		t.Errorf("in-container snapshots are trap-cleaned from before the first SAVE:\n%s", s)
	}
}

// pgSwapExec answers a swap and the pg_database re-read that follows a failed
// one, whose names carry random suffixes prefix mocks cannot match.
type pgSwapExec struct {
	*ssh.MockExecutor
	swapErr  error    // what the swap command returns
	present  []string // "live", "new", "old": what the re-read shows
	readErr  error
	swapRuns int
}

var pgNewName = regexp.MustCompile(`myapp_teploy_new_[0-9a-f]{8}`)
var pgOldName = regexp.MustCompile(`myapp_teploy_old_[0-9a-f]{8}`)

func (e *pgSwapExec) Run(ctx context.Context, cmd string) (string, error) {
	switch {
	case strings.Contains(cmd, `RENAME TO "myapp"`):
		e.swapRuns++
		e.MockExecutor.Calls = append(e.MockExecutor.Calls, cmd)
		return "", e.swapErr
	case strings.Contains(cmd, "SELECT datname FROM pg_database WHERE datname IN"):
		e.MockExecutor.Calls = append(e.MockExecutor.Calls, cmd)
		if e.readErr != nil {
			return "", e.readErr
		}
		var out []string
		for _, p := range e.present {
			switch p {
			case "live":
				out = append(out, "myapp")
			case "new":
				out = append(out, pgNewName.FindString(cmd))
			case "old":
				out = append(out, pgOldName.FindString(cmd))
			}
		}
		return strings.Join(out, "\n") + "\n", nil
	}
	return e.MockExecutor.Run(ctx, cmd)
}

func pgSwapRestore(e *pgSwapExec) (RestoreRecovery, error) {
	c := NewClient(e, io.Discard)
	c.retryDelay = time.Millisecond
	return c.AccessoryRestoreKeep(context.Background(), "myapp", "postgres", "postgres:16", "20260101-000000", nil, S3Config{Bucket: "my-bucket", Region: "us-east-1"})
}

func pgCalled(calls []string, sub string) bool {
	for _, c := range calls {
		if strings.Contains(c, sub) {
			return true
		}
	}
	return false
}

// The swap is one transaction: both renames in one statement.
func TestPostgresRestore_SwapIsOneTransaction(t *testing.T) {
	mock := pgRestoreMock()
	if _, err := pgRestore(t, mock); err != nil {
		t.Fatal(err)
	}
	var swap string
	for _, c := range mock.Calls {
		if strings.Contains(c, `RENAME TO "myapp"`) {
			swap = c
		}
	}
	if !regexp.MustCompile(`ALTER DATABASE "myapp" RENAME TO "myapp_teploy_old_[0-9a-f]{8}"; ALTER DATABASE "myapp_teploy_new_[0-9a-f]{8}" RENAME TO "myapp"`).MatchString(swap) {
		t.Fatalf("both renames must be one statement: %s", swap)
	}
}

// A refused swap (a session the role may not end) rolled back: untouched,
// not recovery-incomplete, the replacement dropped.
func TestPostgresRestore_RefusedSwapIsUntouched(t *testing.T) {
	e := &pgSwapExec{MockExecutor: pgRestoreMock(), swapErr: errors.New(`exit status 1: ERROR:  database "myapp" is being accessed by other users`), present: []string{"live", "new"}}
	_, err := pgSwapRestore(e)
	if err == nil || !strings.Contains(err.Error(), "database untouched") || IsRecoveryIncomplete(err) {
		t.Fatalf("want an untouched failure, got %v", err)
	}
	if e.swapRuns < 2 {
		t.Errorf("a swap refused for open sessions is retried, got %d attempts", e.swapRuns)
	}
	if pgSteps(e.Calls)["drop-new"] < 0 {
		t.Fatal("the unused replacement must be dropped")
	}
}

// The reviewer's race: the transaction committed but its exit status was lost
// (Ctrl-C, SSH). pg_database shows it worked, so the good replacement must
// NOT be dropped, and the restore succeeded.
func TestPostgresRestore_LostSwapStatusIsReadBack(t *testing.T) {
	e := &pgSwapExec{MockExecutor: pgRestoreMock(), swapErr: errors.New("ssh: connection reset by peer"), present: []string{"live", "old"}}
	rec, err := pgSwapRestore(e)
	if err != nil {
		t.Fatalf("a committed swap is a success: %v", err)
	}
	if !strings.HasPrefix(rec.PGDatabase, "myapp_teploy_old_") {
		t.Fatalf("the previous database is the recovery copy: %+v", rec)
	}
	if pgSteps(e.Calls)["drop-new"] >= 0 {
		t.Fatal("the restored database must not be dropped")
	}
	if len(rec.Warnings) == 0 || !strings.Contains(rec.Warnings[0], "shows it completed") {
		t.Fatalf("the lost status is reported: %v", rec.Warnings)
	}
}

// When the server cannot say what happened, or says something impossible,
// nothing is dropped and the app must not restart.
func TestPostgresRestore_UnknownSwapStateIsRecoveryIncomplete(t *testing.T) {
	for name, e := range map[string]*pgSwapExec{
		"no answer":      {MockExecutor: pgRestoreMock(), swapErr: errors.New("ssh: connection reset by peer"), readErr: errors.New("connection refused")},
		"live name gone": {MockExecutor: pgRestoreMock(), swapErr: errors.New("ssh: connection reset by peer"), present: []string{"old", "new"}},
	} {
		_, err := pgSwapRestore(e)
		if err == nil || !IsRecoveryIncomplete(err) || strings.Contains(err.Error(), "database untouched") {
			t.Errorf("%s: want recovery-incomplete, got %v", name, err)
		}
		if pgSteps(e.Calls)["drop-new"] >= 0 {
			t.Errorf("%s: nothing may be dropped", name)
		}
	}
}

// Disk-full: the load takes the server down. Not "untouched"; the app must
// not restart against a dead server.
func TestPostgresRestore_LoadThatKillsTheServerIsRecoveryIncomplete(t *testing.T) {
	loadFails := ssh.MockCommand{Match: "docker exec -i 'myapp-postgres' pg_restore --exit-on-error", Err: errors.New("exit status 1: FATAL: could not write to file: No space left on device")}
	dead := ssh.MockCommand{Match: "docker exec 'myapp-postgres' psql -X -qtA -v ON_ERROR_STOP=1 -U 'postgres' -d 'postgres' -c 'SELECT 1'", Err: errors.New("exit status 2: connection refused")}
	mock := pgRestoreMock(loadFails, dead)
	_, err := pgRestore(t, mock)
	if err == nil || !IsRecoveryIncomplete(err) || strings.Contains(err.Error(), "database untouched") || !strings.Contains(err.Error(), "no longer answers") {
		t.Fatalf("got %v", err)
	}
	// A live server whose replacement cannot be dropped: named, not "dropped".
	cantDrop := ssh.MockCommand{Match: "docker exec 'myapp-postgres' psql -X -qtA -v ON_ERROR_STOP=1 -U 'postgres' -d 'postgres' -c 'SELECT count(pg_terminate_backend", Err: errors.New("exit status 1: permission denied")}
	_, err = pgRestore(t, pgRestoreMock(loadFails, cantDrop))
	if err == nil || strings.Contains(err.Error(), "was dropped") || !strings.Contains(err.Error(), "could not be dropped") {
		t.Fatalf("got %v", err)
	}
}

func dfMock(availKB string) ssh.MockCommand {
	return ssh.MockCommand{Match: "docker exec 'myapp-postgres' df -Pk", Output: "Filesystem 1024-blocks Used Available Capacity Mounted on\n/dev/vdb1 1000000 10 " + availKB + " 1% /var/lib/postgresql/data\n"}
}

func recordedMock(bytes string) ssh.MockCommand {
	return ssh.MockCommand{Match: "cat '/tmp/teploy-restore.abc123/meta.json'", Output: `{"logical_bytes":` + bytes + `}`}
}

func liveSize(bytes string) ssh.MockCommand {
	return ssh.MockCommand{Match: "docker exec 'myapp-postgres' psql -X -qtA -v ON_ERROR_STOP=1 -U 'postgres' -d 'postgres' -c 'SELECT current_setting", Output: "170004|t|t|appowner|UTF8|C|C|" + bytes + "\n"}
}

// The free-space preflight sizes the BACKUP. 15 MB free, a near-empty live
// database and an 83 MB backup must be refused; a bloated live database with
// a small backup must not (that restore is how a runaway table is recovered).
func TestPostgresRestore_SpaceCheckSizesTheBackup(t *testing.T) {
	const mb = "000000"
	// Recorded size larger than free space, live database tiny: refused.
	mock := pgRestoreMock(dfMock("15000"), recordedMock("83"+mb), liveSize("9000000"))
	if _, err := pgRestore(t, mock); err == nil || !strings.Contains(err.Error(), "not enough free space") || !strings.Contains(err.Error(), "--ignore-space-check") {
		t.Fatalf("want a refusal naming the override, got %v", err)
	}
	if pgSteps(mock.Calls)["create"] >= 0 {
		t.Fatal("refused before creating anything")
	}
	// Bloated live database (500 MB), small recorded backup, 15 MB free: fits.
	mock = pgRestoreMock(dfMock("15000"), recordedMock("1"+mb), liveSize("500"+mb))
	if _, err := pgRestore(t, mock); err != nil {
		t.Fatalf("recovering from a runaway table must not be refused: %v", err)
	}
	// No record: artifact x factor, 30 MB x 4 > 15 MB free.
	mock = pgRestoreMock(dfMock("15000"), ssh.MockCommand{Match: "wc -c <", Output: "30" + mb + "\n"}, liveSize("9000000"))
	if _, err := pgRestore(t, mock); err == nil || !strings.Contains(err.Error(), "not enough free space") {
		t.Fatalf("an estimate from the artifact must refuse, got %v", err)
	}
	// No record: the estimate takes the larger of artifact and live size.
	mock = pgRestoreMock(dfMock("15000"), ssh.MockCommand{Match: "wc -c <", Output: "1000\n"}, liveSize("500"+mb))
	if _, err := pgRestore(t, mock); err == nil || !strings.Contains(err.Error(), "not enough free space") {
		t.Fatalf("an estimate is at least the live size, got %v", err)
	}
	// The explicit override always proceeds.
	mock = pgRestoreMock(dfMock("15000"), ssh.MockCommand{Match: "wc -c <", Output: "1000\n"}, liveSize("500"+mb))
	if _, err := pgClient(mock).AccessoryRestoreWith(context.Background(), "myapp", "postgres", "postgres:16", "20260101-000000", nil, S3Config{Bucket: "my-bucket", Region: "us-east-1"}, RestoreOptions{IgnoreSpaceCheck: true}); err != nil {
		t.Fatalf("--ignore-space-check: %v", err)
	}
}

// A legacy .sql.gz has no database-level entries, so they are read from the
// live database before the swap and applied after it. Otherwise ALTER
// DATABASE SET, REVOKE CONNECT and the comment go with the dropped database.
func TestPostgresRestore_LegacyDumpKeepsLiveDatabaseSettings(t *testing.T) {
	mock := pgRestoreMock(
		ssh.MockCommand{Match: "aws s3 ls", Output: "2026-01-01 00:00:00 99 20260101-000000.sql.gz\n"},
		ssh.MockCommand{Match: "docker exec 'myapp-postgres' pg_dump -Fc --schema-only", Output: ""},
	)
	rec, err := pgRestore(t, mock)
	if err != nil {
		t.Fatal(err)
	}
	capture, swap, apply := -1, -1, -1
	for i, c := range mock.Calls {
		switch {
		case strings.Contains(c, "pg_dump -Fc --schema-only") && strings.Contains(c, "live-dblevel.pgdump"):
			capture = i
		case strings.Contains(c, `RENAME TO "myapp"`):
			swap = i
		case strings.Contains(c, "pg_restore -C -L") && strings.Contains(c, "live-dblevel.pgdump"):
			apply = i
		}
	}
	if !(capture >= 0 && capture < swap && swap < apply) {
		t.Fatalf("want capture < swap < apply, got %d %d %d", capture, swap, apply)
	}
	if !strings.Contains(string(mock.Files["/tmp/teploy-restore.abc123/dblevel.sql"]), "ALTER DATABASE myapp SET search_path") {
		t.Fatal("the live settings must be re-applied")
	}
	if len(rec.Warnings) != 0 {
		t.Fatalf("no warnings expected: %v", rec.Warnings)
	}
	// A failed capture warns; it does not silently lose them.
	mock = pgRestoreMock(
		ssh.MockCommand{Match: "aws s3 ls", Output: "2026-01-01 00:00:00 99 20260101-000000.sql.gz\n"},
		ssh.MockCommand{Match: "docker exec 'myapp-postgres' pg_dump -Fc --schema-only", Err: errors.New("exit status 1: boom")},
	)
	if rec, err = pgRestore(t, mock); err != nil || len(rec.Warnings) != 1 || !strings.Contains(rec.Warnings[0], "database-level settings") {
		t.Fatalf("want a warning, got %v %v", rec.Warnings, err)
	}
}

// The collation provider survives: an ICU database used to come back as libc.
func TestPgLocaleClause(t *testing.T) {
	for _, tc := range []struct {
		l       pgLocale
		version int
		want    string
	}{
		{pgLocale{Provider: "i", Locale: "de-DE"}, 170000, " LOCALE_PROVIDER icu ICU_LOCALE 'de-DE'"},
		{pgLocale{Provider: "i", Locale: "de-DE", Rules: "&a < g"}, 170000, " LOCALE_PROVIDER icu ICU_LOCALE 'de-DE' ICU_RULES '&a < g'"},
		{pgLocale{Provider: "i", Locale: "de-DE", Rules: "&a < g"}, 150000, " LOCALE_PROVIDER icu ICU_LOCALE 'de-DE'"},
		{pgLocale{Provider: "b", Locale: "C.UTF-8"}, 170000, " LOCALE_PROVIDER builtin BUILTIN_LOCALE 'C.UTF-8'"},
		{pgLocale{Provider: "c"}, 170000, " LOCALE_PROVIDER libc"},
		{pgLocale{}, 140000, ""},
	} {
		if got := tc.l.createClause(tc.version); got != tc.want {
			t.Errorf("%+v at %d = %q, want %q", tc.l, tc.version, got, tc.want)
		}
	}
	mock := pgRestoreMock(ssh.MockCommand{Match: "docker exec 'myapp-postgres' psql -X -qtA -v ON_ERROR_STOP=1 -U 'postgres' -d 'postgres' -c 'SELECT row_to_json", Output: `{"provider":"i","locale":"de-DE","rules":null}` + "\n"})
	if _, err := pgRestore(t, mock); err != nil {
		t.Fatal(err)
	}
	if !pgCalled(mock.Calls, "datlocale AS locale") || !pgCalled(mock.Calls, "LOCALE_PROVIDER icu ICU_LOCALE") {
		t.Fatalf("the replacement must carry the provider: %v", mock.Calls)
	}
}

func TestTruncBytesKeepsRuneBoundary(t *testing.T) {
	s := strings.Repeat("é", 30) // 2 bytes each
	got := truncBytes(s, 40)
	if len(got) > 40 || !utf8.ValidString(got) {
		t.Fatalf("truncBytes = %d bytes, valid=%v", len(got), utf8.ValidString(got))
	}
	if got := truncBytes(strings.Repeat("é", 30)+"x", 41); len(got) != 40 || !utf8.ValidString(got) {
		t.Fatalf("mid-rune cut: %d bytes", len(got))
	}
	if truncBytes("short", 40) != "short" {
		t.Fatal("short names unchanged")
	}
}

// ---- coverage ----

func coverageMock(databases string, overrides ...ssh.MockCommand) *ssh.MockExecutor {
	cmds := append(append([]ssh.MockCommand{}, overrides...),
		ssh.MockCommand{Match: "which aws", Output: "/usr/bin/aws\n"},
		ssh.MockCommand{Match: "umask 077", Output: "/tmp/teploy-backup.abc123\n"},
		ssh.MockCommand{Match: "docker exec 'myapp-db' psql -X -qtA -v ON_ERROR_STOP=1 -U 'postgres' -d 'postgres' -c 'SELECT datname", Output: databases},
		ssh.MockCommand{Match: "docker exec 'myapp-db' psql -X -qtA -v ON_ERROR_STOP=1 -U 'postgres' -d 'postgres' -c 'SELECT count(*) FROM pg_class", Output: "0\n"},
		ssh.MockCommand{Match: "docker exec 'myapp-db' psql", Output: "1234\n"},
		ssh.MockCommand{Match: "docker exec 'myapp-db' pg_dump -Fc", Output: ""},
		ssh.MockCommand{Match: "wc -c", Output: "4096\n"},
		ssh.MockCommand{Match: "aws s3 cp", Output: ""},
		ssh.MockCommand{Match: "{ docker exec", Output: ""},
		ssh.MockCommand{Match: "rm -rf", Output: ""},
	)
	return newStoreMock("h", cmds...)
}

func coverageBackup(mock *ssh.MockExecutor) (BackupResult, error) {
	return NewClient(mock, io.Discard).AccessoryBackup(context.Background(), "myapp", "db", "postgres:17", map[string]string{"PG_MAJOR": "17", "POSTGRES_DB": "appdb"}, S3Config{Bucket: "b", Region: "r"})
}

// A cluster with a second database still gets a backup of the named one, and
// says what it left out: on upgrade it must not go from partial to none.
func TestAccessoryBackup_MultiDatabaseWarnsAndBacksUp(t *testing.T) {
	mock := coverageMock("appdb\nother\nappdb_teploy_old_0123abcd\nappdb_teploy_new_89abcdef\npostgres\n")
	res, err := coverageBackup(mock)
	if err != nil {
		t.Fatalf("must back up: %v", err)
	}
	if !pgCalled(mock.Calls, "pg_dump -Fc -U 'postgres' 'appdb'") {
		t.Fatal("the named database must be dumped")
	}
	if len(res.UncoveredDatabases) != 1 || res.UncoveredDatabases[0] != "other" {
		t.Fatalf("uncovered = %v: leftovers and an empty postgres db are not uncovered", res.UncoveredDatabases)
	}
	joined := strings.Join(res.Warnings, " | ")
	for _, want := range []string{"also holds other", "appdb_teploy_old_0123abcd", "appdb_teploy_new_89abcdef", "--release", "--rollback"} {
		if !strings.Contains(joined, want) {
			t.Errorf("warnings lack %q: %s", want, joined)
		}
	}
}

// backup_databases: dumps the listed database beside the primary, under the
// id, without a coverage warning.
func TestAccessoryBackup_BackupDatabasesLabel(t *testing.T) {
	mock := coverageMock("appdb\nother\npostgres\n",
		ssh.MockCommand{Match: "docker inspect -f '{{json .Config.Labels}}", Output: `{"teploy.backup-databases":"other"}` + "\nnull\nnull\n"},
		ssh.MockCommand{Match: "docker exec 'myapp-db' pg_dump -Fc -U 'postgres' 'other'", Output: ""},
	)
	res, err := coverageBackup(mock)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.UncoveredDatabases) != 0 {
		t.Fatalf("a listed database is covered: %v", res.UncoveredDatabases)
	}
	if !pgCalled(mock.Calls, "s3://b/myapp/accessories/db/databases/other/"+res.ID+".pgdump") {
		t.Fatalf("the extra dump must be stored under its own prefix: %v", mock.Calls)
	}
	// "all" takes every database, including a postgres db with tables.
	cov, err := NewClient(coverageMock("appdb\nother\nthird\n"), io.Discard).pgReadCoverage(context.Background(), pgTarget{container: "myapp-db", user: "postgres", db: "appdb"}, []string{"all"})
	if err != nil || len(cov.Extra) != 2 || len(cov.Uncovered) != 0 {
		t.Fatalf("all: %+v %v", cov, err)
	}
}

// A stray table in the "postgres" database is a coverage warning, never a
// missing backup.
func TestPgCoverage_PostgresDatabaseCountsOnlyWithTables(t *testing.T) {
	c := NewClient(coverageMock("appdb\npostgres\n", ssh.MockCommand{Match: "docker exec 'myapp-db' psql -X -qtA -v ON_ERROR_STOP=1 -U 'postgres' -d 'postgres' -c 'SELECT count(*) FROM pg_class", Output: "1\n"}), io.Discard)
	cov, err := c.pgReadCoverage(context.Background(), pgTarget{container: "myapp-db", user: "postgres", db: "appdb"}, nil)
	if err != nil || len(cov.Uncovered) != 1 || cov.Uncovered[0] != "postgres" {
		t.Fatalf("%+v %v", cov, err)
	}
}

// The reviewer's injection: a hostile database name ran on the host when the
// refusal printed it. No plan or coverage path evaluates a name in a shell.
func TestPostgresPlanDoesNotEvaluateDatabaseName(t *testing.T) {
	dir := t.TempDir()
	pwned := dir + "/pwned"
	bin := dir + "/bin"
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin+"/docker", []byte("#!/bin/sh\nprintf PGDMP-fake\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	db := "x$(touch " + pwned + ")"
	plan, err := planAccessoryDump("app", "db", EngineDetection{Engine: EnginePostgres}, map[string]string{"POSTGRES_DB": db}, dir)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", plan.cmd)
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("plan: %v %s", err, out)
	}
	if _, err := os.Stat(pwned); err == nil {
		t.Fatal("the database name was evaluated by the shell")
	}
	// The coverage warning carries names as data, for a Go string only.
	cov := pgCoverage{Uncovered: []string{db}}
	if w := cov.warnings(db); len(w) != 1 || !strings.Contains(w[0], "$(touch") {
		t.Fatalf("warning = %v", w)
	}
}

// ---- recovery copies ----

func copiesMock(databases string, overrides ...ssh.MockCommand) *ssh.MockExecutor {
	cmds := append(append([]ssh.MockCommand{}, overrides...),
		ssh.MockCommand{Match: "docker exec 'myapp-postgres' psql -X -qtA -v ON_ERROR_STOP=1 -U 'postgres' -d 'postgres' -c 'SELECT current_setting", Output: "170004|t|t|postgres|UTF8|C|C|100\n"},
		ssh.MockCommand{Match: "docker exec 'myapp-postgres' psql -X -qtA -v ON_ERROR_STOP=1 -U 'postgres' -d 'postgres' -c 'SELECT datname FROM pg_database WHERE NOT", Output: databases},
		ssh.MockCommand{Match: "docker exec 'myapp-postgres' psql", Output: ""},
		ssh.MockCommand{Match: "for d in '/var/tmp/teploy-pre-restore.myapp@postgres@'*", Output: "done /var/tmp/teploy-pre-restore.myapp@postgres@abc\n"},
		ssh.MockCommand{Match: "rm -rf", Output: ""},
	)
	return newStoreMock("h", cmds...)
}

func TestReleaseRecoveryCopies(t *testing.T) {
	ctx := context.Background()
	mock := copiesMock("myapp\nreports\nmyapp_teploy_old_0123abcd\nmyapp_teploy_new_89abcdef\nreports_teploy_old_00000000\n")
	c := NewClient(mock, io.Discard)
	rc, err := c.FindRecoveryCopies(ctx, "myapp", "postgres", "postgres:17", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rc.Previous) != 2 || len(rc.Partial) != 1 || len(rc.Paths) != 1 || len(rc.MissingLive) != 0 {
		t.Fatalf("copies = %+v (a --database restore's copy is this accessory's too)", rc)
	}
	released, err := c.ReleaseRecoveryCopies(ctx, rc, false)
	if err != nil || len(released) != 4 {
		t.Fatalf("released %v, %v", released, err)
	}
	if !pgCalled(mock.Calls, `DROP DATABASE IF EXISTS "reports_teploy_old_00000000" WITH (FORCE)`) || pgCalled(mock.Calls, `DROP DATABASE IF EXISTS "myapp" `) {
		t.Fatalf("release must drop the copies and nothing else: %v", mock.Calls)
	}
	// --database scopes to one database's copies.
	rc, err = NewClient(copiesMock("myapp\nreports\nmyapp_teploy_old_0123abcd\nreports_teploy_old_00000000\n"), io.Discard).FindRecoveryCopies(ctx, "myapp", "postgres", "postgres:17", "reports", nil)
	if err != nil || len(rc.Previous) != 1 || rc.Previous[0] != "reports_teploy_old_00000000" {
		t.Fatalf("scoped copies = %+v, %v", rc, err)
	}
}

// The reproduced data loss: appdb gone, only appdb_teploy_old_* left, and
// --release --yes dropped it. Refused, with or without --force.
func TestReleaseRefusedWhileTheLiveDatabaseIsMissing(t *testing.T) {
	mock := copiesMock("myapp_teploy_old_0123abcd\n")
	c := NewClient(mock, io.Discard)
	rc, err := c.FindRecoveryCopies(context.Background(), "myapp", "postgres", "postgres:17", "", nil)
	if err != nil || len(rc.MissingLive) != 1 {
		t.Fatalf("copies = %+v, %v", rc, err)
	}
	for _, force := range []bool{false, true} {
		if _, err := c.ReleaseRecoveryCopies(context.Background(), rc, force); err == nil || !strings.Contains(err.Error(), "--rollback") {
			t.Fatalf("force=%v: want a refusal pointing at --rollback, got %v", force, err)
		}
	}
	if pgCalled(mock.Calls, "DROP DATABASE") {
		t.Fatalf("nothing may be dropped: %v", mock.Calls)
	}
}

// The other reproduced loss: mysql killed after DROP, 0 live tables, the only
// copy in /var/tmp. Its restore never wrote the marker: refused without --force.
func TestReleaseRefusesUnconfirmedSafetyCopyWithoutForce(t *testing.T) {
	path := "/var/tmp/teploy-pre-restore.myapp@db@abc"
	mock := newStoreMock("h",
		ssh.MockCommand{Match: "docker exec 'myapp-db' sh -c", Output: ""},
		ssh.MockCommand{Match: "for d in '/var/tmp/teploy-pre-restore.myapp@db@'*", Output: "open " + path + "\n"},
		ssh.MockCommand{Match: "rm -rf", Output: ""},
	)
	c := NewClient(mock, io.Discard)
	rc, err := c.FindRecoveryCopies(context.Background(), "myapp", "db", "mariadb:11", "", map[string]string{"MARIADB_VERSION": "11"})
	if err != nil || len(rc.Unconfirmed) != 1 {
		t.Fatalf("copies = %+v, %v", rc, err)
	}
	if _, err := c.ReleaseRecoveryCopies(context.Background(), rc, false); err == nil || !strings.Contains(err.Error(), "only copy") || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("got %v", err)
	}
	if pgCalled(mock.Calls, "rm -rf") {
		t.Fatal("nothing may be removed")
	}
	if released, err := c.ReleaseRecoveryCopies(context.Background(), rc, true); err != nil || len(released) != 1 {
		t.Fatalf("--force: %v %v", released, err)
	}
}

// Prefixes cannot collide: app a + accessory b-c used to share a prefix with
// app a-b + accessory c, and --release on one deleted the other's copies.
func TestRecoveryPrefixIsUnambiguous(t *testing.T) {
	one, two := recoveryPrefix("a", "b-c"), recoveryPrefix("a-b", "c")
	if strings.HasPrefix(one, two) || strings.HasPrefix(two, one) {
		t.Fatalf("%s and %s collide", one, two)
	}
	if strings.HasPrefix(recoveryPrefix("a", "b-c"), recoveryPrefix("a", "b")) {
		t.Fatal("accessory b must not match accessory b-c")
	}
}

// exec wrapper for rollback: the swap and the pg_database re-read carry a
// random displaced name.
type pgRollbackExec struct {
	*ssh.MockExecutor
	present map[string]bool
	swapErr error
}

func (e *pgRollbackExec) Run(ctx context.Context, cmd string) (string, error) {
	switch {
	case strings.Contains(cmd, "RENAME TO"):
		e.MockExecutor.Calls = append(e.MockExecutor.Calls, cmd)
		return "", e.swapErr
	case strings.Contains(cmd, "SELECT datname FROM pg_database WHERE datname IN"):
		var out []string
		for name := range e.present {
			if strings.Contains(cmd, name) {
				out = append(out, name)
			}
		}
		return strings.Join(out, "\n"), nil
	}
	return e.MockExecutor.Run(ctx, cmd)
}

func TestRollbackRestore(t *testing.T) {
	ctx := context.Background()
	t2 := pgTarget{container: "myapp-postgres", user: "postgres", db: "myapp"}
	old := "myapp_teploy_old_0123abcd"
	rc := RecoveryCopies{engine: EnginePostgres, Previous: []string{old}, pg: t2, version: 170004}
	plan, err := rc.PlanRollback("", "")
	if err != nil || plan.Previous != old || plan.Database != "myapp" {
		t.Fatalf("plan = %+v, %v", plan, err)
	}

	e := &pgRollbackExec{MockExecutor: newStoreMock("h"), present: map[string]bool{"myapp": true, old: true}}
	displaced, err := NewClient(e, io.Discard).RollbackRestore(ctx, plan)
	if err != nil || !strings.HasPrefix(displaced, "myapp_teploy_new_") {
		t.Fatalf("rollback = %s %v", displaced, err)
	}
	if !pgCalled(e.Calls, `ALTER DATABASE "myapp" RENAME TO "`+displaced+`"; ALTER DATABASE "`+old+`" RENAME TO "myapp"`) {
		t.Fatalf("the swap back must be one statement: %v", e.Calls)
	}

	// After a recovery-incomplete restore the live name may be gone.
	e = &pgRollbackExec{MockExecutor: newStoreMock("h"), present: map[string]bool{old: true}}
	displaced, err = NewClient(e, io.Discard).RollbackRestore(ctx, plan)
	if err != nil || displaced != "" || pgCalled(e.Calls, `ALTER DATABASE "myapp" RENAME TO`) {
		t.Fatalf("rollback with no live database = %q %v: %v", displaced, err, e.Calls)
	}

	for name, bad := range map[string]RecoveryCopies{
		"none":      {engine: EnginePostgres, pg: t2},
		"several":   {engine: EnginePostgres, Previous: []string{old, "myapp_teploy_old_89abcdef"}, pg: t2},
		"other db":  {engine: EnginePostgres, Previous: []string{"reports_teploy_old_00000000"}, pg: t2},
		"not an pg": {engine: EngineMySQL, Paths: []string{"/var/tmp/x"}},
	} {
		if _, err := bad.PlanRollback("", ""); err == nil {
			t.Errorf("%s: want a refusal", name)
		}
	}
	if _, err := rc.PlanRollback("", "myapp"); err == nil {
		t.Error("pick must name a kept previous database")
	}
	// A --database restore's copy rolls back into that database.
	other := RecoveryCopies{engine: EnginePostgres, Previous: []string{"reports_teploy_old_00000000"}, pg: t2}
	if p, err := other.PlanRollback("reports", ""); err != nil || p.Database != "reports" {
		t.Fatalf("--database rollback = %+v, %v", p, err)
	}
}

// A name with a space must not read as an unknown state after a committed swap.
func TestPgSwapStateHandlesSpacesInNames(t *testing.T) {
	mock := newStoreMock("h", ssh.MockCommand{Match: "docker exec 'c' psql", Output: "my db\nmy db_old\n"})
	st, err := NewClient(mock, io.Discard).pgSwapState(context.Background(), pgTarget{container: "c", user: "u", db: "my db"}, "my db_new", "my db_old")
	if err != nil || !st.live || !st.previous || st.replacement {
		t.Fatalf("state = %+v, %v", st, err)
	}
}

// Disk: WAL counts. 593 MB recorded and 695 MB free passed a 1.1x check, and
// the load PANICed writing WAL.
func TestPostgresRestore_SpaceCheckIncludesWAL(t *testing.T) {
	const mb = "000000"
	wal := func(mbs string) ssh.MockCommand {
		return ssh.MockCommand{Match: "docker exec 'myapp-postgres' psql -X -qtA -v ON_ERROR_STOP=1 -U 'postgres' -d 'postgres' -c 'SELECT setting FROM pg_settings", Output: mbs + "\n"}
	}
	mock := pgRestoreMock(dfMock("695000"), recordedMock("593"+mb), wal("1024"))
	if _, err := pgRestore(t, mock); err == nil || !strings.Contains(err.Error(), "not enough free space") {
		t.Fatalf("593 MB + WAL must not fit in 695 MB: %v", err)
	}
	// A small max_wal_size is what the server recycles at.
	mock = pgRestoreMock(dfMock("695000"), recordedMock("593"+mb), wal("32"))
	if _, err := pgRestore(t, mock); err != nil {
		t.Fatalf("593 MB x 1.1 + 32 MB fits in 695 MB: %v", err)
	}
}

func mysqlQuery(t *testing.T, sql string) string {
	t.Helper()
	m := mysqlTarget{engine: EngineMySQL, container: "myapp-db", credFile: "/tmp/x"}
	cmd := m.client("mysql", "-N -B -e "+ssh.ShellQuote(sql))
	return strings.TrimSuffix(cmd, " < '/tmp/x'")
}

// A dry run does not write the binlog (a full copy, sent to every replica).
func TestMySQLRestore_DryRunSkipsTheBinlog(t *testing.T) {
	mock := mysqlRestoreMock(
		ssh.MockCommand{Match: mysqlQuery(t, "SELECT @@log_bin"), Output: "1\n"},
	)
	if _, err := mysqlRestore(mock); err != nil {
		t.Fatal(err)
	}
	if !pgCalled(mock.Calls, "SET sql_log_bin=0; CREATE DATABASE `appdb_teploy_check`") || !pgCalled(mock.Calls, "SET sql_log_bin=0; DROP DATABASE IF EXISTS `appdb_teploy_check`") {
		t.Fatalf("DDL must skip the binlog: %v", mock.Calls)
	}
	var load string
	for _, c := range mock.Calls {
		if strings.Contains(c, "check.failed'; {") {
			load = c
		}
	}
	if !strings.Contains(load, "SET sql_log_bin=0;") || strings.Contains(load, "s3cret") {
		t.Fatalf("the load session must skip the binlog: %s", load)
	}
	for _, c := range mock.Calls {
		if strings.Contains(c, "load.failed'; {") && strings.Contains(c, "sql_log_bin") {
			t.Fatalf("the REAL load must reach the binlog and replicas: %s", c)
		}
	}
}

// Without the privilege to skip it, the binlog's copy counts toward the room
// needed, and the dry run still runs with what the server allows.
func TestMySQLRestore_SpaceCheckCountsBinlog(t *testing.T) {
	binlogOn := ssh.MockCommand{Match: mysqlQuery(t, "SELECT @@log_bin"), Output: "1\n"}
	noPriv := ssh.MockCommand{Match: strings.TrimSuffix(mysqlQuery(t, "SET sql_log_bin=0"), ""), Err: errors.New("exit status 1: ERROR 1227: Access denied")}
	noPriv.Match = strings.ReplaceAll(noPriv.Match, "-N -B ", "")
	live := ssh.MockCommand{Match: "docker exec -i 'myapp-db' sh -c 'IFS= read -r teploy_credential || exit 1; MYSQL_PWD=${teploy_credential#MYSQL_PWD=}; export MYSQL_PWD; exec mysql -u root -N -B -e '\"'\"'SELECT CONCAT_WS", Output: "utf8mb4|c|60000000|0|0\n"}
	dump := ssh.MockCommand{Match: "wc -c < '/tmp/teploy-restore.abc123/restore.sql'", Output: "52000000\n"}
	df := ssh.MockCommand{Match: "docker exec 'myapp-db' df -Pk", Output: "Filesystem 1024-blocks Used Available Capacity Mounted on\n/dev/x 1000000 10 75000 1% /var/lib/mysql\n"}
	// 52 MB dump over a 60 MB live database, 75 MB free: fits only if the dry
	// run skips the binlog (the real load's binlog is paid for by the DROP).
	if _, err := mysqlRestore(mysqlRestoreMock(binlogOn, live, dump, df)); err != nil {
		t.Fatalf("with the binlog skipped it fits: %v", err)
	}
	_, err := mysqlRestore(mysqlRestoreMock(binlogOn, noPriv, live, dump, df))
	if err == nil || !strings.Contains(err.Error(), "not enough free space") {
		t.Fatalf("without the privilege the binlog must count: %v", err)
	}
}

// exec that hangs like a mysqld "waiting for someone to free space".
type hangExec struct {
	*ssh.MockExecutor
	hangOn []string
}

func (e *hangExec) Run(ctx context.Context, cmd string) (string, error) {
	for _, h := range e.hangOn {
		if strings.Contains(cmd, h) {
			e.MockExecutor.Calls = append(e.MockExecutor.Calls, cmd)
			<-ctx.Done()
			return "", ctx.Err()
		}
	}
	return e.MockExecutor.Run(ctx, cmd)
}

// A wedged server cannot hold the restore (and the stopped app) forever.
func TestMySQLRestore_DryRunAndCleanupHaveDeadlines(t *testing.T) {
	e := &hangExec{MockExecutor: mysqlRestoreMock(), hangOn: []string{"check.failed", "DROP DATABASE IF EXISTS `appdb_teploy_check`"}}
	c := NewClient(e, io.Discard)
	c.loadDeadline = 50 * time.Millisecond
	done := make(chan error, 1)
	go func() {
		_, err := c.AccessoryRestoreKeep(context.Background(), "myapp", "db", "mysql:8", "20260101-000000",
			map[string]string{"MYSQL_VERSION": "8.4", "MYSQL_ROOT_PASSWORD": "s3cret", "MYSQL_DATABASE": "appdb"}, S3Config{Bucket: "b", Region: "us-east-1"})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "could not be dropped") || !strings.Contains(err.Error(), "database untouched") {
			t.Fatalf("got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the restore hung on a wedged server")
	}
	if pgCalled(e.Calls, "DROP DATABASE IF EXISTS `appdb`;") {
		t.Fatal("the live database must not be dropped")
	}
}

// A real database that happens to be called <db>_teploy_check is not ours.
func TestMySQLRestore_RefusesARealCheckNamedDatabase(t *testing.T) {
	mock := mysqlRestoreMock(ssh.MockCommand{Match: "docker exec -i 'myapp-db' sh -c 'IFS= read -r teploy_credential || exit 1; MYSQL_PWD=${teploy_credential#MYSQL_PWD=}; export MYSQL_PWD; exec mysql -u root -N -B -e '\"'\"'SELECT CONCAT_WS", Output: "utf8mb4|c|16384|1|0\n"})
	_, err := mysqlRestore(mock)
	if err == nil || !strings.Contains(err.Error(), "is not a teploy dry run") {
		t.Fatalf("got %v", err)
	}
	if pgCalled(mock.Calls, "DROP DATABASE") {
		t.Fatalf("nothing may be dropped: %v", mock.Calls)
	}
	// Ours (a killed restore's): cleared and the restore proceeds.
	mock = mysqlRestoreMock(ssh.MockCommand{Match: "docker exec -i 'myapp-db' sh -c 'IFS= read -r teploy_credential || exit 1; MYSQL_PWD=${teploy_credential#MYSQL_PWD=}; export MYSQL_PWD; exec mysql -u root -N -B -e '\"'\"'SELECT CONCAT_WS", Output: "utf8mb4|c|16384|1|1\n"})
	if _, err := mysqlRestore(mock); err != nil {
		t.Fatal(err)
	}
	if !pgCalled(mock.Calls, "CREATE TABLE `appdb_teploy_check`."+mysqlDryRunMarker) {
		t.Fatal("the dry run must carry the marker")
	}
}

// A successful restore's copy is marked; a release can tell.
func TestMySQLRestore_MarksTheSafetyCopyCompleted(t *testing.T) {
	mock := mysqlRestoreMock(ssh.MockCommand{Match: ": > '/var/tmp/teploy-pre-restore.m/restore-completed'", Output: ""})
	rec, err := mysqlRestore(mock)
	if err != nil || len(rec.Warnings) != 0 {
		t.Fatalf("%v %v", err, rec.Warnings)
	}
	if !pgCalled(mock.Calls, ": > '/var/tmp/teploy-pre-restore.m/restore-completed'") {
		t.Fatal("marker not written")
	}
	// A load killed mid-way never writes it.
	mock = mysqlRestoreMock(ssh.MockCommand{Match: "rm -f '/tmp/teploy-restore.abc123/load.failed'", Err: errors.New("exit status 1")},
		ssh.MockCommand{Match: "rm -f '/tmp/teploy-restore.abc123/reload.failed'", Err: errors.New("exit status 1")})
	if _, err := mysqlRestore(mock); !IsRecoveryIncomplete(err) {
		t.Fatalf("got %v", err)
	}
	if pgCalled(mock.Calls, "restore-completed") {
		t.Fatal("a failed restore must not mark its copy completed")
	}
}

// A second schema: uncovered, same as postgres. The dry-run leftover is not.
func TestMySQLBackup_SecondSchemaWarns(t *testing.T) {
	q := mysqlEnvClient(EngineMariaDB, "myapp-db", "X", "-N", "-B")
	prefix := q[:strings.Index(q, "-e ")]
	mock := newStoreMock("h",
		ssh.MockCommand{Match: prefix, Output: "appdb\t0\nreports\t0\nappdb_teploy_check\t1\n"},
		ssh.MockCommand{Match: "which aws", Output: "/usr/bin/aws\n"},
		ssh.MockCommand{Match: "umask 077", Output: "/tmp/teploy-backup.abc123\n"},
		ssh.MockCommand{Match: "docker exec", Output: ""},
		ssh.MockCommand{Match: "wc -c", Output: "4096\n"},
		ssh.MockCommand{Match: "aws s3 cp", Output: ""},
		ssh.MockCommand{Match: "rm -rf", Output: ""},
	)
	res, err := NewClient(mock, io.Discard).AccessoryBackup(context.Background(), "myapp", "db", "mariadb:11", map[string]string{"MARIADB_VERSION": "11", "MARIADB_DATABASE": "appdb"}, S3Config{Bucket: "b", Region: "r"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.UncoveredDatabases) != 1 || res.UncoveredDatabases[0] != "reports" {
		t.Fatalf("uncovered = %v", res.UncoveredDatabases)
	}
	if j := strings.Join(res.Warnings, " | "); !strings.Contains(j, "also holds reports") || !strings.Contains(j, "appdb_teploy_check") {
		t.Fatalf("warnings = %s", j)
	}
}

// Mongo: preflight on the data volume, honoring the override, and a dry run
// that kills mongod is recovery-incomplete, not "untouched".
func TestMongoRestore_SpaceAndLiveness(t *testing.T) {
	base := func(o ...ssh.MockCommand) *ssh.MockExecutor {
		return newStoreMock("h", append(o,
			ssh.MockCommand{Match: "which aws", Output: "/usr/bin/aws\n"},
			ssh.MockCommand{Match: "aws s3 ls", Output: "2026-01-01 00:00:00 2048 20260101-000000.archive.gz\n"},
			ssh.MockCommand{Match: "aws s3 cp", Output: ""},
			ssh.MockCommand{Match: "mktemp -d '/var/tmp/teploy-restore.XXXXXX'", Output: "/tmp/teploy-restore.abc123\n"},
			ssh.MockCommand{Match: "gzip -t", Output: ""},
			ssh.MockCommand{Match: "wc -c", Output: "62000000\n"},
			ssh.MockCommand{Match: "docker exec 'myapp-db' df -Pk", Output: "Filesystem 1024-blocks Used Available Capacity Mounted on\n/dev/x 1000000 10 25000 1% /data/db\n"},
			ssh.MockCommand{Match: "umask 077; mktemp -d '/var/tmp/teploy-pre-restore.myapp@db@XXXXXX'", Output: "/var/tmp/teploy-pre-restore.g\n"},
			ssh.MockCommand{Match: "docker exec 'myapp-db' mongodump", Output: ""},
			ssh.MockCommand{Match: "docker exec 'myapp-db' sh -c", Output: ""},
			ssh.MockCommand{Match: "docker exec -i 'myapp-db' mongorestore", Output: ""},
			ssh.MockCommand{Match: ": > ", Output: ""},
			ssh.MockCommand{Match: "rm -", Output: ""},
		)...)
	}
	run := func(m *ssh.MockExecutor, opts RestoreOptions) (RestoreRecovery, error) {
		c := NewClient(m, io.Discard)
		c.retryDelay = time.Millisecond
		return c.AccessoryRestoreWith(context.Background(), "myapp", "db", "mongo:7", "20260101-000000", map[string]string{"MONGO_VERSION": "7.0"}, S3Config{Bucket: "b", Region: "us-east-1"}, opts)
	}
	mock := base()
	if _, err := run(mock, RestoreOptions{}); err == nil || !strings.Contains(err.Error(), "not enough free space") {
		t.Fatalf("62 MB archive into 25 MB free must be refused: %v", err)
	}
	if pgCalled(mock.Calls, "mongorestore") {
		t.Fatal("refused before loading")
	}
	if _, err := run(base(), RestoreOptions{IgnoreSpaceCheck: true}); err != nil {
		t.Fatalf("--ignore-space-check: %v", err)
	}
	dead := base(
		ssh.MockCommand{Match: "docker exec -i 'myapp-db' mongorestore --archive --gzip --drop --nsExclude='admin.*'", Err: errors.New("exit status 1: connection lost")},
		ssh.MockCommand{Match: "docker exec 'myapp-db' sh -c 'mongosh --quiet --eval '\"'\"'db.runCommand", Err: errors.New("exit status 1: connection refused")},
	)
	_, err := run(dead, RestoreOptions{IgnoreSpaceCheck: true})
	if err == nil || !IsRecoveryIncomplete(err) || strings.Contains(err.Error(), "database untouched") {
		t.Fatalf("got %v", err)
	}
}

// One throwaway database with a short random name fits any database name in
// 63 bytes; leftovers are excluded from every real restore and dropped.
func TestMongoRestore_ThrowawayNameAndExclusions(t *testing.T) {
	mock := newStoreMock("h",
		ssh.MockCommand{Match: "gzip -t", Output: ""},
		ssh.MockCommand{Match: "wc -c", Output: "100\n"},
		ssh.MockCommand{Match: "docker exec 'c' df -Pk", Err: errors.New("exit status 1")},
		ssh.MockCommand{Match: "df -Pk", Err: errors.New("exit status 1")},
		ssh.MockCommand{Match: "docker exec 'c' sh -c", Output: ""},
		ssh.MockCommand{Match: "docker exec -i 'c' mongorestore", Output: ""},
		ssh.MockCommand{Match: "umask 077; mktemp -d", Output: "/var/tmp/p\n"},
		ssh.MockCommand{Match: "docker exec 'c' mongodump", Output: ""},
		ssh.MockCommand{Match: ": > ", Output: ""},
	)
	if _, err := NewClient(mock, io.Discard).restoreMongoInPlace(context.Background(), "c", "/tmp/a.gz", "/var/tmp/teploy-pre-restore.a@b@", false); err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`--nsTo='(teploy_check_[0-9a-f]{8})\.\$db\$\.\$coll\$'`)
	var dry, real string
	for _, c := range mock.Calls {
		if strings.Contains(c, "--nsTo=") {
			dry = c
		} else if strings.Contains(c, "mongorestore") {
			real = c
		}
	}
	if !re.MatchString(dry) {
		t.Fatalf("throwaway name: %s", dry)
	}
	if !strings.Contains(real, "--nsExclude='teploy_check_*.*'") {
		t.Fatalf("a real restore must exclude throwaways: %s", real)
	}
}

// A backup clears a killed restore's throwaway first: mongodump takes the
// whole server, so it would land in this backup silently.
func TestMongoBackup_ClearsThrowawaysFirst(t *testing.T) {
	var order []string
	mock := newStoreMock("h",
		ssh.MockCommand{Match: "which aws", Output: "/usr/bin/aws\n"},
		ssh.MockCommand{Match: "umask 077", Output: "/tmp/teploy-backup.abc123\n"},
		ssh.MockCommand{Match: "docker exec 'myapp-db' sh -c", Output: "teploy_check_0123abcd\n"},
		ssh.MockCommand{Match: "docker exec 'myapp-db' mongodump", Output: ""},
		ssh.MockCommand{Match: "wc -c", Output: "4096\n"},
		ssh.MockCommand{Match: "aws s3 cp", Output: ""},
		ssh.MockCommand{Match: "rm -rf", Output: ""},
	)
	res, err := NewClient(mock, io.Discard).AccessoryBackup(context.Background(), "myapp", "db", "mongo:7", map[string]string{"MONGO_VERSION": "7"}, S3Config{Bucket: "b", Region: "r"})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range mock.Calls {
		if strings.Contains(c, "dropDatabase") {
			order = append(order, "drop")
		}
		if strings.Contains(c, "mongodump") {
			order = append(order, "dump")
		}
	}
	if strings.Join(order, ",") != "drop,dump" {
		t.Fatalf("order = %v", order)
	}
	if !strings.Contains(strings.Join(res.Warnings, " "), "teploy_check_0123abcd") {
		t.Fatalf("warnings = %v", res.Warnings)
	}
}

// A missing --database dump is found before anything stops.
func TestResolveRestoreArtifact_ExtraDatabaseMustExist(t *testing.T) {
	mock := newStoreMock("h", ssh.MockCommand{Match: "aws s3 ls", Err: errors.New("exit status 1")})
	_, err := NewClient(mock, io.Discard).ResolveRestoreArtifact(context.Background(), "myapp", "db", "postgres:17", "20260101-000000", map[string]string{"PG_MAJOR": "17", "POSTGRES_DB": "appdb"}, S3Config{Bucket: "b", Region: "r"}, RestoreOptions{Database: "reports"})
	if err == nil || !strings.Contains(err.Error(), "no backup") {
		t.Fatalf("got %v", err)
	}
}

// Reviewer's MariaDB blocker: a dump the server refuses to load (error 1118)
// must be refused before DROP, by loading it into a throwaway database first.
func TestMySQLRestore_DryRunRefusesBeforeDrop(t *testing.T) {
	mock := mysqlRestoreMock(ssh.MockCommand{Match: "rm -f '/tmp/teploy-restore.abc123/check.failed'", Err: errors.New("exit status 1: ERROR 1118 (42000) at line 9: Row size too large")})
	rec, err := mysqlRestore(mock)
	if err == nil || !strings.Contains(err.Error(), "database untouched") || !strings.Contains(err.Error(), "1118") || IsRecoveryIncomplete(err) || !rec.IsZero() {
		t.Fatalf("want an untouched refusal, got %v (%+v)", err, rec)
	}
	for _, c := range mock.Calls {
		if strings.Contains(c, "DROP DATABASE IF EXISTS `appdb`;") {
			t.Fatalf("the live database was dropped: %s", c)
		}
	}
	if !pgCalled(mock.Calls, "CREATE DATABASE `appdb_teploy_check`") || !pgCalled(mock.Calls, "-e 'DROP DATABASE IF EXISTS `appdb_teploy_check`'") && !pgCalled(mock.Calls, "DROP DATABASE IF EXISTS `appdb_teploy_check`'") {
		t.Fatalf("the dry run uses a throwaway database and drops it: %v", mock.Calls)
	}
}

// Mongo: the archive is loaded into throwaway namespaces first.
func TestMongoRestore_DryRunRefusesBeforeTouching(t *testing.T) {
	mock := newStoreMock("h",
		ssh.MockCommand{Match: "docker exec -i 'myapp-db' mongorestore --archive --gzip --drop --nsExclude", Err: errors.New("exit status 1: Failed: corruption")},
		ssh.MockCommand{Match: "which aws", Output: "/usr/bin/aws\n"},
		ssh.MockCommand{Match: "aws s3 ls", Output: "2026-01-01 00:00:00 2048 20260101-000000.archive.gz\n"},
		ssh.MockCommand{Match: "aws s3 cp", Output: ""},
		ssh.MockCommand{Match: "mktemp -d '/var/tmp/teploy-restore.XXXXXX'", Output: "/tmp/teploy-restore.abc123\n"},
		ssh.MockCommand{Match: "gzip -t", Output: ""},
		ssh.MockCommand{Match: "docker exec 'myapp-db' sh -c", Output: ""},
		ssh.MockCommand{Match: "rm -", Output: ""},
	)
	_, err := NewClient(mock, io.Discard).AccessoryRestoreKeep(context.Background(), "myapp", "db", "mongo:7", "20260101-000000", map[string]string{"MONGO_VERSION": "7.0"}, S3Config{Bucket: "b", Region: "us-east-1"})
	if err == nil || !strings.Contains(err.Error(), "database untouched") || IsRecoveryIncomplete(err) {
		t.Fatalf("got %v", err)
	}
	if pgCalled(mock.Calls, "mongodump") || pgCalled(mock.Calls, "--drop --nsExclude='teploy_check_*.*' < ") {
		t.Fatalf("nothing may run after a failed dry run: %v", mock.Calls)
	}
	if !pgCalled(mock.Calls, "teploy_check_") {
		t.Fatal("the dry run's databases must be dropped")
	}
}
