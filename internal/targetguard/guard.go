// Package targetguard is C01-1's on-demand target-side critical section:
// a small helper uploaded over SSH and invoked to run ONE protected effect
// under an OS-exclusive flock with generation fencing (programme §101).
//
// Why a helper and not more client-side checks: the existing lock
// serializes ACQUISITION, but the critical section itself spans many SSH
// round-trips — between the check and the effect another client (or a
// stale-breaking race) can interleave. Moving verify-and-effect into one
// process on the target closes that window: the OS releases the lock on
// process death (a killed helper never freezes the app), and the
// generation fence refuses effects prepared against a superseded
// generation (an abandoned owner's stale rollback cannot stop a newer
// generation).
//
// Outcome protocol: the helper always exits 0 and reports on the FIRST
// stdout line (GUARD_OK / GUARD_BUSY / GUARD_FENCED <c> <e> / GUARD_UNFIT
// / GUARD_BADGEN / GUARD_EFFECT_FAILED <n>) because the ssh Executor
// abstraction does not preserve exit codes; the effect's output follows.
package targetguard

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"strings"

	"github.com/useteploy/teploy/internal/ssh"
)

//go:embed guard.sh
var guardScript string

// Typed outcomes. ErrBusy is retryable (someone else holds the guard —
// back off and retry). ErrFenced means the plan is STALE: the target has
// committed a newer generation than this effect was prepared against —
// reconcile against on-target evidence instead of blind-retrying (the D11
// honesty floor). ErrTargetUnfit is fail-closed: either no flock(1) on
// the target, or the target's flock failed the helper's lock-primitive
// self-test (a flock that does not serialize is worse than none) — the
// effect NEVER runs lock-free or under a fictitious lock.
var (
	ErrBusy        = errors.New("target guard is held by another operation (retryable)")
	ErrFenced      = errors.New("target guard fenced the effect: a newer generation is committed on the target — reconcile before retrying")
	ErrTargetUnfit = errors.New("target's flock is missing or failed the serialization self-test; the guarded effect refuses to run")
)

const helperRemote = "/tmp/teploy-guard.sh"

// ensureUploaded installs the helper on the target. An overwrite never
// races a RUNNING helper: the running process already loaded its copy, and
// sh reads the whole script before executing.
func ensureUploaded(ctx context.Context, exec ssh.Executor) error {
	return exec.Upload(ctx, strings.NewReader(guardScript), helperRemote, "0755")
}

// Run executes cmd on the target under the app's guard with generation
// fencing. expectedGeneration is the generation the calling plan was
// prepared against (state.Generation read at plan time); the effect runs
// only while the committed generation is <= expected — a NEWER committed
// generation fences the stale plan out. The effect's combined output is
// returned on GUARD_OK.
func Run(ctx context.Context, exec ssh.Executor, app string, expectedGeneration uint64, cmd string) (string, error) {
	if err := ensureUploaded(ctx, exec); err != nil {
		return "", fmt.Errorf("uploading target guard: %w", err)
	}
	invoke := fmt.Sprintf("sh %s %s %d -- %s", helperRemote, shQuote(app), expectedGeneration, cmd)
	out, runErr := exec.Run(ctx, invoke)
	if runErr != nil {
		return "", fmt.Errorf("invoking target guard: %w", errDetail(runErr, out))
	}
	// The protocol line is the first GUARD_-prefixed line — not necessarily
	// line 1: the target's shell may emit job-control notices first (bash
	// prints "Killed" to stdout when the effect is SIGKILLed, observed on
	// CI runners 2026-09-23).
	lines := strings.Split(out, "\n")
	head := ""
	headIdx := -1
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "GUARD_") {
			head = strings.TrimSpace(line)
			headIdx = i
			break
		}
	}
	if headIdx == -1 {
		return "", fmt.Errorf("target guard protocol violation (no GUARD_ line in %q)", firstLine(out))
	}
	rest := strings.TrimRight(strings.Join(lines[headIdx+1:], "\n"), "\n")
	fields := strings.Fields(head)
	switch {
	case fields[0] == "GUARD_OK":
		return rest, nil
	case fields[0] == "GUARD_BUSY":
		return "", ErrBusy
	case fields[0] == "GUARD_FENCED":
		return "", fmt.Errorf("%w (committed %s > expected %s)", ErrFenced, orDash(field(fields, 1)), orDash(field(fields, 2)))
	case fields[0] == "GUARD_UNFIT":
		return "", ErrTargetUnfit
	case fields[0] == "GUARD_BADGEN":
		return "", fmt.Errorf("target guard: generation sidecar unreadable for app %s", app)
	case fields[0] == "GUARD_EFFECT_FAILED":
		return "", fmt.Errorf("guarded effect failed (exit %s): %s", orDash(field(fields, 1)), rest)
	default:
		return "", fmt.Errorf("target guard protocol violation (first line %q)", head)
	}
}

func field(fields []string, i int) string {
	if i < len(fields) {
		return fields[i]
	}
	return ""
}

func orDash(s string) string {
	if s == "" {
		return "?"
	}
	return s
}

func errDetail(err error, out string) error {
	if t := strings.TrimSpace(out); t != "" {
		return fmt.Errorf("%w: %s", err, t)
	}
	return err
}

func firstLine(out string) string {
	if i := strings.IndexByte(out, '\n'); i >= 0 {
		return out[:i]
	}
	return out
}

// shQuote quotes one word for the POSIX shell the helper runs under.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
