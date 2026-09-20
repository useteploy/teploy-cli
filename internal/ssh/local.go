package ssh

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
)

// Compile-time check: LocalExecutor implements Executor.
var _ Executor = (*LocalExecutor)(nil)

// LocalExecutor implements Executor by running commands directly on the
// local machine instead of over SSH — for code that already knows how to
// operate against an ssh.Executor (internal/deploy, internal/state,
// internal/caddy, internal/accessories, ...) but needs to run on the
// server itself rather than being driven remotely. This is what lets
// `teploy autodeploy serve` (a resident process installed ON the target
// server, see internal/cli/autodeploy.go) call the exact same deploy code
// `teploy deploy` uses from an operator's machine, instead of
// reimplementing "how to deploy" a second time in a generated shell
// script — see the autodeploy rebuild for the drift problem that caused.
type LocalExecutor struct{}

// NewLocalExecutor creates an Executor that runs commands on this machine.
func NewLocalExecutor() *LocalExecutor {
	return &LocalExecutor{}
}

func (e *LocalExecutor) Run(ctx context.Context, cmd string) (string, error) {
	c := localCommand(ctx, cmd)
	out, err := c.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, string(out))
	}
	return string(out), nil
}

func (e *LocalExecutor) RunStream(ctx context.Context, cmd string, stdout, stderr io.Writer) error {
	c := localCommand(ctx, cmd)
	c.Stdout = stdout
	c.Stderr = stderr
	return c.Run()
}

func (e *LocalExecutor) RunInput(ctx context.Context, cmd string, stdin io.Reader) error {
	c := localCommand(ctx, cmd)
	c.Stdin = stdin
	c.Stdout = io.Discard
	c.Stderr = io.Discard
	return c.Run()
}

// Upload writes content to a local file atomically, mirroring
// RemoteExecutor.Upload's contract (audit A27): the previous version
// buffered the whole input (ignoring cancellation), called os.WriteFile
// directly on the destination (following a leaf symlink, truncating an
// existing file on a mid-write failure), and chmod'd only AFTER the
// content was already visible at the destination's old permissions. The
// resident autodeploy path runs on this executor, so the remote
// hardening did not cover it.
//
// The write lands in a private sibling temp (0600 from creation), is
// chmod'd to the requested mode and fsync'd BEFORE publication, and is
// renamed over the destination — replacing a destination symlink itself,
// never its target. A failed or cancelled upload leaves the previous
// contents untouched.
func (e *LocalExecutor) Upload(ctx context.Context, content io.Reader, path string, mode string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	perm, err := strconv.ParseUint(mode, 8, 32)
	if err != nil {
		return fmt.Errorf("invalid mode %q: %w", mode, err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("creating directory for %s: %w", path, err)
	}
	f, err := os.CreateTemp(dir, ".teploy-upload-*")
	if err != nil {
		return fmt.Errorf("creating temp file beside %s: %w", path, err)
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	// ctx is checked between reads; a reader that can block indefinitely
	// must be closed by its owner (same contract as RemoteExecutor).
	if _, err := io.Copy(f, readerWithCtx{ctx, content}); err != nil {
		f.Close()
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := f.Chmod(os.FileMode(perm)); err != nil {
		f.Close()
		return fmt.Errorf("chmod %s: %w", tmp, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("syncing %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", tmp, err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("publishing %s: %w", path, err)
	}
	// Sync the containing directory after the rename (T46's local half):
	// the file itself was fsynced above, but without a directory fsync a
	// power loss can leave the rename unpersisted — the old file back, or
	// nothing. The remote-shell halves stay on the deferred durability
	// contract (documented in AUDIT_OPEN.md).
	if d, derr := os.Open(dir); derr == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// readerWithCtx fails a copy once the context is done; reads themselves
// still block on the underlying reader (documented above).
type readerWithCtx struct {
	ctx context.Context
	r   io.Reader
}

func (r readerWithCtx) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

func (e *LocalExecutor) Close() error {
	return nil
}

func (e *LocalExecutor) Host() string {
	return "localhost"
}

func (e *LocalExecutor) User() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return "unknown"
}
