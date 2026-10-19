package build

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// openrsync (macOS's system rsync) aborts a --files-from transfer on a
// dangling symlink. Sync leaves such links out — with a warning naming them —
// only when the local rsync is openrsync; GNU rsync gets the full list.
func TestSync_DanglingSymlinksUnderOpenRsync(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fake")
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "metadata"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../store/d3b/missing", filepath.Join(root, "metadata", "system")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("main.go", filepath.Join(root, "alias.go")); err != nil {
		t.Fatal(err)
	}
	src := &Source{Root: root, Entries: []string{"alias.go", "main.go", "metadata/system"}}
	if got := src.DanglingSymlinks(); strings.Join(got, ",") != "metadata/system" {
		t.Fatalf("DanglingSymlinks = %v", got)
	}

	bin := t.TempDir()
	record := filepath.Join(bin, "list")
	fake := "#!/bin/sh\ncat > '" + record + "'\n"
	if err := os.WriteFile(filepath.Join(bin, "rsync"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	orig := isOpenRsync
	t.Cleanup(func() { isOpenRsync = orig })

	for _, tc := range []struct {
		name      string
		openrsync bool
		wantList  string
		warn      bool
	}{
		{"openrsync skips the dangling link", true, "alias.go\x00main.go\x00", true},
		{"GNU rsync keeps it", false, "alias.go\x00main.go\x00metadata/system\x00", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isOpenRsync = func(context.Context) bool { return tc.openrsync }
			var stderr bytes.Buffer
			if err := Sync(context.Background(), SyncConfig{Source: src, RemoteDir: "/r", Host: "192.0.2.1", User: "deploy"}, &bytes.Buffer{}, &stderr); err != nil {
				t.Fatalf("Sync: %v", err)
			}
			got, _ := os.ReadFile(record)
			if string(got) != tc.wantList {
				t.Errorf("files-from list = %q, want %q", got, tc.wantList)
			}
			hasWarn := strings.Contains(stderr.String(), "metadata/system") && strings.Contains(stderr.String(), "openrsync")
			if hasWarn != tc.warn {
				t.Errorf("warning present = %v, want %v: %q", hasWarn, tc.warn, stderr.String())
			}
		})
	}
}
