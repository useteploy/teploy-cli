package cli

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/useteploy/teploy/internal/confighome"
	"github.com/useteploy/teploy/internal/events"
	"github.com/useteploy/teploy/internal/ssh"
)

func localConfigTarget(t *testing.T) *configTarget {
	t.Helper()
	for _, bin := range []string{"git", "python3"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skip(bin + " not available")
		}
	}
	root := filepath.Join(t.TempDir(), ".teploy")
	t.Setenv("TEPLOY_CONFIG_ROOT_TEST", root)
	t.Setenv("TEPLOY_EVENTS_ROOT_TEST", root)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	events.SetProcessActor(events.Actor{Client: "cli", Label: "test@host"})
	return &configTarget{app: "blog", server: "prod", exec: ssh.NewLocalExecutor()}
}

const blogConfig = "app: blog\ndomain: blog.example.com\nimage: nginx:1.27\nenv:\n  LOG_LEVEL: info\n"

func TestCommitConfigValidatesAndRecordsEvent(t *testing.T) {
	tg := localConfigTarget(t)
	ctx := context.Background()
	if _, err := commitConfig(ctx, tg, nil, []byte(blogConfig+"  URL: https://${HOST}/\n"), confighome.ZeroCommit, "x"); err == nil || !strings.Contains(err.Error(), "${HOST}") {
		t.Fatalf("a $VAR config must be refused before any write, got %v", err)
	}
	c1, err := commitConfig(ctx, tg, nil, []byte(blogConfig), confighome.ZeroCommit, "create")
	if err != nil {
		t.Fatal(err)
	}
	next := []byte(blogConfig + "volumes:\n  /etc: /host-etc\n")
	c2, err := commitConfig(ctx, tg, []byte(blogConfig), next, c1, "bind")
	if err != nil {
		t.Fatal(err)
	}
	evs, err := events.Read(ctx, tg.exec, "blog", 0)
	if err != nil || len(evs) != 2 {
		t.Fatalf("events = %+v, %v", evs, err)
	}
	last := evs[1]
	if last.Kind != events.KindConfigSet || last.ConfigCommit != c2 || last.Detail["privileged"] != true || last.Detail["previous_commit"] != c1 || last.Actor.Label != "test@host" {
		t.Fatalf("config.set event = %+v", last)
	}
	if _, err := commitConfig(ctx, tg, []byte(blogConfig), []byte(blogConfig), c1, "stale"); !errors.Is(err, errConfigConflict) {
		t.Fatalf("a write against a superseded commit = %v, want a conflict", err)
	}
	if code := classifyMachineError(&configConflictError{app: "blog", current: c2}); code != codeConflict {
		t.Fatalf("conflict classified as %q", code)
	}
}

// Key edits without --expect rebase onto whatever head they find, so N
// concurrent `config set k=v` all land; with --expect only one does.
func TestSetConfigKeysConcurrent(t *testing.T) {
	tg := localConfigTarget(t)
	ctx := context.Background()
	base, err := commitConfig(ctx, tg, nil, []byte(blogConfig), confighome.ZeroCommit, "create")
	if err != nil {
		t.Fatal(err)
	}
	const n = 4
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = setConfigKeys(ctx, tg, []string{"env.K" + string(rune('A'+i)) + "=v"}, nil, base)
		}(i)
	}
	wg.Wait()
	var wins int
	for _, err := range errs {
		if err == nil {
			wins++
		} else if !errors.Is(err, errConfigConflict) {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if wins != 1 {
		t.Fatalf("with --expect, %d writers won, want exactly 1", wins)
	}

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = setConfigKeys(ctx, tg, []string{"env.R" + string(rune('A'+i)) + "=v"}, nil, "")
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatalf("rebasing key edit failed: %v", err)
		}
	}
	head, _, err := readHead(ctx, tg)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if !bytes.Contains(head, []byte("R"+string(rune('A'+i))+": v")) {
			t.Fatalf("key edit %d lost:\n%s", i, head)
		}
	}
}

func TestPulledHeaderRoundTrip(t *testing.T) {
	c := strings.Repeat("ab", 20)
	data := []byte(pulledHeaderLine("blog", c, "prod") + blogConfig)
	h, body := parsePulledHeader(data)
	if h.app != "blog" || h.commit != c || h.server != "prod" || string(body) != blogConfig {
		t.Fatalf("header = %+v body = %q", h, body)
	}
	if h, body := parsePulledHeader([]byte(blogConfig)); h.app != "" || string(body) != blogConfig {
		t.Fatal("a plain config must not parse as pulled")
	}
}

func TestUnprivilegedRefusesHostReachingChange(t *testing.T) {
	tg := localConfigTarget(t)
	ctx := context.Background()
	c1, err := commitConfig(ctx, tg, nil, []byte(blogConfig), confighome.ZeroCommit, "create")
	if err != nil {
		t.Fatal(err)
	}
	tg.unprivileged = true
	if _, err := commitConfig(ctx, tg, []byte(blogConfig), []byte(blogConfig+"volumes:\n  /etc: /host-etc\n"), c1, "bind"); !errors.Is(err, errConfigPrivileged) {
		t.Fatalf("a host bind under --unprivileged = %v, want refusal", err)
	}
	if _, err := commitConfig(ctx, tg, []byte(blogConfig), []byte(strings.Replace(blogConfig, "nginx:1.27", "nginx:1.28", 1)), c1, "bump"); err != nil {
		t.Fatalf("an image bump is not privileged: %v", err)
	}
}
