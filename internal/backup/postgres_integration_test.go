//go:build integration

// Real-postgres regression scenarios from the slice-2 review (same colima
// fixture as engines_integration_test.go; postgres:17-alpine and
// pgvector/pgvector:pg17). One container at a time, removed on exit.
//
//	go test -tags integration -p 2 -run TestPGIntegration -v ./internal/backup
package backup

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy/internal/ssh"
)

const pgITImage = "postgres:17-alpine"

// dropKeptTmp removes the scratch dir a failed restore keeps for
// inspection ("restore files kept in <dir>"), once a test has asserted
// the failure.
func (f itFixture) dropKeptTmp(err error) {
	if err == nil {
		return
	}
	if _, after, ok := strings.Cut(err.Error(), "restore files kept in "); ok {
		dir := strings.Fields(after)[0]
		if strings.HasPrefix(dir, "/tmp/teploy-restore.") {
			f.base.Run(context.Background(), "rm -rf "+ssh.ShellQuote(dir))
		}
	}
}

func pgITReady(user string) string { return "pg_isready -h 127.0.0.1 -U " + user }

func pgQ(user, db, sql string) string {
	return "psql -X -qtA -v ON_ERROR_STOP=1 -U " + user + " -d " + db + " -c " + ssh.ShellQuote(sql)
}

// 250 tables with max_locks_per_transaction=10: pg_dump succeeds, and a
// single-transaction restore ran out of lock-table memory, emptying the
// database. The swap restore loads per object and succeeds.
func TestPGIntegration_ManyObjectsLowLockLimit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	f := newITFixture(t, ctx)
	app := "teployitlocks"
	dexec := f.container(t, ctx, app+"-db", "-e POSTGRES_USER=app -e POSTGRES_PASSWORD=pw -e POSTGRES_DB=appdb "+pgITImage+" -c max_locks_per_transaction=10 -c max_connections=20", pgITReady("app"))
	time.Sleep(3 * time.Second)
	f.waitFor(t, ctx, app+"-db", pgITReady("app"))
	dexec(`for i in $(seq 1 250); do echo "CREATE TABLE t$i(id int primary key, v text); INSERT INTO t$i VALUES (1, 'orig');"; done | psql -X -q -v ON_ERROR_STOP=1 -U app -d appdb`)

	client := NewClient(f.exec, os.Stdout)
	image, env, err := client.InspectAccessory(ctx, app, "db")
	if err != nil {
		t.Fatal(err)
	}
	res, err := client.AccessoryBackup(ctx, app, "db", image, env, f.s3)
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	dexec(pgQ("app", "appdb", "UPDATE t1 SET v='changed'; DROP TABLE t250; CREATE TABLE later(x int)"))
	if err := client.AccessoryRestore(ctx, app, "db", image, res.ID, env, f.s3); err != nil {
		t.Fatalf("in-place restore of 250 tables under max_locks_per_transaction=10: %v", err)
	}
	got := strings.TrimSpace(dexec(pgQ("app", "appdb", "SELECT (SELECT count(*) FROM information_schema.tables WHERE table_schema='public') || ':' || (SELECT v FROM t1) || ':' || (SELECT count(*) FROM t250)")))
	if got != "250:orig:1" {
		t.Fatalf("after restore = %q, want 250:orig:1 (later dropped, t250 back)", got)
	}
	if left := strings.TrimSpace(dexec(pgQ("app", "postgres", "SELECT count(*) FROM pg_database WHERE datname LIKE '%teploy_%'"))); left != "0" {
		t.Fatalf("%s replacement/previous databases left behind", left)
	}

}

// A Bitnami-style non-superuser owner (POSTGRESQL_USERNAME) and an
// extension only a superuser may create (pg_stat_statements). The restore
// must fail with the live database untouched (it used to be dropped and
// left empty), and verify-backup must fail the same backup (it used to
// pass it by restoring as superuser with --no-owner --no-acl).
func TestPGIntegration_NonSuperuserOwnerWithSuperuserExtension(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	f := newITFixture(t, ctx)
	app := "teployitnosu"
	name := app + "-db"
	dexec := f.container(t, ctx, name, "-e POSTGRES_PASSWORD=pw -e POSTGRESQL_USERNAME=appuser -e POSTGRESQL_DATABASE=appdb "+pgITImage, pgITReady("postgres"))
	time.Sleep(3 * time.Second)
	f.waitFor(t, ctx, name, pgITReady("postgres"))
	dexec(pgQ("postgres", "postgres", "CREATE ROLE appuser LOGIN CREATEDB") + " && " + pgQ("postgres", "postgres", "CREATE DATABASE appdb OWNER appuser"))
	dexec(pgQ("postgres", "appdb", "CREATE EXTENSION pg_stat_statements"))
	dexec(pgQ("appuser", "appdb", "CREATE TABLE items(id int); INSERT INTO items VALUES (1),(2),(3)"))

	client := NewClient(f.exec, os.Stdout)
	image, env, err := client.InspectAccessory(ctx, app, "db")
	if err != nil {
		t.Fatal(err)
	}
	res, err := client.AccessoryBackup(ctx, app, "db", image, env, f.s3)
	if err != nil {
		t.Fatalf("backup as the owner: %v", err)
	}

	f.run(t, ctx, "docker stop "+name+" >/dev/null")
	vr, err := client.VerifyBackup(ctx, app, "db", res.ID, f.s3)
	if err != nil {
		t.Fatal(err)
	}
	if vr.OK {
		t.Fatalf("verify passed a backup the accessory's own role cannot restore (metric %s)", vr.Metric)
	}
	t.Logf("verify correctly failed: %s", vr.Detail)
	f.run(t, ctx, "docker start "+name+" >/dev/null")
	f.waitFor(t, ctx, name, pgITReady("postgres"))

	dexec(pgQ("appuser", "appdb", "INSERT INTO items VALUES (4)"))
	_, rerr := client.AccessoryRestoreKeep(ctx, app, "db", image, res.ID, env, f.s3)
	if rerr == nil {
		t.Fatal("restore as a non-superuser of a superuser-only extension succeeded?")
	}
	if IsRecoveryIncomplete(rerr) || !strings.Contains(rerr.Error(), "database untouched") {
		t.Fatalf("want an untouched failure, got %v", rerr)
	}
	t.Logf("restore correctly refused: %v", rerr)
	f.dropKeptTmp(rerr)
	if got := strings.TrimSpace(dexec(pgQ("postgres", "appdb", "SELECT count(*) FROM items"))); got != "4" {
		t.Fatalf("the live database changed: %s rows, want the untouched 4", got)
	}
}

// A non-superuser owner whose rename is refused (a superuser session it
// may not terminate is connected): untouched, not recovery-incomplete.
// Once that session ends the same restore succeeds as the owner.
func TestPGIntegration_RefusedSwapThenOwnerRestore(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	f := newITFixture(t, ctx)
	app := "teployitswap"
	name := app + "-db"
	dexec := f.container(t, ctx, name, "-e POSTGRES_PASSWORD=pw -e POSTGRESQL_USERNAME=appuser -e POSTGRESQL_DATABASE=appdb "+pgITImage, pgITReady("postgres"))
	time.Sleep(3 * time.Second)
	f.waitFor(t, ctx, name, pgITReady("postgres"))
	dexec(pgQ("postgres", "postgres", "CREATE ROLE appuser LOGIN CREATEDB") + " && " + pgQ("postgres", "postgres", "CREATE DATABASE appdb OWNER appuser"))
	dexec(pgQ("appuser", "appdb", "CREATE TABLE items(id int); INSERT INTO items VALUES (1),(2),(3)"))

	client := NewClient(f.exec, os.Stdout)
	client.retryDelay = 200 * time.Millisecond
	image, env, _ := client.InspectAccessory(ctx, app, "db")
	res, err := client.AccessoryBackup(ctx, app, "db", image, env, f.s3)
	if err != nil {
		t.Fatal(err)
	}
	dexec(pgQ("appuser", "appdb", "INSERT INTO items VALUES (4)"))
	f.run(t, ctx, "docker exec -d "+name+" psql -U postgres -d appdb -c 'SELECT pg_sleep(600)'")
	time.Sleep(time.Second)
	_, rerr := client.AccessoryRestoreKeep(ctx, app, "db", image, res.ID, env, f.s3)
	if rerr == nil || IsRecoveryIncomplete(rerr) || !strings.Contains(rerr.Error(), "database untouched") {
		t.Fatalf("want an untouched, recoverable refusal, got %v", rerr)
	}
	t.Logf("swap correctly refused: %v", rerr)
	f.dropKeptTmp(rerr)
	if got := strings.TrimSpace(dexec(pgQ("postgres", "appdb", "SELECT count(*) FROM items"))); got != "4" {
		t.Fatalf("live database changed: %s", got)
	}
	dexec(pgQ("postgres", "postgres", "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE query LIKE '%pg_sleep(600)%' AND pid <> pg_backend_pid()"))
	if err := client.AccessoryRestore(ctx, app, "db", image, res.ID, env, f.s3); err != nil {
		t.Fatalf("owner restore once the superuser session ended: %v", err)
	}
	if got := strings.TrimSpace(dexec(pgQ("postgres", "appdb", "SELECT count(*) || ':' || pg_get_userbyid((SELECT datdba FROM pg_database WHERE datname='appdb')) FROM items"))); got != "3:appuser" {
		t.Fatalf("after owner restore = %q, want 3:appuser", got)
	}
}

// Database-level settings and ACLs (ALTER DATABASE SET search_path,
// REVOKE CONNECT FROM PUBLIC, a comment) survive the restore, and an open
// connection of the app's own role does not block it.
func TestPGIntegration_DatabaseSettingsAndOpenConnection(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	f := newITFixture(t, ctx)
	app := "teployitdbset"
	name := app + "-db"
	dexec := f.container(t, ctx, name, "-e POSTGRES_USER=app -e POSTGRES_PASSWORD=pw -e POSTGRES_DB=appdb "+pgITImage, pgITReady("app"))
	time.Sleep(3 * time.Second)
	f.waitFor(t, ctx, name, pgITReady("app"))
	dexec(pgQ("app", "appdb", "CREATE SCHEMA s; CREATE TABLE s.items(id int); INSERT INTO s.items VALUES (1),(2)"))
	dexec(pgQ("app", "postgres", "ALTER DATABASE appdb SET search_path TO s, public") + " && " + pgQ("app", "postgres", "REVOKE CONNECT ON DATABASE appdb FROM PUBLIC") + " && " + pgQ("app", "postgres", "COMMENT ON DATABASE appdb IS 'the app'"))
	state := func() string {
		return strings.TrimSpace(dexec(pgQ("app", "postgres", "SELECT coalesce((SELECT array_to_string(setconfig, ';') FROM pg_db_role_setting s JOIN pg_database d ON d.oid = s.setdatabase WHERE d.datname='appdb' AND s.setrole=0), '') || '|' || coalesce((SELECT string_agg(a::text, ',' ORDER BY a::text) FROM unnest(datacl) a), 'default') || '|' || coalesce(shobj_description(oid, 'pg_database'), '') FROM pg_database WHERE datname='appdb'")))
	}
	want := state()
	t.Logf("database-level state before backup: %s", want)
	if !strings.Contains(want, "search_path=s, public") || !strings.Contains(want, "the app") {
		t.Fatalf("fixture state unexpected: %s", want)
	}

	client := NewClient(f.exec, os.Stdout)
	image, env, _ := client.InspectAccessory(ctx, app, "db")
	res, err := client.AccessoryBackup(ctx, app, "db", image, env, f.s3)
	if err != nil {
		t.Fatal(err)
	}
	dexec(pgQ("app", "postgres", "ALTER DATABASE appdb RESET search_path") + " && " + pgQ("app", "postgres", "GRANT CONNECT ON DATABASE appdb TO PUBLIC") + " && " + pgQ("app", "postgres", "COMMENT ON DATABASE appdb IS NULL"))
	dexec(pgQ("app", "appdb", "INSERT INTO s.items VALUES (3)"))
	// The app's own connection, left open across the restore.
	f.run(t, ctx, "docker exec -d "+name+" psql -U app -d appdb -c 'SELECT pg_sleep(600)'")
	time.Sleep(time.Second)

	rec, err := client.AccessoryRestoreKeep(ctx, app, "db", image, res.ID, env, f.s3)
	if err != nil {
		t.Fatalf("restore with an open connection: %v", err)
	}
	if got := state(); got != want {
		t.Fatalf("database-level state after restore = %q, want %q", got, want)
	}
	if got := strings.TrimSpace(dexec("PGOPTIONS= " + pgQ("app", "appdb", "SELECT count(*) FROM items"))); got != "2" {
		t.Fatalf("search_path-resolved rows = %s, want the backed-up 2", got)
	}
	if !strings.HasPrefix(rec.PGDatabase, "appdb_teploy_old_") {
		t.Fatalf("previous database not kept: %+v", rec)
	}
	if got := strings.TrimSpace(dexec(pgQ("app", "postgres", fmt.Sprintf("SELECT count(*) FROM pg_database WHERE datname = '%s'", rec.PGDatabase)))); got != "1" {
		t.Fatalf("previous database %s missing before release", rec.PGDatabase)
	}
	if err := client.ReleaseRecovery(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(dexec(pgQ("app", "postgres", "SELECT count(*) FROM pg_database WHERE datname LIKE '%teploy_%'"))); got != "0" {
		t.Fatalf("release left %s databases", got)
	}
}

// Every backup a pre-G6 CLI took of a pgvector accessory is a raw tar of
// its data directory. It must still restore (as files, container
// stopped), and verify-backup must not pass it as a database or a volume.
func TestPGIntegration_LegacyTarOfReclassifiedEngine(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	f := newITFixture(t, ctx)
	f.exec.sudo = true
	app := "teployitlegacytar"
	name := app + "-db"
	accDir := "/deployments/" + app + "/accessories/db"
	f.run(t, ctx, "sudo -n mkdir -p "+accDir+"/pgdata")
	t.Cleanup(func() {
		f.base.Run(context.Background(), "sudo -n rm -rf /deployments/"+app+"; sudo -n rmdir /deployments 2>/dev/null; true")
	})
	dexec := f.container(t, ctx, name, "-e POSTGRES_USER=app -e POSTGRES_PASSWORD=pw -e POSTGRES_DB=appdb -v "+accDir+"/pgdata:/var/lib/postgresql/data pgvector/pgvector:pg17", pgITReady("app"))
	time.Sleep(3 * time.Second)
	f.waitFor(t, ctx, name, pgITReady("app"))
	dexec(pgQ("app", "appdb", "CREATE EXTENSION vector; CREATE TABLE items(id int, v vector(2)); INSERT INTO items VALUES (1,'[1,2]'),(2,'[3,4]')"))

	// What main's generic branch stored for this accessory.
	id := "20260101-000000-0123456789abcdef"
	obj := f.work + "/s3/itbucket/" + app + "/accessories/db/" + id + ".tar.gz"
	f.run(t, ctx, "docker stop "+name+" >/dev/null && sudo -n mkdir -p $(dirname "+obj+") && sudo -n tar -czf "+obj+" -C "+accDir+" . && docker start "+name+" >/dev/null")
	f.waitFor(t, ctx, name, pgITReady("app"))
	dexec(pgQ("app", "appdb", "INSERT INTO items VALUES (3,'[5,6]')"))

	client := NewClient(f.exec, os.Stdout)
	image, env, _ := client.InspectAccessory(ctx, app, "db")
	art, err := client.ResolveAccessoryArtifact(ctx, app, "db", image, id, env, f.s3)
	if err != nil || !art.FileRestore || art.Engine.Engine != EnginePostgres {
		t.Fatalf("artifact = %+v, %v; want a postgres file restore", art, err)
	}
	if err := client.AccessoryRestore(ctx, app, "db", image, id, env, f.s3); err == nil || !strings.Contains(err.Error(), "must be stopped") {
		t.Fatalf("a file restore into the running accessory must be refused, got %v", err)
	}
	// What `backup restore --accessory` does for a file artifact.
	f.run(t, ctx, "docker stop "+name+" >/dev/null")
	vr, err := client.VerifyBackup(ctx, app, "db", id, f.s3)
	if err != nil || vr.OK || !strings.Contains(vr.Detail, "raw file copy") {
		t.Fatalf("verify of a legacy tar = %+v, %v; want a not-verifiable failure", vr, err)
	}
	if err := client.AccessoryRestore(ctx, app, "db", image, id, env, f.s3); err != nil {
		t.Fatalf("file restore of the legacy tar: %v", err)
	}
	f.run(t, ctx, "docker start "+name+" >/dev/null")
	f.waitFor(t, ctx, name, pgITReady("app"))
	if got := strings.TrimSpace(dexec(pgQ("app", "appdb", "SELECT string_agg(id || '=' || v::text, ',' ORDER BY id) FROM items"))); got != "1=[1,2],2=[3,4]" {
		t.Fatalf("after legacy restore = %q", got)
	}
}

// A cluster with other databases is backed up and warned about. A restore's
// kept copy does not count as another database; --rollback swaps it back and
// --release discards the rest. backup_databases: dumps a listed database,
// and one restores on its own.
func TestPGIntegration_CoverageRollbackRelease(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	f := newITFixture(t, ctx)
	app := "teployitcover"
	name := app + "-db"
	dexec := f.container(t, ctx, name, "--label "+BackupDatabasesLabel+"=listed -e POSTGRES_USER=app -e POSTGRES_PASSWORD=pw -e POSTGRES_DB=appdb "+pgITImage, pgITReady("app"))
	time.Sleep(3 * time.Second)
	f.waitFor(t, ctx, name, pgITReady("app"))
	dexec(pgQ("app", "appdb", "CREATE TABLE items(id int); INSERT INTO items VALUES (1),(2)"))
	dexec(pgQ("app", "postgres", "CREATE DATABASE other") + " && " + pgQ("app", "postgres", "CREATE DATABASE listed") + " && " + pgQ("app", "postgres", "CREATE TABLE stray(x int)"))
	dexec(pgQ("app", "listed", "CREATE TABLE l(id int); INSERT INTO l VALUES (1)"))

	client := NewClient(f.exec, os.Stdout)
	image, env, _ := client.InspectAccessory(ctx, app, "db")
	res, err := client.AccessoryBackup(ctx, app, "db", image, env, f.s3)
	if err != nil {
		t.Fatalf("a multi-database cluster must still be backed up: %v", err)
	}
	if got := strings.Join(res.UncoveredDatabases, ","); got != "other,postgres" {
		t.Fatalf("uncovered = %q", got)
	}
	t.Logf("warnings: %v", res.Warnings)

	// One listed database restores on its own.
	dexec(pgQ("app", "listed", "INSERT INTO l VALUES (2)"))
	rec, err := client.AccessoryRestoreWith(ctx, app, "db", image, res.ID, env, f.s3, RestoreOptions{Database: "listed"})
	if err != nil {
		t.Fatalf("restoring the listed database: %v", err)
	}
	if err := client.ReleaseRecovery(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(dexec(pgQ("app", "listed", "SELECT count(*) FROM l"))); got != "1" {
		t.Fatalf("listed rows = %s", got)
	}

	// A kept previous database: the next backup still runs and warns.
	dexec(pgQ("app", "appdb", "INSERT INTO items VALUES (3)"))
	kept, err := client.AccessoryRestoreKeep(ctx, app, "db", image, res.ID, env, f.s3)
	if err != nil {
		t.Fatal(err)
	}
	next, err := client.AccessoryBackup(ctx, app, "db", image, env, f.s3)
	if err != nil {
		t.Fatalf("backup right after a kept restore: %v", err)
	}
	if strings.Contains(strings.Join(next.UncoveredDatabases, ","), "_teploy_") || !strings.Contains(strings.Join(next.Warnings, " "), kept.PGDatabase) {
		t.Fatalf("leftovers are warned about, not uncovered: %v %v", next.UncoveredDatabases, next.Warnings)
	}

	// Rollback: the pre-restore data (3 rows) comes back.
	rc, err := client.FindRecoveryCopies(ctx, app, "db", image, env)
	if err != nil || len(rc.Previous) != 1 {
		t.Fatalf("copies = %+v, %v", rc, err)
	}
	restored, displaced, err := client.RollbackRestore(ctx, rc, "")
	if err != nil || restored != kept.PGDatabase || displaced == "" {
		t.Fatalf("rollback = %s %s %v", restored, displaced, err)
	}
	if got := strings.TrimSpace(dexec(pgQ("app", "appdb", "SELECT count(*) FROM items"))); got != "3" {
		t.Fatalf("after rollback rows = %s, want the pre-restore 3", got)
	}
	// Release: the displaced restored copy goes, and nothing else does.
	rc, err = client.FindRecoveryCopies(ctx, app, "db", image, env)
	if err != nil || len(rc.Partial) != 1 {
		t.Fatalf("copies after rollback = %+v, %v", rc, err)
	}
	if released, err := client.ReleaseRecoveryCopies(ctx, rc); err != nil || len(released) != 1 {
		t.Fatalf("released %v, %v", released, err)
	}
	if got := strings.TrimSpace(dexec(pgQ("app", "postgres", "SELECT count(*) FROM pg_database WHERE datname LIKE '%teploy_%'"))); got != "0" {
		t.Fatalf("%s copies left", got)
	}
}

// 15 MB free, a near-empty live database and a much larger backup: the
// preflight sizes the backup and refuses. Forced with the override, the load
// fills the disk; the result must say what really happened (a dead server
// is recovery-incomplete, never "untouched").
func TestPGIntegration_DiskFull(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	f := newITFixture(t, ctx)
	app := "teployitdisk"
	name := app + "-db"
	dexec := f.container(t, ctx, name, "--tmpfs /pgtmp:size=220m -e PGDATA=/pgtmp/data -e POSTGRES_USER=app -e POSTGRES_PASSWORD=pw -e POSTGRES_DB=appdb "+pgITImage+" -c max_wal_size=48MB -c min_wal_size=32MB", pgITReady("app"))
	time.Sleep(3 * time.Second)
	f.waitFor(t, ctx, name, pgITReady("app"))
	dexec(pgQ("app", "appdb", "CREATE TABLE keep(id int); INSERT INTO keep VALUES (1); CREATE TABLE big AS SELECT i, md5(i::text)||md5((i+1)::text)||md5((i+2)::text) AS v FROM generate_series(1,400000) i"))

	client := NewClient(f.exec, os.Stdout)
	client.retryDelay = 2 * time.Second
	image, env, _ := client.InspectAccessory(ctx, app, "db")
	res, err := client.AccessoryBackup(ctx, app, "db", image, env, f.s3)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("artifact %d bytes", res.SizeBytes)

	// Shrink the live database and leave about 15 MB free.
	dexec(pgQ("app", "appdb", "DROP TABLE big; CHECKPOINT"))
	dexec(`dd if=/dev/zero of=/pgtmp/fill bs=1M count=$(( $(df -Pk /pgtmp | tail -1 | awk '{print $4}') / 1024 - 15 )) 2>/dev/null; df -Pk /pgtmp | tail -1`)

	_, err = client.AccessoryRestoreKeep(ctx, app, "db", image, res.ID, env, f.s3)
	if err == nil || !strings.Contains(err.Error(), "not enough free space") {
		t.Fatalf("the preflight must refuse a backup that cannot fit: %v", err)
	}
	t.Logf("preflight refused: %v", err)
	f.dropKeptTmp(err)

	_, err = client.AccessoryRestoreWith(ctx, app, "db", image, res.ID, env, f.s3, RestoreOptions{IgnoreSpaceCheck: true})
	if err == nil {
		t.Fatal("the load cannot fit in 15 MB")
	}
	t.Logf("forced load: %v", err)
	f.dropKeptTmp(err)
	state, _ := f.base.Run(ctx, "docker inspect -f '{{.State.Status}} exit={{.State.ExitCode}}' "+name+"; docker logs --tail 12 "+name+" 2>&1")
	t.Logf("container after the forced load: %s", state)
	if out, aliveErr := f.base.Run(ctx, "docker exec "+name+" "+pgQ("app", "appdb", "SELECT count(*) FROM keep")); aliveErr != nil {
		if !IsRecoveryIncomplete(err) || strings.Contains(err.Error(), "database untouched") {
			t.Fatalf("the server is down (%v) but the error claims otherwise: %v", aliveErr, err)
		}
	} else {
		if IsRecoveryIncomplete(err) {
			t.Logf("server came back after the classification: %s", out)
		} else if strings.TrimSpace(out) != "1" || !strings.Contains(err.Error(), "database untouched") {
			t.Fatalf("live rows = %q with error %v", out, err)
		}
	}
}

// A legacy plain .sql.gz carries no database-level entries, so the live
// database's settings, ACL and comment are read before the swap and applied.
func TestPGIntegration_LegacyPlainKeepsLiveSettings(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	f := newITFixture(t, ctx)
	app := "teployitlegacyset"
	name := app + "-db"
	dexec := f.container(t, ctx, name, "-e POSTGRES_USER=app -e POSTGRES_PASSWORD=pw -e POSTGRES_DB=appdb "+pgITImage, pgITReady("app"))
	time.Sleep(3 * time.Second)
	f.waitFor(t, ctx, name, pgITReady("app"))
	dexec(pgQ("app", "appdb", "CREATE SCHEMA s; CREATE TABLE s.items(id int); INSERT INTO s.items VALUES (1)"))
	id := "20260101-000000-0123456789abcdef"
	obj := f.work + "/s3/itbucket/" + app + "/accessories/db/" + id + ".sql.gz"
	f.run(t, ctx, "mkdir -p $(dirname "+obj+") && docker exec "+name+" pg_dump -U app appdb > "+obj+".sql && gzip -c "+obj+".sql > "+obj+" && rm -f "+obj+".sql")
	dexec(pgQ("app", "postgres", "ALTER DATABASE appdb SET search_path TO s, public") + " && " + pgQ("app", "postgres", "REVOKE CONNECT ON DATABASE appdb FROM PUBLIC") + " && " + pgQ("app", "postgres", "COMMENT ON DATABASE appdb IS 'the app'"))
	state := func() string {
		return strings.TrimSpace(dexec(pgQ("app", "postgres", "SELECT coalesce((SELECT array_to_string(setconfig, ';') FROM pg_db_role_setting s JOIN pg_database d ON d.oid = s.setdatabase WHERE d.datname='appdb' AND s.setrole=0), '') || '|' || coalesce((SELECT string_agg(a::text, ',' ORDER BY a::text) FROM unnest(datacl) a), 'default') || '|' || coalesce(shobj_description(oid, 'pg_database'), '') FROM pg_database WHERE datname='appdb'")))
	}
	want := state()
	dexec(pgQ("app", "appdb", "INSERT INTO s.items VALUES (2)"))
	client := NewClient(f.exec, os.Stdout)
	image, env, _ := client.InspectAccessory(ctx, app, "db")
	if err := client.AccessoryRestore(ctx, app, "db", image, id, env, f.s3); err != nil {
		t.Fatal(err)
	}
	if got := state(); got != want {
		t.Fatalf("after legacy restore settings = %q, want %q", got, want)
	}
	if got := strings.TrimSpace(dexec(pgQ("app", "appdb", "SELECT count(*) FROM items"))); got != "1" {
		t.Fatalf("rows = %s", got)
	}
}

// An ICU database comes back ICU, with its locale.
func TestPGIntegration_ICUDatabaseKeepsProvider(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	f := newITFixture(t, ctx)
	app := "teployiticu"
	name := app + "-db"
	dexec := f.container(t, ctx, name, "-e POSTGRES_USER=app -e POSTGRES_PASSWORD=pw -e POSTGRES_DB=appdb "+pgITImage, pgITReady("app"))
	time.Sleep(3 * time.Second)
	f.waitFor(t, ctx, name, pgITReady("app"))
	dexec(pgQ("app", "postgres", "DROP DATABASE appdb") + " && " + pgQ("app", "postgres", "CREATE DATABASE appdb TEMPLATE template0 ENCODING 'UTF8' LOCALE_PROVIDER icu ICU_LOCALE 'de-DE'"))
	dexec(pgQ("app", "appdb", "CREATE TABLE items(id int); INSERT INTO items VALUES (1)"))
	provider := func() string {
		return strings.TrimSpace(dexec(pgQ("app", "postgres", "SELECT datlocprovider::text || ':' || coalesce(datlocale, '') FROM pg_database WHERE datname='appdb'")))
	}
	if got := provider(); got != "i:de-DE" {
		t.Fatalf("fixture provider = %q", got)
	}
	client := NewClient(f.exec, os.Stdout)
	image, env, _ := client.InspectAccessory(ctx, app, "db")
	res, err := client.AccessoryBackup(ctx, app, "db", image, env, f.s3)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.AccessoryRestore(ctx, app, "db", image, res.ID, env, f.s3); err != nil {
		t.Fatal(err)
	}
	if got := provider(); got != "i:de-DE" {
		t.Fatalf("after restore provider = %q, want i:de-DE", got)
	}
}

// A hostile database name stays data: backup and restore run nothing it says.
func TestPGIntegration_HostileDatabaseNameIsData(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	f := newITFixture(t, ctx)
	app := "teployithostile"
	name := app + "-db"
	marker := "/tmp/teploy-it-pwnrr"
	hostile := "x$(touch " + marker + ")"
	t.Cleanup(func() { f.base.Run(context.Background(), "rm -f "+marker) })
	dexec := f.container(t, ctx, name, "-e POSTGRES_PASSWORD=pw -e POSTGRES_DB="+ssh.ShellQuote(hostile)+" "+pgITImage, pgITReady("postgres"))
	time.Sleep(3 * time.Second)
	f.waitFor(t, ctx, name, pgITReady("postgres"))
	dexec("psql -X -q -v ON_ERROR_STOP=1 -U postgres -d postgres -c " + ssh.ShellQuote("CREATE DATABASE other"))
	client := NewClient(f.exec, os.Stdout)
	image, env, _ := client.InspectAccessory(ctx, app, "db")
	dexec("psql -X -q -v ON_ERROR_STOP=1 -U postgres -c 'CREATE TABLE t(i int)' " + ssh.ShellQuote(hostile))
	res, err := client.AccessoryBackup(ctx, app, "db", image, env, f.s3)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.AccessoryRestore(ctx, app, "db", image, res.ID, env, f.s3); err != nil {
		t.Fatal(err)
	}
	if out, _ := f.base.Run(ctx, "ls "+marker+" 2>/dev/null; docker exec "+name+" ls "+marker+" 2>/dev/null; true"); strings.TrimSpace(out) != "" {
		t.Fatalf("the database name was executed: %s", out)
	}
}

// A real Bitnami image: POSTGRESQL_* env reaches the verify scratch, and the
// non-superuser role can back up, verify and restore in place.
func TestPGIntegration_RealBitnami(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	f := newITFixture(t, ctx)
	app := "teployitbitnami"
	name := app + "-db"
	dexec := f.container(t, ctx, name, "-e POSTGRESQL_USERNAME=appuser -e POSTGRESQL_PASSWORD=pw -e POSTGRESQL_DATABASE=appdb bitnami/postgresql:latest", pgITReady("appuser"))
	time.Sleep(5 * time.Second)
	f.waitFor(t, ctx, name, pgITReady("appuser"))
	dexec("PGPASSWORD=pw " + pgQ("appuser", "appdb", "CREATE TABLE items(id int); INSERT INTO items VALUES (1),(2)"))
	client := NewClient(f.exec, os.Stdout)
	image, env, err := client.InspectAccessory(ctx, app, "db")
	if err != nil {
		t.Fatal(err)
	}
	if det, _ := client.ResolveEngine(ctx, app, "db", image, env); det.Engine != EnginePostgres {
		t.Fatalf("bitnami classified as %+v", det)
	}
	res, err := client.AccessoryBackup(ctx, app, "db", image, env, f.s3)
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	f.run(t, ctx, "docker stop "+name+" >/dev/null")
	vr, err := client.VerifyBackup(ctx, app, "db", res.ID, f.s3)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("verify: ok=%v metric=%s detail=%s", vr.OK, vr.Metric, vr.Detail)
	if !vr.OK {
		t.Fatalf("verify of a real bitnami backup failed: %s", vr.Detail)
	}
	f.run(t, ctx, "docker start "+name+" >/dev/null")
	f.waitFor(t, ctx, name, pgITReady("appuser"))
	dexec("PGPASSWORD=pw " + pgQ("appuser", "appdb", "INSERT INTO items VALUES (3)"))
	if err := client.AccessoryRestore(ctx, app, "db", image, res.ID, env, f.s3); err != nil {
		t.Fatalf("in-place restore: %v", err)
	}
	if got := strings.TrimSpace(dexec("PGPASSWORD=pw " + pgQ("appuser", "appdb", "SELECT count(*) FROM items"))); got != "2" {
		t.Fatalf("rows = %s", got)
	}
}

// Reviewer's blocker: a table created with innodb_strict_mode=0 backs up, but
// the dump will not load (error 1118). The dry run refuses before DROP; all
// three tables and the data changed since remain.
func TestMariaDBIntegration_UnloadableDumpRefusedBeforeDrop(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	f := newITFixture(t, ctx)
	app := "teployitstrict"
	name := app + "-db"
	sql := func(s string) string {
		return "MYSQL_PWD=rootpw mariadb -u root -N appdb -e " + ssh.ShellQuote(s)
	}
	dexec := f.container(t, ctx, name, "-e MARIADB_ROOT_PASSWORD=rootpw -e MARIADB_DATABASE=appdb mariadb:11", "sh -c 'MYSQL_PWD=rootpw mariadb --protocol=TCP -h 127.0.0.1 -u root -e \"select 1\" appdb'")
	var cols []string
	for i := 0; i < 40; i++ {
		cols = append(cols, fmt.Sprintf("c%d VARCHAR(255)", i))
	}
	dexec(sql("CREATE TABLE a(id int); INSERT INTO a VALUES (1); CREATE TABLE m(id int); SET SESSION innodb_strict_mode=0; CREATE TABLE z(" + strings.Join(cols, ", ") + ") ROW_FORMAT=COMPACT"))
	client := NewClient(f.exec, os.Stdout)
	image, env, _ := client.InspectAccessory(ctx, app, "db")
	res, err := client.AccessoryBackup(ctx, app, "db", image, env, f.s3)
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	dexec(sql("INSERT INTO a VALUES (2)"))
	_, rerr := client.AccessoryRestoreKeep(ctx, app, "db", image, res.ID, env, f.s3)
	if rerr == nil || IsRecoveryIncomplete(rerr) || !strings.Contains(rerr.Error(), "database untouched") {
		t.Fatalf("want a dry-run refusal, got %v", rerr)
	}
	t.Logf("dry run refused: %v", rerr)
	f.dropKeptTmp(rerr)
	if got := strings.TrimSpace(dexec(sql("SELECT GROUP_CONCAT(table_name ORDER BY table_name) FROM information_schema.tables WHERE table_schema='appdb'; SELECT COUNT(*) FROM a; SELECT COUNT(*) FROM information_schema.schemata WHERE schema_name LIKE '%teploy%';"))); got != "a,m,z\n2\n0" {
		t.Fatalf("live database changed: %q", got)
	}
}

// Mongo: an archive the server cannot load is refused with the data
// untouched and no dry-run databases left.
func TestMongoIntegration_UnloadableArchiveRefused(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	f := newITFixture(t, ctx)
	app := "teployitmongobad"
	name := app + "-db"
	dexec := f.container(t, ctx, name, "mongo:7", "mongosh --quiet --eval 'db.runCommand({ping:1}).ok'")
	dexec("mongosh --quiet appdb --eval " + ssh.ShellQuote("db.items.insertOne({_id:1})"))
	id := "20260101-000000-0123456789abcdef"
	obj := f.work + "/s3/itbucket/" + app + "/accessories/db/" + id + ".archive.gz"
	f.run(t, ctx, "mkdir -p $(dirname "+obj+") && head -c 4096 /dev/urandom | gzip -c > "+obj)
	client := NewClient(f.exec, os.Stdout)
	image, env, _ := client.InspectAccessory(ctx, app, "db")
	_, rerr := client.AccessoryRestoreKeep(ctx, app, "db", image, id, env, f.s3)
	if rerr == nil || IsRecoveryIncomplete(rerr) || !strings.Contains(rerr.Error(), "database untouched") {
		t.Fatalf("want a dry-run refusal, got %v", rerr)
	}
	t.Logf("dry run refused: %v", rerr)
	f.dropKeptTmp(rerr)
	got := strings.TrimSpace(dexec("mongosh --quiet appdb --eval " + ssh.ShellQuote("print(db.items.countDocuments()+':'+db.getMongo().getDBNames().filter(function(n){return n.indexOf('teploy_check_')===0}).length)")))
	if got != "1:0" {
		t.Fatalf("after refusal = %q, want 1:0", got)
	}
}
