package deploy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy/internal/ssh"
)

// drainDeployMocks is the successful blue/green deploy fixture WITH a
// running predecessor (state names old123; the inventory lists its web
// container), so the full switch→drain→retire sequence runs.
func drainDeployMocks(existingState string) []ssh.MockCommand {
	return []ssh.MockCommand{
		ssh.MockCommand{Match: "mkdir -p /deployments/myapp", Output: ""},
		ssh.MockCommand{Match: "mkdir /deployments/myapp/.lock", Output: ""},
		ssh.MockCommand{Match: "if [ ! -e '/deployments/myapp/state.json' ]", Output: "absent"},
		ssh.MockCommand{Match: "if [ ! -e '/deployments/myapp/state' ]", Output: "present\n" + existingState},
		ssh.MockCommand{Match: "ss -tln", Output: ssOutput},
		ssh.MockCommand{Match: "docker ps --all --filter label=teploy.app='myapp'", Output: predecessorInventoryJSON},
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
		ssh.MockCommand{Match: "docker stop", Output: ""},
		ssh.MockCommand{Match: "printf %s", Output: ""},
		ssh.MockCommand{Match: "rm -rf /deployments/myapp/.lock", Output: ""},
	}
}

// TestDeploy_DrainWindowBetweenSwitchAndRetirement is the C03 wiring
// proof: with drain_seconds set, the deploy surfaces the policy, the
// route switch (reload) lands BEFORE the predecessor stop, and the drain
// window actually elapses between them.
func TestDeploy_DrainWindowBetweenSwitchAndRetirement(t *testing.T) {
	existingState := "current_port=49152\ncurrent_hash=old123\nprevious_port=0\nprevious_hash=\n"
	mock := ssh.NewMockExecutor("1.2.3.4", drainDeployMocks(existingState)...)
	var buf bytes.Buffer
	deployer := NewDeployer(mock, &buf)

	start := time.Now()
	err := deployer.Deploy(context.Background(), Config{
		App:          "myapp",
		Domain:       "myapp.com",
		Image:        "myapp:v2",
		Version:      "new456",
		DrainSeconds: 1,
		Health:       HealthConfig{Timeout: 5 * time.Second, Interval: 10 * time.Millisecond},
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("draining deploy: %v", err)
	}
	if elapsed < 1*time.Second {
		t.Errorf("the drain window must elapse before retirement (took %s)", elapsed)
	}
	out := buf.String()
	if !strings.Contains(out, "Draining myapp's predecessor for 1s") {
		t.Errorf("expected the drain to be surfaced, got:\n%s", out)
	}
	if !strings.Contains(out, "Stop policy: graceful stop after 10s (SIGTERM then SIGKILL); drain 1s window before predecessor retirement") {
		t.Errorf("expected the surfaced stop/drain policy distinction, got:\n%s", out)
	}
	reloadIdx, stopIdx := -1, -1
	for i, c := range mock.Calls {
		if strings.HasPrefix(c, "docker exec caddy caddy reload") && reloadIdx < 0 {
			reloadIdx = i
		}
		if strings.HasPrefix(c, "docker stop") && stopIdx < 0 {
			stopIdx = i
		}
	}
	if reloadIdx < 0 || stopIdx < 0 || reloadIdx > stopIdx {
		t.Errorf("route switch (idx %d) must precede predecessor stop (idx %d)", reloadIdx, stopIdx)
	}
}

// TestDeploy_DrainDisabledByDefault pins the compatibility default:
// drain_seconds 0 stops the predecessor immediately after the switch —
// no window, no drain line (the historical behavior).
func TestDeploy_DrainDisabledByDefault(t *testing.T) {
	existingState := "current_port=49152\ncurrent_hash=old123\nprevious_port=0\nprevious_hash=\n"
	mock := ssh.NewMockExecutor("1.2.3.4", drainDeployMocks(existingState)...)
	var buf bytes.Buffer
	deployer := NewDeployer(mock, &buf)

	start := time.Now()
	err := deployer.Deploy(context.Background(), Config{
		App:     "myapp",
		Domain:  "myapp.com",
		Image:   "myapp:v2",
		Version: "new456",
		Health:  HealthConfig{Timeout: 5 * time.Second, Interval: 10 * time.Millisecond},
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("undrained deploy: %v", err)
	}
	if elapsed >= 1*time.Second {
		t.Errorf("no drain window may be added by default (took %s)", elapsed)
	}
	if strings.Contains(buf.String(), "Draining") {
		t.Errorf("drain must not surface when disabled:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "drain disabled — the predecessor stops immediately after the switch") {
		t.Errorf("the surfaced stop policy must name the disabled drain:\n%s", buf.String())
	}
}

// TestDeploy_DrainSkippedWithoutServingPredecessor: external ingress has
// no teploy-managed edge and no serving predecessor behind THIS switch;
// the window must not run even when configured.
func TestDeploy_DrainSkippedWithoutServingPredecessor(t *testing.T) {
	existingState := "current_port=49152\ncurrent_hash=old123\nprevious_port=0\nprevious_hash=\n"
	mocks := drainDeployMocks(existingState)
	// No route step under external ingress: the admin-API curl checks and
	// reload are never reached; keep the rest.
	mock := ssh.NewMockExecutor("1.2.3.4", mocks...)
	var buf bytes.Buffer
	deployer := NewDeployer(mock, &buf)

	start := time.Now()
	err := deployer.Deploy(context.Background(), Config{
		App:          "myapp",
		Domain:       "myapp.com",
		Image:        "myapp:v2",
		Version:      "new456",
		Ingress:      "external",
		DrainSeconds: 1,
		Health:       HealthConfig{Timeout: 5 * time.Second, Interval: 10 * time.Millisecond},
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("external-ingress deploy: %v", err)
	}
	if elapsed >= 1*time.Second {
		t.Errorf("no drain window under external ingress (took %s)", elapsed)
	}
	if strings.Contains(buf.String(), "Draining") {
		t.Errorf("drain must not run without a teploy-managed switch:\n%s", buf.String())
	}
}

// TestRollback_DrainWindow is the rollback-side C03 wiring: the route
// switched back to the target, then the configured window elapses before
// the superseded generation is stopped, surfaced to the operator.
func TestRollback_DrainWindow(t *testing.T) {
	currentManifest := json.RawMessage(`{"release":"v2"}`)
	previousManifest := json.RawMessage(`{"release":"v1"}`)
	stateContent := fmt.Sprintf(`{"schema_version":2,"deployment_type":"container","ingress_mode":"caddy","domain":"myapp.com","updated_at":"2026-07-22T10:00:00Z","manifest_sha256":"%x","source_revision":"rev-v2","image_ref":"myapp:v2","operation_id":"deploy-v2","generation":7,"applied_manifest":%s,"previous_release":{"hash":"v1","manifest_sha256":"%x","source_revision":"rev-v1","image_ref":"myapp:v1","applied_manifest":%s},"current_port":49153,"current_hash":"v2","previous_port":49152,"previous_hash":"v1"}`,
		sha256.Sum256(currentManifest), currentManifest, sha256.Sum256(previousManifest), previousManifest)
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "if [ ! -e '/deployments/myapp/state.json' ]", Output: "present\n" + stateContent},
		ssh.MockCommand{Match: "mkdir -p /deployments/myapp", Output: ""},
		ssh.MockCommand{Match: "mkdir /deployments/myapp/.lock", Output: ""},
		ssh.MockCommand{Match: "cat /deployments/myapp/.lock/info", Err: fmt.Errorf("none")},
		ssh.MockCommand{Match: "docker ps --all --filter label=teploy.app='myapp'",
			Output: `{"ID":"aaa","Names":"myapp-web-v1","Image":"myapp:latest","State":"exited","Status":"Exited","Labels":"teploy.app=myapp,teploy.version=v1,teploy.process=web"}` + "\n" +
				`{"ID":"bbb","Names":"myapp-web-v2","Image":"myapp:latest","State":"running","Status":"Up 1h","Labels":"teploy.app=myapp,teploy.version=v2,teploy.process=web"}`,
		},
		ssh.MockCommand{Match: "docker inspect 'myapp-web-v1'", Output: `[{"Config":{"Image":"myapp:latest","Labels":{"teploy.app":"myapp"}},"HostConfig":{"NetworkMode":"teploy","PortBindings":{"3000/tcp":[{"HostIp":"127.0.0.1","HostPort":"49152"}]},"RestartPolicy":{"Name":"no"}},"NetworkSettings":{"Networks":{"teploy":{"Aliases":["myapp"]}}}}]`},
		ssh.MockCommand{Match: "docker rm -f 'myapp-web-v1'", Output: ""},
		ssh.MockCommand{Match: "docker run", Output: ""},
		ssh.MockCommand{Match: "curl", Output: "200"},
		ssh.MockCommand{Match: "docker inspect -f '{{range $p, $b := .NetworkSettings.Ports}}{{range $b}}{{.HostIp}}", Output: "127.0.0.1 "},
		ssh.MockCommand{Match: "docker inspect -f '{{range $p, $b := .NetworkSettings.Ports}}", Output: "49153"},
		ssh.MockCommand{Match: "docker inspect -f '{{range $p, $_ := .NetworkSettings.Ports}}", Output: "3000/tcp"},
		ssh.MockCommand{Match: "caddy", Output: ""},
		ssh.MockCommand{Match: "cat /deployments/caddy/Caddyfile", Output: "{\n\tadmin 0.0.0.0:2019\n}\n"},
		ssh.MockCommand{Match: "mkdir /deployments/caddy/.lock", Output: ""},
		ssh.MockCommand{Match: "a=$(docker exec caddy md5sum", Output: "TEPLOY_CADDY_OK"},
		ssh.MockCommand{Match: "docker exec caddy caddy reload", Output: ""},
		ssh.MockCommand{Match: "docker stop", Output: ""},
		ssh.MockCommand{Match: "mkdir -p", Output: ""},
		ssh.MockCommand{Match: "cat /tmp", Output: ""},
		ssh.MockCommand{Match: "UPLOAD:", Output: ""},
	)

	var buf bytes.Buffer
	cfg := rollbackCfg()
	cfg.DrainSeconds = 1
	start := time.Now()
	if err := Rollback(context.Background(), mock, &buf, cfg); err != nil {
		t.Fatalf("rollback with drain: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 1*time.Second {
		t.Errorf("the drain window must elapse before the superseded generation stops (took %s)", elapsed)
	}
	if !strings.Contains(buf.String(), "Draining myapp's superseded generation for 1s") {
		t.Errorf("expected the drain to be surfaced, got:\n%s", buf.String())
	}
	reloadIdx, stopIdx := -1, -1
	for i, c := range mock.Calls {
		if strings.HasPrefix(c, "docker exec caddy caddy reload") && reloadIdx < 0 {
			reloadIdx = i
		}
		if strings.HasPrefix(c, "docker stop") && stopIdx < 0 {
			stopIdx = i
		}
	}
	if reloadIdx < 0 || stopIdx < 0 || reloadIdx > stopIdx {
		t.Errorf("route switch back (idx %d) must precede the superseded stop (idx %d)", reloadIdx, stopIdx)
	}
}
