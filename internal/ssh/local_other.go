//go:build !unix

package ssh

import (
	"context"
	"os"
	"os/exec"
	"time"
)

// localCommand is the non-Unix fallback (audit A28): no process groups
// exist to kill, so cancellation kills the shell process directly and
// WaitDelay bounds the wait after cancellation. The LocalExecutor's
// resident mode targets Linux servers; on other platforms this keeps the
// build honest about what it can guarantee instead of pretending.
func localCommand(ctx context.Context, script string) *exec.Cmd {
	c := exec.CommandContext(ctx, "sh", "-c", script)
	c.Cancel = func() error {
		if c.Process == nil {
			return os.ErrProcessDone
		}
		return c.Process.Kill()
	}
	c.WaitDelay = 2 * time.Second
	return c
}
