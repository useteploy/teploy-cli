package deploy

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy/internal/ssh"
	"github.com/useteploy/teploy/internal/state"
)

// predecessorInventoryJSON renders one running predecessor web container
// of release old123 in docker.ListContainers' custom --format shape
// (labels as a JSON object, audit T15).
const predecessorInventoryJSON = `{"ID":"cid-old123","Names":"myapp-web-old123","Image":"myapp:old","State":"running","Status":"Up 2 hours","CreatedAt":"2026-09-20 10:00:00 +0000 UTC","Labels":{"teploy.app":"myapp","teploy.process":"web","teploy.version":"old123"}}` + "\n"

// TestDeploy_LogsDegradedWhenRetirementPartiallyFails is the C01-5 core
// regression: a deploy whose traffic switched and committed, but whose
// predecessor retirement partially failed, must be logged as an outcome
// that is NEITHER clean success NOR deploy failure — Success stays true
// (the app serves the new generation) and Degraded becomes true with the
// escaped retirement itemized. The old log recorded clean success, so a
// fleet rollback keyed on the log would skip a host still running part
// of the superseded generation.
func TestDeploy_LogsDegradedWhenRetirementPartiallyFails(t *testing.T) {
	existingState := "current_port=49152\ncurrent_hash=old123\nprevious_port=0\nprevious_hash=\n"

	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "mkdir -p /deployments/myapp", Output: ""},
		ssh.MockCommand{Match: "mkdir /deployments/myapp/.lock", Output: ""},
		ssh.MockCommand{Match: "if [ ! -e '/deployments/myapp/state.json' ]", Output: "absent"},
		ssh.MockCommand{Match: "if [ ! -e '/deployments/myapp/state' ]", Output: "present\n" + existingState},
		ssh.MockCommand{Match: "ss -tln", Output: ssOutput},
		// The predecessor snapshot listing (step 6b) sees old123 running.
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
		ssh.MockCommand{Match: "rmdir /deployments/caddy/.lock", Output: ""},
		// Post-commit retirement of the snapshotted predecessor FAILS.
		ssh.MockCommand{Match: "docker stop", Err: errBoom},
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
		t.Fatalf("a partial retirement failure is degraded, not a failed deploy: %v", err)
	}

	logData, ok := logEntryFromCalls(mock)
	if !ok {
		t.Fatal("log entry not written")
	}
	var logEntry state.LogEntry
	if err := json.Unmarshal(logData, &logEntry); err != nil {
		t.Fatalf("parsing log entry: %v", err)
	}
	if !logEntry.Success {
		t.Error("Success must stay true: traffic switched and the app serves (this is not a deploy failure)")
	}
	if !logEntry.Degraded {
		t.Error("Degraded must be true: the predecessor escaped retirement and the log must not record clean success")
	}
	if !strings.Contains(logEntry.DegradedReason, "myapp-web-old123") {
		t.Errorf("DegradedReason must name the escaped container, got %q", logEntry.DegradedReason)
	}
	if !strings.Contains(buf.String(), "myapp-web-old123") {
		t.Error("the deploy output must still report the failed retirement")
	}
}

// TestDeploy_LogsCleanSuccessWhenRetirementCompletes pins the other side
// of C01-5: a fully retired predecessor logs Success WITHOUT the degraded
// flag — the new outcome class must not leak into clean deploys.
func TestDeploy_LogsCleanSuccessWhenRetirementCompletes(t *testing.T) {
	existingState := "current_port=49152\ncurrent_hash=old123\nprevious_port=0\nprevious_hash=\n"

	mock := ssh.NewMockExecutor("1.2.3.4",
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
		ssh.MockCommand{Match: "rmdir /deployments/caddy/.lock", Output: ""},
		ssh.MockCommand{Match: "docker stop", Output: ""},
		ssh.MockCommand{Match: "printf %s", Output: ""},
		ssh.MockCommand{Match: "rm -rf /deployments/myapp/.lock", Output: ""},
	)

	var buf bytes.Buffer
	deployer := NewDeployer(mock, &buf)
	if err := deployer.Deploy(context.Background(), Config{
		App:     "myapp",
		Domain:  "myapp.com",
		Image:   "myapp:v2",
		Version: "new456",
		Health:  HealthConfig{Timeout: 5 * time.Second, Interval: 10 * time.Millisecond},
	}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	logData, ok := logEntryFromCalls(mock)
	if !ok {
		t.Fatal("log entry not written")
	}
	var logEntry state.LogEntry
	if err := json.Unmarshal(logData, &logEntry); err != nil {
		t.Fatalf("parsing log entry: %v", err)
	}
	if !logEntry.Success || logEntry.Degraded {
		t.Errorf("a clean deploy must log clean success, got success=%v degraded=%v", logEntry.Success, logEntry.Degraded)
	}
}
