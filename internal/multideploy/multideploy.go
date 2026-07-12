package multideploy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"
)

// ServerTarget describes a server to deploy to.
type ServerTarget struct {
	Name string
	Host string
	User string
	Key  string
	Role string
	Tags map[string]string // per-host env vars
}

// Result tracks the outcome of a single-server deploy.
type Result struct {
	Server  string
	Success bool
	Error   error
}

// PrefixWriter wraps a writer to prefix each line with a server name.
type PrefixWriter struct {
	prefix string
	w      io.Writer
	buf    []byte // partial line buffer
}

// NewPrefixWriter creates a writer that prefixes each line with the given string.
func NewPrefixWriter(prefix string, w io.Writer) *PrefixWriter {
	return &PrefixWriter{
		prefix: prefix,
		w:      w,
	}
}

func (pw *PrefixWriter) Write(p []byte) (n int, err error) {
	pw.buf = append(pw.buf, p...)
	for {
		idx := bytes.IndexByte(pw.buf, '\n')
		if idx < 0 {
			break
		}
		line := pw.buf[:idx+1]
		_, err = fmt.Fprintf(pw.w, "%s%s", pw.prefix, line)
		if err != nil {
			return len(p), err
		}
		pw.buf = pw.buf[idx+1:]
	}
	return len(p), nil
}

// Flush writes any remaining partial line in the buffer.
func (pw *PrefixWriter) Flush() error {
	if len(pw.buf) > 0 {
		_, err := fmt.Fprintf(pw.w, "%s%s\n", pw.prefix, pw.buf)
		pw.buf = nil
		return err
	}
	return nil
}

// syncWriter wraps an io.Writer with a mutex for concurrent safety.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (sw *syncWriter) Write(p []byte) (n int, err error) {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	return sw.w.Write(p)
}

// DeployFunc is the function signature for deploying to a single server.
// The io.Writer should be used for all output so it can be prefixed.
type DeployFunc func(ctx context.Context, target ServerTarget, out io.Writer) error

// ParallelDeploy runs deploys across multiple servers with concurrency control.
//
// Servers are deployed in batches controlled by the parallel parameter.
// If any server in a batch fails, subsequent servers that haven't started
// are skipped and marked as such (fail-fast) — the right behavior for a
// forward deploy, where continuing to roll a bad release to more servers is
// pointless.
func ParallelDeploy(ctx context.Context, servers []ServerTarget, parallel int, deployFn DeployFunc, out io.Writer) []Result {
	return parallelDeploy(ctx, servers, parallel, true, deployFn, out)
}

// ParallelDeployAll is like ParallelDeploy but best-effort: it attempts EVERY
// server regardless of other servers' failures (no fail-fast skip). Use it for
// rollback — skipping a server's rollback because a *different* server's
// rollback failed would strand that server on the new version, leaving the
// fleet split across two versions with no reconciliation (the exact desync a
// rollback exists to avoid).
func ParallelDeployAll(ctx context.Context, servers []ServerTarget, parallel int, deployFn DeployFunc, out io.Writer) []Result {
	return parallelDeploy(ctx, servers, parallel, false, deployFn, out)
}

func parallelDeploy(ctx context.Context, servers []ServerTarget, parallel int, failFast bool, deployFn DeployFunc, out io.Writer) []Result {
	if parallel <= 0 {
		parallel = 1
	}

	results := make([]Result, len(servers))
	sem := make(chan struct{}, parallel)
	sw := &syncWriter{w: out}
	var wg sync.WaitGroup
	var mu sync.Mutex
	failed := false

	for i, server := range servers {
		wg.Add(1)
		sem <- struct{}{} // acquire semaphore (blocks until a slot is free)

		// Fail-fast: if a previous deploy failed, skip the rest. Disabled for
		// best-effort (rollback) runs, which must attempt every server.
		if failFast {
			mu.Lock()
			if failed {
				mu.Unlock()
				<-sem
				wg.Done()
				results[i] = Result{
					Server:  server.Name,
					Success: false,
					Error:   fmt.Errorf("skipped: previous server failed"),
				}
				continue
			}
			mu.Unlock()
		}

		go func(idx int, srv ServerTarget) {
			defer wg.Done()

			pw := NewPrefixWriter(fmt.Sprintf("[%s] ", srv.Name), sw)
			err := deployFn(ctx, srv, pw)
			pw.Flush()

			results[idx] = Result{
				Server:  srv.Name,
				Success: err == nil,
				Error:   err,
			}

			if err != nil {
				mu.Lock()
				failed = true
				mu.Unlock()
			}

			<-sem // release semaphore after setting failed flag
		}(i, server)
	}

	wg.Wait()
	return results
}

// FormatResults returns a human-readable summary of deploy results.
func FormatResults(results []Result) string {
	var buf bytes.Buffer
	var succeeded, failedCount, skipped int

	for _, r := range results {
		if r.Success {
			succeeded++
			fmt.Fprintf(&buf, "  %s: success\n", r.Server)
		} else if r.Error != nil && r.Error.Error() == "skipped: previous server failed" {
			skipped++
			fmt.Fprintf(&buf, "  %s: skipped\n", r.Server)
		} else {
			failedCount++
			fmt.Fprintf(&buf, "  %s: failed (%v)\n", r.Server, r.Error)
		}
	}

	summary := fmt.Sprintf("\nDeploy summary: %d succeeded, %d failed, %d skipped\n",
		succeeded, failedCount, skipped)
	return summary + buf.String()
}
