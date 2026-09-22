package preview

// C06 preview lifecycle tests: blue/green updates (the predecessor serves
// until the candidate is healthy and routed — never destroyed first) and
// TTL pruning across all apps and both record eras.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy/internal/ssh"
)

// indexOf returns the first call index containing needle, or -1.
func indexOf(calls []string, needle string) int {
	for i, c := range calls {
		if strings.Contains(c, needle) {
			return i
		}
	}
	return -1
}

// Blue/green ordering (C06 defect 1): an update must start the candidate,
// health-check it, switch the Caddy route, and only THEN stop+remove the
// predecessor. The predecessor's stop command may never precede the route
// switch — that ordering was the downtime window the main deploy path
// doesn't have.
func TestDeploy_BlueGreenOrdering(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		append(previewDeployMocks(),
			ssh.MockCommand{Match: "curl -s -o /dev/null", Output: "200"})...)
	var buf bytes.Buffer
	mgr := NewManager(mock, &buf)

	mustDeploy(t, mgr, deployCfg(loginBranch, "v1"))
	n := len(mock.Calls)
	mustDeploy(t, mgr, deployCfg(loginBranch, "v2"))
	update := mock.Calls[n:]

	predStop := indexOf(update, "docker stop -t 5 'myapp-preview-p-"+loginIDHex+"-v1'")
	if predStop == -1 {
		t.Fatalf("predecessor v1 was never stopped after the switch — retire step missing, calls: %v", update)
	}
	switchIdx := indexOf(update, "docker exec caddy caddy reload")
	if switchIdx == -1 {
		t.Fatalf("no Caddy route switch (reload) during the update, calls: %v", update)
	}
	candRun := indexOf(update, "docker run")
	if candRun == -1 {
		t.Fatalf("candidate container never started, calls: %v", update)
	}
	if !(candRun < switchIdx && switchIdx < predStop) {
		t.Errorf("blue/green order violated: candidate run=%d, route switch=%d, predecessor stop=%d (want run < switch < stop)",
			candRun, switchIdx, predStop)
	}
	// The predecessor is removed too, after the switch.
	predRm := indexOf(update, "docker rm 'myapp-preview-p-"+loginIDHex+"-v1'")
	if predRm == -1 || predRm < predStop {
		t.Errorf("predecessor removal must follow its stop (stop=%d rm=%d), calls: %v", predStop, predRm, update)
	}
	// The record now names the candidate, under the SAME canonical key and
	// stable route key.
	var s State
	if err := json.Unmarshal(mock.Files[previewStatePath("myapp", loginBranch)], &s); err != nil {
		t.Fatal(err)
	}
	if s.Container != "myapp-preview-p-"+loginIDHex+"-v2" {
		t.Errorf("record names container %q, want the v2 candidate", s.Container)
	}
	if s.Route != "myapp-preview-p-"+loginIDHex {
		t.Errorf("route key changed across the update: %q", s.Route)
	}
}

// A candidate that fails its health gate leaves the predecessor RUNNING,
// ROUTED, and RECORDED: the only teardown is the failed candidate itself,
// and the error names the candidate that failed.
func TestDeploy_FailedCandidateLeavesPredecessor(t *testing.T) {
	// The bundle's default healthy probe must answer "000" (no response)
	// for every probe EXCEPT v1's first — a One-shot "200" entry is
	// prepended so it wins while it exists; once consumed, the persistent
	// "000" takes over and the v2 candidate never becomes ready.
	mocks := []ssh.MockCommand{{Match: "curl -s -o /dev/null", Output: "200", Once: true}}
	for _, mc := range previewDeployMocks() {
		if mc.Match == "curl -s -o /dev/null" {
			mc.Output = "000"
		}
		mocks = append(mocks, mc)
	}
	mock := ssh.NewMockExecutor("1.2.3.4", mocks...)
	var buf bytes.Buffer
	mgr := NewManager(mock, &buf)
	mgr.healthTimeout = 150 * time.Millisecond
	mgr.healthInterval = 30 * time.Millisecond

	mustDeploy(t, mgr, deployCfg(loginBranch, "v1"))
	recordPath := previewStatePath("myapp", loginBranch)
	recordBefore := string(mock.Files[recordPath])
	caddyBefore := string(mock.Files["/deployments/caddy/Caddyfile"])
	n := len(mock.Calls)

	err := mgr.Deploy(context.Background(), deployCfg(loginBranch, "v2"))
	if err == nil {
		t.Fatal("a candidate that never becomes healthy must fail the deploy")
	}
	if !strings.Contains(err.Error(), "myapp-preview-p-"+loginIDHex+"-v2") {
		t.Errorf("failure must name the candidate container, got: %v", err)
	}
	if !strings.Contains(strings.ToLower(err.Error()), "health") {
		t.Errorf("failure must say the health gate failed, got: %v", err)
	}

	update := mock.Calls[n:]
	for _, forbidden := range []string{
		"docker stop -t 5 'myapp-preview-p-" + loginIDHex + "-v1'",
		"docker rm 'myapp-preview-p-" + loginIDHex + "-v1'",
	} {
		if indexOf(update, forbidden) != -1 {
			t.Errorf("predecessor touched during a failed candidate update: %q ran", forbidden)
		}
	}
	if got := string(mock.Files[recordPath]); got != recordBefore {
		t.Errorf("predecessor record was modified:\nbefore: %s\nafter:  %s", recordBefore, got)
	}
	if got := string(mock.Files["/deployments/caddy/Caddyfile"]); got != caddyBefore {
		t.Errorf("route was modified during a failed candidate update:\nbefore: %s\nafter:  %s", caddyBefore, got)
	}
	// The failed candidate itself is cleaned up (stopped + removed).
	for _, want := range []string{
		"docker stop -t 5 'myapp-preview-p-" + loginIDHex + "-v2'",
		"docker rm 'myapp-preview-p-" + loginIDHex + "-v2'",
	} {
		if indexOf(update, want) == -1 {
			t.Errorf("failed candidate must be cleaned up, missing %q in: %v", want, update)
		}
	}
}

// Same-version updates keep working: the running predecessor shares the
// candidate's name, so it is renamed aside before the candidate starts and
// retired after the switch (the main engine's same-version pattern).
func TestDeploy_SameVersionUpdate(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		append(previewDeployMocks(),
			ssh.MockCommand{Match: "curl -s -o /dev/null", Output: "200"},
			ssh.MockCommand{Match: "docker rename", Output: ""})...)
	var buf bytes.Buffer
	mgr := NewManager(mock, &buf)

	mustDeploy(t, mgr, deployCfg(loginBranch, "v1"))
	n := len(mock.Calls)
	mustDeploy(t, mgr, deployCfg(loginBranch, "v1"))
	update := mock.Calls[n:]

	rename := indexOf(update, "docker rename 'myapp-preview-p-"+loginIDHex+"-v1' 'myapp-preview-p-"+loginIDHex+"-v1-replaced'")
	if rename == -1 {
		t.Fatalf("same-version update must rename the predecessor aside first, calls: %v", update)
	}
	run := indexOf(update, "docker run")
	if run < rename {
		t.Errorf("candidate started before the rename freed the name (rename=%d run=%d)", rename, run)
	}
	// A same-version switch needs no Caddyfile edit — the rendered block is
	// byte-identical, and mutate() correctly skips no-op writes — so the
	// handoff is the NAME: the candidate holds it (healthy, running) before
	// the renamed predecessor is retired.
	probe := indexOf(update, "curl -s -o /dev/null")
	stop := indexOf(update, "docker stop -t 5 'myapp-preview-p-"+loginIDHex+"-v1-replaced'")
	if probe == -1 || stop == -1 || stop < probe {
		t.Errorf("renamed predecessor must be retired AFTER the healthy candidate holds the name (probe=%d stop=%d)", probe, stop)
	}
	if stop < run {
		t.Errorf("renamed predecessor retired before the candidate started (run=%d stop=%d)", run, stop)
	}
}

// canonicalRecordJSON renders a modern-era record for seeding.
func canonicalRecordJSON(app, idHex, branch, container, domain string, expires time.Time) string {
	return fmt.Sprintf(`{"id":%q,"branch":%q,"repo":"github.com/tyler/myapp","route":"%s-preview-p-%s","domain":%q,"port":49200,"container":%q,"image":"myapp:v1","created_at":"2020-01-01T00:00:00Z","expires_at":%q}`,
		app+"-p-"+idHex, branch, app, idHex, domain, container, expires.Format(time.RFC3339))
}

// Prune removes EXACTLY the expired set across BOTH record eras, and a
// second run is a no-op (idempotent).
func TestPrune_RemovesExactlyExpiredBothErasIdempotent(t *testing.T) {
	expiredAt := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	freshAt := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)

	// myapp: canonical expired + canonical fresh.
	myExpired := previewStatePath("myapp", loginBranch)
	myFresh := previewStatePath("myapp", dashBranch)
	// otherapp: legacy expired (slug-keyed file) + canonical fresh.
	otherExpired := legacyPreviewStatePath("otherapp", "legacy-feature")
	otherFresh := previewStatePath("otherapp", "fresh-feature")

	destroyMocks := []ssh.MockCommand{
		// Per-app record listings (PruneAll enumerates apps first).
		ssh.MockCommand{Match: "ls -d /deployments/*/previews", Output: "/deployments/myapp/previews\n/deployments/otherapp/previews"},
		ssh.MockCommand{Match: "ls /deployments/myapp/previews/*.json", Output: myExpired + "\n" + myFresh},
		ssh.MockCommand{Match: "ls /deployments/otherapp/previews/*.json", Output: otherExpired + "\n" + otherFresh},
		// Destroy's Caddyfile transaction (see previewDeployMocks).
		ssh.MockCommand{Match: "cat /deployments/caddy/Caddyfile", Output: "{\n\tadmin 0.0.0.0:2019\n}\n"},
		ssh.MockCommand{Match: "mkdir /deployments/caddy/.lock", Output: ""},
		ssh.MockCommand{Match: "a=$(docker exec caddy md5sum", Output: "TEPLOY_CADDY_OK"},
		ssh.MockCommand{Match: "docker exec caddy caddy reload", Output: ""},
		ssh.MockCommand{Match: "rmdir /deployments/caddy/.lock", Output: ""},
	}
	mock := ssh.NewMockExecutor("1.2.3.4", destroyMocks...)
	mock.Files[myExpired] = []byte(canonicalRecordJSON("myapp", loginIDHex, loginBranch,
		"myapp-preview-p-"+loginIDHex+"-v1", "preview-feature-login-"+loginIDHex+".myapp.com", expiredAt))
	mock.Files[myFresh] = []byte(canonicalRecordJSON("myapp", dashIDHex, dashBranch,
		"myapp-preview-p-"+dashIDHex+"-v1", "preview-feature-login-"+dashIDHex+".myapp.com", freshAt))
	mock.Files[otherExpired] = []byte(`{"branch":"legacy-feature","domain":"preview-legacy-feature.otherapp.com","port":49200,"container":"otherapp-preview-legacy-feature-v1","image":"myapp:v1","created_at":"2020-01-01T00:00:00Z","expires_at":"2020-01-02T00:00:00Z"}`)
	mock.Files[otherFresh] = []byte(canonicalRecordJSON("otherapp", "abcd1234", "fresh-feature",
		"otherapp-preview-p-abcd1234-v1", "preview-fresh-feature-abcd1234.otherapp.com", freshAt))
	// A non-preview deployment resource that must never be touched.
	mock.Files["/deployments/myapp/state.json"] = []byte(`{"app":"myapp"}`)

	var buf bytes.Buffer
	mgr := NewManager(mock, &buf)
	pruned, err := mgr.PruneAll(context.Background())
	if err != nil {
		t.Fatalf("PruneAll: %v", err)
	}
	if pruned != 2 {
		t.Fatalf("pruned %d previews, want exactly the 2 expired ones", pruned)
	}
	for _, path := range []string{myExpired, otherExpired} {
		if _, ok := mock.Files[path]; ok {
			t.Errorf("expired record still present: %s", path)
		}
	}
	for _, path := range []string{myFresh, otherFresh} {
		if _, ok := mock.Files[path]; !ok {
			t.Errorf("fresh record removed: %s", path)
		}
	}
	// The expired previews' containers were stopped (canonical + legacy
	// both by their stored container names).
	for _, container := range []string{
		"docker stop -t 5 'myapp-preview-p-" + loginIDHex + "-v1'",
		"docker stop -t 5 'otherapp-preview-legacy-feature-v1'",
	} {
		if indexOf(mock.Calls, container) == -1 {
			t.Errorf("expired preview's container not stopped: %q, calls: %v", container, mock.Calls)
		}
	}
	if indexOf(mock.Calls, "docker stop -t 5 'myapp-preview-p-"+dashIDHex+"-v1'") != -1 {
		t.Error("fresh preview's container was stopped")
	}
	if indexOf(mock.Calls, "/deployments/myapp/state.json") != -1 {
		t.Error("a non-preview deployment resource was touched")
	}
	if _, ok := mock.Files["/deployments/myapp/state.json"]; !ok {
		t.Error("non-preview state file removed")
	}

	// Idempotent: the second run finds nothing expired and issues no
	// docker commands at all.
	n := len(mock.Calls)
	pruned2, err := mgr.PruneAll(context.Background())
	if err != nil {
		t.Fatalf("second PruneAll: %v", err)
	}
	if pruned2 != 0 {
		t.Fatalf("second prune removed %d, want 0 (idempotent)", pruned2)
	}
	for _, call := range mock.Calls[n:] {
		if strings.Contains(call, "docker") {
			t.Errorf("idle prune issued docker commands: %q", call)
		}
	}
}

// The TTL default is applied at create AND refreshed at update when the
// caller doesn't pass one: ExpiresAt is exactly CreatedAt + 72h.
func TestDeploy_TTLDefaultApplied(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		append(previewDeployMocks(),
			ssh.MockCommand{Match: "curl -s -o /dev/null", Output: "200"})...)
	var buf bytes.Buffer
	mgr := NewManager(mock, &buf)

	cfg := deployCfg(loginBranch, "v1")
	cfg.TTL = 0
	before := time.Now().UTC()
	mustDeploy(t, mgr, cfg)

	var s State
	if err := json.Unmarshal(mock.Files[previewStatePath("myapp", loginBranch)], &s); err != nil {
		t.Fatal(err)
	}
	wantTTL := 72 * time.Hour
	got := s.ExpiresAt.Sub(s.CreatedAt)
	if got != wantTTL {
		t.Errorf("record TTL = %v, want the documented default %v", got, wantTTL)
	}
	if s.CreatedAt.Before(before) {
		t.Errorf("CreatedAt %v predates the deploy %v", s.CreatedAt, before)
	}
}
