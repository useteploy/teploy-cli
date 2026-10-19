package confighome

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/useteploy/teploy/internal/ssh"
)

func localStore(t *testing.T) (string, ssh.Executor) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	root := filepath.Join(t.TempDir(), ".teploy")
	t.Setenv("TEPLOY_CONFIG_ROOT_TEST", root)
	// A server with no git identity configured anywhere.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	return root, ssh.NewLocalExecutor()
}

var alice = Author{Name: "dash:alice", Email: "dash@teploy"}

func TestWriteReadHistory(t *testing.T) {
	_, ex := localStore(t)
	ctx := context.Background()

	if exists, _, err := Head(ctx, ex, "blog"); err != nil || exists {
		t.Fatalf("fresh app: exists=%v err=%v", exists, err)
	}
	if _, _, err := Read(ctx, ex, "blog", ""); !errors.Is(err, ErrAbsent) {
		t.Fatalf("read before any write = %v, want ErrAbsent", err)
	}
	c1, err := Write(ctx, ex, "blog", []byte("app: blog\nimage: nginx:1\n"), "", alice, "create")
	if err != nil {
		t.Fatal(err)
	}
	c2, err := Write(ctx, ex, "blog", []byte("app: blog\nimage: nginx:2\n"), c1, alice, "bump")
	if err != nil {
		t.Fatal(err)
	}
	body, at, err := Read(ctx, ex, "blog", "")
	if err != nil || at != c2 || !bytes.Contains(body, []byte("nginx:2")) {
		t.Fatalf("head read = %q at %s (%v), want nginx:2 at %s", body, at, err, c2)
	}
	old, _, err := Read(ctx, ex, "blog", c1)
	if err != nil || !bytes.Contains(old, []byte("nginx:1")) {
		t.Fatalf("read at c1 = %q (%v)", old, err)
	}
	hist, err := History(ctx, ex, "blog", 10)
	if err != nil || len(hist) != 2 || hist[0].OID != c2 || hist[0].Author != "dash:alice" || hist[1].Message != "create" {
		t.Fatalf("history = %+v (%v)", hist, err)
	}
}

func TestConcurrentCASExactlyOneWins(t *testing.T) {
	_, ex := localStore(t)
	ctx := context.Background()
	base, err := Write(ctx, ex, "blog", []byte("app: blog\nimage: nginx:1\n"), "", alice, "create")
	if err != nil {
		t.Fatal(err)
	}
	const n = 6
	var wins, conflicts int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := Write(ctx, ex, "blog", []byte("app: blog\nimage: nginx:"+string(rune('a'+i))+"\n"), base, alice, "race")
			mu.Lock()
			defer mu.Unlock()
			var ce *ConflictError
			switch {
			case err == nil:
				wins++
			case errors.As(err, &ce):
				if ce.Current == "" || ce.Current == base {
					t.Errorf("conflict must name the new head, got %q", ce.Current)
				}
				conflicts++
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if wins != 1 || conflicts != n-1 {
		t.Fatalf("wins=%d conflicts=%d, want exactly one winner", wins, conflicts)
	}
}

func TestStaleLockFromKilledWriteIsCleared(t *testing.T) {
	root, ex := localStore(t)
	ctx := context.Background()
	c1, err := Write(ctx, ex, "blog", []byte("app: blog\n"), "", alice, "create")
	if err != nil {
		t.Fatal(err)
	}
	// What a kill -9 during update-ref leaves behind.
	repo := filepath.Join(root, "apps", "blog", "config.git")
	if err := os.WriteFile(filepath.Join(repo, "refs", "heads", "teploy.lock"), []byte(c1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Write(ctx, ex, "blog", []byte("app: blog\nport: 80\n"), c1, alice, "after crash"); err != nil {
		t.Fatalf("next write after a killed one must succeed immediately: %v", err)
	}
}

func TestPushRefusedAndLargeConfig(t *testing.T) {
	root, ex := localStore(t)
	ctx := context.Background()
	big := append([]byte("app: blog\n# "), bytes.Repeat([]byte("x"), MaxConfigBytes-20)...)
	big = append(big, '\n')
	if _, err := Write(ctx, ex, "blog", big, "", alice, "big"); err != nil {
		t.Fatalf("a 1 MiB config must be accepted: %v", err)
	}
	if _, err := Write(ctx, ex, "blog", append(big, bytes.Repeat([]byte("y"), 64)...), "", alice, "too big"); err == nil {
		t.Fatal("over the limit must be refused")
	}

	work := t.TempDir()
	repo := filepath.Join(root, "apps", "blog", "config.git")
	run := func(args ...string) error {
		cmd := exec.Command("git", args...)
		cmd.Dir = work
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=x", "GIT_AUTHOR_EMAIL=x@x", "GIT_COMMITTER_NAME=x", "GIT_COMMITTER_EMAIL=x@x")
		out, err := cmd.CombinedOutput()
		if err != nil {
			return errors.New(string(out))
		}
		return nil
	}
	if err := run("clone", "-q", "-b", "teploy", repo, "."); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(work, "teploy.yml"), []byte("app: hijack\n"), 0o644)
	run("commit", "-qam", "manual")
	if err := run("push", "-q", "origin", "teploy"); err == nil || !strings.Contains(err.Error(), "written only by the teploy CLI") {
		t.Fatalf("a manual push must be refused by the hook, got %v", err)
	}
}

func TestPurgeHistoryAndArchive(t *testing.T) {
	root, ex := localStore(t)
	ctx := context.Background()
	c1, _ := Write(ctx, ex, "blog", []byte("app: blog\nenv:\n  TOKEN: oops-literal\n"), "", alice, "leak")
	c2, err := Write(ctx, ex, "blog", []byte("app: blog\n"), c1, alice, "fix")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PurgeHistory(ctx, ex, "blog", c1, alice); err == nil {
		t.Fatal("purge against a stale head must conflict")
	}
	if _, err := PurgeHistory(ctx, ex, "blog", c2, alice); err != nil {
		t.Fatal(err)
	}
	hist, _ := History(ctx, ex, "blog", 10)
	if len(hist) != 1 {
		t.Fatalf("history after purge = %d commits, want 1", len(hist))
	}
	repo := filepath.Join(root, "apps", "blog", "config.git")
	out, _ := exec.Command("git", "--git-dir="+repo, "cat-file", "-e", c1).CombinedOutput()
	if cmd := exec.Command("git", "--git-dir="+repo, "cat-file", "-e", c1); cmd.Run() == nil {
		t.Fatalf("purged commit %s still readable (%s)", c1, out)
	}
	dst, err := Archive(ctx, ex, "blog")
	if err != nil || !strings.Contains(dst, "config.git.archived-") {
		t.Fatalf("archive = %q, %v", dst, err)
	}
	if exists, _, _ := Head(ctx, ex, "blog"); exists {
		t.Fatal("archived repo must read as absent (folder home)")
	}
}
