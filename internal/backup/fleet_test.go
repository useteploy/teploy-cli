package backup

import (
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

	"github.com/useteploy/teploy/internal/ssh"
)

// ---- policy ----

func TestFleetPolicyValidate(t *testing.T) {
	ok := FleetPolicy{V: "1", Interval: "1h", Recipient: testOwnerRecipient, Adopted: []AdoptedEntry{{Container: "omni_postgres", Engine: "postgres", Databases: DatabaseSelector{All: true}}}, Excludes: []string{"app", "app/db", "omni_x"}}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, p := range map[string]FleetPolicy{
		"interval":      {Interval: "1 2 3"},
		"retention":     {Retention: FleetRetention{KeepDaily: -1}},
		"recipient":     {Recipient: "age1short"},
		"container":     {Adopted: []AdoptedEntry{{Container: "x'; rm -rf /", Databases: DatabaseSelector{All: true}}}},
		"twice":         {Adopted: []AdoptedEntry{{Container: "x", Databases: DatabaseSelector{All: true}}, {Container: "x", Databases: DatabaseSelector{All: true}}}},
		"engine":        {Adopted: []AdoptedEntry{{Container: "x", Engine: "generic", Databases: DatabaseSelector{All: true}}}},
		"no-databases":  {Adopted: []AdoptedEntry{{Container: "x"}}},
		"database":      {Adopted: []AdoptedEntry{{Container: "x", Databases: DatabaseSelector{Names: []string{"--all"}}}}},
		"exclude":       {Excludes: []string{"a/b/c"}},
		"exclude-shell": {Excludes: []string{"$(x)"}},
		"heartbeat":     {HeartbeatURLs: []string{"ftp://h/token"}},
	} {
		if err := p.Validate(); err == nil {
			t.Errorf("%s: expected a refusal", name)
		}
	}
	// A refused heartbeat URL never echoes itself: it is a secret.
	err := (FleetPolicy{HeartbeatURLs: []string{"ftp://h/TOKEN"}}).Validate()
	if strings.Contains(err.Error(), "TOKEN") {
		t.Fatalf("error leaks the URL: %v", err)
	}
}

func TestDatabaseSelectorJSON(t *testing.T) {
	var d DatabaseSelector
	if err := json.Unmarshal([]byte(`"all"`), &d); err != nil || !d.All {
		t.Fatalf("%+v %v", d, err)
	}
	if err := json.Unmarshal([]byte(`["a","b"]`), &d); err != nil || d.All || len(d.Names) != 2 {
		t.Fatalf("%+v %v", d, err)
	}
	if err := json.Unmarshal([]byte(`"some"`), &d); err == nil {
		t.Fatal(`only "all" is a string value`)
	}
	b, _ := json.Marshal(DatabaseSelector{All: true})
	if string(b) != `"all"` {
		t.Fatalf("%s", b)
	}
}

func TestIntervalCron(t *testing.T) {
	for _, iv := range []string{"15m", "30m", "1h", "hourly", "2h", "3h", "4h", "6h", "8h", "12h", "24h", "1d", "daily", "7d", "1w", "weekly"} {
		c, err := IntervalCron(iv, "srv")
		if err != nil {
			t.Fatalf("%s: %v", iv, err)
		}
		if err := ValidateSchedule(c); err != nil {
			t.Fatalf("%s -> %q: %v", iv, c, err)
		}
	}
	a, _ := IntervalCron("1h", "srv-a")
	again, _ := IntervalCron("1h", "srv-a")
	if a != again {
		t.Fatal("the minute must be stable per server")
	}
	if _, err := IntervalCron("5m", "x"); err == nil {
		t.Fatal("5m is not offered")
	}
	if (FleetPolicy{Interval: "6h"}).IntervalDuration() != 6*time.Hour {
		t.Fatal("duration")
	}
}

func TestExcludedMatches(t *testing.T) {
	p := FleetPolicy{Excludes: []string{"app1", "app2/db", "app3/volumes", "omni_x"}}
	for _, tc := range []struct {
		app, name, container string
		want                 bool
	}{
		{"app1", "db", "app1-db", true},
		{"app1", "volumes", "", true},
		{"app2", "db", "app2-db", true},
		{"app2", "cache", "app2-cache", false},
		{"app3", "volumes", "", true},
		{"app3", "db", "app3-db", false},
		{AdoptedApp, "omni_x", "omni_x", true},
		{AdoptedApp, "other", "other", false},
		{"", "", "omni_x", true},
	} {
		if got := p.Excluded(tc.app, tc.name, tc.container); got != tc.want {
			t.Errorf("Excluded(%q,%q,%q) = %v", tc.app, tc.name, tc.container, got)
		}
	}
}

func TestReadFleetPolicyFailsClosed(t *testing.T) {
	mock := ssh.NewMockExecutor("h", ssh.MockCommand{Match: "for p in", Err: errors.New("exit status 3: cannot read /etc/teploy/backup-policy.json")})
	if _, err := ReadFleetPolicy(context.Background(), mock); err == nil {
		t.Fatal("an unreadable policy must not read as no policy")
	}
	var cfg S3Config
	if err := ApplyPolicyEncryption(context.Background(), mock, &cfg); err == nil {
		t.Fatal("an unreadable policy must refuse, not go on unencrypted")
	}
	mock = ssh.NewMockExecutor("h", ssh.MockCommand{Match: "for p in", Output: "path /etc/teploy/backup-policy.json\n" + `{"v":"1","recipient":"` + testOwnerRecipient + `"}`})
	if err := ApplyPolicyEncryption(context.Background(), mock, &cfg); err != nil || cfg.OwnerRecipient != testOwnerRecipient {
		t.Fatalf("%v %+v", err, cfg)
	}
}

// ---- enumeration ----

func inventoryLine(name, image, state string, labels map[string]string, env ...string) string {
	b, _ := json.Marshal(map[string]any{"name": "/" + name, "image": image, "state": state, "labels": labels, "env": env, "entrypoint": []string{}, "cmd": []string{}})
	return string(b)
}

func TestEnumerateFleet(t *testing.T) {
	labels := func(app, acc string) map[string]string {
		return map[string]string{"teploy.role": "accessory", "teploy.app": app, "teploy.accessory": acc}
	}
	inv := strings.Join([]string{
		inventoryLine("a-db", "postgres:17", "running", labels("a", "db"), "PG_MAJOR=17"),
		inventoryLine("b-db", "postgres:17", "running", labels("b", "db"), "PG_MAJOR=17"),
		inventoryLine("c-db", "postgres:17", "running", labels("c", "db"), "PG_MAJOR=17"),
		inventoryLine("wrongname", "postgres:17", "running", labels("d", "db"), "PG_MAJOR=17"),
		inventoryLine("e-db", "postgres:17", "running", labels("e;x", "db"), "PG_MAJOR=17"),
		inventoryLine("omni_postgres", "postgres:17", "running", nil, "PG_MAJOR=17"),
		inventoryLine("stray", "redis:7", "running", nil, "REDIS_VERSION=7"),
		inventoryLine("web", "nginx", "running", map[string]string{"teploy.role": "web", "teploy.app": "a"}),
		inventoryLine("teploy-backup-verify-x", "postgres:17", "running", nil, "PG_MAJOR=17"),
	}, "\n")
	mock := ssh.NewMockExecutor("h",
		ssh.MockCommand{Match: "ids=$(docker ps -aq", Output: inv},
		ssh.MockCommand{Match: "crontab -l", Output: "0 3 * * * /usr/local/bin/teploy backup run --app b --accessory db # [teploy-accessory-backup:b:db]\n0 4 * * * /opt/hand.sh # [teploy-accessory-backup:c:db]\n"},
		ssh.MockCommand{Match: "cat", Output: ""},
		ssh.MockCommand{Match: "for d in /deployments/*/volumes", Output: "100\ta\n99999999\tbig\n50\tgone\n"},
	)
	p := FleetPolicy{Retention: FleetRetention{KeepLast: 3}, Volumes: true, VolumeMaxGB: 1, Excludes: []string{"gone/volumes"},
		Adopted: []AdoptedEntry{{Container: "omni_postgres", Databases: DatabaseSelector{All: true}}, {Container: "missing", Databases: DatabaseSelector{All: true}}}}
	targets, uncovered, err := NewClient(mock, io.Discard).EnumerateFleet(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(uncovered, ",") != "stray" {
		t.Fatalf("uncovered = %v", uncovered)
	}
	byName := map[string]FleetTarget{}
	for _, tr := range targets {
		n := tr.Container
		if n == "" {
			n = tr.App + "/volumes"
		}
		byName[n] = tr
	}
	if tr := byName["a-db"]; tr.Status != "" || tr.Retention.KeepLast != 3 {
		t.Fatalf("a-db: %+v", tr)
	}
	if tr := byName["b-db"]; tr.Status != TargetOwnCron {
		t.Fatalf("a per-app schedule overrides the policy: %+v", tr)
	}
	if tr := byName["c-db"]; tr.Status != "" {
		t.Fatalf("a hand-written job under a marker is no override: %+v", tr)
	}
	if tr := byName["wrongname"]; tr.Status != TargetFailed {
		t.Fatalf("mislabelled: %+v", tr)
	}
	if tr := byName["e-db"]; tr.Status != TargetFailed {
		t.Fatalf("hostile label: %+v", tr)
	}
	if tr := byName["omni_postgres"]; tr.Status != "" || tr.App != AdoptedApp || tr.Fact == nil {
		t.Fatalf("adopted: %+v", tr)
	}
	if tr := byName["missing"]; tr.Status != TargetFailed || !strings.Contains(tr.Reason, "does not exist") {
		t.Fatalf("an adopted container that is gone is a failure: %+v", tr)
	}
	if tr := byName["a/volumes"]; tr.Status != "" || tr.SizeBytes != 100*1024 {
		t.Fatalf("volumes: %+v", tr)
	}
	if tr := byName["big/volumes"]; tr.Status != TargetExcluded || !tr.SizeGuard {
		t.Fatalf("size guard: %+v", tr)
	}
	if tr := byName["gone/volumes"]; tr.Status != TargetExcluded || tr.SizeGuard {
		t.Fatalf("exclude: %+v", tr)
	}
	if _, ok := byName["teploy-backup-verify-x"]; ok {
		t.Fatal("a verify scratch is not a target")
	}
}

// A dead docker daemon must not read as an empty server: a run with no targets
// reports success.
func TestInventoryDockerDownIsAnError(t *testing.T) {
	mock := ssh.NewMockExecutor("h", ssh.MockCommand{Match: "ids=$(docker ps -aq", Err: errors.New("exit status 1: cannot connect to the docker daemon")})
	if _, err := NewClient(mock, io.Discard).Inventory(context.Background()); err == nil {
		t.Fatal("must be an error")
	}
	if !strings.Contains(mock.Calls[0], "|| exit 1") || strings.Contains(mock.Calls[0], "| xargs") {
		t.Fatalf("the status of docker ps must be checked: %s", mock.Calls[0])
	}
}

func TestSummarizeFleet(t *testing.T) {
	stored := []BackupObject{{Key: "k"}}
	r := FleetRunResult{Targets: []FleetTargetResult{{Status: TargetOK, Container: "a", Objects: stored}, {Status: TargetExcluded, Container: "b", Reason: "excluded by policy"}}, UncoveredContainers: []string{"c"}}
	SummarizeFleet(&r)
	if r.Status != RunOK || len(r.Omissions) != 2 {
		t.Fatalf("%+v", r)
	}
	// An ok target that stored nothing is named: "ok" alone would hide it.
	r = FleetRunResult{Targets: []FleetTargetResult{{Status: TargetOK, Container: "empty"}}}
	SummarizeFleet(&r)
	if r.Status != RunOK || !strings.Contains(strings.Join(r.Omissions, ""), "nothing was stored") {
		t.Fatalf("%+v", r)
	}
	r = FleetRunResult{Targets: []FleetTargetResult{{Status: TargetOK, Container: "a", Objects: stored, UncoveredDatabases: []string{"x"}}, {Status: TargetFailed, Container: "b"}}}
	SummarizeFleet(&r)
	if r.Status != RunPartial || !strings.Contains(strings.Join(r.Omissions, ""), "database x") {
		t.Fatalf("%+v", r)
	}
	r = FleetRunResult{Targets: []FleetTargetResult{{Status: TargetFailed, Container: "b"}}}
	SummarizeFleet(&r)
	if r.Status != RunFailed {
		t.Fatalf("%+v", r)
	}
}

// RunFleetTarget prunes nothing when the backup failed, and a failing prune does
// not fail what was backed up.
func TestRunFleetTargetNeverPrunesAFailedBackup(t *testing.T) {
	mock := newStoreMock("h",
		ssh.MockCommand{Match: "which aws", Err: errors.New("exit status 1")},
		ssh.MockCommand{Match: "aws s3", Output: ""},
	)
	r := NewClient(mock, io.Discard).RunFleetTarget(context.Background(), FleetTarget{Kind: TargetVolumes, App: "a", Retention: RetentionPolicy{KeepLast: 1}}, S3Config{Bucket: "b", Region: "r"}, BackupOptions{})
	if r.Status != TargetFailed || !strings.Contains(r.Reason, "aws CLI") {
		t.Fatalf("%+v", r)
	}
	for _, c := range mock.Calls {
		if strings.Contains(c, "s3 rm") {
			t.Fatalf("pruned after a failed backup: %s", c)
		}
	}
}

// ---- heartbeat ----

func TestHeartbeatSemantics(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.URL.RequestURI())
		if strings.Contains(r.URL.Path, "bad") {
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()
	up := SendHeartbeats(context.Background(), []string{srv.URL + "/api/push/TOKEN"}, true, nil)
	if !up[0].OK || got[0] != "/api/push/TOKEN" {
		t.Fatalf("%+v %v", up, got)
	}
	got = nil
	down := SendHeartbeats(context.Background(), []string{srv.URL + "/api/push/TOKEN?x=1"}, false, nil)
	if !down[0].OK || !strings.Contains(got[0], "status=down") || !strings.Contains(got[0], "x=1") {
		t.Fatalf("%+v %v", down, got)
	}
	if u, _ := heartbeatURL("https://hc-ping.com/abc", false); u != "https://hc-ping.com/abc/fail" {
		t.Fatalf("healthchecks: %s", u)
	}
	// Failures say so with no token anywhere, and do not panic or block.
	dead := SendHeartbeats(context.Background(), []string{"http://127.0.0.1:1/SECRET", srv.URL + "/bad/SECRET"}, true, &http.Client{Timeout: time.Second})
	for _, r := range dead {
		if r.OK || r.Error == "" {
			t.Fatalf("%+v", r)
		}
		if strings.Contains(r.Error, "SECRET") || strings.Contains(r.Host, "SECRET") {
			t.Fatalf("the token leaked: %+v", r)
		}
	}
}

func TestMaskedHeartbeats(t *testing.T) {
	got := MaskedHeartbeats([]string{"https://dash.example/api/push/TOKEN?x=y"})
	if got[0] != "https://dash.example/..." {
		t.Fatal(got)
	}
}

// ---- run lock ----

func TestRunLockOverlapAndRelease(t *testing.T) {
	p := filepath.Join(t.TempDir(), "lock")
	now := time.Now()
	l, _, err := AcquireRunLock(p, now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	_, holder, err := AcquireRunLock(p, now, time.Hour)
	if !errors.Is(err, ErrRunLocked) {
		t.Fatalf("second lock: %v", err)
	}
	if holder.PID != os.Getpid() {
		t.Fatalf("holder = %+v", holder)
	}
	// Past staleAfter, but the holder is not a teploy --all process: never killed.
	if _, _, err := AcquireRunLock(p, now.Add(48*time.Hour), time.Hour); !errors.Is(err, ErrRunLocked) {
		t.Fatalf("a recycled pid must not be killed: %v", err)
	}
	l.Release()
	l2, _, err := AcquireRunLock(p, now, time.Hour)
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	l2.Release()
	// A lock file left by a dead process blocks nothing: only a held flock does.
	if err := os.WriteFile(p, []byte(`{"pid":1,"started_at":"2020-01-01T00:00:00Z"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	l3, _, err := AcquireRunLock(p, now, time.Hour)
	if err != nil {
		t.Fatalf("a stale file must not block: %v", err)
	}
	l3.Release()
}

// ---- status ----

func TestFleetStatusCoverage(t *testing.T) {
	labels := map[string]string{"teploy.role": "accessory", "teploy.app": "a", "teploy.accessory": "db"}
	inv := strings.Join([]string{
		inventoryLine("a-db", "postgres:17", "exited", labels, "PG_MAJOR=17", "POSTGRES_PASSWORD=SECRETPW"),
		inventoryLine("b_mysql", "mysql:8", "exited", nil, "MYSQL_VERSION=8", "MYSQL_ROOT_PASSWORD=SECRETPW"),
		inventoryLine("stray", "redis:7", "running", nil, "REDIS_VERSION=7"),
	}, "\n")
	now := mustTime(t, "20260201-120000")
	evA := `{"v":1,"id":"1","seq":1,"ts":"2026-02-01T11:00:00Z","app":"a","kind":"backup.run","outcome":"succeeded","detail":{"accessory":"db","kind":"accessory"}}`
	evAd := `{"v":1,"id":"2","seq":1,"ts":"2026-01-01T11:00:00Z","app":"_adopted","kind":"backup.run","outcome":"failed","detail":{"accessory":"b_mysql","kind":"accessory","error":"boom"}}`
	mock := ssh.NewMockExecutor("h",
		ssh.MockCommand{Match: "for p in /etc/teploy/backup-policy.json", Output: "path /etc/teploy/backup-policy.json\n" + `{"v":"1","interval":"1h","recipient":"` + testOwnerRecipient + `","heartbeat_urls":["https://d/api/push/SECRETTOKEN"],"adopted":[{"container":"b_mysql","databases":"all"},{"container":"gone","databases":"all"}]}`},
		ssh.MockCommand{Match: "for p in /etc/teploy/backup-target.json", Output: "path /etc/teploy/backup-target.json\n" + `{"v":"1","bucket":"bk","region":"r","prefix":"p","credentials":"SECRETARMOR"}`},
		ssh.MockCommand{Match: "command -v aws >/dev/null", Output: "aws\nage\n"},
		ssh.MockCommand{Match: "crontab -l", Output: "5 * * * * /usr/local/bin/teploy backup run --all # [teploy-backup-all]\n"},
		ssh.MockCommand{Match: "ids=$(docker ps -aq", Output: inv},
		ssh.MockCommand{Match: "which aws", Output: "/usr/bin/aws\n"},
		ssh.MockCommand{Match: "( c=$(python3", Output: "2026-02-01 11:00:00 1000 p/a/accessories/db/20260201-110000.pgdump.age\n"},
		ssh.MockCommand{Match: "python3 -c", Output: evA + "\n" + evAd + "\n"},
		ssh.MockCommand{Match: "for d in /deployments/*/volumes", Output: ""},
		ssh.MockCommand{Match: "for d in /var/tmp/teploy-backup.", Output: "/var/tmp/teploy-backup.old\n"},
	)
	st, err := ReadFleetStatus(context.Background(), mock, now)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(st)
	for _, secretValue := range []string{"SECRETPW", "SECRETTOKEN", "SECRETARMOR"} {
		if strings.Contains(string(b), secretValue) {
			t.Fatalf("status leaks %s: %s", secretValue, b)
		}
	}
	entry := map[string]StatusEntry{}
	for _, e := range st.Entries {
		entry[e.Name] = e
	}
	if e := entry["a-db"]; e.Coverage != CoverageCovered || e.Schedule != "policy" || e.Newest == nil || !e.Newest.Encrypted {
		t.Fatalf("a-db: %+v", e)
	}
	if e := entry["stray"]; e.Coverage != CoverageUncovered || !e.Unmanaged || e.Kind != "unmanaged" {
		t.Fatalf("stray: %+v", e)
	}
	if e := entry["gone"]; e.Coverage != CoverageFailing || e.State != "missing" {
		t.Fatalf("gone: %+v", e)
	}
	if !st.Policy.CronInstalled || st.Policy.HeartbeatCount != 1 || !st.Encrypted || st.Target.Prefix != "p" || !st.Target.HasCredentials {
		t.Fatalf("summary: %+v %+v", st.Policy, st.Target)
	}
	if len(st.LeftoverWorkspaces) != 1 {
		t.Fatalf("leftovers: %v", st.LeftoverWorkspaces)
	}
	if strings.Join(st.UncoveredDatabases, ",") != "stray" {
		t.Fatalf("uncovered: %v", st.UncoveredDatabases)
	}
	if e := entry["b_mysql"]; e.Coverage != CoverageFailing || !e.Adopted || e.LastError != "boom" {
		t.Fatalf("a failed adopted container reads failing, adopted: %+v", e)
	}
	// Past twice the interval with no new success.
	later, err := ReadFleetStatus(context.Background(), mock, now.Add(3*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range later.Entries {
		if e.Name == "a-db" && e.Coverage != CoverageStale {
			t.Fatalf("stale: %+v", e)
		}
	}
}

func TestEntryObjectsAndEvents(t *testing.T) {
	var none StatusEntry
	fillEntryEvents(&none, nil, "x")
	if none.LastSuccess != nil {
		t.Fatal("no events, no success")
	}
	e := StatusEntry{}
	fillEntryObjects(&e, []ListedBackup{{Kind: KindAccessory, Accessory: "db", Restorable: false, CreatedAt: "2026-02-01T11:00:00Z", ID: "x"}}, "db")
	if e.Newest != nil {
		t.Fatal("a non-restorable object must not count as the newest")
	}
	fillEntryObjects(&e, []ListedBackup{
		{Kind: KindAccessory, Accessory: "db", Database: "b", Restorable: true, CreatedAt: "2026-02-01T11:00:00Z", ID: "x"},
		{Kind: KindAccessory, Accessory: "db", Database: "a", Restorable: true, CreatedAt: "2026-01-01T11:00:00Z", ID: "y"},
		{Kind: KindAccessory, Accessory: "db", Database: "a", Restorable: true, CreatedAt: "2025-01-01T11:00:00Z", ID: "z"},
	}, "db")
	if e.Newest == nil || e.Newest.ID != "x" || len(e.Databases) != 2 || e.Databases[0].Name != "a" || e.Databases[0].Newest.ID != "y" {
		t.Fatalf("%+v", e)
	}
	if e.LastSuccess == nil {
		t.Fatal("a good object is evidence of a success")
	}
}
