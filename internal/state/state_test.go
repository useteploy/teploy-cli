package state

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

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
	if s.SchemaVersion != 1 {
		t.Errorf("expected imported legacy schema 1, got %d", s.SchemaVersion)
	}
}

func TestRead_CanonicalV2TakesPrecedenceOverLegacy(t *testing.T) {
	v2 := `{"schema_version":2,"deployment_type":"container","ingress_mode":"external","updated_at":"2026-07-22T10:00:00Z","operation_id":"op-2","generation":4,"current_hash":"v2"}`
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "cat -- /deployments/myapp/state.json", Output: v2},
		ssh.MockCommand{Match: "cat /deployments/myapp/state", Output: "current_hash=stale\n"},
	)

	s, err := Read(context.Background(), mock, "myapp")
	if err != nil {
		t.Fatal(err)
	}
	if s.CurrentHash != "v2" || s.Generation != 4 || s.IngressMode != "external" {
		t.Fatalf("canonical state was not authoritative: %+v", s)
	}
	for _, call := range mock.Calls {
		if strings.HasPrefix(call, "cat /deployments/myapp/state 2>") {
			t.Fatalf("legacy state was read despite canonical state: %v", mock.Calls)
		}
	}
}

func TestRead_MalformedCanonicalDoesNotFallBack(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "cat -- /deployments/myapp/state.json", Output: "{not-json"},
		ssh.MockCommand{Match: "cat /deployments/myapp/state", Output: "current_hash=stale\n"},
	)

	if _, err := Read(context.Background(), mock, "myapp"); err == nil {
		t.Fatal("expected malformed canonical state error")
	}
	if len(mock.Calls) != 1 {
		t.Fatalf("malformed canonical state fell back to legacy: %v", mock.Calls)
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

	data, ok := mock.Files["/deployments/myapp/state.json"]
	if !ok {
		t.Fatal("state file not uploaded")
	}

	var written AppState
	if err := json.Unmarshal(data, &written); err != nil {
		t.Fatalf("invalid state JSON: %v", err)
	}
	if written.SchemaVersion != SchemaVersionV2 || written.CurrentPort != 49153 || written.CurrentHash != "b2c4d6e8" || written.PreviousPort != 49152 || written.PreviousHash != "6ef8a6a8" {
		t.Fatalf("unexpected state: %+v", written)
	}
}

func TestWrite_LegacyImportMigratesToCanonicalAuthority(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "cat /deployments/myapp/state", Output: "current_hash=old\ndomain=old.example.com\n"},
	)
	mock.Files["/deployments/myapp/state"] = []byte("current_hash=old\ndomain=old.example.com\n")
	legacy, err := Read(context.Background(), mock, "myapp")
	if err != nil {
		t.Fatal(err)
	}
	next := NewAppliedState(legacy, "container", "caddy", "new.example.com")
	next.CurrentHash = "new"
	if err := Write(context.Background(), mock, "myapp", next); err != nil {
		t.Fatal(err)
	}

	if got := string(mock.Files["/deployments/myapp/state"]); got != "current_hash=old\ndomain=old.example.com\n" {
		t.Fatalf("legacy authority was modified: %q", got)
	}
	var written AppState
	if err := json.Unmarshal(mock.Files["/deployments/myapp/state.json"], &written); err != nil {
		t.Fatal(err)
	}
	if written.Migration == nil || written.Migration.Source != "legacy-key-value" || written.Generation != 1 {
		t.Fatalf("migration was not recorded explicitly: %+v", written)
	}
}

func TestWrite_UploadFailurePreservesExistingStateAndCleansTemp(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "UPLOAD:/deployments/myapp/state.json.tmp-", Err: fmt.Errorf("upload failed")},
	)
	mock.Files["/deployments/myapp/state.json"] = []byte(`{"schema_version":2,"current_hash":"old"}`)

	err := Write(context.Background(), mock, "myapp", &AppState{CurrentHash: "new"})
	if err == nil || !strings.Contains(err.Error(), "uploading temporary file") {
		t.Fatalf("expected temporary upload error, got %v", err)
	}
	if got := string(mock.Files["/deployments/myapp/state.json"]); got != `{"schema_version":2,"current_hash":"old"}` {
		t.Fatalf("existing state changed after failed upload: %q", got)
	}
	assertNoStateTempFiles(t, mock)
}

func TestWrite_RenameFailurePreservesExistingStateAndCleansTemp(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "mv -f -- '/deployments/myapp/state.json.tmp-", Err: fmt.Errorf("rename failed")},
	)
	mock.Files["/deployments/myapp/state.json"] = []byte(`{"schema_version":2,"current_hash":"old"}`)

	err := Write(context.Background(), mock, "myapp", &AppState{CurrentHash: "new"})
	if err == nil || !strings.Contains(err.Error(), "renaming temporary file") {
		t.Fatalf("expected atomic rename error, got %v", err)
	}
	if got := string(mock.Files["/deployments/myapp/state.json"]); got != `{"schema_version":2,"current_hash":"old"}` {
		t.Fatalf("existing state changed after failed rename: %q", got)
	}
	assertNoStateTempFiles(t, mock)
}

func assertNoStateTempFiles(t *testing.T, mock *ssh.MockExecutor) {
	t.Helper()
	for path := range mock.Files {
		if strings.Contains(path, "/state.json.tmp-") {
			t.Fatalf("temporary state file was not cleaned up: %s", path)
		}
	}
	for _, call := range mock.Calls {
		if strings.HasPrefix(call, "rm -f -- '/deployments/myapp/state.json.tmp-") {
			return
		}
	}
	t.Fatal("temporary state cleanup was not attempted")
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
	// A recent (non-stale) timestamp — this test is specifically for the
	// "lock exists and is still fresh" case. See
	// TestAcquireLock_StaleAutoLockIsBroken for the staleness path.
	recentTS := time.Now().UTC().Format(time.RFC3339)
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "mkdir /deployments/myapp/.lock", Err: fmt.Errorf("mkdir: cannot create directory: File exists")},
		ssh.MockCommand{Match: "cat /deployments/myapp/.lock/info", Output: fmt.Sprintf(`{"type":"auto","ts":%q}`, recentTS)},
	)

	err := AcquireLock(context.Background(), mock, "myapp")
	if err == nil {
		t.Fatal("expected error when lock exists")
	}
	if !strings.Contains(err.Error(), "already in progress") {
		t.Errorf("expected 'already in progress' error, got: %v", err)
	}
	// Must NOT have attempted to break a fresh lock.
	for _, c := range mock.Calls {
		if strings.HasPrefix(c, "rm -rf") {
			t.Errorf("fresh lock should not be broken, but got rm call: %s", c)
		}
	}
}

// TestAcquireLock_StaleAutoLockIsBroken is the regression test for the
// stale-lock self-healing fix: a deploy that crashed or lost its SSH
// connection before its deferred ReleaseLockDetached ran would otherwise
// leave the app locked forever. An "auto" lock older than staleLockTTL
// should be broken and the new deploy allowed to proceed.
func TestAcquireLock_StaleAutoLockIsBroken(t *testing.T) {
	staleTS := time.Now().UTC().Add(-2 * staleLockTTL).Format(time.RFC3339)
	mock := ssh.NewMockExecutor("1.2.3.4",
		// First mkdir call fails (lock exists) — Once so it's consumed and
		// the second (persistent) entry below matches the retry after the
		// stale lock is broken.
		ssh.MockCommand{Match: "mkdir /deployments/myapp/.lock", Err: fmt.Errorf("exists"), Once: true},
		ssh.MockCommand{Match: "mkdir /deployments/myapp/.lock", Output: ""},
		ssh.MockCommand{Match: "cat /deployments/myapp/.lock/info", Output: fmt.Sprintf(`{"type":"auto","ts":%q}`, staleTS)},
	)

	if err := AcquireLock(context.Background(), mock, "myapp"); err != nil {
		t.Fatalf("AcquireLock: %v", err)
	}

	var sawBreak, sawRetryMkdir bool
	mkdirCount := 0
	for _, c := range mock.Calls {
		if strings.HasPrefix(c, "rm -rf /deployments/myapp/.lock") {
			sawBreak = true
		}
		if strings.HasPrefix(c, "mkdir /deployments/myapp/.lock") {
			mkdirCount++
		}
	}
	sawRetryMkdir = mkdirCount >= 2
	if !sawBreak {
		t.Error("expected the stale lock to be broken (rm -rf)")
	}
	if !sawRetryMkdir {
		t.Errorf("expected mkdir to be retried after breaking the stale lock, calls: %v", mock.Calls)
	}
}

// TestAcquireLock_StaleManualLockNeverBreaks confirms manual locks
// (an explicit, intentional freeze via `teploy lock`) are never
// auto-broken regardless of age — only "auto" locks self-heal.
func TestAcquireLock_StaleManualLockNeverBreaks(t *testing.T) {
	veryOldTS := time.Now().UTC().Add(-10 * staleLockTTL).Format(time.RFC3339)
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "mkdir /deployments/myapp/.lock", Err: fmt.Errorf("exists")},
		ssh.MockCommand{Match: "cat /deployments/myapp/.lock/info",
			Output: fmt.Sprintf(`{"type":"manual","user":"tyler","message":"freeze","ts":%q}`, veryOldTS)},
	)

	err := AcquireLock(context.Background(), mock, "myapp")
	if err == nil {
		t.Fatal("expected error — manual lock must never be auto-broken")
	}
	if !strings.Contains(err.Error(), "Deploy locked by tyler") {
		t.Errorf("expected manual-lock error, got: %v", err)
	}
	for _, c := range mock.Calls {
		if strings.HasPrefix(c, "rm -rf") {
			t.Errorf("manual lock must never be broken, but got rm call: %s", c)
		}
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

// TestReleaseLockDetached_WorksWithoutACallerContext is the regression
// test for the Ctrl+C-leaves-a-stale-lock fix: unlike ReleaseLock,
// ReleaseLockDetached takes no context from the caller at all — it builds
// its own — so a deploy's cancelled context (from Ctrl+C or a dropped SSH
// connection) can never prevent this cleanup call from reaching the
// server. There is deliberately no way to pass in an already-cancelled
// context here; that's the whole point of the fix.
func TestReleaseLockDetached_WorksWithoutACallerContext(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "rm -rf /deployments/myapp/.lock", Output: ""},
	)

	ReleaseLockDetached(mock, "myapp")

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
