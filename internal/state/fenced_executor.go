package state

import (
	"context"
	"github.com/useteploy/teploy/internal/ssh"
	"io"
)

// FencedExecutor keeps preparation effects under the caller's renewable lease.
// Upload writes only into already admitted private managed directories.
type FencedExecutor struct {
	ssh.Executor
	Lock *Lock
}

func (e *FencedExecutor) UnderFence() bool { return true }
func (e *FencedExecutor) Run(ctx context.Context, command string) (string, error) {
	return e.Lock.Guarded(ctx, e.Executor, command)
}
func (e *FencedExecutor) RunInput(ctx context.Context, command string, input io.Reader) error {
	return e.Executor.RunInput(ctx, e.Lock.GuardPrefix()+command, input)
}
func (e *FencedExecutor) RunStream(ctx context.Context, command string, out, stderr io.Writer) error {
	return e.Executor.RunStream(ctx, e.Lock.GuardPrefix()+command, out, stderr)
}
func (e *FencedExecutor) Upload(ctx context.Context, input io.Reader, path, mode string) error {
	if err := e.Lock.Check(ctx, e.Executor); err != nil {
		return err
	}
	return e.Executor.Upload(ctx, input, path, mode)
}
