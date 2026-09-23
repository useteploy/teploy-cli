package ssh

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// TestRunDetailed_LocalExecutor pins the structured contract on the one
// executor tests can drive for real: stdout/stderr arrive SEPARATE and
// untrimmed, the exit code is the process's own, and a clean non-zero
// exit is a completed command (Err nil), not a transport failure.
func TestRunDetailed_LocalExecutor(t *testing.T) {
	e := NewLocalExecutor()
	res := RunDetailed(context.Background(), e, "printf out; printf err >&2; exit 42")
	if res.Err != nil {
		t.Fatalf("non-zero exit is not an Err: %v", res.Err)
	}
	if res.ExitCode != 42 {
		t.Fatalf("ExitCode = %d, want 42", res.ExitCode)
	}
	if !bytes.Equal(res.Stdout, []byte("out")) {
		t.Fatalf("Stdout = %q, want \"out\"", res.Stdout)
	}
	if !bytes.Equal(res.Stderr, []byte("err")) {
		t.Fatalf("Stderr = %q, want \"err\"", res.Stderr)
	}
	if res.Truncated || res.Canceled || res.TimedOut {
		t.Fatalf("no flag should be set: %+v", res)
	}
	if !res.Failed() {
		t.Fatal("exit 42 must count as Failed")
	}
	if res.TrimmedStdout() != "out" {
		t.Fatalf("TrimmedStdout = %q", res.TrimmedStdout())
	}
}

func TestRunDetailed_LocalExecutor_Success(t *testing.T) {
	e := NewLocalExecutor()
	res := RunDetailed(context.Background(), e, "echo ok")
	if res.Failed() || res.ExitCode != 0 || res.TrimmedStdout() != "ok" {
		t.Fatalf("success shape: %+v %q", res, res.Stdout)
	}
}

// TestRunDetailed_LocalExecutor_TimedOutVsCanceled pins the flag
// distinction: a context deadline expiry sets TimedOut only, an explicit
// cancel sets Canceled only. Both kill the process group (A28) and
// return rather than hanging.
func TestRunDetailed_LocalExecutor_TimedOutVsCanceled(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns real processes")
	}
	e := NewLocalExecutor()

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	res := RunDetailed(ctx, e, "sleep 5")
	cancel()
	if !res.TimedOut || res.Canceled {
		t.Fatalf("deadline expiry: TimedOut=%v Canceled=%v, want true/false", res.TimedOut, res.Canceled)
	}
	if res.Err == nil || !errors.Is(res.Err, context.DeadlineExceeded) {
		t.Fatalf("Err = %v, want DeadlineExceeded", res.Err)
	}
	if res.ExitCode != -1 {
		t.Fatalf("ExitCode = %d, want -1 (no status delivered)", res.ExitCode)
	}

	ctx2, cancel2 := context.WithCancel(context.Background())
	go func() {
		time.Sleep(300 * time.Millisecond)
		cancel2()
	}()
	res2 := RunDetailed(ctx2, e, "sleep 5")
	cancel2()
	if res2.TimedOut || !res2.Canceled {
		t.Fatalf("explicit cancel: TimedOut=%v Canceled=%v, want false/true", res2.TimedOut, res2.Canceled)
	}
}

// TestRunDetailed_LocalExecutor_Truncated pins the output bound: output
// beyond the limit is dropped and flagged, never buffered.
func TestRunDetailed_LocalExecutor_Truncated(t *testing.T) {
	e := NewLocalExecutor()
	res := runDetailedWithLimit(context.Background(), e, "yes a", nil, 64)
	if !res.Truncated {
		t.Fatal("Truncated must be set when the capture limit is hit")
	}
	if int64(len(res.Stdout)) > 64 {
		t.Fatalf("captured %d bytes, limit was 64", len(res.Stdout))
	}
	if !strings.HasPrefix(string(res.Stdout), "a") {
		t.Fatalf("the FIRST bytes must be kept, got %q", res.Stdout)
	}
}

// TestRunDetailed_LocalExecutor_CancellationKillsGroup extends the A28
// pin to the structured path: canceling mid-run leaves no hidden child
// work — a descendant that would write a marker after the group kill
// must never write it.
func TestRunDetailed_LocalExecutor_CancellationKillsGroup(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns real processes")
	}
	dir := t.TempDir()
	marker := dir + "/alive"
	e := NewLocalExecutor()
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	res := RunDetailed(ctx, e, "sh -c 'sleep 2 && touch "+marker+"' >/dev/null 2>&1; sleep 2")
	if !res.Failed() {
		t.Fatal("canceled run must report failure")
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(marker); os.IsNotExist(err) {
			return // child died with the group — pass
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("a descendant survived cancellation of the structured run")
}

// TestRunInputDetailed_LocalExecutor pins stdin transport plus capture:
// stdin reaches the command, and the failure's stderr is available even
// on the RunInput path (whose legacy contract discards it).
func TestRunInputDetailed_LocalExecutor(t *testing.T) {
	e := NewLocalExecutor()
	res := RunInputDetailed(context.Background(), e, "cat; printf 'boom' >&2; exit 3", strings.NewReader("payload"))
	if res.ExitCode != 3 || res.Err != nil {
		t.Fatalf("status: %+v", res)
	}
	if string(res.Stdout) != "payload" {
		t.Fatalf("stdin did not reach the command: %q", res.Stdout)
	}
	if string(res.Stderr) != "boom" {
		t.Fatalf("stderr lost: %q", res.Stderr)
	}
}

// TestRunDetailed_MockExecutor pins the derivation rules tests rely on:
// a registered error carrying "exit status N" is a command failure with
// that code (Err nil, text in Stderr); an error without one is a
// transport failure (Err set); stdin payloads are recorded for the
// secret-transport assertions.
func TestRunDetailed_MockExecutor(t *testing.T) {
	mock := NewMockExecutor("h",
		MockCommand{Match: "echo fine", Output: "fine"},
		MockCommand{Match: "failing", Err: errors.New("exit status 75: TEPLOY_FENCE_LOST")},
		MockCommand{Match: "transport", Err: errors.New("ssh: connection timed out")},
	)

	res := RunDetailed(context.Background(), mock, "echo fine")
	if res.ExitCode != 0 || res.TrimmedStdout() != "fine" || res.Failed() {
		t.Fatalf("success shape: %+v", res)
	}

	res = RunDetailed(context.Background(), mock, "failing")
	if res.ExitCode != 75 || res.Err != nil {
		t.Fatalf("exit-status error shape: %+v", res)
	}
	if !bytes.Contains(res.Stderr, []byte("TEPLOY_FENCE_LOST")) {
		t.Fatalf("failure text must land in Stderr: %q", res.Stderr)
	}

	res = RunDetailed(context.Background(), mock, "transport")
	if res.Err == nil || res.ExitCode != -1 {
		t.Fatalf("transport shape: %+v", res)
	}

	res = RunInputDetailed(context.Background(), mock, "echo fine", strings.NewReader("s3cret"))
	if res.ExitCode != 0 {
		t.Fatalf("input run: %+v", res)
	}
	if len(mock.Inputs) != 1 || mock.Inputs[0] != "s3cret" {
		t.Fatalf("stdin payload not recorded: %v", mock.Inputs)
	}
}

// TestRunDetailed_FallbackExecutor pins the generic path used by test
// doubles that implement only the base Executor interface: output is
// captured, the exit code is derived when the error carries one, and an
// underivable error stays a transport failure.
func TestRunDetailed_FallbackExecutor(t *testing.T) {
	fb := &fakeFallbackExecutor{output: "hello", err: nil}
	res := RunDetailed(context.Background(), fb, "anything")
	if res.ExitCode != 0 || res.TrimmedStdout() != "hello" {
		t.Fatalf("fallback success: %+v %q", res, res.Stdout)
	}

	// The string shape the base contract produces derives the code.
	fb3 := &fakeFallbackExecutor{output: "x", err: errors.New("exit status 9: nope")}
	res3 := RunDetailed(context.Background(), fb3, "anything")
	if res3.ExitCode != 9 || res3.Err != nil {
		t.Fatalf("fallback exit-status derivation: %+v", res3)
	}

	fb4 := &fakeFallbackExecutor{err: errors.New("ssh: connection lost")}
	res4 := RunDetailed(context.Background(), fb4, "anything")
	if res4.Err == nil || res4.ExitCode != -1 {
		t.Fatalf("fallback transport shape: %+v", res4)
	}
}

type fakeFallbackExecutor struct {
	output string
	err    error
	called int
}

func (f *fakeFallbackExecutor) Run(ctx context.Context, cmd string) (string, error) {
	f.called++
	return f.output, f.err
}

func (f *fakeFallbackExecutor) RunStream(ctx context.Context, cmd string, stdout, stderr io.Writer) error {
	if f.output != "" {
		stdout.Write([]byte(f.output))
	}
	return f.err
}

func (f *fakeFallbackExecutor) RunInput(ctx context.Context, cmd string, stdin io.Reader) error {
	return f.err
}

func (f *fakeFallbackExecutor) Upload(ctx context.Context, content io.Reader, remotePath string, mode string) error {
	return nil
}

func (f *fakeFallbackExecutor) Close() error { return nil }
func (f *fakeFallbackExecutor) Host() string { return "fake" }
func (f *fakeFallbackExecutor) User() string { return "root" }
