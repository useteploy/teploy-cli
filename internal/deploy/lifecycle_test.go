package deploy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/ssh"
)

func TestStop(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "docker ps --all --filter label=teploy.app=myapp",
			Output: `{"ID":"abc123","Names":"myapp-web-v1","Image":"myapp:latest","State":"running","Status":"Up 2h","Labels":"teploy.app=myapp,teploy.version=v1"}` + "\n" +
				`{"ID":"def456","Names":"myapp-worker-v1","Image":"myapp:latest","State":"running","Status":"Up 2h","Labels":"teploy.app=myapp,teploy.version=v1"}`,
		},
		ssh.MockCommand{Match: "docker stop", Output: ""},
		ssh.MockCommand{Match: "cat /tmp", Output: ""},
		ssh.MockCommand{Match: "UPLOAD:", Output: ""},
	)

	var buf bytes.Buffer
	lc := NewLifecycle(mock, &buf)
	if err := lc.Stop(context.Background(), "myapp", 10); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify both containers were stopped.
	stopCalls := 0
	for _, call := range mock.Calls {
		if strings.HasPrefix(call, "docker stop") {
			stopCalls++
		}
	}
	if stopCalls != 2 {
		t.Errorf("expected 2 stop calls, got %d", stopCalls)
	}

	if !strings.Contains(buf.String(), "Stopped") {
		t.Error("expected 'Stopped' message")
	}
}

func TestStop_NoContainers(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "docker ps --all --filter label=teploy.app=myapp", Output: ""},
	)

	lc := NewLifecycle(mock, &bytes.Buffer{})
	err := lc.Stop(context.Background(), "myapp", 10)
	if err == nil {
		t.Fatal("expected error for no running containers")
	}
}

func TestStart(t *testing.T) {
	stateContent := "current_port=49152\ncurrent_hash=v1\n"
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "cat /deployments/myapp/state", Output: stateContent},
		ssh.MockCommand{Match: "docker ps --all --filter label=teploy.app=myapp",
			Output: `{"ID":"abc123","Names":"myapp-web-v1","Image":"myapp:latest","State":"exited","Status":"Exited (0)","Labels":"teploy.app=myapp,teploy.version=v1"}`,
		},
		ssh.MockCommand{Match: "docker start", Output: ""},
		ssh.MockCommand{Match: "curl", Output: "200"},
		ssh.MockCommand{Match: "cat /tmp", Output: ""},
		ssh.MockCommand{Match: "UPLOAD:", Output: ""},
	)

	var buf bytes.Buffer
	lc := NewLifecycle(mock, &buf)
	if err := lc.Start(context.Background(), "myapp"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(buf.String(), "Started") {
		t.Error("expected 'Started' message")
	}
}

func TestStart_NoState(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "cat /deployments/myapp/state", Err: fmt.Errorf("not found")},
	)

	lc := NewLifecycle(mock, &bytes.Buffer{})
	err := lc.Start(context.Background(), "myapp")
	if err == nil {
		t.Fatal("expected error for no state")
	}
	if !strings.Contains(err.Error(), "deploy first") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestRestart(t *testing.T) {
	stateContent := "current_port=49152\ncurrent_hash=v1\n"
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "cat /deployments/myapp/state", Output: stateContent},
		ssh.MockCommand{Match: "docker ps --all --filter label=teploy.app=myapp",
			Output: `{"ID":"abc123","Names":"myapp-web-v1","Image":"myapp:latest","State":"running","Status":"Up 2h","Labels":"teploy.app=myapp,teploy.version=v1"}`,
		},
		ssh.MockCommand{Match: "docker stop", Output: ""},
		ssh.MockCommand{Match: "docker start", Output: ""},
		ssh.MockCommand{Match: "curl", Output: "200"},
		ssh.MockCommand{Match: "cat /tmp", Output: ""},
		ssh.MockCommand{Match: "UPLOAD:", Output: ""},
	)

	var buf bytes.Buffer
	lc := NewLifecycle(mock, &buf)
	if err := lc.Restart(context.Background(), "myapp", 10); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "Stopping") {
		t.Error("expected 'Stopping' message")
	}
	if !strings.Contains(output, "Starting") {
		t.Error("expected 'Starting' message")
	}
	if !strings.Contains(output, "Restarted") {
		t.Error("expected 'Restarted' message")
	}
}

func TestStop_LogsAction(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "docker ps --all --filter label=teploy.app=myapp",
			Output: `{"ID":"abc123","Names":"myapp-web-v1","Image":"myapp:latest","State":"running","Status":"Up 2h","Labels":"teploy.app=myapp,teploy.version=v1"}`,
		},
		ssh.MockCommand{Match: "docker stop", Output: ""},
		ssh.MockCommand{Match: "cat /tmp", Output: ""},
		ssh.MockCommand{Match: "UPLOAD:", Output: ""},
	)

	lc := NewLifecycle(mock, &bytes.Buffer{})
	lc.Stop(context.Background(), "myapp", 10)

	// Verify a log entry was written.
	data, ok := logEntryFromCalls(mock)
	if ok {
		var entry map[string]interface{}
		if err := json.Unmarshal(data, &entry); err == nil {
			if entry["type"] == "stop" && entry["app"] == "myapp" {
				return // found it
			}
		}
	}
	t.Error("expected log entry with type=stop")
}
