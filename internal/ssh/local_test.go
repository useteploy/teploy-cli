package ssh

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLocalExecutor_Run(t *testing.T) {
	e := NewLocalExecutor()
	out, err := e.Run(context.Background(), "echo hello")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.TrimSpace(out) != "hello" {
		t.Errorf("got %q, want \"hello\"", out)
	}
}

func TestLocalExecutor_Run_Error(t *testing.T) {
	e := NewLocalExecutor()
	if _, err := e.Run(context.Background(), "exit 1"); err == nil {
		t.Error("expected error for a failing command")
	}
}

func TestLocalExecutor_Upload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "file.txt")

	e := NewLocalExecutor()
	if err := e.Upload(context.Background(), strings.NewReader("hello world"), path, "0644"); err != nil {
		t.Fatalf("Upload: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading uploaded file: %v", err)
	}
	if string(data) != "hello world" {
		t.Errorf("got %q, want \"hello world\"", data)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0644 {
		t.Errorf("got mode %o, want 0644", info.Mode().Perm())
	}
}

func TestLocalExecutor_Upload_ExecutableMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "teploy")

	e := NewLocalExecutor()
	if err := e.Upload(context.Background(), strings.NewReader("#!/bin/sh\necho hi"), path, "0755"); err != nil {
		t.Fatalf("Upload: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0755 {
		t.Errorf("got mode %o, want 0755", info.Mode().Perm())
	}
}

func TestLocalExecutor_HostAndUser(t *testing.T) {
	e := NewLocalExecutor()
	if e.Host() != "localhost" {
		t.Errorf("Host() = %q, want \"localhost\"", e.Host())
	}
	if e.User() == "" {
		t.Error("User() should not be empty")
	}
}

// TestLocalExecutor_Upload_AtomicAndSymlinkSafe is the A27 regression:
// a destination leaf symlink is REPLACED (its target untouched), a failed
// read preserves the previous contents, the requested mode is applied
// before publication, and a cancelled context writes nothing.
func TestLocalExecutor_Upload_AtomicAndSymlinkSafe(t *testing.T) {
	dir := t.TempDir()
	e := NewLocalExecutor()

	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(dir, "secret")
	if err := os.Symlink(victim, dest); err != nil {
		t.Fatal(err)
	}
	if err := e.Upload(context.Background(), strings.NewReader("new"), dest, "0600"); err != nil {
		t.Fatalf("Upload over symlink: %v", err)
	}
	if got, _ := os.ReadFile(victim); string(got) != "old" {
		t.Errorf("symlink target was modified: %q", got)
	}
	data, err := os.ReadFile(dest)
	if err != nil || string(data) != "new" {
		t.Fatalf("destination content: %q %v", data, err)
	}
	if fi, err := os.Lstat(dest); err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm() != 0600 {
		t.Errorf("destination must be a regular 0600 file: %v %v", fi, err)
	}

	// Failed reader: previous contents survive.
	persistent := filepath.Join(dir, "persistent")
	if err := os.WriteFile(persistent, []byte("keep"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := e.Upload(context.Background(), &failingReader{}, persistent, "0600"); err == nil {
		t.Fatal("expected the failed read to error")
	}
	if got, _ := os.ReadFile(persistent); string(got) != "keep" {
		t.Errorf("failed upload corrupted the destination: %q", got)
	}

	// Cancelled context: nothing is written.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cancelled := filepath.Join(dir, "cancelled")
	if err := e.Upload(ctx, strings.NewReader("x"), cancelled, "0600"); err == nil {
		t.Fatal("expected cancellation to error")
	}
	if _, err := os.Stat(cancelled); !os.IsNotExist(err) {
		t.Errorf("cancelled upload wrote the destination: %v", err)
	}
	// No staging siblings are left behind.
	entries, _ := os.ReadDir(dir)
	for _, en := range entries {
		if strings.HasPrefix(en.Name(), ".teploy-upload-") {
			t.Errorf("staging sibling left behind: %s", en.Name())
		}
	}
}

type failingReader struct{ done bool }

func (r *failingReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, os.ErrClosed
	}
	r.done = true
	copy(p, "partial")
	return len("partial"), nil
}

// TestLocalExecutor_CancellationKillsProcessGroup is the A28 regression:
// cancelling a command must terminate the whole process tree and return,
// not leave descendants running with the pipes open. The shell spawns a
// sleeping child; cancellation must let Run return promptly while the
// child is gone.
func TestLocalExecutor_CancellationKillsProcessGroup(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns real processes")
	}
	marker := filepath.Join(t.TempDir(), "alive")
	e := NewLocalExecutor()
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := e.Run(ctx, "sh -c 'sleep 5 && touch "+marker+"' >/dev/null 2>&1; sleep 5")
	if err == nil {
		t.Fatal("expected cancellation error")
	}
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Fatalf("Run did not return promptly after cancellation: %s", elapsed)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(marker); os.IsNotExist(err) {
			return // child died — pass
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("a descendant survived process-group cancellation")
}
