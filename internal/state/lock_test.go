package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy/internal/ssh"
)

// takeFencedLock acquires a fenced lock on a mock whose mkdir succeeds, and
// returns the handle plus the mock so tests can inspect/corrupt the info
// file the guard reads. Extra commands (e.g. the guarded effect's response)
// are registered up front.
func takeFencedLock(t *testing.T, app string, extra ...ssh.MockCommand) (*Lock, *ssh.MockExecutor) {
	t.Helper()
	cmds := append([]ssh.MockCommand{
		ssh.MockCommand{Match: "mkdir /deployments/" + app + "/.lock", Output: ""},
	}, extra...)
	mock := ssh.NewMockExecutor("1.2.3.4", cmds...)
	lk, err := AcquireLockFenced(context.Background(), mock, app)
	if err != nil {
		t.Fatalf("AcquireLockFenced: %v", err)
	}
	if _, ok := mock.Files["/deployments/"+app+"/.lock/info"]; !ok {
		t.Fatal("lock info not uploaded")
	}
	return lk, mock
}

func TestAcquireLockFenced_IssuesOwnerToken(t *testing.T) {
	lk, mock := takeFencedLock(t, "myapp")
	if lk.Owner() == "" {
		t.Fatal("expected a non-empty owner token")
	}
	var info LockInfo
	if err := json.Unmarshal(mock.Files["/deployments/myapp/.lock/info"], &info); err != nil {
		t.Fatalf("parsing lock info: %v", err)
	}
	if info.Owner != lk.Owner() {
		t.Errorf("lock info owner %q != handle owner %q", info.Owner, lk.Owner())
	}
	if info.Type != "auto" {
		t.Errorf("expected auto lock, got %q", info.Type)
	}
}

func TestLock_Check_Held(t *testing.T) {
	lk, mock := takeFencedLock(t, "myapp")
	if err := lk.Check(context.Background(), mock); err != nil {
		t.Fatalf("Check while held: %v", err)
	}
}

func TestLock_Check_LostWhenServerNamesSomeoneElse(t *testing.T) {
	lk, mock := takeFencedLock(t, "myapp")
	// Simulate the lock being broken and re-acquired by another operation:
	// the info file names a different owner.
	mock.Files["/deployments/myapp/.lock/info"] = []byte(`{"type":"auto","owner":"someoneelse","ts":"2026-01-01T00:00:00Z"}`)
	if err := lk.Check(context.Background(), mock); !errors.Is(err, ErrFenceLost) {
		t.Fatalf("expected ErrFenceLost, got %v", err)
	}
}

func TestLock_Check_LostWhenLockVanishes(t *testing.T) {
	lk, mock := takeFencedLock(t, "myapp")
	delete(mock.Files, "/deployments/myapp/.lock/info")
	if err := lk.Check(context.Background(), mock); !errors.Is(err, ErrFenceLost) {
		t.Fatalf("expected ErrFenceLost for a vanished lock, got %v", err)
	}
}

func TestLock_Guarded_RunsEffectWhenHeld(t *testing.T) {
	lk, mock := takeFencedLock(t, "myapp", ssh.MockCommand{Match: "docker run", Output: "abc123"})
	out, err := lk.Guarded(context.Background(), mock, "docker run --name x")
	if err != nil {
		t.Fatalf("Guarded: %v", err)
	}
	if out != "abc123" {
		t.Errorf("expected effect output abc123, got %q", out)
	}
}

func TestLock_Guarded_RefusesEffectWhenLost(t *testing.T) {
	lk, mock := takeFencedLock(t, "myapp")
	mock.Files["/deployments/myapp/.lock/info"] = []byte(`{"type":"auto","owner":"someoneelse"}`)
	_, err := lk.Guarded(context.Background(), mock, "docker run --name x")
	if !errors.Is(err, ErrFenceLost) {
		t.Fatalf("expected ErrFenceLost, got %v", err)
	}
	for _, c := range mock.Calls {
		if strings.HasPrefix(c, "docker run") {
			t.Errorf("refused effect must not execute, saw: %s", c)
		}
	}
}

func TestLock_Renew_FreshensRenewTS(t *testing.T) {
	lk, mock := takeFencedLock(t, "myapp")
	lk.StartRenewal(mock)
	defer lk.StopRenewal()
	if err := lk.renew(context.Background()); err != nil {
		t.Fatalf("renew: %v", err)
	}
	var info LockInfo
	if err := json.Unmarshal(mock.Files["/deployments/myapp/.lock/info"], &info); err != nil {
		t.Fatalf("parsing renewed info: %v", err)
	}
	if info.RenewTS == "" {
		t.Error("expected renew_ts to be written")
	}
	if info.Owner != lk.Owner() {
		t.Errorf("renewal changed the owner: %q != %q", info.Owner, lk.Owner())
	}
}

func TestLock_Renew_DetectsLossAndPoisonsHandle(t *testing.T) {
	lk, mock := takeFencedLock(t, "myapp")
	lk.StartRenewal(mock)
	defer lk.StopRenewal()
	mock.Files["/deployments/myapp/.lock/info"] = []byte(`{"type":"auto","owner":"someoneelse"}`)
	if err := lk.renew(context.Background()); !errors.Is(err, ErrFenceLost) {
		t.Fatalf("expected renewal to report ErrFenceLost, got %v", err)
	}
	// A renewal that no longer holds must not have clobbered the new
	// holder's info file.
	var info LockInfo
	if err := json.Unmarshal(mock.Files["/deployments/myapp/.lock/info"], &info); err != nil {
		t.Fatalf("parsing info: %v", err)
	}
	if info.Owner != "someoneelse" {
		t.Errorf("renewal overwrote the new holder's info (owner=%q)", info.Owner)
	}
}

// TestAcquireLock_StaleByRenewTS proves staleness is measured from the last
// renewal (F16): a lock acquired long ago but renewed just now must NOT be
// broken, while the same acquisition age without a renewal must be.
func TestAcquireLock_StaleByRenewTS(t *testing.T) {
	old := time.Now().UTC().Add(-2 * staleLockTTL).Format(time.RFC3339)
	fresh := time.Now().UTC().Format(time.RFC3339)

	t.Run("renewed lock is fresh", func(t *testing.T) {
		mock := ssh.NewMockExecutor("1.2.3.4",
			ssh.MockCommand{Match: "mkdir /deployments/myapp/.lock", Err: fmt.Errorf("exists"), Once: true},
			ssh.MockCommand{Match: "cat /deployments/myapp/.lock/info", Output: fmt.Sprintf(`{"type":"auto","owner":"o1","ts":%q,"renew_ts":%q}`, old, fresh)},
		)
		if err := AcquireLock(context.Background(), mock, "myapp"); err == nil || !strings.Contains(err.Error(), "already in progress") {
			t.Fatalf("expected 'already in progress' for a renewed lock, got %v", err)
		}
		for _, c := range mock.Calls {
			if strings.HasPrefix(c, "rm -rf") {
				t.Errorf("renewed lock must not be broken, saw %s", c)
			}
		}
	})

	t.Run("unrenewed lock with unparseable renew_ts is stale", func(t *testing.T) {
		// An unparseable renew_ts is treated as stale (isStale), so the
		// lock is broken and acquisition retried; the retry's mkdir
		// succeeds (first attempt fails Once).
		mock := ssh.NewMockExecutor("1.2.3.4",
			ssh.MockCommand{Match: "mkdir /deployments/myapp/.lock", Err: fmt.Errorf("exists"), Once: true},
			ssh.MockCommand{Match: "mkdir /deployments/myapp/.lock", Output: ""},
			ssh.MockCommand{Match: "cat /deployments/myapp/.lock/info", Output: fmt.Sprintf(`{"type":"auto","ts":%q,"renew_ts":"garbage"}`, old)},
		)
		if err := AcquireLock(context.Background(), mock, "myapp"); err != nil {
			t.Fatalf("AcquireLock after breaking the corrupt lock: %v", err)
		}
	})
}

func TestWriteFenced_CommitsUnderFence(t *testing.T) {
	lk, mock := takeFencedLock(t, "myapp")
	s := &AppState{SchemaVersion: SchemaVersionV2, CurrentHash: "abc", UpdatedAt: time.Now().UTC(), OperationID: "op", Generation: 1}
	if err := WriteFenced(context.Background(), mock, "myapp", s, lk); err != nil {
		t.Fatalf("WriteFenced: %v", err)
	}
	data, ok := mock.Files["/deployments/myapp/state.json"]
	if !ok {
		t.Fatal("state.json not committed")
	}
	if !strings.Contains(string(data), `"current_hash":"abc"`) {
		t.Errorf("unexpected state content: %s", data)
	}
}

func TestWriteFenced_RefusedWhenFenceLost(t *testing.T) {
	lk, mock := takeFencedLock(t, "myapp")
	mock.Files["/deployments/myapp/.lock/info"] = []byte(`{"type":"auto","owner":"someoneelse"}`)
	s := &AppState{SchemaVersion: SchemaVersionV2, CurrentHash: "abc", UpdatedAt: time.Now().UTC(), OperationID: "op", Generation: 1}
	err := WriteFenced(context.Background(), mock, "myapp", s, lk)
	if !errors.Is(err, ErrFenceLost) {
		t.Fatalf("expected ErrFenceLost, got %v", err)
	}
	if _, ok := mock.Files["/deployments/myapp/state.json"]; ok {
		t.Error("a refused fence must not commit state")
	}
}

func TestNilLockIsUnfencedPassthrough(t *testing.T) {
	var lk *Lock
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "echo hi", Output: "hi"},
	)
	if err := lk.Check(context.Background(), mock); err != nil {
		t.Fatalf("nil Check must be a no-op, got %v", err)
	}
	out, err := lk.Guarded(context.Background(), mock, "echo hi")
	if err != nil || out != "hi" {
		t.Fatalf("nil Guarded must run the effect plainly, got (%q, %v)", out, err)
	}
	lk.StartRenewal(mock)
	lk.StopRenewal()
}
