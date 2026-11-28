package cli

// sandbox_commands_test.go — `ls`, `new` and `exec` against the engine: what
// they send, what they print, and their --json documents.

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/sandbox"
)

func TestSandboxList(t *testing.T) {
	h := newInstalledSandboxHarness(t)
	if err := h.run("ls"); err != nil {
		t.Fatal(err)
	}
	if out := h.out.String(); !strings.Contains(out, "No sandboxes on prod") || !strings.Contains(out, "teploy sandbox new --host prod --image <image>") {
		t.Errorf("output %q", out)
	}

	first, second := withRun(h, "running"), withRun(h, "stopped")
	engineState(h.engine, func() bool { h.engine.runs[0].Labels = map[string]string{"b": "2", "a": "1"}; return true })
	h.out.Reset()
	if err := h.run("ls"); err != nil {
		t.Fatal(err)
	}
	out := h.out.String()
	for _, want := range []string{
		"Sandboxes on prod (engine 0.1.0, docker driver):",
		first, "running", "alpine:3.20", "expires 2026-11-25T16:30:00Z", "a=1,b=2",
		second, "stopped", "kept until 2026-12-09T16:00:00Z",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the listing does not show %q:\n%s", want, out)
		}
	}
	// Oldest first, whatever order the engine listed them in.
	if strings.Index(out, second) < strings.Index(out, first) {
		t.Errorf("the stopped run is listed first:\n%s", out)
	}

	// A run the engine has sampled carries what it measured.
	engineState(h.engine, func() bool {
		cpu, disk := 2.5, int64(4096)
		h.engine.runs[1].Usage = sandbox.Usage{RunningSec: 300, CPUSeconds: &cpu, DiskBytes: &disk}
		return true
	})
	h.out.Reset()
	doc, err := h.json("ls")
	if err != nil {
		t.Fatal(err)
	}
	if doc["machine_interface"] != float64(2) || doc["server"] != "prod" || doc["host"] != "prod" || doc["driver"] != "docker" {
		t.Errorf("the envelope's root is %v", doc)
	}
	runs, _ := doc["runs"].([]any)
	if len(runs) != 2 {
		t.Fatalf("runs = %v", doc["runs"])
	}
	running, _ := runs[0].(map[string]any)
	if running["id"] != first || running["state"] != "running" || running["image"] != "alpine:3.20" || running["created_at"] != "2026-11-25T16:00:00Z" || running["expires_at"] != "2026-11-25T16:30:00Z" || running["on_expire"] != "destroy" || running["user"] != "ship" || running["network"] != "none" {
		t.Errorf("the running run is %v", running)
	}
	if _, present := running["stopped_at"]; present {
		t.Errorf("a running run carries stopped_at: %v", running)
	}
	if usage, _ := running["usage"].(map[string]any); usage["running_sec"] != float64(0) || len(usage) != 1 {
		t.Errorf("unsampled usage is %v, want running_sec alone", running["usage"])
	}
	stopped, _ := runs[1].(map[string]any)
	if stopped["state"] != "stopped" || stopped["stopped_at"] != "2026-11-25T16:00:00Z" || stopped["retain_until"] != "2026-12-09T16:00:00Z" {
		t.Errorf("the stopped run is %v", stopped)
	}
	if usage, _ := stopped["usage"].(map[string]any); usage["running_sec"] != float64(300) || usage["cpu_seconds"] != float64(2.5) || usage["disk_bytes"] != float64(4096) {
		t.Errorf("the sampled usage is %v, want what the engine measured", stopped["usage"])
	}
	if _, present := stopped["usage"].(map[string]any)["rx_bytes"]; present {
		t.Errorf("a measure the engine left out is in the document: %v", stopped["usage"])
	}
	if h.errOut.Len() != 0 {
		t.Errorf("under --json, stderr held %q", h.errOut.String())
	}
}

func TestSandboxNew(t *testing.T) {
	h := newInstalledSandboxHarness(t)
	err := h.run("new", "--image", "alpine:3.20", "--ttl", "2h", "--network", "allowlist", "--env", "CI=true",
		"--cpus", "1.5", "--memory-mb", "512", "--pids", "64", "--run-as", "root", "--label", "team=x", "--on-expire", "stop")
	if err != nil {
		t.Fatal(err)
	}
	id := sandboxTestID(0)
	if out := h.out.String(); !strings.Contains(out, "Created "+id+" on prod (alpine:3.20, expires 2026-11-25T18:00:00Z)") || !strings.Contains(out, "teploy sandbox exec "+id+" --host prod -- <command>") {
		t.Errorf("output %q", out)
	}

	// What reached the engine, in the wire's own field names. The CLI adds
	// one label of its own to every run it makes.
	sent := h.engine.createdJSON(t, 0)
	want := `^\{"image":"alpine:3.20","env":\{"CI":"true"\},"ttlSec":7200,"network":"allowlist","limits":\{"memoryMb":512,"cpus":1.5,"pids":64\},"user":"root","labels":\{"team":"x","teploy.cli-run":"[0-9a-f]{12}"\},"onExpire":"stop"\}$`
	if !regexp.MustCompile(want).MatchString(sent) {
		t.Errorf("the create sent %s", sent)
	}

	// A minimal create leaves every default to the engine.
	h.out.Reset()
	if err := h.run("new", "--image", "node:22"); err != nil {
		t.Fatal(err)
	}
	if sent := h.engine.createdJSON(t, 1); !regexp.MustCompile(`^\{"image":"node:22","labels":\{"teploy.cli-run":"[0-9a-f]{12}"\}\}$`).MatchString(sent) {
		t.Errorf("a minimal create sent %s", sent)
	}

	h.out.Reset()
	doc, err := h.json("new", "--image", "redis:7")
	if err != nil {
		t.Fatal(err)
	}
	run, _ := doc["run"].(map[string]any)
	if doc["machine_interface"] != float64(2) || doc["server"] != "prod" || doc["host"] != "prod" || run["id"] != sandboxTestID(2) || run["image"] != "redis:7" || run["state"] != "running" {
		t.Errorf("the envelope is %v", doc)
	}
}

// A create is never sent twice. When its answer is lost the outcome is
// unknown, and the label the CLI put on the run is how it is found.
func TestSandboxNewWithALostAnswer(t *testing.T) {
	h := newInstalledSandboxHarness(t)
	engineState(h.engine, func() bool { h.engine.loseCreateAnswer = true; return true })
	err := h.run("new", "--image", "alpine")
	if err == nil {
		t.Fatal("a lost answer read as success")
	}
	if !errors.Is(err, errSandboxOutcomeUnknown) || classifyMachineError(err) != codeUncertainOutcome {
		t.Errorf("a lost answer is the unknown-outcome class (code %s): %v", classifyMachineError(err), err)
	}
	label := engineState(h.engine, func() string { return h.engine.runs[0].Labels[sandboxRunLabel] })
	for _, want := range []string{"whether it happened is unknown", "teploy sandbox ls --host prod", sandboxRunLabel + "=" + label} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not say %q:\n%v", want, err)
		}
	}
	if created := engineState(h.engine, func() int { return len(h.engine.created) }); created != 1 {
		t.Fatalf("the create reached the engine %d times", created)
	}
	// And looking finds it.
	h.out.Reset()
	if err := h.run("ls"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.out.String(), sandboxRunLabel+"="+label) {
		t.Errorf("the listing does not show the label:\n%s", h.out.String())
	}
}

func TestSandboxExec(t *testing.T) {
	h := newInstalledSandboxHarness(t)
	id := withRun(h, "running")
	if err := h.run("exec", id, "--", "sh", "-c", "echo out; echo err >&2; exit 3"); err == nil || err.Error() != "exit status 3" {
		t.Fatalf("got %v, want the command's exit status", err)
	}
	if h.out.String() != "out\n" || h.errOut.String() != "err\n" {
		t.Errorf("stdout %q, stderr %q: the command's streams crossed", h.out.String(), h.errOut.String())
	}
	if h.engine.execCount() != 1 {
		t.Fatal("the engine did not run the command")
	}

	// A stopped sandbox cannot run one: the error says its state, and the
	// command that changes it.
	h.out.Reset()
	if err := h.run("stop", id); err != nil {
		t.Fatal(err)
	}
	h.out.Reset()
	err := h.run("exec", id, "--", "true")
	if err == nil || !strings.Contains(err.Error(), "sandbox "+id+" on prod is stopped. Resume it first:\n  teploy sandbox resume "+id+" --host prod") {
		t.Errorf("got %v, want the run's state and the way out", err)
	}
	if classifyMachineError(err) != codeConflict {
		t.Errorf("the engine's 409 is conflict, got %s", classifyMachineError(err))
	}

	// A timeout, and a sandbox that could not run the command, are each
	// said as themselves.
	h.run("resume", id)
	h.engine.setExecHook(func(w http.ResponseWriter, req sandbox.ExecRequest) {
		sandboxSSE(w, req, [2]string{"stdout", "partial"}, [2]string{"exit", `{"exitCode":124,"timedOut":true}`})
	})
	h.out.Reset()
	if err := h.run("exec", id, "--timeout", "5s", "--", "sleep 9"); err == nil || err.Error() != "the command timed out after 5s" || h.out.String() != "partial" {
		t.Errorf("a timeout: %v, output %q", err, h.out.String())
	}
	h.engine.setExecHook(func(w http.ResponseWriter, req sandbox.ExecRequest) {
		sandboxSSE(w, req, [2]string{"stderr", "sandbox: docker exec failed"}, [2]string{"exit", `{"exitCode":-1,"timedOut":false}`})
	})
	h.out.Reset()
	h.errOut.Reset()
	if err := h.run("exec", id, "--", "true"); err == nil || err.Error() != "the sandbox could not run the command; its reason is in its stderr" || h.errOut.String() != "sandbox: docker exec failed" {
		t.Errorf("a sandbox failure: %v, stderr %q", err, h.errOut.String())
	}

	// A stream that dies before the exit leaves the command's fate unknown.
	h.engine.setExecHook(func(w http.ResponseWriter, req sandbox.ExecRequest) { sandboxSSE(w, req, [2]string{"stdout", "half"}) })
	h.out.Reset()
	err = h.run("exec", id, "--", "true")
	if !errors.Is(err, errSandboxOutcomeUnknown) || classifyMachineError(err) != codeUncertainOutcome || h.out.String() != "half" {
		t.Errorf("a stream that died: %v, output %q", err, h.out.String())
	}
}

// A refusal before the stream ran nothing, so there is nothing to report
// on: under --json stdout stays empty and the error envelope is the whole
// answer. (It used to print an exec document with exit_code null, which the
// contract reads as "the command may have finished".)
func TestSandboxExecJSONRefusalPrintsOnlyTheError(t *testing.T) {
	stopped := func(t *testing.T) (*sandboxHarness, []string) {
		h := newInstalledSandboxHarness(t)
		return h, []string{"exec", withRun(h, "stopped"), "--", "true"}
	}
	notThere := func(t *testing.T) (*sandboxHarness, []string) {
		return newInstalledSandboxHarness(t), []string{"exec", sandboxTestID(9), "--", "true"}
	}
	badID := func(t *testing.T) (*sandboxHarness, []string) {
		return newInstalledSandboxHarness(t), []string{"exec", "../v1/runs", "--", "true"}
	}
	full := func(t *testing.T) (*sandboxHarness, []string) {
		h := newInstalledSandboxHarness(t)
		h.engine.setExecHook(func(w http.ResponseWriter, req sandbox.ExecRequest) {
			sandboxProblem(w, http.StatusServiceUnavailable, "at-capacity", "the daemon is full")
		})
		return h, []string{"exec", withRun(h, "running"), "--", "true"}
	}
	unreachable := func(t *testing.T) (*sandboxHarness, []string) {
		h := newInstalledSandboxHarness(t)
		h.link.setTunnel("refused")
		return h, []string{"exec", withRun(h, "running"), "--", "true"}
	}
	for _, c := range []struct {
		name string
		run  func(t *testing.T) (*sandboxHarness, []string)
		code string
	}{
		{"a stopped sandbox", stopped, codeConflict},
		{"a sandbox that is not there", notThere, codeInternal},
		{"an id that is not one", badID, codeInternal},
		{"an engine at capacity", full, codeInternal},
		{"an engine that cannot be reached", unreachable, codeUnsupported},
	} {
		h, args := c.run(t)
		h.flags.JSON = true
		err := h.run(args...)
		if err == nil {
			t.Errorf("%s: the exec succeeded", c.name)
			continue
		}
		if h.out.Len() != 0 {
			t.Errorf("%s: a refused exec printed on stdout: %s", c.name, h.out.String())
		}
		if code := classifyMachineError(err); code != c.code {
			t.Errorf("%s: the class is %s, want %s (%v)", c.name, code, c.code, err)
		}
	}

	// An id that is not one never reaches the server at all.
	h, args := badID(t)
	if err := h.run(args...); err == nil || h.engine.execCount() != 0 || len(h.connected) != 0 {
		t.Errorf("an id that is not one was sent: err %v, %d execs, connected to %v", err, h.engine.execCount(), h.connected)
	}
}

// An answer that never came is not a document either. The command was
// sent, so it may be running: that is the unknown outcome, and the exec is
// not sent again.
func TestSandboxExecWithALostAnswer(t *testing.T) {
	for _, json := range []bool{false, true} {
		h := newInstalledSandboxHarness(t)
		id := withRun(h, "running")
		engineState(h.engine, func() bool { h.engine.loseExecAnswer = true; return true })
		h.flags.JSON = json
		err := h.run("exec", id, "--", "rm -rf /tmp/x")
		if err == nil || !errors.Is(err, errSandboxOutcomeUnknown) || classifyMachineError(err) != codeUncertainOutcome {
			t.Errorf("--json %v: a lost answer is not the unknown outcome: %v (%s)", json, err, classifyMachineError(err))
		}
		for _, want := range []string{"was sent, but no answer from the engine arrived", "whether it ran is unknown", "it is not sent again"} {
			if err != nil && !strings.Contains(err.Error(), want) {
				t.Errorf("--json %v: the error does not say %q: %v", json, want, err)
			}
		}
		if h.out.Len() != 0 {
			t.Errorf("--json %v: an exec that was never answered printed on stdout: %s", json, h.out.String())
		}
		if h.engine.execCount() != 1 {
			t.Errorf("--json %v: the engine was asked %d times; an exec is never sent twice", json, h.engine.execCount())
		}
	}
}

// A 200 says the engine took the command. When what follows is not the
// stream, the command's exit is still not known: the document says so with
// a null exit_code, and the error is the unknown outcome, not a failure.
func TestSandboxExecAnsweredWithSomethingElse(t *testing.T) {
	h := newInstalledSandboxHarness(t)
	id := withRun(h, "running")
	h.engine.setExecHook(func(w http.ResponseWriter, req sandbox.ExecRequest) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<h1>login</h1>")
	})
	h.flags.JSON = true
	err := h.run("exec", id, "--", "true")
	if !errors.Is(err, errSandboxOutcomeUnknown) || classifyMachineError(err) != codeUncertainOutcome {
		t.Errorf("a 200 that is not the stream is not the unknown outcome: %v (%s)", err, classifyMachineError(err))
	}
	if doc := h.jsonDocument(); doc["exit_code"] != nil || doc["stdout"] != "" {
		t.Errorf("the document is %v", doc)
	}
}

// The output is had from the engine byte for byte: the exec asks a daemon
// that speaks base64 for it, and what the command wrote is what is
// printed, and what the document holds (a stream that is not valid UTF-8
// is given as base64 beside its text).
func TestSandboxExecOutputIsByteForByte(t *testing.T) {
	const bytes = "\xff\xfe\x00\x01\rprogress 50%\rprogress 100%\r\nlast line without a newline"
	// The command makes its bytes with printf's escapes (an argument cannot
	// hold a NUL).
	h := newInstalledSandboxHarness(t) // the engine says 0.1.0: a release that speaks base64
	id := withRun(h, "running")
	if err := h.run("exec", id, "--", "printf", `\377\376\000\001\rprogress 50%%\rprogress 100%%\r\nlast line without a newline`); err != nil {
		t.Fatal(err)
	}
	if h.out.String() != bytes {
		t.Errorf("the output came back %q, want the command's bytes %q", h.out.String(), bytes)
	}
	if sent := h.engine.execs[0]; sent.OutputEncoding != "base64" {
		t.Errorf("the engine was asked %+v, want outputEncoding base64", sent)
	}

	h.out.Reset()
	doc, err := h.json("exec", id, "--", "printf", `\377\376\000\001\rprogress 50%%\rprogress 100%%\r\nlast line without a newline`)
	if err != nil {
		t.Fatal(err)
	}
	if doc["stdout"] != "\ufffd\ufffd\x00\x01\rprogress 50%\rprogress 100%\r\nlast line without a newline" {
		t.Errorf("stdout in the document is %q", doc["stdout"])
	}
	if doc["stdout_base64"] != base64.StdEncoding.EncodeToString([]byte(bytes)) {
		t.Errorf("stdout_base64 in the document is %v", doc["stdout_base64"])
	}
	if _, present := doc["stderr_base64"]; present {
		t.Errorf("an empty stderr has a base64 form: %v", doc)
	}
	decoded, err := base64.StdEncoding.DecodeString(fmt.Sprint(doc["stdout_base64"]))
	if err != nil || string(decoded) != bytes {
		t.Errorf("stdout_base64 decodes to %q, %v", decoded, err)
	}

	// Text that is valid UTF-8 is already byte for byte: no base64 form.
	h.out.Reset()
	doc, err = h.json("exec", id, "--", "sh", "-c", "echo héllo; echo err >&2")
	if err != nil {
		t.Fatal(err)
	}
	if doc["stdout"] != "héllo\n" || doc["stderr"] != "err\n" {
		t.Errorf("the document is %v", doc)
	}
	for _, key := range []string{"stdout_base64", "stderr_base64"} {
		if _, present := doc[key]; present {
			t.Errorf("%s is in the document for valid UTF-8: %v", key, doc)
		}
	}
}

// Only a daemon that documents base64 is asked for it. One that reports no
// version (a build from before the field, or any dev build) gets the
// request it always got, and its text is read as text.
func TestSandboxExecAsksForBytesOnlyWhereTheyAreSpoken(t *testing.T) {
	for _, c := range []struct {
		version string
		asks    bool
	}{
		{"0.1.0", true}, {"0.2.0", true}, {"1.0.0", true}, {"0.1.0-rc.1", false}, {"0.0.9", false}, {"dev", false}, {"", false},
	} {
		h := newInstalledSandboxHarness(t)
		engineState(h.engine, func() bool { h.engine.version = c.version; return true })
		id := withRun(h, "running")
		if err := h.run("exec", id, "--", "echo", "text"); err != nil {
			t.Fatalf("%s: %v", c.version, err)
		}
		if asked := h.engine.execs[0].OutputEncoding == "base64"; asked != c.asks || h.out.String() != "text\n" {
			t.Errorf("engine %q: asked for base64 %v (want %v), output %q", c.version, asked, c.asks, h.out.String())
		}
	}

	// A daemon that says it is a release and answers in text anyway cannot
	// be read: the output is not passed on as something it is not, and the
	// command's fate is unknown.
	h := newInstalledSandboxHarness(t)
	id := withRun(h, "running")
	h.engine.setExecHook(func(w http.ResponseWriter, _ sandbox.ExecRequest) {
		fmt.Fprint(w, "event: stdout\ndata: looks like text\n\nevent: exit\ndata: {\"exitCode\":0,\"timedOut\":false}\n\n")
	})
	err := h.run("exec", id, "--", "true")
	if !errors.Is(err, errSandboxOutcomeUnknown) || classifyMachineError(err) != codeUncertainOutcome || h.out.Len() != 0 {
		t.Errorf("text where base64 was asked: %v (%s), output %q", err, classifyMachineError(err), h.out.String())
	}
}

func TestSandboxExecJSON(t *testing.T) {
	h := newInstalledSandboxHarness(t)
	id := withRun(h, "running")
	doc, err := h.json("exec", id, "--", "sh", "-c", "echo out; echo err >&2")
	if err != nil {
		t.Fatal(err)
	}
	if doc["machine_interface"] != float64(2) || doc["server"] != "prod" || doc["host"] != "prod" || doc["run_id"] != id || doc["exit_code"] != float64(0) || doc["timed_out"] != false || doc["stdout"] != "out\n" || doc["stderr"] != "err\n" || doc["truncated"] != false {
		t.Errorf("the envelope is %v", doc)
	}
	if _, isNumber := doc["duration_ms"].(float64); !isNumber {
		t.Errorf("duration_ms is %v", doc["duration_ms"])
	}
	if h.errOut.Len() != 0 {
		t.Errorf("under --json the command's stderr went to stderr: %q", h.errOut.String())
	}

	// A command that fails: the document says so, and teploy exits nonzero.
	h.flags.JSON = true
	h.out.Reset()
	if err := h.run("exec", id, "--", "exit", "3"); err == nil || err.Error() != "exit status 3" {
		t.Errorf("got %v, want the exit status", err)
	}
	if doc := h.jsonDocument(); doc["exit_code"] != float64(3) {
		t.Errorf("the document is %v", doc)
	}

	// Output past the capture is cut and said to be.
	h.engine.setExecHook(func(w http.ResponseWriter, req sandbox.ExecRequest) {
		sandboxSSE(w, req, [2]string{"stdout", strings.Repeat("x", sandboxExecCapture+10)}, [2]string{"exit", `{"exitCode":0,"timedOut":false}`})
	})
	h.out.Reset()
	if err := h.run("exec", id, "--", "true"); err != nil {
		t.Fatal(err)
	}
	if doc := h.jsonDocument(); doc["truncated"] != true || len(doc["stdout"].(string)) != sandboxExecCapture {
		t.Errorf("a long output: truncated %v, %d bytes", doc["truncated"], len(doc["stdout"].(string)))
	}

	// A stream that died before the exit: no exit code, and unknown.
	h.engine.setExecHook(func(w http.ResponseWriter, req sandbox.ExecRequest) {
		sandboxSSE(w, req, [2]string{"stdout", "partial"})
	})
	h.out.Reset()
	err = h.run("exec", id, "--", "true")
	if !errors.Is(err, errSandboxOutcomeUnknown) || classifyMachineError(err) != codeUncertainOutcome {
		t.Errorf("a stream that died is the unknown-outcome class: %v", err)
	}
	if doc := h.jsonDocument(); doc["exit_code"] != nil || doc["stdout"] != "partial" {
		t.Errorf("the document is %v", doc)
	}

	// A sandbox that is not there is said as such.
	h.out.Reset()
	if err := h.run("exec", sandboxTestID(9), "--", "true"); err == nil || !strings.Contains(err.Error(), "no sandbox with that id on prod") {
		t.Errorf("got %v", err)
	}
}
