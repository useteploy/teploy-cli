//go:build unix

package ssh

import (
	"os"
	"syscall"
)

// lockKnownHosts takes an exclusive advisory lock on a lock file beside
// known_hosts, serializing concurrent TOFU enrollments (C08): two
// simultaneous first connects must not race the verify+enroll window.
// flock is used rather than locking known_hosts itself so stock ssh(1)
// — which takes no locks — never interacts with ours. The lock file is
// never deleted after use: unlink-races (two processes each holding a
// lock on an unlinked inode) are exactly the corruption vector the lock
// exists to close.
func lockKnownHosts(knownHostsPath string) (func(), error) {
	f, err := os.OpenFile(knownHostsPath+".teploy-lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	fd := int(f.Fd())
	return func() {
		_ = syscall.Flock(fd, syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
