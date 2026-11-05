//go:build integration

// End-to-end verification of the fleet backup engine (Slice 4): the real linux
// binary on the colima SSH+Docker fixture, real engines, a real S3-compatible
// store (MinIO, driven by the real aws CLI, never the fake one), real age. The
// VM needs aws, age and a running cron, and a user-owned /deployments. MinIO is
// cgr.dev/chainguard/minio: the official images are no longer publicly pullable.
//
//	TEPLOY_FAULT_HOST=127.0.0.1:<colima ssh port> TEPLOY_FAULT_USER=tyler \
//	TEPLOY_FAULT_KEY=~/.colima/_lima/_config/user \
//	go test -tags integration -p 2 -run TestFleetIntegration -v -timeout 80m ./internal/cli
//
// Containers are capped and removed on exit.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy/internal/backup"
	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/ssh"
)

const (
	itDir       = "/tmp/teploy-fleet-it"
	itBin       = itDir + "/teploy"
	itShims     = itDir + "/shims"
	itMinioPort = "19100"
	itHookPort  = "19200"
	itEndpoint  = "http://127.0.0.1:" + itMinioPort
	itAK        = "teployitaccess"
	itSK        = "teployitsecret1234567890"
	itDBPW      = "fixture-db-password-XYZ"
	itToken     = "SECRETHEARTBEATTOKEN"
)

type fleetIT struct {
	t          *testing.T
	exec       ssh.Executor
	ctx        context.Context
	s3         backup.S3Config
	owner      string // owner identity file on the VM
	ownerLocal string // the same key on this machine, for --identity
	pub        string // owner recipient
	local      string // darwin/linux binary for the --host commands
	host       string
	user       string
	key        string
}

func (f *fleetIT) sh(cmd string) string {
	f.t.Helper()
	out, err := f.exec.Run(f.ctx, cmd)
	if err != nil {
		f.t.Fatalf("command failed: %s\nerror: %v\noutput: %s", cmd, err, out)
	}
	return out
}

// awsIT runs the real aws CLI with the fixture's credentials.
func (f *fleetIT) awsIT(args string) string {
	f.t.Helper()
	return f.sh("AWS_ACCESS_KEY_ID=" + itAK + " AWS_SECRET_ACCESS_KEY=" + itSK + " aws --region us-east-1 --endpoint-url " + itEndpoint + " " + args)
}

// keys lists the bucket keys under prefix, sorted.
func (f *fleetIT) keys(prefix string) []string {
	f.t.Helper()
	res := ssh.RunDetailed(f.ctx, f.exec, "AWS_ACCESS_KEY_ID="+itAK+" AWS_SECRET_ACCESS_KEY="+itSK+" aws --region us-east-1 --endpoint-url "+itEndpoint+" s3 ls --recursive "+ssh.ShellQuote("s3://itfleet/"+prefix))
	var out []string
	for _, line := range strings.Split(string(res.Stdout), "\n") {
		if fs := strings.Fields(line); len(fs) >= 4 {
			out = append(out, fs[3])
		}
	}
	sort.Strings(out)
	return out
}

// runAll runs `teploy backup run --all --json` on the VM. PATH may be shimmed.
func (f *fleetIT) runAll(env string) (backupRunEnvelope, int, string) {
	f.t.Helper()
	res := ssh.RunDetailedLimit(f.ctx, f.exec, env+" "+itBin+" backup run --all --json", 16<<20)
	if res.Err != nil {
		f.t.Fatalf("backup run --all: %v", res.Err)
	}
	var env2 backupRunEnvelope
	if err := json.Unmarshal(lastJSONLine(res.Stdout), &env2); err != nil {
		f.t.Fatalf("parsing the run envelope: %v\nstdout: %s\nstderr: %s", err, res.Stdout, res.Stderr)
	}
	return env2, res.ExitCode, string(res.Stdout) + string(res.Stderr)
}

func lastJSONLine(b []byte) []byte {
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	return []byte(lines[len(lines)-1])
}

func (f *fleetIT) target(r backupRunEnvelope, container string) backup.FleetTargetResult {
	f.t.Helper()
	for _, t := range r.Targets {
		if t.Container == container || (container == t.App+"/volumes" && t.Kind == backup.TargetVolumes) {
			return t
		}
	}
	f.t.Fatalf("no target %q in %+v", container, r.Targets)
	return backup.FleetTargetResult{}
}

// cli runs the local binary against the VM (the --host commands). stdin optional.
func (f *fleetIT) cli(stdin string, args ...string) (string, error) {
	f.t.Helper()
	all := append([]string{"--host", f.host, "--user", f.user, "--key", f.key}, args...)
	c := exec.CommandContext(f.ctx, f.local, all...)
	c.Env = append(os.Environ(), "TEPLOY_SERVER_BINARY="+itBin)
	if stdin != "" {
		c.Stdin = strings.NewReader(stdin)
	}
	out, err := c.CombinedOutput()
	return string(out), err
}

func (f *fleetIT) setPolicy(p backup.FleetPolicy) {
	f.t.Helper()
	if _, _, err := backup.WriteFleetPolicy(f.ctx, f.exec, p); err != nil {
		f.t.Fatalf("writing the policy: %v", err)
	}
}

func (f *fleetIT) psql(container, db, sql string) string {
	f.t.Helper()
	return strings.TrimSpace(f.sh("docker exec " + container + " psql -X -qtA -v ON_ERROR_STOP=1 -U appuser -d " + db + " -c " + ssh.ShellQuote(sql)))
}

func (f *fleetIT) mysql(sql string) string {
	f.t.Helper()
	return strings.TrimSpace(f.sh("docker exec -e MYSQL_PWD=" + itDBPW + " wordpress-shared-mysql mysql -u root -N -B -e " + ssh.ShellQuote(sql)))
}

func (f *fleetIT) waitFor(what, cmd string) {
	f.t.Helper()
	for i := 0; i < 120; i++ {
		if _, err := f.exec.Run(f.ctx, cmd); err == nil {
			return
		}
		time.Sleep(time.Second)
	}
	f.t.Fatalf("timed out waiting for %s", what)
}

func TestFleetIntegration(t *testing.T) {
	host, user, keyPath := os.Getenv("TEPLOY_FAULT_HOST"), os.Getenv("TEPLOY_FAULT_USER"), os.Getenv("TEPLOY_FAULT_KEY")
	if host == "" || user == "" || keyPath == "" {
		t.Skip("needs TEPLOY_FAULT_HOST, TEPLOY_FAULT_USER, TEPLOY_FAULT_KEY")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 70*time.Minute)
	defer cancel()
	cctx, ccancel := context.WithTimeout(ctx, 10*time.Second)
	defer ccancel()
	vm, err := ssh.Connect(cctx, ssh.ConnectConfig{Host: host, User: user, KeyPath: keyPath})
	if err != nil {
		t.Fatalf("connecting fixture: %v", err)
	}
	t.Cleanup(func() { vm.Close() })
	f := &fleetIT{t: t, exec: vm, ctx: ctx, host: host, user: user, key: keyPath}

	containers := []string{"teploy-it-minio", "teploy-it-minio2", "teployit-db", "teployit-cache", "teployit2-db", "teployit3-db", "wordpress-shared-mysql", "omni_postgres", "teploy-it-hook", "teploy-it-scratch"}
	cleanup := func() {
		c := context.Background()
		vm.Run(c, "docker rm -fv "+strings.Join(containers, " ")+" >/dev/null 2>&1; docker ps -aq --filter name=teploy-backup-verify- | xargs -r docker rm -fv >/dev/null 2>&1; pkill -f teploy-it-hook.py; pkill -9 -f '[s]leep 600'; true")
		vm.Run(c, "rm -rf "+itDir+" /deployments/teployit /deployments/teployit2 /deployments/teployit3 /deployments/_adopted /deployments/.teploy/apps/teployit /deployments/.teploy/apps/teployit2 /deployments/.teploy/apps/teployit3 /deployments/.teploy/apps/_adopted /deployments/.teploy/apps/_fleet /var/tmp/teploy-backup.* /var/tmp/teploy-pre-restore.* /var/tmp/teploy-verify.* /var/tmp/teploy-restore.* /var/tmp/teploy-dr /tmp/teploy-pwned; crontab -r 2>/dev/null; sudo -n rm -rf /etc/teploy/backup-policy.json; sudo -n chmod 755 /etc/teploy 2>/dev/null; sudo -n rmdir /etc/teploy 2>/dev/null; sudo -n userdel -rf teployit 2>/dev/null; rm -f \"$HOME\"/.config/teploy/backup-policy.json \"$HOME\"/.config/teploy/backup-target.json \"$HOME\"/.config/teploy/backup-run.lock; true")
	}
	cleanup()
	t.Cleanup(cleanup)

	// Binaries: linux for the VM, this platform for the --host commands.
	tmp := t.TempDir()
	root, _ := filepath.Abs("../..")
	build := func(goos, goarch, out string) {
		c := exec.Command("go", "build", "-o", out, "./cmd/teploy")
		c.Dir = root
		c.Env = append(os.Environ(), "GOOS="+goos, "GOARCH="+goarch, "CGO_ENABLED=0")
		if b, err := c.CombinedOutput(); err != nil {
			t.Fatalf("building %s/%s: %v\n%s", goos, goarch, err, b)
		}
	}
	arch := strings.TrimSpace(f.sh("uname -m"))
	goarch := "arm64"
	if arch == "x86_64" {
		goarch = "amd64"
	}
	build("linux", goarch, filepath.Join(tmp, "teploy-linux"))
	f.local = filepath.Join(tmp, "teploy-local")
	build(runtime.GOOS, runtime.GOARCH, f.local)
	f.sh("mkdir -p " + itShims)
	body, err := os.ReadFile(filepath.Join(tmp, "teploy-linux"))
	if err != nil {
		t.Fatal(err)
	}
	if err := vm.Upload(ctx, strings.NewReader(string(body)), itBin, "0755"); err != nil {
		t.Fatalf("uploading the binary: %v", err)
	}

	// MinIO, the owner's recovery key, the stored target.
	f.sh("docker run -d --name teploy-it-minio --memory 512m -p 127.0.0.1:" + itMinioPort + ":9000 -e MINIO_ROOT_USER=" + itAK + " -e MINIO_ROOT_PASSWORD=" + itSK + " cgr.dev/chainguard/minio:latest server /tmp/data >/dev/null")
	f.waitFor("minio", "curl -fs "+itEndpoint+"/minio/health/live")
	f.awsIT("s3 mb s3://itfleet")
	keygen := func(file string) string {
		c := exec.Command(f.local, "backup", "keygen")
		b, err := c.Output()
		if err != nil {
			t.Fatalf("keygen: %v", err)
		}
		if err := vm.Upload(ctx, strings.NewReader(string(b)), file, "0600"); err != nil {
			t.Fatal(err)
		}
		f.ownerLocal = filepath.Join(tmp, "owner.txt")
		if err := os.WriteFile(f.ownerLocal, b, 0o600); err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(b), "\n") {
			if v, ok := strings.CutPrefix(line, "# public key: "); ok {
				return v
			}
		}
		t.Fatalf("keygen printed no public key: %s", b)
		return ""
	}
	f.owner = itDir + "/owner.txt"
	f.pub = keygen(f.owner)
	armor, err := backup.EncryptCredentials(ctx, vm, itAK, itSK)
	if err != nil {
		t.Fatalf("encrypting credentials: %v", err)
	}
	f.s3 = backup.S3Config{Bucket: "itfleet", Region: "us-east-1", Endpoint: itEndpoint}
	if _, _, err := backup.WriteTarget(ctx, vm, backup.Target{Bucket: "itfleet", Region: "us-east-1", Endpoint: itEndpoint}, armor); err != nil {
		t.Fatalf("writing the target: %v", err)
	}
	target, err := backup.ReadTarget(ctx, vm)
	if err != nil {
		t.Fatal(err)
	}
	f.s3 = TargetS3(target)

	// A heartbeat receiver that records what it was asked for.
	hook := `import http.server
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        code = 500 if "broken" in self.path else 200
        open("` + itDir + `/hook.log","a").write(self.path+"\n")
        self.send_response(code); self.end_headers()
    def log_message(self,*a): pass
http.server.HTTPServer(("127.0.0.1",` + itHookPort + `),H).serve_forever()
`
	if err := vm.Upload(ctx, strings.NewReader(hook), itDir+"/teploy-it-hook.py", "0644"); err != nil {
		t.Fatal(err)
	}
	f.sh("setsid nohup python3 " + itDir + "/teploy-it-hook.py >/dev/null 2>&1 < /dev/null & true")
	f.waitFor("hook", "curl -fs http://127.0.0.1:"+itHookPort+"/ready")

	// The fleet: a labelled postgres, a labelled redis, an adopted multi-schema
	// mysql holding hostile schema names, an unmanaged postgres nothing adopts,
	// and one app's volumes with an .env.
	f.sh("docker run -d --name teployit-db --memory 256m --label teploy.role=accessory --label teploy.app=teployit --label teploy.accessory=db -e POSTGRES_DB=appdb -e POSTGRES_USER=appuser -e POSTGRES_PASSWORD=" + itDBPW + " postgres:17-alpine >/dev/null")
	f.waitFor("postgres", "docker exec teployit-db pg_isready -h 127.0.0.1 -U appuser")
	f.psql("teployit-db", "appdb", "CREATE TABLE t(id int); INSERT INTO t VALUES (1),(2),(3)")
	f.sh("docker exec teployit-db psql -X -q -U appuser -d postgres -c 'CREATE DATABASE reports'")
	f.psql("teployit-db", "reports", "CREATE TABLE r(id int); INSERT INTO r VALUES (7)")
	f.sh("docker run -d --name teployit-cache --memory 64m --label teploy.role=accessory --label teploy.app=teployit --label teploy.accessory=cache redis:7-alpine >/dev/null")
	f.waitFor("redis", "docker exec teployit-cache redis-cli ping")
	f.sh("docker exec teployit-cache redis-cli set k v >/dev/null")
	f.sh("docker run -d --name wordpress-shared-mysql --memory 768m -e MYSQL_ROOT_PASSWORD=" + itDBPW + " mysql:8 >/dev/null")
	f.waitFor("mysql", "docker exec -e MYSQL_PWD="+itDBPW+" wordpress-shared-mysql mysql --protocol=TCP -h 127.0.0.1 -u root -e 'SELECT 1'")
	hostile := "wp_evil'; touch /tmp/teploy-pwned; '"
	spaced := "wp space $(touch /tmp/teploy-pwned)"
	for _, s := range []string{"wp_a", "wp_b", hostile, spaced} {
		ident := "`" + s + "`"
		f.mysql("CREATE DATABASE " + ident + "; CREATE TABLE " + ident + ".posts(id int); INSERT INTO " + ident + ".posts VALUES (1),(2)")
	}
	f.sh("mkdir -p /deployments/teployit/volumes/data && printf marker-v1 > /deployments/teployit/volumes/data/marker.txt && printf 'SECRET_ENV=plain-env-value\\n' > /deployments/teployit/.env")

	retention := backup.FleetRetention{KeepLast: 5}
	base := backup.FleetPolicy{
		V: "1", Interval: "1h", Retention: retention, Volumes: true,
		Adopted:       []backup.AdoptedEntry{{Container: "wordpress-shared-mysql", Engine: "mysql", Databases: backup.DatabaseSelector{All: true}}},
		HeartbeatURLs: []string{"http://127.0.0.1:" + itHookPort + "/api/push/" + itToken},
	}

	var legacyID string
	t.Run("no recovery key: plaintext, encrypted false, .env left out", func(t *testing.T) {
		f.t = t
		f.setPolicy(base)
		res, code, out := f.runAll("")
		if code != 0 || res.Status != backup.RunOK {
			t.Fatalf("exit %d status %s: %+v\n%s", code, res.Status, res.Targets, out)
		}
		if res.Encrypted {
			t.Fatal("encrypted must be false with no recipient")
		}
		db := f.target(res, "teployit-db")
		legacyID = db.ID
		if db.Encrypted || !strings.HasSuffix(db.Objects[len(db.Objects)-1].Key, ".pgdump") {
			t.Fatalf("db object: %+v", db)
		}
		vol := f.target(res, "teployit/volumes")
		if len(vol.Warnings) == 0 || !strings.Contains(vol.Warnings[0], ".env was left out") {
			t.Fatalf("no .env warning: %+v", vol)
		}
		f.sh("mkdir -p " + itDir + "/v && AWS_ACCESS_KEY_ID=" + itAK + " AWS_SECRET_ACCESS_KEY=" + itSK + " aws --region us-east-1 --endpoint-url " + itEndpoint + " s3 cp " + vol.Objects[0].Key + " " + itDir + "/v/a.tar.gz --only-show-errors")
		if strings.Contains(f.sh("tar -tzf "+itDir+"/v/a.tar.gz"), ".env") {
			t.Fatal("an unencrypted volume archive contains .env")
		}
	})

	var firstAll backupRunEnvelope
	t.Run("mixed engines, adopted multi-schema, hostile names, everything encrypted", func(t *testing.T) {
		f.t = t
		p := base
		p.Recipient = f.pub
		f.setPolicy(p)
		res, code, out := f.runAll("")
		firstAll = res
		if code != 0 || res.Status != backup.RunOK {
			t.Fatalf("exit %d status %s: %+v\n%s", code, res.Status, res.Targets, out)
		}
		if !res.Encrypted {
			t.Fatal("encrypted must be true")
		}
		for _, name := range []string{"teployit-db", "teployit-cache", "wordpress-shared-mysql", "teployit/volumes"} {
			tr := f.target(res, name)
			if tr.Status != backup.TargetOK || !tr.Encrypted || len(tr.Objects) == 0 {
				t.Fatalf("%s: %+v", name, tr)
			}
			for _, o := range tr.Objects {
				if !strings.HasSuffix(o.Key, ".age") {
					t.Fatalf("%s stored plaintext: %s", name, o.Key)
				}
			}
		}
		// One object per schema, so one site restores alone.
		mysql := f.target(res, "wordpress-shared-mysql")
		dbs := map[string]bool{}
		for _, o := range mysql.Objects {
			dbs[o.Database] = true
		}
		for _, want := range []string{"wp_a", "wp_b", spaced} {
			if !dbs[want] {
				t.Fatalf("schema %q has no object: %+v", want, mysql.Objects)
			}
		}
		// mysqldump cannot address a quoted name: never a silent success.
		if dbs[hostile] || !strings.Contains(strings.Join(res.Omissions, "\n"), hostile) {
			t.Fatalf("a schema mysqldump cannot address must be named as not backed up: %+v", res.Omissions)
		}
		// Every sidecar is encrypted too.
		pgKeys := f.keys("teployit/accessories/db/")
		for _, k := range pgKeys {
			if strings.Contains(k, firstAll.Targets[0].ID) && !strings.HasSuffix(k, ".age") {
				t.Fatalf("plaintext sidecar: %s", k)
			}
		}
		for _, want := range []string{"/globals/", "/meta/", "/databases/reports/"} {
			found := false
			for _, k := range pgKeys {
				found = found || (strings.Contains(k, want) && strings.HasSuffix(k, ".age"))
			}
			if !found {
				t.Fatalf("no encrypted %s object: %v", want, pgKeys)
			}
		}
		// Hostile names went through quoting, not through a shell.
		if strings.TrimSpace(f.sh("if [ -e /tmp/teploy-pwned ]; then echo yes; fi")) != "" {
			t.Fatal("a hostile schema name executed a command")
		}
		// omni_postgres does not exist yet, so it is not in the uncovered list.
		for _, c := range res.UncoveredContainers {
			if c == "omni_postgres" || c == "teployit-db" || c == "wordpress-shared-mysql" {
				t.Fatalf("uncovered: %v", res.UncoveredContainers)
			}
		}
		// Heartbeat: up, with no status=down.
		log := f.sh("cat " + itDir + "/hook.log")
		last := lastLine(log)
		if !strings.Contains(last, "/api/push/"+itToken) || strings.Contains(last, "status=down") {
			t.Fatalf("heartbeat: %q", log)
		}
		// No credential or token in anything a person reads.
		for _, secretValue := range []string{itToken, itDBPW, itSK} {
			if strings.Contains(out, secretValue) {
				t.Fatalf("output leaks %q", secretValue)
			}
		}
		events := f.sh("cat /deployments/.teploy/apps/_fleet/events.jsonl /deployments/.teploy/apps/_adopted/events.jsonl /deployments/.teploy/apps/teployit/events.jsonl")
		for _, secretValue := range []string{itToken, itDBPW, itSK} {
			if strings.Contains(events, secretValue) {
				t.Fatalf("events leak %q", secretValue)
			}
		}
	})

	t.Run("new database appearing between runs is covered with no command", func(t *testing.T) {
		f.t = t
		f.sh("docker run -d --name teployit2-db --memory 128m --label teploy.role=accessory --label teploy.app=teployit2 --label teploy.accessory=db -e POSTGRES_DB=two -e POSTGRES_USER=appuser -e POSTGRES_PASSWORD=" + itDBPW + " postgres:17-alpine >/dev/null")
		f.waitFor("second postgres", "docker exec teployit2-db pg_isready -h 127.0.0.1 -U appuser")
		f.psql("teployit2-db", "two", "CREATE TABLE z(id int)")
		f.mysql("CREATE DATABASE wp_new; CREATE TABLE wp_new.posts(id int)")
		f.sh("docker exec teployit-db psql -X -q -U appuser -d postgres -c 'CREATE DATABASE fresh'")
		f.psql("teployit-db", "fresh", "CREATE TABLE f(id int)")
		res, code, out := f.runAll("")
		if code != 0 {
			t.Fatalf("exit %d: %s", code, out)
		}
		if tr := f.target(res, "teployit2-db"); tr.Status != backup.TargetOK {
			t.Fatalf("new container: %+v", tr)
		}
		if len(f.keys("_adopted/accessories/wordpress-shared-mysql/databases/wp_new/")) == 0 {
			t.Fatal("a new schema has no object")
		}
		if len(f.keys("teployit/accessories/db/databases/fresh/")) == 0 {
			t.Fatal("a new database in a labelled accessory has no object")
		}
		// Containers are gone next run: not backed up, not an error for a labelled one.
		f.sh("docker rm -fv teployit2-db >/dev/null")
	})

	t.Run("one schema restores alone; other schemas untouched; safety backup taken", func(t *testing.T) {
		f.t = t
		id := ""
		for _, o := range f.target(firstAll, "wordpress-shared-mysql").Objects {
			if o.Database == "wp_a" {
				parts := strings.Split(o.Key, "/")
				id = strings.Split(parts[len(parts)-1], ".")[0]
			}
		}
		f.mysql("INSERT INTO wp_a.posts VALUES (99); INSERT INTO wp_b.posts VALUES (99)")
		out, err := f.cli("", "backup", "restore", id, "--adopted", "--accessory", "wordpress-shared-mysql", "--database", "wp_a", "--yes", "--json")
		if err != nil {
			t.Fatalf("restore: %v\n%s", err, out)
		}
		var dto backupRestoreDTO
		if err := json.Unmarshal(lastJSONLine([]byte(out)), &dto); err != nil {
			t.Fatalf("restore JSON: %v\n%s", err, out)
		}
		if !dto.OK || dto.PreBackupID == "" {
			t.Fatalf("restore: %+v", dto)
		}
		if got := f.mysql("SELECT COUNT(*) FROM wp_a.posts"); got != "2" {
			t.Fatalf("wp_a rows = %s after restore", got)
		}
		if got := f.mysql("SELECT COUNT(*) FROM wp_b.posts"); got != "3" {
			t.Fatalf("wp_b was touched: %s rows", got)
		}
		// The pre-restore safety backup holds the row the restore just removed.
		if len(f.keys("_adopted/accessories/wordpress-shared-mysql/databases/wp_a/"+dto.PreBackupID)) == 0 {
			t.Fatalf("no pre-restore backup %s", dto.PreBackupID)
		}
		// A hostile schema through the same path, quoting and all.
		var hostileID string
		for _, o := range f.target(firstAll, "wordpress-shared-mysql").Objects {
			if o.Database == spaced {
				parts := strings.Split(o.Key, "/")
				hostileID = strings.Split(parts[len(parts)-1], ".")[0]
			}
		}
		f.mysql("INSERT INTO `" + spaced + "`.posts VALUES (99)")
		out, err = f.cli("", "backup", "restore", hostileID, "--adopted", "--accessory", "wordpress-shared-mysql", "--database", spaced, "--yes", "--no-backup", "--json")
		if err != nil {
			t.Fatalf("hostile restore: %v\n%s", err, out)
		}
		if got := f.mysql("SELECT COUNT(*) FROM `" + spaced + "`.posts"); got != "2" {
			t.Fatalf("hostile schema rows = %s after restore", got)
		}
		if strings.TrimSpace(f.sh("if [ -e /tmp/teploy-pwned ]; then echo yes; fi")) != "" {
			t.Fatal("a hostile schema name executed a command during restore")
		}
	})

	t.Run("verify runs on the server with its own identity; corrupt object fails", func(t *testing.T) {
		f.t = t
		out, err := f.cli("", "--json", "accessory", "verify-backup", "wordpress-shared-mysql", "--adopted", "--database", "wp_a")
		if err != nil {
			t.Fatalf("verify: %v\n%s", err, out)
		}
		if !strings.Contains(out, `"ok": true`) || !strings.Contains(out, `"encrypted": true`) || !strings.Contains(out, "tables=1") {
			t.Fatalf("verify result: %s", out)
		}
		// Corrupt the newest object of wp_b: verification must fail, loudly.
		keys := f.keys("_adopted/accessories/wordpress-shared-mysql/databases/wp_b/")
		var newest string
		for _, k := range keys {
			if strings.HasSuffix(k, ".age") && !strings.Contains(k, "/meta/") {
				newest = k
			}
		}
		f.sh("head -c 2000 /dev/urandom > " + itDir + "/garbage")
		f.awsIT("s3 cp " + itDir + "/garbage s3://itfleet/" + newest + " --only-show-errors")
		out, err = f.cli("", "--json", "accessory", "verify-backup", "wordpress-shared-mysql", "--adopted", "--database", "wp_b")
		if err == nil || !strings.Contains(out, `"ok": false`) {
			t.Fatalf("a corrupted object must fail verification: err=%v\n%s", err, out)
		}
		f.awsIT("s3 rm s3://itfleet/" + newest + " --only-show-errors")
	})

	t.Run("legacy unencrypted object restores after encryption is on", func(t *testing.T) {
		f.t = t
		f.psql("teployit-db", "appdb", "INSERT INTO t VALUES (100)")
		out, err := f.cli("", "backup", "restore", legacyID, "--adopted=false", "--app", "teployit", "--accessory", "db", "--yes", "--json")
		if err != nil {
			t.Fatalf("restore: %v\n%s", err, out)
		}
		if got := f.psql("teployit-db", "appdb", "SELECT COUNT(*) FROM t"); got != "3" {
			t.Fatalf("rows = %s", got)
		}
	})

	t.Run("recovery on a clean host with only the owner identity", func(t *testing.T) {
		f.t = t
		var key string
		for _, o := range f.target(firstAll, "teployit-db").Objects {
			if o.Database == "" {
				key = o.Key
			}
		}
		clean := itDir + "/clean"
		f.sh("mkdir -p " + clean)
		// A wrong identity is refused and leaves nothing behind.
		wrong := itDir + "/wrong.txt"
		if b, err := exec.Command(f.local, "backup", "keygen").Output(); err != nil {
			t.Fatal(err)
		} else if err := vm.Upload(ctx, strings.NewReader(string(b)), wrong, "0600"); err != nil {
			t.Fatal(err)
		}
		cleanEnv := "cd " + clean + " && env -i PATH=/usr/local/bin:/usr/bin:/bin HOME=" + clean + " TEPLOY_S3_ACCESS_KEY=" + itAK + " TEPLOY_S3_SECRET_KEY=" + itSK + " "
		if res := ssh.RunDetailed(ctx, vm, cleanEnv+itBin+" backup fetch "+ssh.ShellQuote(key)+" --identity "+wrong+" --endpoint "+itEndpoint); res.ExitCode == 0 {
			t.Fatalf("a wrong identity decrypted: %s", res.Stdout)
		}
		if left := strings.TrimSpace(f.sh("ls -A " + clean + " | grep -v '^.aws$'; true")); left != "" {
			t.Fatalf("a failed fetch left files: %s", left)
		}
		// The owner's own key, no server identity, no policy, no target.
		res := ssh.RunDetailed(ctx, vm, cleanEnv+itBin+" backup fetch "+ssh.ShellQuote(key)+" --identity "+f.owner+" --endpoint "+itEndpoint+" -o restored.pgdump")
		if res.Failed() {
			t.Fatalf("fetch: %s %s", res.Stdout, res.Stderr)
		}
		f.sh("docker run -d --name teploy-it-scratch --memory 256m -e POSTGRES_USER=appuser -e POSTGRES_PASSWORD=x -e POSTGRES_DB=appdb postgres:17-alpine >/dev/null")
		f.waitFor("scratch", "docker exec teploy-it-scratch pg_isready -h 127.0.0.1 -U appuser")
		f.sh("docker exec -i teploy-it-scratch pg_restore -U appuser -d appdb < " + clean + "/restored.pgdump")
		if got := f.psql("teploy-it-scratch", "appdb", "SELECT COUNT(*) FROM t"); got != "3" {
			t.Fatalf("recovered rows = %s", got)
		}
		f.sh("docker rm -fv teploy-it-scratch >/dev/null")
	})

	t.Run("killed mid-upload, then killed before publishing, then a good run", func(t *testing.T) {
		f.t = t
		realAWS := strings.TrimSpace(f.sh("command -v aws"))
		// A shim: the first .incoming upload hangs, as a slow link would.
		shim := "#!/bin/sh\ncase \"$*\" in *.incoming*) case \"$*\" in *--metadata*) touch " + itDir + "/uploading; sleep 600;; esac;; esac\nexec " + realAWS + " \"$@\"\n"
		if err := vm.Upload(ctx, strings.NewReader(shim), itShims+"/aws", "0755"); err != nil {
			t.Fatal(err)
		}
		before2 := f.keys("teployit/accessories/db/")
		startKilled := func(marker string) {
			f.sh("rm -f " + marker + "; PATH=" + itShims + ":$PATH setsid nohup " + itBin + " backup run --all > " + itDir + "/killed.log 2>&1 < /dev/null & echo $! > " + itDir + "/pid")
			f.waitFor("the run to reach its upload", "test -e "+marker)
		}
		startKilled(itDir + "/uploading")
		// Overlap: a second run stands down while the first holds the lock.
		res, code, _ := f.runAll("")
		if code != 0 || res.Status != "skipped" {
			t.Fatalf("overlap: exit %d status %s", code, res.Status)
		}
		f.sh("kill -9 -- -$(cat " + itDir + "/pid); sleep 1; true")
		// Nothing a later run could misread: no final object appeared, the lock
		// died with the process, the workspace is a visible leftover.
		if got := f.keys("teployit/accessories/db/"); strings.Join(got, ",") != strings.Join(before2, ",") {
			t.Fatalf("a killed run changed the final objects:\nbefore %v\nafter %v", before2, got)
		}
		// A partial object left at the verification-pending key, as a provider
		// with weaker semantics or a future bug might: never a backup.
		partial := ".incoming/teployit/accessories/db/29991231-235959-0123456789abcdef.pgdump.age"
		f.sh("printf partial > " + itDir + "/partial")
		f.awsIT("s3 cp " + itDir + "/partial s3://itfleet/" + partial + " --only-show-errors")
		// A truncated object at a FINAL key is also refused by the floor.
		bad := "teployit/accessories/db/29991231-235959-fedcba9876543210.pgdump.age"
		f.awsIT("s3 cp " + itDir + "/partial s3://itfleet/" + bad + " --only-show-errors")

		// Kill it again after the upload finished, before publication.
		shim2 := "#!/bin/sh\ncase \"$*\" in *'s3 cp s3://'*.incoming*s3://*) touch " + itDir + "/publishing; sleep 600;; esac\nexec " + realAWS + " \"$@\"\n"
		if err := vm.Upload(ctx, strings.NewReader(shim2), itShims+"/aws", "0755"); err != nil {
			t.Fatal(err)
		}
		startKilled(itDir + "/publishing")
		f.sh("kill -9 -- -$(cat " + itDir + "/pid); sleep 1; true")

		// The next run needs no cleanup and takes no misreading from any of it.
		res, code, out := f.runAll("")
		if code != 0 || res.Status != backup.RunOK {
			t.Fatalf("the run after the kills: exit %d %s: %s", code, res.Status, out)
		}
		good := f.target(res, "teployit-db")
		rows, err := backup.NewClient(vm, os.Stderr).ListBackupsRecursive(ctx, "teployit", f.s3)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			if r.Accessory == "db" && r.Database == "" && r.ID == "29991231-235959-fedcba9876543210" && r.Restorable {
				t.Fatal("a below-floor object lists as restorable")
			}
		}
		id, _, err := backup.NewClient(vm, os.Stderr).LatestBackupDate(ctx, "teployit", "db", f.s3)
		if err != nil {
			t.Fatal(err)
		}
		if id != good.ID {
			t.Fatalf("latest = %s, want the good run's %s: a partial upload counted", id, good.ID)
		}
		f.awsIT("s3 rm s3://itfleet/" + bad + " --only-show-errors")
		f.awsIT("s3 rm s3://itfleet/" + partial + " --only-show-errors")
		// An external kill -9 cannot reach the runs' own process groups: clear what
		// those two left, so what follows measures the stale-holder path alone.
		f.sh("pkill -9 -f '[s]leep 600'; true")
		// A holder alive and stuck past every deadline it had: the next run ends it.
		startKilled(itDir + "/publishing")
		holderPID := strings.TrimSpace(f.sh("cat " + itDir + "/pid"))
		f.sh(`printf '{"pid":` + holderPID + `,"started_at":"2020-01-01T00:00:00Z"}' > "$HOME"/.config/teploy/backup-run.lock`)
		res, code, out = f.runAll("")
		if code != 0 || res.Status != backup.RunOK {
			t.Fatalf("a stuck holder must not block forever: exit %d %s: %s", code, res.Status, out)
		}
		if alive := ssh.RunDetailed(ctx, vm, "kill -0 "+holderPID); !alive.Failed() {
			t.Fatalf("the stuck holder %s is still alive", holderPID)
		}
		// Its upload ran in a process group of its own: that must not survive
		// into the next run either.
		if left := strings.TrimSpace(f.sh("pgrep -fa '[s]leep 600' || true")); left != "" {
			t.Fatalf("the stuck holder's children survived: %s", left)
		}
		f.sh("kill -9 -- -" + holderPID + " 2>/dev/null; rm -rf /var/tmp/teploy-backup.* " + itShims + "/aws")
	})

	t.Run("partial failure across targets: others done, nothing pruned, exit and heartbeat say so", func(t *testing.T) {
		f.t = t
		p := base
		p.Recipient = f.pub
		p.HeartbeatURLs = append(p.HeartbeatURLs, "http://127.0.0.1:"+itHookPort+"/broken/"+itToken, "http://127.0.0.1:1/dead/"+itToken)
		f.setPolicy(p)
		f.sh("docker stop teployit-cache >/dev/null")
		beforeKeys := f.keys("teployit/accessories/cache/")
		res, code, out := f.runAll("")
		f.sh("docker start teployit-cache >/dev/null")
		f.waitFor("redis", "docker exec teployit-cache redis-cli ping")
		if code == 0 || res.Status != backup.RunPartial {
			t.Fatalf("exit %d status %s\n%s", code, res.Status, out)
		}
		if tr := f.target(res, "teployit-cache"); tr.Status != backup.TargetFailed || tr.Pruned != 0 {
			t.Fatalf("cache: %+v", tr)
		}
		if tr := f.target(res, "teployit-db"); tr.Status != backup.TargetOK {
			t.Fatalf("db must still be backed up: %+v", tr)
		}
		if got := f.keys("teployit/accessories/cache/"); strings.Join(got, ",") != strings.Join(beforeKeys, ",") {
			t.Fatalf("a failed target's objects changed:\n%v\n%v", beforeKeys, got)
		}
		log := f.sh("cat " + itDir + "/hook.log")
		if !strings.Contains(lastLines(log, 2), "status=down") {
			t.Fatalf("heartbeat must say down: %s", log)
		}
		// A broken and a dead receiver did not fail what was backed up, and their
		// tokens are nowhere in the output.
		for _, h := range res.Heartbeats {
			if strings.Contains(h.Host, itToken) || strings.Contains(h.Error, itToken) {
				t.Fatalf("heartbeat result leaks the token: %+v", h)
			}
		}
		if strings.Contains(out, itToken) {
			t.Fatalf("output leaks the token: %s", out)
		}
		if lastEvent := f.sh("tail -n 1 /deployments/.teploy/apps/_fleet/events.jsonl"); !strings.Contains(lastEvent, `"status":"partial"`) || !strings.Contains(lastEvent, `"outcome":"failed"`) {
			t.Fatalf("event: %s", lastEvent)
		}
		f.setPolicy(backup.FleetPolicy{V: "1", Interval: "1h", Retention: retention, Volumes: true, Recipient: f.pub, Adopted: base.Adopted, HeartbeatURLs: base.HeartbeatURLs})
	})

	t.Run("GFS prunes exactly the expected set, sidecars included, in a real bucket", func(t *testing.T) {
		f.t = t
		// Twelve weeks of history for one accessory, mixed sidecars, one bad object.
		seed := []string{}
		start := time.Date(2026, 8, 1, 3, 0, 0, 0, time.UTC)
		for i := 0; i < 90; i += 3 {
			seed = append(seed, start.AddDate(0, 0, i).Format("20060102-150405"))
		}
		f.sh("head -c 2000 /dev/urandom > " + itDir + "/blob")
		seedScript := "export AWS_ACCESS_KEY_ID=" + itAK + " AWS_SECRET_ACCESS_KEY=" + itSK + " AWS_DEFAULT_REGION=us-east-1; s() { aws --endpoint-url " + itEndpoint + " s3 cp --only-show-errors " + itDir + "/blob \"s3://itfleet/gfsapp/accessories/db/$1\"; }; "
		for _, id := range seed {
			seedScript += fmt.Sprintf("s %s.pgdump.age; s globals/%s.sql.gz.age; s meta/%s.json.age; s databases/extra/%s.pgdump.age; ", id, id, id, id)
		}
		f.sh(seedScript)
		client := backup.NewClient(vm, os.Stderr)
		policy := backup.RetentionPolicy{KeepLast: 2, KeepDaily: 3, KeepWeekly: 4, KeepMonthly: 2}
		// Most of a series going at once is refused, and nothing goes.
		beforeBrake := f.keys("gfsapp/accessories/db/")
		if _, err := client.PruneBackups(ctx, "gfsapp", "accessories/db", f.s3, policy); !errors.Is(err, backup.ErrPruneBrake) {
			t.Fatalf("brake: %v", err)
		}
		if got := f.keys("gfsapp/accessories/db/"); len(got) != len(beforeBrake) {
			t.Fatalf("the brake deleted something: %d of %d left", len(got), len(beforeBrake))
		}
		policy.AllowMassDelete = true
		deleted, err := client.PruneBackups(ctx, "gfsapp", "accessories/db", f.s3, policy)
		if err != nil {
			t.Fatalf("prune: %v (deleted %v)", err, deleted)
		}
		want := map[string]bool{}
		for _, id := range resticForget(seed, policy) {
			want[id] = true
		}
		if len(want) >= len(seed) || len(want) < 5 {
			t.Fatalf("a useless fixture: keeps %d of %d", len(want), len(seed))
		}
		got := map[string]bool{}
		for _, k := range f.keys("gfsapp/accessories/db/") {
			parts := strings.Split(k, "/")
			got[strings.Split(parts[len(parts)-1], ".")[0]] = true
		}
		if fmt.Sprint(sortedKeysOf(got)) != fmt.Sprint(sortedKeysOf(want)) {
			t.Fatalf("kept %v, want %v", sortedKeysOf(got), sortedKeysOf(want))
		}
		// Sidecars and per-database objects are gone with their dump, kept with it.
		rest := f.keys("gfsapp/accessories/db/")
		if len(rest) != 4*len(want) {
			t.Fatalf("%d objects left for %d backups: %v", len(rest), len(want), rest)
		}

		// A database whose last run of each day fails: it has its own history, and
		// keep_daily holds for it, not only for the primary's ids.
		var primary, reports []string
		seed2 := "export AWS_ACCESS_KEY_ID=" + itAK + " AWS_SECRET_ACCESS_KEY=" + itSK + " AWS_DEFAULT_REGION=us-east-1; s() { aws --endpoint-url " + itEndpoint + " s3 cp --only-show-errors " + itDir + "/blob \"s3://itfleet/seriesapp/accessories/db/$1\"; }; "
		for d := 0; d < 12; d++ {
			day := time.Date(2026, 10, 10+d, 0, 0, 0, 0, time.UTC)
			early, late := day.Add(2*time.Hour).Format("20060102-150405"), day.Add(20*time.Hour).Format("20060102-150405")
			primary = append(primary, early, late)
			reports = append(reports, early)
			seed2 += "s " + early + ".pgdump.age; s " + late + ".pgdump.age; s databases/reports/" + early + ".pgdump.age; "
		}
		f.sh(seed2)
		daily := backup.RetentionPolicy{KeepDaily: 4, AllowMassDelete: true}
		if _, err := client.PruneBackups(ctx, "seriesapp", "accessories/db", f.s3, daily); err != nil {
			t.Fatalf("prune: %v", err)
		}
		gotReports := map[string]bool{}
		for _, k := range f.keys("seriesapp/accessories/db/databases/reports/") {
			parts := strings.Split(k, "/")
			gotReports[strings.Split(parts[len(parts)-1], ".")[0]] = true
		}
		wantReports := map[string]bool{}
		for _, id := range resticForget(reports, daily) {
			wantReports[id] = true
		}
		if fmt.Sprint(sortedKeysOf(gotReports)) != fmt.Sprint(sortedKeysOf(wantReports)) {
			t.Fatalf("reports kept %v, want %v: a database must get its own keep_daily", sortedKeysOf(gotReports), sortedKeysOf(wantReports))
		}
		_ = primary
	})

	t.Run("a leftover schedule store does not decide retention", func(t *testing.T) {
		f.t = t
		before := len(f.keys("teployit/accessories/db/"))
		if err := backup.WritePolicy(ctx, vm, "teployit", backup.AppPolicy{Accessories: map[string]*backup.KindPolicy{"db": {KeepLast: 1}}}); err != nil {
			t.Fatal(err)
		}
		res, code, out := f.runAll("")
		f.sh("rm -f " + backup.PolicyPath("teployit"))
		if code != 0 {
			t.Fatalf("exit %d: %s", code, out)
		}
		if !strings.Contains(strings.Join(res.Warnings, "\n"), "no cron line runs it") {
			t.Fatalf("no warning about the leftover: %v", res.Warnings)
		}
		var primaries int
		for _, k := range f.keys("teployit/accessories/db/") {
			if strings.Count(strings.TrimPrefix(k, "teployit/accessories/db/"), "/") == 0 {
				primaries++
			}
		}
		if primaries < 3 {
			t.Fatalf("keep_last 1 from a stale store pruned the history: %d primaries left (of %d keys before)", primaries, before)
		}
	})

	t.Run("every database dropped: failure and heartbeat down, nothing pruned", func(t *testing.T) {
		f.t = t
		f.sh("docker run -d --name teployit3-db --memory 128m --label teploy.role=accessory --label teploy.app=teployit3 --label teploy.accessory=db -e POSTGRES_DB=three -e POSTGRES_USER=appuser -e POSTGRES_PASSWORD=" + itDBPW + " postgres:17-alpine >/dev/null")
		f.waitFor("postgres", "docker exec teployit3-db pg_isready -h 127.0.0.1 -U appuser")
		f.psql("teployit3-db", "three", "CREATE TABLE t(id int)")
		defer f.sh("docker rm -fv teployit3-db >/dev/null 2>&1; true")
		res, code, out := f.runAll("")
		if code != 0 || f.target(res, "teployit3-db").Status != backup.TargetOK {
			t.Fatalf("first run: %d %s", code, out)
		}
		before := f.keys("teployit3/accessories/db/")
		f.sh("docker exec teployit3-db psql -X -q -U appuser -d postgres -c 'DROP DATABASE three'")
		res, code, out = f.runAll("")
		if code == 0 || f.target(res, "teployit3-db").Status != backup.TargetFailed {
			t.Fatalf("a target that had backups and now holds nothing must fail: exit %d %+v\n%s", code, f.target(res, "teployit3-db"), out)
		}
		if got := f.keys("teployit3/accessories/db/"); strings.Join(got, ",") != strings.Join(before, ",") {
			t.Fatalf("pruned on a run that stored nothing:\n%v\n%v", before, got)
		}
		if log := f.sh("cat " + itDir + "/hook.log"); !strings.Contains(lastLine(log), "status=down") {
			t.Fatalf("heartbeat must be down: %s", lastLine(log))
		}
	})

	t.Run("a target over 256 MiB gets a checksum the store verified", func(t *testing.T) {
		f.t = t
		f.sh("dd if=/dev/urandom of=/deployments/teployit/volumes/data/big bs=1M count=258 2>/dev/null")
		res, code, out := f.runAll("")
		f.sh("rm -f /deployments/teployit/volumes/data/big")
		if code != 0 {
			t.Fatalf("exit %d: %s", code, out)
		}
		vol := f.target(res, "teployit/volumes")
		if vol.SizeBytes < 256<<20 {
			t.Fatalf("not over the read-back limit: %+v", vol)
		}
		bucket, key, _ := strings.Cut(strings.TrimPrefix(vol.Objects[0].Key, "s3://"), "/")
		head := f.awsIT("s3api head-object --checksum-mode ENABLED --bucket " + bucket + " --key " + key)
		if !strings.Contains(head, "sha256") {
			t.Fatalf("no recorded sha256: %s", head)
		}
		if left := f.keys(".incoming/teployit/volumes/"); len(left) != 0 {
			t.Fatalf("pending upload left: %v", left)
		}
	})

	t.Run("8 MB and up: multipart copy keeps its checksum, a refused copy re-uploads, one pending tree", func(t *testing.T) {
		f.t = t
		bucketKey := func(uri string) (string, string) {
			b, k, _ := strings.Cut(strings.TrimPrefix(uri, "s3://"), "/")
			return b, k
		}
		f.sh("dd if=/dev/urandom of=/deployments/teployit/volumes/data/mid bs=1M count=20 2>/dev/null")
		defer f.sh("rm -f /deployments/teployit/volumes/data/mid " + itShims + "/aws")
		res, code, out := f.runAll("")
		if code != 0 {
			t.Fatalf("exit %d: %s", code, out)
		}
		vol := f.target(res, "teployit/volumes")
		if vol.SizeBytes < 8<<20 {
			t.Fatalf("not multipart: %+v", vol)
		}
		b, k := bucketKey(vol.Objects[0].Key)
		if head := f.awsIT("s3api head-object --bucket " + b + " --key " + k); !strings.Contains(head, `"sha256"`) {
			t.Fatalf("the multipart copy lost the recorded checksum: %s", head)
		}
		// A store that refuses the copy, as B2 is documented to: the object still
		// arrives, verified, with its checksum, and restores.
		realAWS := strings.TrimSpace(f.sh("command -v aws"))
		shim := "#!/bin/sh\ncase \"$*\" in *'s3 cp s3://'*.incoming*s3://*) echo 'copy failed: An error occurred (InvalidRequest) when calling the UploadPartCopy operation: checksum algorithm cannot be changed' >&2; exit 1;; esac\nexec " + realAWS + " \"$@\"\n"
		if err := vm.Upload(ctx, strings.NewReader(shim), itShims+"/aws", "0755"); err != nil {
			t.Fatal(err)
		}
		res, code, out = f.runAll("PATH=" + itShims + ":$PATH")
		if code != 0 || !strings.Contains(out, "uploaded straight to its final name") {
			t.Fatalf("exit %d: %s", code, out)
		}
		vol = f.target(res, "teployit/volumes")
		b, k = bucketKey(vol.Objects[0].Key)
		if head := f.awsIT("s3api head-object --bucket " + b + " --key " + k); !strings.Contains(head, `"sha256"`) {
			t.Fatalf("the re-upload lost the recorded checksum: %s", head)
		}
		f.sh("rm -f " + itShims + "/aws")
		if left := f.keys(".incoming/teployit/volumes/"); len(left) != 0 {
			t.Fatalf("pending uploads left: %v", left)
		}
		// Doctor reads the lifecycle rule the pending tree is meant for.
		if st, err := backup.ReadFleetStatus(ctx, vm, time.Now().UTC()); err != nil || st.Target.IncomingLifecycle != backup.LifecycleAbsent {
			t.Fatalf("no rule yet: %q %v", st.Target.IncomingLifecycle, err)
		}
		if c := doctorBackupPolicyCheck(ctx, vm, time.Now().UTC()); !strings.Contains(c.Detail, "no lifecycle rule expires .incoming/") {
			t.Fatalf("doctor: %+v", c)
		}
		f.sh("printf '%s' " + ssh.ShellQuote(`{"Rules":[{"ID":"incoming","Status":"Enabled","Filter":{"Prefix":".incoming/"},"Expiration":{"Days":1}}]}`) + " > " + itDir + "/rules.json")
		f.awsIT("s3api put-bucket-lifecycle-configuration --bucket itfleet --lifecycle-configuration file://" + itDir + "/rules.json")
		defer f.awsIT("s3api delete-bucket-lifecycle --bucket itfleet")
		if st, err := backup.ReadFleetStatus(ctx, vm, time.Now().UTC()); err != nil || st.Target.IncomingLifecycle != backup.LifecycleCovered {
			t.Fatalf("with the rule: %q %v", st.Target.IncomingLifecycle, err)
		}
	})

	t.Run("on a versioned store the pending bytes sit in one tree and markers are not rewritten", func(t *testing.T) {
		f.t = t
		// Versioning needs erasure coding: a second MinIO on four directories.
		const port = "19101"
		endpoint := "http://127.0.0.1:" + port
		f.sh("docker rm -fv teploy-it-minio2 >/dev/null 2>&1; docker run -d --name teploy-it-minio2 --memory 512m -p 127.0.0.1:" + port + ":9000 -e MINIO_ROOT_USER=" + itAK + " -e MINIO_ROOT_PASSWORD=" + itSK + " cgr.dev/chainguard/minio:latest server /tmp/d{1...4} >/dev/null")
		defer f.sh("docker rm -fv teploy-it-minio2 >/dev/null 2>&1; true")
		aws := "AWS_ACCESS_KEY_ID=" + itAK + " AWS_SECRET_ACCESS_KEY=" + itSK + " aws --region us-east-1 --endpoint-url " + endpoint + " "
		f.waitFor("the second minio", aws+"s3 mb s3://itv")
		f.sh(aws + "s3api put-bucket-versioning --bucket itv --versioning-configuration Status=Enabled")
		cfg := backup.S3Config{Bucket: "itv", Region: "us-east-1", Endpoint: endpoint, AccessKey: itAK, SecretKey: itSK, Prefix: "srv1"}
		client := backup.NewClient(vm, os.Stderr)
		retention := backup.RetentionPolicy{KeepLast: 1}
		for i := 0; i < 3; i++ {
			if _, err := client.AccessoryBackup(ctx, "teployit", "cache", "redis:7-alpine", nil, cfg); err != nil {
				t.Fatal(err)
			}
			if _, err := client.PruneBackups(ctx, "teployit", "accessories/cache", cfg, retention); err != nil {
				t.Fatal(err)
			}
			time.Sleep(1100 * time.Millisecond)
		}
		var versions struct {
			Versions []struct {
				Key string `json:"Key"`
			} `json:"Versions"`
		}
		if err := json.Unmarshal([]byte(f.sh(aws+"s3api list-object-versions --bucket itv --output json")), &versions); err != nil {
			t.Fatal(err)
		}
		counts := map[string]int{}
		for _, v := range versions.Versions {
			counts[v.Key]++
			if strings.Contains(strings.TrimPrefix(v.Key, "srv1/.incoming/"), ".incoming") {
				t.Fatalf("a pending upload outside the one tree: %s", v.Key)
			}
		}
		pending, marker := 0, 0
		for k, n := range counts {
			switch {
			case strings.HasPrefix(k, "srv1/.incoming/"):
				pending += n
			case strings.Contains(k, "/.owners/"):
				marker += n
			}
		}
		if pending == 0 {
			t.Fatalf("no pending versions at all, so this proved nothing: %v", counts)
		}
		if marker != 1 {
			t.Fatalf("the marker was rewritten: %d versions: %v", marker, counts)
		}
	})

	t.Run("never prunes across servers", func(t *testing.T) {
		f.t = t
		client := backup.NewClient(vm, os.Stderr)
		// Another server's marker in this namespace.
		f.sh("printf x > " + itDir + "/o")
		f.awsIT("s3 cp " + itDir + "/o s3://itfleet/gfsapp/.owners/aaaaaaaaaaaaaaaa --only-show-errors")
		before := f.keys("gfsapp/accessories/db/")
		_, err := client.PruneBackups(ctx, "gfsapp", "accessories/db", f.s3, backup.RetentionPolicy{KeepLast: 1})
		if err == nil || !strings.Contains(err.Error(), "shared backup namespace") {
			t.Fatalf("err = %v", err)
		}
		if got := f.keys("gfsapp/accessories/db/"); len(got) != len(before) {
			t.Fatalf("pruned across servers: %v", got)
		}
		// A rebuilt server's old marker: visible, and clearable by name.
		out, err := f.cli("", "--json", "server", "backup-target", "owners")
		if err != nil || !strings.Contains(out, "aaaaaaaaaaaaaaaa") {
			t.Fatalf("owners: %v\n%s", err, out)
		}
		// A marker written minutes ago may be a live server: refused without --force.
		if out, err := f.cli("", "server", "backup-target", "owners", "--clear", "aaaaaaaaaaaaaaaa"); err == nil || !strings.Contains(out, "--force") {
			t.Fatalf("clear of a recent marker: %v\n%s", err, out)
		}
		if out, err := f.cli("", "server", "backup-target", "owners", "--clear", "aaaaaaaaaaaaaaaa", "--force"); err != nil || !strings.Contains(out, "Removed 1 marker") {
			t.Fatalf("clear: %v\n%s", err, out)
		}
		if log := f.sh("grep backup_target.owners_clear /deployments/.teploy/apps/_fleet/events.jsonl"); !strings.Contains(log, `"outcome":"refused"`) || !strings.Contains(log, `"force":true`) {
			t.Fatalf("the clears are not on record: %s", log)
		}
		if _, err := client.PruneBackups(ctx, "gfsapp", "accessories/db", f.s3, backup.RetentionPolicy{KeepLast: 1, AllowMassDelete: true}); err != nil {
			t.Fatalf("prune after clearing: %v", err)
		}
		// Per-server prefixes: a sibling prefix is invisible to this one's prune.
		f.sh("head -c 2000 /dev/urandom > " + itDir + "/blob")
		f.awsIT("s3 cp " + itDir + "/blob s3://itfleet/server-b/shared/accessories/db/20200101-000000.pgdump --only-show-errors")
		f.awsIT("s3 cp " + itDir + "/blob s3://itfleet/server-b/shared/accessories/db/20200102-000000.pgdump --only-show-errors")
		f.awsIT("s3 cp " + itDir + "/blob s3://itfleet/server-a/shared/accessories/db/20200101-000000.pgdump --only-show-errors")
		f.awsIT("s3 cp " + itDir + "/blob s3://itfleet/server-a/shared/accessories/db/20200102-000000.pgdump --only-show-errors")
		a := f.s3
		a.Prefix = "server-a"
		if _, err := client.PruneBackups(ctx, "shared", "accessories/db", a, backup.RetentionPolicy{KeepLast: 1, AllowMassDelete: true}); err != nil {
			t.Fatal(err)
		}
		if got := f.keys("server-a/shared/accessories/"); len(got) != 1 {
			t.Fatalf("server-a kept %v", got)
		}
		if got := f.keys("server-b/shared/accessories/"); len(got) != 2 {
			t.Fatalf("server-a's prune reached server-b: %v", got)
		}
	})

	t.Run("prefix change: old objects found and restorable, never pruned unless owned", func(t *testing.T) {
		f.t = t
		rows, err := backup.NewClient(vm, os.Stderr).ListBackupsRecursive(ctx, "teployit", f.s3)
		if err != nil || len(rows) == 0 {
			t.Fatalf("listing: %v %v", rows, err)
		}
		oldID := ""
		for _, r := range rows {
			if r.Accessory == "db" && r.Database == "" && r.Restorable && strings.HasSuffix(r.Key, ".age") {
				oldID = r.ID
				break
			}
		}
		if oldID == "" {
			t.Fatalf("no restorable db object: %+v", rows)
		}
		// Move the target onto a per-server prefix through the real command.
		cred := itAK + "\n" + itSK + "\n"
		out, err := f.cli(cred, "server", "backup-target", "set", "--bucket", "itfleet", "--endpoint", itEndpoint, "--prefix", "srv1")
		if err != nil {
			t.Fatalf("target set: %v\n%s", err, out)
		}
		if !strings.Contains(out, `Previous prefix ""`) {
			t.Fatalf("prefix move not reported: %s", out)
		}
		target, err := backup.ReadTarget(ctx, vm)
		if err != nil {
			t.Fatal(err)
		}
		cfg := TargetS3(target)
		if cfg.Prefix != "srv1" || len(cfg.PreviousPrefixes) != 1 {
			t.Fatalf("target: %+v", cfg)
		}
		client := backup.NewClient(vm, os.Stderr)
		// New run goes under the prefix; list finds both; old id restores.
		res, code, runOut := f.runAll("")
		if code != 0 {
			t.Fatalf("run after prefix move: %s", runOut)
		}
		if tr := f.target(res, "teployit-db"); !strings.Contains(tr.Objects[len(tr.Objects)-1].Key, "s3://itfleet/srv1/teployit/") {
			t.Fatalf("new objects not under the prefix: %+v", tr.Objects)
		}
		rows2, err := client.ListBackupsRecursive(ctx, "teployit", cfg)
		if err != nil {
			t.Fatal(err)
		}
		var sawOld, sawNew bool
		for _, r := range rows2 {
			sawOld = sawOld || (r.Previous && r.ID == oldID)
			sawNew = sawNew || (!r.Previous && strings.Contains(r.Key, "/srv1/"))
		}
		if !sawOld || !sawNew {
			t.Fatalf("list across prefixes: old=%v new=%v", sawOld, sawNew)
		}
		out, err = f.cli("", "backup", "restore", oldID, "--app", "teployit", "--accessory", "db", "--yes", "--no-backup", "--json")
		if err != nil {
			t.Fatalf("restoring from the old prefix: %v\n%s", err, out)
		}
		// Prune never touches the unowned old prefix.
		before := f.keys("teployit/accessories/db/")
		if _, err := client.PruneBackups(ctx, "teployit", "accessories/db", cfg, backup.RetentionPolicy{KeepLast: 1, AllowMassDelete: true}); err != nil {
			t.Fatal(err)
		}
		if got := f.keys("teployit/accessories/db/"); len(got) != len(before) {
			t.Fatalf("pruned an unowned previous prefix: %d -> %d", len(before), len(got))
		}
		// The real flag: owned applies only to the prefix being left. A second run
		// first, so the prefix being left has history to age out.
		if _, code, runOut := f.runAll(""); code != 0 {
			t.Fatalf("second run: %s", runOut)
		}
		srv1Before := len(f.keys("srv1/teployit/accessories/db/"))
		rootBefore := len(f.keys("teployit/accessories/db/"))
		out, err = f.cli(cred, "server", "backup-target", "set", "--bucket", "itfleet", "--endpoint", itEndpoint, "--prefix", "srv2", "--previous-prefix-owned")
		if err != nil {
			t.Fatalf("target set --previous-prefix-owned: %v\n%s", err, out)
		}
		moved, err := backup.ReadTarget(ctx, vm)
		if err != nil {
			t.Fatal(err)
		}
		for _, pp := range moved.PreviousPrefixes {
			if want := pp.Prefix == "srv1"; pp.Owned != want {
				t.Fatalf("%q owned=%v: %+v", pp.Prefix, pp.Owned, moved.PreviousPrefixes)
			}
		}
		if _, err := client.PruneBackups(ctx, "teployit", "accessories/db", TargetS3(moved), backup.RetentionPolicy{KeepLast: 1, AllowMassDelete: true}); err != nil {
			t.Fatal(err)
		}
		if got := len(f.keys("teployit/accessories/db/")); got != rootBefore {
			t.Fatalf("--previous-prefix-owned pruned the shared root: %d -> %d", rootBefore, got)
		}
		if got := len(f.keys("srv1/teployit/accessories/db/")); got >= srv1Before {
			t.Fatalf("the owned prefix did not age out: %d -> %d", srv1Before, got)
		}
		// A marker from another server in the owned prefix overrides the word.
		f.sh("printf x > " + itDir + "/o")
		f.awsIT("s3 cp " + itDir + "/o s3://itfleet/srv1/teployit/.owners/bbbbbbbbbbbbbbbb --only-show-errors")
		srv1Now := len(f.keys("srv1/teployit/accessories/db/"))
		if _, err := client.PruneBackups(ctx, "teployit", "accessories/db", TargetS3(moved), backup.RetentionPolicy{KeepLast: 1}); err == nil || !strings.Contains(err.Error(), "shared backup namespace") {
			t.Fatalf("a foreign marker in an owned prefix: %v", err)
		}
		if got := len(f.keys("srv1/teployit/accessories/db/")); got != srv1Now {
			t.Fatalf("pruned a prefix with a foreign marker: %d -> %d", srv1Now, got)
		}
		f.awsIT("s3 rm s3://itfleet/srv1/teployit/.owners/bbbbbbbbbbbbbbbb --only-show-errors")
		if out, err := f.cli(cred, "server", "backup-target", "set", "--bucket", "itfleet", "--endpoint", itEndpoint, "--prefix", "srv1"); err != nil {
			t.Fatalf("moving back: %v\n%s", err, out)
		}
		// Owned: it ages out under retention, and the newest of all survives.
		owned := cfg
		owned.PreviousPrefixes = []backup.PreviousPrefix{{Prefix: "", Owned: true}}
		if _, err := client.PruneBackups(ctx, "teployit", "accessories/db", owned, backup.RetentionPolicy{KeepLast: 1, AllowMassDelete: true}); err != nil {
			t.Fatal(err)
		}
		if got := f.keys("teployit/accessories/db/"); len(got) >= len(before) {
			t.Fatalf("owned previous prefix did not age out: %v", got)
		}
		id, _, err := client.LatestBackupDate(ctx, "teployit", "db", owned)
		if err != nil || id == "" {
			t.Fatalf("latest after pruning: %q %v", id, err)
		}
	})

	t.Run("status names every database, adopted and unmanaged, with no secrets", func(t *testing.T) {
		f.t = t
		f.sh("docker run -d --name omni_postgres --memory 128m -e POSTGRES_USER=appuser -e POSTGRES_PASSWORD=" + itDBPW + " postgres:17-alpine >/dev/null")
		f.waitFor("unmanaged postgres", "docker exec omni_postgres pg_isready -h 127.0.0.1 -U appuser")
		res, _, _ := f.runAll("")
		found := false
		for _, c := range res.UncoveredContainers {
			found = found || c == "omni_postgres"
		}
		if !found {
			t.Fatalf("an unmanaged postgres is not reported uncovered: %+v", res)
		}
		out, err := f.cli("", "--json", "backup", "status")
		if err != nil {
			t.Fatalf("status: %v\n%s", err, out)
		}
		for _, secretValue := range []string{itToken, itDBPW, itSK, itAK} {
			if strings.Contains(out, secretValue) {
				t.Fatalf("status leaks %q: %s", secretValue, out)
			}
		}
		var st backupStatusDTO
		if err := json.Unmarshal(lastJSONLine([]byte(out)), &st); err != nil {
			t.Fatalf("status JSON: %v\n%s", err, out)
		}
		entry := func(name string) backup.StatusEntry {
			for _, e := range st.Entries {
				if e.Name == name {
					return e
				}
			}
			t.Fatalf("no entry %q in %+v", name, st.Entries)
			return backup.StatusEntry{}
		}
		if e := entry("omni_postgres"); e.Coverage != backup.CoverageUncovered || !e.Unmanaged {
			t.Fatalf("omni_postgres: %+v", e)
		}
		if e := entry("wordpress-shared-mysql"); e.Coverage != backup.CoverageAdopted || !e.Adopted || e.Newest == nil || !e.Newest.Encrypted || len(e.Databases) == 0 {
			t.Fatalf("mysql: %+v", e)
		}
		if e := entry("teployit-db"); e.Coverage != backup.CoverageCovered || e.LastSuccess == nil || e.Newest == nil {
			t.Fatalf("db: %+v", e)
		}
		if e := entry("teployit/volumes"); e.Schedule != "policy" || e.Newest == nil {
			t.Fatalf("volumes: %+v", e)
		}
		if !st.Encrypted || !st.Target.Reachable {
			t.Fatalf("summary: %+v", st)
		}
		if st.LastRun == nil {
			t.Fatal("no last run")
		}
		sawUncovered := false
		for _, u := range st.UncoveredDatabases {
			sawUncovered = sawUncovered || u == "omni_postgres"
		}
		if !sawUncovered {
			t.Fatalf("uncovered_databases: %v", st.UncoveredDatabases)
		}

		// Dash restores a row by id and accessory alone: per-database rows only on
		// request, or restoring one would replace the primary.
		listOut, err := f.cli("", "--json", "backup", "list", "--app", "teployit")
		if err != nil {
			t.Fatalf("list: %v\n%s", err, listOut)
		}
		if strings.Contains(listOut, `"database"`) {
			t.Fatalf("per-database rows without --databases: %s", listOut)
		}
		listOut, err = f.cli("", "--json", "backup", "list", "--app", "teployit", "--databases")
		if err != nil || !strings.Contains(listOut, `"database":"reports"`) {
			t.Fatalf("--databases: %v\n%s", err, listOut)
		}

		// A wiped namespace is not covered, whatever the last run said.
		f.awsIT("s3 rm --recursive --only-show-errors s3://itfleet/srv1/_adopted/accessories/wordpress-shared-mysql/")
		f.awsIT("s3 rm --recursive --only-show-errors s3://itfleet/_adopted/accessories/wordpress-shared-mysql/")
		out, err = f.cli("", "--json", "backup", "status")
		if err != nil {
			t.Fatalf("status: %v\n%s", err, out)
		}
		st = backupStatusDTO{}
		if err := json.Unmarshal(lastJSONLine([]byte(out)), &st); err != nil {
			t.Fatal(err)
		}
		if e := entry("wordpress-shared-mysql"); e.Coverage != backup.CoverageStale || e.Newest != nil {
			t.Fatalf("a wiped namespace: %+v", e)
		}
		res, code, runOut := f.runAll("")
		if code != 0 {
			t.Fatalf("run: %d %s", code, runOut)
		}
		_ = res
	})

	t.Run("adopting by name: an underscore, no labels", func(t *testing.T) {
		f.t = t
		f.psql("omni_postgres", "postgres", "CREATE DATABASE omni")
		f.psql("omni_postgres", "omni", "CREATE TABLE o(id int); INSERT INTO o VALUES (1)")
		// The machine path, end to end. --host must not count as a policy flag.
		policyJSON, _ := json.Marshal(backup.FleetPolicy{Interval: "1h", Retention: retention, Volumes: true, Recipient: f.pub, Adopted: base.Adopted, HeartbeatURLs: base.HeartbeatURLs})
		out, err := f.cli(string(policyJSON), "--json", "server", "backup-policy", "set", "--stdin")
		if err != nil || strings.Contains(out, itToken) {
			t.Fatalf("policy set --stdin: %v\n%s", err, out)
		}
		// A machine-written policy that drops the key does not silently stop encrypting.
		noKey, _ := json.Marshal(backup.FleetPolicy{Interval: "1h", Retention: retention, Volumes: true})
		if out, err := f.cli(string(noKey), "server", "backup-policy", "set", "--stdin"); err == nil || !strings.Contains(out, "--no-recipient") {
			t.Fatalf("dropping the recipient on stdin: %v\n%s", err, out)
		}
		out, err = f.cli("", "server", "backup-policy", "set", "--adopt", "omni_postgres:postgres:all", "--volume-max-gb", "0.000001")
		if err != nil {
			t.Fatalf("policy set: %v\n%s", err, out)
		}
		if !strings.Contains(out, "Adopted: omni_postgres") || strings.Contains(out, itToken) {
			t.Fatalf("policy set output: %s", out)
		}
		crontab := f.sh("crontab -l")
		if strings.Count(crontab, "backup run --all") != 1 || !strings.Contains(crontab, "[teploy-backup-all]") {
			t.Fatalf("crontab: %s", crontab)
		}
		// Twice: still one line.
		if out, err := f.cli("", "server", "backup-policy", "set", "--interval", "6h"); err != nil {
			t.Fatalf("policy set: %v\n%s", err, out)
		}
		if crontab := f.sh("crontab -l"); strings.Count(crontab, "backup run --all") != 1 {
			t.Fatalf("crontab grew: %s", crontab)
		}
		res, code, runOut := f.runAll("")
		if code != 0 {
			t.Fatalf("run: %s", runOut)
		}
		tr := f.target(res, "omni_postgres")
		if tr.Status != backup.TargetOK {
			t.Fatalf("omni: %+v", tr)
		}
		if len(f.keys("srv1/_adopted/accessories/omni_postgres/databases/omni/")) == 0 {
			t.Fatal("no per-database object for an adopted postgres")
		}
		if len(f.keys("srv1/_adopted/accessories/omni_postgres/globals/")) == 0 {
			t.Fatal("no globals for an adopted postgres cluster")
		}
		// Size guard: visible in the result, the event and status, nothing stored.
		vol := f.target(res, "teployit/volumes")
		if vol.Status != backup.TargetExcluded || !vol.SizeGuard {
			t.Fatalf("size guard: %+v", vol)
		}
		if !strings.Contains(strings.Join(res.Omissions, "\n"), "volume_max_gb") {
			t.Fatalf("omissions: %v", res.Omissions)
		}
		if ev := f.sh("tail -n 3 /deployments/.teploy/apps/teployit/events.jsonl"); !strings.Contains(ev, `"outcome":"refused"`) {
			t.Fatalf("size guard not in events: %s", ev)
		}
		// Restore and verify address the adopted postgres.
		out, err = f.cli("", "--json", "accessory", "verify-backup", "omni_postgres", "--adopted", "--database", "omni")
		if err != nil || !strings.Contains(out, `"ok": true`) {
			t.Fatalf("verify adopted postgres: %v\n%s", err, out)
		}
		id := ""
		for _, o := range tr.Objects {
			if o.Database == "omni" {
				parts := strings.Split(o.Key, "/")
				id = strings.Split(parts[len(parts)-1], ".")[0]
			}
		}
		f.psql("omni_postgres", "omni", "DELETE FROM o")
		out, err = f.cli("", "backup", "restore", id, "--adopted", "--accessory", "omni_postgres", "--database", "omni", "--yes", "--json")
		if err != nil {
			t.Fatalf("restore adopted postgres: %v\n%s", err, out)
		}
		if got := f.psql("omni_postgres", "omni", "SELECT COUNT(*) FROM o"); got != "1" {
			t.Fatalf("restored rows = %s", got)
		}
	})

	t.Run("destructive operations back up first and refuse when they cannot", func(t *testing.T) {
		f.t = t
		cur, err := backup.ReadTarget(ctx, vm)
		if err != nil {
			t.Fatal(err)
		}
		base := ""
		if cur.Prefix != "" {
			base = cur.Prefix + "/"
		}
		const state = `{"schema_version":2,"deployment_type":"container","ingress_mode":"host","updated_at":"2026-09-23T09:00:00Z","image_ref":"alpine:3","operation_id":"op1","generation":2,"current_port":3000,"current_hash":"h2"}`
		f.sh("mkdir -p /deployments/teployit2/volumes/data && printf keep > /deployments/teployit2/volumes/data/f && chmod go-w /deployments/teployit2")
		if err := vm.Upload(ctx, strings.NewReader(state), "/deployments/teployit2/state.json", "0600"); err != nil {
			t.Fatal(err)
		}
		f.sh("docker run -d --name teployit2-db --memory 128m --label teploy.role=accessory --label teploy.app=teployit2 --label teploy.accessory=db -e POSTGRES_DB=two -e POSTGRES_USER=appuser -e POSTGRES_PASSWORD=" + itDBPW + " postgres:17-alpine >/dev/null")
		f.waitFor("postgres", "docker exec teployit2-db pg_isready -h 127.0.0.1 -U appuser")
		f.psql("teployit2-db", "two", "CREATE TABLE z(id int)")
		f.sh("docker exec teployit2-db psql -X -q -U appuser -d postgres -c 'CREATE DATABASE analytics'")
		f.psql("teployit2-db", "analytics", "CREATE TABLE a(id int); INSERT INTO a VALUES (41),(42)")
		// Upgrade, from a folder with teploy.yml.
		dir := t.TempDir()
		yml := "app: teployit2\nserver: " + f.host + "\ndomain: teployit2.teploy-fixture.dev\nimage: alpine:3\naccessories:\n  db:\n    image: postgres:17-alpine\n    env:\n      POSTGRES_DB: two\n      POSTGRES_USER: appuser\n      POSTGRES_PASSWORD: " + itDBPW + "\n"
		if err := os.WriteFile(filepath.Join(dir, "teploy.yml"), []byte(yml), 0o600); err != nil {
			t.Fatal(err)
		}
		inDir := func(args ...string) (string, error) {
			c := exec.CommandContext(ctx, f.local, append([]string{"--host", f.host, "--user", f.user, "--key", f.key}, args...)...)
			c.Dir = dir
			out, err := c.CombinedOutput()
			return string(out), err
		}
		// The DB is down: the backup fails, so the upgrade is refused and nothing moves.
		f.sh("docker stop teployit2-db >/dev/null")
		before := f.sh("docker inspect -f '{{.Id}}' teployit2-db")
		out, err := inDir("accessory", "upgrade", "db", "postgres:17-alpine")
		if err == nil || !strings.Contains(out, "--no-backup") || !strings.Contains(out, "teployit2-db") {
			t.Fatalf("upgrade with a failing backup: %v\n%s", err, out)
		}
		if after := f.sh("docker inspect -f '{{.Id}}' teployit2-db"); after != before {
			t.Fatal("a refused upgrade replaced the container")
		}
		// Purge refuses the same way, naming what it would delete, and deletes nothing.
		out, err = f.cli("", "remove", "--app", "teployit2", "--purge", "--yes")
		if err == nil || !strings.Contains(out, "--no-backup") {
			t.Fatalf("purge with a failing backup: %v\n%s", err, out)
		}
		if strings.TrimSpace(f.sh("cat /deployments/teployit2/volumes/data/f")) != "keep" {
			t.Fatal("a refused purge deleted data")
		}
		f.sh("docker start teployit2-db >/dev/null")
		f.waitFor("postgres", "docker exec teployit2-db pg_isready -h 127.0.0.1 -U appuser")
		// Healthy: the upgrade backs up, then recreates.
		keysBefore := len(f.keys(base + "teployit2/accessories/db/"))
		out, err = inDir("accessory", "upgrade", "db", "postgres:17-alpine")
		if err != nil || !strings.Contains(out, "Backed up the current data first") {
			t.Fatalf("upgrade: %v\n%s", err, out)
		}
		if len(f.keys(base+"teployit2/accessories/db/")) <= keysBefore {
			t.Fatal("no backup object from the upgrade")
		}
		if len(f.keys(base+"teployit2/accessories/db/databases/analytics/")) == 0 {
			t.Fatalf("the upgrade's backup left out a database: %v", f.keys(base+"teployit2/accessories/db/"))
		}
		f.waitFor("postgres", "docker exec teployit2-db pg_isready -h 127.0.0.1 -U appuser")
		f.psql("teployit2-db", "two", "CREATE TABLE IF NOT EXISTS z(id int)")
		// The accessory has no volume in this fixture, so the data did not survive
		// the recreate: seed the second database again for the purge.
		f.sh("docker exec teployit2-db psql -X -q -U appuser -d postgres -c 'CREATE DATABASE analytics'")
		f.psql("teployit2-db", "analytics", "CREATE TABLE a(id int); INSERT INTO a VALUES (41),(42)")
		analyticsBefore := len(f.keys(base + "teployit2/accessories/db/databases/analytics/"))
		// Healthy: the purge backs up the accessory and the volumes, then deletes.
		keysBefore = len(f.keys(base + "teployit2/accessories/db/"))
		out, err = f.cli("", "remove", "--app", "teployit2", "--purge", "--yes")
		if err != nil {
			t.Fatalf("purge: %v\n%s", err, out)
		}
		if len(f.keys(base+"teployit2/accessories/db/")) <= keysBefore || len(f.keys(base+"teployit2/volumes/")) == 0 {
			t.Fatalf("no backups from the purge: %v", f.keys(base+"teployit2/"))
		}
		if !strings.Contains(out, "Backed up every database") {
			t.Fatalf("purge output: %s", out)
		}
		// What the purge destroyed is in the bucket: every database, not the primary alone.
		if got := len(f.keys(base + "teployit2/accessories/db/databases/analytics/")); got <= analyticsBefore {
			t.Fatalf("the purge's backup left out a database: %d -> %d", analyticsBefore, got)
		}
		// Safety copies are marked: retention leaves them for a week.
		if len(f.keys(base+"teployit2/accessories/db/safety/")) == 0 {
			t.Fatalf("no safety markers: %v", f.keys(base+"teployit2/accessories/db/"))
		}
		if res := ssh.RunDetailed(ctx, vm, "docker inspect teployit2-db >/dev/null 2>&1"); !res.Failed() {
			t.Fatal("the purge left the accessory")
		}
		if strings.TrimSpace(f.sh("if [ -e /deployments/teployit2/volumes ]; then echo yes; fi")) != "" {
			t.Fatal("the purge left the volumes")
		}
	})

	t.Run("DR bundle: members encrypted, the age key wrapped to the owner, restored with the owner key", func(t *testing.T) {
		f.t = t
		const state = `{"schema_version":2,"deployment_type":"container","ingress_mode":"host","updated_at":"2026-09-23T09:00:00Z","image_ref":"alpine:3","operation_id":"op1","generation":2,"current_port":3000,"current_hash":"h2"}`
		const release = `{"schema_version":1,"app":"teployit","hash":"h2","created_at":"2026-09-22T10:00:00Z","image_ref":"alpine:3","cmd":"sleep 600"}`
		f.sh("mkdir -p /deployments/teployit/meta /deployments/teployit/volumes/data")
		if err := vm.Upload(ctx, strings.NewReader(state), "/deployments/teployit/state.json", "0600"); err != nil {
			t.Fatal(err)
		}
		if err := vm.Upload(ctx, strings.NewReader(release), "/deployments/teployit/meta/h2.json", "0600"); err != nil {
			t.Fatal(err)
		}
		target, err := backup.ReadTarget(ctx, vm)
		if err != nil {
			t.Fatal(err)
		}
		cfg := TargetS3(target)
		cfg.OwnerRecipient = f.pub
		store := backup.S3BundleStore{S3: cfg}
		appCfg := config.AppConfig{App: "teployit", Volumes: map[string]string{"data": "/app/data"}, Accessories: map[string]config.AccessoryConfig{
			"db": {Image: "postgres:17-alpine", Env: map[string]string{"POSTGRES_DB": "appdb", "POSTGRES_USER": "appuser", "POSTGRES_PASSWORD": itDBPW}},
		}}
		client := backup.NewClient(vm, os.Stderr)
		// No recovery key: the private key must not go anywhere.
		if _, err := client.CreateBundle(ctx, backup.BundleOptions{App: "teployit", Config: appCfg, IncludeSecrets: true, IncludeAgeKey: true}, store); err == nil || !strings.Contains(err.Error(), "recovery key") {
			t.Fatalf("--include-age-key without a recipient: %v", err)
		}
		m, err := client.CreateBundle(ctx, backup.BundleOptions{App: "teployit", Config: appCfg, IncludeSecrets: true, IncludeAgeKey: true, OwnerRecipient: f.pub, Version: "it"}, store)
		if err != nil {
			t.Fatalf("CreateBundle: %v", err)
		}
		if !m.Secrets.AgeKeyWrapped {
			t.Fatalf("manifest: %+v", m.Secrets)
		}
		prefix := "srv1/teployit/dr/" + m.ID + "/"
		keys := strings.Join(f.keys(prefix), "\n")
		for _, want := range []string{prefix + "accessories/db.pgdump.age", prefix + "volumes/data.tar.gz.age", prefix + "age-key.age", prefix + "manifest.json"} {
			if !strings.Contains(keys, want+"\n") && !strings.HasSuffix(keys, want) {
				t.Fatalf("missing %s in:\n%s", want, keys)
			}
		}
		// The bundled key opens with the owner key, and matches the server's;
		// the server's own identity cannot open it.
		f.awsIT("s3 cp s3://itfleet/" + prefix + "age-key.age " + itDir + "/age-key.age --only-show-errors")
		if res := ssh.RunDetailed(ctx, vm, "age -d -i /deployments/.age-key "+itDir+"/age-key.age"); !res.Failed() {
			t.Fatal("the wrapped key must open only with the owner's key")
		}
		if got, want := f.sh("age -d -i "+f.owner+" "+itDir+"/age-key.age | grep AGE-SECRET-KEY"), f.sh("grep AGE-SECRET-KEY /deployments/.age-key"); got != want {
			t.Fatalf("the bundled key is not the server's")
		}
		// Restore needs the owner key; it works with it, nothing live touched.
		if _, err := client.RestoreBundleIsolated(ctx, backup.BundleRestoreOptions{App: "teployit", ID: m.ID, Config: appCfg}, store); err == nil || !strings.Contains(err.Error(), "--identity") {
			t.Fatalf("a wrapped key without an identity: %v", err)
		}
		f.sh("rm -rf /var/tmp/teploy-dr/teployit")
		receipt, err := client.RestoreBundleIsolated(ctx, backup.BundleRestoreOptions{App: "teployit", ID: m.ID, Config: appCfg, Identity: f.owner}, store)
		if err != nil {
			t.Fatalf("RestoreBundleIsolated: %v", err)
		}
		if !receipt.OK {
			t.Fatalf("receipt: %+v", receipt)
		}
		vm.Run(ctx, "docker ps -aq --filter name=teploy-dr- | xargs -r docker rm -fv >/dev/null 2>&1; rm -rf /var/tmp/teploy-dr/teployit")

		// Clean-host recovery through the normal commands: no server identity, no
		// policy, no stored target; only the bucket credentials and the owner's key.
		cleanHost := "mkdir -p " + itDir + "/hidden && mv /deployments/.age-key " + itDir + "/hidden/age-key && for f in backup-policy.json backup-target.json; do [ -e \"$HOME\"/.config/teploy/$f ] && mv \"$HOME\"/.config/teploy/$f " + itDir + "/hidden/$f; done; true"
		restoreHost := "mv " + itDir + "/hidden/age-key /deployments/.age-key; for f in backup-policy.json backup-target.json; do [ -e " + itDir + "/hidden/$f ] && mv " + itDir + "/hidden/$f \"$HOME\"/.config/teploy/$f; done; true"
		f.sh(cleanHost)
		defer f.sh(restoreHost)
		dir := t.TempDir()
		yml := "app: teployit\nserver: " + f.host + "\ndomain: teployit.teploy-fixture.dev\nimage: alpine:3\nvolumes:\n  data: /app/data\naccessories:\n  db:\n    image: postgres:17-alpine\n    env:\n      POSTGRES_DB: appdb\n      POSTGRES_USER: appuser\n      POSTGRES_PASSWORD: " + itDBPW + "\n"
		if err := os.WriteFile(filepath.Join(dir, "teploy.yml"), []byte(yml), 0o600); err != nil {
			t.Fatal(err)
		}
		creds := []string{"TEPLOY_S3_ACCESS_KEY=" + itAK, "TEPLOY_S3_SECRET_KEY=" + itSK}
		run := func(args ...string) (string, error) {
			c := exec.CommandContext(ctx, f.local, append([]string{"--host", f.host, "--user", f.user, "--key", f.key}, args...)...)
			c.Dir = dir
			c.Env = append(os.Environ(), creds...)
			o, e := c.CombinedOutput()
			return string(o), e
		}
		s3Flags := []string{"--bucket", "itfleet", "--endpoint", itEndpoint, "--prefix", "srv1"}
		// Without the key: refused, naming the flag.
		out, err := run(append([]string{"dr", "restore", m.ID}, s3Flags...)...)
		if err == nil || !strings.Contains(out, "--identity") {
			t.Fatalf("dr restore with no key on a clean host: %v\n%s", err, out)
		}
		f.sh("rm -rf /var/tmp/teploy-dr/teployit")
		out, err = run(append(append([]string{"dr", "restore", m.ID}, s3Flags...), "--identity", f.ownerLocal)...)
		if err != nil {
			t.Fatalf("dr restore --identity --bucket --prefix on a clean host: %v\n%s", err, out)
		}
		vm.Run(ctx, "docker ps -aq --filter name=teploy-dr- | xargs -r docker rm -fv >/dev/null 2>&1; rm -rf /var/tmp/teploy-dr/teployit")
		if left := strings.TrimSpace(f.sh("ls -d /var/tmp/teploy-dr-identity.* 2>/dev/null; true")); left != "" {
			t.Fatalf("the owner key was left on the server: %s", left)
		}
		// A database and one schema, encrypted, restored with only the owner's key.
		rows := f.keys("srv1/teployit/accessories/db/")
		id := ""
		for _, k := range rows {
			if rel := strings.TrimPrefix(k, "srv1/teployit/accessories/db/"); !strings.Contains(rel, "/") && strings.HasSuffix(rel, ".pgdump.age") {
				id = strings.Split(rel, ".")[0]
			}
		}
		if id == "" {
			t.Fatalf("no encrypted primary: %v", rows)
		}
		f.psql("teployit-db", "appdb", "INSERT INTO t VALUES (777)")
		// The server identity alone is absent: without --identity it says what to do.
		out, err = run(append([]string{"backup", "restore", id, "--app", "teployit", "--accessory", "db", "--yes", "--no-backup"}, s3Flags...)...)
		if err == nil || !strings.Contains(out, "--identity") {
			t.Fatalf("restore with no key on a clean host: %v\n%s", err, out)
		}
		out, err = run(append(append([]string{"backup", "restore", id, "--app", "teployit", "--accessory", "db", "--yes", "--no-backup"}, s3Flags...), "--identity", f.ownerLocal)...)
		if err != nil {
			t.Fatalf("backup restore --identity on a clean host: %v\n%s", err, out)
		}
		if got := f.psql("teployit-db", "appdb", "SELECT COUNT(*) FROM t WHERE id = 777"); got != "0" {
			t.Fatalf("the restore did not replace the data: %s", got)
		}
		if left := strings.TrimSpace(f.sh("ls -d /var/tmp/teploy-dr-identity.* 2>/dev/null; true")); left != "" {
			t.Fatalf("the owner key was left on the server: %s", left)
		}
	})

	t.Run("a real cron firing, with cron's own PATH", func(t *testing.T) {
		f.t = t
		seq := f.sh("wc -l < /deployments/.teploy/apps/_fleet/events.jsonl")
		f.sh(`crontab -l | sed -E 's|^([^ ]+ +){5}|* * * * * |' | crontab -`)
		line := f.sh("crontab -l")
		if !strings.HasPrefix(line, "* * * * * ") {
			t.Fatalf("crontab: %s", line)
		}
		fired := false
		for i := 0; i < 240 && !fired; i++ {
			if strings.TrimSpace(f.sh("wc -l < /deployments/.teploy/apps/_fleet/events.jsonl")) != strings.TrimSpace(seq) {
				fired = true
				break
			}
			time.Sleep(time.Second)
		}
		f.sh(`crontab -l | sed -E 's|^([^ ]+ +){5}|7 3 * * * |' | crontab -`)
		f.waitFor("the cron run to finish", "! pgrep -f '[b]ackup run --all' >/dev/null")
		if !fired {
			t.Fatal("cron never ran it")
		}
		last := f.sh("tail -n 1 /deployments/.teploy/apps/_fleet/events.jsonl")
		if !strings.Contains(last, `"kind":"backup.all"`) || strings.Contains(last, "aws CLI not found") {
			t.Fatalf("cron's run: %s", last)
		}
	})

	t.Run("a non-root user who cannot read /etc/teploy never uploads plaintext", func(t *testing.T) {
		f.t = t
		policy := `{"v":"1","interval":"1h","recipient":"` + f.pub + `"}`
		f.sh("sudo -n mkdir -p /etc/teploy && printf '%s' " + ssh.ShellQuote(policy) + " | sudo -n tee /etc/teploy/backup-policy.json >/dev/null && sudo -n chmod 600 /etc/teploy/backup-policy.json && sudo -n chmod 700 /etc/teploy")
		defer f.sh("sudo -n rm -f /etc/teploy/backup-policy.json; sudo -n chmod 755 /etc/teploy; sudo -n rmdir /etc/teploy 2>/dev/null; sudo -n userdel -rf teployit 2>/dev/null; true")
		// This user has passwordless sudo, as deploy users do: the policy is read.
		got, err := backup.ReadFleetPolicy(ctx, vm)
		if err != nil || got.Recipient != f.pub {
			t.Fatalf("through sudo: %+v %v", got, err)
		}
		// One without sudo: refused, not "no policy". A target of its own, so the
		// policy read is what stands between it and a plaintext upload.
		f.sh("sudo -n useradd -m teployit 2>/dev/null; printf '%s' " + ssh.ShellQuote(`{"v":"1","bucket":"itfleet","region":"us-east-1","endpoint":"`+itEndpoint+`"}`) + " | sudo -n -u teployit -H sh -c 'mkdir -p ~/.config/teploy && cat > ~/.config/teploy/backup-target.json'; sudo -n chmod 755 " + itDir + " " + itBin)
		before := strings.Join(f.keys("srv1/teployit/volumes/"), ",")
		res := ssh.RunDetailed(ctx, vm, "sudo -n -u teployit -H env AWS_ACCESS_KEY_ID="+itAK+" AWS_SECRET_ACCESS_KEY="+itSK+" "+itBin+" backup run --app teployit")
		if !res.Failed() || !strings.Contains(string(res.Stdout)+string(res.Stderr), "cannot tell whether") {
			t.Fatalf("a hidden policy must refuse: exit %d\n%s%s", res.ExitCode, res.Stdout, res.Stderr)
		}
		if strings.Join(f.keys("srv1/teployit/volumes/"), ",") != before {
			t.Fatal("something was uploaded")
		}
		res = ssh.RunDetailed(ctx, vm, "sudo -n -u teployit -H "+itBin+" backup run --all")
		if !res.Failed() || !strings.Contains(string(res.Stdout)+string(res.Stderr), "cannot tell whether") {
			t.Fatalf("--all: exit %d\n%s%s", res.ExitCode, res.Stdout, res.Stderr)
		}
		// Back to the user with sudo: rotation lands where reads look, no second copy.
		rotated := strings.TrimSpace(strings.Split(strings.SplitN(func() string {
			c := exec.Command(f.local, "backup", "keygen")
			b, err := c.Output()
			if err != nil {
				t.Fatal(err)
			}
			return string(b)
		}(), "# public key: ", 2)[1], "\n")[0])
		path, fallback, err := backup.WriteFleetPolicy(ctx, vm, backup.FleetPolicy{Interval: "1h", Recipient: rotated})
		if err != nil || path != backup.PolicyFileEtc || fallback {
			t.Fatalf("write through sudo: %s %v %v", path, fallback, err)
		}
		if got, err = backup.ReadFleetPolicy(ctx, vm); err != nil || got.Recipient != rotated {
			t.Fatalf("show after set: %+v %v", got, err)
		}
		if !strings.Contains(f.sh("sudo -n cat "+backup.PolicyFileEtc), rotated) {
			t.Fatal("the file in /etc was not rotated")
		}
		if left := ssh.RunDetailed(ctx, vm, `test -e "$HOME"/.config/teploy/backup-policy.json`); !left.Failed() {
			t.Fatal("a second policy was written under ~/.config")
		}
		// The run this user's cron would make: its lock cannot live in /etc/teploy.
		_, _, out := f.runAll("")
		if strings.Contains(out, "backup-run.lock") || strings.Contains(out, "permission denied") {
			t.Fatalf("the run failed on its lock: %s", out)
		}
		// Unset through the same path, and it says so.
		removed, err := backup.UnsetFleetPolicy(ctx, vm)
		if err != nil || strings.Join(removed, ",") != backup.PolicyFileEtc {
			t.Fatalf("unset: %v %v", removed, err)
		}
		if got, err = backup.ReadFleetPolicy(ctx, vm); err != nil || got.Configured() {
			t.Fatalf("after unset: %+v %v", got, err)
		}
	})

	t.Run("zero targets is a failure unless the policy allows it", func(t *testing.T) {
		f.t = t
		f.sh("docker rm -fv teployit-db teployit-cache wordpress-shared-mysql omni_postgres >/dev/null 2>&1; true")
		p := backup.FleetPolicy{V: "1", Interval: "1h", Retention: retention, Recipient: f.pub, HeartbeatURLs: base.HeartbeatURLs}
		f.setPolicy(p)
		res, code, out := f.runAll("")
		if code == 0 || res.Status != backup.RunFailed || !strings.Contains(res.Error, "allow_empty") {
			t.Fatalf("exit %d %s %s\n%s", code, res.Status, res.Error, out)
		}
		if log := f.sh("cat " + itDir + "/hook.log"); !strings.Contains(lastLine(log), "status=down") {
			t.Fatalf("heartbeat: %s", lastLine(log))
		}
		p.AllowEmpty = true
		f.setPolicy(p)
		res, code, out = f.runAll("")
		if code != 0 || res.Status != backup.RunOK {
			t.Fatalf("allowed: exit %d %s\n%s", code, res.Status, out)
		}
	})

	t.Run("doctor flags a policy with no target and no recipient", func(t *testing.T) {
		f.t = t
		if _, err := backup.UnsetTarget(ctx, vm); err != nil {
			t.Fatal(err)
		}
		c := doctorBackupPolicyCheck(ctx, vm, time.Now().UTC())
		if c.Result != doctorFail || !strings.Contains(c.Detail, "no target") {
			t.Fatalf("doctor: %+v", c)
		}
		res, code, out := f.runAll("")
		if code == 0 || res.Status != backup.RunFailed {
			t.Fatalf("a run with no target: exit %d %s\n%s", code, res.Status, out)
		}
		if log := f.sh("cat " + itDir + "/hook.log"); !strings.Contains(lastLines(log, 1), "status=down") {
			t.Fatalf("no-target run must heartbeat down: %s", log)
		}
	})
}

func lastLine(s string) string {
	return lastLines(s, 1)
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) < n {
		return strings.Join(lines, "\n")
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}

func sortedKeysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// resticForget is restic's forget, written out independently of selectKept: a
// second derivation of the expected set.
func resticForget(ids []string, p backup.RetentionPolicy) []string {
	sorted := append([]string{}, ids...)
	sort.Sort(sort.Reverse(sort.StringSlice(sorted)))
	keep := map[string]bool{sorted[0]: true}
	var lastD, lastW, lastM string
	d, w, m := p.KeepDaily, p.KeepWeekly, p.KeepMonthly
	for i, id := range sorted {
		ts, _ := time.Parse("20060102-150405", id)
		if i < p.KeepLast {
			keep[id] = true
		}
		day := ts.Format("2006-01-02")
		y, wk := ts.ISOWeek()
		week := fmt.Sprintf("%d-%d", y, wk)
		month := ts.Format("2006-01")
		if d > 0 && day != lastD {
			lastD, d = day, d-1
			keep[id] = true
		}
		if w > 0 && week != lastW {
			lastW, w = week, w-1
			keep[id] = true
		}
		if m > 0 && month != lastM {
			lastM, m = month, m-1
			keep[id] = true
		}
	}
	return sortedKeysOf(keep)
}
