package cli

// sandbox_lifecycle_test.go — `stop`, `resume`, `fork`, `rm` and `cp`
// against the engine, and the error envelope a machine consumer reads.

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/sandbox"
	"github.com/useteploy/teploy/internal/ssh"
)

func TestSandboxStopResumeFork(t *testing.T) {
	h := newInstalledSandboxHarness(t)
	id := withRun(h, "running")

	if err := h.run("stop", id); err != nil {
		t.Fatal(err)
	}
	if out := h.out.String(); !strings.Contains(out, "Stopped "+id+" on prod (kept until 2026-12-09T16:00:00Z)") || !strings.Contains(out, "teploy sandbox resume "+id+" --host prod") {
		t.Errorf("output %q", out)
	}
	// The contract: a stop repeated after it succeeded is a 409, and the run
	// is read to confirm. Already stopped is what was asked for.
	h.out.Reset()
	if err := h.run("stop", id); err != nil {
		t.Fatalf("stopping a stopped sandbox: %v", err)
	}
	if out := h.out.String(); !strings.Contains(out, id+" on prod is already stopped (kept until 2026-12-09T16:00:00Z)") {
		t.Errorf("output %q", out)
	}
	if stops := engineState(h.engine, func() int { return len(h.engine.stopped) }); stops != 1 {
		t.Errorf("the engine stopped %d sandboxes", stops)
	}

	h.out.Reset()
	if err := h.run("resume", id, "--ttl", "1h"); err != nil {
		t.Fatal(err)
	}
	if out := h.out.String(); !strings.Contains(out, "Resumed "+id+" on prod (running until 2026-11-25T17:00:00Z)") {
		t.Errorf("output %q", out)
	}
	h.out.Reset()
	if err := h.run("resume", id); err != nil {
		t.Fatalf("resuming a running sandbox: %v", err)
	}
	if out := h.out.String(); !strings.Contains(out, id+" on prod is already running (until 2026-11-25T17:00:00Z)") {
		t.Errorf("output %q", out)
	}
	if resumes := engineState(h.engine, func() int { return len(h.engine.resumed) }); resumes != 1 {
		t.Errorf("the engine resumed %d sandboxes", resumes)
	}

	h.out.Reset()
	doc, err := h.json("stop", id)
	if err != nil {
		t.Fatal(err)
	}
	run, _ := doc["run"].(map[string]any)
	if doc["machine_interface"] != float64(2) || doc["server"] != "prod" || doc["host"] != "prod" || run["id"] != id || run["state"] != "stopped" || run["retain_until"] != "2026-12-09T16:00:00Z" {
		t.Errorf("the envelope is %v", doc)
	}

	// A fork takes the source's labels, and a run label of its own: a copy
	// carrying the source's could not be told from it after a lost answer.
	engineState(h.engine, func() bool {
		h.engine.runs[0].Labels = map[string]string{"team": "x", sandboxRunLabel: "aaaaaaaaaaaa"}
		return true
	})
	h.out.Reset()
	if err := h.run("fork", id, "--label", "extra=1"); err != nil {
		t.Fatal(err)
	}
	fork := sandboxTestID(1)
	if out := h.out.String(); !strings.Contains(out, "Forked "+fork+" from "+id+" on prod (running until 2026-11-25T16:30:00Z)") || !strings.Contains(out, "teploy sandbox exec "+fork+" --host prod -- <command>") {
		t.Errorf("output %q", out)
	}
	labels := engineState(h.engine, func() map[string]string { return h.engine.runs[1].Labels })
	if labels["team"] != "x" || labels["extra"] != "1" || labels[sandboxRunLabel] == "aaaaaaaaaaaa" || !regexp.MustCompile(`^[0-9a-f]{12}$`).MatchString(labels[sandboxRunLabel]) {
		t.Errorf("the fork's labels are %v", labels)
	}
	if source := engineState(h.engine, func() string { return h.engine.runs[0].State }); source != "stopped" {
		t.Errorf("the source is %s after the fork", source)
	}
	if forkedFrom := engineState(h.engine, func() string { return h.engine.runs[1].ForkedFrom }); forkedFrom != id {
		t.Errorf("the fork came from %s", forkedFrom)
	}
	if sent := engineState(h.engine, func() string { return h.engine.forked[0].Labels[sandboxRunLabel] }); sent != labels[sandboxRunLabel] {
		t.Errorf("the fork's own label was not the one sent: %s", sent)
	}
}

func TestSandboxRemove(t *testing.T) {
	h := newInstalledSandboxHarness(t)
	id := withRun(h, "running")
	h.answer = false
	err := h.run("rm", id)
	if !errors.Is(err, errSandboxAborted) {
		t.Fatalf("got %v, want the refusal to destroy without a confirmation", err)
	}
	if len(h.prompts) != 1 || !strings.Contains(h.prompts[0], "Destroy sandbox "+id+" on prod (alpine:3.20, running)? Its filesystem is lost.") {
		t.Errorf("the prompt was %q", h.prompts)
	}
	if destroyed := engineState(h.engine, func() int { return len(h.engine.destroyed) }); destroyed != 0 {
		t.Fatalf("a sandbox was destroyed without a confirmation (%d)", destroyed)
	}

	h.answer = true
	h.out.Reset()
	if err := h.run("rm", "-y", id); err != nil {
		t.Fatal(err)
	}
	if out := h.out.String(); out != "Destroyed "+id+" on prod\n" {
		t.Errorf("output %q", out)
	}
	if len(h.prompts) != 1 {
		t.Errorf("--yes still asked: %q", h.prompts)
	}
	if err := h.run("rm", "-y", id); err == nil || !strings.Contains(err.Error(), "no sandbox with that id on prod") {
		t.Errorf("a sandbox that is gone: %v", err)
	}

	id = withRun(h, "running")
	h.out.Reset()
	doc, err := h.json("rm", "-y", id)
	if err != nil {
		t.Fatal(err)
	}
	if doc["machine_interface"] != float64(2) || doc["server"] != "prod" || doc["host"] != "prod" || doc["run_id"] != id {
		t.Errorf("the envelope is %v", doc)
	}
}

func TestSandboxCopy(t *testing.T) {
	h := newInstalledSandboxHarness(t)
	id := withRun(h, "running")
	dir := t.TempDir()
	local := filepath.Join(dir, "app.tar.gz")
	if err := os.WriteFile(local, []byte("payload\x00"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := h.run("cp", local, id+":uploads/app.tar.gz"); err != nil {
		t.Fatal(err)
	}
	if content, ok := h.engine.file(id, "uploads/app.tar.gz"); !ok || string(content) != "payload\x00" {
		t.Errorf("the sandbox received %q (present %v)", content, ok)
	}
	if out := h.out.String(); !strings.Contains(out, "Copied 8 B from "+local+" to "+id+":uploads/app.tar.gz on prod") {
		t.Errorf("output %q", out)
	}

	// Back out, into a directory: the file lands under its own name. A
	// leading slash names the same file.
	h.out.Reset()
	if err := h.run("cp", id+":/uploads/app.tar.gz", dir); err != nil {
		t.Fatal(err)
	}
	if content, err := os.ReadFile(filepath.Join(dir, "app.tar.gz")); err != nil || string(content) != "payload\x00" {
		t.Errorf("the file came back as %q, %v", content, err)
	}

	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"cp", filepath.Join(dir, "nope"), id + ":x"}, "reading " + filepath.Join(dir, "nope")},
		{[]string{"cp", dir, id + ":x"}, "is a directory; `cp` copies one file"},
		{[]string{"cp", id + ":nope.txt", filepath.Join(dir, "y")}, "sandbox " + id + " on prod has no file nope.txt"},
	} {
		h.out.Reset()
		if err := h.run(c.args...); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: got %v, want it to say %q", c.args, err, c.want)
		}
	}

	// A file past the engine's bound is refused before it is read or sent.
	big := filepath.Join(dir, "big.bin")
	if err := os.WriteFile(big, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(big, sandbox.MaxFileBytes+1); err != nil { // sparse: no 64 MiB is written
		t.Fatal(err)
	}
	if err := h.run("cp", big, id+":big.bin"); err == nil || !strings.Contains(err.Error(), "the engine takes at most 64.0 MiB") {
		t.Errorf("an oversized file: %v", err)
	}

	h.out.Reset()
	doc, err := h.json("cp", local, id+":again.bin")
	if err != nil {
		t.Fatal(err)
	}
	if doc["machine_interface"] != float64(2) || doc["server"] != "prod" || doc["host"] != "prod" || doc["run_id"] != id || doc["path"] != "again.bin" || doc["direction"] != "to_sandbox" || doc["bytes"] != float64(8) {
		t.Errorf("the envelope is %v", doc)
	}
	h.out.Reset()
	if doc, err = h.json("cp", id+":again.bin", filepath.Join(dir, "back.bin")); err != nil || doc["direction"] != "from_sandbox" {
		t.Errorf("the envelope is %v, %v", doc, err)
	}
}

// What a machine consumer reads when a sandbox command fails. The exit code
// stays 1; the envelope is where the reason's class is.
func TestSandboxErrorEnvelope(t *testing.T) {
	notInstalled := func(t *testing.T) error {
		h := newSandboxHarness(t,
			ssh.MockCommand{Match: "if command -v systemctl", Output: sandboxProbeLines(true, false, false, "inactive", "", false)},
			tokenCommand(sandboxTokenFile, sandboxTokenMarker),
		)
		h.link.tunnel = "refused"
		return h.run("ls")
	}
	stoppedExec := func(t *testing.T) error {
		h := newInstalledSandboxHarness(t)
		id := withRun(h, "stopped")
		return h.run("exec", id, "--", "true")
	}
	lostAnswer := func(t *testing.T) error {
		h := newInstalledSandboxHarness(t)
		engineState(h.engine, func() bool { h.engine.loseCreateAnswer = true; return true })
		return h.run("new", "--image", "alpine")
	}
	refusedByEngine := func(t *testing.T) error {
		h := newInstalledSandboxHarness(t)
		h.engine.setExecHook(func(w http.ResponseWriter) { sandboxProblem(w, 503, "at-capacity", "the daemon is full") })
		return h.run("exec", withRun(h, "running"), "--", "true")
	}
	for _, c := range []struct {
		name    string
		err     func(t *testing.T) error
		code    string
		message string
	}{
		{"engine not installed", notInstalled, "unsupported", "the sandbox engine cannot serve this request on this server"},
		{"exec in a stopped sandbox", stoppedExec, "conflict", "the sandbox is in the wrong state for this request"},
		{"create whose answer was lost", lostAnswer, "uncertain-outcome", "sandbox request outcome unknown — list the sandboxes before trying again"},
	} {
		err := c.err(t)
		if err == nil {
			t.Fatalf("%s: no error", c.name)
		}
		var out bytes.Buffer
		if err := writeMachineErrorEnvelope(&out, err); err != nil {
			t.Fatal(err)
		}
		var envelope struct {
			MachineInterface int    `json:"machine_interface"`
			Code             string `json:"code"`
			Message          string `json:"message"`
			Detail           string `json:"detail"`
		}
		if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
			t.Fatalf("%s: %v\n%s", c.name, err, out.String())
		}
		if envelope.MachineInterface != MachineInterface || envelope.Code != c.code || envelope.Message != c.message || envelope.Detail != err.Error() {
			t.Errorf("%s: the envelope is %+v", c.name, envelope)
		}
	}

	// An answer the engine gave is the engine's own, whatever it was.
	err := refusedByEngine(t)
	if classifyMachineError(err) != codeInternal {
		t.Errorf("a 503 from the engine classified as %s", classifyMachineError(err))
	}
}
