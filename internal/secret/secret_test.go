package secret

import (
	"context"
	"fmt"
	"testing"

	"github.com/useteploy/teploy/internal/ssh"
)

func TestSet(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "which age", Output: "/usr/bin/age"},
		ssh.MockCommand{Match: "test -f /deployments/.age-key", Output: ""},
		ssh.MockCommand{Match: "grep 'public key:'", Output: "# public key: age1abc123"},
		ssh.MockCommand{Match: "mkdir -p /deployments/myapp/secrets", Output: ""},
		ssh.MockCommand{Match: "printf", Output: ""},
	)

	mgr := NewManager(mock)
	if err := mgr.Set(context.Background(), "myapp", "DB_PASS", "supersecret"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Verify age encrypt command was called.
	found := false
	for _, call := range mock.Calls {
		if len(call) >= 6 && call[:6] == "printf" {
			found = true
		}
	}
	if !found {
		t.Error("expected age encrypt command to be called")
	}
}

func TestGet(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "test -f /deployments/myapp/secrets/DB_PASS.age", Output: ""},
		ssh.MockCommand{Match: "age -d", Output: "supersecret\n"},
	)

	mgr := NewManager(mock)
	val, err := mgr.Get(context.Background(), "myapp", "DB_PASS")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if val != "supersecret" {
		t.Errorf("expected 'supersecret', got %q", val)
	}
}

func TestGet_NotSet(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "test -f", Err: fmt.Errorf("not found")},
	)

	mgr := NewManager(mock)
	_, err := mgr.Get(context.Background(), "myapp", "MISSING")
	if err == nil {
		t.Fatal("expected error for missing secret")
	}
}

func TestList(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "ls /deployments/myapp/secrets", Output: "/deployments/myapp/secrets/DB_PASS.age\n/deployments/myapp/secrets/SECRET_KEY.age\n"},
	)

	mgr := NewManager(mock)
	keys, err := mgr.List(context.Background(), "myapp")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("expected 2 keys, got %d", len(keys))
	}
	if keys[0] != "DB_PASS" || keys[1] != "SECRET_KEY" {
		t.Errorf("unexpected keys: %v", keys)
	}
}

func TestList_Empty(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "ls", Output: "", Err: fmt.Errorf("no matches")},
	)

	mgr := NewManager(mock)
	keys, err := mgr.List(context.Background(), "myapp")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(keys) != 0 {
		t.Errorf("expected empty list, got %v", keys)
	}
}

func TestRotate(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "test -f /deployments/myapp/secrets/DB_PASS.age", Output: ""},
		ssh.MockCommand{Match: "which age", Output: "/usr/bin/age"},
		ssh.MockCommand{Match: "test -f /deployments/.age-key", Output: ""},
		ssh.MockCommand{Match: "grep 'public key:'", Output: "# public key: age1abc123"},
		ssh.MockCommand{Match: "mkdir -p /deployments/myapp/secrets", Output: ""},
		ssh.MockCommand{Match: "printf", Output: ""},
	)

	mgr := NewManager(mock)
	newVal, err := mgr.Rotate(context.Background(), "myapp", "DB_PASS")
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if len(newVal) != 64 { // 32 bytes hex encoded
		t.Errorf("expected 64-char hex string, got %d chars: %s", len(newVal), newVal)
	}
}

func TestEnsureAge_AlreadyInstalled(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "which age", Output: "/usr/bin/age"},
	)

	mgr := NewManager(mock)
	if err := mgr.EnsureAge(context.Background()); err != nil {
		t.Fatalf("EnsureAge: %v", err)
	}

	if len(mock.Calls) != 1 {
		t.Errorf("expected 1 call (which age), got %d", len(mock.Calls))
	}
}
