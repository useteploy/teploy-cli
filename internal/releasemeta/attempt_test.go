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
	if !strings.HasPrefix(a.TLSDir(), "/deployments/caddy/tls/att/abc123.") {
		t.Errorf("TLSDir not under the caddy tls att namespace: %s", a.TLSDir())
	}
	if want := "/etc/caddy/tls/att/" + a.Name() + "/myapp.crt"; a.TLSCertPath() != want {
		t.Errorf("TLSCertPath: got %s want %s", a.TLSCertPath(), want)
	}
	if want := "/etc/caddy/tls/att/" + a.Name() + "/myapp.key"; a.TLSKeyPath() != want {
		t.Errorf("TLSKeyPath: got %s want %s", a.TLSKeyPath(), want)
	}
	// A second attempt of the same release shares NO path with the first.
	b := MustAttempt("myapp", "abc123")
	if a.BuildDir() == b.BuildDir() || a.EnvFile() == b.EnvFile() || a.TLSDir() == b.TLSDir() {
		t.Error("attempts of the same release must not share artifact paths")
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
		ssh.MockCommand{Match: "ls -1 /deployments/caddy/tls/att", Output: strings.Join([]string{
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
