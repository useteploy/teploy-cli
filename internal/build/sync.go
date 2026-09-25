package build

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/useteploy/teploy/internal/ssh"
)

// SyncConfig holds parameters for rsyncing source to the server.
type SyncConfig struct {
	// Source is the resolved selection (ResolveSource): rsync transfers
	// exactly its entries, from its root — never a directory wholesale.
	Source        *Source
	RemoteDir     string // remote destination directory (fresh per attempt)
	Host          string // SSH host (may carry :port; IPv6 literals bracketed automatically)
	User          string // SSH user
	KeyPath       string // SSH key path (optional)
	AcceptNewHost bool   // mirror the control connection's --accept-new policy (see Sync)
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
	if cfg.Source == nil {
		return fmt.Errorf("rsync: no resolved source to transfer")
	}
	// rsync re-parses the -e value through a shell — quote each argument
	// so an identity path containing spaces survives (TCL-52).
	sshCmd := ssh.ExternalSSHCommand(cfg.Host, cfg.KeyPath, cfg.AcceptNewHost)

	// Trailing slash: the entry paths are relative to the root's contents.
	localDir := strings.TrimRight(cfg.Source.Root, "/") + "/"

	// The list is the whole selection (L14): --files-from sends exactly
	// the resolved entries and nothing else, so .gitignore'd and protected
	// files cannot ride along. No --delete: every attempt's build dir is
	// fresh (F08), and rsync's --files-from mode does not recurse into the
	// listed directories anyway.
	args := []string{
		"-az", "--from0", "--files-from=-",
		"-e", sshCmd,
	}
	if cfg.LinkDest != "" {
		args = append(args, "--link-dest="+cfg.LinkDest)
	}

	remote := ssh.RsyncTarget(cfg.User, cfg.Host, cfg.RemoteDir)
	args = append(args, localDir, remote)

	cmd := exec.CommandContext(ctx, "rsync", args...)
	cmd.Stdin = bytes.NewReader(cfg.Source.FileList())
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("rsync failed: %w", err)
	}
	return nil
}
