package state

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy/internal/ssh"
)

func recentTS() string { return time.Now().UTC().Format(time.RFC3339) }
func staleHealTS() string {
	return time.Now().Add(-5 * time.Minute).UTC().Format(time.RFC3339)
}

func sawReleaseCall(calls []string) bool {
	for _, c := range calls {
		if strings.HasPrefix(c, "rm -rf") {
			return true
		}
	}
	return false
}

func TestAcquireHealLock_NoLock(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "mkdir /deployments/myapp/.lock", Output: ""},
	)
	acquired, err := AcquireHealLock(context.Background(), mock, "myapp")
	if err != nil {
		t.Fatalf("AcquireHealLock: %v", err)
	}
	if !acquired {
		t.Fatal("expected acquired=true when no lock exists")
	}
}

// The load-bearing invariant: heal must NEVER break a deploy's lock.
func TestAcquireHealLock_YieldsToDeployLockNeverBreaks(t *testing.T) {
	for _, lockType := range []string{"auto", "manual"} {
		t.Run(lockType, func(t *testing.T) {
			mock := ssh.NewMockExecutor("1.2.3.4",
				ssh.MockCommand{Match: "mkdir /deployments/myapp/.lock", Err: fmt.Errorf("exists")},
				ssh.MockCommand{Match: "cat /deployments/myapp/.lock/info",
					Output: fmt.Sprintf(`{"type":%q,"ts":%q}`, lockType, recentTS())},
			)
			acquired, err := AcquireHealLock(context.Background(), mock, "myapp")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if acquired {
				t.Fatalf("heal must yield to a %s lock, not acquire", lockType)
			}
			if sawReleaseCall(mock.Calls) {
				t.Fatalf("heal must NEVER break a %s deploy lock (saw rm -rf)", lockType)
			}
		})
	}
}

// A fresh heal lock (another heal mid-restart) is also yielded to, not broken.
func TestAcquireHealLock_YieldsToFreshHealLock(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "mkdir /deployments/myapp/.lock", Err: fmt.Errorf("exists")},
		ssh.MockCommand{Match: "cat /deployments/myapp/.lock/info",
			Output: fmt.Sprintf(`{"type":"heal","ts":%q}`, recentTS())},
	)
	acquired, err := AcquireHealLock(context.Background(), mock, "myapp")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if acquired {
		t.Fatal("heal must yield to a fresh heal lock")
	}
	if sawReleaseCall(mock.Calls) {
		t.Fatal("heal must not break a fresh heal lock")
	}
}

// Heal may break its OWN stale lock so a crashed predecessor can't block it forever.
func TestAcquireHealLock_BreaksOwnStaleHealLock(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "mkdir /deployments/myapp/.lock", Err: fmt.Errorf("exists"), Once: true},
		ssh.MockCommand{Match: "mkdir /deployments/myapp/.lock", Output: ""},
		ssh.MockCommand{Match: "cat /deployments/myapp/.lock/info",
			Output: fmt.Sprintf(`{"type":"heal","ts":%q}`, staleHealTS())},
	)
	acquired, err := AcquireHealLock(context.Background(), mock, "myapp")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !acquired {
		t.Fatal("heal should break its own stale lock and acquire")
	}
	if !sawReleaseCall(mock.Calls) {
		t.Fatal("expected the stale heal lock to be released (rm -rf)")
	}
}

// A deploy (authoritative) may break a stale heal lock so it isn't blocked.
func TestAcquireLock_BreaksStaleHealLock(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "mkdir /deployments/myapp/.lock", Err: fmt.Errorf("exists"), Once: true},
		ssh.MockCommand{Match: "mkdir /deployments/myapp/.lock", Output: ""},
		ssh.MockCommand{Match: "cat /deployments/myapp/.lock/info",
			Output: fmt.Sprintf(`{"type":"heal","ts":%q}`, staleHealTS())},
	)
	if err := AcquireLock(context.Background(), mock, "myapp"); err != nil {
		t.Fatalf("deploy should break a stale heal lock: %v", err)
	}
}

// But a FRESH heal lock (heal mid-restart, seconds) makes the deploy yield.
func TestAcquireLock_FreshHealLockBlocksDeploy(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "mkdir /deployments/myapp/.lock", Err: fmt.Errorf("exists")},
		ssh.MockCommand{Match: "cat /deployments/myapp/.lock/info",
			Output: fmt.Sprintf(`{"type":"heal","ts":%q}`, recentTS())},
	)
	if err := AcquireLock(context.Background(), mock, "myapp"); err == nil {
		t.Fatal("deploy must not proceed while a fresh heal lock is held")
	}
}
