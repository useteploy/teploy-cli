//go:build linux

package targetguard

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// localExecutor runs commands against a real local sh with
// TEPLOY_DEPLOYMENTS_ROOT redirected into the test's temp dir — the REAL
// guard script runs, the REAL flock serializes. The helper's fixed remote
// path is translated into the root so the invocation finds the uploaded
// copy without writing to the real /tmp.
type localExecutor struct {
	root string
	mu   sync.Mutex
	saw  []string
}

func (e *localExecutor) Run(_ context.Context, cmd string) (string, error) {
	e.mu.Lock()
	e.saw = append(e.saw, cmd)
	e.mu.Unlock()
	cmd = strings.ReplaceAll(cmd, helperRemote, filepath.Join(e.root, "teploy-guard.sh"))
	full := fmt.Sprintf("TEPLOY_DEPLOYMENTS_ROOT=%s; %s", e.root, cmd)
	script := filepath.Join(e.root, "run.sh")
	if err := os.WriteFile(script, []byte(full), 0755); err != nil {
		return "", err
	}
	out, err := exec.Command("/bin/sh", script).CombinedOutput()
	return string(out), err
}

func (e *localExecutor) RunStream(_ context.Context, cmd string, stdout, stderr io.Writer) error {
	e.mu.Lock()
	e.saw = append(e.saw, cmd)
	e.mu.Unlock()
	cmd = strings.ReplaceAll(cmd, helperRemote, filepath.Join(e.root, "teploy-guard.sh"))
	full := fmt.Sprintf("TEPLOY_DEPLOYMENTS_ROOT=%s; %s", e.root, cmd)
	script := filepath.Join(e.root, "run.sh")
	if err := os.WriteFile(script, []byte(full), 0755); err != nil {
		return err
	}
	proc := exec.Command("/bin/sh", script)
	proc.Stdout = stdout
	proc.Stderr = stderr
	return proc.Run()
}

func (e *localExecutor) RunInput(_ context.Context, cmd string, stdin io.Reader) error {
	e.mu.Lock()
	e.saw = append(e.saw, cmd)
	e.mu.Unlock()
	cmd = strings.ReplaceAll(cmd, helperRemote, filepath.Join(e.root, "teploy-guard.sh"))
	full := fmt.Sprintf("TEPLOY_DEPLOYMENTS_ROOT=%s; %s", e.root, cmd)
	script := filepath.Join(e.root, "run.sh")
	if err := os.WriteFile(script, []byte(full), 0755); err != nil {
		return err
	}
	proc := exec.Command("/bin/sh", script)
	proc.Stdin = stdin
	proc.Stdout = io.Discard
	proc.Stderr = io.Discard
	return proc.Run()
}

func (e *localExecutor) Upload(_ context.Context, content io.Reader, remotePath string, mode string) error {
	data, err := io.ReadAll(content)
	if err != nil {
		return err
	}
	local := filepath.Join(e.root, filepath.Base(remotePath))
	return os.WriteFile(local, data, 0755)
}

func (e *localExecutor) Close() error      { return nil }
func (e *localExecutor) Host() string      { return "local" }
func (e *localExecutor) User() string      { return "root" }

// The three C01-1 acceptance invariants, for real:
//
// 1. Two concurrent guarded effects SERIALIZE — one waits, neither
//    interleaves inside the critical section.
// 2. A killed helper auto-releases the lock (process death — the property
//    the mkdir lock lacks); the next guarded effect is NOT blocked.
// 3. Generation fencing: a plan prepared against generation 3 is refused
//    when generation 7 is committed (GUARD_FENCED), and the effect does
//    not run.
func TestGuardSerializesConcurrentEffects(t *testing.T) {
	root := t.TempDir()
	exec := &localExecutor{root: root}

	// The effect appends a marker, sleeps, appends again. SERIALIZED runs
	// produce ABAB (one process's full critical section, then the other's);
	// interleaved runs produce AABB (both enter before either finishes).
	// Proven live with timestamps on Debian/util-linux and alpine/busybox
	// (podman, 2026-09-23) — see the repo's C01-1 receipt.
	effect := `printf A >> "$TEPLOY_DEPLOYMENTS_ROOT/web/log"; sleep 0.3; printf B >> "$TEPLOY_DEPLOYMENTS_ROOT/web/log"`
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := Run(context.Background(), exec, "web", 0, effect); err != nil {
				t.Errorf("guarded effect: %v", err)
			}
		}()
	}
	wg.Wait()
	log, err := os.ReadFile(filepath.Join(root, "web", "log"))
	if err != nil {
		t.Fatal(err)
	}
	if string(log) != "ABAB" {
		t.Fatalf("critical section interleaved (want ABAB serialized, got %q)", log)
	}
}

func TestGuardKilledHelperReleasesLock(t *testing.T) {
	root := t.TempDir()
	exec := &localExecutor{root: root}

	// A helper that dies mid-critical-section (SIGKILL — no cleanup).
	deadly := `sh -c 'echo killed >> "$TEPLOY_DEPLOYMENTS_ROOT/web/log"; kill -9 $$'`
	if _, err := Run(context.Background(), exec, "web", 0, deadly); err == nil || !strings.Contains(err.Error(), "exit") {
		t.Fatalf("killed effect should report failure: %v", err)
	}

	// The OS released the lock with the process: the next guarded effect
	// proceeds immediately (a mkdir lock would still hold it).
	done := make(chan error, 1)
	go func() {
		_, err := Run(context.Background(), exec, "web", 0, "true")
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("post-death effect: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("lock not released after helper death")
	}
}

func TestGuardFencesStaleGeneration(t *testing.T) {
	root := t.TempDir()
	exec := &localExecutor{root: root}

	appDir := filepath.Join(root, "web")
	if err := os.MkdirAll(appDir, 0755); err != nil {
		t.Fatal(err)
	}
	// Generation 7 committed on-target; the calling plan is stale (3).
	if err := os.WriteFile(filepath.Join(appDir, ".generation"), []byte("7\n"), 0644); err != nil {
		t.Fatal(err)
	}

	marker := filepath.Join(root, "web", "stale-effect-ran")
	_, err := Run(context.Background(), exec, "web", 3, "touch "+marker)
	if err == nil || !strings.Contains(err.Error(), "fenced") {
		t.Fatalf("stale plan: %v", err)
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatal("fenced effect ran anyway")
	}

	// The same generation (not superseded) is allowed.
	if _, err := Run(context.Background(), exec, "web", 7, "true"); err != nil {
		t.Fatalf("current-generation effect: %v", err)
	}
}
