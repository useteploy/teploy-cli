package secret

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/ssh"
)

func TestSet(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "which age", Output: "/usr/bin/age"},
		ssh.MockCommand{Match: "test -f /deployments/.age-key", Output: ""},
		ssh.MockCommand{Match: "grep 'public key:'", Output: "# public key: age1abc123"},
		ssh.MockCommand{Match: "mkdir -p", Output: ""},
		ssh.MockCommand{Match: "umask 077", Output: ""},
	)

	mgr := NewManager(mock)
	if err := mgr.Set(context.Background(), "myapp", "DB_PASS", "supersecret"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// The plaintext must ride stdin (RunInput), never the command string —
	// the remote process list would otherwise show it (audit F22).
	for _, call := range mock.Calls {
		if strings.Contains(call, "supersecret") {
			t.Errorf("secret value leaked into command string: %q", call)
		}
	}
	found := false
	for _, call := range mock.Calls {
		if strings.HasPrefix(call, "umask 077") {
			found = true
		}
	}
	if !found {
		t.Error("expected the streaming age-encrypt command to be called")
	}
}

func TestSet_RejectsUnsafeKeys(t *testing.T) {
	mgr := NewManager(ssh.NewMockExecutor("1.2.3.4"))
	for _, key := range []string{"", "../escape", "a/b", ".hidden", "-flag", "has space", "new\nline"} {
		if err := mgr.Set(context.Background(), "myapp", key, "v"); err == nil {
			t.Errorf("Set accepted unsafe key %q", key)
		}
	}
}

func TestGet(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "test -f '/deployments/myapp/secrets/DB_PASS.age'", Output: ""},
		ssh.MockCommand{Match: "age -d", Output: "supersecret"},
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

// audit F23: Get must return decrypted bytes EXACTLY as stored. The old
// implementation trimmed the value, silently corrupting passwords that
// intentionally carry leading/trailing whitespace.
func TestGet_PreservesExactBytes(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "test -f", Output: ""},
		ssh.MockCommand{Match: "age -d", Output: "  padded password  \n"},
	)
	mgr := NewManager(mock)
	val, err := mgr.Get(context.Background(), "myapp", "PADDED")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if val != "  padded password  \n" {
		t.Errorf("value altered in transit: %q", val)
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
		ssh.MockCommand{Match: "test -d '/deployments/myapp/secrets'", Output: ""},
		ssh.MockCommand{Match: "find", Output: "DB_PASS.age\nSECRET_KEY.age\n"},
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
		ssh.MockCommand{Match: "test -d", Err: fmt.Errorf("exit status 1")},
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

// audit F23: a secrets directory that exists but cannot be listed must be an
// error, never an empty list — a transport failure used to make deployments
// proceed as though every secret was simply absent.
func TestList_ReadFailureIsError(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "test -d", Output: ""},
		ssh.MockCommand{Match: "find", Err: fmt.Errorf("permission denied")},
	)
	mgr := NewManager(mock)
	if _, err := mgr.List(context.Background(), "myapp"); err == nil {
		t.Fatal("expected an error when the secrets directory cannot be listed")
	}
}

func TestRotate(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "test -f '/deployments/myapp/secrets/DB_PASS.age'", Output: ""},
		ssh.MockCommand{Match: "which age", Output: "/usr/bin/age"},
		ssh.MockCommand{Match: "test -f /deployments/.age-key", Output: ""},
		ssh.MockCommand{Match: "grep 'public key:'", Output: "# public key: age1abc123"},
		ssh.MockCommand{Match: "mkdir -p", Output: ""},
		ssh.MockCommand{Match: "umask 077", Output: ""},
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

func TestRemove(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "test -f '/deployments/myapp/secrets/DB_PASS.age'", Output: ""},
		ssh.MockCommand{Match: "rm -f '/deployments/myapp/secrets/DB_PASS.age'", Output: ""},
	)

	mgr := NewManager(mock)
	removed, err := mgr.Remove(context.Background(), "myapp", "DB_PASS")
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if !removed {
		t.Error("Remove reported nothing removed for an existing secret")
	}

	found := false
	for _, call := range mock.Calls {
		if strings.HasPrefix(call, "rm -f ") {
			found = true
		}
	}
	if !found {
		t.Error("expected the secret file to be deleted")
	}
}

func TestRemove_NotSet(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "test -f '/deployments/myapp/secrets/GONE.age'", Err: fmt.Errorf("exit status 1")},
	)

	mgr := NewManager(mock)
	removed, err := mgr.Remove(context.Background(), "myapp", "GONE")
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if removed {
		t.Error("Remove reported a removal for a secret that was never set")
	}
	for _, call := range mock.Calls {
		if strings.HasPrefix(call, "rm -f ") {
			t.Errorf("unexpected delete for a missing secret: %q", call)
		}
	}
}

// audit F01 (P0): OpenBao control-plane secrets share the age store with
// application secrets. DecryptAll must never hand them to a workload.
func TestDecryptAll_ExcludesManagementSecrets(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "test -d", Output: ""},
		ssh.MockCommand{Match: "find", Output: "API_TOKEN.age\nVAULT_ROOT_TOKEN.age\nVAULT_RECOVERY_KEYS.age\nVAULT_SEAL_KEY.age\nVAULT_SEAL_KEY_ID.age\n"},
		ssh.MockCommand{Match: "test -f", Output: ""},
		ssh.MockCommand{Match: "age -d", Output: "value"},
	)

	mgr := NewManager(mock)
	got, err := mgr.DecryptAll(context.Background(), "myapp")
	if err != nil {
		t.Fatalf("DecryptAll: %v", err)
	}
	if len(got) != 1 || got["API_TOKEN"] != "value" {
		t.Fatalf("expected only API_TOKEN, got %v", got)
	}
}
