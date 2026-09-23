package deploy

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

// debtStubSet is the mock command set of a clean blue/green caddy deploy of
// new456 (the TestDeploy_LogsCleanSuccessWhenRetirementCompletes shape).
func debtStubSet() []ssh.MockCommand {
	return []ssh.MockCommand{
		{Match: "mkdir -p /deployments/myapp", Output: ""},
		{Match: "mkdir /deployments/myapp/.lock", Output: ""},
		{Match: "if [ ! -e '/deployments/myapp/state.json' ]", Output: "absent"},
		{Match: "if [ ! -e '/deployments/myapp/state' ]", Output: "present\ncurrent_port=49152\ncurrent_hash=old123\nprevious_port=0\nprevious_hash=\n"},
		{Match: "ss -tln", Output: ssOutput},
		{Match: "docker ps --all --filter label=teploy.app='myapp'", Output: predecessorInventoryJSON},
		{Match: "docker run", Output: "newcontainer123"},
		{Match: "docker inspect", Output: "running"},
		{Match: "curl -s -o /dev/null", Output: "200"},
		{Match: "rm -f /tmp/teploy_caddy", Output: ""},
		{Match: "cat /deployments/caddy/Caddyfile", Output: "{\n\tadmin 0.0.0.0:2019\n}\n"},
		{Match: "mv /tmp/teploy_caddyfile.tmp", Output: ""},
		{Match: "mkdir /deployments/caddy/.lock", Output: ""},
		{Match: "a=$(docker exec caddy md5sum", Output: "TEPLOY_CADDY_OK"},
		{Match: "docker exec caddy caddy reload", Output: ""},
		{Match: "rmdir /deployments/caddy/.lock", Output: ""},
		{Match: "docker stop", Output: ""},
		{Match: "printf %s", Output: ""},
		{Match: "rm -rf /deployments/myapp/.lock", Output: ""},
	}
}

// debtMarkerUnderTest finds and parses the repair-debt marker a deploy wrote.
func debtMarkerUnderTest(t *testing.T, mock *ssh.MockExecutor, app string) (path string, debt RepairDebt) {
	t.Helper()
	path = "/deployments/" + app + "/repair-debt.json"
	raw, ok := mock.Files[path]
	if !ok {
		t.Fatalf("no repair-debt marker persisted at %s", path)
	}
	if err := json.Unmarshal(raw, &debt); err != nil {
		t.Fatalf("parsing repair-debt marker: %v", err)
	}
	return path, debt
}

// TestRecordWriteFailure_PersistsRepairDebtMarker is the C01-6 write-side
// regression: when the releasemeta record write fails AFTER the live commit,
// the deploy still completes (deliberate degradation — a record failure must
// never roll back live traffic) but the debt becomes DURABLE and visible: a
// repair-debt marker in the app's state namespace naming app, attempt, what
// failed, and when.
func TestRecordWriteFailure_PersistsRepairDebtMarker(t *testing.T) {
	stubs := debtStubSet()
	// The F14 record upload for new456 fails; everything else succeeds.
	stubs = append(stubs, ssh.MockCommand{Match: "UPLOAD:/deployments/myapp/meta/new456.json", Err: errBoom})
	mock := ssh.NewMockExecutor("1.2.3.4", stubs...)

	var buf bytes.Buffer
	if err := NewDeployer(mock, &buf).Deploy(context.Background(), Config{
		App:     "myapp",
		Domain:  "myapp.com",
		Image:   "myapp:v2",
		Version: "new456",
		Health:  HealthConfig{Timeout: 5 * time.Second, Interval: 10 * time.Millisecond},
	}); err != nil {
		t.Fatalf("a record-write failure must not fail the deploy (deliberate degradation): %v", err)
	}

	_, debt := debtMarkerUnderTest(t, mock, "myapp")
	if debt.App != "myapp" || debt.Release != "new456" {
		t.Errorf("marker must name the app and the release whose record failed, got app=%q release=%q", debt.App, debt.Release)
	}
	if !strings.HasPrefix(debt.Attempt, "new456.") || len(debt.Attempt) != len("new456.")+16 {
		t.Errorf("marker must name the failed deploy attempt, got %q", debt.Attempt)
	}
	if !strings.Contains(debt.Reason, "boom") {
		t.Errorf("marker must record what failed, got reason %q", debt.Reason)
	}
	if debt.Attempts != 1 {
		t.Errorf("first failure records attempt count 1, got %d", debt.Attempts)
	}
	if debt.FirstFailedAt.IsZero() {
		t.Error("marker must record when the write failed")
	}
	if !strings.Contains(buf.String(), "repair debt") {
		t.Errorf("the deploy output must surface the debt, got:\n%s", buf.String())
	}
}

// TestNextDeploy_RepairsRecordDebtAndClearsMarker is the C01-6 convergence
// regression: the NEXT deploy of the app, before its own work, rebuilds the
// failed record (live-container backfill — the table's transition-7
// convergence), clears the marker, and says so in the output.
func TestNextDeploy_RepairsRecordDebtAndClearsMarker(t *testing.T) {
	const nextInventory = `{"ID":"cid-new456","Names":"myapp-web-new456","Image":"myapp:v2","State":"running","Status":"Up","CreatedAt":"2026-09-20 10:00:00 +0000 UTC","Labels":{"teploy.app":"myapp","teploy.process":"web","teploy.version":"new456"}}` + "\n"

	stubs := []ssh.MockCommand{
		{Match: "mkdir -p /deployments/myapp", Output: ""},
		{Match: "mkdir /deployments/myapp/.lock", Output: ""},
		// The running predecessor is new456 (the release whose record failed).
		{Match: "docker ps --all --filter label=teploy.app='myapp'", Output: nextInventory},
		// Backfill inspects the new456 web container for its recreate spec.
		{Match: "docker inspect 'myapp-web-new456'", Output: myappInspectJSON("myapp-web-new456", "new456")},
		{Match: "ss -tln", Output: ssOutput},
		{Match: "docker run", Output: "newcid789"},
		{Match: "docker inspect", Output: "running"},
		{Match: "curl -s -o /dev/null", Output: "200"},
		{Match: "rm -f /tmp/teploy_caddy", Output: ""},
		{Match: "cat /deployments/caddy/Caddyfile", Output: "{\n\tadmin 0.0.0.0:2019\n}\n"},
		{Match: "mv /tmp/teploy_caddyfile.tmp", Output: ""},
		{Match: "mkdir /deployments/caddy/.lock", Output: ""},
		{Match: "a=$(docker exec caddy md5sum", Output: "TEPLOY_CADDY_OK"},
		{Match: "docker exec caddy caddy reload", Output: ""},
		{Match: "rmdir /deployments/caddy/.lock", Output: ""},
		{Match: "docker stop", Output: ""},
		{Match: "printf %s", Output: ""},
		{Match: "rm -rf /deployments/myapp/.lock", Output: ""},
	}
	mock := ssh.NewMockExecutor("1.2.3.4", stubs...)

	// Seed the durable world the next deploy finds: legacy state naming
	// new456 current, a repair-debt marker for new456, and NO record.
	mock.Files["/deployments/myapp/state"] = []byte("current_port=49152\ncurrent_hash=new456\nprevious_port=0\nprevious_hash=\n")
	seedDebt := RepairDebt{
		SchemaVersion: repairDebtSchemaVersion,
		App:           "myapp",
		Release:       "new456",
		Attempt:       "new456.0123456789abcdef",
		Reason:        "uploading temporary file: boom",
		Attempts:      1,
		FirstFailedAt: time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC),
		LastFailedAt:  time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC),
	}
	seedDebtJSON, _ := json.Marshal(seedDebt)
	mock.Files["/deployments/myapp/repair-debt.json"] = seedDebtJSON

	var buf bytes.Buffer
	if err := NewDeployer(mock, &buf).Deploy(context.Background(), Config{
		App:     "myapp",
		Domain:  "myapp.com",
		Image:   "myapp:v3",
		Version: "abc789",
		Health:  HealthConfig{Timeout: 5 * time.Second, Interval: 10 * time.Millisecond},
	}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	// The record was rebuilt from the live containers.
	recRaw, ok := mock.Files["/deployments/myapp/meta/new456.json"]
	if !ok {
		t.Fatal("the next deploy must rebuild the failed record for new456")
	}
	var rec struct {
		App        string `json:"app"`
		Hash       string `json:"hash"`
		Backfilled bool   `json:"backfilled"`
	}
	if err := json.Unmarshal(recRaw, &rec); err != nil {
		t.Fatalf("parsing the repaired record: %v", err)
	}
	if rec.App != "myapp" || rec.Hash != "new456" || !rec.Backfilled {
		t.Errorf("repaired record must be the backfilled record for myapp@new456, got %+v", rec)
	}

	// The marker is cleared on success.
	if _, still := mock.Files["/deployments/myapp/repair-debt.json"]; still {
		t.Error("the repair-debt marker must be cleared after a successful repair")
	}

	// The repair is reported, BEFORE the deploy's own work.
	out := buf.String()
	if !strings.Contains(out, "repair debt cleared") {
		t.Errorf("the repair must be reported in the output, got:\n%s", out)
	}
	if repairIdx, deployIdx := strings.Index(out, "repair debt cleared"), strings.Index(out, "Deploying myapp"); repairIdx < 0 || deployIdx < 0 || repairIdx < deployIdx {
		t.Errorf("the repair must run before the deploy's own work (repair=%d deploying=%d)", repairIdx, deployIdx)
	}
}

// TestPersistentRecordFailure_KeepsMarkerWithIncrementedAttempts pins the
// repeated-failure contract: when the repair itself fails again, the marker
// STAYS with an incremented attempt count and the output says so — the debt
// is never silently dropped or reset.
func TestPersistentRecordFailure_KeepsMarkerWithIncrementedAttempts(t *testing.T) {
	const nextInventory = `{"ID":"cid-new456","Names":"myapp-web-new456","Image":"myapp:v2","State":"running","Status":"Up","CreatedAt":"2026-09-20 10:00:00 +0000 UTC","Labels":{"teploy.app":"myapp","teploy.process":"web","teploy.version":"new456"}}` + "\n"

	stubs := []ssh.MockCommand{
		{Match: "mkdir -p /deployments/myapp", Output: ""},
		{Match: "mkdir /deployments/myapp/.lock", Output: ""},
		{Match: "docker ps --all --filter label=teploy.app='myapp'", Output: nextInventory},
		{Match: "docker inspect 'myapp-web-new456'", Output: myappInspectJSON("myapp-web-new456", "new456")},
		// The record for new456 STILL cannot be written.
		{Match: "UPLOAD:/deployments/myapp/meta/new456.json", Err: errBoom},
		{Match: "ss -tln", Output: ssOutput},
		{Match: "docker run", Output: "newcid789"},
		{Match: "docker inspect", Output: "running"},
		{Match: "curl -s -o /dev/null", Output: "200"},
		{Match: "rm -f /tmp/teploy_caddy", Output: ""},
		{Match: "cat /deployments/caddy/Caddyfile", Output: "{\n\tadmin 0.0.0.0:2019\n}\n"},
		{Match: "mv /tmp/teploy_caddyfile.tmp", Output: ""},
		{Match: "mkdir /deployments/caddy/.lock", Output: ""},
		{Match: "a=$(docker exec caddy md5sum", Output: "TEPLOY_CADDY_OK"},
		{Match: "docker exec caddy caddy reload", Output: ""},
		{Match: "rmdir /deployments/caddy/.lock", Output: ""},
		{Match: "docker stop", Output: ""},
		{Match: "printf %s", Output: ""},
		{Match: "rm -rf /deployments/myapp/.lock", Output: ""},
	}
	mock := ssh.NewMockExecutor("1.2.3.4", stubs...)
	mock.Files["/deployments/myapp/state"] = []byte("current_port=49152\ncurrent_hash=new456\nprevious_port=0\nprevious_hash=\n")
	seedDebt := RepairDebt{
		SchemaVersion: repairDebtSchemaVersion,
		App:           "myapp",
		Release:       "new456",
		Attempt:       "new456.0123456789abcdef",
		Reason:        "uploading temporary file: boom",
		Attempts:      1,
		FirstFailedAt: time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC),
		LastFailedAt:  time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC),
	}
	seedDebtJSON, _ := json.Marshal(seedDebt)
	mock.Files["/deployments/myapp/repair-debt.json"] = seedDebtJSON

	var buf bytes.Buffer
	if err := NewDeployer(mock, &buf).Deploy(context.Background(), Config{
		App:     "myapp",
		Domain:  "myapp.com",
		Image:   "myapp:v3",
		Version: "abc789",
		Health:  HealthConfig{Timeout: 5 * time.Second, Interval: 10 * time.Millisecond},
	}); err != nil {
		t.Fatalf("outstanding repair debt must not fail the next deploy: %v", err)
	}

	_, debt := debtMarkerUnderTest(t, mock, "myapp")
	if debt.Attempts != 2 {
		t.Errorf("a failed repair must keep the marker with an incremented attempt count, got %d", debt.Attempts)
	}
	if !strings.Contains(buf.String(), "repair debt remains") {
		t.Errorf("the output must say the debt remains, got:\n%s", buf.String())
	}
}

// TestNextDeploy_NoDebtMarkerNoOutputNoise pins the quiet path: an app with
// no outstanding repair debt produces no repair output at all.
func TestNextDeploy_NoDebtMarkerNoOutputNoise(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4", debtStubSet()...)
	var buf bytes.Buffer
	if err := NewDeployer(mock, &buf).Deploy(context.Background(), Config{
		App:     "myapp",
		Domain:  "myapp.com",
		Image:   "myapp:v2",
		Version: "new456",
		Health:  HealthConfig{Timeout: 5 * time.Second, Interval: 10 * time.Millisecond},
	}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if strings.Contains(strings.ToLower(buf.String()), "repair") {
		t.Errorf("no debt marker means no repair output noise, got:\n%s", buf.String())
	}
	if _, exists := mock.Files["/deployments/myapp/repair-debt.json"]; exists {
		t.Error("no marker must be written when the record write succeeds")
	}
}

// myappInspectJSON is a minimal recreate-able container inspect for the
// myapp fixtures (the shape docker.InspectRecreate consumes).
func myappInspectJSON(name, version string) string {
	return fmt.Sprintf(`[{
  "Image": "sha256:%s",
  "Config": {"Image": "myapp:v2", "Cmd": ["npm", "start"], "Labels": {"teploy.app": "myapp", "teploy.process": "web", "teploy.version": %q}},
  "HostConfig": {"NetworkMode": "teploy", "PortBindings": {"3000/tcp": [{"HostIp": "0.0.0.0", "HostPort": "3000"}]}, "RestartPolicy": {"Name": "no"}},
  "NetworkSettings": {"Networks": {"teploy": {"Aliases": ["myapp"]}}}
}]`, strings.Repeat("c", 64), version)
}
