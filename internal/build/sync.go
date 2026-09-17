package build

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/useteploy/teploy/internal/ssh"
)

// SyncConfig holds parameters for rsyncing source to the server.
type SyncConfig struct {
	LocalDir      string   // local source directory
	RemoteDir     string   // remote destination directory
	Host          string   // SSH host (may carry :port; IPv6 literals bracketed automatically)
	User          string   // SSH user
	KeyPath       string   // SSH key path (optional)
	Excludes      []string // patterns to exclude
	AcceptNewHost bool     // mirror the control connection's --accept-new policy (see Sync)
}

// Sync transfers the local directory to the remote server via rsync over SSH.
// Output is streamed to stdout in real time.
//
// The transport enforces strict host-key verification (audit F27): the
// old `-o StrictHostKeyChecking=no` disabled host-key checking entirely
// for the source-sync channel. acceptNew mirrors the control connection's
// policy, which by sync time has verified (and, under --accept-new,
// recorded) the host key.
func Sync(ctx context.Context, cfg SyncConfig, stdout, stderr io.Writer) error {
	sshArgs := append([]string{"ssh"}, ssh.ExternalSSHArgs(cfg.Host, cfg.KeyPath, cfg.AcceptNewHost)...)
	sshCmd := strings.Join(sshArgs, " ")

	// Ensure local dir has trailing slash so rsync copies contents, not the dir itself.
	localDir := strings.TrimRight(cfg.LocalDir, "/") + "/"

	args := []string{
		"-az", "--delete",
		"-e", sshCmd,
	}

	for _, pattern := range cfg.Excludes {
		args = append(args, "--exclude", pattern)
	}

	remote := ssh.RsyncTarget(cfg.User, cfg.Host, cfg.RemoteDir)
	args = append(args, localDir, remote)

	cmd := exec.CommandContext(ctx, "rsync", args...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("rsync failed: %w", err)
	}
	return nil
}
