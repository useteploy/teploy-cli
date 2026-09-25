package releasemeta

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestAttemptMkdirCmd_PrivateModes runs the real command in a real shell
// (with /deployments rebased onto a temp dir) and checks the modes on disk:
// the attempt dir and the attempt root end up 0700 — including a root an
// older CLI left at 0755 — so nothing a sync writes beneath them (the build
// context keeps the operator's 0644 modes on purpose) is reachable by
// other host users (L14).
func TestAttemptMkdirCmd_PrivateModes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell + modes")
	}
	base := t.TempDir()
	att := Attempt{App: "dash", Hash: "v1", ID: "0123456789abcdef"}

	// Pre-existing world-traversable root, as older CLIs created it.
	legacyRoot := filepath.Join(base, "dash/meta/att")
	if err := os.MkdirAll(legacyRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(legacyRoot, 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := strings.ReplaceAll(att.MkdirCmd("build"), deploymentsDir, base)
	if out, err := exec.Command("sh", "-c", cmd).CombinedOutput(); err != nil {
		t.Fatalf("%s: %v\n%s", cmd, err, out)
	}

	for _, dir := range []string{
		filepath.Join(base, "dash/meta/att"),
		filepath.Join(base, "dash/meta/att/v1.0123456789abcdef"),
	} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o700 {
			t.Errorf("%s mode = %o, want 700", dir, perm)
		}
	}
	if info, err := os.Stat(filepath.Join(base, "dash/meta/att/v1.0123456789abcdef/build")); err != nil || !info.IsDir() {
		t.Fatalf("build subdir not created: %v", err)
	}

	// Idempotent: a second writer of the same attempt (env file, receipts)
	// runs the same command without error.
	if out, err := exec.Command("sh", "-c", cmd).CombinedOutput(); err != nil {
		t.Fatalf("rerun: %v\n%s", err, out)
	}
}

// The command string is what every attempt-artifact writer runs; pin its
// shape so a refactor cannot quietly drop the chmod.
func TestAttemptMkdirCmd_Shape(t *testing.T) {
	att := Attempt{App: "ship", Hash: "abc123", ID: "0123456789abcdef"}
	got := att.MkdirCmd()
	want := "mkdir -p /deployments/ship/meta/att/abc123.0123456789abcdef && chmod 700 /deployments/ship/meta/att/abc123.0123456789abcdef && { chmod 700 /deployments/ship/meta/att 2>/dev/null || true; }"
	if got != want {
		t.Fatalf("MkdirCmd() =\n%s\nwant\n%s", got, want)
	}
	if !strings.HasPrefix(att.MkdirCmd("build"), "mkdir -p /deployments/ship/meta/att/abc123.0123456789abcdef /deployments/ship/meta/att/abc123.0123456789abcdef/build && ") {
		t.Fatalf("MkdirCmd(build) = %s", att.MkdirCmd("build"))
	}
}
