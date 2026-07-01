package cli

import (
	"context"
	"os"
	"os/exec"
	"testing"

	"github.com/useteploy/teploy/internal/ssh"
)

func TestEnsureServerBuildCheckout_NoOpIfAlreadyCloned(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "test -d", Output: "yes"},
	)

	if err := ensureServerBuildCheckout(context.Background(), mock, "myapp"); err != nil {
		t.Fatalf("ensureServerBuildCheckout: %v", err)
	}

	for _, c := range mock.Calls {
		if len(c) >= 10 && c[:10] == "git clone " {
			t.Errorf("should not clone when a checkout already exists, but got: %s", c)
		}
	}
}

// TestLocalGitRemoteURL uses a real temp git repo (git init + remote add)
// rather than a mock, since this specifically wraps the local `git`
// binary — the thing worth verifying is that the command/parsing is
// correct, not something a mock can meaningfully stand in for.
func TestLocalGitRemoteURL(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("remote", "add", "origin", "https://example.com/tyler/myapp.git")

	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldWD)

	url, err := localGitRemoteURL()
	if err != nil {
		t.Fatalf("localGitRemoteURL: %v", err)
	}
	if url != "https://example.com/tyler/myapp.git" {
		t.Errorf("got %q, want %q", url, "https://example.com/tyler/myapp.git")
	}
}

func TestLocalGitRemoteURL_NoRemote(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	dir := t.TempDir()
	cmd := exec.Command("git", "init", "-q")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}

	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldWD)

	if _, err := localGitRemoteURL(); err == nil {
		t.Error("expected error when there's no 'origin' remote")
	}
}
