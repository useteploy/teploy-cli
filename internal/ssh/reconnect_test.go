package ssh

// reconnect_test.go — pins the C08 connection-recovery contract:
// transport-class failures redial and retry exactly the dead
// invocation; ran-and-failed commands are never retried; cancellation
// is not a transport failure; partial streamed output is never
// duplicated by a retry; stdin/upload payloads are resent whole.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
)

// fakeExec is a scripted Executor for the recovery pins: every method
// pops its next scripted behavior (falling back to success) and counts
// invocations.
type fakeExec struct {
	host string

	mu       sync.Mutex
	runCalls int
	uploads  []string
	closed   bool

	runBehaviors    []func(string) (string, error)
	streamBehaviors []func(string, io.Writer) error
	inputBehaviors  []func(string, io.Reader) error
	uploadBehaviors []func(string) error
}

func (f *fakeExec) Run(ctx context.Context, cmd string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runCalls++
	if len(f.runBehaviors) == 0 {
		return "", nil
	}
	b := f.runBehaviors[0]
	f.runBehaviors = f.runBehaviors[1:]
	return b(cmd)
}

func (f *fakeExec) RunStream(ctx context.Context, cmd string, stdout, stderr io.Writer) error {
	f.mu.Lock()
	f.runCalls++
	if len(f.streamBehaviors) == 0 {
		f.mu.Unlock()
		return nil
	}
	b := f.streamBehaviors[0]
	f.streamBehaviors = f.streamBehaviors[1:]
	f.mu.Unlock()
	return b(cmd, stdout)
}

func (f *fakeExec) RunInput(ctx context.Context, cmd string, stdin io.Reader) error {
	f.mu.Lock()
	f.runCalls++
	if len(f.inputBehaviors) == 0 {
		f.mu.Unlock()
		_, _ = io.Copy(io.Discard, stdin)
		return nil
	}
	b := f.inputBehaviors[0]
	f.inputBehaviors = f.inputBehaviors[1:]
	f.mu.Unlock()
	return b(cmd, stdin)
}

func (f *fakeExec) Upload(ctx context.Context, content io.Reader, remotePath string, mode string) error {
	f.mu.Lock()
	f.runCalls++
	f.uploads = append(f.uploads, remotePath)
	if len(f.uploadBehaviors) == 0 {
		f.mu.Unlock()
		_, _ = io.Copy(io.Discard, content)
		return nil
	}
	b := f.uploadBehaviors[0]
	f.uploadBehaviors = f.uploadBehaviors[1:]
	f.mu.Unlock()
	return b(remotePath)
}

func (f *fakeExec) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func (f *fakeExec) Host() string { return f.host }
func (f *fakeExec) User() string { return "root" }

var errConnReset = errors.New("ssh: connection reset by peer")

// TestIsTransportError pins the classification everything else depends
// on: only "did not complete" counts; ran-and-failed (any exit-status
// shape) and caller cancellation do not.
func TestIsTransportError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain transport", errConnReset, true},
		{"wrapped transport", fmt.Errorf("run: %w", errConnReset), true},
		{"exit status text", errors.New("exit status 1: boom"), false},
		{"wrapped exit status", fmt.Errorf("command failed: exit status 3: denied"), false},
		{"context canceled", context.Canceled, false},
		{"context deadline", context.DeadlineExceeded, false},
		{"wrapped cancel", fmt.Errorf("session: %w", context.Canceled), false},
	}
	for _, tc := range cases {
		if got := IsTransportError(tc.err); got != tc.want {
			t.Errorf("%s: IsTransportError(%v) = %v, want %v", tc.name, tc.err, got, tc.want)
		}
	}
}

// TestReconnect_RetriesTransportFailureAndRedials pins the core
// recovery: a transport-dead command is retried once per redial, and
// the dead inner connection is closed when replaced.
func TestReconnect_RetriesTransportFailureAndRedials(t *testing.T) {
	first := &fakeExec{
		runBehaviors: []func(string) (string, error){
			func(string) (string, error) { return "", errConnReset },
		},
	}
	second := &fakeExec{
		runBehaviors: []func(string) (string, error){
			func(string) (string, error) { return "ok", nil },
		},
	}
	dials := 0
	dial := func(ctx context.Context) (Executor, error) {
		dials++
		if dials == 1 {
			return first, nil
		}
		return second, nil
	}
	r, err := NewReconnectingExecutor(context.Background(), dial, 2)
	if err != nil {
		t.Fatal(err)
	}
	out, err := r.Run(context.Background(), "whoami")
	if err != nil || out != "ok" {
		t.Fatalf("Run = %q, %v; want ok, nil", out, err)
	}
	if dials != 2 {
		t.Fatalf("dials = %d, want 2", dials)
	}
	if !first.closed {
		t.Error("the dead inner connection must be closed when replaced")
	}
	if second.closed {
		t.Error("the live inner connection must NOT be closed by a recovery")
	}
}

// TestReconnect_RanAndFailedIsNeverRetried pins that a command that
// RAN and exited non-zero is the caller's decision, not the wrapper's.
func TestReconnect_RanAndFailedIsNeverRetried(t *testing.T) {
	inner := &fakeExec{
		runBehaviors: []func(string) (string, error){
			func(string) (string, error) { return "", fmt.Errorf("exit status 3: boom") },
		},
	}
	dials := 0
	dial := func(ctx context.Context) (Executor, error) {
		dials++
		if dials > 1 {
			t.Error("a ran-and-failed command must not trigger a redial")
		}
		return inner, nil
	}
	r, err := NewReconnectingExecutor(context.Background(), dial, 5)
	if err != nil {
		t.Fatal(err)
	}
	_, runErr := r.Run(context.Background(), "false")
	if runErr == nil || !strings.Contains(runErr.Error(), "exit status 3") {
		t.Fatalf("ran-and-failed error must pass through verbatim, got %v", runErr)
	}
	if inner.runCalls != 1 {
		t.Fatalf("invocations = %d, want exactly 1", inner.runCalls)
	}
}

// TestReconnect_BudgetExhaustion pins the bound: after maxReconnects
// redials the failure surfaces naming the budget, with exactly
// maxReconnects+1 invocations total.
func TestReconnect_BudgetExhaustion(t *testing.T) {
	inner := &fakeExec{
		runBehaviors: []func(string) (string, error){
			func(string) (string, error) { return "", errConnReset },
			func(string) (string, error) { return "", errConnReset },
			func(string) (string, error) { return "", errConnReset },
			func(string) (string, error) { return "", errConnReset },
		},
	}
	dials := 0
	dial := func(ctx context.Context) (Executor, error) {
		dials++
		return inner, nil
	}
	r, err := NewReconnectingExecutor(context.Background(), dial, 2)
	if err != nil {
		t.Fatal(err)
	}
	_, runErr := r.Run(context.Background(), "hang")
	if runErr == nil || !strings.Contains(runErr.Error(), "reconnect budget is exhausted") {
		t.Fatalf("budget exhaustion must be named, got %v", runErr)
	}
	if !strings.Contains(runErr.Error(), "connection reset") {
		t.Fatalf("the underlying transport failure must be wrapped, got %v", runErr)
	}
	if inner.runCalls != 3 || dials != 3 {
		t.Fatalf("invocations = %d, dials = %d; want 3 and 3 (initial + 2 retries)", inner.runCalls, dials)
	}
}

// TestReconnect_CancellationIsNotRetried pins that an operator Ctrl-C
// (context cancellation surfaced through the invocation) is respected:
// no redial, exactly one invocation.
func TestReconnect_CancellationIsNotRetried(t *testing.T) {
	inner := &fakeExec{
		runBehaviors: []func(string) (string, error){
			func(string) (string, error) { return "", fmt.Errorf("session: %w", context.Canceled) },
		},
	}
	dials := 0
	dial := func(ctx context.Context) (Executor, error) {
		dials++
		if dials > 1 {
			t.Error("cancellation must not trigger a redial")
		}
		return inner, nil
	}
	r, err := NewReconnectingExecutor(context.Background(), dial, 5)
	if err != nil {
		t.Fatal(err)
	}
	_, runErr := r.Run(context.Background(), "slow")
	if !errors.Is(runErr, context.Canceled) {
		t.Fatalf("cancellation must surface as itself, got %v", runErr)
	}
	if inner.runCalls != 1 {
		t.Fatalf("invocations = %d, want 1", inner.runCalls)
	}
}

// TestReconnect_RunStreamPartialOutputNotDuplicated pins the streamed
// guard: once bytes reached the caller's writer, a transport failure is
// NOT retried (a retry would duplicate the output) and the error names
// the byte count.
func TestReconnect_RunStreamPartialOutputNotDuplicated(t *testing.T) {
	inner := &fakeExec{
		streamBehaviors: []func(string, io.Writer) error{
			func(_ string, stdout io.Writer) error {
				fmt.Fprint(stdout, "partial")
				return errConnReset
			},
			func(_ string, stdout io.Writer) error {
				fmt.Fprint(stdout, "SHOULD-NOT-APPEAR")
				return nil
			},
		},
	}
	dial := func(ctx context.Context) (Executor, error) { return inner, nil }
	r, err := NewReconnectingExecutor(context.Background(), dial, 5)
	if err != nil {
		t.Fatal(err)
	}
	var buf strings.Builder
	streamErr := r.RunStream(context.Background(), "logs", &buf, io.Discard)
	if streamErr == nil || !strings.Contains(streamErr.Error(), "7 bytes") {
		t.Fatalf("partial-stream failure must name the streamed byte count, got %v", streamErr)
	}
	if !strings.Contains(streamErr.Error(), "re-run when ready") {
		t.Fatalf("the failure must tell the operator re-running is the recovery, got %v", streamErr)
	}
	if buf.String() != "partial" {
		t.Fatalf("streamed output = %q, want exactly the partial bytes", buf.String())
	}
	if inner.runCalls != 1 {
		t.Fatalf("a partial-output failure must not be retried, invocations = %d", inner.runCalls)
	}
}

// TestReconnect_RunStreamNoOutputIsRetried pins the other half: a
// transport death BEFORE any output is safely retried, streaming the
// full output exactly once.
func TestReconnect_RunStreamNoOutputIsRetried(t *testing.T) {
	inner := &fakeExec{
		streamBehaviors: []func(string, io.Writer) error{
			func(string, io.Writer) error { return errConnReset },
			func(_ string, stdout io.Writer) error {
				fmt.Fprint(stdout, "full output")
				return nil
			},
		},
	}
	dial := func(ctx context.Context) (Executor, error) { return inner, nil }
	r, err := NewReconnectingExecutor(context.Background(), dial, 1)
	if err != nil {
		t.Fatal(err)
	}
	var buf strings.Builder
	if streamErr := r.RunStream(context.Background(), "logs", &buf, io.Discard); streamErr != nil {
		t.Fatalf("zero-output transport failure must recover: %v", streamErr)
	}
	if buf.String() != "full output" {
		t.Fatalf("streamed output = %q, want exactly one full copy", buf.String())
	}
}

// TestReconnect_RunInputResendsPayloadWhole pins the stdin fidelity
// fix: the first attempt consumed the reader, so the redialed retry
// must still receive the complete payload — not an exhausted reader's
// empty string.
func TestReconnect_RunInputResendsPayloadWhole(t *testing.T) {
	var received []string
	inner := &fakeExec{
		inputBehaviors: []func(string, io.Reader) error{
			func(_ string, stdin io.Reader) error {
				data, _ := io.ReadAll(stdin)
				received = append(received, string(data))
				return errConnReset
			},
			func(_ string, stdin io.Reader) error {
				data, _ := io.ReadAll(stdin)
				received = append(received, string(data))
				return nil
			},
		},
	}
	dial := func(ctx context.Context) (Executor, error) { return inner, nil }
	r, err := NewReconnectingExecutor(context.Background(), dial, 1)
	if err != nil {
		t.Fatal(err)
	}
	if inputErr := r.RunInput(context.Background(), "read-secret", strings.NewReader("root-password\n")); inputErr != nil {
		t.Fatalf("RunInput must recover after a transport death: %v", inputErr)
	}
	if len(received) != 2 || received[0] != "root-password\n" || received[1] != "root-password\n" {
		t.Fatalf("both attempts must receive the full payload, got %q", received)
	}
}

// TestReconnect_UploadRetried pins the upload retry: a transport-dead
// upload is attempted again on the redialed connection (content
// fidelity is pinned separately below via MockExecutor's Files).
func TestReconnect_UploadRetried(t *testing.T) {
	inner := &fakeExec{
		uploadBehaviors: []func(string) error{
			func(string) error { return errConnReset },
			func(string) error { return nil },
		},
	}
	dial := func(ctx context.Context) (Executor, error) { return inner, nil }
	r, err := NewReconnectingExecutor(context.Background(), dial, 1)
	if err != nil {
		t.Fatal(err)
	}
	if upErr := r.Upload(context.Background(), strings.NewReader("payload"), "/tmp/f", "0600"); upErr != nil {
		t.Fatalf("Upload must recover after a transport death: %v", upErr)
	}
	if len(inner.uploads) != 2 || inner.uploads[0] != "/tmp/f" || inner.uploads[1] != "/tmp/f" {
		t.Fatalf("upload paths = %v; want 2 attempts on /tmp/f", inner.uploads)
	}
}

// TestReconnect_UploadResendsContentWhole pins the content fidelity the
// path-count pin above cannot: after a transport death, the redialed
// MockExecutor receives the FULL upload content (Files records what
// actually arrived) — and a second failure plus exhausted budget
// surfaces rather than publishing a partial file.
func TestReconnect_UploadResendsContentWhole(t *testing.T) {
	mock := NewMockExecutor("h",
		MockCommand{Match: "UPLOAD:/deployments/caddy/Caddyfile", Err: errConnReset, Once: true},
	)
	dial := func(ctx context.Context) (Executor, error) { return mock, nil }
	r, err := NewReconnectingExecutor(context.Background(), dial, 1)
	if err != nil {
		t.Fatal(err)
	}
	content := "{\n\tadmin 127.0.0.1:2019\n}\n"
	if upErr := r.Upload(context.Background(), strings.NewReader(content), "/deployments/caddy/Caddyfile", "0644"); upErr != nil {
		t.Fatalf("Upload must recover: %v", upErr)
	}
	if got := string(mock.Files["/deployments/caddy/Caddyfile"]); got != content {
		t.Fatalf("the retried upload must land the FULL content, got %q", got)
	}
}

// TestReconnect_RunInputDetailedKeepsStructuredFields pins the
// structured-result delegation: RunInputDetailed through the wrapper
// returns the inner executor's native fields (exit code, stdout) after
// recovering — the generic fallback would lose the stdout markers
// callers classify (installSudoViaSu's TEPLOY_SUDO_OK).
func TestReconnect_RunInputDetailedKeepsStructuredFields(t *testing.T) {
	mock := NewMockExecutor("h",
		MockCommand{Match: "su -c", Err: errConnReset, Once: true},
		MockCommand{Match: "su -c", Output: "TEPLOY_SUDO_OK\n"},
	)
	dial := func(ctx context.Context) (Executor, error) { return mock, nil }
	r, err := NewReconnectingExecutor(context.Background(), dial, 1)
	if err != nil {
		t.Fatal(err)
	}
	res := RunInputDetailed(context.Background(), r, "su -c 'install-sudo' - root", strings.NewReader("rootpw\n"))
	if res.Err != nil {
		t.Fatalf("recovered invocation must be a success result, got %+v", res)
	}
	if res.ExitCode != 0 || !strings.Contains(string(res.Stdout), "TEPLOY_SUDO_OK") {
		t.Fatalf("structured fields must survive recovery: %+v %q", res, res.Stdout)
	}
	// Both attempts received the password — the retry resent it whole.
	if len(mock.Inputs) != 2 || mock.Inputs[0] != "rootpw\n" || mock.Inputs[1] != "rootpw\n" {
		t.Fatalf("stdin must be resent whole on the retry, inputs: %q", mock.Inputs)
	}
}

// TestReconnect_RunDetailedRanAndFailedNotRetried pins that the
// structured path shares the no-retry rule for completed commands: a
// non-zero exit is a result, not a recovery trigger.
func TestReconnect_RunDetailedRanAndFailedNotRetried(t *testing.T) {
	mock := NewMockExecutor("h",
		MockCommand{Match: "failing", Err: errors.New("exit status 2: nope")},
	)
	dial := func(ctx context.Context) (Executor, error) { return mock, nil }
	r, err := NewReconnectingExecutor(context.Background(), dial, 5)
	if err != nil {
		t.Fatal(err)
	}
	res := RunDetailed(context.Background(), r, "failing")
	if res.Err != nil || res.ExitCode != 2 {
		t.Fatalf("ran-and-failed must surface as a structured result, got %+v", res)
	}
	if len(mock.Calls) != 1 {
		t.Fatalf("a ran-and-failed command must not be retried, calls: %v", mock.Calls)
	}
}

// TestReconnect_ClosePreventsFurtherUse pins lifecycle: after Close,
// invocations fail fast without touching a dead connection, and Close
// is idempotent.
func TestReconnect_ClosePreventsFurtherUse(t *testing.T) {
	inner := &fakeExec{}
	dial := func(ctx context.Context) (Executor, error) { return inner, nil }
	r, err := NewReconnectingExecutor(context.Background(), dial, 3)
	if err != nil {
		t.Fatal(err)
	}
	if closeErr := r.Close(); closeErr != nil {
		t.Fatalf("Close: %v", closeErr)
	}
	if closeErr := r.Close(); closeErr != nil {
		t.Fatalf("Close must be idempotent: %v", closeErr)
	}
	if _, runErr := r.Run(context.Background(), "whoami"); runErr == nil || !strings.Contains(runErr.Error(), "connection closed") {
		t.Fatalf("post-Close Run must fail fast, got %v", runErr)
	}
}
