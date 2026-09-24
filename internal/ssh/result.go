package ssh

// result.go — the structured executor result (C08). Executor.Run's
// (trimmedStdout, error) contract folds everything else into an error
// string: callers that needed the exit code, the real stderr, or the
// difference between "the command ran and failed" and "the command never
// completed" had to parse text — "not found", "status 127" — out of a
// generic wrapper, which breaks the moment a tool changes wording or a
// value legitimately contains the phrase. Result carries each fact as a
// field so call sites stop guessing.
//
// RunDetailed/RunInputDetailed are package-level helpers over any
// Executor: native structured capture for RemoteExecutor and
// LocalExecutor, a derived shape for MockExecutor (whose registered
// errors represent combined failure text), and a best-effort fallback
// (stdout captured, exit code -1) for other implementations.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"sync"

	gossh "golang.org/x/crypto/ssh"
)

// DefaultResultLimit bounds stdout and stderr capture in a Result: one
// stream of unbounded output (a runaway log dump) must not take the
// CLI's memory with it. When either stream crosses the limit the tail is
// dropped and Truncated is set — callers that need whole-stream bytes
// must use RunStream into their own writer, not RunDetailed.
const DefaultResultLimit = 1 << 20 // 1 MiB per stream

// Result is the structured outcome of one invocation. Err is nil when
// the command RAN to completion — including a non-zero exit, which is a
// result, not a transport failure. Err is non-nil only when the command
// did not complete: session/transport failure, cancellation, or timeout.
// When Err is non-nil, ExitCode carries no information (-1 unless the
// server happened to deliver a status first).
type Result struct {
	Stdout    []byte
	Stderr    []byte
	ExitCode  int  // 0..255 when known; -1 when unavailable
	TimedOut  bool // the context deadline expired before completion
	Canceled  bool // the context was canceled before completion
	Truncated bool // Stdout or Stderr hit the capture limit
	Err       error
}

// Failed reports whether the invocation ended in any non-success state:
// a non-zero exit, a transport failure, cancellation, or timeout.
func (r Result) Failed() bool {
	return r.Err != nil || r.ExitCode != 0
}

// TrimmedStdout is Stdout with surrounding whitespace removed — the
// shape Executor.Run callers are used to, for code that reads one
// scalar value out of a command.
func (r Result) TrimmedStdout() string {
	return strings.TrimSpace(string(r.Stdout))
}

// ExitErrorText renders the failure reason for display: stderr when the
// command produced some, the transport error otherwise. Empty for
// success and for failures that said nothing.
func (r Result) ExitErrorText() string {
	if len(r.Stderr) > 0 {
		return strings.TrimSpace(string(r.Stderr))
	}
	if r.Err != nil {
		return r.Err.Error()
	}
	return ""
}

// RunDetailed executes cmd and returns its structured outcome. stdin is
// nil. See Result for the field semantics.
func RunDetailed(ctx context.Context, ex Executor, cmd string) Result {
	return runDetailedWithLimit(ctx, ex, cmd, nil, DefaultResultLimit)
}

// RunInputDetailed executes cmd with stdin streamed from stdin (the
// secret-transport contract of Executor.RunInput) while ALSO capturing
// stdout/stderr and the structured status — RunInput's discard-both
// contract hides the diagnostics a failing stdin-fed pipeline needs
// (docker login refusals, su authentication failures).
func RunInputDetailed(ctx context.Context, ex Executor, cmd string, stdin io.Reader) Result {
	return runDetailedWithLimit(ctx, ex, cmd, stdin, DefaultResultLimit)
}

func runDetailedWithLimit(ctx context.Context, ex Executor, cmd string, stdin io.Reader, limit int64) Result {
	switch v := ex.(type) {
	case *RemoteExecutor:
		return v.runDetailed(ctx, cmd, stdin, limit)
	case *LocalExecutor:
		return v.runDetailed(ctx, cmd, stdin, limit)
	case *MockExecutor:
		return v.runDetailed(ctx, cmd, stdin, limit)
	case *ReconnectingExecutor:
		return v.runDetailed(ctx, cmd, stdin, limit)
	default:
		return fallbackDetailed(ctx, ex, cmd, stdin, limit)
	}
}

// fallbackDetailed covers executors without native structured capture
// (test doubles, other Executor implementations). Output is captured
// via RunStream; stdout and stderr are not separated by the base
// contract, so a failure's combined text lands in Stderr, with the exit
// code derived from the error when it carries one. An error with no
// derivable exit status is a transport failure and stays in Err.
func fallbackDetailed(ctx context.Context, ex Executor, cmd string, stdin io.Reader, limit int64) Result {
	res := Result{ExitCode: -1}
	if err := ctx.Err(); err != nil {
		res.Canceled = errors.Is(err, context.Canceled)
		res.TimedOut = errors.Is(err, context.DeadlineExceeded)
		res.Err = err
		return res
	}

	var out, diag limitedBuffer
	out.limit, diag.limit = limit, limit
	runErr := ex.RunStream(ctx, cmd, &out, &diag)
	return finishFallback(runErr, &out, &diag)
}

func finishFallback(runErr error, out, diag *limitedBuffer) Result {
	res := Result{
		Stdout:    out.bytes(),
		Truncated: out.overflow || diag.overflow,
		ExitCode:  -1,
	}
	if runErr == nil {
		res.ExitCode = 0
		return res
	}
	if code, ok := exitCodeFromError(runErr); ok {
		res.ExitCode = code
		// The error text is the command's combined failure output — keep
		// it where stderr-shaped text lives so callers can classify it.
		if len(res.Stderr) == 0 {
			res.Stderr = []byte(runErr.Error())
		}
		return res
	}
	res.Stderr = diag.bytes()
	res.Err = runErr
	return res
}

var exitStatusRe = regexp.MustCompile(`(?:^|: )exit status (\d+)`)

// exitCodeFromError derives an exit code from a failure error:
// *exec.ExitError (local) and *gossh.ExitError (remote) carry it
// structurally; the string form "exit status N" (what Executor.Run's
// wrappers and MockExecutor registrations produce) is parsed as the
// documented mock shape. ok is false when the error represents no exit
// status at all (transport failure, cancellation).
func exitCodeFromError(err error) (int, bool) {
	var execErr *exec.ExitError
	if errors.As(err, &execErr) {
		return execErr.ExitCode(), true
	}
	var sshErr *gossh.ExitError
	if errors.As(err, &sshErr) {
		return sshErr.Waitmsg.ExitStatus(), true
	}
	if m := exitStatusRe.FindStringSubmatch(err.Error()); m != nil {
		var code int
		if _, err := fmt.Sscanf(m[1], "%d", &code); err == nil {
			return code, true
		}
	}
	return -1, false
}

// contextFailureResult classifies a context error into the
// TimedOut/Canceled flags.
func contextFailureResult(ctx context.Context) (Result, bool) {
	err := ctx.Err()
	if err == nil {
		return Result{}, false
	}
	return Result{
		ExitCode: -1,
		Canceled: errors.Is(err, context.Canceled),
		TimedOut: errors.Is(err, context.DeadlineExceeded),
		Err:      err,
	}, true
}

// limitedBuffer keeps the first limit bytes written to it and records
// whether anything was dropped.
type limitedBuffer struct {
	mu       sync.Mutex
	buf      bytes.Buffer
	limit    int64
	overflow bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.limit <= 0 {
		b.limit = DefaultResultLimit
	}
	room := b.limit - int64(b.buf.Len())
	if room <= 0 {
		b.overflow = true
		return len(p), nil // swallow, but report success to the writer
	}
	if int64(len(p)) > room {
		b.overflow = true
		p = p[:room]
	}
	return b.buf.Write(p)
}

func (b *limitedBuffer) bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buf.Bytes()...)
}
