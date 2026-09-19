// Attempt-scoped immutable deploy artifacts (audit F08).
//
// Before F08, the three artifacts a deploy attempt generates were written to
// SHARED per-app paths — the build context at /deployments/<app>/build
// (rsynced before the app lock was ever taken), the resolved env file at
// /deployments/<app>/.deploy-env, and the TLS cert/key at
// /deployments/caddy/tls/<app>.crt — so two attempts of the same app (a
// racing deploy, a `teploy build` during a deploy) interleaved writes, and
// the F14 record's env-file reference named a path a LATER attempt would
// overwrite.
//
// F08 keys them per release-attempt in the releasemeta namespace:
//
//   - build context: /deployments/<app>/meta/att/<hash>.<id>/build
//   - env file:      /deployments/<app>/meta/att/<hash>.<id>/env
//   - TLS cert/key:  /deployments/caddy/tls/att/<hash>.<id>/<app>.{crt,key}
//
// Each attempt gets a fresh random id, so its paths are written exactly once
// and never rewritten by anyone — concurrent attempts cannot interleave
// because they cannot collide. The deploy lease (F16's fenced lock, now
// acquired BEFORE artifact generation on the terminal path) serializes
// attempts on top of that.
//
// TLS deliberately stays under /deployments/caddy/tls rather than moving
// into meta/: the caddy container mounts that directory at /etc/caddy (the
// only mount a server that can do custom TLS provably has), so attempt
// scoping there changes no mount topology. Container-side path:
// /etc/caddy/tls/att/<hash>.<id>/<app>.crt.
//
// Retention: attempt dirs are dead weight once their release is outside the
// rollback window (env is baked into the container at create; recreate uses
// the inspect-derived resolved env, never the file). PruneAttempts removes
// attempt dirs whose release hash is not protected — the same window
// keep_versions pruning honors — and fails closed on entries it cannot
// parse, like pin pruning (F78).

package releasemeta

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"context"

	"github.com/useteploy/teploy/internal/ssh"
)

// attemptIDLen is the random suffix length (16 hex chars).
const attemptIDLen = 16

// attemptDirRE matches an attempt directory name: <release hash>.<id>. The
// hash part reuses validHash's grammar; unparsable names are NEVER pruned.
var attemptDirRE = regexp.MustCompile(`^([A-Za-z0-9_][A-Za-z0-9._-]{0,127})\.([0-9a-f]{16})$`)

// Attempt identifies one deploy attempt of one release: (app, hash) plus a
// random id that makes every artifact path this attempt writes unique and
// therefore write-once.
type Attempt struct {
	App  string
	Hash string
	ID   string
}

// NewAttempt mints an attempt for (app, hash).
func NewAttempt(app, hash string) (Attempt, error) {
	if app == "" {
		return Attempt{}, fmt.Errorf("attempt requires an app")
	}
	if !validHash.MatchString(hash) {
		return Attempt{}, fmt.Errorf("invalid release id %q for app %q", hash, app)
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return Attempt{}, fmt.Errorf("generating attempt id: %w", err)
	}
	return Attempt{App: app, Hash: hash, ID: hex.EncodeToString(b[:])}, nil
}

// MustAttempt is NewAttempt for call sites that validated app/hash already
// (deploy flows validate the version grammar before this point).
func MustAttempt(app, hash string) Attempt {
	a, err := NewAttempt(app, hash)
	if err != nil {
		panic(fmt.Sprintf("releasemeta: invalid attempt %s@%s: %v", app, hash, err))
	}
	return a
}

// Name is the attempt's directory name under the att/ namespace.
func (a Attempt) Name() string { return a.Hash + "." + a.ID }

// Dir is the attempt's artifact root: /deployments/<app>/meta/att/<hash>.<id>
func (a Attempt) Dir() string {
	return fmt.Sprintf("%s/%s/meta/att/%s", deploymentsDir, a.App, a.Name())
}

// BuildDir is where the attempt's rsync'd build context lives.
func (a Attempt) BuildDir() string { return a.Dir() + "/build" }

// EnvFile is the attempt's resolved container env file (docker --env-file).
func (a Attempt) EnvFile() string { return a.Dir() + "/env" }

// TLSDir is the attempt's TLS directory on the HOST. It sits under
// /deployments/caddy/tls (mounted at /etc/caddy in the caddy container) —
// see the package doc for why not under meta/.
func (a Attempt) TLSDir() string {
	return fmt.Sprintf("/deployments/caddy/tls/att/%s", a.Name())
}

// TLSCertPath / TLSKeyPath are the attempt's cert/key as seen INSIDE the
// caddy container (what the Caddyfile site block references): the host's
// /deployments/caddy/tls/att/... is mounted at /etc/caddy.
func (a Attempt) TLSCertPath() string { return "/etc/caddy/tls/att/" + a.Name() + "/" + a.App + ".crt" }
func (a Attempt) TLSKeyPath() string  { return "/etc/caddy/tls/att/" + a.Name() + "/" + a.App + ".key" }

// tlsAttemptRoot is the host root holding per-attempt TLS directories.
const tlsAttemptRoot = "/deployments/caddy/tls/att"

// attemptRoot is the host root holding per-attempt artifact directories.
func attemptRoot(app string) string {
	return fmt.Sprintf("%s/%s/meta/att", deploymentsDir, app)
}

// listAttempts lists attempt directory names under root ("" when the
// directory does not exist yet — a first deploy).
func listAttempts(ctx context.Context, exec ssh.Executor, root string) ([]string, error) {
	out, err := exec.Run(ctx, "ls -1 "+root+" 2>/dev/null || true")
	if err != nil {
		return nil, fmt.Errorf("listing attempts under %s: %w", root, err)
	}
	var names []string
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			names = append(names, l)
		}
	}
	return names, nil
}

// PruneAttempts removes the attempt directories (artifact root and TLS
// root) of every release hash NOT in keepHashes. Entries whose names do not
// parse as <hash>.<id> are kept — an unparsable name is not proof the
// attempt is prunable (F78's rule). Removal failures are returned; callers
// treat pruning as best-effort.
func PruneAttempts(ctx context.Context, exec ssh.Executor, app string, keepHashes ...string) error {
	keep := make(map[string]bool, len(keepHashes))
	for _, h := range keepHashes {
		if h != "" {
			keep[h] = true
		}
	}
	var failures []string
	for _, root := range []string{attemptRoot(app), tlsAttemptRoot} {
		names, err := listAttempts(ctx, exec, root)
		if err != nil {
			return err
		}
		for _, name := range names {
			m := attemptDirRE.FindStringSubmatch(name)
			if m == nil || keep[m[1]] {
				continue
			}
			if _, rmErr := exec.Run(ctx, "rm -rf "+ssh.ShellQuote(root+"/"+name)); rmErr != nil {
				failures = append(failures, root+"/"+name)
			}
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("could not remove %d pruned attempt dir(s): %s", len(failures), strings.Join(failures, ", "))
	}
	return nil
}

// PreviousAttemptBuildDir returns the build directory of the most recent
// OTHER attempt (any release hash) — the rsync --link-dest basis, so an
// attempt-scoped build context still transfers incrementally and shares
// unchanged files by hardlink instead of copying them. Empty when there is
// none (first attempt). Best-effort by contract: any failure simply means a
// full transfer.
func PreviousAttemptBuildDir(ctx context.Context, exec ssh.Executor, app, excludeID string) string {
	names, err := listAttempts(ctx, exec, attemptRoot(app))
	if err != nil {
		return ""
	}
	filtered := names[:0]
	for _, n := range names {
		if m := attemptDirRE.FindStringSubmatch(n); m != nil && m[2] != excludeID {
			filtered = append(filtered, n)
		}
	}
	if len(filtered) == 0 {
		return ""
	}
	// Deterministic pick: lexicographically greatest name. The id is
	// random, so this is not chronology — it does not need to be; any
	// recent-ish basis gives rsync its delta.
	sort.Strings(filtered)
	return attemptRoot(app) + "/" + filtered[len(filtered)-1] + "/build"
}
