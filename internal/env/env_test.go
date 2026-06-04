package env

import (
	"context"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/ssh"
)

func TestSet_NewFile(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "if test -f /deployments/myapp/.env", Output: ""},
		ssh.MockCommand{Match: "mkdir -p", Output: ""},
		ssh.MockCommand{Match: "chown", Output: ""},
	)

	mgr := NewManager(mock)
	err := mgr.Set(context.Background(), "myapp", map[string]string{
		"DB_HOST": "localhost",
		"DB_PORT": "5432",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify file was uploaded with correct permissions.
	data := string(mock.Files["/deployments/myapp/.env"])
	if !strings.Contains(data, "DB_HOST=localhost") {
		t.Errorf("expected DB_HOST=localhost in env file, got: %s", data)
	}
	if !strings.Contains(data, "DB_PORT=5432") {
		t.Errorf("expected DB_PORT=5432 in env file, got: %s", data)
	}

	// Verify secure permissions in upload call.
	found := false
	for _, call := range mock.Calls {
		if strings.Contains(call, "UPLOAD:") && strings.Contains(call, "mode 0600") {
			found = true
		}
	}
	if !found {
		t.Error("expected upload with mode 0600")
	}
}

func TestSet_UpdateExisting(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "if test -f /deployments/myapp/.env", Output: "DB_HOST=old\nAPI_KEY=secret\n"},
		ssh.MockCommand{Match: "mkdir -p", Output: ""},
		ssh.MockCommand{Match: "chown", Output: ""},
	)

	mgr := NewManager(mock)
	err := mgr.Set(context.Background(), "myapp", map[string]string{
		"DB_HOST": "new-host",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data := string(mock.Files["/deployments/myapp/.env"])
	if !strings.Contains(data, "DB_HOST=new-host") {
		t.Errorf("expected updated DB_HOST, got: %s", data)
	}
	if !strings.Contains(data, "API_KEY=secret") {
		t.Errorf("expected preserved API_KEY, got: %s", data)
	}
}

func TestSet_RejectsPort(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4")
	mgr := NewManager(mock)

	err := mgr.Set(context.Background(), "myapp", map[string]string{"PORT": "3000"})
	if err == nil {
		t.Fatal("expected error for PORT")
	}
	if !strings.Contains(err.Error(), "PORT is reserved") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestSet_RejectsPortCaseInsensitive(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4")
	mgr := NewManager(mock)

	err := mgr.Set(context.Background(), "myapp", map[string]string{"port": "3000"})
	if err == nil {
		t.Fatal("expected error for port (lowercase)")
	}
}

func TestGet(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "if test -f /deployments/myapp/.env", Output: "DB_HOST=localhost\nDB_PORT=5432\n"},
	)

	mgr := NewManager(mock)
	val, err := mgr.Get(context.Background(), "myapp", "DB_HOST")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if val != "localhost" {
		t.Errorf("expected localhost, got %s", val)
	}
}

func TestGet_NotSet(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "if test -f /deployments/myapp/.env", Output: "DB_HOST=localhost\n"},
	)

	mgr := NewManager(mock)
	_, err := mgr.Get(context.Background(), "myapp", "MISSING")
	if err == nil {
		t.Fatal("expected error for missing key")
	}
	if !strings.Contains(err.Error(), "not set") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestGet_EmptyFile(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "if test -f /deployments/myapp/.env", Output: ""},
	)

	mgr := NewManager(mock)
	_, err := mgr.Get(context.Background(), "myapp", "ANY")
	if err == nil {
		t.Fatal("expected error for empty env")
	}
}

func TestUnset(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "if test -f /deployments/myapp/.env", Output: "DB_HOST=localhost\nDB_PORT=5432\n"},
		ssh.MockCommand{Match: "mkdir -p", Output: ""},
		ssh.MockCommand{Match: "chown", Output: ""},
	)

	mgr := NewManager(mock)
	err := mgr.Unset(context.Background(), "myapp", "DB_HOST")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data := string(mock.Files["/deployments/myapp/.env"])
	if strings.Contains(data, "DB_HOST") {
		t.Errorf("expected DB_HOST removed, got: %s", data)
	}
	if !strings.Contains(data, "DB_PORT=5432") {
		t.Errorf("expected DB_PORT preserved, got: %s", data)
	}
}

func TestUnset_NotSet(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "if test -f /deployments/myapp/.env", Output: "DB_HOST=localhost\n"},
	)

	mgr := NewManager(mock)
	err := mgr.Unset(context.Background(), "myapp", "MISSING")
	if err == nil {
		t.Fatal("expected error for missing key")
	}
}

func TestList(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "if test -f /deployments/myapp/.env", Output: "Z_KEY=last\nA_KEY=first\nM_KEY=middle\n"},
	)

	mgr := NewManager(mock)
	entries, err := mgr.List(context.Background(), "myapp")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}

	// Verify sorted order.
	if entries[0].Key != "A_KEY" || entries[0].Value != "first" {
		t.Errorf("expected first entry A_KEY=first, got %s=%s", entries[0].Key, entries[0].Value)
	}
	if entries[1].Key != "M_KEY" || entries[1].Value != "middle" {
		t.Errorf("expected second entry M_KEY=middle, got %s=%s", entries[1].Key, entries[1].Value)
	}
	if entries[2].Key != "Z_KEY" || entries[2].Value != "last" {
		t.Errorf("expected third entry Z_KEY=last, got %s=%s", entries[2].Key, entries[2].Value)
	}
}

func TestList_Empty(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "if test -f /deployments/myapp/.env", Output: ""},
	)

	mgr := NewManager(mock)
	entries, err := mgr.List(context.Background(), "myapp")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected empty list, got %d entries", len(entries))
	}
}

func TestParseEnv_QuotedValues(t *testing.T) {
	content := `DB_URL="postgres://user:pass@host/db"
SECRET='my secret value'
PLAIN=simple
# comment
EMPTY=
`
	vars := parseEnv(content)

	tests := map[string]string{
		"DB_URL": "postgres://user:pass@host/db",
		"SECRET": "my secret value",
		"PLAIN":  "simple",
		"EMPTY":  "",
	}

	for k, want := range tests {
		if got := vars[k]; got != want {
			t.Errorf("%s: expected %q, got %q", k, want, got)
		}
	}

	if _, ok := vars["# comment"]; ok {
		t.Error("comment should not be parsed as a key")
	}
}

func TestSerializeEnv_Verbatim(t *testing.T) {
	// Values are written exactly as given — no quoting — because the file is
	// consumed by `docker run --env-file`, which reads everything after '='
	// literally. Quoting here would corrupt the value the container receives.
	vars := map[string]string{
		"SIMPLE":   "value",
		"SPACED":   "hello world",
		"DOLLAR":   "price is $5",
		"SAFE_URL": "postgres://user:pass@host/db",
		"QUOTED":   `say "hi"`,
	}
	output := serializeEnv(vars)

	for k, v := range vars {
		want := k + "=" + v + "\n"
		if !strings.Contains(output, want) {
			t.Errorf("expected verbatim line %q, got:\n%s", want, output)
		}
	}
	if strings.Contains(output, `SPACED="hello world"`) {
		t.Errorf("spaced value must NOT be quoted, got:\n%s", output)
	}
}

func TestSerializeEnv_SortedKeys(t *testing.T) {
	vars := map[string]string{
		"Z": "last",
		"A": "first",
		"M": "middle",
	}
	output := serializeEnv(vars)
	lines := strings.Split(strings.TrimSpace(output), "\n")

	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d", len(lines))
	}
	if !strings.HasPrefix(lines[0], "A=") {
		t.Errorf("expected first key A, got: %s", lines[0])
	}
	if !strings.HasPrefix(lines[1], "M=") {
		t.Errorf("expected second key M, got: %s", lines[1])
	}
	if !strings.HasPrefix(lines[2], "Z=") {
		t.Errorf("expected third key Z, got: %s", lines[2])
	}
}
