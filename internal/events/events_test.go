package events

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/useteploy/teploy/internal/ssh"
)

func localRoot(t *testing.T, max string) string {
	t.Helper()
	if _, err := os.Stat("/usr/bin/python3"); err != nil {
		if _, err := os.Stat("/opt/homebrew/bin/python3"); err != nil {
			t.Skip("python3 not available")
		}
	}
	root := filepath.Join(t.TempDir(), ".teploy")
	t.Setenv("TEPLOY_EVENTS_ROOT_TEST", root)
	if max != "" {
		t.Setenv("TEPLOY_EVENTS_MAX_TEST", max)
	}
	return root
}

// Acceptance gate 4: concurrent writers and rotation lose no lines, seq is
// strictly increasing, files are 0600.
func TestAppendConcurrentWithRotation(t *testing.T) {
	root := localRoot(t, "4096")
	exec := ssh.NewLocalExecutor()
	ctx := context.Background()

	const writers, each = 3, 12
	var wg sync.WaitGroup
	errs := make(chan error, writers*each)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				_, err := Append(ctx, exec, Event{App: "blog", Kind: KindDeploy, Outcome: Succeeded,
					Actor: Actor{Client: fmt.Sprintf("w%d", w)}, Detail: map[string]any{"image": strings.Repeat("x", 40)}})
				if err != nil {
					errs <- err
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	got, err := Read(ctx, exec, "blog", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != writers*each {
		t.Fatalf("read %d events, want %d (lost lines across rotation?)", len(got), writers*each)
	}
	for i, ev := range got {
		if ev.Seq != int64(i+1) {
			t.Fatalf("event %d has seq %d, want strictly increasing from 1", i, ev.Seq)
		}
		if ev.TS.IsZero() || ev.V != 1 || ev.ID == "" {
			t.Fatalf("server fields not assigned: %+v", ev)
		}
	}
	dir := filepath.Join(root, "apps", "blog")
	if _, err := os.Stat(filepath.Join(dir, "events.jsonl.1")); err != nil {
		t.Fatal("expected a rotated file at this size limit")
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		info, _ := e.Info()
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s mode %v, want 0600", e.Name(), info.Mode().Perm())
		}
	}

	since, err := Read(ctx, exec, "blog", int64(writers*each-2))
	if err != nil || len(since) != 2 {
		t.Fatalf("since-seq read = %d events (%v), want 2", len(since), err)
	}
}

func TestReadAllAppsAndSanitize(t *testing.T) {
	localRoot(t, "")
	exec := ssh.NewLocalExecutor()
	ctx := context.Background()
	if _, err := Append(ctx, exec, Event{App: "a", Kind: KindEnvSet, Outcome: Succeeded,
		Detail: map[string]any{"keys": []string{"DB_URL"}, "value": "postgres://u:p@h/db"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := Append(ctx, exec, Event{App: "b", Kind: KindDeploy, Outcome: Failed,
		Detail: map[string]any{"error": strings.Repeat("e", 1000)}}); err != nil {
		t.Fatal(err)
	}
	all, err := Read(ctx, exec, "", 0)
	if err != nil || len(all) != 2 {
		t.Fatalf("all apps = %d (%v)", len(all), err)
	}
	for _, ev := range all {
		if _, leaked := ev.Detail["value"]; leaked {
			t.Fatal("a value outside the kind's allowlist reached the log")
		}
		if e, ok := ev.Detail["error"].(string); ok && len(e) > maxErrorDetail+3 {
			t.Fatalf("error detail not bounded: %d chars", len(e))
		}
	}
}

func TestReadMissingIsEmpty(t *testing.T) {
	localRoot(t, "")
	got, err := Read(context.Background(), ssh.NewLocalExecutor(), "nothing", 0)
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v", got, err)
	}
}
