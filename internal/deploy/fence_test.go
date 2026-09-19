package deploy

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy/internal/ssh"
	"github.com/useteploy/teploy/internal/state"
)

// fenceHappyPathMocks is the minimal successful-deploy mock set (mirrors
// TestDeploy_FirstDeploy) plus the lock acquisition, so a test can hold a
// REAL fenced lock handle and drive DeployFenced with it.
func fenceHappyPathMocks(app string) []ssh.MockCommand {
	return []ssh.MockCommand{
		ssh.MockCommand{Match: "mkdir /deployments/" + app + "/.lock", Output: ""},
		ssh.MockCommand{Match: "if [ ! -e '/deployments/" + app + "/state.json' ]", Output: "absent"},
		ssh.MockCommand{Match: "if [ ! -e '/deployments/" + app + "/state' ]", Output: "absent"},
		ssh.MockCommand{Match: "ss -tln", Output: ssOutput},
		ssh.MockCommand{Match: "docker run", Output: "abc123def456"},
		ssh.MockCommand{Match: "docker inspect -f '{{.Image}}'", Output: "sha256:" + strings.Repeat("a", 64)},
		ssh.MockCommand{Match: "docker inspect", Output: "running"},
		ssh.MockCommand{Match: "curl -s -o /dev/null", Output: "200"},
		ssh.MockCommand{Match: "curl -sf http://localhost:2019/config/apps/http/servers/srv0", Output: `{"listen":[":80",":443"]}`},
		ssh.MockCommand{Match: "curl -sf -X PATCH", Err: errors.New("not found")},
		ssh.MockCommand{Match: "curl -sf -X POST http://localhost:2019/config/apps/http/servers/srv0/routes", Output: ""},
		ssh.MockCommand{Match: "rm -f /tmp/teploy_caddy", Output: ""},
		ssh.MockCommand{Match: "cat /deployments/caddy/Caddyfile", Output: "{\n\tadmin 0.0.0.0:2019\n}\n"},
		ssh.MockCommand{Match: "mv /tmp/teploy_caddyfile.tmp", Output: ""},
		ssh.MockCommand{Match: "mkdir /deployments/caddy/.lock", Output: ""},
		ssh.MockCommand{Match: "a=$(docker exec caddy md5sum", Output: "TEPLOY_CADDY_OK"},
		ssh.MockCommand{Match: "docker exec caddy caddy reload", Output: ""},
		ssh.MockCommand{Match: "rmdir /deployments/caddy/.lock", Output: ""},
		ssh.MockCommand{Match: "printf %s", Output: ""},
		ssh.MockCommand{Match: "rm -rf /deployments/" + app + "/.lock", Output: ""},
	}
}

// TestDeployFenced_HappyPathChecksFence proves the fenced deploy path asks
// the server for holdership before its effectful phases: the grep guard
// commands appear, and with the lock held the deploy succeeds end to end.
func TestDeployFenced_HappyPathChecksFence(t *testing.T) {
	app := "fency"
	mock := ssh.NewMockExecutor("1.2.3.4", fenceHappyPathMocks(app)...)
	lk, err := state.AcquireLockFenced(context.Background(), mock, app)
	if err != nil {
		t.Fatalf("AcquireLockFenced: %v", err)
	}
	var buf bytes.Buffer
	d := NewDeployer(mock, &buf)
	if err := d.DeployFenced(context.Background(), Config{
		App:     app,
		Domain:  "fency.com",
		Image:   "fency:latest",
		Version: "abc123",
		Health:  HealthConfig{Timeout: 5 * time.Second, Interval: 10 * time.Millisecond},
	}, lk); err != nil {
		t.Fatalf("DeployFenced: %v", err)
	}
	guards := 0
	for _, c := range mock.Calls {
		if strings.HasPrefix(c, "grep -q '") && strings.Contains(c, "/.lock/info") {
			guards++
		}
	}
	if guards == 0 {
		t.Error("expected fence guard checks against .lock/info during the deploy")
	}
}

// TestDeployFenced_LateHolderRefusedToStartContainers is the F16 core
// safety property: a holder whose lock was broken and re-acquired by
// another operation must have its effects refused — here, before any
// container starts, so nothing is half-applied and nothing was mutated.
func TestDeployFenced_LateHolderRefusedToStartContainers(t *testing.T) {
	app := "fency"
	mock := ssh.NewMockExecutor("1.2.3.4", fenceHappyPathMocks(app)...)
	lk, err := state.AcquireLockFenced(context.Background(), mock, app)
	if err != nil {
		t.Fatalf("AcquireLockFenced: %v", err)
	}
	// The lock breaks and a second operation acquires it mid-flight: the
	// info file now names the other owner.
	mock.Files["/deployments/"+app+"/.lock/info"] = []byte(`{"type":"auto","owner":"secondoperation"}`)

	var buf bytes.Buffer
	d := NewDeployer(mock, &buf)
	err = d.DeployFenced(context.Background(), Config{
		App:     app,
		Domain:  "fency.com",
		Image:   "fency:latest",
		Version: "abc123",
		Health:  HealthConfig{Timeout: 5 * time.Second, Interval: 10 * time.Millisecond},
	}, lk)
	if !errors.Is(err, state.ErrFenceLost) {
		t.Fatalf("expected ErrFenceLost, got %v", err)
	}
	for _, c := range mock.Calls {
		if strings.Contains(c, "docker run") {
			t.Errorf("late holder's container start must be refused, saw: %s", c)
		}
	}
	if _, ok := mock.Files["/deployments/"+app+"/state.json"]; ok {
		t.Error("late holder must not commit state")
	}
}
