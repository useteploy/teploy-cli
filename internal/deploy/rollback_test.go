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

func rollbackCfg() RollbackConfig {
	return RollbackConfig{
		App:    "myapp",
		Domain: "myapp.com",
		Health: HealthConfig{Timeout: 5 * time.Second, Interval: 10 * time.Millisecond},
	}
}

func TestRollback(t *testing.T) {
	stateContent := "current_port=49153\ncurrent_hash=v2\nprevious_port=49152\nprevious_hash=v1\n"
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "cat /deployments/myapp/state", Output: stateContent},
		ssh.MockCommand{Match: "docker ps --all --filter label=teploy.app=myapp",
			Output: `{"ID":"aaa","Names":"myapp-web-v1","Image":"myapp:latest","State":"exited","Status":"Exited"}` + "\n" +
				`{"ID":"bbb","Names":"myapp-web-v2","Image":"myapp:latest","State":"running","Status":"Up 1h"}`,
		},
		// rollback uses Restart (inspect → rm -f → docker run) instead of bare
		// `docker start` so HostConfig.PortBindings actually re-apply on Docker 29.
		ssh.MockCommand{Match: "docker inspect myapp-web-v1", Output: `[{"Config":{"Image":"myapp:latest","Labels":{"teploy.app":"myapp"}},"HostConfig":{"NetworkMode":"teploy","PortBindings":{"3000/tcp":[{"HostIp":"127.0.0.1","HostPort":"49152"}]},"RestartPolicy":{"Name":"no"}},"NetworkSettings":{"Networks":{"teploy":{"Aliases":["myapp"]}}}}]`},
		ssh.MockCommand{Match: "docker rm -f myapp-web-v1", Output: ""},
		ssh.MockCommand{Match: "docker run", Output: ""},
		ssh.MockCommand{Match: "curl", Output: "200"},
		// Previous container's internal port resolved via docker inspect -f.
		ssh.MockCommand{Match: "docker inspect -f '{{range $p, $_ := .NetworkSettings.Ports}}", Output: "3000/tcp"},
		ssh.MockCommand{Match: "caddy", Output: ""},
		ssh.MockCommand{Match: "cat /deployments/caddy/Caddyfile", Output: "{\n\tadmin 0.0.0.0:2019\n}\n"},
		ssh.MockCommand{Match: "mv /tmp/teploy_caddyfile.tmp", Output: ""},
		ssh.MockCommand{Match: "mkdir /deployments/caddy/.lock", Output: ""},
		ssh.MockCommand{Match: "docker exec caddy caddy reload", Output: ""},
		ssh.MockCommand{Match: "rmdir /deployments/caddy/.lock", Output: ""},
		ssh.MockCommand{Match: "docker stop", Output: ""},
		ssh.MockCommand{Match: "mkdir -p", Output: ""},
		ssh.MockCommand{Match: "cat /tmp", Output: ""},
		ssh.MockCommand{Match: "UPLOAD:", Output: ""},
	)

	var buf bytes.Buffer
	err := Rollback(context.Background(), mock, &buf, rollbackCfg())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "Rolling back") {
		t.Error("expected 'Rolling back' message")
	}
	if !strings.Contains(output, "Rolled back myapp to version v1") {
		t.Errorf("expected rollback success message, got: %s", output)
	}

	// Verify previous container was recreated (Restart = inspect → rm -f → run).
	var inspected, removed, ran bool
	for _, call := range mock.Calls {
		switch {
		case strings.HasPrefix(call, "docker inspect myapp-web-v1"):
			inspected = true
		case strings.HasPrefix(call, "docker rm -f myapp-web-v1"):
			removed = true
		case strings.HasPrefix(call, "docker run") && strings.Contains(call, "myapp-web-v1"):
			ran = true
		}
	}
	if !inspected || !removed || !ran {
		t.Errorf("expected Restart sequence (inspect+rm+run) for v1; inspected=%v removed=%v ran=%v",
			inspected, removed, ran)
	}

	// Verify Caddy was pointed at the container's *internal* port, not
	// the host-mapped PreviousPort (49152). The container exposes 3000.
	var caddyfile []byte
	for path, data := range mock.Files {
		if strings.HasSuffix(path, "teploy_caddyfile.tmp") {
			caddyfile = data
		}
	}
	if caddyfile == nil {
		t.Fatal("expected Caddyfile to be written")
	}
	if !strings.Contains(string(caddyfile), "myapp-web-v1:3000") {
		t.Errorf("rollback route should dial container's internal port (3000), got: %s", string(caddyfile))
	}
	if strings.Contains(string(caddyfile), ":49152") {
		t.Errorf("rollback route should not use host port 49152, got: %s", string(caddyfile))
	}

	// Verify current container was stopped.
	stopCalls := 0
	for _, call := range mock.Calls {
		if strings.HasPrefix(call, "docker stop") {
			stopCalls++
			if !strings.Contains(call, "myapp-web-v2") {
				t.Errorf("expected stop for v2 container, got: %s", call)
			}
		}
	}
	if stopCalls != 1 {
		t.Errorf("expected 1 stop call, got %d", stopCalls)
	}

	// Verify state was swapped.
	stateData := string(mock.Files["/deployments/myapp/state"])
	if !strings.Contains(stateData, "current_hash=v1") {
		t.Errorf("expected state current_hash=v1, got: %s", stateData)
	}
	if !strings.Contains(stateData, "previous_hash=v2") {
		t.Errorf("expected state previous_hash=v2, got: %s", stateData)
	}
}

func TestRollback_NoState(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "cat /deployments/myapp/state", Err: fmt.Errorf("not found")},
	)

	err := Rollback(context.Background(), mock, &bytes.Buffer{}, rollbackCfg())
	if err == nil {
		t.Fatal("expected error for no state")
	}
	if !strings.Contains(err.Error(), "deploy first") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestRollback_NoPreviousDeploy(t *testing.T) {
	stateContent := "current_port=49152\ncurrent_hash=v1\n"
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "cat /deployments/myapp/state", Output: stateContent},
	)

	err := Rollback(context.Background(), mock, &bytes.Buffer{}, rollbackCfg())
	if err == nil {
		t.Fatal("expected error for no previous deploy")
	}
	if !strings.Contains(err.Error(), "no previous deploy") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestRollback_NoPreviousContainers(t *testing.T) {
	stateContent := "current_port=49153\ncurrent_hash=v2\nprevious_port=49152\nprevious_hash=v1\n"
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "cat /deployments/myapp/state", Output: stateContent},
		ssh.MockCommand{Match: "docker ps --all --filter label=teploy.app=myapp",
			Output: `{"ID":"bbb","Names":"myapp-web-v2","Image":"myapp:latest","State":"running","Status":"Up 1h"}`,
		},
	)

	err := Rollback(context.Background(), mock, &bytes.Buffer{}, rollbackCfg())
	if err == nil {
		t.Fatal("expected error for missing previous containers")
	}
	if !strings.Contains(err.Error(), "no previous containers") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestRollback_HealthCheckFails(t *testing.T) {
	stateContent := "current_port=49153\ncurrent_hash=v2\nprevious_port=49152\nprevious_hash=v1\n"
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "cat /deployments/myapp/state", Output: stateContent},
		ssh.MockCommand{Match: "docker ps --all --filter label=teploy.app=myapp",
			Output: `{"ID":"aaa","Names":"myapp-web-v1","Image":"myapp:latest","State":"exited","Status":"Exited"}` + "\n" +
				`{"ID":"bbb","Names":"myapp-web-v2","Image":"myapp:latest","State":"running","Status":"Up 1h"}`,
		},
		// Restart (inspect → rm -f → docker run).
		ssh.MockCommand{Match: "docker inspect myapp-web-v1", Output: `[{"Config":{"Image":"myapp:latest","Labels":{"teploy.app":"myapp"}},"HostConfig":{"NetworkMode":"teploy","PortBindings":{"3000/tcp":[{"HostIp":"127.0.0.1","HostPort":"49152"}]},"RestartPolicy":{"Name":"no"}},"NetworkSettings":{"Networks":{"teploy":{"Aliases":["myapp"]}}}}]`},
		ssh.MockCommand{Match: "docker rm -f myapp-web-v1", Output: ""},
		ssh.MockCommand{Match: "docker run", Output: ""},
		ssh.MockCommand{Match: "curl", Err: fmt.Errorf("connection refused")},
		ssh.MockCommand{Match: "bash -c", Err: fmt.Errorf("connection refused")},
		ssh.MockCommand{Match: "docker stop", Output: ""},
	)

	cfg := rollbackCfg()
	cfg.Health = HealthConfig{Timeout: 100 * time.Millisecond, Interval: 10 * time.Millisecond}
	err := Rollback(context.Background(), mock, &bytes.Buffer{}, cfg)
	if err == nil {
		t.Fatal("expected error for health check failure")
	}
	if !strings.Contains(err.Error(), "health check failed") {
		t.Errorf("unexpected error: %v", err)
	}
}
