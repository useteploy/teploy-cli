// Fenced deploy locks (audit F16).
//
// The pre-F16 lock was an atomic mkdir plus an info file whose only liveness
// signal was a timestamp: a deploy older than staleLockTTL could be broken by
// the next acquire, and a broken holder that was merely SLOW (not dead) kept
// mutating the server afterwards — its docker runs, state writes, and route
// switches landed on top of whatever the new holder was doing. The register
// warned that a fencing redesign can strand apps mid-incident if it ships
// wrong, so the failure modes here are chosen explicitly:
//
//   - Liveness: a holder RENEWS its lock (renew_ts) every renewalInterval;
//     a lock only looks stale after staleLockTTL measured from the LAST
//     renewal. A live-but-slow deploy is therefore never falsely broken —
//     the stranding scenario — while a dead one still self-heals after the
//     same 30-minute window as before.
//   - Safety: every acquire carries a unique owner token. Effect sites
//     verify the token immediately before (and, for single-command effects,
//     in the same shell invocation as) the mutation, so a holder whose lock
//     was broken or replaced has its late writes REFUSED with
//     ErrFenceLost instead of interleaving with the new holder.
//   - Recovery is never fenced: cleanup paths that undo this operation's
//     own effects (restoring displaced containers, rolling the Caddyfile
//     back) still run after fence loss — refusing to CLEAN UP is how a
//     fencing design strands an app mid-incident.
//
// The owner token doubles as the fencing token. A separate monotonic
// counter adds nothing in this topology: the .lock directory on the target
// is the single authority (there is no shared resource that could compare
// token ordering independently), and refusal is exactly the test "does the
// authority still name us", which a unique random token answers.

package state

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/useteploy/teploy/internal/ssh"
)

// ErrFenceLost is returned when the lock this operation holds no longer
// names it on the server — broken as stale and re-acquired, manually
// unlocked, or its info overwritten. Effects must be refused, not retried:
// another operation owns the app now.
var ErrFenceLost = errors.New("deploy lock fence lost — the lock no longer names this operation on the server; refusing to apply further effects")

const (
	// fenceLostMarker is echoed to stderr by the guard fragment below so a
	// refused effect is identifiable from the (transport-wrapped) error.
	fenceLostMarker = "TEPLOY_FENCE_LOST"

	// renewalInterval is how often a held lock is renewed. Three missed
	// renewals fit inside staleLockTTL, so a holder that can still talk to
	// the server never lets its lock look stale.
	renewalInterval = staleLockTTL / 3
)

// Lock is a handle to a held deploy lock: the owner token plus the renewal
// state. Acquire it with AcquireLockFenced, start renewal with StartRenewal,
// check effect sites with Check/Guarded, and release with ReleaseLockFenced.
// A nil *Lock is valid and unfenced: every method is a no-op or plain
// passthrough, which keeps the pre-F16 entry points (and their tests) honest
// about doing no fencing rather than silently faking it.
type Lock struct {
	app   string
	owner string

	mu        sync.Mutex
	lost      bool
	renewer   ssh.Executor
	stopRenew chan struct{}
	renewDone chan struct{}
}

// AcquireLockFenced is AcquireLock returning the fence handle. The lock
// itself is identical (same directory, same info schema plus owner); callers
// that ignore the handle get exactly the old behavior.
func AcquireLockFenced(ctx context.Context, exec ssh.Executor, app string) (*Lock, error) {
	owner := newOperationID()
	if err := acquireAutoLock(ctx, exec, app, owner); err != nil {
		return nil, err
	}
	return &Lock{app: app, owner: owner}, nil
}

func lockInfoPath(app string) string {
	return fmt.Sprintf("%s/%s/.lock/info", deploymentsDir, app)
}

// Owner returns the lock's owner token (the fencing token).
func (l *Lock) Owner() string {
	if l == nil {
		return ""
	}
	return l.owner
}

// App returns the app the lock was taken for.
func (l *Lock) App() string {
	if l == nil {
		return ""
	}
	return l.app
}

// guardFragment is the shell precondition asserting holdership. Composed
// into the SAME command as the effect it guards, so no interleaving window
// exists between the check and the mutation at the transport granularity.
func (l *Lock) guardFragment() string {
	return fmt.Sprintf("grep -q %s %s", ssh.ShellQuote(l.owner), ssh.ShellQuote(lockInfoPath(l.app)))
}

// Check verifies the server still names this operation as the lock holder.
// Any failure — including transport failure, because an unreachable answer
// cannot prove holdership — reports ErrFenceLost. Call before effectful
// phases; for single-command effects prefer Guarded.
func (l *Lock) Check(ctx context.Context, exec ssh.Executor) error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	lost := l.lost
	l.mu.Unlock()
	if lost {
		return ErrFenceLost
	}
	if _, err := exec.Run(ctx, l.guardFragment()); err != nil {
		return fmt.Errorf("%w: lock check failed for %s (%v)", ErrFenceLost, l.app, err)
	}
	return nil
}

// Guarded runs effect as a single remote command prefixed by the holdership
// guard: the mutation only executes if the lock still names this operation.
// A refused effect returns an error wrapping ErrFenceLost; any other error is
// the effect's own.
func (l *Lock) Guarded(ctx context.Context, exec ssh.Executor, effect string) (string, error) {
	if l == nil {
		return exec.Run(ctx, effect)
	}
	cmd := l.guardFragment() + " || { printf '" + fenceLostMarker + `\n' >&2; exit 75; }; ` + effect
	out, err := exec.Run(ctx, cmd)
	if err != nil && fenceLostErr(err) {
		return "", fmt.Errorf("%w: refusing to run effect for %s", ErrFenceLost, l.app)
	}
	return out, err
}

// fenceLostErr reports whether err is the guard refusing the effect (or the
// renewal detecting loss): the stderr marker echoed by the guard fragment,
// or its exit status surfaced by either executor flavor.
func fenceLostErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrFenceLost) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, fenceLostMarker) ||
		strings.Contains(msg, "status 75") ||
		strings.Contains(msg, "exit status 75")
}

// StartRenewal begins background renewal of the lock's liveness timestamp.
// Renewal is what makes the TTL safe: a slow-but-alive deploy keeps its lock
// fresh, so only a genuinely dead holder's lock ever looks stale. Proven
// loss (the server no longer names us) stops renewal and poisons the handle;
// transient renewal failures keep trying — the server-side Check remains the
// authority, and a lock that stops being renewed is eventually broken by
// someone else, at which point our own checks refuse.
func (l *Lock) StartRenewal(exec ssh.Executor) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.stopRenew != nil || l.lost {
		return
	}
	l.renewer = exec
	l.stopRenew = make(chan struct{})
	l.renewDone = make(chan struct{})
	// The channels are passed by value: the loop must never re-read the
	// fields, because StopRenewal nils them before closing (a select on a
	// re-read nil channel sleeps forever).
	stop, done := l.stopRenew, l.renewDone
	go l.renewLoop(stop, done)
}

// StopRenewal stops the background renewer and waits for it to exit.
// Idempotent.
func (l *Lock) StopRenewal() {
	if l == nil {
		return
	}
	l.mu.Lock()
	stop, done := l.stopRenew, l.renewDone
	l.stopRenew, l.renewDone = nil, nil
	l.mu.Unlock()
	if stop != nil {
		close(stop)
		<-done
	}
}

func (l *Lock) renewLoop(stop, done chan struct{}) {
	defer close(done)
	ticker := time.NewTicker(renewalInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			err := l.renew(ctx)
			cancel()
			if err != nil && fenceLostErr(err) {
				// The server proved we are no longer the holder.
				l.mu.Lock()
				l.lost = true
				l.mu.Unlock()
				return
			}
			// Other failures: keep retrying on the next tick. The
			// server-side fence check at the next effect boundary is the
			// authority on whether to proceed.
		}
	}
}

// ownerTempName returns a staging sibling unique to this write: path +
// ".tmp-" + owner + "-" + random. F16's first cut staged to FIXED names
// (state.json.tmp-fence, .lock/info.renew) shared by every generation, so a
// stale holder could upload into the successor's staging path and have the
// successor's own guard pass those stale bytes into authority (audit A06).
// Staging under a random owner-scoped name makes cross-generation clobber
// impossible.
func ownerTempName(path, owner string) (string, error) {
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", fmt.Errorf("generating staging name: %w", err)
	}
	return fmt.Sprintf("%s.tmp-%s-%s", path, owner, hex.EncodeToString(nonce[:])), nil
}

// renew refreshes renew_ts under the holdership guard — a renewal that no
// longer holds must never clobber the new holder's info file. The payload
// is staged to a unique sibling in the APP directory (not inside .lock: a
// stale renewal whose lock directory was already removed must never
// RECREATE it, which Upload's mkdir -p would do — leaving a ghost .lock
// that blocks the next acquire for a full staleLockTTL), and the rename
// that makes it live is the guarded effect, so guard and write cannot
// interleave.
func (l *Lock) renew(ctx context.Context) error {
	l.mu.Lock()
	exec := l.renewer
	l.mu.Unlock()
	if exec == nil {
		return nil
	}
	info := LockInfo{
		Type:    "auto",
		Owner:   l.owner,
		TS:      time.Now().UTC().Format(time.RFC3339),
		RenewTS: time.Now().UTC().Format(time.RFC3339),
	}
	payload, err := json.Marshal(info)
	if err != nil {
		return err
	}
	path := lockInfoPath(l.app)
	tmp, err := ownerTempName(fmt.Sprintf("%s/%s/.lock-info", deploymentsDir, l.app), l.owner)
	if err != nil {
		return err
	}
	if err := exec.Upload(ctx, strings.NewReader(string(payload)), tmp, "0644"); err != nil {
		return fmt.Errorf("renewing lock info: %w", err)
	}
	// No mkdir here: the guard just proved .lock/info exists, so .lock
	// exists; recreating it unconditionally would resurrect a released
	// lock's directory. A lock removed in the grep→mv window makes mv
	// fail — a transient renewal error that is retried on the next tick.
	_, err = l.Guarded(ctx, exec, "mv -f -- "+ssh.ShellQuote(tmp)+" "+ssh.ShellQuote(path))
	if err != nil {
		// The staged file is inert outside .lock; a fence-lost renewal
		// must not leave litter behind it (best-effort, bounded).
		if fenceLostErr(err) {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			exec.Run(cleanupCtx, "rm -f -- "+ssh.ShellQuote(tmp))
			cancel()
		}
		return err
	}
	return nil
}

// ReleaseLockFenced stops renewal and releases the lock — but only when the
// server still names THIS operation as the holder. The release effect runs
// under the holdership guard (audit A04): after a takeover or a manual
// unlock/reacquire, a stale deploy's deferred release used to execute an
// unconditional rm -rf on the lock directory, deleting the SUCCESSOR's lock
// and letting a third operation in. A refused release (ErrFenceLost) is a
// success here — the lock belongs to someone else, and leaving it alone is
// exactly the correct outcome. A nil handle keeps the historical unfenced
// release for callers that never held a fence (admin unlock, pre-F16
// paths).
func ReleaseLockFenced(exec ssh.Executor, lk *Lock, app string) {
	if lk != nil {
		lk.StopRenewal()
		if lk.App() != "" && lk.App() != app {
			// A handle for a different app has no authority over this
			// app's lock; releasing it would be A04's defect with extra
			// steps. Refuse and say so.
			fmt.Fprintf(os.Stderr, "teploy: refusing to release %s's lock with a lease held for %s\n", app, lk.App())
			return
		}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	lockDir := fmt.Sprintf("%s/%s/.lock", deploymentsDir, app)
	_, err := lk.Guarded(ctx, exec, "rm -rf -- "+ssh.ShellQuote(lockDir))
	if err != nil && !fenceLostErr(err) {
		// The guarded release failed ambiguously (transport timeout, for
		// one): the release MAY have completed, and a successor may have
		// acquired the path in the meantime. An unconditional detached
		// release here can delete the SUCCESSOR's lock (audit T02) — but
		// never releasing strands the app for a full staleLockTTL. Resolve
		// the ambiguity with one shell-level conditional: remove the lock
		// only when it still names THIS operation, or when it is already
		// gone. A lock that names someone else is left strictly alone.
		conditional := fmt.Sprintf(
			"if [ -d %s ] && grep -q %s %s 2>/dev/null; then rm -rf -- %s; fi",
			ssh.ShellQuote(lockDir), ssh.ShellQuote(lk.owner), ssh.ShellQuote(lockInfoPath(app)), ssh.ShellQuote(lockDir),
		)
		if _, cerr := exec.Run(ctx, conditional); cerr != nil {
			fmt.Fprintf(os.Stderr, "teploy: could not confirm release of %s's deploy lock: %v (the lock will self-heal after the stale window if abandoned)\n", app, cerr)
		}
	}
	return
}
	ReleaseLockDetached(exec, app)
}

// WriteFenced is Write with the state commit under the fence: the content is
// staged to a unique sibling (no effect — a stale holder cannot clobber the
// successor's staging, A06), and the atomic rename — the instant the new
// state becomes authoritative — runs as a guarded effect. A holder that
// lost the lock commits nothing.
func WriteFenced(ctx context.Context, exec ssh.Executor, app string, s *AppState, lk *Lock) error {
	if lk == nil {
		return Write(ctx, exec, app, s)
	}
	if lk.App() != app {
		return fmt.Errorf("refusing to commit state for %s under a lease held for %s", app, lk.App())
	}
	data, err := prepareState(s)
	if err != nil {
		return err
	}
	path := fmt.Sprintf("%s/%s/state.json", deploymentsDir, app)
	tmpPath, err := ownerTempName(path, lk.Owner())
	if err != nil {
		return err
	}
	if err := exec.Upload(ctx, strings.NewReader(string(data)), tmpPath, "0644"); err != nil {
		return fmt.Errorf("uploading temporary state file: %w", err)
	}
	if _, err := lk.Guarded(ctx, exec, "mv -f -- "+ssh.ShellQuote(tmpPath)+" "+ssh.ShellQuote(path)); err != nil {
		// Leave the temp file for diagnosis; it is inert.
		return err
	}
	return nil
}
