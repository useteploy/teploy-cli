package releasemeta

import (
	"context"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/ssh"
)

func TestNewAttempt_IDGrammarAndUniqueness(t *testing.T) {
	a1, err := NewAttempt("myapp", "abc123")
	if err != nil {
		t.Fatalf("NewAttempt: %v", err)
	}
	a2, err := NewAttempt("myapp", "abc123")
	if err != nil {
		t.Fatalf("NewAttempt: %v", err)
	}
	if a1.ID == a2.ID {
		t.Error("two attempts of the same release must have distinct ids")
	}
	if got, want := a1.Name(), "abc123."+a1.ID; got != want {
		t.Errorf("Name: got %s want %s", got, want)
	}
	if _, err := NewAttempt("myapp", "../escape"); err == nil {
		t.Error("an invalid release id must be rejected")
	}
}

func TestAttemptPaths_AreAttemptScoped(t *testing.T) {
	a := MustAttempt("myapp", "abc123")
	if !strings.HasPrefix(a.BuildDir(), "/deployments/myapp/meta/att/abc123.") {
		t.Errorf("BuildDir not in the attempt namespace: %s", a.BuildDir())
	}
	if !strings.HasPrefix(a.EnvFile(), "/deployments/myapp/meta/att/abc123.") {
		t.Errorf("EnvFile not in the attempt namespace: %s", a.EnvFile())
	}
	// A second attempt of the same release shares NO path with the first.
	b := MustAttempt("myapp", "abc123")
	if a.BuildDir() == b.BuildDir() || a.EnvFile() == b.EnvFile() || a.TLSDir() == b.TLSDir() {
		t.Error("attempts of the same release must not share artifact paths")
	}
	// The TLS namespace is app-scoped (A01): the host dir sits under the
	// app's own root beneath the caddy mount.
	if !strings.HasPrefix(a.TLSDir(), "/deployments/caddy/tls/att/myapp/abc123.") {
		t.Errorf("TLSDir not in the app-scoped caddy tls att namespace: %s", a.TLSDir())
	}
	if want := "/etc/caddy/tls/att/myapp/" + a.Name() + "/myapp.crt"; a.TLSCertPath() != want {
		t.Errorf("TLSCertPath: got %s want %s", a.TLSCertPath(), want)
	}
	if want := "/etc/caddy/tls/att/myapp/" + a.Name() + "/myapp.key"; a.TLSKeyPath() != want {
		t.Errorf("TLSKeyPath: got %s want %s", a.TLSKeyPath(), want)
	}
}

func TestNewAttempt_RejectsInvalidAppName(t *testing.T) {
	if _, err := NewAttempt("../escape", "abc123"); err == nil {
		t.Error("an app name with path metacharacters must be rejected")
	}
	if _, err := NewAttempt("", "abc123"); err == nil {
		t.Error("an empty app name must be rejected")
	}
}

func TestPruneAttempts_ProtectsKeepSetAndUnparsable(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		// Artifact root listing: a protected current attempt, a protected
		// previous attempt, an unprotected old one, and an unparsable name
		// that must be KEPT (fail closed, F78 parity).
		ssh.MockCommand{Match: "ls -1 /deployments/myapp/meta/att", Output: strings.Join([]string{
			"newhash.0000000000000001",
			"oldhash.0000000000000002",
			"ancient.0000000000000003",
			"stray-directory",
		}, "\n")},
		// The app's OWN TLS root (A01: /deployments/caddy/tls/att/<app>).
		ssh.MockCommand{Match: "ls -1 /deployments/caddy/tls/att/myapp", Output: strings.Join([]string{
			"ancient.0000000000000003",
		}, "\n")},
		ssh.MockCommand{Match: "rm -rf", Output: ""},
	)
	if err := PruneAttempts(context.Background(), mock, "myapp", "newhash", "oldhash"); err != nil {
		t.Fatalf("PruneAttempts: %v", err)
	}
	var removed []string
	for _, c := range mock.Calls {
		if strings.HasPrefix(c, "rm -rf ") {
			removed = append(removed, c)
		}
	}
	if len(removed) != 2 {
		t.Fatalf("expected exactly the ancient attempt's two dirs removed, got %v", removed)
	}
	for _, r := range removed {
		if !strings.Contains(r, "ancient.") {
			t.Errorf("removed a protected or unparsable entry: %s", r)
		}
	}
}

// TestPruneAttempts_NeverTouchesOtherAppsOrLegacyFlatRoot is the A01
// regression: pruning app A must not remove app B's TLS attempts (even when
// B's release hash collides with an unprotected hash of A's) and must never
// sweep the legacy flat /deployments/caddy/tls/att root, whose entries
// cannot be attributed to an owning app.
func TestPruneAttempts_NeverTouchesOtherAppsOrLegacyFlatRoot(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "ls -1 /deployments/app-a/meta/att", Output: strings.Join([]string{
			"shared-name.0000000000000001", // A's prunable attempt...
		}, "\n")},
		ssh.MockCommand{Match: "ls -1 /deployments/caddy/tls/att/app-a", Output: strings.Join([]string{
			"shared-name.0000000000000001", // ...in both of A's roots
		}, "\n")},
		ssh.MockCommand{Match: "rm -rf", Output: ""},
	)
	if err := PruneAttempts(context.Background(), mock, "app-a", "currenthash"); err != nil {
		t.Fatalf("PruneAttempts: %v", err)
	}
	for _, c := range mock.Calls {
		if !strings.HasPrefix(c, "rm -rf ") {
			continue
		}
		if strings.Contains(c, "/deployments/caddy/tls/att/app-b") {
			t.Errorf("pruned another app's TLS material: %s", c)
		}
		if !strings.Contains(c, "app-a") {
			t.Errorf("pruned outside the pruning app's namespaces: %s", c)
		}
	}
	// The sweep must be scoped to app-a's TLS root — a listing of the flat
	// root or app-b's root proves the sweep reached beyond A's namespace.
	for _, c := range mock.Calls {
		if strings.HasPrefix(c, "ls -1 ") {
			if c != "ls -1 /deployments/app-a/meta/att 2>/dev/null || true" &&
				c != "ls -1 /deployments/caddy/tls/att/app-a 2>/dev/null || true" {
				t.Errorf("attempt sweep listed a namespace it must not touch: %s", c)
			}
		}
	}
}

func TestPruneAttempts_AbsentRootsAreNoops(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "ls -1", Output: ""},
	)
	if err := PruneAttempts(context.Background(), mock, "myapp", "newhash"); err != nil {
		t.Fatalf("PruneAttempts on a fresh install: %v", err)
	}
	for _, c := range mock.Calls {
		if strings.HasPrefix(c, "rm -rf") {
			t.Errorf("nothing should be removed, saw %s", c)
		}
	}
}

func TestPreviousAttemptBuildDir(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "ls -1 /deployments/myapp/meta/att", Output: strings.Join([]string{
			"aaa.0000000000000001",
			"bbb.0000000000000002",
			"not-an-attempt",
		}, "\n")},
	)
	got := PreviousAttemptBuildDir(context.Background(), mock, "myapp", "0000000000000002")
	if want := "/deployments/myapp/meta/att/aaa.0000000000000001/build"; got != want {
		t.Errorf("PreviousAttemptBuildDir: got %s want %s", got, want)
	}
	// Excluding the other parseable attempt selects the remaining one;
	// unparsable names never become a basis.
	if got := PreviousAttemptBuildDir(context.Background(), mock, "myapp", "0000000000000001"); got != "/deployments/myapp/meta/att/bbb.0000000000000002/build" {
		t.Errorf("unexpected basis when 0001 is excluded: %s", got)
	}
}
