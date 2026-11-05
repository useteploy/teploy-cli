//go:build !windows

package backup

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// RunLockHolder describes the process that holds the overlap lock.
type RunLockHolder struct {
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"started_at"`
}

// RunLock is the overlap lock of `backup run --all`: an flock on a file. The
// kernel drops it the moment the process dies, however it died, so a killed run
// cannot block the next one, and Go opens the file close-on-exec so no dump or
// upload child inherits it. What the lock cannot release by itself is a holder
// that is alive and stuck: past staleAfter (longer than the run's own deadline,
// which should have ended it) the holder is killed, after checking that the pid
// is still a teploy process.
type RunLock struct {
	f *os.File
}

// ErrRunLocked means another run holds the lock.
var ErrRunLocked = errors.New("another `teploy backup run --all` is running")

// AcquireRunLock takes the lock at path. When held it returns ErrRunLocked and
// the holder. killStale says what to do with a holder past staleAfter.
func AcquireRunLock(path string, now time.Time, staleAfter time.Duration) (*RunLock, RunLockHolder, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, RunLockHolder{}, fmt.Errorf("opening the run lock %s: %w", path, err)
	}
	try := func() error { return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) }
	err = try()
	if err != nil {
		holder := readRunLockHolder(path)
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			f.Close()
			return nil, holder, fmt.Errorf("locking %s: %w", path, err)
		}
		if holder.PID <= 0 || holder.StartedAt.IsZero() || now.Sub(holder.StartedAt) <= staleAfter || !isTeployProcess(holder.PID) {
			f.Close()
			return nil, holder, ErrRunLocked
		}
		// Alive, stuck and past every deadline it had: it will never let go.
		if killErr := syscall.Kill(holder.PID, syscall.SIGKILL); killErr != nil {
			f.Close()
			return nil, holder, fmt.Errorf("%w (pid %d has held it since %s and could not be stopped: %v)", ErrRunLocked, holder.PID, holder.StartedAt.Format(time.RFC3339), killErr)
		}
		for i := 0; i < 50; i++ {
			if err = try(); err == nil {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if err != nil {
			f.Close()
			return nil, holder, ErrRunLocked
		}
	}
	if err := f.Truncate(0); err == nil {
		body, _ := json.Marshal(RunLockHolder{PID: os.Getpid(), StartedAt: now})
		f.WriteAt(body, 0)
		f.Sync()
	}
	return &RunLock{f: f}, RunLockHolder{}, nil
}

// Release drops the lock (and exiting does too).
func (l *RunLock) Release() {
	if l == nil || l.f == nil {
		return
	}
	l.f.Truncate(0)
	l.f.Close()
	l.f = nil
}

func readRunLockHolder(path string) RunLockHolder {
	var h RunLockHolder
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &h)
	}
	return h
}

// isTeployProcess guards the kill against a recycled pid.
func isTeployProcess(pid int) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil {
		return false
	}
	return strings.Contains(string(b), "teploy") && strings.Contains(string(b), "--all")
}
