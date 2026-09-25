package build

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// contextFingerprint resolves dir's upload selection and fingerprints it.
func contextFingerprint(dir string) (string, error) {
	src, err := ResolveSource(dir)
	if err != nil {
		return "", err
	}
	return src.Fingerprint("")
}

func writeTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// The context fingerprint is the provenance identity of the synced tree:
// identical trees must fingerprint identically (retry stability), and any
// change the builder could observe — content, path, structure, symlink
// target — must move it.
func TestContextFingerprint_DeterministicAndSensitive(t *testing.T) {
	base := map[string]string{
		"main.go":         "package main",
		"cmd/app/run.go":  "func Run() {}",
		"Dockerfile":      "FROM alpine",
		"web/index.html":  "<html></html>",
		"web/empty/.keep": "",
		"docs/README.md":  "# docs",
	}
	a, b := t.TempDir(), t.TempDir()
	writeTree(t, a, base)
	writeTree(t, b, base)

	fa, err := contextFingerprint(a)
	if err != nil {
		t.Fatalf("ContextFingerprint: %v", err)
	}
	fb, err := contextFingerprint(b)
	if err != nil {
		t.Fatalf("ContextFingerprint: %v", err)
	}
	if fa == "" || fa != fb {
		t.Fatalf("identical trees must fingerprint identically: %q vs %q", fa, fb)
	}

	// Re-resolving the SAME tree (a second attempt) is stable.
	fa2, err := contextFingerprint(a)
	if err != nil || fa2 != fa {
		t.Fatalf("same tree re-fingerprinted differently: %q vs %q (%v)", fa, fa2, err)
	}

	// Content change moves the fingerprint (same size, different bytes —
	// isolates content from length).
	if err := os.WriteFile(filepath.Join(b, "main.go"), []byte("package mian"), 0o644); err != nil {
		t.Fatal(err)
	}
	if fb, err = contextFingerprint(b); err != nil || fb == fa {
		t.Fatalf("a content change must move the fingerprint: %q vs %q (%v)", fa, fb, err)
	}

	// Path change (rename, same bytes) moves the fingerprint.
	writeTree(t, b, base)
	if err := os.Rename(filepath.Join(b, "docs"), filepath.Join(b, "docz")); err != nil {
		t.Fatal(err)
	}
	if fb, err = contextFingerprint(b); err != nil || fb == fa {
		t.Fatalf("a rename must move the fingerprint (path is part of identity): %q vs %q (%v)", fa, fb, err)
	}

	// Directory-structure change (new empty dir) moves the fingerprint.
	writeTree(t, b, base)
	if err := os.MkdirAll(filepath.Join(b, "brand/new/dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if fb, err = contextFingerprint(b); err != nil || fb == fa {
		t.Fatalf("a new empty directory must move the fingerprint (TCL-38 parity): %q vs %q (%v)", fa, fb, err)
	}
}

// The fingerprint must describe the tree that gets SYNCED: the always-
// protected patterns (and .teployignore extensions) are not build input
// and must not influence the identity.
func TestContextFingerprint_HonorsExcludePatterns(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	core := map[string]string{"Dockerfile": "FROM alpine", "app.py": "print(1)"}
	writeTree(t, a, core)
	writeTree(t, b, core)
	writeTree(t, b, map[string]string{
		"node_modules/pkg/index.js": "junk",
		".git/config":               "junk",
		".env":                      "SECRET=1",
		".env.local":                "SECRET=2",
	})

	fa, err := contextFingerprint(a)
	if err != nil {
		t.Fatal(err)
	}
	fb, err := contextFingerprint(b)
	if err != nil {
		t.Fatal(err)
	}
	if fa != fb {
		t.Fatalf("excluded patterns leaked into the fingerprint: %q vs %q", fa, fb)
	}
}

func TestContextFingerprint_SymlinkTargetMovesIdentity(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	for _, dir := range []*string{&a, &b} {
		if err := os.WriteFile(filepath.Join(*dir, "target-a"), []byte("A"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("target-a", filepath.Join(a, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b, "target-a"), []byte("A"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("elsewhere", filepath.Join(b, "link")); err != nil {
		t.Fatal(err)
	}
	fa, err := contextFingerprint(a)
	if err != nil {
		t.Fatal(err)
	}
	fb, err := contextFingerprint(b)
	if err != nil {
		t.Fatal(err)
	}
	if fa == fb {
		t.Fatal("a symlink retarget must move the fingerprint (rsync preserves links; the builder sees the target)")
	}
}

func TestEffectiveLocalPlatform(t *testing.T) {
	if got := EffectiveLocalPlatform("linux/arm64"); got != "linux/arm64" {
		t.Errorf("explicit platform must win: %q", got)
	}
	// The implicit cross-compile rule only exists on Apple silicon.
	want := ""
	if runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" {
		want = "linux/amd64"
	}
	if got := EffectiveLocalPlatform(""); got != want {
		t.Errorf("implicit platform: got %q want %q", got, want)
	}
}
