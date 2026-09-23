package deploy

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy/internal/deploy/recovery"
	"github.com/useteploy/teploy/internal/docker"
	"github.com/useteploy/teploy/internal/releasemeta"
	"github.com/useteploy/teploy/internal/ssh"
)

// receiptUnderTest finds and parses the readiness receipt a deploy wrote.
func receiptUnderTest(t *testing.T, mock *ssh.MockExecutor, app string) (string, readinessReceipt) {
	t.Helper()
	for p := range mock.Files {
		if strings.HasPrefix(p, "/deployments/"+app+"/meta/att/") && strings.HasSuffix(p, "/readiness.json") {
			var r readinessReceipt
			if err := json.Unmarshal(mock.Files[p], &r); err != nil {
				t.Fatalf("parsing receipt %s: %v", p, err)
			}
			return p, r
		}
	}
	t.Fatalf("no readiness receipt persisted under /deployments/%s/meta/att/", app)
	return "", readinessReceipt{}
}

// TestReadinessReceipt_WrittenExactlyOnReadinessPass is the C01-4
// write-side regression: the receipt lands in the attempt namespace the
// moment the health gate passes — AFTER the last readiness probe and
// BEFORE the traffic switch issues its first command — and records the
// candidate identities plus what was probed.
func TestReadinessReceipt_WrittenExactlyOnReadinessPass(t *testing.T) {
	existingState := "current_port=49152\ncurrent_hash=old123\nprevious_port=0\nprevious_hash=\n"
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "mkdir -p /deployments/myapp", Output: ""},
		ssh.MockCommand{Match: "mkdir /deployments/myapp/.lock", Output: ""},
		ssh.MockCommand{Match: "if [ ! -e '/deployments/myapp/state.json' ]", Output: "absent"},
		ssh.MockCommand{Match: "if [ ! -e '/deployments/myapp/state' ]", Output: "present\n" + existingState},
		ssh.MockCommand{Match: "docker ps --all --filter label=teploy.app='myapp'", Output: predecessorInventoryJSON},
		ssh.MockCommand{Match: "ss -tln", Output: ssOutput},
		ssh.MockCommand{Match: "docker run", Output: "newcontainer123"},
		ssh.MockCommand{Match: "docker inspect", Output: "running"},
		ssh.MockCommand{Match: "curl -s -o /dev/null", Output: "200"},
		ssh.MockCommand{Match: "curl -sf http://localhost:2019/config/apps/http/servers/srv0", Output: `{"listen":[":80",":443"]}`},
		ssh.MockCommand{Match: "curl -sf -X PATCH", Err: errBoom},
		ssh.MockCommand{Match: "curl -sf -X POST http://localhost:2019/config/apps/http/servers/srv0/routes", Output: ""},
		ssh.MockCommand{Match: "rm -f /tmp/teploy_caddy", Output: ""},
		ssh.MockCommand{Match: "cat /deployments/caddy/Caddyfile", Output: "{\n\tadmin 0.0.0.0:2019\n}\n"},
		ssh.MockCommand{Match: "mv /tmp/teploy_caddyfile.tmp", Output: ""},
		ssh.MockCommand{Match: "mkdir /deployments/caddy/.lock", Output: ""},
		ssh.MockCommand{Match: "a=$(docker exec caddy md5sum", Output: "TEPLOY_CADDY_OK"},
		ssh.MockCommand{Match: "docker exec caddy caddy reload", Output: ""},
		ssh.MockCommand{Match: "rmdir /deployments/caddy/.lock", Output: ""},
		ssh.MockCommand{Match: "docker stop", Output: ""},
		ssh.MockCommand{Match: "printf %s", Output: ""},
		ssh.MockCommand{Match: "rm -rf /deployments/myapp/.lock", Output: ""},
	)

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

	_, r := receiptUnderTest(t, mock, "myapp")
	if r.App != "myapp" || r.Release != "new456" || r.Outcome != "passed" {
		t.Errorf("receipt identity/outcome: app=%q release=%q outcome=%q", r.App, r.Release, r.Outcome)
	}
	if len(r.Containers) != 1 || r.Containers[0].Name != "myapp-web-new456" || r.Containers[0].ID != "newcontainer123" {
		t.Errorf("receipt must record the exact candidate identities (docker run's IDs), got %+v", r.Containers)
	}
	if len(r.Probes) != 1 || r.Probes[0].Port != 49152 || r.Probes[0].Path != "/health" || r.Probes[0].Host != "localhost" {
		t.Errorf("receipt must record what was probed, got %+v", r.Probes)
	}
	if r.ProbedAt.IsZero() {
		t.Error("receipt must carry the probe timestamp")
	}

	// Ordering: after the LAST readiness probe, before the FIRST edge
	// command (the traffic switch begins with the Caddyfile transaction:
	// the caddy lock + read of the current file).
	commitIdx, lastProbeIdx, firstEdgeIdx := -1, -1, -1
	for i, c := range mock.Calls {
		if strings.HasPrefix(c, "mv -fT -- ") && strings.Contains(c, "/readiness.json.tmp-") {
			commitIdx = i
		}
		if strings.HasPrefix(c, "curl -s -o /dev/null") {
			lastProbeIdx = i
		}
		if firstEdgeIdx < 0 && strings.HasPrefix(c, "cat /deployments/caddy/Caddyfile") {
			firstEdgeIdx = i
		}
	}
	if commitIdx < 0 || lastProbeIdx < 0 || firstEdgeIdx < 0 {
		t.Fatalf("missing calls for ordering (receipt=%d probe=%d edge=%d)", commitIdx, lastProbeIdx, firstEdgeIdx)
	}
	if commitIdx < lastProbeIdx {
		t.Errorf("receipt must not exist before readiness passes: receipt=%d lastProbe=%d", commitIdx, lastProbeIdx)
	}
	if commitIdx > firstEdgeIdx {
		t.Errorf("receipt must be durable before the traffic switch begins: receipt=%d firstEdge=%d", commitIdx, firstEdgeIdx)
	}
}

// TestReadinessReceipt_NotWrittenWhenReadinessFails pins the other side:
// a deploy whose health gate never passes leaves NO receipt — its crash
// window is CandidatesRunning's, and a recovery owner must see exactly
// that.
func TestReadinessReceipt_NotWrittenWhenReadinessFails(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "mkdir -p /deployments/myapp", Output: ""},
		ssh.MockCommand{Match: "mkdir /deployments/myapp/.lock", Output: ""},
		ssh.MockCommand{Match: "if [ ! -e '/deployments/myapp/state.json' ]", Output: "absent"},
		ssh.MockCommand{Match: "if [ ! -e '/deployments/myapp/state' ]", Output: "absent"},
		ssh.MockCommand{Match: "ss -tln", Output: ssOutput},
		ssh.MockCommand{Match: "docker run", Output: "failcontainer"},
		ssh.MockCommand{Match: "docker inspect -f '{{json .State}}'", Output: `{"Status":"running","Running":true}`},
		ssh.MockCommand{Match: "docker inspect -f", Output: "running"},
		ssh.MockCommand{Match: "curl -s -o /dev/null", Err: errBoom},
		ssh.MockCommand{Match: "docker logs", Output: "Error: app crashed on startup"},
		ssh.MockCommand{Match: "docker stop", Output: ""},
		ssh.MockCommand{Match: "docker rm", Output: ""},
		ssh.MockCommand{Match: "printf %s", Output: ""},
		ssh.MockCommand{Match: "rm -rf /deployments/myapp/.lock", Output: ""},
	)
	if err := NewDeployer(mock, &bytes.Buffer{}).Deploy(context.Background(), Config{
		App:     "myapp",
		Domain:  "myapp.com",
		Image:   "myapp:latest",
		Version: "abc123",
		Health:  HealthConfig{Timeout: 300 * time.Millisecond, Interval: 50 * time.Millisecond},
	}); err == nil {
		t.Fatal("expected the health-failing deploy to fail")
	}
	for p := range mock.Files {
		if strings.HasSuffix(p, "/readiness.json") {
			t.Fatalf("no readiness receipt may exist when the gate never passed, found %s", p)
		}
	}
}

// TestReadinessReceipt_ReadValidatesIdentity covers the read helper:
// confirmed absent is (nil, nil); a foreign-identity receipt is refused.
func TestReadinessReceipt_ReadValidatesIdentity(t *testing.T) {
	att := releasemeta.MustAttempt("myapp", "new456")
	path := readinessReceiptPath(att)

	mock := ssh.NewMockExecutor("1.2.3.4")
	r, err := readReadinessReceipt(context.Background(), mock, att)
	if err != nil || r != nil {
		t.Fatalf("confirmed-absent receipt must be (nil, nil), got (%v, %v)", r, err)
	}

	foreign, _ := json.Marshal(readinessReceipt{SchemaVersion: journalSchemaVersion, App: "otherapp", Release: "new456", Attempt: att.Name(), Outcome: "passed"})
	mock.Files[path] = foreign
	if _, err := readReadinessReceipt(context.Background(), mock, att); err == nil {
		t.Fatal("a receipt describing another app/attempt must be refused, not used")
	}
}

// TestReadinessReceipt_DecideDistinguishesCompensateFromInspect is the
// C01-4 recovery wiring proof, asserted through the recovery package's
// Decide: the world a crash-after-readiness leaves (traffic switched onto
// an uncommitted generation) takes the COMPENSATE branch when the
// attempt's receipt lets the owner attribute the running candidates, and
// collapses to INSPECT without it — the receipt is the evidence that
// readiness held for THESE containers; Decide's own semantics are
// unchanged (it already models this evidence; the receipt is its
// producer).
func TestReadinessReceipt_DecideDistinguishesCompensateFromInspect(t *testing.T) {
	app, attempted, predecessor := "myapp", "new456", "old123"
	receipt := &readinessReceipt{
		SchemaVersion: journalSchemaVersion,
		App:           app,
		Release:       attempted,
		Outcome:       "passed",
		Containers: []receiptCandidate{
			{Name: "myapp-web-new456", ID: "cid-new-1"},
		},
		Probes:   []readinessProbe{{Container: "myapp-web-new456", Host: "localhost", Port: 49152, Path: "/health"}},
		ProbedAt: time.Now().UTC(),
	}
	// The crashed world: the candidate runs under the receipt's exact ID,
	// the edge names it, state.json still names the predecessor
	// (crash-before-commit), and the predecessor still serves elsewhere.
	inv := []docker.Container{
		{ID: "cid-new-1", Name: "myapp-web-new456", State: "running", Labels: map[string]string{"teploy.app": app, "teploy.version": attempted}},
		{ID: "cid-old-1", Name: "myapp-web-old123", State: "running", Labels: map[string]string{"teploy.app": app, "teploy.version": predecessor}},
	}
	observation := func(candidates recovery.Evidence) recovery.Observation {
		return recovery.Observation{
			Candidates:         candidates,
			CandidateCorpses:   recovery.Absent,
			ForeignCandidates:  recovery.Absent,
			RouteToCandidate:   recovery.Present,
			RouteToPredecessor: recovery.Absent,
			StateToCandidate:   recovery.Absent,
			StateToPredecessor: recovery.Present,
			PredecessorServing: recovery.Present,
			PredecessorStopped: recovery.Absent,
			ReleaseRecord:      recovery.Absent,
		}
	}

	// With the receipt: the owner's state is ReadinessPassed (proven) and
	// the candidates are attributable (exact IDs) -> COMPENSATE.
	if got := recovery.Decide(attemptReadinessState(receipt), observation(candidateAttribution(receipt, inv, app, attempted))); got != recovery.Compensate {
		t.Errorf("with the readiness receipt, crash-after-readiness must COMPENSATE, got %s", got)
	}
	// Without it (confirmed absent): the state collapses to
	// CandidatesRunning and the running candidates are not attributable
	// to the attempt -> INSPECT, never auto-decided.
	noReceiptState := attemptReadinessState(nil)
	if noReceiptState != recovery.CandidatesRunning {
		t.Errorf("without a receipt the state must collapse to CandidatesRunning, got %s", noReceiptState)
	}
	if got := recovery.Decide(noReceiptState, observation(candidateAttribution(nil, inv, app, attempted))); got != recovery.Inspect {
		t.Errorf("without the readiness receipt the same world must INSPECT, got %s", got)
	}
}

// TestCandidateAttribution pins the evidence derivation's truth table.
func TestCandidateAttribution(t *testing.T) {
	app, release := "myapp", "new456"
	running := docker.Container{ID: "cid-1", Name: "myapp-web-new456", State: "running"}
	receipt := &readinessReceipt{Containers: []receiptCandidate{{Name: "myapp-web-new456", ID: "cid-1"}}}

	if got := candidateAttribution(receipt, []docker.Container{running}, app, release); got != recovery.Present {
		t.Errorf("running container matching the receipt's ID: want Present, got %s", got)
	}
	if got := candidateAttribution(receipt, nil, app, release); got != recovery.Absent {
		t.Errorf("nothing candidate-shaped running: want Absent, got %s", got)
	}
	if got := candidateAttribution(nil, []docker.Container{running}, app, release); got != recovery.Unknown {
		t.Errorf("running candidate without a receipt is not attributable to the attempt: want Unknown, got %s", got)
	}
	// Another attempt's candidates of the same release (version-keyed
	// names, register F04/A09): provably not the receipt's — still never
	// auto-decided.
	other := docker.Container{ID: "cid-other", Name: "myapp-web-new456", State: "running"}
	if got := candidateAttribution(receipt, []docker.Container{other}, app, release); got != recovery.Unknown {
		t.Errorf("running candidate provably not the receipt's: want Unknown, got %s", got)
	}
	// Stopped corpses are not the Candidates class.
	stopped := docker.Container{ID: "cid-1", Name: "myapp-web-new456", State: "exited"}
	if got := candidateAttribution(receipt, []docker.Container{stopped}, app, release); got != recovery.Absent {
		t.Errorf("only running containers are Candidates: want Absent, got %s", got)
	}
}
