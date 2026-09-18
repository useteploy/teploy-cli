package deploy

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy/internal/ssh"
)

// TCL-01: Deploy used to acquire the app's mkdir lock and then call
// DeployLocked, which acquired it AGAIN. A mkdir lock is not reentrant — the
// second acquisition fails and every normal Deploy errored with "deploy is
// already in progress" (masked in tests only because the mock lets repeated
// mkdirs succeed). The lock must be acquired exactly once per Deploy call.
func TestDeploy_AcquiresLockExactlyOnce(t *testing.T) {
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
		ssh.MockCommand{Match: "docker ps --all", Output: ""},
		ssh.MockCommand{Match: "docker stop -t", Output: ""},
		ssh.MockCommand{Match: "docker rm ", Output: ""},
		ssh.MockCommand{Match: "docker ps --filter label=teploy.app", Err: fmt.Errorf("none")},
		ssh.MockCommand{Match: "docker exec caddy caddy reload", Output: ""},
		ssh.MockCommand{Match: "caddy", Output: ""},
		ssh.MockCommand{Match: "cat /deployments/caddy/Caddyfile", Output: "{\n\tadmin 0.0.0.0:2019\n}\n"},
		ssh.MockCommand{Match: "mkdir /deployments/caddy/.lock", Output: ""},
		ssh.MockCommand{Match: "[ \"$(docker exec caddy md5sum", Output: "TEPLOY_CADDY_OK"},
		ssh.MockCommand{Match: "UPLOAD:", Output: ""},
		ssh.MockCommand{Match: "mv -f -- ", Output: ""},
		ssh.MockCommand{Match: "if [ ! -e '/deployments/caddy/Caddyfile'", Err: fmt.Errorf("none")},
	)

	var buf bytes.Buffer
	deployer := NewDeployer(mock, &buf)
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

	acquires := 0
	for _, call := range mock.Calls {
		if strings.Contains(call, "mkdir /deployments/myapp/.lock") {
			acquires++
		}
	}
	if acquires != 1 {
		t.Errorf("expected exactly one lock acquisition, got %d (calls: %v)", acquires, mock.Calls)
	}
}

// TCL-02: same-version redeploy. The old cleanup re-inventoried containers
// by the teploy.version label AFTER the replacement started — the
// replacement carries the same label, so the sweep stopped and REMOVED the
// just-deployed live containers. The cleanup must touch only the
// predecessors snapshotted before the new containers started (here:
// myapp-web-abc123_replaced), never the live replacement name.
func TestDeploy_SameVersionCleanupNeverTouchesReplacement(t *testing.T) {
	existingState := "current_port=49152\ncurrent_hash=abc123\nprevious_port=0\nprevious_hash=\n"

	// The predecessor snapshot (docker ps --all) returns the renamed old
	// container only — the replacement does not exist yet at snapshot time.
	// The post-deploy reality is that BOTH exist; the mock's flat response
	// models the snapshot, which is exactly what the fix must consume.
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
		ssh.MockCommand{Match: "docker ps --all", Output: `{"ID":"old","Names":"myapp-web-abc123_replaced","Image":"myapp:v1","State":"running","Status":"Up","CreatedAt":"2026-01-01 00:00:00 +0000 UTC","Labels":"teploy.app=myapp,teploy.version=abc123,teploy.process=web"}`},
		ssh.MockCommand{Match: "docker stop -t", Output: ""},
		ssh.MockCommand{Match: "docker rm ", Output: ""},
		ssh.MockCommand{Match: "docker ps --filter label=teploy.app", Err: fmt.Errorf("none")},
		ssh.MockCommand{Match: "docker exec caddy caddy reload", Output: ""},
		ssh.MockCommand{Match: "caddy", Output: ""},
		ssh.MockCommand{Match: "cat /deployments/caddy/Caddyfile", Output: "{\n\tadmin 0.0.0.0:2019\n}\n"},
		ssh.MockCommand{Match: "mkdir /deployments/caddy/.lock", Output: ""},
		ssh.MockCommand{Match: "[ \"$(docker exec caddy md5sum", Output: "TEPLOY_CADDY_OK"},
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

	for _, call := range mock.Calls {
		if strings.HasPrefix(call, "docker stop") || strings.HasPrefix(call, "docker rm ") {
			if strings.Contains(call, "myapp-web-abc123'") && !strings.Contains(call, "_replaced") {
				t.Errorf("cleanup touched the live replacement container: %s", call)
			}
		}
	}
	stoppedPredecessor := false
	for _, call := range mock.Calls {
		if strings.Contains(call, "myapp-web-abc123_replaced") && (strings.HasPrefix(call, "docker stop") || strings.HasPrefix(call, "docker rm ")) {
			stoppedPredecessor = true
		}
	}
	if !stoppedPredecessor {
		t.Error("predecessor _replaced container was never cleaned up")
	}
}
