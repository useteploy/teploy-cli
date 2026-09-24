package ssh

// reconnect.go — bounded connection recovery for long multi-stage remote
// flows (C08: an interrupted setup resumes safely). A transport failure
// mid-flow (SSH channel death, connection reset) used to abort the whole
// run at whatever stage it hit; re-running was safe only because every
// stage happened to be idempotent, and the operator had to notice and
// do it.
//
// ReconnectingExecutor wraps a dial function: on a TRANSPORT-class
// failure (the command did not complete — no exit status, not a caller
// cancellation) it redials, bounded by a reconnect budget with backoff,
// and retries the single invocation that died. Commands that RAN and
// failed (any exit status) are never retried here — that decision
// belongs to the caller, who knows the command's semantics.
//
// Retrying requires every command sent through the wrapper to be safe
// to re-run; setup's commands are check-then-act idempotent (mkdir -p,
// network create-if-missing, container run-if-absent, atomic uploads),
// and hardening's are documented idempotent. Do not wrap callers whose
// commands are not.
//
// Input fidelity on retry: RunInput and Upload buffer their payload so
// a redialed attempt resends it whole (the first attempt consumed the
// reader); RunStream refuses to retry once any output has been streamed
// — a retry would duplicate it — and surfaces the byte count instead.
// This wrapper is for control-plane flows; do not route bulk transfers
// through it.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// IsTransportError reports whether err means "the invocation did not
// complete" — connection/channel failure — as opposed to "the command
// ran and exited non-zero". Caller cancellation and deadlines are not
// transport failures: the operator asked to stop, and retrying would
// ignore them.
func IsTransportError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if _, ok := exitCodeFromError(err); ok {
		return false
	}
	return true
}

// ReconnectingExecutor is an Executor that survives transport failures
// by redialing through the provided dial function.
type ReconnectingExecutor struct {
	dial func(ctx context.Context) (Executor, error)

	mu             sync.Mutex
	inner          Executor
	reconnectsLeft int
	closed         bool
}

// NewReconnectingExecutor dials once and returns a wrapper that will
// redial at most maxReconnects further times across the wrapper's
// lifetime.
func NewReconnectingExecutor(ctx context.Context, dial func(ctx context.Context) (Executor, error), maxReconnects int) (*ReconnectingExecutor, error) {
	inner, err := dial(ctx)
	if err != nil {
		return nil, fmt.Errorf("connecting: %w", err)
	}
	return &ReconnectingExecutor{dial: dial, inner: inner, reconnectsLeft: maxReconnects}, nil
}

// recover runs invoke against the current inner executor, redialing and
// retrying once per transport failure while the reconnect budget lasts.
// mayRetry, when non-nil, is consulted before each redial: a false
// return surfaces the failure instead of retrying it (RunStream uses
// this to refuse retrying after partial output). Backoff: 500ms, 1s,
// then 2s. The mutex is NOT held across invoke or the backoff sleep —
// only around state transitions.
func (r *ReconnectingExecutor) recover(ctx context.Context, invoke func(Executor) error, mayRetry func() bool) error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return fmt.Errorf("connection closed")
	}
	inner := r.inner
	r.mu.Unlock()

	err := invoke(inner)
	backoff := 500 * time.Millisecond
	for IsTransportError(err) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if mayRetry != nil && !mayRetry() {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > 2*time.Second {
			backoff = 2 * time.Second
		}

		r.mu.Lock()
		if r.closed {
			r.mu.Unlock()
			return fmt.Errorf("connection closed")
		}
		if r.reconnectsLeft <= 0 {
			r.mu.Unlock()
			return fmt.Errorf("connection lost and the reconnect budget is exhausted: %w", err)
		}
		r.reconnectsLeft--
		r.mu.Unlock()

		next, derr := r.dial(ctx)
		if derr != nil {
			err = fmt.Errorf("reconnecting after transport failure (%v): %w", err, derr)
			continue
		}
		r.mu.Lock()
		if r.closed {
			r.mu.Unlock()
			next.Close()
			return fmt.Errorf("connection closed")
		}
		old := r.inner
		r.inner = next
		r.mu.Unlock()
		_ = old.Close() // already dead; best effort
		inner = next
		err = invoke(inner)
	}
	return err
}

func (r *ReconnectingExecutor) Run(ctx context.Context, cmd string) (string, error) {
	var out string
	err := r.recover(ctx, func(ex Executor) error {
		var invokeErr error
		out, invokeErr = ex.Run(ctx, cmd)
		return invokeErr
	}, nil)
	if err != nil {
		return "", err
	}
	return out, nil
}

func (r *ReconnectingExecutor) RunStream(ctx context.Context, cmd string, stdout, stderr io.Writer) error {
	// A retry would re-run the command from its start; if any output
	// already streamed to the caller, a retry would DUPLICATE it. Only
	// retry when nothing was written yet — otherwise surface the
	// failure with the byte count and let the operator decide to re-run.
	guard := &countingWriter{w: stdout}
	err := r.recover(ctx, func(ex Executor) error {
		guard.mu.Lock()
		guard.n = 0
		guard.mu.Unlock()
		return ex.RunStream(ctx, cmd, guard, stderr)
	}, func() bool {
		guard.mu.Lock()
		defer guard.mu.Unlock()
		return guard.n == 0
	})
	if err == nil || !IsTransportError(err) {
		return err
	}
	guard.mu.Lock()
	wrote := guard.n
	guard.mu.Unlock()
	if wrote == 0 {
		return err // no output yet — safe for the caller to re-run
	}
	return fmt.Errorf("connection lost after %d bytes of streamed output — the command may have partially run; re-run when ready: %w", wrote, err)
}

type countingWriter struct {
	w  io.Writer
	mu sync.Mutex
	n  int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.mu.Lock()
	c.n += int64(n)
	c.mu.Unlock()
	return n, err
}

func (r *ReconnectingExecutor) RunInput(ctx context.Context, cmd string, stdin io.Reader) error {
	// Buffer the payload so a redialed attempt resends it whole: the
	// first attempt consumed the reader, and a retry fed an exhausted
	// one would send an empty or partial secret (C08 secret transport).
	payload, err := io.ReadAll(stdin)
	if err != nil {
		return fmt.Errorf("reading stdin payload: %w", err)
	}
	return r.recover(ctx, func(ex Executor) error {
		return ex.RunInput(ctx, cmd, bytes.NewReader(payload))
	}, nil)
}

func (r *ReconnectingExecutor) Upload(ctx context.Context, content io.Reader, remotePath string, mode string) error {
	// Same buffering contract as RunInput: Upload is atomic end-to-end
	// (private temp + rename) on both real executors, so a retried
	// upload publishes exactly one complete file — provided the retry
	// still has the content.
	payload, err := io.ReadAll(content)
	if err != nil {
		return fmt.Errorf("reading upload content: %w", err)
	}
	return r.recover(ctx, func(ex Executor) error {
		return ex.Upload(ctx, bytes.NewReader(payload), remotePath, mode)
	}, nil)
}

// runDetailed delegates to the inner executor's structured capture and
// retries transport-class failures (res.Err set, no exit status): a
// ran-and-failed command (res.Err nil, any exit code) is a result and
// is never retried. Registered in runDetailedWithLimit's type switch so
// RunInputDetailed keeps full fidelity through the wrapper — the
// fallback path would lose the stdout markers callers classify (the su
// path's TEPLOY_SUDO_OK, C08).
func (r *ReconnectingExecutor) runDetailed(ctx context.Context, cmd string, stdin io.Reader, limit int64) Result {
	var payload []byte
	if stdin != nil {
		var err error
		payload, err = io.ReadAll(stdin)
		if err != nil {
			return Result{ExitCode: -1, Err: fmt.Errorf("reading stdin payload: %w", err)}
		}
	}
	var res Result
	rerr := r.recover(ctx, func(ex Executor) error {
		var in io.Reader
		if stdin != nil {
			in = bytes.NewReader(payload)
		}
		res = runDetailedWithLimit(ctx, ex, cmd, in, limit)
		return res.Err
	}, nil)
	if rerr != nil {
		res.Err = rerr
	}
	return res
}

func (r *ReconnectingExecutor) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	return r.inner.Close()
}

func (r *ReconnectingExecutor) Host() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.inner.Host()
}

func (r *ReconnectingExecutor) User() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.inner.User()
}

// Compile-time check: the wrapper is itself an Executor.
var _ Executor = (*ReconnectingExecutor)(nil)
