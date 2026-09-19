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
	// LinkDest is an optional remote basis directory for --link-dest: the
	// F08 attempt-scoped build contexts are fresh per attempt, so without
	// a basis every deploy would re-transfer the whole tree. Pointing at
	// the previous attempt's build dir restores incremental transfer and
	// hardlink-shares unchanged files (no extra disk per attempt).
	LinkDest string
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
	// rsync re-parses the -e value through a shell — quote each argument
	// so an identity path containing spaces survives (TCL-52).
	sshCmd := ssh.ExternalSSHCommand(cfg.Host, cfg.KeyPath, cfg.AcceptNewHost)

	// Ensure local dir has trailing slash so rsync copies contents, not the dir itself.
	localDir := strings.TrimRight(cfg.LocalDir, "/") + "/"

	args := []string{
		"-az", "--delete",
		"-e", sshCmd,
	}

	for _, pattern := range cfg.Excludes {
		args = append(args, "--exclude", pattern)
	}
	if cfg.LinkDest != "" {
		args = append(args, "--link-dest="+cfg.LinkDest)
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
