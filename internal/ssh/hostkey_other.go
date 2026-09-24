//go:build !unix

package ssh

import (
	"os"
	"time"
)

// lockKnownHosts is the non-Unix fallback: no flock exists, so mutual
// exclusion is approximated with O_CREATE|O_EXCL on a lock file, with a
// stale-lock takeover after a bounded wait (a crashed holder must not
// wedge enrollment forever). Best-effort by design — the resident
// deployment mode targets Linux, where the flock implementation runs;
// this keeps the build honest about what it can guarantee.
func lockKnownHosts(knownHostsPath string) (func(), error) {
	lockPath := knownHostsPath + ".teploy-lock"
	deadline := time.Now().Add(5 * time.Second)
	tookOver := false
	for {
		f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err == nil {
			return func() {
				f.Close()
				os.Remove(lockPath)
			}, nil
		}
		if !os.IsExist(err) {
			return nil, err
		}
		if time.Now().After(deadline) {
			if tookOver {
				return nil, os.ErrDeadlineExceeded
			}
			// Stale-lock takeover, once — then fail rather than spin.
			os.Remove(lockPath)
			tookOver = true
			deadline = time.Now().Add(5 * time.Second)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
