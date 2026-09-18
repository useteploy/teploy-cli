package deploy

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy/internal/ssh"
)

// audit F03: with replicas==1 the replica name and the non-indexed name are
// the same string. The old loop processed both: it renamed the live
// container to _replaced, then the second pass REMOVED _replaced (killing
// the running predecessor) and renamed again. The dedup fix must issue
// exactly one rm -f + one rename for the shared name before the new
// container starts.
func TestDeploy_SameVersion_DedupesReplicaAndPlainNames(t *testing.T) {
	existingState := "current_port=49152\ncurrent_hash=abc123\nprevious_port=0\nprevious_hash=\n"

	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "mkdir -p /deployments/myapp", Output: ""},
		ssh.MockCommand{Match: "mkdir /deployments/myapp/.lock", Output: ""},
		ssh.MockCommand{Match: "if [ ! -e '/deployments/myapp/state.json' ]", Output: "absent"},
		ssh.MockCommand{Match: "if [ ! -e '/deployments/myapp/state' ]", Output: "present\n" + existingState},
		ssh.MockCommand{Match: "ss -tln", Output: ""},
		ssh.MockCommand{Match: "docker rm -f", Output: ""},
		ssh.MockCommand{Match: "docker rename", Output: ""},
		ssh.MockCommand{Match: "docker run", Output: "abc123def456\n"},
		ssh.MockCommand{Match: "docker inspect -f '{{.State.Status}}'", Output: "running"},
		ssh.MockCommand{Match: "curl", Output: "200"},
		ssh.MockCommand{Match: "docker ps --all", Output: ""},
		ssh.MockCommand{Match: "docker stop -t", Output: ""},
		ssh.MockCommand{Match: "docker ps --filter label=teploy.app", Err: fmt.Errorf("none")},
		ssh.MockCommand{Match: "docker exec caddy caddy reload", Output: ""},
		ssh.MockCommand{Match: "caddy", Output: ""},
		ssh.MockCommand{Match: "cat /deployments/caddy/Caddyfile", Output: "{\n\tadmin 0.0.0.0:2019\n}\n"},
		ssh.MockCommand{Match: "mkdir /deployments/caddy/.lock", Output: ""},
		ssh.MockCommand{Match: "a=$(docker exec caddy md5sum", Output: "TEPLOY_CADDY_OK"},
		ssh.MockCommand{Match: "UPLOAD:", Output: ""},
		ssh.MockCommand{Match: "mv -f -- ", Output: ""},
		ssh.MockCommand{Match: "if [ ! -e '/deployments/caddy/Caddyfile'", Err: fmt.Errorf("none")},
	)

	var buf bytes.Buffer
	deployer := NewDeployer(mock, &buf)

	err := deployer.Deploy(context.Background(), Config{
		App:     "myapp",
		Domain:  "myapp.com",
		Image:   "myapp:latest",
		Version: "abc123",
		Health:  HealthConfig{Timeout: 5 * time.Second, Interval: 10 * time.Millisecond},
	})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	// Count pre-cleanup rm -f calls on the _replaced name: exactly one.
	rmCount := 0
	renameIdx, runIdx := -1, -1
	for i, call := range mock.Calls {
		if strings.Contains(call, "docker rm -f myapp-web-abc123_replaced") {
			rmCount++
		}
		if strings.Contains(call, "docker rename myapp-web-abc123 ") && renameIdx < 0 {
			renameIdx = i
		}
		if strings.HasPrefix(call, "docker run") && runIdx < 0 {
			runIdx = i
		}
	}
	if rmCount != 1 {
		t.Errorf("expected exactly one rm -f of the _replaced name before the rename, got %d (calls: %v)", rmCount, mock.Calls)
	}
	if renameIdx < 0 || runIdx < 0 || renameIdx > runIdx {
		t.Errorf("rename must precede the new container start (rename@%d, run@%d)", renameIdx, runIdx)
	}
}

// audit F06: a worker REMOVED from the new manifest has no entry in the new
// process map, so the old name-derived cleanup never stopped it. The
// inventory sweep must stop any still-running container carrying the
// replaced version label.
func TestDeploy_RemovedWorkerIsStoppedByInventory(t *testing.T) {
	existingState := "current_port=49152\ncurrent_hash=old123\nprevious_port=0\nprevious_hash=\n"

	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "mkdir -p /deployments/myapp", Output: ""},
		ssh.MockCommand{Match: "mkdir /deployments/myapp/.lock", Output: ""},
		ssh.MockCommand{Match: "if [ ! -e '/deployments/myapp/state.json' ]", Output: "absent"},
		ssh.MockCommand{Match: "if [ ! -e '/deployments/myapp/state' ]", Output: "present\n" + existingState},
		ssh.MockCommand{Match: "ss -tln", Output: ""},
		ssh.MockCommand{Match: "docker run", Output: "abc123def456\n"},
		ssh.MockCommand{Match: "docker inspect -f '{{.State.Status}}'", Output: "running"},
		ssh.MockCommand{Match: "curl", Output: "200"},
		ssh.MockCommand{Match: "docker ps --filter label=teploy.app", Err: fmt.Errorf("none")},
		// Inventory: web-old123 running, plus the REMOVED worker still running.
		ssh.MockCommand{Match: "docker ps --all", Output: `{"ID":"a","Names":"myapp-web-old123","Image":"myapp:v1","State":"running","Status":"Up","CreatedAt":"2026-01-01 00:00:00 +0000 UTC","Labels":"teploy.app=myapp,teploy.version=old123,teploy.process=web"}
{"ID":"b","Names":"myapp-worker-old123","Image":"myapp:v1","State":"running","Status":"Up","CreatedAt":"2026-01-01 00:00:00 +0000 UTC","Labels":"teploy.app=myapp,teploy.version=old123,teploy.process=worker"}`},
		ssh.MockCommand{Match: "docker stop -t", Output: ""},
		ssh.MockCommand{Match: "docker exec caddy caddy reload", Output: ""},
		ssh.MockCommand{Match: "caddy", Output: ""},
		ssh.MockCommand{Match: "cat /deployments/caddy/Caddyfile", Output: "{\n\tadmin 0.0.0.0:2019\n}\n"},
		ssh.MockCommand{Match: "mkdir /deployments/caddy/.lock", Output: ""},
		ssh.MockCommand{Match: "a=$(docker exec caddy md5sum", Output: "TEPLOY_CADDY_OK"},
		ssh.MockCommand{Match: "UPLOAD:", Output: ""},
		ssh.MockCommand{Match: "mv -f -- ", Output: ""},
		ssh.MockCommand{Match: "if [ ! -e '/deployments/caddy/Caddyfile'", Err: fmt.Errorf("none")},
	)

	var buf bytes.Buffer
	deployer := NewDeployer(mock, &buf)
	// New manifest: web only — the worker was removed.
	err := deployer.Deploy(context.Background(), Config{
		App:     "myapp",
		Domain:  "myapp.com",
		Image:   "myapp:v2",
		Version: "new456",
		Health:  HealthConfig{Timeout: 5 * time.Second, Interval: 10 * time.Millisecond},
	})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	stoppedWorker := false
	for _, call := range mock.Calls {
		if strings.Contains(call, "myapp-worker-old123") && strings.HasPrefix(call, "docker stop") {
			stoppedWorker = true
		}
	}
	if !stoppedWorker {
		t.Error("a worker removed from the manifest was left running (inventory cleanup did not stop it)")
	}
}

// audit F51: the v1 hash encoding (path + NUL + bytes + NUL) was ambiguous —
// a file containing embedded NULs could impersonate a two-file tree. The v2
// length-prefixed encoding must give these two trees different digests.
func TestHashDir_V2EncodingSeparatesStructuralCollision(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(a, "a"), []byte("X\x00b\x00Y"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b, "a"), []byte("X"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b, "b"), []byte("Y"), 0600); err != nil {
		t.Fatal(err)
	}
	ha, err := hashDir(a)
	if err != nil {
		t.Fatal(err)
	}
	hb, err := hashDir(b)
	if err != nil {
		t.Fatal(err)
	}
	if ha == hb {
		t.Fatalf("structural collision survived: both trees hash to %s", ha)
	}
}

// audit F52: hashDir follows symlinks while rsync preserves them, so a
// symlinked source can hash content the server never receives (or that
// escapes the release). Symlinks must be rejected before upload.
func TestHashDir_RejectsSymlinksAndSpecials(t *testing.T) {
	d := t.TempDir()
	if err := os.WriteFile(filepath.Join(d, "real"), []byte("ok"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(d, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := hashDir(d); err == nil {
		t.Fatal("symlinked source accepted: the hash would not describe what rsync delivers")
	}

	empty := t.TempDir()
	if _, err := hashDir(empty); err == nil {
		t.Fatal("empty tree accepted")
	}
}

// TCL-16: the health-check URL must be a single quoted argument with the
// IPv6 host bracketed, and a query string must survive verbatim — the old
// unquoted interpolation let '&' change shell parsing.
func TestProbeURL(t *testing.T) {
	got, ok := probeURL("::1", 8080, "/ready?a=1&b=2")
	if !ok || got != "http://[::1]:8080/ready?a=1&b=2" {
		t.Fatalf("got %q, %v", got, ok)
	}
	if got, ok := probeURL("localhost", 80, "/health"); !ok || got != "http://localhost:80/health" {
		t.Fatalf("got %q, %v", got, ok)
	}
	for _, tc := range []struct{ host, path string }{
		{"localhost", "https://elsewhere/"},
		{"localhost", "//elsewhere/"},
		{"localhost", "/x\ncmd"},
		{"evil;host", "/health"},
		{"localhost", "noslash"},
	} {
		if _, ok := probeURL(tc.host, 8080, tc.path); ok {
			t.Errorf("probeURL accepted %q / %q", tc.host, tc.path)
		}
	}
	if _, ok := probeURL("localhost", 0, "/health"); ok {
		t.Error("probeURL accepted port 0")
	}
}
