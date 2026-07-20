package state

import (
	"context"
	"encoding/base64"
	"io"
	"strings"
	"testing"
)

// fakeFS is a minimal ssh.Executor that emulates just the shell commands the
// pin helpers issue (cat, mkdir -p, and the base64 write), so the full
// read/modify/write round-trip — including base64 encode/decode and the
// dedup/sort logic — is exercised for real.
type fakeFS struct{ files map[string]string }

func newFakeFS() *fakeFS { return &fakeFS{files: map[string]string{}} }

func (f *fakeFS) Run(ctx context.Context, cmd string) (string, error) {
	switch {
	case strings.HasPrefix(cmd, "cat "):
		path := strings.TrimSuffix(strings.TrimPrefix(cmd, "cat "), " 2>/dev/null")
		path = strings.TrimSpace(path)
		return f.files[path], nil
	case strings.HasPrefix(cmd, "mkdir -p"):
		return "", nil
	case strings.HasPrefix(cmd, "printf %s "):
		// printf %s '<b64>' | base64 -d > <path>
		rest := strings.TrimPrefix(cmd, "printf %s ")
		pipe := strings.Index(rest, " | base64 -d > ")
		b64 := strings.Trim(rest[:pipe], "'")
		path := strings.TrimSpace(rest[pipe+len(" | base64 -d > "):])
		decoded, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return "", err
		}
		f.files[path] = string(decoded)
		return "", nil
	}
	return "", nil
}

func (f *fakeFS) RunStream(ctx context.Context, cmd string, stdout, stderr io.Writer) error {
	_, err := f.Run(ctx, cmd)
	return err
}
func (f *fakeFS) Upload(ctx context.Context, content io.Reader, remotePath, mode string) error {
	return nil
}
func (f *fakeFS) Host() string { return "fake" }
func (f *fakeFS) User() string { return "root" }
func (f *fakeFS) Close() error { return nil }

func TestPinRoundTrip(t *testing.T) {
	ctx := context.Background()
	fs := newFakeFS()

	if pins, _ := ReadPins(ctx, fs, "web"); len(pins) != 0 {
		t.Fatalf("expected no pins initially, got %v", pins)
	}

	if err := AddPin(ctx, fs, "web", "abc123"); err != nil {
		t.Fatal(err)
	}
	if err := AddPin(ctx, fs, "web", "def456"); err != nil {
		t.Fatal(err)
	}
	// Idempotent.
	if err := AddPin(ctx, fs, "web", "abc123"); err != nil {
		t.Fatal(err)
	}

	pins, _ := ReadPins(ctx, fs, "web")
	if len(pins) != 2 {
		t.Fatalf("expected 2 pins, got %v", pins)
	}
	// Sorted on write.
	if pins[0] != "abc123" || pins[1] != "def456" {
		t.Fatalf("unexpected pins: %v", pins)
	}

	if err := RemovePin(ctx, fs, "web", "abc123"); err != nil {
		t.Fatal(err)
	}
	pins, _ = ReadPins(ctx, fs, "web")
	if len(pins) != 1 || pins[0] != "def456" {
		t.Fatalf("expected [def456] after remove, got %v", pins)
	}

	// Removing the last pin truncates to empty.
	if err := RemovePin(ctx, fs, "web", "def456"); err != nil {
		t.Fatal(err)
	}
	if pins, _ := ReadPins(ctx, fs, "web"); len(pins) != 0 {
		t.Fatalf("expected empty after removing all, got %v", pins)
	}
}

func TestReadPinsIgnoresBlankLines(t *testing.T) {
	ctx := context.Background()
	fs := newFakeFS()
	fs.files["/deployments/web/pinned"] = "abc123\n\n  def456  \n\n"
	pins, _ := ReadPins(ctx, fs, "web")
	if len(pins) != 2 || pins[0] != "abc123" || pins[1] != "def456" {
		t.Fatalf("unexpected pins: %v", pins)
	}
}
