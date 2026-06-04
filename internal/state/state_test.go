package state

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/ssh"
)

func TestRead(t *testing.T) {
	stateContent := "current_port=49153\ncurrent_hash=b2c4d6e8\nprevious_port=49152\nprevious_hash=6ef8a6a8\n"

	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "cat /deployments/myapp/state", Output: stateContent},
	)

	s, err := Read(context.Background(), mock, "myapp")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if s == nil {
		t.Fatal("expected non-nil state")
	}
	if s.CurrentPort != 49153 {
		t.Errorf("expected current_port 49153, got %d", s.CurrentPort)
	}
	if s.CurrentHash != "b2c4d6e8" {
		t.Errorf("expected current_hash b2c4d6e8, got %s", s.CurrentHash)
	}
	if s.PreviousPort != 49152 {
		t.Errorf("expected previous_port 49152, got %d", s.PreviousPort)
	}
	if s.PreviousHash != "6ef8a6a8" {
		t.Errorf("expected previous_hash 6ef8a6a8, got %s", s.PreviousHash)
	}
}

func TestRead_NoState(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "cat /deployments/myapp/state", Err: fmt.Errorf("no such file")},
	)

	s, err := Read(context.Background(), mock, "myapp")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if s != nil {
		t.Fatalf("expected nil state for missing file, got %+v", s)
	}
}

func TestWrite(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4")

	s := &AppState{
		CurrentPort:  49153,
		CurrentHash:  "b2c4d6e8",
		PreviousPort: 49152,
		PreviousHash: "6ef8a6a8",
	}

	if err := Write(context.Background(), mock, "myapp", s); err != nil {
		t.Fatalf("Write: %v", err)
	}

	data, ok := mock.Files["/deployments/myapp/state"]
	if !ok {
		t.Fatal("state file not uploaded")
	}

	content := string(data)
	if !strings.Contains(content, "current_port=49153") {
		t.Errorf("missing current_port in state: %s", content)
	}
	if !strings.Contains(content, "current_hash=b2c4d6e8") {
		t.Errorf("missing current_hash in state: %s", content)
	}
	if !strings.Contains(content, "previous_port=49152") {
		t.Errorf("missing previous_port in state: %s", content)
	}
	if !strings.Contains(content, "previous_hash=6ef8a6a8") {
		t.Errorf("missing previous_hash in state: %s", content)
	}
}

func TestAcquireLock(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "mkdir /deployments/myapp/.lock", Output: ""},
	)

	if err := AcquireLock(context.Background(), mock, "myapp"); err != nil {
		t.Fatalf("AcquireLock: %v", err)
	}

	// Lock info should be uploaded.
	data, ok := mock.Files["/deployments/myapp/.lock/info"]
	if !ok {
		t.Fatal("lock info not uploaded")
	}

	var info map[string]string
	if err := json.Unmarshal(data, &info); err != nil {
		t.Fatalf("parsing lock info: %v", err)
	}
	if info["type"] != "auto" {
		t.Errorf("expected lock type auto, got %s", info["type"])
	}
	if info["ts"] == "" {
		t.Error("expected non-empty timestamp")
	}
}

func TestAcquireLock_AlreadyLocked(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "mkdir /deployments/myapp/.lock", Err: fmt.Errorf("mkdir: cannot create directory: File exists")},
		ssh.MockCommand{Match: "cat /deployments/myapp/.lock/info", Output: `{"type":"auto","ts":"2026-03-11T00:00:00Z"}`},
	)

	err := AcquireLock(context.Background(), mock, "myapp")
	if err == nil {
		t.Fatal("expected error when lock exists")
	}
	if !strings.Contains(err.Error(), "already in progress") {
		t.Errorf("expected 'already in progress' error, got: %v", err)
	}
}

func TestAcquireLock_ManualLockBlocks(t *testing.T) {
	lockInfo := `{"type":"manual","user":"tyler","message":"running migration","ts":"2026-03-06T14:32:00Z"}`
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "mkdir /deployments/myapp/.lock", Err: fmt.Errorf("exists")},
		ssh.MockCommand{Match: "cat /deployments/myapp/.lock/info", Output: lockInfo},
	)

	err := AcquireLock(context.Background(), mock, "myapp")
	if err == nil {
		t.Fatal("expected error when manual lock exists")
	}
	if !strings.Contains(err.Error(), "Deploy locked by tyler") {
		t.Errorf("expected descriptive lock message, got: %v", err)
	}
	if !strings.Contains(err.Error(), "running migration") {
		t.Errorf("expected lock message in error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "teploy unlock") {
		t.Errorf("expected unlock hint in error, got: %v", err)
	}
}

func TestAcquireManualLock(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "mkdir /deployments/myapp/.lock", Output: ""},
	)

	err := AcquireManualLock(context.Background(), mock, "myapp", "tyler", "deploying hotfix")
	if err != nil {
		t.Fatalf("AcquireManualLock: %v", err)
	}

	data, ok := mock.Files["/deployments/myapp/.lock/info"]
	if !ok {
		t.Fatal("lock info not uploaded")
	}

	var info LockInfo
	if err := json.Unmarshal(data, &info); err != nil {
		t.Fatalf("parsing lock info: %v", err)
	}
	if info.Type != "manual" {
		t.Errorf("expected type manual, got %s", info.Type)
	}
	if info.User != "tyler" {
		t.Errorf("expected user tyler, got %s", info.User)
	}
	if info.Message != "deploying hotfix" {
		t.Errorf("expected message 'deploying hotfix', got %s", info.Message)
	}
}

func TestAcquireManualLock_AlreadyLocked(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "mkdir /deployments/myapp/.lock", Err: fmt.Errorf("exists")},
	)

	err := AcquireManualLock(context.Background(), mock, "myapp", "tyler", "test")
	if err == nil {
		t.Fatal("expected error when already locked")
	}
	if !strings.Contains(err.Error(), "already locked") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestReadLock(t *testing.T) {
	lockInfo := `{"type":"manual","user":"tyler","message":"maintenance","ts":"2026-03-11T12:00:00Z"}`
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "cat /deployments/myapp/.lock/info", Output: lockInfo},
	)

	info, err := ReadLock(context.Background(), mock, "myapp")
	if err != nil {
		t.Fatalf("ReadLock: %v", err)
	}
	if info == nil {
		t.Fatal("expected non-nil lock info")
	}
	if info.Type != "manual" {
		t.Errorf("expected type manual, got %s", info.Type)
	}
	if info.User != "tyler" {
		t.Errorf("expected user tyler, got %s", info.User)
	}
}

func TestReadLock_NoLock(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "cat /deployments/myapp/.lock/info", Err: fmt.Errorf("no such file")},
	)

	info, err := ReadLock(context.Background(), mock, "myapp")
	if err != nil {
		t.Fatalf("ReadLock: %v", err)
	}
	if info != nil {
		t.Fatalf("expected nil for no lock, got %+v", info)
	}
}

func TestReleaseLock(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "rm -rf /deployments/myapp/.lock", Output: ""},
	)

	ReleaseLock(context.Background(), mock, "myapp")

	found := false
	for _, call := range mock.Calls {
		if strings.Contains(call, "rm -rf /deployments/myapp/.lock") {
			found = true
		}
	}
	if !found {
		t.Error("expected rm -rf command for lock release")
	}
}

func TestAppendLog(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "printf %s", Output: ""},
	)

	entry := LogEntry{
		App:        "myapp",
		Type:       "deploy",
		Hash:       "abc123",
		Success:    true,
		DurationMs: 5000,
	}

	if err := AppendLog(context.Background(), mock, entry); err != nil {
		t.Fatalf("AppendLog: %v", err)
	}

	// The entry is base64-encoded in a single append command (no staging file).
	// Decode it back out of the recorded call.
	var data []byte
	for _, call := range mock.Calls {
		if strings.HasPrefix(call, "printf %s ") && strings.Contains(call, "teploy.log") {
			b64 := strings.TrimSpace(strings.TrimPrefix(strings.SplitN(call, "|", 2)[0], "printf %s "))
			b64 = strings.Trim(b64, "'")
			data, _ = base64.StdEncoding.DecodeString(b64)
		}
	}
	if len(data) == 0 {
		t.Fatal("log entry not written")
	}

	var parsed LogEntry
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("parsing log entry: %v", err)
	}
	if parsed.App != "myapp" {
		t.Errorf("expected app myapp, got %s", parsed.App)
	}
	if parsed.Type != "deploy" {
		t.Errorf("expected type deploy, got %s", parsed.Type)
	}
	if !parsed.Success {
		t.Error("expected success=true")
	}

	// Verify the append command was issued.
	found := false
	for _, call := range mock.Calls {
		if strings.Contains(call, ">> /deployments/teploy.log") {
			found = true
		}
	}
	if !found {
		t.Error("expected log append command")
	}
}

func TestEnsureAppDir(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "mkdir -p /deployments/myapp", Output: ""},
	)

	if err := EnsureAppDir(context.Background(), mock, "myapp"); err != nil {
		t.Fatalf("EnsureAppDir: %v", err)
	}

	if len(mock.Calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(mock.Calls))
	}
	if !strings.Contains(mock.Calls[0], "mkdir -p /deployments/myapp") {
		t.Errorf("expected mkdir -p command, got: %s", mock.Calls[0])
	}
}
