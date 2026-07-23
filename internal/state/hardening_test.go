package state

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/useteploy/teploy/internal/ssh"
)

func TestRead_MalformedPort(t *testing.T) {
	mock := ssh.NewMockExecutor("server1",
		ssh.MockCommand{Match: "cat /deployments/myapp/state", Output: "current_port=notanumber\ncurrent_hash=abc123\n"},
	)

	s, err := Read(context.Background(), mock, "myapp")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if s == nil {
		t.Fatal("expected non-nil state")
	}
	// Port should be 0 (zero value from failed Atoi).
	if s.CurrentPort != 0 {
		t.Errorf("malformed port should result in 0, got %d", s.CurrentPort)
	}
	if s.CurrentHash != "abc123" {
		t.Errorf("hash should still be parsed, got %q", s.CurrentHash)
	}
}

func TestRead_ExtraFields(t *testing.T) {
	mock := ssh.NewMockExecutor("server1",
		ssh.MockCommand{Match: "cat /deployments/myapp/state", Output: "current_port=49152\ncurrent_hash=abc123\nunknown_field=value\n"},
	)

	s, err := Read(context.Background(), mock, "myapp")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if s.CurrentPort != 49152 {
		t.Errorf("expected port 49152, got %d", s.CurrentPort)
	}
}

func TestRead_EmptyLines(t *testing.T) {
	mock := ssh.NewMockExecutor("server1",
		ssh.MockCommand{Match: "cat /deployments/myapp/state", Output: "\n\ncurrent_port=49152\n\ncurrent_hash=abc123\n\n"},
	)

	s, err := Read(context.Background(), mock, "myapp")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if s.CurrentPort != 49152 {
		t.Errorf("expected port 49152, got %d", s.CurrentPort)
	}
}

func TestRead_MalformedLines(t *testing.T) {
	mock := ssh.NewMockExecutor("server1",
		ssh.MockCommand{Match: "cat /deployments/myapp/state", Output: "garbage\nno-equals-sign\ncurrent_port=49152\n"},
	)

	s, err := Read(context.Background(), mock, "myapp")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if s.CurrentPort != 49152 {
		t.Errorf("should parse valid lines despite garbage, got %d", s.CurrentPort)
	}
}

func TestWrite_RoundTrip(t *testing.T) {
	mock := ssh.NewMockExecutor("server1",
		ssh.MockCommand{Match: "UPLOAD:", Output: ""},
	)

	s := &AppState{
		CurrentPort:  49152,
		CurrentHash:  "abc123",
		PreviousPort: 49153,
		PreviousHash: "def456",
	}

	err := Write(context.Background(), mock, "myapp", s)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Verify the uploaded content.
	content, ok := mock.Files["/deployments/myapp/state.json"]
	if !ok {
		t.Fatal("state file not uploaded")
	}

	var got AppState
	if err := json.Unmarshal(content, &got); err != nil {
		t.Fatal(err)
	}
	if got.CurrentPort != 49152 || got.CurrentHash != "abc123" || got.PreviousPort != 49153 || got.PreviousHash != "def456" {
		t.Errorf("state content mismatch: %+v", got)
	}
}

func TestReleaseLock_NoError(t *testing.T) {
	mock := ssh.NewMockExecutor("server1",
		ssh.MockCommand{Match: "rm -rf", Output: ""},
	)

	// ReleaseLock should not panic even if it can't remove the lock.
	ReleaseLock(context.Background(), mock, "myapp")

	if len(mock.Calls) != 1 {
		t.Errorf("expected 1 call, got %d", len(mock.Calls))
	}
}

func TestEnsureAppDir_Simple(t *testing.T) {
	mock := ssh.NewMockExecutor("server1",
		ssh.MockCommand{Match: "mkdir -p", Output: ""},
	)

	err := EnsureAppDir(context.Background(), mock, "myapp")
	if err != nil {
		t.Fatalf("EnsureAppDir: %v", err)
	}
}
