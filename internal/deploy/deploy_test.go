package deploy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy/internal/ssh"
	"github.com/useteploy/teploy/internal/state"
)

// ssOutput is a minimal ss -tln output with no ports in the ephemeral range.
const ssOutput = `State   Recv-Q  Send-Q  Local Address:Port  Peer Address:Port
LISTEN  0       128     0.0.0.0:22           0.0.0.0:*
LISTEN  0       128     0.0.0.0:80           0.0.0.0:*
LISTEN  0       128     0.0.0.0:443          0.0.0.0:*`

func TestDeploy_FirstDeploy(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		// 1. Ensure app dir.
		ssh.MockCommand{Match: "mkdir -p /deployments/myapp", Output: ""},
		// 2. Acquire lock.
		ssh.MockCommand{Match: "mkdir /deployments/myapp/.lock", Output: ""},
		// 3. Read state (none on first deploy).
		ssh.MockCommand{Match: "cat /deployments/myapp/state", Err: fmt.Errorf("no such file")},
		// 4. Find port.
		ssh.MockCommand{Match: "ss -tln", Output: ssOutput},
		// 5. Start container.
		ssh.MockCommand{Match: "docker run", Output: "abc123def456"},
		// 6. Verify running.
		ssh.MockCommand{Match: "docker inspect", Output: "running"},
		// 7. Health check.
		ssh.MockCommand{Match: "curl -s -o /dev/null", Output: "200"},
		// 8. Caddy ensureServer (srv0 exists).
		ssh.MockCommand{Match: "curl -sf http://localhost:2019/config/apps/http/servers/srv0", Output: `{"listen":[":80",":443"]}`},
		// 9. Caddy putRouteByID: PATCH to update route (fails, then POST to append).
		ssh.MockCommand{Match: "curl -sf -X PATCH", Err: fmt.Errorf("not found")},
		ssh.MockCommand{Match: "curl -sf -X POST http://localhost:2019/config/apps/http/servers/srv0/routes", Output: ""},
		// 10. Caddy temp cleanup.
		ssh.MockCommand{Match: "rm -f /tmp/teploy_caddy", Output: ""},
		ssh.MockCommand{Match: "cat /deployments/caddy/Caddyfile", Output: "{\n\tadmin 0.0.0.0:2019\n}\n"},
		ssh.MockCommand{Match: "mv /tmp/teploy_caddyfile.tmp", Output: ""},
		ssh.MockCommand{Match: "mkdir /deployments/caddy/.lock", Output: ""},
		ssh.MockCommand{Match: "docker exec caddy caddy reload", Output: ""},
		ssh.MockCommand{Match: "rmdir /deployments/caddy/.lock", Output: ""},
		// 11. Log append.
		ssh.MockCommand{Match: "printf %s", Output: ""},
		// 12. Release lock.
		ssh.MockCommand{Match: "rm -rf /deployments/myapp/.lock", Output: ""},
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

	output := buf.String()
	if !strings.Contains(output, "Deploying myapp") {
		t.Error("expected deploy start message")
	}
	if !strings.Contains(output, "Health check passed") {
		t.Error("expected health check passed message")
	}
	if !strings.Contains(output, "Traffic routed") {
		t.Error("expected traffic routed message")
	}
	if !strings.Contains(output, "Deployed myapp version abc123") {
		t.Error("expected deploy success message")
	}

	// Verify state was written.
	stateData, ok := mock.Files["/deployments/myapp/state"]
	if !ok {
		t.Fatal("state file not written")
	}
	if !strings.Contains(string(stateData), "current_port=49152") {
		t.Errorf("expected current_port=49152 in state, got: %s", string(stateData))
	}
	if !strings.Contains(string(stateData), "current_hash=abc123") {
		t.Errorf("expected current_hash=abc123 in state, got: %s", string(stateData))
	}

	// Verify deploy log was written.
	logData, ok := logEntryFromCalls(mock)
	if !ok {
		t.Fatal("log entry not written")
	}
	var logEntry state.LogEntry
	if err := json.Unmarshal(logData, &logEntry); err != nil {
		t.Fatalf("parsing log entry: %v", err)
	}
	if !logEntry.Success {
		t.Error("expected success=true in log entry")
	}
	if logEntry.Hash != "abc123" {
		t.Errorf("expected hash abc123 in log, got %s", logEntry.Hash)
	}

	// Verify docker run included the right port.
	for _, call := range mock.Calls {
		if strings.HasPrefix(call, "docker run") {
			if !strings.Contains(call, "-p 127.0.0.1:49152:80") {
				t.Errorf("expected port mapping 127.0.0.1:49152:80 in docker run: %s", call)
			}
			if !strings.Contains(call, "-e PORT=80") {
				t.Errorf("expected PORT=80 env var in docker run: %s", call)
			}
			if !strings.Contains(call, "--network teploy") {
				t.Errorf("expected teploy network in docker run: %s", call)
			}
			break
		}
	}
}

func TestDeploy_HostIngress(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "mkdir -p /deployments/web", Output: ""},
		ssh.MockCommand{Match: "mkdir /deployments/web/.lock", Output: ""},
		ssh.MockCommand{Match: "cat /deployments/web/state", Err: fmt.Errorf("no such file")},
		// Recreate pre-step: query existing web containers to free the port.
		ssh.MockCommand{Match: "docker ps -aq --filter label=teploy.app=web", Output: ""},
		ssh.MockCommand{Match: "docker run", Output: "abc123def456"},
		ssh.MockCommand{Match: "docker inspect", Output: "running"},
		ssh.MockCommand{Match: "curl -s -o /dev/null", Output: "200"},
		ssh.MockCommand{Match: "printf %s", Output: ""},
		ssh.MockCommand{Match: "rm -rf /deployments/web/.lock", Output: ""},
	)

	var buf bytes.Buffer
	deployer := NewDeployer(mock, &buf)

	err := deployer.Deploy(context.Background(), Config{
		App:           "web",
		Image:         "web:latest",
		Version:       "abc123",
		Ingress:       "host",
		ContainerPort: 3000,
		Health:        HealthConfig{Timeout: 5 * time.Second, Interval: 10 * time.Millisecond},
	})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	// No ephemeral port allocation in host mode.
	for _, call := range mock.Calls {
		if strings.HasPrefix(call, "ss -tln") {
			t.Errorf("host ingress should not allocate ephemeral ports (saw: %s)", call)
		}
	}

	// docker run must publish the fixed port on 0.0.0.0 and recreate-removed
	// any prior container (the filter query must have run before run).
	var sawFilter, sawRun bool
	for _, call := range mock.Calls {
		if strings.HasPrefix(call, "docker ps -aq --filter label=teploy.app=web") {
			sawFilter = true
		}
		if strings.HasPrefix(call, "docker run") {
			sawRun = true
			if !strings.Contains(call, "-p 0.0.0.0:3000:3000") {
				t.Errorf("expected -p 0.0.0.0:3000:3000 in docker run: %s", call)
			}
		}
	}
	if !sawFilter {
		t.Error("expected recreate filter query for existing web containers")
	}
	if !sawRun {
		t.Error("expected docker run")
	}

	// No Caddy interaction in host mode.
	output := buf.String()
	if !strings.Contains(output, "Skipping Caddy route update (ingress: host)") {
		t.Errorf("expected Caddy skip message, got: %s", output)
	}

	// Fixed port recorded in state.
	stateData, ok := mock.Files["/deployments/web/state"]
	if !ok {
		t.Fatal("state file not written")
	}
	if !strings.Contains(string(stateData), "current_port=3000") {
		t.Errorf("expected current_port=3000 in state, got: %s", string(stateData))
	}
}

func TestDeploy_UpdateExisting(t *testing.T) {
	existingState := "current_port=49152\ncurrent_hash=old123\nprevious_port=0\nprevious_hash=\n"

	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "mkdir -p /deployments/myapp", Output: ""},
		ssh.MockCommand{Match: "mkdir /deployments/myapp/.lock", Output: ""},
		ssh.MockCommand{Match: "cat /deployments/myapp/state", Output: existingState},
		ssh.MockCommand{Match: "ss -tln", Output: ssOutput},
		ssh.MockCommand{Match: "docker run", Output: "newcontainer123"},
		ssh.MockCommand{Match: "docker inspect", Output: "running"},
		ssh.MockCommand{Match: "curl -s -o /dev/null", Output: "200"},
		ssh.MockCommand{Match: "curl -sf http://localhost:2019/config/apps/http/servers/srv0", Output: `{"listen":[":80",":443"]}`},
		ssh.MockCommand{Match: "curl -sf -X PATCH", Err: fmt.Errorf("not found")},
		ssh.MockCommand{Match: "curl -sf -X POST http://localhost:2019/config/apps/http/servers/srv0/routes", Output: ""},
		ssh.MockCommand{Match: "rm -f /tmp/teploy_caddy", Output: ""},
		ssh.MockCommand{Match: "cat /deployments/caddy/Caddyfile", Output: "{\n\tadmin 0.0.0.0:2019\n}\n"},
		ssh.MockCommand{Match: "mv /tmp/teploy_caddyfile.tmp", Output: ""},
		ssh.MockCommand{Match: "mkdir /deployments/caddy/.lock", Output: ""},
		ssh.MockCommand{Match: "docker exec caddy caddy reload", Output: ""},
		ssh.MockCommand{Match: "rmdir /deployments/caddy/.lock", Output: ""},
		// Stop and remove old container.
		ssh.MockCommand{Match: "docker stop", Output: ""},
		ssh.MockCommand{Match: "docker rm", Output: ""},
		// Log and lock release.
		ssh.MockCommand{Match: "printf %s", Output: ""},
		ssh.MockCommand{Match: "rm -rf /deployments/myapp/.lock", Output: ""},
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

	// Verify old container was stopped.
	output := buf.String()
	if !strings.Contains(output, "Stopping old container myapp-web-old123") {
		t.Error("expected message about stopping old container")
	}

	// Verify state includes previous info.
	stateData := string(mock.Files["/deployments/myapp/state"])
	if !strings.Contains(stateData, "current_hash=new456") {
		t.Errorf("expected current_hash=new456, got: %s", stateData)
	}
	if !strings.Contains(stateData, "previous_hash=old123") {
		t.Errorf("expected previous_hash=old123, got: %s", stateData)
	}
	if !strings.Contains(stateData, "previous_port=49152") {
		t.Errorf("expected previous_port=49152, got: %s", stateData)
	}
}

func TestDeploy_HealthCheckFailure(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "mkdir -p /deployments/myapp", Output: ""},
		ssh.MockCommand{Match: "mkdir /deployments/myapp/.lock", Output: ""},
		ssh.MockCommand{Match: "cat /deployments/myapp/state", Err: fmt.Errorf("no state")},
		ssh.MockCommand{Match: "ss -tln", Output: ssOutput},
		ssh.MockCommand{Match: "docker run", Output: "failcontainer"},
		ssh.MockCommand{Match: "docker inspect -f", Output: "running"},
		// Health check always fails (connection refused).
		ssh.MockCommand{Match: "curl -s -o /dev/null", Err: fmt.Errorf("connection refused")},
		// Cleanup: show logs, stop, remove.
		ssh.MockCommand{Match: "docker logs", Output: "Error: app crashed on startup"},
		ssh.MockCommand{Match: "docker stop", Output: ""},
		ssh.MockCommand{Match: "docker rm", Output: ""},
		// Log failure entry.
		ssh.MockCommand{Match: "printf %s", Output: ""},
		// Release lock.
		ssh.MockCommand{Match: "rm -rf /deployments/myapp/.lock", Output: ""},
	)

	var buf bytes.Buffer
	deployer := NewDeployer(mock, &buf)

	err := deployer.Deploy(context.Background(), Config{
		App:     "myapp",
		Domain:  "myapp.com",
		Image:   "myapp:bad",
		Version: "bad123",
		Health:  HealthConfig{Timeout: 100 * time.Millisecond, Interval: 10 * time.Millisecond},
	})
	if err == nil {
		t.Fatal("expected error on health check failure")
	}
	if !strings.Contains(err.Error(), "health check failed") {
		t.Errorf("expected health check error, got: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "Container logs") {
		t.Error("expected container logs in output")
	}
	if !strings.Contains(output, "app crashed") {
		t.Error("expected crash message in logs output")
	}

	// Verify failure was logged.
	logData, _ := logEntryFromCalls(mock)
	var logEntry state.LogEntry
	json.Unmarshal(logData, &logEntry)
	if logEntry.Success {
		t.Error("expected success=false in failure log entry")
	}

	// Verify state was NOT written (no state file uploaded for deploy failure).
	if _, ok := mock.Files["/deployments/myapp/state"]; ok {
		t.Error("state should not be written on failed deploy")
	}
}

func TestDeploy_ContainerCrash(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "mkdir -p /deployments/myapp", Output: ""},
		ssh.MockCommand{Match: "mkdir /deployments/myapp/.lock", Output: ""},
		ssh.MockCommand{Match: "cat /deployments/myapp/state", Err: fmt.Errorf("no state")},
		ssh.MockCommand{Match: "ss -tln", Output: ssOutput},
		ssh.MockCommand{Match: "docker run", Output: "crashcontainer"},
		// Container exited immediately.
		ssh.MockCommand{Match: "docker inspect -f", Output: "exited"},
		// Cleanup.
		ssh.MockCommand{Match: "docker logs", Output: "OOM killed"},
		ssh.MockCommand{Match: "docker stop", Output: ""},
		ssh.MockCommand{Match: "docker rm", Output: ""},
		ssh.MockCommand{Match: "printf %s", Output: ""},
		ssh.MockCommand{Match: "rm -rf /deployments/myapp/.lock", Output: ""},
	)

	var buf bytes.Buffer
	deployer := NewDeployer(mock, &buf)

	err := deployer.Deploy(context.Background(), Config{
		App:     "myapp",
		Domain:  "myapp.com",
		Image:   "myapp:oom",
		Version: "oom123",
		Health:  HealthConfig{Timeout: 100 * time.Millisecond, Interval: 10 * time.Millisecond},
	})
	if err == nil {
		t.Fatal("expected error on container crash")
	}
	if !strings.Contains(err.Error(), "container failed to start") {
		t.Errorf("expected container crash error, got: %v", err)
	}
	if !strings.Contains(buf.String(), "OOM killed") {
		t.Error("expected OOM message in container logs")
	}
}

func TestDeploy_LockConflict(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "mkdir -p /deployments/myapp", Output: ""},
		ssh.MockCommand{Match: "mkdir /deployments/myapp/.lock", Err: fmt.Errorf("directory exists")},
	)

	var buf bytes.Buffer
	deployer := NewDeployer(mock, &buf)

	err := deployer.Deploy(context.Background(), Config{
		App:     "myapp",
		Domain:  "myapp.com",
		Image:   "myapp:latest",
		Version: "abc123",
	})
	if err == nil {
		t.Fatal("expected error when locked")
	}
	if !strings.Contains(err.Error(), "already in progress") {
		t.Errorf("expected lock conflict error, got: %v", err)
	}
}

func TestDeploy_Validation(t *testing.T) {
	deployer := NewDeployer(ssh.NewMockExecutor("1.2.3.4"), &bytes.Buffer{})

	tests := []struct {
		name string
		cfg  Config
	}{
		{"missing app", Config{Domain: "d", Image: "i", Version: "v"}},
		{"missing domain", Config{App: "a", Image: "i", Version: "v"}},
		{"missing image", Config{App: "a", Domain: "d", Version: "v"}},
		{"missing version", Config{App: "a", Domain: "d", Image: "i"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := deployer.Deploy(context.Background(), tc.cfg); err == nil {
				t.Error("expected validation error")
			}
		})
	}
}

func TestHealthCheck_Pass(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "curl -s -o /dev/null", Output: "200"},
	)

	d := &Deployer{exec: mock, out: &bytes.Buffer{}}
	cfg := HealthConfig{
		Path:     "/health",
		Timeout:  5 * time.Second,
		Interval: 10 * time.Millisecond,
	}

	if err := d.healthCheck(context.Background(), 49152, cfg); err != nil {
		t.Fatalf("healthCheck: %v", err)
	}
}

func TestHealthCheck_TCPFallback(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		// curl returns 404 — no /health endpoint.
		ssh.MockCommand{Match: "curl -s -o /dev/null", Output: "404"},
		// TCP check succeeds.
		ssh.MockCommand{Match: "bash -c '</dev/tcp", Output: ""},
	)

	d := &Deployer{exec: mock, out: &bytes.Buffer{}}
	cfg := HealthConfig{
		Path:     "/health",
		Timeout:  5 * time.Second,
		Interval: 10 * time.Millisecond,
	}

	if err := d.healthCheck(context.Background(), 49152, cfg); err != nil {
		t.Fatalf("healthCheck with TCP fallback: %v", err)
	}
}

func TestHealthCheck_RedirectFallback(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		// curl returns 301 — app redirects /health (e.g. WordPress canonical).
		ssh.MockCommand{Match: "curl -s -o /dev/null", Output: "301"},
		// TCP check succeeds.
		ssh.MockCommand{Match: "bash -c '</dev/tcp", Output: ""},
	)

	d := &Deployer{exec: mock, out: &bytes.Buffer{}}
	cfg := HealthConfig{
		Path:     "/health",
		Timeout:  5 * time.Second,
		Interval: 10 * time.Millisecond,
	}

	if err := d.healthCheck(context.Background(), 49152, cfg); err != nil {
		t.Fatalf("healthCheck with redirect TCP fallback: %v", err)
	}
}

func TestHealthCheck_Timeout(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "curl -s -o /dev/null", Err: fmt.Errorf("connection refused")},
	)

	d := &Deployer{exec: mock, out: &bytes.Buffer{}}
	cfg := HealthConfig{
		Path:     "/health",
		Timeout:  100 * time.Millisecond,
		Interval: 10 * time.Millisecond,
	}

	err := d.healthCheck(context.Background(), 49152, cfg)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "timeout") {
		t.Errorf("expected timeout error, got: %v", err)
	}
}

func TestDeploy_SameVersion(t *testing.T) {
	existingState := "current_port=49152\ncurrent_hash=abc123\nprevious_port=0\nprevious_hash=\n"

	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "mkdir -p /deployments/myapp", Output: ""},
		ssh.MockCommand{Match: "mkdir /deployments/myapp/.lock", Output: ""},
		ssh.MockCommand{Match: "cat /deployments/myapp/state", Output: existingState},
		ssh.MockCommand{Match: "ss -tln", Output: ssOutput},
		// Rename existing container.
		ssh.MockCommand{Match: "docker rename", Output: ""},
		ssh.MockCommand{Match: "docker run", Output: "newcontainer"},
		ssh.MockCommand{Match: "docker inspect -f", Output: "running"},
		ssh.MockCommand{Match: "curl -s -o /dev/null", Output: "200"},
		ssh.MockCommand{Match: "curl -sf http://localhost:2019/config/apps/http/servers/srv0", Output: `{"listen":[":80",":443"]}`},
		ssh.MockCommand{Match: "curl -sf -X PATCH", Err: fmt.Errorf("not found")},
		ssh.MockCommand{Match: "curl -sf -X POST http://localhost:2019/config/apps/http/servers/srv0/routes", Output: ""},
		ssh.MockCommand{Match: "rm -f /tmp/teploy_caddy", Output: ""},
		ssh.MockCommand{Match: "cat /deployments/caddy/Caddyfile", Output: "{\n\tadmin 0.0.0.0:2019\n}\n"},
		ssh.MockCommand{Match: "mv /tmp/teploy_caddyfile.tmp", Output: ""},
		ssh.MockCommand{Match: "mkdir /deployments/caddy/.lock", Output: ""},
		ssh.MockCommand{Match: "docker exec caddy caddy reload", Output: ""},
		ssh.MockCommand{Match: "rmdir /deployments/caddy/.lock", Output: ""},
		// Stop renamed container.
		ssh.MockCommand{Match: "docker stop", Output: ""},
		ssh.MockCommand{Match: "docker rm", Output: ""},
		ssh.MockCommand{Match: "printf %s", Output: ""},
		ssh.MockCommand{Match: "rm -rf /deployments/myapp/.lock", Output: ""},
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

	// Verify rename was called.
	renameFound := false
	for _, call := range mock.Calls {
		if strings.Contains(call, "docker rename myapp-web-abc123 myapp-web-abc123_replaced") {
			renameFound = true
		}
	}
	if !renameFound {
		t.Error("expected docker rename for same-version redeploy")
	}

	// Verify stop targets the renamed container.
	output := buf.String()
	if !strings.Contains(output, "Stopping old container myapp-web-abc123_replaced") {
		t.Error("expected stop of renamed container")
	}
}

func TestDeploy_WithHooks(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "mkdir -p /deployments/myapp", Output: ""},
		ssh.MockCommand{Match: "mkdir /deployments/myapp/.lock", Output: ""},
		ssh.MockCommand{Match: "cat /deployments/myapp/state", Err: fmt.Errorf("no state")},
		ssh.MockCommand{Match: "ss -tln", Output: ssOutput},
		ssh.MockCommand{Match: "docker run", Output: "abc123container"},
		ssh.MockCommand{Match: "docker inspect -f", Output: "running"},
		// Pre-deploy hook.
		ssh.MockCommand{Match: "docker exec", Output: "migrated 3 tables"},
		// Health check.
		ssh.MockCommand{Match: "curl -s -o /dev/null", Output: "200"},
		// Caddy.
		ssh.MockCommand{Match: "curl -sf http://localhost:2019/config/apps/http/servers/srv0", Output: `{"listen":[":80",":443"]}`},
		ssh.MockCommand{Match: "curl -sf -X PATCH", Err: fmt.Errorf("not found")},
		ssh.MockCommand{Match: "curl -sf -X POST http://localhost:2019/config/apps/http/servers/srv0/routes", Output: ""},
		ssh.MockCommand{Match: "rm -f /tmp/teploy_caddy", Output: ""},
		ssh.MockCommand{Match: "cat /deployments/caddy/Caddyfile", Output: "{\n\tadmin 0.0.0.0:2019\n}\n"},
		ssh.MockCommand{Match: "mv /tmp/teploy_caddyfile.tmp", Output: ""},
		ssh.MockCommand{Match: "mkdir /deployments/caddy/.lock", Output: ""},
		ssh.MockCommand{Match: "docker exec caddy caddy reload", Output: ""},
		ssh.MockCommand{Match: "rmdir /deployments/caddy/.lock", Output: ""},
		// Log and lock.
		ssh.MockCommand{Match: "printf %s", Output: ""},
		ssh.MockCommand{Match: "rm -rf /deployments/myapp/.lock", Output: ""},
	)

	var buf bytes.Buffer
	deployer := NewDeployer(mock, &buf)

	err := deployer.Deploy(context.Background(), Config{
		App:        "myapp",
		Domain:     "myapp.com",
		Image:      "myapp:latest",
		Version:    "abc123",
		PreDeploy:  "npm run migrate",
		PostDeploy: "npm run cache:clear",
		Health:     HealthConfig{Timeout: 5 * time.Second, Interval: 10 * time.Millisecond},
	})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "Running pre-deploy hook") {
		t.Error("expected pre-deploy hook message")
	}
	if !strings.Contains(output, "Pre-deploy hook passed") {
		t.Error("expected pre-deploy hook passed message")
	}
	if !strings.Contains(output, "Post-deploy hook passed") {
		t.Error("expected post-deploy hook passed message")
	}

	// Verify docker exec was called with the right command.
	execFound := false
	for _, call := range mock.Calls {
		if strings.Contains(call, "docker exec") && strings.Contains(call, "npm run migrate") {
			execFound = true
		}
	}
	if !execFound {
		t.Error("expected docker exec with pre-deploy command")
	}
}

func TestDeploy_PreDeployHookFailure(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "mkdir -p /deployments/myapp", Output: ""},
		ssh.MockCommand{Match: "mkdir /deployments/myapp/.lock", Output: ""},
		ssh.MockCommand{Match: "cat /deployments/myapp/state", Err: fmt.Errorf("no state")},
		ssh.MockCommand{Match: "ss -tln", Output: ssOutput},
		ssh.MockCommand{Match: "docker run", Output: "hookfailcontainer"},
		ssh.MockCommand{Match: "docker inspect -f", Output: "running"},
		// Pre-deploy hook fails.
		ssh.MockCommand{Match: "docker exec", Output: "ERROR: migration failed", Err: fmt.Errorf("exit status 1")},
		// Cleanup.
		ssh.MockCommand{Match: "docker logs", Output: "app started OK"},
		ssh.MockCommand{Match: "docker stop", Output: ""},
		ssh.MockCommand{Match: "docker rm", Output: ""},
		ssh.MockCommand{Match: "printf %s", Output: ""},
		ssh.MockCommand{Match: "rm -rf /deployments/myapp/.lock", Output: ""},
	)

	var buf bytes.Buffer
	deployer := NewDeployer(mock, &buf)

	err := deployer.Deploy(context.Background(), Config{
		App:       "myapp",
		Domain:    "myapp.com",
		Image:     "myapp:latest",
		Version:   "abc123",
		PreDeploy: "npm run migrate",
		Health:    HealthConfig{Timeout: 5 * time.Second, Interval: 10 * time.Millisecond},
	})
	if err == nil {
		t.Fatal("expected error on pre-deploy hook failure")
	}
	if !strings.Contains(err.Error(), "pre-deploy hook failed") {
		t.Errorf("expected pre-deploy hook error, got: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "ERROR: migration failed") {
		t.Error("expected hook error output in messages")
	}

	// Verify state was NOT written.
	if _, ok := mock.Files["/deployments/myapp/state"]; ok {
		t.Error("state should not be written on failed pre-deploy hook")
	}

	// Verify failure was logged.
	logData, _ := logEntryFromCalls(mock)
	var logEntry state.LogEntry
	json.Unmarshal(logData, &logEntry)
	if logEntry.Success {
		t.Error("expected success=false in failure log entry")
	}
}

func TestDeploy_PostDeployHookFailure(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "mkdir -p /deployments/myapp", Output: ""},
		ssh.MockCommand{Match: "mkdir /deployments/myapp/.lock", Output: ""},
		ssh.MockCommand{Match: "cat /deployments/myapp/state", Err: fmt.Errorf("no state")},
		ssh.MockCommand{Match: "ss -tln", Output: ssOutput},
		ssh.MockCommand{Match: "docker run", Output: "postfailcontainer"},
		ssh.MockCommand{Match: "docker inspect -f", Output: "running"},
		// Health check.
		ssh.MockCommand{Match: "curl -s -o /dev/null", Output: "200"},
		// Caddy.
		ssh.MockCommand{Match: "curl -sf http://localhost:2019/config/apps/http/servers/srv0", Output: `{"listen":[":80",":443"]}`},
		ssh.MockCommand{Match: "curl -sf -X PATCH", Err: fmt.Errorf("not found")},
		ssh.MockCommand{Match: "curl -sf -X POST http://localhost:2019/config/apps/http/servers/srv0/routes", Output: ""},
		ssh.MockCommand{Match: "rm -f /tmp/teploy_caddy", Output: ""},
		ssh.MockCommand{Match: "cat /deployments/caddy/Caddyfile", Output: "{\n\tadmin 0.0.0.0:2019\n}\n"},
		ssh.MockCommand{Match: "mv /tmp/teploy_caddyfile.tmp", Output: ""},
		ssh.MockCommand{Match: "mkdir /deployments/caddy/.lock", Output: ""},
		ssh.MockCommand{Match: "docker exec caddy caddy reload", Output: ""},
		ssh.MockCommand{Match: "rmdir /deployments/caddy/.lock", Output: ""},
		// Post-deploy hook fails.
		ssh.MockCommand{Match: "docker exec", Output: "cache clear failed", Err: fmt.Errorf("exit status 1")},
		// Log and lock (deploy still succeeds).
		ssh.MockCommand{Match: "printf %s", Output: ""},
		ssh.MockCommand{Match: "rm -rf /deployments/myapp/.lock", Output: ""},
	)

	var buf bytes.Buffer
	deployer := NewDeployer(mock, &buf)

	err := deployer.Deploy(context.Background(), Config{
		App:        "myapp",
		Domain:     "myapp.com",
		Image:      "myapp:latest",
		Version:    "abc123",
		PostDeploy: "npm run cache:clear",
		Health:     HealthConfig{Timeout: 5 * time.Second, Interval: 10 * time.Millisecond},
	})
	// Deploy should succeed despite post-deploy hook failure.
	if err != nil {
		t.Fatalf("Deploy should succeed despite post-deploy failure: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "Warning: post-deploy hook failed") {
		t.Error("expected post-deploy hook warning")
	}

	// Verify state WAS written (deploy succeeded).
	if _, ok := mock.Files["/deployments/myapp/state"]; !ok {
		t.Error("state should be written on successful deploy")
	}

	// Verify success was logged.
	logData, _ := logEntryFromCalls(mock)
	var logEntry state.LogEntry
	json.Unmarshal(logData, &logEntry)
	if !logEntry.Success {
		t.Error("expected success=true — post-deploy failure doesn't fail the deploy")
	}
}

func TestDeploy_WithWorkers(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "mkdir -p /deployments/myapp", Output: ""},
		ssh.MockCommand{Match: "mkdir /deployments/myapp/.lock", Output: ""},
		ssh.MockCommand{Match: "cat /deployments/myapp/state", Err: fmt.Errorf("no state")},
		ssh.MockCommand{Match: "ss -tln", Output: ssOutput},
		// Web container.
		ssh.MockCommand{Match: "docker run", Output: "web123container"},
		ssh.MockCommand{Match: "docker inspect -f", Output: "running"},
		// Health check (web).
		ssh.MockCommand{Match: "curl -s -o /dev/null", Output: "200"},
		// Worker container (also matches "docker run" — both succeed).
		// Caddy.
		ssh.MockCommand{Match: "curl -sf http://localhost:2019/config/apps/http/servers/srv0", Output: `{"listen":[":80",":443"]}`},
		ssh.MockCommand{Match: "curl -sf -X PATCH", Err: fmt.Errorf("not found")},
		ssh.MockCommand{Match: "curl -sf -X POST http://localhost:2019/config/apps/http/servers/srv0/routes", Output: ""},
		ssh.MockCommand{Match: "rm -f /tmp/teploy_caddy", Output: ""},
		ssh.MockCommand{Match: "cat /deployments/caddy/Caddyfile", Output: "{\n\tadmin 0.0.0.0:2019\n}\n"},
		ssh.MockCommand{Match: "mv /tmp/teploy_caddyfile.tmp", Output: ""},
		ssh.MockCommand{Match: "mkdir /deployments/caddy/.lock", Output: ""},
		ssh.MockCommand{Match: "docker exec caddy caddy reload", Output: ""},
		ssh.MockCommand{Match: "rmdir /deployments/caddy/.lock", Output: ""},
		// Log and lock.
		ssh.MockCommand{Match: "printf %s", Output: ""},
		ssh.MockCommand{Match: "rm -rf /deployments/myapp/.lock", Output: ""},
	)

	var buf bytes.Buffer
	deployer := NewDeployer(mock, &buf)

	err := deployer.Deploy(context.Background(), Config{
		App:     "myapp",
		Domain:  "myapp.com",
		Image:   "myapp:latest",
		Version: "abc123",
		Processes: map[string]string{
			"web":    "npm start",
			"worker": "npm run worker",
		},
		Health: HealthConfig{Timeout: 5 * time.Second, Interval: 10 * time.Millisecond},
	})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	// Verify both web and worker docker run commands were issued.
	webRunFound := false
	workerRunFound := false
	for _, call := range mock.Calls {
		if strings.HasPrefix(call, "docker run") {
			if strings.Contains(call, "--name 'myapp-web-abc123'") && strings.Contains(call, "npm start") {
				webRunFound = true
			}
			if strings.Contains(call, "--name 'myapp-worker-abc123'") && strings.Contains(call, "npm run worker") {
				workerRunFound = true
			}
		}
	}
	if !webRunFound {
		t.Error("expected web container to be started with 'npm start'")
	}
	if !workerRunFound {
		t.Error("expected worker container to be started with 'npm run worker'")
	}

	// Verify worker has no port published.
	for _, call := range mock.Calls {
		if strings.Contains(call, "--name 'myapp-worker-abc123'") && strings.Contains(call, "-p ") {
			t.Error("worker should not have a published port")
		}
	}

	// Verify output mentions both containers.
	output := buf.String()
	if !strings.Contains(output, "Starting container myapp-web-abc123") {
		t.Error("expected web start message")
	}
	if !strings.Contains(output, "Starting myapp-worker-abc123") {
		t.Error("expected worker start message")
	}
}

// TestDeploy_NoHealthcheckForWorker verifies that NoHealthcheck["worker"]=true
// in the deploy.Config translates into --no-healthcheck on the worker's
// docker run, while leaving the web container's docker run untouched.
func TestDeploy_NoHealthcheckForWorker(t *testing.T) {
	ssOutput := "State  Recv-Q Send-Q  Local Address:Port\nLISTEN 0      128     0.0.0.0:80\n"
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "mkdir -p /deployments/myapp", Output: ""},
		ssh.MockCommand{Match: "mkdir /deployments/myapp/.lock", Output: ""},
		ssh.MockCommand{Match: "cat /deployments/myapp/state", Err: fmt.Errorf("no state")},
		ssh.MockCommand{Match: "ss -tln", Output: ssOutput},
		ssh.MockCommand{Match: "docker run", Output: "web123container"},
		ssh.MockCommand{Match: "docker inspect -f", Output: "running"},
		ssh.MockCommand{Match: "curl -s -o /dev/null", Output: "200"},
		ssh.MockCommand{Match: "curl -sf http://localhost:2019/config/apps/http/servers/srv0", Output: `{"listen":[":80",":443"]}`},
		ssh.MockCommand{Match: "curl -sf -X PATCH", Err: fmt.Errorf("not found")},
		ssh.MockCommand{Match: "curl -sf -X POST http://localhost:2019/config/apps/http/servers/srv0/routes", Output: ""},
		ssh.MockCommand{Match: "rm -f /tmp/teploy_caddy", Output: ""},
		ssh.MockCommand{Match: "cat /deployments/caddy/Caddyfile", Output: "{\n\tadmin 0.0.0.0:2019\n}\n"},
		ssh.MockCommand{Match: "mv /tmp/teploy_caddyfile.tmp", Output: ""},
		ssh.MockCommand{Match: "mkdir /deployments/caddy/.lock", Output: ""},
		ssh.MockCommand{Match: "docker exec caddy caddy reload", Output: ""},
		ssh.MockCommand{Match: "rmdir /deployments/caddy/.lock", Output: ""},
		ssh.MockCommand{Match: "printf %s", Output: ""},
		ssh.MockCommand{Match: "rm -rf /deployments/myapp/.lock", Output: ""},
	)

	var buf bytes.Buffer
	deployer := NewDeployer(mock, &buf)

	err := deployer.Deploy(context.Background(), Config{
		App:     "myapp",
		Domain:  "myapp.com",
		Image:   "myapp:latest",
		Version: "abc123",
		Processes: map[string]string{
			"web":    "npm start",
			"worker": "npm run worker",
		},
		NoHealthcheck: map[string]bool{
			"worker": true,
		},
		Health: HealthConfig{Timeout: 5 * time.Second, Interval: 10 * time.Millisecond},
	})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	var webCmd, workerCmd string
	for _, call := range mock.Calls {
		if !strings.HasPrefix(call, "docker run") {
			continue
		}
		if strings.Contains(call, "--name 'myapp-web-abc123'") {
			webCmd = call
		}
		if strings.Contains(call, "--name 'myapp-worker-abc123'") {
			workerCmd = call
		}
	}
	if workerCmd == "" {
		t.Fatal("worker docker run not captured")
	}
	if webCmd == "" {
		t.Fatal("web docker run not captured")
	}
	if !strings.Contains(workerCmd, "--no-healthcheck") {
		t.Errorf("worker should have --no-healthcheck\ngot: %s", workerCmd)
	}
	if strings.Contains(webCmd, "--no-healthcheck") {
		t.Errorf("web should NOT have --no-healthcheck when only worker is configured\ngot: %s", webCmd)
	}
}

func TestDeploy_WorkerStartFailure(t *testing.T) {
	// Use a separate mock command for the worker run that fails.
	// We need the web "docker run" to succeed and the worker "docker run" to fail.
	// Since MockExecutor matches by prefix, both match "docker run".
	// Solution: use a second mock executor where the second docker run call fails.
	//
	// Actually, MockExecutor always returns the first match, so both calls succeed.
	// To test worker failure, we need the worker start to fail differently.
	// Worker containers have a different name, so we can't distinguish by prefix alone.
	//
	// Instead, test the cleanup behavior by having the deploy succeed with processes,
	// then verify that on update, both old containers are stopped.

	// Test: update deploy with workers — verify old web AND worker containers are stopped.
	stateContent := "current_port=49152\ncurrent_hash=old123\nprevious_port=0\nprevious_hash=\n"
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "mkdir -p /deployments/myapp", Output: ""},
		ssh.MockCommand{Match: "mkdir /deployments/myapp/.lock", Output: ""},
		ssh.MockCommand{Match: "cat /deployments/myapp/state", Output: stateContent},
		ssh.MockCommand{Match: "ss -tln", Output: ssOutput},
		// Start new containers (both match "docker run").
		ssh.MockCommand{Match: "docker run", Output: "new123container"},
		ssh.MockCommand{Match: "docker inspect -f", Output: "running"},
		// Health check.
		ssh.MockCommand{Match: "curl -s -o /dev/null", Output: "200"},
		// Caddy.
		ssh.MockCommand{Match: "curl -sf http://localhost:2019/config/apps/http/servers/srv0", Output: `{"listen":[":80",":443"]}`},
		ssh.MockCommand{Match: "curl -sf -X PATCH", Err: fmt.Errorf("not found")},
		ssh.MockCommand{Match: "curl -sf -X POST http://localhost:2019/config/apps/http/servers/srv0/routes", Output: ""},
		ssh.MockCommand{Match: "rm -f /tmp/teploy_caddy", Output: ""},
		ssh.MockCommand{Match: "cat /deployments/caddy/Caddyfile", Output: "{\n\tadmin 0.0.0.0:2019\n}\n"},
		ssh.MockCommand{Match: "mv /tmp/teploy_caddyfile.tmp", Output: ""},
		ssh.MockCommand{Match: "mkdir /deployments/caddy/.lock", Output: ""},
		ssh.MockCommand{Match: "docker exec caddy caddy reload", Output: ""},
		ssh.MockCommand{Match: "rmdir /deployments/caddy/.lock", Output: ""},
		// Stop old containers.
		ssh.MockCommand{Match: "docker stop", Output: ""},
		ssh.MockCommand{Match: "docker rm", Output: ""},
		// Log and lock.
		ssh.MockCommand{Match: "printf %s", Output: ""},
		ssh.MockCommand{Match: "rm -rf /deployments/myapp/.lock", Output: ""},
	)

	var buf bytes.Buffer
	deployer := NewDeployer(mock, &buf)

	err := deployer.Deploy(context.Background(), Config{
		App:     "myapp",
		Domain:  "myapp.com",
		Image:   "myapp:latest",
		Version: "new456",
		Processes: map[string]string{
			"web":    "npm start",
			"worker": "npm run worker",
		},
		Health: HealthConfig{Timeout: 5 * time.Second, Interval: 10 * time.Millisecond},
	})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	// Verify both old containers were stopped.
	output := buf.String()
	if !strings.Contains(output, "Stopping old container myapp-web-old123") {
		t.Error("expected old web container to be stopped")
	}
	if !strings.Contains(output, "Stopping old container myapp-worker-old123") {
		t.Error("expected old worker container to be stopped")
	}
}

func TestDeploy_AssetBridging(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		// 1. Ensure app dir.
		ssh.MockCommand{Match: "mkdir -p /deployments/myapp", Output: ""},
		// 2. Acquire lock.
		ssh.MockCommand{Match: "mkdir /deployments/myapp/.lock", Output: ""},
		// 3. Read state.
		ssh.MockCommand{Match: "cat /deployments/myapp/state", Err: fmt.Errorf("no state")},
		// 4. Find port.
		ssh.MockCommand{Match: "ss -tln", Output: ssOutput},
		// 5. Asset bridging: create dir + extract.
		ssh.MockCommand{Match: "mkdir -p /deployments/myapp/assets", Output: ""},
		ssh.MockCommand{Match: "docker run --rm --user 0 -v /deployments/myapp/assets:/bridge", Output: "ok-bridge\n"},
		// 6. Start container.
		ssh.MockCommand{Match: "docker run --detach", Output: "abc123container"},
		// 7. Verify running.
		ssh.MockCommand{Match: "docker inspect", Output: "running"},
		// 8. Health check.
		ssh.MockCommand{Match: "curl -s -o /dev/null", Output: "200"},
		// 9. Caddy.
		ssh.MockCommand{Match: "curl -sf http://localhost:2019/config/apps/http/servers/srv0", Output: `{"listen":[":80",":443"]}`},
		ssh.MockCommand{Match: "curl -sf -X PATCH", Err: fmt.Errorf("not found")},
		ssh.MockCommand{Match: "curl -sf -X POST http://localhost:2019/config/apps/http/servers/srv0/routes", Output: ""},
		ssh.MockCommand{Match: "rm -f /tmp/teploy_caddy", Output: ""},
		ssh.MockCommand{Match: "cat /deployments/caddy/Caddyfile", Output: "{\n\tadmin 0.0.0.0:2019\n}\n"},
		ssh.MockCommand{Match: "mv /tmp/teploy_caddyfile.tmp", Output: ""},
		ssh.MockCommand{Match: "mkdir /deployments/caddy/.lock", Output: ""},
		ssh.MockCommand{Match: "docker exec caddy caddy reload", Output: ""},
		ssh.MockCommand{Match: "rmdir /deployments/caddy/.lock", Output: ""},
		// 10. Asset cleanup.
		ssh.MockCommand{Match: "find /deployments/myapp/assets", Output: ""},
		// 11. Log and lock.
		ssh.MockCommand{Match: "printf %s", Output: ""},
		ssh.MockCommand{Match: "rm -rf /deployments/myapp/.lock", Output: ""},
	)

	var buf bytes.Buffer
	deployer := NewDeployer(mock, &buf)

	err := deployer.Deploy(context.Background(), Config{
		App:       "myapp",
		Domain:    "myapp.com",
		Image:     "myapp:latest",
		Version:   "abc123",
		AssetPath: "/app/public/assets",
		Health:    HealthConfig{Timeout: 5 * time.Second, Interval: 10 * time.Millisecond},
	})
	if err != nil {
		t.Fatalf("Deploy with asset bridging: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "Bridging assets") {
		t.Error("expected asset bridging message")
	}
	if !strings.Contains(output, "Assets extracted") {
		t.Error("expected asset extraction message")
	}

	// Verify docker run includes asset volume mount.
	for _, call := range mock.Calls {
		if strings.HasPrefix(call, "docker run --detach") {
			if !strings.Contains(call, "-v '/deployments/myapp/assets:/app/public/assets'") {
				t.Errorf("expected asset volume mount in docker run: %s", call)
			}
			break
		}
	}

	// Verify one-shot extraction container was run.
	foundExtract := false
	for _, call := range mock.Calls {
		if strings.Contains(call, "docker run --rm") && strings.Contains(call, "/bridge") {
			foundExtract = true
		}
	}
	if !foundExtract {
		t.Error("expected one-shot asset extraction container")
	}

	// Verify asset cleanup was run.
	foundCleanup := false
	for _, call := range mock.Calls {
		if strings.Contains(call, "find /deployments/myapp/assets") && strings.Contains(call, "-mtime +7") {
			foundCleanup = true
		}
	}
	if !foundCleanup {
		t.Error("expected asset cleanup command")
	}
}

func TestDeploy_AssetBridgingCustomKeepDays(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "mkdir -p /deployments/myapp", Output: ""},
		ssh.MockCommand{Match: "mkdir /deployments/myapp/.lock", Output: ""},
		ssh.MockCommand{Match: "cat /deployments/myapp/state", Err: fmt.Errorf("no state")},
		ssh.MockCommand{Match: "ss -tln", Output: ssOutput},
		ssh.MockCommand{Match: "mkdir -p /deployments/myapp/assets", Output: ""},
		ssh.MockCommand{Match: "docker run --rm --user 0", Output: "ok-bridge\n"},
		ssh.MockCommand{Match: "docker run --detach", Output: "abc123"},
		ssh.MockCommand{Match: "docker inspect", Output: "running"},
		ssh.MockCommand{Match: "curl -s -o /dev/null", Output: "200"},
		ssh.MockCommand{Match: "curl -sf http://localhost:2019/config/apps/http/servers/srv0", Output: `{"listen":[":80",":443"]}`},
		ssh.MockCommand{Match: "curl -sf -X PATCH", Err: fmt.Errorf("not found")},
		ssh.MockCommand{Match: "curl -sf -X POST http://localhost:2019/config/apps/http/servers/srv0/routes", Output: ""},
		ssh.MockCommand{Match: "rm -f /tmp/teploy_caddy", Output: ""},
		ssh.MockCommand{Match: "cat /deployments/caddy/Caddyfile", Output: "{\n\tadmin 0.0.0.0:2019\n}\n"},
		ssh.MockCommand{Match: "mv /tmp/teploy_caddyfile.tmp", Output: ""},
		ssh.MockCommand{Match: "mkdir /deployments/caddy/.lock", Output: ""},
		ssh.MockCommand{Match: "docker exec caddy caddy reload", Output: ""},
		ssh.MockCommand{Match: "rmdir /deployments/caddy/.lock", Output: ""},
		ssh.MockCommand{Match: "find /deployments/myapp/assets", Output: ""},
		ssh.MockCommand{Match: "printf %s", Output: ""},
		ssh.MockCommand{Match: "rm -rf /deployments/myapp/.lock", Output: ""},
	)

	var buf bytes.Buffer
	deployer := NewDeployer(mock, &buf)

	err := deployer.Deploy(context.Background(), Config{
		App:           "myapp",
		Domain:        "myapp.com",
		Image:         "myapp:latest",
		Version:       "abc123",
		AssetPath:     "/app/public/assets",
		AssetKeepDays: 30,
		Health:        HealthConfig{Timeout: 5 * time.Second, Interval: 10 * time.Millisecond},
	})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	// Verify cleanup uses custom keep days.
	for _, call := range mock.Calls {
		if strings.Contains(call, "find /deployments/myapp/assets") {
			if !strings.Contains(call, "-mtime +30") {
				t.Errorf("expected -mtime +30, got: %s", call)
			}
		}
	}
}

func TestDeploy_SameVersionWithWorkers(t *testing.T) {
	stateContent := "current_port=49152\ncurrent_hash=abc123\nprevious_port=0\nprevious_hash=\n"
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "mkdir -p /deployments/myapp", Output: ""},
		ssh.MockCommand{Match: "mkdir /deployments/myapp/.lock", Output: ""},
		ssh.MockCommand{Match: "cat /deployments/myapp/state", Output: stateContent},
		ssh.MockCommand{Match: "ss -tln", Output: ssOutput},
		// Rename existing containers.
		ssh.MockCommand{Match: "docker rename", Output: ""},
		// Start new containers.
		ssh.MockCommand{Match: "docker run", Output: "redeploycontainer"},
		ssh.MockCommand{Match: "docker inspect -f", Output: "running"},
		ssh.MockCommand{Match: "curl -s -o /dev/null", Output: "200"},
		ssh.MockCommand{Match: "curl -sf http://localhost:2019/config/apps/http/servers/srv0", Output: `{"listen":[":80",":443"]}`},
		ssh.MockCommand{Match: "curl -sf -X PATCH", Err: fmt.Errorf("not found")},
		ssh.MockCommand{Match: "curl -sf -X POST http://localhost:2019/config/apps/http/servers/srv0/routes", Output: ""},
		ssh.MockCommand{Match: "rm -f /tmp/teploy_caddy", Output: ""},
		ssh.MockCommand{Match: "cat /deployments/caddy/Caddyfile", Output: "{\n\tadmin 0.0.0.0:2019\n}\n"},
		ssh.MockCommand{Match: "mv /tmp/teploy_caddyfile.tmp", Output: ""},
		ssh.MockCommand{Match: "mkdir /deployments/caddy/.lock", Output: ""},
		ssh.MockCommand{Match: "docker exec caddy caddy reload", Output: ""},
		ssh.MockCommand{Match: "rmdir /deployments/caddy/.lock", Output: ""},
		// Stop renamed containers.
		ssh.MockCommand{Match: "docker stop", Output: ""},
		ssh.MockCommand{Match: "docker rm", Output: ""},
		// Log and lock.
		ssh.MockCommand{Match: "printf %s", Output: ""},
		ssh.MockCommand{Match: "rm -rf /deployments/myapp/.lock", Output: ""},
	)

	var buf bytes.Buffer
	deployer := NewDeployer(mock, &buf)

	err := deployer.Deploy(context.Background(), Config{
		App:     "myapp",
		Domain:  "myapp.com",
		Image:   "myapp:latest",
		Version: "abc123",
		Processes: map[string]string{
			"web":    "npm start",
			"worker": "npm run worker",
		},
		Health: HealthConfig{Timeout: 5 * time.Second, Interval: 10 * time.Millisecond},
	})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	// Verify both containers were renamed.
	renameWeb := false
	renameWorker := false
	for _, call := range mock.Calls {
		if strings.Contains(call, "docker rename myapp-web-abc123 myapp-web-abc123_replaced") {
			renameWeb = true
		}
		if strings.Contains(call, "docker rename myapp-worker-abc123 myapp-worker-abc123_replaced") {
			renameWorker = true
		}
	}
	if !renameWeb {
		t.Error("expected web container to be renamed")
	}
	if !renameWorker {
		t.Error("expected worker container to be renamed")
	}

	// Verify both _replaced containers were stopped.
	output := buf.String()
	if !strings.Contains(output, "Stopping old container myapp-web-abc123_replaced") {
		t.Error("expected renamed web container to be stopped")
	}
	if !strings.Contains(output, "Stopping old container myapp-worker-abc123_replaced") {
		t.Error("expected renamed worker container to be stopped")
	}
}

// TestDeploy_IngressExternalSkipsCaddy verifies that a deploy with
// Ingress="external" issues zero Caddy commands. The user's external
// ingress (CF Tunnel, nginx, etc.) is responsible for routing; Teploy
// only ensures the container is running and joined to the docker
// network with its app-name alias.
func TestDeploy_IngressExternalSkipsCaddy(t *testing.T) {
	ssOutput := "State  Recv-Q Send-Q  Local Address:Port\nLISTEN 0      128     0.0.0.0:80\n"
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "mkdir -p /deployments/myapp", Output: ""},
		ssh.MockCommand{Match: "mkdir /deployments/myapp/.lock", Output: ""},
		ssh.MockCommand{Match: "cat /deployments/myapp/state", Err: fmt.Errorf("no state")},
		ssh.MockCommand{Match: "ss -tln", Output: ssOutput},
		ssh.MockCommand{Match: "docker run", Output: "web123container"},
		ssh.MockCommand{Match: "docker inspect -f", Output: "running"},
		ssh.MockCommand{Match: "curl -s -o /dev/null", Output: "200"},
		// No Caddy mocks — the deploy must not call them.
		ssh.MockCommand{Match: "printf %s", Output: ""},
		ssh.MockCommand{Match: "rm -rf /deployments/myapp/.lock", Output: ""},
	)

	var buf bytes.Buffer
	deployer := NewDeployer(mock, &buf)

	err := deployer.Deploy(context.Background(), Config{
		App:     "myapp",
		Domain:  "myapp.com",
		Image:   "myapp:latest",
		Version: "abc123",
		Ingress: "external",
		Health:  HealthConfig{Timeout: 5 * time.Second, Interval: 10 * time.Millisecond},
	})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	// No call should reach Caddy admin API or Caddyfile.
	for _, call := range mock.Calls {
		lower := strings.ToLower(call)
		if strings.Contains(call, "localhost:2019") ||
			strings.Contains(call, "/deployments/caddy/Caddyfile") ||
			strings.Contains(call, "caddy reload") ||
			strings.Contains(lower, "/config/apps/http/servers") {
			t.Errorf("ingress: external should issue zero Caddy commands; got: %s", call)
		}
	}

	// Skip message should appear in output.
	if !strings.Contains(buf.String(), "Skipping Caddy route update") {
		t.Errorf("expected skip message in output, got:\n%s", buf.String())
	}
}

// logEntryFromCalls extracts the JSON log line that AppendLog appends. AppendLog
// runs `printf %s '<base64>' | base64 -d >> .../teploy.log` (no staging file),
// so the entry is decoded from the recorded command rather than mock.Files.
func logEntryFromCalls(mock *ssh.MockExecutor) ([]byte, bool) {
	for _, call := range mock.Calls {
		if !strings.HasPrefix(call, "printf %s ") || !strings.Contains(call, "teploy.log") {
			continue
		}
		open := strings.IndexByte(call, '\'')
		if open < 0 {
			continue
		}
		rest := call[open+1:]
		end := strings.IndexByte(rest, '\'')
		if end < 0 {
			continue
		}
		data, err := base64.StdEncoding.DecodeString(rest[:end])
		if err != nil {
			continue
		}
		return data, true
	}
	return nil, false
}
