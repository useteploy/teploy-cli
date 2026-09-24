package deploy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy/internal/releasemeta"
	"github.com/useteploy/teploy/internal/ssh"
	"github.com/useteploy/teploy/internal/state"
)

// snapshotUnderTest finds the predecessor snapshot a deploy wrote in the
// mock's recorded file state and parses it. The attempt id is random, so
// the path is discovered by prefix/suffix, not constructed.
func snapshotUnderTest(t *testing.T, mock *ssh.MockExecutor, app string) (path string, snap predecessorSnapshot, att releasemeta.Attempt) {
	t.Helper()
	for p := range mock.Files {
		if strings.HasPrefix(p, "/deployments/"+app+"/meta/att/") && strings.HasSuffix(p, "/predecessors.json") {
			path = p
			break
		}
	}
	if path == "" {
		t.Fatalf("no predecessor snapshot persisted under /deployments/%s/meta/att/", app)
	}
	if err := json.Unmarshal(mock.Files[path], &snap); err != nil {
		t.Fatalf("parsing snapshot %s: %v", path, err)
	}
	// <att-root>/<hash>.<id>/predecessors.json
	dir := path[:strings.LastIndex(path, "/")]
	name := dir[strings.LastIndex(dir, "/")+1:]
	hash, id, ok := strings.Cut(name, ".")
	if !ok {
		t.Fatalf("attempt dir name %q does not parse as <hash>.<id>", name)
	}
	return path, snap, releasemeta.Attempt{App: app, Hash: hash, ID: id}
}

// restoreFixtureInspect is a minimal recreate-able container inspect for
// the displaced workload (the shape docker.InspectRecreate consumes).
func restoreFixtureInspect(name, version string) string {
	return fmt.Sprintf(`[{
  "Image": "sha256:%s",
  "Config": {"Image": "web:old", "Cmd": ["npm", "start"], "Labels": {"teploy.app": "web", "teploy.process": "web", "teploy.version": %q}},
  "HostConfig": {"NetworkMode": "teploy", "PortBindings": {"3000/tcp": [{"HostIp": "0.0.0.0", "HostPort": "3000"}]}, "RestartPolicy": {"Name": "no"}},
  "NetworkSettings": {"Networks": {"teploy": {"Aliases": ["web"]}}}
}]`, strings.Repeat("b", 64), version)
}

// TestPredecessorSnapshot_PersistedAtRenamePhaseBeforeAnyContainerStarts is
// the C01-10 write-side regression: the predecessor snapshot (exact
// container identities) must be durable in the attempt's immutable
// artifact namespace from the rename phase onward — written after the
// same-version renames / predecessor listing, BEFORE the recreate
// displacement stops anything and before any candidate container starts.
func TestPredecessorSnapshot_PersistedAtRenamePhaseBeforeAnyContainerStarts(t *testing.T) {
	existingState := "current_port=3000\ncurrent_hash=old123\n"
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "mkdir -p /deployments/web", Output: ""},
		ssh.MockCommand{Match: "mkdir /deployments/web/.lock", Output: ""},
		ssh.MockCommand{Match: "if [ ! -e '/deployments/web/state.json' ]", Output: "absent"},
		ssh.MockCommand{Match: "if [ ! -e '/deployments/web/state' ]", Output: "present\n" + existingState},
		// Step 6b listing: the predecessor web workload of old123, running.
		ssh.MockCommand{Match: "docker ps --all --filter label=teploy.app='web'", Output: `{"ID":"cid-old123","Names":"web-web-old123","Image":"web:old","State":"running","Status":"Up","CreatedAt":"2026-09-20 10:00:00 +0000 UTC","Labels":{"teploy.app":"web","teploy.process":"web","teploy.version":"old123"}}` + "\n"},
		// Recreate displacement query + stop.
		ssh.MockCommand{Match: "docker ps --filter label=teploy.app=web", Output: "web-web-old123"},
		ssh.MockCommand{Match: "docker stop", Output: ""},
		ssh.MockCommand{Match: "docker run", Output: "newcid"},
		ssh.MockCommand{Match: "docker inspect", Output: "running"},
		ssh.MockCommand{Match: "curl -s -o /dev/null", Output: "200"},
		ssh.MockCommand{Match: "printf %s", Output: ""},
		ssh.MockCommand{Match: "rm -rf /deployments/web/.lock", Output: ""},
	)

	var buf bytes.Buffer
	if err := NewDeployer(mock, &buf).Deploy(context.Background(), Config{
		App:           "web",
		Image:         "web:new",
		Version:       "new456",
		Ingress:       "host",
		ContainerPort: 3000,
		Health:        HealthConfig{Timeout: 5 * time.Second, Interval: 10 * time.Millisecond},
	}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	_, snap, att := snapshotUnderTest(t, mock, "web")
	if snap.App != "web" || snap.Release != "old123" || snap.Attempt != att.Name() {
		t.Errorf("snapshot identity: app=%q release=%q attempt=%q (want web/old123/%s)", snap.App, snap.Release, snap.Attempt, att.Name())
	}
	if len(snap.Containers) != 1 || snap.Containers[0].ID != "cid-old123" || snap.Containers[0].Name != "web-web-old123" {
		t.Errorf("snapshot must record the exact predecessor identities, got %+v", snap.Containers)
	}
	if snap.Containers[0].Labels["teploy.version"] != "old123" {
		t.Errorf("snapshot must carry the releasemeta-record identity of the predecessor, got labels %+v", snap.Containers[0].Labels)
	}

	// Ordering: the snapshot's atomic rename landed BEFORE the first
	// displacement stop and BEFORE the first candidate docker run.
	commitIdx, stopIdx, runIdx := -1, -1, -1
	for i, c := range mock.Calls {
		if commitIdx < 0 && strings.HasPrefix(c, "mv -fT -- ") && strings.Contains(c, "/predecessors.json.tmp-") {
			commitIdx = i
		}
		if stopIdx < 0 && strings.HasPrefix(c, "docker stop") {
			stopIdx = i
		}
		if runIdx < 0 && strings.HasPrefix(c, "docker run") {
			runIdx = i
		}
	}
	if commitIdx < 0 || stopIdx < 0 || runIdx < 0 {
		t.Fatalf("missing calls for ordering (snapshot=%d stop=%d run=%d)", commitIdx, stopIdx, runIdx)
	}
	if commitIdx > stopIdx || commitIdx > runIdx {
		t.Errorf("snapshot must be durable before any container starts or stops: snapshot=%d stop=%d run=%d", commitIdx, stopIdx, runIdx)
	}
}

// TestPredecessorSnapshot_CrashRecoveryCompensatesExactlyRecordedIDs is
// the C01-10 recover-side regression: a NEW process (no in-memory
// displaced list) recovers a crashed attempt by reading the snapshot from
// disk and compensating EXACTLY the recorded container identities — not a
// re-derivation. The live world also carries a stopped old123 WORKER
// that the name-derived fallback would touch; the snapshot does not name
// it, so recovery must not.
func TestPredecessorSnapshot_CrashRecoveryCompensatesExactlyRecordedIDs(t *testing.T) {
	existingState := "current_port=3000\ncurrent_hash=old123\n"

	// Phase 1 — the crashed attempt: the deploy writes the snapshot, then
	// dies during the recreate displacement (the stop fails; the process
	// "crashes" with the error, abandoning all in-memory knowledge).
	crashed := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "mkdir -p /deployments/web", Output: ""},
		ssh.MockCommand{Match: "mkdir /deployments/web/.lock", Output: ""},
		ssh.MockCommand{Match: "if [ ! -e '/deployments/web/state.json' ]", Output: "absent"},
		ssh.MockCommand{Match: "if [ ! -e '/deployments/web/state' ]", Output: "present\n" + existingState},
		ssh.MockCommand{Match: "docker ps --all --filter label=teploy.app='web'", Output: `{"ID":"cid-old123","Names":"web-web-old123","Image":"web:old","State":"running","Status":"Up","CreatedAt":"2026-09-20 10:00:00 +0000 UTC","Labels":{"teploy.app":"web","teploy.process":"web","teploy.version":"old123"}}` + "\n"},
		ssh.MockCommand{Match: "docker ps --filter label=teploy.app=web", Output: "web-web-old123"},
		ssh.MockCommand{Match: "docker stop", Err: errBoom},
		ssh.MockCommand{Match: "printf %s", Output: ""},
		ssh.MockCommand{Match: "rm -rf /deployments/web/.lock", Output: ""},
	)
	var crashedOut bytes.Buffer
	if err := NewDeployer(crashed, &crashedOut).Deploy(context.Background(), Config{
		App: "web", Image: "web:new", Version: "new456", Ingress: "host", ContainerPort: 3000,
	}); err == nil {
		t.Fatal("the crashed attempt must fail (its displacement stop failed)")
	}
	_, snap, att := snapshotUnderTest(t, crashed, "web")
	if len(snap.Containers) != 1 {
		t.Fatalf("crashed attempt's snapshot must name the predecessor, got %+v", snap.Containers)
	}

	// Phase 2 — a NEW process recovers: fresh executor sharing only the
	// durable file state (the snapshot on disk). It has no in-memory
	// displaced list. The live docker world: the snapshot's web container
	// is STOPPED (displaced before the crash), and a stopped old123
	// worker exists that the snapshot does NOT name.
	snapshotPath := fmt.Sprintf("/deployments/web/meta/att/%s/predecessors.json", att.Name())
	recovered := ssh.NewMockExecutor("1.2.3.4",
		// The snapshot's web container: stopped -> restorable.
		ssh.MockCommand{Match: "docker inspect -f '{{.State.Status}}' 'web-web-old123'", Output: "exited"},
		ssh.MockCommand{Match: "docker inspect 'web-web-old123'", Output: restoreFixtureInspect("web-web-old123", "old123")},
		ssh.MockCommand{Match: "docker rm -f", Output: ""},
		ssh.MockCommand{Match: "docker run", Output: "restored-old123"},
		ssh.MockCommand{Match: "printf %s", Output: ""},
	)
	recovered.Files[snapshotPath] = crashed.Files[snapshotPath]

	var out bytes.Buffer
	d := NewDeployer(recovered, &out)
	err := d.abortStateCommit(context.Background(), Config{
		App: "web", Image: "web:new", Version: "new456", Ingress: "host", ContainerPort: 3000,
	}, &state.AppState{SchemaVersion: 2, CurrentHash: "old123", IngressMode: "host"}, nil, nil, nil, att, nil, time.Now(), errBoom)
	if err == nil {
		t.Fatal("expected the commit error to surface")
	}
	if !strings.Contains(err.Error(), "original workload was restored") {
		t.Errorf("recovery must report the restored workload, got: %v", err)
	}

	// Exactly the recorded container was restored.
	restored := false
	for _, c := range recovered.Calls {
		if strings.HasPrefix(c, "docker run") && strings.Contains(c, "--name 'web-web-old123'") {
			restored = true
		}
	}
	if !restored {
		t.Fatalf("the snapshot's displaced web container was not restored: %v", recovered.Calls)
	}
	// Nothing outside the snapshot was touched: the stopped old123 worker
	// (which the name-derived fallback stopOldWorkloadsByName derives
	// from the process list) was never inspected, stopped, or restored.
	for _, c := range recovered.Calls {
		if strings.Contains(c, "worker") {
			t.Errorf("recovery touched a container the snapshot does not name: %s", c)
		}
	}
}
