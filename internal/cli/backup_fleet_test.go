package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/spf13/cobra"
	"github.com/useteploy/teploy/internal/backup"
	"github.com/useteploy/teploy/internal/ssh"
)

const testRecipient = "age1lggyhqrw2nlhcxprm67z43rta597azn8gknawjehu9d9dl0jq3yqqvfafg"

func TestParseAdoptFlag(t *testing.T) {
	e, err := parseAdoptFlag("omni_postgres")
	if err != nil || e.Container != "omni_postgres" || !e.Databases.All || e.Engine != "" {
		t.Fatalf("%+v %v", e, err)
	}
	e, _ = parseAdoptFlag("m:mysql:wp_a,wp_b")
	if e.Engine != "mysql" || e.Databases.All || strings.Join(e.Databases.Names, ",") != "wp_a,wp_b" {
		t.Fatalf("%+v", e)
	}
	e, _ = parseAdoptFlag("m:mysql:all")
	if !e.Databases.All {
		t.Fatalf("%+v", e)
	}
}

// Flags change only what they name.
func TestApplyPolicyFlagsMerges(t *testing.T) {
	cmd := &cobra.Command{}
	var f policySetFlags
	cmd.Flags().StringVar(&f.interval, "interval", "", "")
	cmd.Flags().IntVar(&f.keepLast, "keep-last", 0, "")
	cmd.Flags().IntVar(&f.keepDaily, "keep-daily", 0, "")
	cmd.Flags().IntVar(&f.keepWeekly, "keep-weekly", 0, "")
	cmd.Flags().IntVar(&f.keepMonthly, "keep-monthly", 0, "")
	cmd.Flags().IntVar(&f.maxAgeDays, "max-age-days", 0, "")
	cmd.Flags().StringSliceVar(&f.excludes, "exclude", nil, "")
	cmd.Flags().BoolVar(&f.volumes, "volumes", false, "")
	cmd.Flags().Float64Var(&f.volumeMaxGB, "volume-max-gb", 0, "")
	cmd.Flags().StringVar(&f.recipient, "recipient", "", "")
	cmd.Flags().BoolVar(&f.noRecipient, "no-recipient", false, "")
	if err := cmd.ParseFlags([]string{"--keep-daily", "7"}); err != nil {
		t.Fatal(err)
	}
	f.adopts = []string{"a:postgres:all", "b"}
	f.removeAdopted = []string{"gone"}
	p := backup.FleetPolicy{Interval: "1h", Retention: backup.FleetRetention{KeepLast: 3, KeepDaily: 1}, Recipient: testRecipient, Volumes: true,
		Adopted:       []backup.AdoptedEntry{{Container: "gone", Databases: backup.DatabaseSelector{All: true}}, {Container: "a", Databases: backup.DatabaseSelector{Names: []string{"x"}}}},
		HeartbeatURLs: []string{"https://h/t"},
	}
	if err := applyPolicyFlags(cmd, &p, &f); err != nil {
		t.Fatal(err)
	}
	if p.Interval != "1h" || p.Retention.KeepLast != 3 || p.Retention.KeepDaily != 7 || p.Recipient != testRecipient || !p.Volumes || len(p.HeartbeatURLs) != 1 {
		t.Fatalf("%+v", p)
	}
	if len(p.Adopted) != 2 || p.Adopted[0].Container != "a" || !p.Adopted[0].Databases.All || p.Adopted[1].Container != "b" {
		t.Fatalf("adopted: %+v", p.Adopted)
	}
}

func TestRestorePromptNamesWhatIsReplaced(t *testing.T) {
	for _, tc := range []struct{ app, acc, db, want string }{
		{"blog", "db", "", "Replace the data in blog-db"},
		{"blog", "db", "reports", "Replace database reports in blog-db"},
		{"blog", "", "", "Replace everything in blog's volumes"},
		{backup.AdoptedApp, "shared", "wp_a", "Replace database wp_a in adopted container shared"},
	} {
		got := restorePrompt(tc.app, tc.acc, tc.db, "20260101-000000")
		if !strings.Contains(got, tc.want) || !strings.Contains(got, "20260101-000000") {
			t.Errorf("%q does not name %q", got, tc.want)
		}
	}
	if !strings.Contains(restorePrompt(backup.AdoptedApp, "s", "d", "x"), "Nothing that uses it is stopped") {
		t.Error("an adopted restore must say nothing is stopped for you")
	}
}

func TestMergePreviousPrefixes(t *testing.T) {
	old := backup.Target{Bucket: "b", Endpoint: "e", Prefix: "mine", PreviousPrefixes: []backup.PreviousPrefix{{Prefix: ""}, {Prefix: "older", Owned: true}}}
	// owned applies only to the prefix being left, never to the shared root.
	prev, lost, err := mergePreviousPrefixes(old, "b", "e", "srv1", true, false, nil)
	if err != nil || lost || len(prev) != 3 {
		t.Fatalf("%+v %v %v", prev, lost, err)
	}
	for _, p := range prev {
		want := p.Prefix == "mine" || p.Prefix == "older"
		if p.Owned != want {
			t.Fatalf("%q owned=%v: %+v", p.Prefix, p.Owned, prev)
		}
	}
	// An earlier one is claimed by name, and only one that exists.
	prev, _, err = mergePreviousPrefixes(old, "b", "e", "mine", false, false, []string{""})
	if err != nil || len(prev) != 2 || !prev[0].Owned {
		t.Fatalf("%+v %v", prev, err)
	}
	if _, _, err := mergePreviousPrefixes(old, "b", "e", "mine", false, false, []string{"never"}); err == nil {
		t.Fatal("claiming an unknown prefix must be refused")
	}
	// Moving back takes it off the list.
	prev, _, _ = mergePreviousPrefixes(backup.Target{Bucket: "b", Endpoint: "e", Prefix: "srv1", PreviousPrefixes: []backup.PreviousPrefix{{Prefix: ""}}}, "b", "e", "", false, false, nil)
	if len(prev) != 1 || prev[0].Prefix != "srv1" {
		t.Fatalf("%+v", prev)
	}
	if prev, lost, _ := mergePreviousPrefixes(old, "other", "e", "x", false, false, nil); !lost || prev != nil {
		t.Fatalf("a new bucket starts fresh: %+v %v", prev, lost)
	}
	if prev, lost, _ := mergePreviousPrefixes(old, "b", "e", "x", false, true, nil); lost || prev != nil {
		t.Fatalf("forget: %+v %v", prev, lost)
	}
	if prev, lost, _ := mergePreviousPrefixes(backup.Target{}, "b", "e", "x", false, false, nil); lost || prev != nil {
		t.Fatalf("first target: %+v %v", prev, lost)
	}
}

func TestBackupKeygenPrintsAUsablePair(t *testing.T) {
	var b bytes.Buffer
	if err := writeBackupKeygen(&b, false); err != nil {
		t.Fatal(err)
	}
	ids, err := age.ParseIdentities(strings.NewReader(b.String()))
	if err != nil || len(ids) != 1 {
		t.Fatalf("the printed output must parse as an identity file: %v", err)
	}
	if !strings.Contains(b.String(), "# public key: age1") {
		t.Fatalf("%s", b.String())
	}
	var j bytes.Buffer
	if err := writeBackupKeygen(&j, true); err != nil {
		t.Fatal(err)
	}
	var dto struct {
		Public  string `json:"public_key"`
		Private string `json:"private_key"`
	}
	if err := json.Unmarshal(j.Bytes(), &dto); err != nil {
		t.Fatal(err)
	}
	if backup.ValidateRecipient(dto.Public) != nil || !strings.HasPrefix(dto.Private, "AGE-SECRET-KEY-") {
		t.Fatalf("%+v", dto)
	}
}

// fetchFixture puts a fake aws on PATH that copies a local file as "the object".
func fetchFixture(t *testing.T, recipient age.Recipient, plain string) string {
	t.Helper()
	dir := t.TempDir()
	obj := filepath.Join(dir, "object")
	f, err := os.Create(obj)
	if err != nil {
		t.Fatal(err)
	}
	w, err := age.Encrypt(f, recipient)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, plain); err != nil {
		t.Fatal(err)
	}
	w.Close()
	f.Close()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n[ \"$1\" = s3 ] && [ \"$2\" = cp ] || exit 2\ncp " + obj + " \"$4\"\n"
	if err := os.WriteFile(filepath.Join(bin, "aws"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return obj
}

func TestBackupFetchRecoversWithOnlyTheOwnerIdentity(t *testing.T) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	fetchFixture(t, id.Recipient(), "the dump")
	dir := t.TempDir()
	idFile := filepath.Join(dir, "recovery.txt")
	if err := os.WriteFile(idFile, []byte("# comment\n"+id.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "restored.pgdump")
	ctx := context.Background()
	dto, err := runBackupFetch(ctx, "s3://b/a/accessories/db/1.pgdump.age", idFile, out, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !dto.Encrypted || dto.SizeBytes != 8 {
		t.Fatalf("%+v", dto)
	}
	if b, _ := os.ReadFile(out); string(b) != "the dump" {
		t.Fatalf("content %q", b)
	}
	st, _ := os.Stat(out)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode())
	}
	// Refuses to overwrite.
	if _, err := runBackupFetch(ctx, "s3://b/k.pgdump.age", idFile, out, "", ""); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("overwrite: %v", err)
	}
	// A wrong identity leaves nothing behind.
	wrong, _ := age.GenerateX25519Identity()
	wrongFile := filepath.Join(dir, "wrong.txt")
	os.WriteFile(wrongFile, []byte(wrong.String()+"\n"), 0o600)
	out2 := filepath.Join(dir, "second.pgdump")
	if _, err := runBackupFetch(ctx, "s3://b/k.pgdump.age", wrongFile, out2, "", ""); err == nil {
		t.Fatal("a wrong identity must fail")
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".teploy-fetch-") || e.Name() == "second.pgdump" {
			t.Fatalf("left behind: %s", e.Name())
		}
	}
	// An encrypted object needs an identity; a key must be a key.
	if _, err := runBackupFetch(ctx, "s3://b/k.pgdump.age", "", filepath.Join(dir, "x"), "", ""); err == nil || !strings.Contains(err.Error(), "--identity") {
		t.Fatalf("no identity: %v", err)
	}
	if _, err := runBackupFetch(ctx, "/etc/passwd", "", filepath.Join(dir, "x"), "", ""); err == nil {
		t.Fatal("not an s3 key")
	}
}

// A truncated ciphertext fails authentication: never a short "success".
func TestBackupFetchRefusesATruncatedObject(t *testing.T) {
	id, _ := age.GenerateX25519Identity()
	obj := fetchFixture(t, id.Recipient(), strings.Repeat("x", 200000))
	b, _ := os.ReadFile(obj)
	os.WriteFile(obj, b[:len(b)/2], 0o600)
	dir := t.TempDir()
	idFile := filepath.Join(dir, "id")
	os.WriteFile(idFile, []byte(id.String()+"\n"), 0o600)
	out := filepath.Join(dir, "out")
	if _, err := runBackupFetch(context.Background(), "s3://b/k.age", idFile, out, "", ""); err == nil {
		t.Fatal("a truncated object must fail")
	}
	if _, err := os.Stat(out); err == nil {
		t.Fatal("no output may appear")
	}
}

func TestFleetFailedExitStatus(t *testing.T) {
	if fleetFailed(backup.FleetRunResult{Status: backup.RunOK}) != nil || fleetFailed(backup.FleetRunResult{Status: "skipped"}) != nil {
		t.Fatal("ok and skipped exit zero")
	}
	if err := fleetFailed(backup.FleetRunResult{Status: backup.RunPartial, Targets: []backup.FleetTargetResult{{Status: backup.TargetFailed}}}); err == nil || !strings.Contains(err.Error(), "1 target") {
		t.Fatalf("%v", err)
	}
	if err := fleetFailed(backup.FleetRunResult{Status: backup.RunFailed, Error: "boom"}); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("%v", err)
	}
}

// runFleet with no target: fails, heartbeats down, names what to do, and the
// token reaches neither output nor event.
func TestRunFleetNoTargetHeartbeatsDown(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = append(got, r.URL.RequestURI()) }))
	defer srv.Close()
	dir := t.TempDir()
	mock := ssh.NewMockExecutor("h",
		ssh.MockCommand{Match: "if [ -d /etc/teploy ]", Output: "path " + dir + "/backup-policy.json\n" + `{"v":"1","interval":"1h","heartbeat_urls":["` + srv.URL + `/api/push/SECRET"]}`},
		ssh.MockCommand{Match: "for p in /etc/teploy/backup-target.json", Output: "absent\n"},
		ssh.MockCommand{Match: "python3 -c", Output: `{"seq":1,"ts":"2026-01-01T00:00:00Z"}`},
	)
	var out bytes.Buffer
	res, err := runFleet(context.Background(), mock, &out, false, nil)
	if err == nil || res.Status != backup.RunFailed || !strings.Contains(res.Error, "backup-target set") {
		t.Fatalf("%v %+v", err, res)
	}
	if len(got) != 1 || !strings.Contains(got[0], "status=down") {
		t.Fatalf("heartbeats %v", got)
	}
	if strings.Contains(out.String(), "SECRET") {
		t.Fatalf("output leaks the token: %s", out.String())
	}
	for _, input := range mock.Inputs {
		if strings.Contains(input, "SECRET") {
			t.Fatalf("event leaks the token: %s", input)
		}
	}
}

func TestRunFleetWithNoPolicyFails(t *testing.T) {
	mock := ssh.NewMockExecutor("h", ssh.MockCommand{Match: "if [ -d /etc/teploy ]", Output: "absent\n"})
	res, err := runFleet(context.Background(), mock, io.Discard, false, nil)
	if err == nil || res.Status != backup.RunFailed || !strings.Contains(res.Error, "backup-policy set") {
		t.Fatalf("%v %+v", err, res)
	}
}

// A run that finds the lock held stands down: exit zero, no heartbeat, no event.
func TestRunFleetStandsDownOnOverlap(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = append(got, r.URL.RequestURI()) }))
	defer srv.Close()
	dir := t.TempDir()
	lock, _, err := backup.AcquireRunLock(backup.RunLockPath(dir+"/backup-policy.json"), time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	mock := ssh.NewMockExecutor("h",
		ssh.MockCommand{Match: "if [ -d /etc/teploy ]", Output: "path " + dir + "/backup-policy.json\n" + `{"v":"1","heartbeat_urls":["` + srv.URL + `/hb"]}`},
	)
	res, err := runFleet(context.Background(), mock, io.Discard, false, nil)
	if err != nil || res.Status != "skipped" {
		t.Fatalf("%v %+v", err, res)
	}
	if len(got) != 0 {
		t.Fatalf("a skipped run must not heartbeat: %v", got)
	}
}

// One target failing does not stop the next; exit, event and heartbeat say so.
func TestRunFleetPartialFailure(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = append(got, r.URL.RequestURI()) }))
	defer srv.Close()
	dir := t.TempDir()
	line := func(name, app, acc string) string {
		b, _ := json.Marshal(map[string]any{"name": "/" + name, "image": "redis:7", "state": "running",
			"labels": map[string]string{"teploy.role": "accessory", "teploy.app": app, "teploy.accessory": acc},
			"env":    []string{"REDIS_VERSION=7"}, "entrypoint": []string{}, "cmd": []string{}})
		return string(b)
	}
	mock := ssh.NewMockExecutor("h",
		ssh.MockCommand{Match: "if [ -d /etc/teploy ]", Output: "path " + dir + "/backup-policy.json\n" + `{"v":"1","interval":"1h","retention":{"keep_last":1},"heartbeat_urls":["` + srv.URL + `/hb"]}`},
		ssh.MockCommand{Match: "for p in /etc/teploy/backup-target.json", Output: "path /etc/teploy/backup-target.json\n" + `{"v":"1","bucket":"b","region":"r"}`},
		ssh.MockCommand{Match: "which aws", Output: "/usr/bin/aws\n"},
		ssh.MockCommand{Match: "for d in", Output: ""},
		ssh.MockCommand{Match: "( c=$(python3", Output: ""},
		ssh.MockCommand{Match: "ids=$(docker ps -aq", Output: line("a-cache", "a", "cache") + "\n" + line("b-cache", "b", "cache") + "\n"},
		ssh.MockCommand{Match: "crontab -l", Output: ""},
		ssh.MockCommand{Match: "cat", Output: ""},
		ssh.MockCommand{Match: "docker inspect -f", Output: "{}\n[]\n[]\n"},
		ssh.MockCommand{Match: "umask 077", Output: "/var/tmp/teploy-backup.t\n"},
		ssh.MockCommand{Match: "docker exec 'a-cache'", Err: errors.New("exit status 1: no such container")},
		ssh.MockCommand{Match: "set -eu", Err: errors.New("exit status 1: bgsave refused")},
		ssh.MockCommand{Match: "docker exec", Output: ""},
		ssh.MockCommand{Match: "rm -rf", Output: ""},
		ssh.MockCommand{Match: "python3 -c", Output: `{"seq":1,"ts":"2026-01-01T00:00:00Z"}`},
	)
	var out bytes.Buffer
	res, err := runFleet(context.Background(), mock, &out, false, nil)
	if err == nil {
		t.Fatal("a run with failures must exit nonzero")
	}
	if len(res.Targets) != 2 {
		t.Fatalf("the second target must still run: %+v", res.Targets)
	}
	if res.Status != backup.RunFailed {
		t.Fatalf("both failed here: %s", res.Status)
	}
	if len(got) != 1 || !strings.Contains(got[0], "status=down") {
		t.Fatalf("heartbeats %v", got)
	}
	for _, c := range mock.Calls {
		if strings.Contains(c, "s3 rm") {
			t.Fatalf("pruned after failures: %s", c)
		}
	}
}

// ---- backup before destructive operations ----

// The safety backup failing refuses the operation and names the way past it.
func TestPreOperationBackupRefuses(t *testing.T) {
	mock := ssh.NewMockExecutor("h",
		ssh.MockCommand{Match: "which aws", Err: errors.New("exit status 1")},
		ssh.MockCommand{Match: "docker inspect -f '{{.Config.Image}}'", Output: "postgres:17\n"},
		ssh.MockCommand{Match: "docker inspect -f '{{range .Config.Env}}", Output: "PG_MAJOR=17\n"},
		ssh.MockCommand{Match: "docker inspect -f", Output: "{}\n[]\n[]\n"},
		ssh.MockCommand{Match: "python3 -c", Output: `{"seq":1,"ts":"2026-01-01T00:00:00Z"}`},
	)
	client := backup.NewClient(mock, io.Discard)
	_, err := preOperationBackup(context.Background(), client, mock, "a", "db", backup.S3Config{Bucket: "b", Region: "r"}, "this restore", preBackupScope{})
	if err == nil || !strings.Contains(err.Error(), "--no-backup") || !strings.Contains(err.Error(), "nothing was changed") {
		t.Fatalf("err = %v", err)
	}
}

// No such container: nothing to protect, not a refusal.
func TestPreOperationBackupNothingToProtect(t *testing.T) {
	mock := ssh.NewMockExecutor("h", ssh.MockCommand{Match: "docker inspect -f", Err: errors.New("exit status 1: Error: No such object: a-db")})
	id, err := preOperationBackup(context.Background(), backup.NewClient(mock, io.Discard), mock, "a", "db", backup.S3Config{Bucket: "b", Region: "r"}, "this restore", preBackupScope{})
	if err != nil || id != "" {
		t.Fatalf("%q %v", id, err)
	}
}

// A database that is already gone has nothing to protect: restoring it after it
// was dropped must not fail the same way the database did.
func TestPreOperationBackupOfADroppedDatabaseIsNotARefusal(t *testing.T) {
	mock := ssh.NewMockExecutor("h",
		ssh.MockCommand{Match: "which aws", Output: "/usr/bin/aws\n"},
		ssh.MockCommand{Match: "docker inspect -f '{{.Config.Image}}'", Output: "postgres:17\n"},
		ssh.MockCommand{Match: "docker inspect -f '{{range .Config.Env}}", Output: "PG_MAJOR=17\nPOSTGRES_DB=app\n"},
		ssh.MockCommand{Match: "docker inspect -f", Output: "{}\n[]\n[]\n"},
		ssh.MockCommand{Match: "umask 077", Output: "/var/tmp/teploy-backup.t\n"},
		ssh.MockCommand{Match: "docker exec 'a-db' psql -X -qtA -v ON_ERROR_STOP=1 -U 'postgres' -d 'postgres' -c 'SELECT datname", Output: "postgres\n"},
		ssh.MockCommand{Match: "docker exec", Output: "0\n"},
		ssh.MockCommand{Match: "rm -rf", Output: ""},
		ssh.MockCommand{Match: "{ docker", Output: ""},
		ssh.MockCommand{Match: "aws s3", Output: ""},
		ssh.MockCommand{Match: "sha256sum", Output: "abc  f\n4096\n"},
		ssh.MockCommand{Match: "aws s3api", Output: `{"ContentLength":4096,"Metadata":{"sha256":"abc"}}`},
		ssh.MockCommand{Match: "python3 -c", Output: `{"seq":1,"ts":"2026-01-01T00:00:00Z"}`},
	)
	id, err := preOperationBackup(context.Background(), backup.NewClient(mock, io.Discard), mock, "a", "db", backup.S3Config{Bucket: "b", Region: "r"}, "this restore", preBackupScope{})
	if err != nil || id != "" {
		t.Fatalf("a dropped database must not refuse the restore that brings it back: %q %v", id, err)
	}
}

func TestCronMarkerRecognisesThePolicyLine(t *testing.T) {
	m := backup.FindCronMarker("5 * * * * /usr/local/bin/teploy backup run --all # [teploy-backup-all]")
	if m == nil || m.Kind != backup.KindFleet || m.Form != backup.CronFormRun || m.Cron != "5 * * * *" {
		t.Fatalf("%+v", m)
	}
}

func TestPolicyDTOCarriesNoToken(t *testing.T) {
	d := policyDTO("h", backup.FleetPolicy{V: "1", HeartbeatURLs: []string{"https://dash.example/api/push/SECRET"}}, "")
	b, _ := json.Marshal(d)
	if strings.Contains(string(b), "SECRET") || d.HeartbeatCount != 1 || d.HeartbeatHosts[0] != "https://dash.example" {
		t.Fatalf("%s", b)
	}
}

func TestEnsureServerPathCompletesCronPath(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/bin")
	ensureServerPath()
	if !strings.Contains(os.Getenv("PATH"), "/usr/local/bin") {
		t.Fatal(os.Getenv("PATH"))
	}
}

// The machine path: the whole policy on stdin. It is validated before anything is
// stored, gets exactly one marked cron line, and is reported back with no token.
func TestSetBackupPolicyFromStdin(t *testing.T) {
	t.Setenv("TEPLOY_SERVER_BINARY", "/opt/teploy")
	mock := ssh.NewMockExecutor("srv",
		ssh.MockCommand{Match: "if [ -d /etc/teploy ]", Output: "path /etc/teploy/backup-policy.json\n" + `{"v":"1"}`},
		ssh.MockCommand{Match: "for p in /etc/teploy/backup-target.json", Output: "path /etc/teploy/backup-target.json\n" + `{"v":"1","bucket":"b","region":"r"}`},
		ssh.MockCommand{Match: "sched_daemon=no", Output: "daemon=yes\nenabled=enabled\ncrontab=yes\nteploy_entries=0\n"},
		ssh.MockCommand{Match: "which age", Output: "/usr/bin/age"},
		ssh.MockCommand{Match: "'/opt/teploy' backup run --help", Output: ""},
		ssh.MockCommand{Match: "e=/etc/teploy/backup-policy.json", Output: "written /etc/teploy/backup-policy.json\n"},
		ssh.MockCommand{Match: "raw=$(crontab -l", Output: ""},
		ssh.MockCommand{Match: "python3 -c", Output: `{"seq":1,"ts":"2026-01-01T00:00:00Z"}`},
	)
	p := backup.FleetPolicy{Interval: "1h", Retention: backup.FleetRetention{KeepDaily: 7}, Recipient: testRecipient,
		HeartbeatURLs: []string{"https://dash.example/api/push/SECRET"},
		Adopted:       []backup.AdoptedEntry{{Container: "omni_postgres", Databases: backup.DatabaseSelector{All: true}}}}
	stdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	err := setBackupPolicyOn(context.Background(), &cobra.Command{}, mock, &Flags{JSON: true}, &policySetFlags{stdin: true}, &p, nil)
	w.Close()
	os.Stdout = stdout
	if err != nil {
		t.Fatal(err)
	}
	printed, _ := io.ReadAll(r)
	if strings.Contains(string(printed), "SECRET") || !strings.Contains(string(printed), `"heartbeat_count":1`) || !strings.Contains(string(printed), `"encrypted":true`) {
		t.Fatalf("%s", printed)
	}
	stored := strings.Join(mock.Inputs, "\n")
	if !strings.Contains(stored, "SECRET") || !strings.Contains(stored, `"v":"1"`) {
		t.Fatalf("the policy must reach the server's file: %s", stored)
	}
	lines := 0
	for _, c := range mock.Calls {
		if strings.Contains(c, "/opt/teploy backup run --all # [teploy-backup-all]") {
			lines++
		}
		if strings.Contains(c, "SECRET") {
			t.Fatalf("a token reached a command line: %s", c)
		}
	}
	if lines != 1 {
		t.Fatalf("cron lines: %d\n%v", lines, mock.Calls)
	}

	// Refused before anything is stored.
	bad := backup.FleetPolicy{Adopted: []backup.AdoptedEntry{{Container: "x'; rm -rf /", Databases: backup.DatabaseSelector{All: true}}}}
	refused := ssh.NewMockExecutor("srv", ssh.MockCommand{Match: "if [ -d /etc/teploy ]", Output: "absent\n"})
	if err := setBackupPolicyOn(context.Background(), &cobra.Command{}, refused, &Flags{}, &policySetFlags{stdin: true}, &bad, nil); err == nil {
		t.Fatal("a hostile container name must be refused")
	}
	if len(refused.Calls) != 1 {
		t.Fatalf("refused policy touched the server: %v", refused.Calls)
	}
}

// Per-database rows reach Dash only when asked for: it restores a row by id and
// accessory alone, and restoring a database row that way replaces the primary.
func TestFilterDatabaseRows(t *testing.T) {
	rows := []backup.ListedBackup{{ID: "1"}, {ID: "1", Database: "analytics"}}
	got, hidden := filterDatabaseRows(rows, false)
	if len(got) != 1 || hidden != 1 || got[0].Database != "" {
		t.Fatalf("%+v %d", got, hidden)
	}
	got, hidden = filterDatabaseRows(rows, true)
	if len(got) != 2 || hidden != 0 {
		t.Fatalf("%+v %d", got, hidden)
	}
	if got, _ := filterDatabaseRows(nil, false); got == nil {
		t.Fatal("must encode as []")
	}
}

// A --no-credentials target has nothing to decrypt.
func TestTargetS3StoredOnlyWithCredentials(t *testing.T) {
	if cfg := TargetS3(backup.Target{Bucket: "b", Path: "/etc/teploy/backup-target.json"}); cfg.StoredTarget != "" {
		t.Fatalf("%+v", cfg)
	}
	if cfg := TargetS3(backup.Target{Bucket: "b", Path: "/p", HasCredentials: true}); cfg.StoredTarget != "/p" {
		t.Fatalf("%+v", cfg)
	}
}

// Zero targets is a failure unless the policy allows it.
func TestRunFleetZeroTargets(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = append(got, r.URL.RequestURI()) }))
	defer srv.Close()
	dir := t.TempDir()
	mk := func(extra string) *ssh.MockExecutor {
		return ssh.NewMockExecutor("h",
			ssh.MockCommand{Match: "if [ -d /etc/teploy ]", Output: "path " + dir + "/backup-policy.json\n" + `{"v":"1","interval":"1h"` + extra + `,"heartbeat_urls":["` + srv.URL + `/hb"]}`},
			ssh.MockCommand{Match: "for p in /etc/teploy/backup-target.json", Output: "path /etc/teploy/backup-target.json\n" + `{"v":"1","bucket":"b","region":"r"}`},
			ssh.MockCommand{Match: "which aws", Output: "/usr/bin/aws\n"},
			ssh.MockCommand{Match: "for d in", Output: ""},
			ssh.MockCommand{Match: "ids=$(docker ps -aq", Output: ""},
			ssh.MockCommand{Match: "crontab -l", Output: ""},
			ssh.MockCommand{Match: "python3 -c", Output: `{"seq":1,"ts":"2026-01-01T00:00:00Z"}`},
		)
	}
	res, err := runFleet(context.Background(), mk(""), io.Discard, false, nil)
	if err == nil || res.Status != backup.RunFailed || !strings.Contains(res.Error, "allow_empty") {
		t.Fatalf("%v %+v", err, res)
	}
	if !strings.Contains(got[len(got)-1], "status=down") {
		t.Fatalf("%v", got)
	}
	res, err = runFleet(context.Background(), mk(`,"allow_empty":true`), io.Discard, false, nil)
	if err != nil || res.Status != backup.RunOK {
		t.Fatalf("%v %+v", err, res)
	}
}

// A server set up with nothing on it keeps allow_empty after it stops being true, and
// then losing every target reports ok.
func TestRunFleetWarnsAboutAllowEmptyWithTargets(t *testing.T) {
	dir := t.TempDir()
	line, _ := json.Marshal(map[string]any{"name": "/a-db", "image": "postgres:17", "state": "running",
		"labels": map[string]string{"teploy.role": "accessory", "teploy.app": "a", "teploy.accessory": "db"},
		"env":    []string{"PG_MAJOR=17"}, "entrypoint": []string{}, "cmd": []string{}})
	mock := ssh.NewMockExecutor("h",
		ssh.MockCommand{Match: "if [ -d /etc/teploy ]", Output: "path " + dir + "/backup-policy.json\n" + `{"v":"1","interval":"1h","allow_empty":true,"excludes":["a/db"]}`},
		ssh.MockCommand{Match: "for p in /etc/teploy/backup-target.json", Output: "path /etc/teploy/backup-target.json\n" + `{"v":"1","bucket":"b","region":"r"}`},
		ssh.MockCommand{Match: "which aws", Output: "/usr/bin/aws\n"},
		ssh.MockCommand{Match: "for d in", Output: ""},
		ssh.MockCommand{Match: "ids=$(docker ps -aq", Output: string(line) + "\n"},
		ssh.MockCommand{Match: "crontab -l", Output: ""},
		ssh.MockCommand{Match: "python3 -c", Output: `{"seq":1,"ts":"2026-01-01T00:00:00Z"}`},
	)
	res, _ := runFleet(context.Background(), mock, io.Discard, false, nil)
	if !strings.Contains(strings.Join(res.Warnings, "\n"), "allow_empty is set") {
		t.Fatalf("%+v", res)
	}
}
