package ssh

// hostkey.go — trust-on-first-use host-key enrollment (C08). The v0.1.37
// diagnostics (mismatchHint) are the foundation; this adds what
// concurrent first connects need:
//
//   - every verify+enroll runs under an exclusive lock on a lock file
//     beside known_hosts, so two simultaneous first connects serialize
//     instead of racing the write;
//   - the known_hosts database is re-read FRESH under the lock (the old
//     callback captured the database at Connect() time, so a concurrent
//     enrollment was invisible and both processes enrolled the same
//     host, duplicating lines);
//   - enrollment writes temp-then-rename (the old O_APPEND write could
//     leave a torn line if the process died mid-write, which fails
//     knownhosts parsing and locks EVERY future connection out until
//     the file is hand-repaired);
//   - host identity CHANGE is distinguished from RENAME: an unknown
//     host presenting a key already trusted under a DIFFERENT hostname
//     is the same machine, re-addressed (VPN IP rotation, DNS change) —
//     the new name is enrolled with a note. An unknown key for a KNOWN
//     host is an identity change and still fails closed (mismatchHint).
//
// The file format is unchanged — plain knownhosts lines, appended — so
// every older teploy and stock ssh(1) keeps reading what we write.

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"

	gossh "golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// acceptNewHostKeyCallback returns a host key callback that accepts
// unknown host keys (enrolling them in known_hosts) but rejects every
// verification failure that is NOT "host simply unknown": key
// mismatches (any algorithm), revoked keys, and an unreadable or
// malformed known_hosts database all fail closed. Trust-on-first-use
// must mean "unknown host", never "verification was inconvenient".
func acceptNewHostKeyCallback(knownHostsPath string) gossh.HostKeyCallback {
	return func(hostname string, remote net.Addr, key gossh.PublicKey) error {
		return enrollHostKey(knownHostsPath, hostname, remote, key)
	}
}

// enrollHostKey verifies the presented key against the CURRENT contents
// of known_hosts and enrolls unknown hosts — all under the TOFU lock.
func enrollHostKey(knownHostsPath, hostname string, remote net.Addr, key gossh.PublicKey) error {
	// A fresh box may have no ~/.ssh at all; the lock file lives beside
	// known_hosts, so the directory must exist before the lock is taken.
	if err := os.MkdirAll(filepath.Dir(knownHostsPath), 0700); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(knownHostsPath), err)
	}
	unlock, err := lockKnownHosts(knownHostsPath)
	if err != nil {
		return fmt.Errorf("cannot verify host key: locking %s: %w", knownHostsPath, err)
	}
	defer unlock()

	// Fresh read under the lock. A missing known_hosts is the fresh-box
	// case (nothing known, everything enrollable); a file that EXISTS
	// but cannot be parsed fails closed.
	existing, existingErr := loadKnownHosts(knownHostsPath)
	if existingErr != nil {
		return fmt.Errorf("cannot verify host key: reading %s failed: %w", knownHostsPath, existingErr)
	}
	if existing != nil {
		err := existing(hostname, remote, key)
		if err == nil {
			return nil // known and matches
		}
		// Only a genuinely unknown host (empty Want list) may be
		// enrolled. Any non-KeyError (revocation, database problem) and
		// any mismatch against a known host (nonempty Want) is rejected.
		var keyErr *knownhosts.KeyError
		if !errors.As(err, &keyErr) || len(keyErr.Want) != 0 {
			return mismatchHint(hostname, key, err)
		}
	}

	line := knownhosts.Line([]string{knownhosts.Normalize(hostname)}, key)

	// Rename, not change: the presented key is already trusted under
	// another name. Possession of the host key is what TOFU established;
	// re-addressing the same machine is the benign explanation and is
	// enrolled (with a note) rather than presented as a first use.
	if others := hostsKnownUnder(knownHostsPath, key); len(others) > 0 {
		if err := appendKnownHostsLine(knownHostsPath, line); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "teploy: host key for %s matches the key already trusted for %s — treating this as the same server reached by a new address, and enrolling the new name\n",
			hostname, strings.Join(others, ", "))
		return nil
	}

	return appendKnownHostsLine(knownHostsPath, line)
}

// loadKnownHosts builds a verifier from the file's current contents.
// A missing file is (nil, nil) — nothing is known. A file that exists
// but cannot be read or parsed is an error: TOFU must not treat a
// broken database as an empty one.
func loadKnownHosts(knownHostsPath string) (gossh.HostKeyCallback, error) {
	if _, err := os.Stat(knownHostsPath); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	cb, err := knownhosts.New(knownHostsPath)
	if err != nil {
		return nil, err
	}
	return cb, nil
}

// appendKnownHostsLine adds one line to known_hosts atomically:
// existing bytes + the new line are written to a sibling temp file,
// synced, and renamed over the destination. A crash mid-write leaves
// the previous complete file in place — never a torn line — and the
// rename preserves every other process's entries (a plain rewrite of
// only our line would clobber them).
func appendKnownHostsLine(knownHostsPath, line string) error {
	dir := filepath.Dir(knownHostsPath)
	// A fresh box may have no ~/.ssh at all.
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}

	mode := os.FileMode(0644)
	var contents []byte
	if data, err := os.ReadFile(knownHostsPath); err == nil {
		contents = data
		// Preserve an existing file's permissions.
		if fi, statErr := os.Stat(knownHostsPath); statErr == nil {
			mode = fi.Mode().Perm()
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("reading %s: %w", knownHostsPath, err)
	}
	if len(contents) > 0 && !bytes.HasSuffix(contents, []byte("\n")) {
		contents = append(contents, '\n')
	}
	contents = append(contents, []byte(line+"\n")...)

	tmp, err := os.CreateTemp(dir, ".known_hosts.teploy-*")
	if err != nil {
		return fmt.Errorf("recording host key in %s: %w", knownHostsPath, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return fmt.Errorf("recording host key in %s: %w", knownHostsPath, err)
	}
	if _, err := tmp.Write(contents); err != nil {
		tmp.Close()
		return fmt.Errorf("recording host key in %s: %w", knownHostsPath, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("recording host key in %s: %w", knownHostsPath, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("recording host key in %s: %w", knownHostsPath, err)
	}
	if err := os.Rename(tmpName, knownHostsPath); err != nil {
		return fmt.Errorf("recording host key in %s: %w", knownHostsPath, err)
	}
	if d, derr := os.Open(dir); derr == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// hostsKnownUnder scans known_hosts for entries whose KEY matches the
// presented key, returning the hostname fields they are trusted under.
// Hashed entries expose the key material in the line, so key matching
// works across them (the hostname field itself is reported as written).
// An unreadable file yields no matches — the caller's enroll path will
// fail on the write it then attempts.
func hostsKnownUnder(knownHostsPath string, key gossh.PublicKey) []string {
	data, err := os.ReadFile(knownHostsPath)
	if err != nil {
		return nil
	}
	want := base64.StdEncoding.EncodeToString(key.Marshal())
	var hosts []string
	for _, ln := range strings.Split(string(data), "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" || strings.HasPrefix(ln, "#") {
			continue
		}
		fields := strings.Fields(ln)
		if len(fields) < 3 {
			continue
		}
		marker := ""
		hostField := fields[0]
		if strings.HasPrefix(hostField, "@") {
			if len(fields) < 4 {
				continue
			}
			marker, hostField = hostField, fields[1]
			// fields[2] is the key type here; the base64 is fields[3].
			if fields[3] != want {
				continue
			}
		} else if fields[2] != want {
			continue
		}
		if marker == "@revoked" {
			continue // revoked keys are not "trusted under" anything
		}
		hosts = append(hosts, hostField)
	}
	return hosts
}
