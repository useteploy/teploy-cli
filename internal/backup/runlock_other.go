//go:build windows

package backup

import (
	"errors"
	"time"
)

// RunLockHolder describes the process that holds the overlap lock.
type RunLockHolder struct {
	PID       int
	StartedAt time.Time
}

// RunLock is unavailable here: `backup run --all` runs on Linux servers.
type RunLock struct{}

// ErrRunLocked means another run holds the lock.
var ErrRunLocked = errors.New("another `teploy backup run --all` is running")

// AcquireRunLock refuses: this is a server-side command.
func AcquireRunLock(path string, now time.Time, staleAfter time.Duration) (*RunLock, RunLockHolder, error) {
	return nil, RunLockHolder{}, errors.New("backup run --all runs on the server (Linux)")
}

// Release is a no-op.
func (l *RunLock) Release() {}
