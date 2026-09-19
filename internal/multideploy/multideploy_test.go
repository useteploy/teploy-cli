package multideploy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPrefixWriter_SingleLine(t *testing.T) {
	var buf bytes.Buffer
	pw := NewPrefixWriter("[srv1] ", &buf)
	pw.Write([]byte("hello world\n"))
	pw.Flush()

	got := buf.String()
	want := "[srv1] hello world\n"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestPrefixWriter_MultipleLines(t *testing.T) {
	var buf bytes.Buffer
	pw := NewPrefixWriter("[srv1] ", &buf)
	pw.Write([]byte("line1\nline2\nline3\n"))
	pw.Flush()

	got := buf.String()
	want := "[srv1] line1\n[srv1] line2\n[srv1] line3\n"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestPrefixWriter_PartialLine(t *testing.T) {
	var buf bytes.Buffer
	pw := NewPrefixWriter("[srv1] ", &buf)
	pw.Write([]byte("partial"))
	pw.Flush()

	got := buf.String()
	want := "[srv1] partial\n"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestPrefixWriter_SplitWrites(t *testing.T) {
	var buf bytes.Buffer
	pw := NewPrefixWriter("[srv1] ", &buf)
	pw.Write([]byte("hel"))
	pw.Write([]byte("lo\nwor"))
	pw.Write([]byte("ld\n"))
	pw.Flush()

	got := buf.String()
	want := "[srv1] hello\n[srv1] world\n"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestPrefixWriter_EmptyFlush(t *testing.T) {
	var buf bytes.Buffer
	pw := NewPrefixWriter("[srv1] ", &buf)
	if err := pw.Flush(); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != 0 {
		t.Errorf("expected empty buffer after empty flush, got %q", buf.String())
	}
}

func TestParallelDeploy_AllSucceed(t *testing.T) {
	servers := []ServerTarget{
		{Name: "app1", Host: "10.0.0.1"},
		{Name: "app2", Host: "10.0.0.2"},
		{Name: "app3", Host: "10.0.0.3"},
	}

	var out bytes.Buffer
	results := ParallelDeploy(context.Background(), servers, 3, func(ctx context.Context, target ServerTarget, w io.Writer) error {
		fmt.Fprintf(w, "deploying to %s\n", target.Name)
		return nil
	}, &out)

	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}
	for _, r := range results {
		if !r.Success {
			t.Errorf("expected success for %s, got error: %v", r.Server, r.Error)
		}
	}

	output := out.String()
	for _, name := range []string{"app1", "app2", "app3"} {
		if !strings.Contains(output, fmt.Sprintf("[%s] deploying to %s", name, name)) {
			t.Errorf("expected prefixed output for %s in %q", name, output)
		}
	}
}

func TestParallelDeploy_OneFailsSkipsRest(t *testing.T) {
	servers := []ServerTarget{
		{Name: "app1", Host: "10.0.0.1"},
		{Name: "app2", Host: "10.0.0.2"},
		{Name: "app3", Host: "10.0.0.3"},
	}

	var out bytes.Buffer
	// With parallel=1, servers run sequentially. Fail on app2.
	results := ParallelDeploy(context.Background(), servers, 1, func(ctx context.Context, target ServerTarget, w io.Writer) error {
		if target.Name == "app2" {
			return fmt.Errorf("deploy failed on app2")
		}
		fmt.Fprintf(w, "deployed %s\n", target.Name)
		return nil
	}, &out)

	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}

	// app1 should succeed
	if !results[0].Success {
		t.Errorf("app1 should have succeeded")
	}

	// app2 should fail
	if results[1].Success {
		t.Errorf("app2 should have failed")
	}
	if results[1].Error == nil || !strings.Contains(results[1].Error.Error(), "deploy failed on app2") {
		t.Errorf("app2 error should mention failure, got: %v", results[1].Error)
	}

	// app3 should be skipped
	if results[2].Success {
		t.Errorf("app3 should have been skipped")
	}
	if results[2].Error == nil || !strings.Contains(results[2].Error.Error(), "skipped") {
		t.Errorf("app3 should be marked as skipped, got: %v", results[2].Error)
	}
}

// ParallelDeployAll (best-effort, used for rollback) must attempt EVERY server
// even after one fails — no fail-fast skip. This is the M1 fix: a rollback that
// fail-fast-skipped would strand later servers on the new version.
func TestParallelDeployAll_BestEffortAttemptsAll(t *testing.T) {
	servers := []ServerTarget{
		{Name: "app1", Host: "10.0.0.1"},
		{Name: "app2", Host: "10.0.0.2"},
		{Name: "app3", Host: "10.0.0.3"},
	}

	var attempted sync.Map
	var out bytes.Buffer
	// parallel=1 so ordering is deterministic; fail on app2.
	results := ParallelDeployAll(context.Background(), servers, 1, func(ctx context.Context, target ServerTarget, w io.Writer) error {
		attempted.Store(target.Name, true)
		if target.Name == "app2" {
			return fmt.Errorf("rollback failed on app2")
		}
		return nil
	}, &out)

	// app3 must have been ATTEMPTED (not skipped) despite app2 failing.
	if _, ok := attempted.Load("app3"); !ok {
		t.Fatal("app3 was skipped — best-effort must attempt every server")
	}
	if !results[0].Success || results[1].Success || !results[2].Success {
		t.Fatalf("want app1=ok app2=fail app3=ok, got %v/%v/%v",
			results[0].Success, results[1].Success, results[2].Success)
	}
	for _, r := range results {
		if r.Error != nil && strings.Contains(r.Error.Error(), "skipped") {
			t.Fatalf("no server should be skipped in best-effort mode; %s was", r.Server)
		}
	}
}

func TestParallelDeploy_Semaphore(t *testing.T) {
	servers := []ServerTarget{
		{Name: "app1", Host: "10.0.0.1"},
		{Name: "app2", Host: "10.0.0.2"},
		{Name: "app3", Host: "10.0.0.3"},
		{Name: "app4", Host: "10.0.0.4"},
	}

	var maxConcurrent int64
	var current int64

	var out bytes.Buffer
	results := ParallelDeploy(context.Background(), servers, 2, func(ctx context.Context, target ServerTarget, w io.Writer) error {
		n := atomic.AddInt64(&current, 1)
		for {
			old := atomic.LoadInt64(&maxConcurrent)
			if n <= old {
				break
			}
			if atomic.CompareAndSwapInt64(&maxConcurrent, old, n) {
				break
			}
		}
		time.Sleep(10 * time.Millisecond) // simulate work
		atomic.AddInt64(&current, -1)
		return nil
	}, &out)

	for _, r := range results {
		if !r.Success {
			t.Errorf("expected success for %s, got: %v", r.Server, r.Error)
		}
	}

	got := atomic.LoadInt64(&maxConcurrent)
	if got > 2 {
		t.Errorf("max concurrent should be <= 2, got %d", got)
	}
	_ = results
}

func TestParallelDeploy_DefaultParallel(t *testing.T) {
	servers := []ServerTarget{
		{Name: "app1", Host: "10.0.0.1"},
		{Name: "app2", Host: "10.0.0.2"},
	}

	var out bytes.Buffer
	// parallel=0 should default to 1 (sequential)
	results := ParallelDeploy(context.Background(), servers, 0, func(ctx context.Context, target ServerTarget, w io.Writer) error {
		return nil
	}, &out)

	for _, r := range results {
		if !r.Success {
			t.Errorf("expected success for %s", r.Server)
		}
	}
}

func TestParallelDeploy_EmptyServers(t *testing.T) {
	var out bytes.Buffer
	results := ParallelDeploy(context.Background(), nil, 1, func(ctx context.Context, target ServerTarget, w io.Writer) error {
		return nil
	}, &out)

	if len(results) != 0 {
		t.Fatalf("expected 0 results, got %d", len(results))
	}
}

func TestFormatResults(t *testing.T) {
	results := []Result{
		{Server: "app1", Success: true},
		{Server: "app2", Success: false, Error: fmt.Errorf("connection refused")},
		{Server: "app3", Success: false, Error: fmt.Errorf("skipped: previous server failed")},
	}

	output := FormatResults(results)
	if !strings.Contains(output, "1 succeeded") {
		t.Errorf("expected '1 succeeded' in output: %s", output)
	}
	if !strings.Contains(output, "1 failed") {
		t.Errorf("expected '1 failed' in output: %s", output)
	}
	if !strings.Contains(output, "1 skipped") {
		t.Errorf("expected '1 skipped' in output: %s", output)
	}
	if !strings.Contains(output, "app1: success") {
		t.Errorf("expected 'app1: success' in output: %s", output)
	}
	if !strings.Contains(output, "app2: failed") {
		t.Errorf("expected 'app2: failed' in output: %s", output)
	}
	if !strings.Contains(output, "app3: skipped") {
		t.Errorf("expected 'app3: skipped' in output: %s", output)
	}
}

// TestPrefixWriter_ConcurrentWritesAreLineSafe is the A51 regression: two
// concurrent writers into ONE PrefixWriter (stdout+stderr of the same
// command) must not interleave partial lines, and every flushed line
// carries the prefix.
func TestPrefixWriter_ConcurrentWritesAreLineSafe(t *testing.T) {
	var sink lockedBuffer
	pw := NewPrefixWriter("[srv] ", &sink)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				if _, err := pw.Write([]byte("line-of-output\n")); err != nil {
					t.Errorf("write: %v", err)
				}
			}
		}()
	}
	wg.Wait()
	if err := pw.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	for _, line := range strings.Split(strings.TrimSuffix(sink.String(), "\n"), "\n") {
		if line != "[srv] line-of-output" {
			t.Fatalf("interleaved or unprefixed line: %q", line)
		}
	}
	if n := strings.Count(sink.String(), "line-of-output"); n != 8*200 {
		t.Fatalf("lost output: got %d lines want %d", n, 8*200)
	}
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestPrefixWriter_BoundsUn newlineFreeOutput: a newline-free write larger
// than the fragment cap is flushed in capped pieces instead of buffering
// without bound (A51).
func TestPrefixWriter_BoundsNewlineFreeOutput(t *testing.T) {
	var sink bytes.Buffer
	pw := NewPrefixWriter("[srv] ", &sink)
	big := strings.Repeat("x", 200<<10)
	if _, err := pw.Write([]byte(big)); err != nil {
		t.Fatalf("write: %v", err)
	}
	// The buffer must never hold more than the cap.
	pw.mu.Lock()
	buffered := len(pw.buf)
	pw.mu.Unlock()
	if buffered > 64<<10 {
		t.Errorf("partial-line buffer exceeded the cap: %d", buffered)
	}
	if err := pw.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if got := sink.Len(); got < 200<<10 {
		t.Errorf("output was lost: %d bytes written of %d", got, 200<<10)
	}
}

// TestParallelDeploy_CancelledContextSkipsWaitingServers is the A51
// regression: a cancelled fleet operation must not wait for concurrency
// slots or launch callbacks — every server reports a context skip.
func TestParallelDeploy_CancelledContextSkipsWaitingServers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled before the fleet even starts
	var launched atomic.Int32
	targets := []ServerTarget{{Name: "a"}, {Name: "b"}, {Name: "c"}}
	results := ParallelDeploy(ctx, targets, 2, func(ctx context.Context, srv ServerTarget, out io.Writer) error {
		launched.Add(1)
		return nil
	}, io.Discard)
	if got := launched.Load(); got != 0 {
		t.Errorf("a cancelled fleet launched %d callbacks", got)
	}
	for _, r := range results {
		if r.Success || !errors.Is(r.Error, context.Canceled) {
			t.Errorf("server %s should report a context skip, got %+v", r.Server, r)
		}
	}
}
