package ssh

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"time"
)

// Executor defines the interface for running commands on a remote server.
// All server interaction in teploy goes through this interface.
// Tests use MockExecutor; production uses RemoteExecutor.
type Executor interface {
	// Run executes a command and returns the combined output.
	Run(ctx context.Context, cmd string) (string, error)

	// RunStream executes a command and streams stdout/stderr to the provided writers in real time.
	RunStream(ctx context.Context, cmd string, stdout, stderr io.Writer) error

	// Upload sends content to a remote file with the specified permissions.
	Upload(ctx context.Context, content io.Reader, remotePath string, mode string) error

	// Close closes the underlying SSH connection.
	Close() error

	// Host returns the target host address.
	Host() string

	// User returns the SSH user for this connection.
	User() string
}

// UploadAtomic uploads content beside remotePath, then atomically renames it
// into place. A failed upload or rename leaves the existing destination intact.
func UploadAtomic(ctx context.Context, exec Executor, content io.Reader, remotePath, mode string) error {
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return fmt.Errorf("generating temporary upload path: %w", err)
	}
	tmpPath := remotePath + ".tmp-" + hex.EncodeToString(suffix[:])
	committed := false
	defer func() {
		if committed {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = exec.Run(cleanupCtx, "rm -f -- "+ShellQuote(tmpPath))
	}()

	if err := exec.Upload(ctx, content, tmpPath, mode); err != nil {
		return fmt.Errorf("uploading temporary file: %w", err)
	}
	if _, err := exec.Run(ctx, "mv -f -- "+ShellQuote(tmpPath)+" "+ShellQuote(remotePath)); err != nil {
		return fmt.Errorf("renaming temporary file into place: %w", err)
	}
	committed = true
	return nil
}
