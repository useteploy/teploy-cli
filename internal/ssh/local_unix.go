//go:build unix

package ssh

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// localCommand builds the local `sh -c` invocation with whole-process-tree
// cancellation semantics (audit A28): the shell runs in its own process
// group, and cancelling the context SIGKILLs the GROUP — exec.Command's
// default kills only the shell process, so descendants kept running (and
// kept stdout/stderr open, blocking CombinedOutput/Wait indefinitely on a
// resident deployment engine whose lease was already released). WaitDelay
// bounds the wait after cancellation even when a descendant slips past the
// group kill.
func localCommand(ctx context.Context, script string) *exec.Cmd {
	c := exec.CommandContext(ctx, "sh", "-c", script)
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Cancel = func() error {
		if c.Process == nil {
			return os.ErrProcessDone
		}
		err := syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	c.WaitDelay = 2 * time.Second
	return c
}
