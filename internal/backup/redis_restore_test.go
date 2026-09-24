package backup

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// redisRestoreExec is a behavioral executor for the redis restore branch:
// the scaffolding commands (aws check, mktemp, AOF preflight, s3 download)
// are answered directly, and the generated restore SCRIPT runs under a real
// bash with a stub docker binary on PATH — the T37/T38 regression tests
// exercise the script's actual control flow (arming, baseline capture,
// compensation), not its string shape.
type redisRestoreExec struct {
	mu       sync.Mutex
	calls    []string
	stubDir  string
	env      []string // extra env for the stub docker (scenario knobs)
	dockerFn func(scriptPath string) (string, error)
	tmpdirs  []string
}

func (e *redisRestoreExec) Run(ctx context.Context, cmd string) (string, error) {
	e.mu.Lock()
	e.calls = append(e.calls, cmd)
	e.mu.Unlock()
	switch {
	case strings.HasPrefix(cmd, "which aws"):
		return "", nil
	case strings.HasPrefix(cmd, "mktemp -d "):
		dir, err := os.MkdirTemp("", "teploy-restore-test-")
		if err != nil {
			return "", err
		}
		e.tmpdirs = append(e.tmpdirs, dir)
		return dir, nil
	case strings.HasPrefix(cmd, "docker exec 'myapp-cache' redis-cli --raw config get appendonly"):
		return "appendonly\nno", nil
	case strings.Contains(cmd, "aws s3 cp "):
		// "Download" a real gzip archive so the script's gunzip succeeds.
		var out string
		for _, f := range strings.Fields(cmd) {
			if strings.HasSuffix(f, ".rdb.gz'") || strings.HasSuffix(f, ".rdb.gz") {
				out = strings.Trim(f, "'")
			}
		}
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		zw.Write([]byte("new-dump"))
		zw.Close()
		if err := os.WriteFile(out, buf.Bytes(), 0644); err != nil {
			return "", err
		}
		return "", nil
	case strings.HasPrefix(cmd, "set -eu"):
		script := filepath.Join(e.stubDir, "script.sh")
		if err := os.WriteFile(script, []byte(cmd), 0755); err != nil {
			return "", err
		}
		c := exec.CommandContext(ctx, "bash", script)
		c.Env = append(append(os.Environ(), "PATH="+e.stubDir+":"+os.Getenv("PATH")), e.env...)
		var out bytes.Buffer
		c.Stdout = &out
		c.Stderr = &out
		err := c.Run()
		return out.String(), err
	case strings.HasPrefix(cmd, "rm -rf "):
		path := strings.Trim(strings.TrimPrefix(cmd, "rm -rf "), "'")
		os.RemoveAll(path)
		return "", nil
	}
	return "", fmt.Errorf("redisRestoreExec: unexpected command: %s", cmd)
}

func (e *redisRestoreExec) RunStream(ctx context.Context, cmd string, stdout, stderr io.Writer) error {
	out, err := e.Run(ctx, cmd)
	if out != "" {
		stdout.Write([]byte(out))
	}
	return err
}

func (e *redisRestoreExec) RunInput(ctx context.Context, cmd string, stdin io.Reader) error {
	_, err := e.Run(ctx, cmd)
	return err
}

func (e *redisRestoreExec) Upload(ctx context.Context, content io.Reader, remotePath, mode string) error {
	return nil
}

func (e *redisRestoreExec) Close() error { return nil }
func (e *redisRestoreExec) Host() string { return "1.2.3.4" }
func (e *redisRestoreExec) User() string { return "root" }

// writeDockerStub writes the stub docker binary the restore script drives.
// Scenario knobs (env): HAVE_DUMP=yes/no, FAIL_BASELINE=transport,
// FAIL_INSTALL=yes.
func writeDockerStub(t *testing.T, dir string) string {
	t.Helper()
	stub := `#!/bin/sh
log="$DOCKER_LOG"
echo "$*" >> "$log"
cmd="$1"; shift
case "$cmd" in
  stop) exit 0 ;;
  start) exit 0 ;;
  exec) printf 'appendonly\nno\n'; exit 0 ;;
  cp)
    if [ "$1" = "myapp-cache:/data/dump.rdb" ]; then
      if [ "$FAIL_BASELINE" = "transport" ]; then echo "transport error" >&2; exit 1; fi
      if [ "$HAVE_DUMP" = "yes" ]; then echo old-dump > "$2"; exit 0; fi
      echo "Error: No such container/path: myapp-cache:/data/dump.rdb" >&2; exit 1
    fi
    if [ "$FAIL_INSTALL" = "yes" ]; then echo "install failed" >&2; exit 1; fi
    exit 0 ;;
esac
exit 0
`
	path := filepath.Join(dir, "docker")
	if err := os.WriteFile(path, []byte(stub), 0755); err != nil {
		t.Fatal(err)
	}
	return path
}

func runRedisRestore(t *testing.T, env ...string) (error, string) {
	t.Helper()
	stubDir := t.TempDir()
	writeDockerStub(t, stubDir)
	dockerLog := filepath.Join(stubDir, "docker.log")
	ex := &redisRestoreExec{stubDir: stubDir, env: append([]string{"DOCKER_LOG=" + dockerLog}, env...)}
	c := NewClient(ex, os.Stdout)
	err := c.AccessoryRestore(context.Background(), "myapp", "cache", "redis:7", "20260919", nil, S3Config{Bucket: "b", Region: "us-east-1"})
	logBytes, _ := os.ReadFile(dockerLog)
	return err, string(logBytes)
}

// TestRedisRestore_BaselineCaptureFailureRestartsService is the T37
// regression: a failed pre-restore docker cp (under set -e) used to exit the
// script immediately with Redis stopped and no recovery attempted.
func TestRedisRestore_BaselineCaptureFailureRestartsService(t *testing.T) {
	err, log := runRedisRestore(t, "FAIL_BASELINE=transport")
	if err == nil {
		t.Fatal("the restore must fail when the baseline capture fails")
	}
	lines := strings.Split(strings.TrimSpace(log), "\n")
	var cpIdx, startIdx = -1, -1
	for i, l := range lines {
		if strings.Contains(l, "myapp-cache:/data/dump.rdb") && cpIdx == -1 {
			cpIdx = i
		}
		if strings.Contains(l, "start myapp-cache") && startIdx == -1 {
			startIdx = i
		}
	}
	if cpIdx == -1 {
		t.Fatalf("baseline capture not attempted: %q", log)
	}
	if startIdx < cpIdx {
		t.Fatalf("no restart attempted after the failed baseline capture: %q", log)
	}
}

// TestRedisRestore_PostStopBaselineCapturedWhenPresent pins A41+T38: the
// baseline copy runs against the STOPPED container (after the shutdown
// save), so a dump present at shutdown is preserved and put back when the
// install fails.
func TestRedisRestore_PostStopBaselineCapturedWhenPresent(t *testing.T) {
	err, log := runRedisRestore(t, "HAVE_DUMP=yes", "FAIL_INSTALL=yes")
	if err == nil {
		t.Fatal("the restore must fail when installing the replacement dump fails")
	}
	// Order: stop → baseline cp (container→host) → failed install cp →
	// restore_original's cp back → start.
	var stop, baseline, install, restoreCp, restart = -1, -1, -1, -1, -1
	for i, l := range strings.Split(log, "\n") {
		switch {
		case strings.Contains(l, "stop myapp-cache") && stop == -1:
			stop = i
		case strings.Contains(l, "myapp-cache:/data/dump.rdb") && baseline == -1:
			baseline = i // first container→host copy
		case strings.Contains(l, "/data/dump.rdb") && strings.HasSuffix(l, "myapp-cache:/data/dump.rdb") && install == -1 && i > baseline:
			install = i
		case strings.Contains(l, "myapp-cache:/data/dump.rdb") && i > install && restoreCp == -1:
			restoreCp = i
		case strings.Contains(l, "start myapp-cache") && restart == -1:
			restart = i
		}
	}
	if stop == -1 || baseline == -1 || install == -1 || restoreCp == -1 || restart == -1 {
		t.Fatalf("expected stop → baseline → install → restore-back → restart, got: %q", log)
	}
	if !(stop < baseline && baseline < install && install < restoreCp && restoreCp < restart) {
		t.Fatalf("compensation order wrong: %q", log)
	}
}

// TestRedisRestore_NoDumpMeansNoBaseline: a proven not-found baseline (the
// shutdown wrote no RDB) is not an error — the restore proceeds and the
// service starts.
func TestRedisRestore_NoDumpMeansNoBaseline(t *testing.T) {
	err, log := runRedisRestore(t, "HAVE_DUMP=no")
	if err != nil {
		t.Fatalf("restore with no pre-existing dump must succeed: %v", err)
	}
	if !strings.Contains(log, "start myapp-cache") {
		t.Fatalf("container was not started: %q", log)
	}
}
