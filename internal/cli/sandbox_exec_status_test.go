package cli

// sandbox_exec_status_test.go — how `teploy sandbox exec` ends, as the
// status the process exits with: the command's own, or 255 for a failure of
// teploy's own. Never 1 for either. A caller that branches on a command's
// status (make, a test runner, a deploy script) must not read teploy's
// failure as the command having exited with that status, and the other way
// round; 255 is the one status that says "teploy".
//
// The root here is the real one, with only the sandbox group bound to the
// harness (newRootCmd), so what is tested is the path Execute takes short
// of os.Exit itself (runRoot).

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/useteploy/teploy/internal/sandbox"
)

// invoke runs args through the real root and returns the status the process
// would exit with. Output goes to the harness's buffers.
func (h *sandboxHarness) invoke(args ...string) int {
	h.t.Helper()
	root := newRootCmd("test", func(flags *Flags) *cobra.Command { return newSandboxCmdWithEnv(flags, h.env) })
	root.SetArgs(args)
	root.SetOut(h.out)
	root.SetErr(h.errOut)
	root.SilenceUsage = true
	root.SilenceErrors = true
	return runRoot(root, h.errOut)
}

// errorEnvelope is the one document stderr holds under --json after a
// failure: it fails the test if stderr holds anything else.
func (h *sandboxHarness) errorEnvelope(t *testing.T) map[string]any {
	t.Helper()
	trimmed := strings.TrimSpace(h.errOut.String())
	var envelope map[string]any
	if err := json.Unmarshal([]byte(trimmed), &envelope); err != nil {
		t.Fatalf("stderr is not one error envelope: %v\n%s", err, h.errOut.String())
	}
	if envelope["machine_interface"] != float64(MachineInterface) {
		t.Errorf("the envelope is %v", envelope)
	}
	return envelope
}

// A command's exit status is teploy's, whatever it is: 0, and each status a
// script branches on, 2 (which no other teploy command exits with) and 255
// (which teploy's own failures use: the status alone cannot tell them
// apart, the --json classes can).
func TestSandboxExecExitStatusIsTheCommands(t *testing.T) {
	for _, code := range []int{0, 1, 2, 3, 42, 124, 255} {
		h := newInstalledSandboxHarness(t)
		id := withRun(h, "running")
		if status := h.invoke("sandbox", "exec", id, "--host", "prod", "--", "sh", "-c", fmt.Sprintf("echo out %d; exit %d", code, code)); status != code {
			t.Errorf("a command exiting %d made teploy exit %d", code, status)
		}
		if code == 0 && h.errOut.Len() != 0 {
			t.Errorf("a command that exited 0 printed on stderr: %q", h.errOut.String())
		}
	}
	// A command a signal killed is the engine's 128+n, passed on like any
	// other status.
	h := newInstalledSandboxHarness(t)
	id := withRun(h, "running")
	h.engine.setExecHook(func(w http.ResponseWriter, req sandbox.ExecRequest) {
		sandboxSSE(w, req, [2]string{"stdout", "killed"}, [2]string{"exit", `{"exitCode":137,"timedOut":false}`})
	})
	if status := h.invoke("sandbox", "exec", id, "--host", "prod", "--", "true"); status != 137 {
		t.Errorf("a command killed by a signal made teploy exit %d, want 137", status)
	}
}

// Everything that is not the command's status is 255: teploy could not
// reach the engine, the engine refused, the sandbox could not run the
// command, the stream ended before the exit, or the invocation itself was
// wrong. The error's class is whatever it is (the envelope says which).
func TestSandboxExecTeploysOwnFailuresExit255(t *testing.T) {
	stopped := func(h *sandboxHarness) []string { return []string{"exec", withRun(h, "stopped"), "--", "true"} }
	notThere := func(h *sandboxHarness) []string { return []string{"exec", sandboxTestID(9), "--", "true"} }
	unreachable := func(h *sandboxHarness) []string {
		h.link.setTunnel("refused")
		return []string{"exec", withRun(h, "running"), "--", "true"}
	}
	atCapacity := func(h *sandboxHarness) []string {
		h.engine.setExecHook(func(w http.ResponseWriter, req sandbox.ExecRequest) {
			sandboxProblem(w, http.StatusServiceUnavailable, "at-capacity", "the daemon is full")
		})
		return []string{"exec", withRun(h, "running"), "--", "true"}
	}
	couldNotRun := func(h *sandboxHarness) []string {
		h.engine.setExecHook(func(w http.ResponseWriter, req sandbox.ExecRequest) {
			sandboxSSE(w, req, [2]string{"stderr", "sandbox: docker exec failed"}, [2]string{"exit", `{"exitCode":-1,"timedOut":false}`})
		})
		return []string{"exec", withRun(h, "running"), "--", "true"}
	}
	streamDied := func(h *sandboxHarness) []string {
		h.engine.setExecHook(func(w http.ResponseWriter, req sandbox.ExecRequest) { sandboxSSE(w, req, [2]string{"stdout", "half"}) })
		return []string{"exec", withRun(h, "running"), "--", "true"}
	}
	noStatus := func(h *sandboxHarness) []string {
		// A timeout the engine did not give a status for: nothing to pass on.
		h.engine.setExecHook(func(w http.ResponseWriter, req sandbox.ExecRequest) {
			sandboxSSE(w, req, [2]string{"exit", `{"exitCode":0,"timedOut":true}`})
		})
		return []string{"exec", withRun(h, "running"), "--", "true"}
	}
	lostAnswer := func(h *sandboxHarness) []string {
		engineState(h.engine, func() bool { h.engine.loseExecAnswer = true; return true })
		return []string{"exec", withRun(h, "running"), "--", "true"}
	}
	outOfRange := func(h *sandboxHarness) []string {
		h.engine.setExecHook(func(w http.ResponseWriter, req sandbox.ExecRequest) {
			sandboxSSE(w, req, [2]string{"exit", `{"exitCode":300,"timedOut":false}`})
		})
		return []string{"exec", withRun(h, "running"), "--", "true"}
	}
	for _, c := range []struct {
		name string
		args func(h *sandboxHarness) []string
		code string
		// noHost: the case is the missing --host itself (the harness's
		// flags otherwise name the server).
		noHost bool
	}{
		{name: "a sandbox that is not there", args: notThere, code: codeInternal},
		{name: "a stopped sandbox", args: stopped, code: codeConflict},
		{name: "an engine that cannot be reached", args: unreachable, code: codeUnsupported},
		{name: "an engine at capacity", args: atCapacity, code: codeInternal},
		{name: "a sandbox that could not run the command", args: couldNotRun, code: codeInternal},
		{name: "a stream that died before the exit", args: streamDied, code: codeUncertainOutcome},
		{name: "a timeout with no status to pass on", args: noStatus, code: codeInternal},
		{name: "an answer that was lost", args: lostAnswer, code: codeUncertainOutcome},
		{name: "a status no process can exit with", args: outOfRange, code: codeInternal},
		{name: "an id that is not one", args: func(h *sandboxHarness) []string { return []string{"exec", "nope", "--", "true"} }, code: codeInternal},
		{name: "a --timeout under a second", args: func(h *sandboxHarness) []string {
			return []string{"exec", withRun(h, "running"), "--timeout", "500ms", "--", "true"}
		}, code: codeInternal},
		{name: "no --host", args: func(h *sandboxHarness) []string { return []string{"exec", withRun(h, "running"), "--", "true"} }, code: codeInternal, noHost: true},
		{name: "no command after the --", args: func(h *sandboxHarness) []string {
			return []string{"exec", withRun(h, "running"), "--host", "prod", "--"}
		}, code: codeInternal},
		{name: "no -- at all", args: func(h *sandboxHarness) []string { return []string{"exec", withRun(h, "running"), "true"} }, code: codeInternal},
		{name: "a flag the command does not know", args: func(h *sandboxHarness) []string {
			return []string{"exec", "--no-such-flag", withRun(h, "running"), "--", "true"}
		}, code: codeInternal},
	} {
		h := newInstalledSandboxHarness(t)
		if c.noHost {
			h.flags.Host = ""
		}
		err := h.run(c.args(h)...)
		if err == nil {
			t.Errorf("%s: the exec succeeded", c.name)
			continue
		}
		if code := classifyMachineError(err); code != c.code {
			t.Errorf("%s: the class is %s, want %s (%v)", c.name, code, c.code, err)
		}
		h = newInstalledSandboxHarness(t)
		h.flags.Host = sandboxTestHost
		if status := h.invoke(append([]string{"sandbox"}, c.args(h)...)...); status != sandboxExecFailure {
			t.Errorf("%s: teploy exited %d, want %d", c.name, status, sandboxExecFailure)
		}
	}
}

// Under --json a command that exits nonzero is one document on stdout and
// one error envelope on stderr, code command-failed: it is the command that
// failed, not teploy, and the status is the command's.
func TestSandboxExitStatusUnderJSON(t *testing.T) {
	h := newInstalledSandboxHarness(t)
	id := withRun(h, "running")
	if status := h.invoke("sandbox", "exec", "--json", id, "--host", "prod", "--", "sh", "-c", "echo out; exit 3"); status != 3 {
		t.Errorf("teploy exited %d, want the command's 3", status)
	}
	if doc := h.jsonDocument(); doc["exit_code"] != float64(3) || doc["stdout"] != "out\n" {
		t.Errorf("the document is %v", doc)
	}
	envelope := h.errorEnvelope(t)
	if envelope["code"] != "command-failed" || envelope["message"] != "the command exited with status 3" || envelope["detail"] != "exit status 3" {
		t.Errorf("the envelope is %v", envelope)
	}

	// A timeout is the command's outcome too: its status, and the class.
	h = newInstalledSandboxHarness(t)
	id = withRun(h, "running")
	h.engine.setExecHook(func(w http.ResponseWriter, req sandbox.ExecRequest) {
		sandboxSSE(w, req, [2]string{"stdout", "partial"}, [2]string{"exit", `{"exitCode":124,"timedOut":true}`})
	})
	if status := h.invoke("sandbox", "exec", "--json", id, "--host", "prod", "--timeout", "5s", "--", "sleep 9"); status != 124 {
		t.Errorf("a timed-out command made teploy exit %d, want 124", status)
	}
	if doc := h.jsonDocument(); doc["exit_code"] != float64(124) || doc["timed_out"] != true {
		t.Errorf("the document is %v", doc)
	}
	if envelope := h.errorEnvelope(t); envelope["code"] != "command-failed" || envelope["message"] != "the command timed out" {
		t.Errorf("the envelope is %v", envelope)
	}

	// An exec the engine refused: 255, nothing on stdout, the envelope
	// alone on stderr.
	h = newInstalledSandboxHarness(t)
	if status := h.invoke("sandbox", "exec", "--json", sandboxTestID(9), "--host", "prod", "--", "true"); status != sandboxExecFailure {
		t.Errorf("a refused exec made teploy exit %d, want %d", status, sandboxExecFailure)
	}
	if h.out.Len() != 0 {
		t.Errorf("a refused exec printed on stdout: %s", h.out.String())
	}
	if envelope := h.errorEnvelope(t); envelope["code"] != "internal" {
		t.Errorf("the envelope is %v", envelope)
	}

	// No other command's exit moved: a failure is still 1, and 0 is 0.
	h = newInstalledSandboxHarness(t)
	if status := h.invoke("sandbox", "ls"); status != 1 {
		t.Errorf("`ls` without --host exited %d, want 1", status)
	}
	h = newInstalledSandboxHarness(t)
	if status := h.invoke("sandbox", "new", "--host", "prod", "--image", "alpine"); status != 0 {
		t.Errorf("`new` exited %d, want 0", status)
	}
}

// A failure wrapped on its way up keeps its status, and an error that
// carries none is 1: every other command's.
func TestExitStatus(t *testing.T) {
	if exitStatus(errors.New("anything")) != 1 || exitStatus(fmt.Errorf("wrapped: %w", errors.New("anything"))) != 1 {
		t.Error("an error with no status of its own is not 1")
	}
	if got := exitStatus(fmt.Errorf("wrapped: %w", &sandboxCommandError{code: 7, err: errors.New("exit status 7")})); got != 7 {
		t.Errorf("a command's status through a wrap is %d, want 7", got)
	}
	if got := exitStatus(fmt.Errorf("wrapped: %w", &sandboxExecError{err: errors.New("x")})); got != sandboxExecFailure {
		t.Errorf("a failure of exec's own through a wrap is %d, want %d", got, sandboxExecFailure)
	}
}
