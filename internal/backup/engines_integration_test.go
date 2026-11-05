//go:build integration

// REAL-engine verification of the accessory backup, verify-backup and
// in-place restore paths (G6/G7): one real container per engine on the
// colima SSH+Docker fixture, real dump/restore tooling, no mocks in the
// path under test. Same fixture contract as dr_integration_test.go:
//
//	TEPLOY_FAULT_HOST=127.0.0.1:<colima ssh port> \
//	TEPLOY_FAULT_USER=tyler \
//	TEPLOY_FAULT_KEY=~/.colima/_lima/_config/user \
//	go test -tags integration -p 2 -run TestEngineIntegration -v ./internal/backup
//
// TEPLOY_IT_ENGINES (comma list) narrows the matrix. S3 is replaced by a
// fake `aws` on PATH that maps s3://bucket/key to a private directory, so
// the test needs no object store (and no second container). At most one
// engine container runs at a time: the accessory is stopped while the
// verify-backup scratch runs, and every container is removed on exit.
package backup

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy/internal/ssh"
)

// fakeAWS implements the three aws subcommands the backup client uses
// (s3 cp, s3 ls, s3 rm) against a local directory.
const fakeAWS = `#!/bin/sh
set -eu
root=__ROOT__
[ "${1:-}" = --version ] && { echo "fake aws"; exit 0; }
if [ "$1" = s3api ]; then
  # head-object --bucket B --key K [--region R] [--endpoint-url E]
  [ "$2" = head-object ] || exit 1
  shift 2; b=""; k=""
  while [ $# -gt 0 ]; do case "$1" in --bucket) b=$2; shift 2;; --key) k=$2; shift 2;; *) shift;; esac; done
  f="$root/$b/$k"; [ -f "$f" ] || { echo "An error occurred (404) when calling the HeadObject operation" >&2; exit 254; }
  printf '{"ContentLength":%s,"LastModified":"%s","Metadata":{"sha256":"%s"}}\n' "$(wc -c < "$f" | tr -d ' ')" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$(sha256sum "$f" | cut -d' ' -f1)"
  exit 0
fi
[ "$1" = s3 ] || { echo "fake aws: only s3 is supported" >&2; exit 2; }
op=$2; shift 2
args=""
recursive=no
while [ $# -gt 0 ]; do
  case "$1" in
    --region|--endpoint-url|--metadata|--copy-props) shift 2 ;;
    --recursive) recursive=yes; shift ;;
    --only-show-errors) shift ;;
    *) args="$args $1"; shift ;;
  esac
done
set -- $args
map() { case "$1" in s3://*) printf '%s/%s' "$root" "${1#s3://}" ;; *) printf '%s' "$1" ;; esac; }
case "$op" in
  cp)
    src=$(map "$1"); dst=$(map "$2")
    if [ "$1" = - ]; then mkdir -p "$(dirname "$dst")"; cat > "$dst"; exit 0; fi
    [ -f "$src" ] || { echo "fatal error: An error occurred (404) when calling the HeadObject operation: Key \"$1\" does not exist" >&2; exit 1; }
    if [ "$2" = - ]; then cat "$src"; exit 0; fi
    mkdir -p "$(dirname "$dst")"
    cp "$src" "$dst" ;;
  rm)
    rm -f "$(map "$1")" ;;
  ls)
    p=$(map "$1")
    if [ "$recursive" = yes ]; then
      [ -d "${p%/}" ] || exit 1
      (cd "$root/${1#s3://}" 2>/dev/null && find . -type f | sed 's|^\./||') | while read -r f; do
        printf '2026-11-03 00:00:00 %s %s\n' "$(wc -c < "${p%/}/$f" | tr -d ' ')" "${1#s3://}$f"
      done | sed "s| itbucket/| |"
      exit 0
    fi
    case "$p" in
      */) dir=${p%/}; pre="" ;;
      *) dir=$(dirname "$p"); pre=$(basename "$p") ;;
    esac
    [ -d "$dir" ] || exit 1
    found=no
    for f in "$dir"/"$pre"*; do
      [ -d "$f" ] && { found=yes; printf '                           PRE %s/\n' "$(basename "$f")"; continue; }
      [ -f "$f" ] || continue
      found=yes
      printf '2026-11-03 00:00:00 %s %s\n' "$(wc -c < "$f" | tr -d ' ')" "$(basename "$f")"
    done
    [ "$found" = yes ] || exit 1 ;;
  *) echo "fake aws: unsupported op $op" >&2; exit 2 ;;
esac
`

// pathExecutor prefixes every command with the fake-aws PATH; with sudo
// set it runs each command as root (production SSH users are root, and
// file restores move engine-owned 0700 data directories).
type pathExecutor struct {
	ssh.Executor
	prefix string
	sudo   bool
}

func (p pathExecutor) wrap(cmd string) string {
	if p.sudo {
		return "sudo -n sh -c " + ssh.ShellQuote(p.prefix+cmd)
	}
	return p.prefix + cmd
}

func (p pathExecutor) Run(ctx context.Context, cmd string) (string, error) {
	return p.Executor.Run(ctx, p.wrap(cmd))
}

func (p pathExecutor) RunStream(ctx context.Context, cmd string, stdout, stderr io.Writer) error {
	return p.Executor.RunStream(ctx, p.wrap(cmd), stdout, stderr)
}

func (p pathExecutor) RunInput(ctx context.Context, cmd string, stdin io.Reader) error {
	return p.Executor.RunInput(ctx, p.wrap(cmd), stdin)
}

// Upload writes through sudo too when the commands do: the scratch dirs a
// root executor makes are 0700 root.
func (p pathExecutor) Upload(ctx context.Context, r io.Reader, path, mode string) error {
	if !p.sudo {
		return p.Executor.Upload(ctx, r, path, mode)
	}
	return p.Executor.RunInput(ctx, "sudo -n sh -c "+ssh.ShellQuote("umask 077; cat > "+ssh.ShellQuote(path)+" && chmod "+mode+" "+ssh.ShellQuote(path)), r)
}

// itFixture is the shared integration fixture: the colima SSH executor, a
// private work dir holding the fake aws and its "bucket", and the
// fake-aws executor.
type itFixture struct {
	base ssh.Executor
	exec pathExecutor
	work string
	s3   S3Config
}

func newITFixture(t *testing.T, ctx context.Context) itFixture {
	t.Helper()
	base := drFixtureEnv(t)
	var nonce [6]byte
	_, _ = rand.Read(nonce[:])
	work := "/tmp/teploy-it-" + hex.EncodeToString(nonce[:])
	if _, err := base.Run(ctx, "mkdir -p "+work+"/bin "+work+"/s3"); err != nil {
		t.Fatalf("creating fixture workspace: %v", err)
	}
	t.Cleanup(func() {
		base.Run(context.Background(), "sudo -n rm -rf "+ssh.ShellQuote(work)+" /var/tmp/teploy-restore.* /var/tmp/teploy-pre-restore.teployit* || rm -rf "+ssh.ShellQuote(work))
	})
	if err := base.Upload(ctx, strings.NewReader(strings.ReplaceAll(fakeAWS, "__ROOT__", work+"/s3")), work+"/bin/aws", "0755"); err != nil {
		t.Fatalf("installing fake aws: %v", err)
	}
	return itFixture{base: base, exec: pathExecutor{Executor: base, prefix: "PATH=" + work + "/bin:$PATH; export PATH; "}, work: work, s3: S3Config{Bucket: "itbucket", Region: "us-east-1"}}
}

// container starts one engine container (removed on cleanup, along with
// any verify scratch) and waits until probe succeeds inside it.
func (f itFixture) container(t *testing.T, ctx context.Context, name, runArgs, probe string) func(string) string {
	t.Helper()
	rm := func() {
		f.base.Run(context.Background(), "docker rm -fv "+ssh.ShellQuote(name)+" >/dev/null 2>&1 || true")
		f.base.Run(context.Background(), "docker ps -aq --filter name=teploy-backup-verify- | xargs -r docker rm -fv >/dev/null 2>&1 || true")
	}
	rm()
	t.Cleanup(rm)
	f.run(t, ctx, "docker run -d --name "+ssh.ShellQuote(name)+" --restart no --memory 768m "+runArgs+" >/dev/null")
	f.waitFor(t, ctx, name, probe)
	return func(cmd string) string {
		return f.run(t, ctx, "docker exec "+ssh.ShellQuote(name)+" sh -c "+ssh.ShellQuote(cmd))
	}
}

// loopVolume mounts a real ext4 volume of sizeMB (no reserved blocks), the way
// a small data disk behaves, and returns its mountpoint; bind mnt/data into a
// container, and fill the rest with fill.
func (f itFixture) loopVolume(t *testing.T, ctx context.Context, sizeMB int) string {
	t.Helper()
	img, mnt := f.work+"/vol.img", f.work+"/vol"
	f.run(t, ctx, fmt.Sprintf("truncate -s %dM %s && mkfs.ext4 -q -F -m 0 %s && mkdir -p %s && sudo -n mount -o loop %s %s && sudo -n mkdir -p %s/data", sizeMB, img, img, mnt, img, mnt, mnt))
	t.Cleanup(func() {
		f.base.Run(context.Background(), "sudo -n umount "+mnt+" 2>/dev/null; rm -f "+img)
	})
	return mnt
}

// fill leaves about freeMB free on the volume.
func (f itFixture) fill(t *testing.T, ctx context.Context, mnt string, freeMB int) {
	t.Helper()
	f.run(t, ctx, fmt.Sprintf("sudo -n rm -f %s/fill; sync; sudo -n dd if=/dev/zero of=%s/fill bs=1M count=$(( $(df -Pk %s | tail -1 | awk '{print $4}') / 1024 - %d )) 2>/dev/null; sync; df -Pk %s | tail -1", mnt, mnt, mnt, freeMB, mnt))
}

func (f itFixture) waitFor(t *testing.T, ctx context.Context, name, probe string) {
	t.Helper()
	f.run(t, ctx, fmt.Sprintf("for i in $(seq 1 60); do docker exec %s %s >/dev/null 2>&1 && exit 0; sleep 2; done; docker logs --tail 30 %s; exit 1", ssh.ShellQuote(name), probe, ssh.ShellQuote(name)))
}

func (f itFixture) run(t *testing.T, ctx context.Context, cmd string) string {
	t.Helper()
	out, err := f.base.Run(ctx, cmd)
	if err != nil {
		t.Fatalf("fixture command failed: %s\nerror: %v\noutput: %s", cmd, err, out)
	}
	return out
}

type engineCase struct {
	key   string
	image string
	env   []string
	// ready is a shell probe run via `docker exec <container>`.
	ready string
	// seed writes the backed-up dataset; mutate changes it afterwards
	// (including a new table/key and a deleted row); check returns the
	// dataset fingerprint that must equal the seeded one after restore.
	seed, mutate, check string
	want                string
	wantKind            string
	cmd                 []string
}

func engineCases() []engineCase {
	psql := func(sql string) string {
		return "psql -v ON_ERROR_STOP=1 -qtA -U app -d appdb -c " + ssh.ShellQuote(sql)
	}
	pgSeed := psql("CREATE TABLE items(id int primary key, name text); INSERT INTO items VALUES (1,'one'),(2,'two'),(3,'three');")
	pgMutate := psql("UPDATE items SET name='changed' WHERE id=1; DELETE FROM items WHERE id=2; INSERT INTO items VALUES (4,'four'); CREATE TABLE later(x int);")
	pgCheck := psql("SELECT string_agg(id||':'||name, ',' ORDER BY id) FROM items") + " && " + psql("SELECT count(*) FROM information_schema.tables WHERE table_name='later'")
	pgEnv := []string{"POSTGRES_USER=app", "POSTGRES_PASSWORD=pw", "POSTGRES_DB=appdb"}
	pgReady := "pg_isready -U app -d appdb"

	mysql := func(sql string) string {
		return "sh -c " + ssh.ShellQuote("MYSQL_PWD=rootpw mysql -u root -N appdb -e "+ssh.ShellQuote(sql))
	}
	redisCLI := func(_, args string) string { return args }

	mariadb := func(sql string) string {
		return "sh -c " + ssh.ShellQuote("MYSQL_PWD=rootpw mariadb -u root -N appdb -e "+ssh.ShellQuote(sql))
	}
	mongo := func(js string) string { return "mongosh --quiet appdb --eval " + ssh.ShellQuote(js) }

	cases := []engineCase{
		{key: "postgres", image: "postgres:17-alpine", env: pgEnv, ready: pgReady, seed: pgSeed, mutate: pgMutate, check: pgCheck, want: "1:one,2:two,3:three\n0", wantKind: "postgres"},
		{key: "pgvector", image: "pgvector/pgvector:pg17", env: pgEnv, ready: pgReady,
			seed:   psql("CREATE EXTENSION vector; CREATE TABLE items(id int primary key, name text, v vector(3)); INSERT INTO items VALUES (1,'one','[1,2,3]'),(2,'two','[4,5,6]'),(3,'three','[7,8,9]');"),
			mutate: pgMutate,
			check:  psql("SELECT string_agg(id||':'||name||':'||v::text, ',' ORDER BY id) FROM items") + " && " + psql("SELECT count(*) FROM information_schema.tables WHERE table_name='later'"),
			want:   "1:one:[1,2,3],2:two:[4,5,6],3:three:[7,8,9]\n0", wantKind: "postgres"},
		{key: "timescale", image: "timescale/timescaledb:latest-pg17", env: append(pgEnv, "TIMESCALEDB_TELEMETRY=off"), ready: pgReady,
			seed: psql("CREATE EXTENSION IF NOT EXISTS timescaledb; CREATE TABLE metrics(ts timestamptz not null, id int, name text); SELECT create_hypertable('metrics','ts'); " +
				"INSERT INTO metrics VALUES ('2026-01-01',1,'one'),('2026-02-01',2,'two'),('2026-03-01',3,'three');"),
			mutate: psql("UPDATE metrics SET name='changed' WHERE id=1; DELETE FROM metrics WHERE id=2; INSERT INTO metrics VALUES ('2026-04-01',4,'four'); CREATE TABLE later(x int);"),
			check: psql("SELECT string_agg(id||':'||name, ',' ORDER BY id) FROM metrics") + " && " + psql("SELECT count(*) FROM information_schema.tables WHERE table_name='later'") +
				" && " + psql("SELECT count(*) FROM timescaledb_information.hypertables WHERE hypertable_name='metrics'"),
			want: "1:one,2:two,3:three\n0\n1", wantKind: "postgres"},
		{key: "mysql", image: "mysql:8", env: []string{"MYSQL_ROOT_PASSWORD=rootpw", "MYSQL_DATABASE=appdb"},
			ready:  "sh -c 'MYSQL_PWD=rootpw mysqladmin ping -u root --silent && MYSQL_PWD=rootpw mysql -u root -e \"select 1\" appdb'",
			seed:   mysql("CREATE TABLE items(id int primary key, name varchar(20)); INSERT INTO items VALUES (1,'one'),(2,'two'),(3,'three'); CREATE TRIGGER items_bi BEFORE INSERT ON items FOR EACH ROW SET NEW.name=NEW.name; CREATE PROCEDURE p1() SELECT 1;"),
			mutate: mysql("UPDATE items SET name='changed' WHERE id=1; DELETE FROM items WHERE id=2; INSERT INTO items VALUES (4,'four'); DROP PROCEDURE p1; CREATE TABLE later(x int);"),
			check:  mysql("SELECT GROUP_CONCAT(CONCAT(id,':',name) ORDER BY id) FROM items; SELECT COUNT(*) FROM information_schema.triggers WHERE trigger_schema='appdb'; SELECT COUNT(*) FROM information_schema.routines WHERE routine_schema='appdb'; SELECT COUNT(*) FROM information_schema.tables WHERE table_schema='appdb' AND table_name='later';"),
			want:   "1:one,2:two,3:three\n1\n1\n0", wantKind: "mysql"},
		{key: "mariadb", image: "mariadb:11", env: []string{"MARIADB_ROOT_PASSWORD=rootpw", "MARIADB_DATABASE=appdb"},
			ready:  "sh -c 'MYSQL_PWD=rootpw mariadb --protocol=TCP -h 127.0.0.1 -u root -e \"select 1\" appdb'",
			seed:   mariadb("CREATE TABLE items(id int primary key, name varchar(20)); INSERT INTO items VALUES (1,'one'),(2,'two'),(3,'three'); CREATE TRIGGER items_bi BEFORE INSERT ON items FOR EACH ROW SET NEW.name=NEW.name; CREATE PROCEDURE p1() SELECT 1;"),
			mutate: mariadb("UPDATE items SET name='changed' WHERE id=1; DELETE FROM items WHERE id=2; INSERT INTO items VALUES (4,'four'); DROP PROCEDURE p1; CREATE TABLE later(x int);"),
			check:  mariadb("SELECT GROUP_CONCAT(CONCAT(id,':',name) ORDER BY id) FROM items; SELECT COUNT(*) FROM information_schema.triggers WHERE trigger_schema='appdb'; SELECT COUNT(*) FROM information_schema.routines WHERE routine_schema='appdb'; SELECT COUNT(*) FROM information_schema.tables WHERE table_schema='appdb' AND table_name='later';"),
			want:   "1:one,2:two,3:three\n1\n1\n0", wantKind: "mysql"},
		{key: "mongo", image: "mongo:7", ready: "mongosh --quiet --eval 'db.runCommand({ping:1}).ok'",
			seed:   mongo("db.items.insertMany([{_id:1,name:'one'},{_id:2,name:'two'},{_id:3,name:'three'}])"),
			mutate: mongo("db.items.updateOne({_id:1},{$set:{name:'changed'}}); db.items.deleteOne({_id:2}); db.items.insertOne({_id:4,name:'four'})"),
			check:  mongo("print(db.items.find().sort({_id:1}).toArray().map(function(d){return d._id+':'+d.name}).join(','))"),
			want:   "1:one,2:two,3:three", wantKind: "mongo"},
		{key: "redis", image: "redis:7-alpine", ready: "redis-cli ping",
			seed:   redisCLI("", ("redis-cli set a one && redis-cli set b two && redis-cli rpush l x y z")),
			mutate: redisCLI("", ("redis-cli set a changed && redis-cli del b && redis-cli set c new")),
			check:  redisCLI("", ("redis-cli get a; redis-cli get b; redis-cli exists c; redis-cli lrange l 0 -1")),
			want:   "one\ntwo\n0\nx\ny\nz", wantKind: "redis"},
		{key: "valkey", image: "valkey/valkey:8-alpine", ready: "valkey-cli ping",
			seed:   redisCLI("", ("valkey-cli set a one && valkey-cli set b two && valkey-cli rpush l x y z")),
			mutate: redisCLI("", ("valkey-cli set a changed && valkey-cli del b && valkey-cli set c new")),
			check:  redisCLI("", ("valkey-cli get a; valkey-cli get b; valkey-cli exists c; valkey-cli lrange l 0 -1")),
			want:   "one\ntwo\n0\nx\ny\nz", wantKind: "valkey"},
		{key: "dragonfly", image: "docker.dragonflydb.io/dragonflydb/dragonfly:latest", ready: "redis-cli ping",
			seed:   redisCLI("", ("redis-cli set a one && redis-cli set b two && redis-cli rpush l x y z")),
			mutate: redisCLI("", ("redis-cli set a changed && redis-cli del b && redis-cli set c new")),
			check:  redisCLI("", ("redis-cli get a; redis-cli get b; redis-cli exists c; redis-cli lrange l 0 -1")),
			want:   "one\ntwo\n0\nx\ny\nz", wantKind: "dragonfly",
			cmd: []string{"--proactor_threads=1", "--maxmemory=256mb"}},
	}
	only := strings.TrimSpace(os.Getenv("TEPLOY_IT_ENGINES"))
	if only == "" {
		return cases
	}
	var out []engineCase
	for _, c := range cases {
		for _, k := range strings.Split(only, ",") {
			if strings.TrimSpace(k) == c.key {
				out = append(out, c)
			}
		}
	}
	return out
}

func TestEngineIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	defer cancel()
	f := newITFixture(t, ctx)
	base, exec, work, s3 := f.base, f.exec, f.work, f.s3

	for _, tc := range engineCases() {
		t.Run(tc.key, func(t *testing.T) {
			app := "teployit" + tc.key
			container := app + "-db"
			rm := func() {
				base.Run(context.Background(), "docker rm -fv "+ssh.ShellQuote(container)+" >/dev/null 2>&1 || true")
			}
			rm()
			t.Cleanup(rm)
			// Belt and braces: no verify scratch may outlive the subtest.
			t.Cleanup(func() {
				base.Run(context.Background(), "docker ps -aq --filter name=teploy-backup-verify- | xargs -r docker rm -fv >/dev/null 2>&1 || true")
			})
			run := func(cmd string) string {
				t.Helper()
				out, err := base.Run(ctx, cmd)
				if err != nil {
					t.Fatalf("fixture command failed: %s\nerror: %v\noutput: %s", cmd, err, out)
				}
				return out
			}
			dexec := func(cmd string) string {
				return run("docker exec " + ssh.ShellQuote(container) + " sh -c " + ssh.ShellQuote(cmd))
			}
			waitReady := func() {
				t.Helper()
				run(fmt.Sprintf("for i in $(seq 1 60); do docker exec %s %s >/dev/null 2>&1 && exit 0; sleep 2; done; docker logs --tail 30 %s; exit 1",
					ssh.ShellQuote(container), tc.ready, ssh.ShellQuote(container)))
			}

			envFlags := ""
			for _, e := range tc.env {
				envFlags += " -e " + ssh.ShellQuote(e)
			}
			cmdArgs := ""
			for _, a := range tc.cmd {
				cmdArgs += " " + ssh.ShellQuote(a)
			}
			run(fmt.Sprintf("docker run -d --name %s --restart no --memory 768m --label teploy.app=%s --label teploy.role=accessory --label teploy.accessory=db%s %s%s >/dev/null",
				ssh.ShellQuote(container), app, envFlags, ssh.ShellQuote(tc.image), cmdArgs))
			waitReady()
			// The postgres entrypoint restarts the server once after init;
			// a second probe after a pause rules out racing that restart.
			time.Sleep(3 * time.Second)
			waitReady()
			dexec(tc.seed)
			if got := strings.TrimSpace(dexec(tc.check)); got != tc.want {
				t.Fatalf("seeded fingerprint = %q, want %q", got, tc.want)
			}

			client := NewClient(exec, os.Stdout)
			image, env, err := client.InspectAccessory(ctx, app, "db")
			if err != nil {
				t.Fatalf("InspectAccessory: %v", err)
			}
			start := time.Now()
			res, err := client.AccessoryBackup(ctx, app, "db", image, env, s3)
			if err != nil {
				t.Fatalf("AccessoryBackup: %v", err)
			}
			t.Logf("backup %s key=%s size=%d in %s", res.ID, res.Key, res.SizeBytes, time.Since(start).Round(time.Millisecond))

			// verify-backup into a scratch container while the accessory is
			// stopped (one running container at a time).
			run("docker stop " + ssh.ShellQuote(container) + " >/dev/null")
			volumesBefore := strings.TrimSpace(run("docker volume ls -q | wc -l"))
			vr, err := client.VerifyBackup(ctx, app, "db", "", s3)
			if err != nil {
				t.Fatalf("VerifyBackup: %v", err)
			}
			if after := strings.TrimSpace(run("docker volume ls -q | wc -l")); after != volumesBefore {
				t.Fatalf("verify left a volume behind (%s -> %s): an unencrypted copy of the database on the host", volumesBefore, after)
			}
			t.Logf("verify: kind=%s metric=%s ok=%v detail=%s", vr.Kind, vr.Metric, vr.OK, vr.Detail)
			if !vr.OK || vr.Kind != tc.wantKind {
				t.Fatalf("verify = ok:%v kind:%s detail:%q, want ok kind %s", vr.OK, vr.Kind, vr.Detail, tc.wantKind)
			}
			// A deliberately corrupted object (truncated to half) must fail
			// verification, never pass on "something extracted".
			obj := work + "/s3/" + strings.TrimPrefix(res.Key, "s3://")
			badID := strings.Replace(res.ID, res.ID[:15], "20000101-000000", 1)
			badObj := strings.Replace(obj, res.ID, badID, 1)
			run(fmt.Sprintf("head -c $(( $(wc -c < %s) / 2 )) %s > %s", ssh.ShellQuote(obj), ssh.ShellQuote(obj), ssh.ShellQuote(badObj)))
			bad, err := client.VerifyBackup(ctx, app, "db", badID, s3)
			if err != nil {
				t.Fatalf("VerifyBackup(corrupt): %v", err)
			}
			if bad.OK {
				t.Fatalf("a truncated backup object verified OK (metric %s) — false green", bad.Metric)
			}
			t.Logf("corrupt object correctly failed verification: %s", bad.Detail)
			run("rm -f " + ssh.ShellQuote(badObj))

			run("docker start " + ssh.ShellQuote(container) + " >/dev/null")
			waitReady()

			if tc.key == "mysql" {
				// Wrong credentials must fail the backup, not store an
				// empty dump (the deploy-ovh 20-byte files).
				wrong := map[string]string{}
				for k, v := range env {
					wrong[k] = v
				}
				wrong["MYSQL_ROOT_PASSWORD"] = "not-the-password"
				if r, err := client.AccessoryBackup(ctx, app, "db", image, wrong, s3); err == nil {
					t.Fatalf("backup with a wrong password succeeded (%d bytes at %s)", r.SizeBytes, r.Key)
				} else {
					t.Logf("wrong-password backup correctly refused: %v", err)
				}
			}

			dexec(tc.mutate)
			if got := strings.TrimSpace(dexec(tc.check)); got == tc.want {
				t.Fatalf("mutation did not change the fingerprint (%q)", got)
			}

			if err := client.AccessoryRestore(ctx, app, "db", image, res.ID, env, s3); err != nil {
				t.Fatalf("in-place AccessoryRestore into the populated database: %v", err)
			}
			waitReady()
			if got := strings.TrimSpace(dexec(tc.check)); got != tc.want {
				t.Fatalf("after restore fingerprint = %q, want the backed-up %q", got, tc.want)
			}

			if tc.key == "postgres" {
				// A legacy plain .sql.gz backup (what every CLI before G7
				// wrote) still restores in place into a populated database.
				legacyID := strings.Replace(res.ID, res.ID[:15], "20000102-000000", 1)
				legacyObj := strings.Replace(work+"/s3/"+strings.TrimPrefix(res.Key, "s3://"), res.ID+".pgdump", legacyID+".sql.gz", 1)
				run(fmt.Sprintf("docker exec %s pg_dump -U app appdb > %s.sql && gzip -c %s.sql > %s && rm -f %s.sql",
					ssh.ShellQuote(container), ssh.ShellQuote(legacyObj), ssh.ShellQuote(legacyObj), ssh.ShellQuote(legacyObj), ssh.ShellQuote(legacyObj)))
				dexec(tc.mutate)
				if err := client.AccessoryRestore(ctx, app, "db", image, legacyID, env, s3); err != nil {
					t.Fatalf("in-place restore of a legacy plain backup: %v", err)
				}
				if got := strings.TrimSpace(dexec(tc.check)); got != tc.want {
					t.Fatalf("after legacy restore fingerprint = %q, want %q", got, tc.want)
				}
				vr, err := client.VerifyBackup(ctx, app, "db", legacyID, s3)
				if err != nil || !vr.OK {
					t.Fatalf("verify of the legacy plain backup: %v %+v", err, vr)
				}
			}
		})
	}
}
