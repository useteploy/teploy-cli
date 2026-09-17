package state

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
)

// fakeFS is a minimal ssh.Executor that emulates the shell commands the pin
// helpers and state reader issue (the framed existence check, mkdir, and the
// atomic upload + rename), so the full read/modify/write round-trip —
// including the framing and the dedup/sort logic — is exercised for real.
type fakeFS struct{ files map[string]string }

func newFakeFS() *fakeFS { return &fakeFS{files: map[string]string{}} }

func (f *fakeFS) Run(ctx context.Context, cmd string) (string, error) {
	switch {
	case strings.HasPrefix(cmd, "if [ ! -e "):
		// if [ ! -e 'path' ]; then printf 'absent\n'; else printf 'present\n'; cat -- 'path'; fi
		rest := strings.TrimPrefix(cmd, "if [ ! -e ")
		quoted := rest[:strings.Index(rest, " ]; then")]
		path := strings.Trim(quoted, "'")
		content, ok := f.files[path]
		if !ok {
			return "absent", nil
		}
		return "present\n" + content, nil
	case strings.HasPrefix(cmd, "cat "):
		path := strings.TrimSuffix(strings.TrimPrefix(cmd, "cat "), " 2>/dev/null")
		path = strings.TrimSpace(path)
		return f.files[path], nil
	case strings.HasPrefix(cmd, "mkdir -p"), strings.HasPrefix(cmd, "mkdir "):
		return "", nil
	case strings.HasPrefix(cmd, "mv -f -- "):
		fields := strings.Fields(strings.TrimPrefix(cmd, "mv -f -- "))
		if len(fields) == 2 {
			if data, ok := f.files[strings.Trim(fields[0], "'")]; ok {
				f.files[strings.Trim(fields[1], "'")] = data
			}
		}
		return "", nil
	case strings.HasPrefix(cmd, "rm -f -- "):
		delete(f.files, strings.Trim(strings.TrimPrefix(cmd, "rm -f -- "), "'"))
		return "", nil
	}
	return "", nil
}

func (f *fakeFS) RunStream(ctx context.Context, cmd string, stdout, stderr io.Writer) error {
	_, err := f.Run(ctx, cmd)
	return err
}

func (f *fakeFS) RunInput(ctx context.Context, cmd string, stdin io.Reader) error {
	_, err := f.Run(ctx, cmd)
	return err
}

func (f *fakeFS) Upload(ctx context.Context, content io.Reader, remotePath, mode string) error {
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, content); err != nil {
		return err
	}
	f.files[remotePath] = buf.String()
	return nil
}

func (f *fakeFS) Host() string { return "fake" }
func (f *fakeFS) User() string { return "root" }
func (f *fakeFS) Close() error { return nil }

func TestPinRoundTrip(t *testing.T) {
	ctx := context.Background()
	fs := newFakeFS()

	if pins, err := ReadPins(ctx, fs, "web"); err != nil || len(pins) != 0 {
		t.Fatalf("expected no pins initially, got %v (err %v)", pins, err)
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
	pins, err := ReadPins(ctx, fs, "web")
	if err != nil {
		t.Fatal(err)
	}
	if len(pins) != 2 || pins[0] != "abc123" || pins[1] != "def456" {
		t.Fatalf("unexpected pins: %v", pins)
	}
}

// audit F78: a pin file that exists but cannot be read must be an ERROR,
// never an empty pin set — callers that treat it as "no pins" would prune
// versions the operator deliberately protected.
func TestReadPins_ReadFailureIsError(t *testing.T) {
	exec := &failingFS{}
	if _, err := ReadPins(context.Background(), exec, "web"); err == nil {
		t.Fatal("expected ReadPins to fail when the pin file cannot be read")
	}
}

type failingFS struct{ fakeFS }

func (f *failingFS) Run(ctx context.Context, cmd string) (string, error) {
	return "", fmt.Errorf("permission denied")
}
