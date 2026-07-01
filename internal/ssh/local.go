package ssh

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
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
	out, err := exec.CommandContext(ctx, "sh", "-c", cmd).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, string(out))
	}
	return string(out), nil
}

func (e *LocalExecutor) RunStream(ctx context.Context, cmd string, stdout, stderr io.Writer) error {
	c := exec.CommandContext(ctx, "sh", "-c", cmd)
	c.Stdout = stdout
	c.Stderr = stderr
	return c.Run()
}

// Upload writes content to a local file, creating parent directories and
// setting mode (an octal string, e.g. "0644") — mirroring
// RemoteExecutor.Upload's semantics exactly so callers built against the
// Executor interface don't need to know which implementation they have.
func (e *LocalExecutor) Upload(ctx context.Context, content io.Reader, path string, mode string) error {
	data, err := io.ReadAll(content)
	if err != nil {
		return fmt.Errorf("reading upload content: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("creating directory for %s: %w", path, err)
	}

	perm, err := strconv.ParseUint(mode, 8, 32)
	if err != nil {
		return fmt.Errorf("invalid mode %q: %w", mode, err)
	}

	if err := os.WriteFile(path, data, os.FileMode(perm)); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	// os.WriteFile only applies the mode on create — an existing file keeps
	// its old permissions. Chmod explicitly so re-uploading (e.g. a
	// redeployed binary) always ends up at the requested mode.
	if err := os.Chmod(path, os.FileMode(perm)); err != nil {
		return fmt.Errorf("chmod %s: %w", path, err)
	}
	return nil
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
