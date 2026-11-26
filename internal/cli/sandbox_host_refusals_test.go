package cli

// sandbox_host_refusals_test.go — what `teploy sandbox host` upgrades, and
// everything it refuses: a server it did not install the engine on, a
// server it cannot install on, and a release it cannot trust. To refuse is
// to have changed nothing.

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy/internal/ssh"
)

var nothingInstalled = sandboxProbeLines(true, false, false, "inactive", "", false)

// changedNothing reports whether any command that changes the server ran.
func changedNothing(calls []string) string {
	for _, call := range calls {
		if strings.HasPrefix(call, "flock") || strings.HasPrefix(call, "install -") || strings.HasPrefix(call, "UPLOAD:/tmp/teploy-sandbox") ||
			strings.Contains(call, "systemctl enable") || strings.Contains(call, "systemctl start") || strings.Contains(call, "systemctl restart") ||
			strings.HasPrefix(call, "python3 -c ") {
			return call
		}
	}
	return ""
}

// olderUnit is a service file an earlier CLI, or a hand, wrote: it has
// the marker (so this command manages the service) and differs from what
// this CLI writes.
const olderUnit = "# Installed by: teploy sandbox host\n[Service]\nExecStart=/usr/local/bin/teploy-sandbox serve\n"

// unitsInstalled is the service file's content at each `install` of it,
// read while the staged file is still on the server.
func unitsInstalled(h *sandboxHarness) *[]string {
	inner := h.link.beforeRun
	units := &[]string{}
	h.link.beforeRun = func(cmd string) {
		if inner != nil {
			inner(cmd)
		}
		if strings.HasPrefix(cmd, "install -o 0 -g 0 -m 0644 "+stagedUnit) {
			*units = append(*units, string(h.link.Files["/tmp/teploy-install.sandbox/teploy-sandbox.service"]))
		}
	}
	return units
}

func count(calls []string, part string) int {
	n := 0
	for _, call := range calls {
		if strings.Contains(call, part) {
			n++
		}
	}
	return n
}

// An upgrade restarts the engine, and the prompt says what that costs by
// the engine's own rule: the running sandboxes made with `--on-expire stop`
// are stopped and kept, every other running one is destroyed, and every
// command still running in one is cut off. The binary it replaces is kept
// as teploy-sandbox.prev, and the service file is rewritten when it differs.
func TestSandboxHostUpgrades(t *testing.T) {
	// An older release is running, with two sandboxes in it: one made to be
	// kept, one not.
	h := newSandboxHarness(t, hostCommands(sandboxProbeLines(true, true, true, "active", "0.0.9", true), tokenCommand(sandboxTokenFile, sandboxTestToken),
		ssh.MockCommand{Match: catUnit, Output: olderUnit})...)
	withRun(h, "running")
	withRun(h, "running")
	engineState(h.engine, func() bool { h.engine.runs[0].OnExpire = "stop"; return true })
	units := unitsInstalled(h)
	fetched := h.release(engineBinary)
	h.answer = false
	err := h.run("host", "prod")
	if !errors.Is(err, errSandboxAborted) {
		t.Fatalf("got %v, want the refusal to upgrade without a confirmation", err)
	}
	// The prompt says what a restart costs: the running sandboxes, each by
	// what the engine does with it.
	if len(h.prompts) != 1 || h.prompts[0] != "Upgrade the sandbox engine on prod from 0.0.9 to "+sandboxEngineVersion+"? The service restarts, and 1 running sandbox(es) on it are destroyed and 1, the ones made with `--on-expire stop`, are stopped and kept; stopped ones are kept, and every command still running in one is cut off." {
		t.Errorf("the prompt was %q", h.prompts)
	}
	if len(*fetched) != 0 {
		t.Errorf("a release was downloaded before the confirmation: %v", *fetched)
	}

	h.answer = true
	h.out.Reset()
	if err := h.run("host", "prod"); err != nil {
		t.Fatal(err)
	}
	if len(*fetched) != 2 {
		t.Errorf("the release fetched was %v", *fetched)
	}
	// Looked at, the candidate staged and run, the previous binary kept,
	// the new one published, the service file rewritten (it differed), and
	// the service restarted.
	calls := h.link.Calls
	if !callsInOrder(calls, stagedBinary+" version", keepPrevious, "flock -w 30",
		"UPLOAD:/tmp/teploy-install.sandbox/teploy-sandbox.service", "install -o 0 -g 0 -m 0644 "+stagedUnit,
		"systemctl daemon-reload", "systemctl enable teploy-sandbox.service", "systemctl restart teploy-sandbox.service", "python3 -c ") {
		t.Errorf("the server was not asked in the order of an upgrade:\n%s", strings.Join(calls, "\n"))
	}
	if len(*units) != 1 || (*units)[0] != sandboxSystemdUnit() {
		t.Errorf("the service files installed were %q", *units)
	}
	for _, want := range []string{"Upgraded the sandbox engine on prod from 0.0.9 to " + sandboxEngineVersion + " and restarted it. The previous binary is kept as " + sandboxEnginePrevPath} {
		if !strings.Contains(h.out.String(), want) {
			t.Errorf("the output does not say %q:\n%s", want, h.out.String())
		}
	}

	// Under --json the document names the version that was there, and the
	// progress went to stderr.
	h = newSandboxHarness(t, hostCommands(sandboxProbeLines(true, true, true, "active", "0.0.9", true), tokenCommand(sandboxTokenFile, sandboxTestToken),
		ssh.MockCommand{Match: catUnit, Output: olderUnit})...)
	h.release(engineBinary)
	h.flags.JSON = true
	if err := h.run("host", "prod", "--yes"); err != nil {
		t.Fatal(err)
	}
	if doc := h.jsonDocument(); doc["action"] != "upgrade" || doc["from"] != "0.0.9" || doc["version"] != strings.TrimPrefix(sandboxEngineVersion, "v") {
		t.Errorf("the envelope is %v", doc)
	}
	if !strings.Contains(h.errOut.String(), "Keeping the current binary as "+sandboxEnginePrevPath) {
		t.Errorf("stderr held %q", h.errOut.String())
	}

	// A binary built by hand reports no version: it is replaced as well.
	h = newSandboxHarness(t, hostCommands(sandboxProbeLines(true, true, true, "inactive", "dev", false))...)
	engineStartsWhen(h)
	h.release(engineBinary)
	if err := h.run("host", "prod", "--yes"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.out.String(), "from dev to "+sandboxEngineVersion) {
		t.Errorf("output %q", h.out.String())
	}
	// Its service was not running: the prompt had no sandboxes to count.
	h = newSandboxHarness(t, hostCommands(sandboxProbeLines(true, true, true, "inactive", "dev", false))...)
	h.release(engineBinary)
	h.answer = false
	if err := h.run("host", "prod"); !errors.Is(err, errSandboxAborted) || len(h.prompts) != 1 || h.prompts[0] != "Upgrade the sandbox engine on prod from dev to "+sandboxEngineVersion+"? Its service is not running; it will be started." {
		t.Errorf("got %v, prompts %q", err, h.prompts)
	}
}

// When the engine does not answer after this command changed it, what was
// there is put back (the binary an upgrade kept, the service file it
// rewrote), the service is restarted, and the old engine is waited for. The
// error says why the new one was not accepted and whether the old one is
// back. Nothing is written to the history as done.
func TestSandboxHostPutsBackWhatItChanged(t *testing.T) {
	upgrade := func(t *testing.T) *sandboxHarness {
		h := newSandboxHarness(t, hostCommands(sandboxProbeLines(true, true, true, "active", "0.0.9", true), tokenCommand(sandboxTokenFile, sandboxTestToken),
			ssh.MockCommand{Match: catUnit, Output: olderUnit},
			ssh.MockCommand{Match: putBackPrev, Output: ""},
		)...)
		withRun(h, "running")
		return h
	}
	repair := func(t *testing.T) *sandboxHarness {
		return newSandboxHarness(t, hostCommands(sandboxProbeLines(true, true, true, "active", strings.TrimPrefix(sandboxEngineVersion, "v"), true),
			ssh.MockCommand{Match: catUnit, Output: olderUnit})...)
	}
	for _, c := range []struct {
		name     string
		server   func(t *testing.T) *sandboxHarness
		answers  func(h *sandboxHarness)
		there    string
		put      string
		oldBack  string
		oldGone  string
		progress string
	}{
		{"an upgrade whose new engine does not answer, and the old one does", upgrade, engineAnswersOnlyWithTheOldOne, "0.0.9", putBackPrev,
			"was put back and restarted, and it answers again. Nothing was upgraded",
			"was put back and restarted, but it does not answer either",
			"Putting back the previous binary (0.0.9)"},
		{"an upgrade whose new engine does not answer, nor the old one", upgrade, engineNeverAnswersAgain, "0.0.9", putBackPrev,
			"was put back and restarted, and it answers again. Nothing was upgraded",
			"was put back and restarted, but it does not answer either",
			"Putting back the previous binary (0.0.9)"},
		{"a rewritten service file the engine does not answer with, and the old one does", repair, engineAnswersOnlyWithTheOldOne, strings.TrimPrefix(sandboxEngineVersion, "v"), "install -o 0 -g 0 -m 0644 " + stagedUnit,
			"was put back and restarted, and it answers again. Nothing was upgraded",
			"was put back and restarted, but it does not answer either",
			"Putting back the service file"},
	} {
		h := c.server(t)
		c.answers(h)
		units := unitsInstalled(h)
		h.release(engineBinary)
		h.env.startTimeout = 3 * time.Second
		err := h.run("host", "prod", "--yes")
		if err == nil {
			t.Fatalf("%s: an engine that never answered read as upgraded", c.name)
		}
		back := c.oldBack
		if strings.Contains(c.name, "nor the old one") {
			back = c.oldGone
		}
		for _, want := range []string{"did not answer at 127.0.0.1:7439 on prod within 3s", "The engine that was there (" + c.there + ") " + back} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: the error does not say %q:\n%v", c.name, want, err)
			}
		}
		calls := h.link.Calls
		if !callsInOrder(calls, "systemctl restart teploy-sandbox.service", c.put, "systemctl restart teploy-sandbox.service") || count(calls, "systemctl restart teploy-sandbox.service") != 2 {
			t.Errorf("%s: what was there was not put back and the service restarted:\n%s", c.name, strings.Join(calls, "\n"))
		}
		if strings.HasPrefix(c.put, "mv ") && count(calls, keepPrevious) != 1 {
			t.Errorf("%s: the previous binary was not kept before the upgrade:\n%s", c.name, strings.Join(calls, "\n"))
		}
		// The service file this run rewrote went back too: what was there
		// was installed again, and systemd read it.
		if !callsInOrder(calls, c.put, "UPLOAD:/tmp/teploy-install.sandbox/teploy-sandbox.service", "install -o 0 -g 0 -m 0644 "+stagedUnit, "systemctl daemon-reload", "systemctl restart teploy-sandbox.service") {
			t.Errorf("%s: the service file that was there was not put back:\n%s", c.name, strings.Join(calls, "\n"))
		}
		if len(*units) != 2 || (*units)[0] != sandboxSystemdUnit() || (*units)[1] != olderUnit {
			t.Errorf("%s: the service files installed were %q", c.name, *units)
		}
		for _, call := range calls {
			if strings.HasPrefix(call, "python3 -c ") {
				t.Errorf("%s: a run whose engine never answered was written to the history as done", c.name)
			}
		}
		if !strings.Contains(h.out.String(), c.progress) {
			t.Errorf("%s: the output does not say %q:\n%s", c.name, c.progress, h.out.String())
		}
		// It says so when the old engine answers again, and only then.
		if strings.Contains(h.out.String(), "It answers again.") != strings.Contains(back, "answers again") {
			t.Errorf("%s: the output does not say whether the old engine answers:\n%s", c.name, h.out.String())
		}
		if lastCall(calls) != "rm -rf -- '/tmp/teploy-install.sandbox'" {
			t.Errorf("%s: the staging directory was left:\n%s", c.name, strings.Join(calls, "\n"))
		}
	}

	// Under --json the progress is on stderr and stdout holds nothing: the
	// error envelope is the whole answer.
	h := upgrade(t)
	engineAnswersOnlyWithTheOldOne(h)
	h.release(engineBinary)
	h.flags.JSON = true
	if err := h.run("host", "prod", "--yes"); err == nil || h.out.Len() != 0 || !strings.Contains(h.errOut.String(), "Putting back the previous binary (0.0.9)") {
		t.Errorf("--json: %v, stdout %q, stderr %q", err, h.out.String(), h.errOut.String())
	}
}

// A re-run on a server with this CLI's version rewrites the service file
// when it differs from what this CLI writes, and restarts the engine only
// then: a restart costs the sandboxes, so it is asked first. On a service
// that is not running there is nothing to ask.
func TestSandboxHostRewritesAChangedServiceFile(t *testing.T) {
	pinned := strings.TrimPrefix(sandboxEngineVersion, "v")
	// Running: it asks first, and the prompt says what the restart costs.
	h := newSandboxHarness(t, hostCommands(sandboxProbeLines(true, true, true, "active", pinned, true), tokenCommand(sandboxTokenFile, sandboxTestToken),
		ssh.MockCommand{Match: catUnit, Output: olderUnit})...)
	withRun(h, "running")
	h.answer = false
	if err := h.run("host", "prod"); !errors.Is(err, errSandboxAborted) || len(h.prompts) != 1 || h.prompts[0] != "Rewrite the sandbox engine's service file on prod? It was changed since `teploy sandbox host` wrote it. The service restarts, and 1 running sandbox(es) on it are destroyed; stopped ones are kept, and every command still running in one is cut off." {
		t.Errorf("got %v, prompts %q", err, h.prompts)
	}
	if call := changedNothing(h.link.Calls); call != "" {
		t.Errorf("the server was changed without a confirmation: %s", call)
	}

	// Nothing is downloaded: the engine is the right one.
	fetched := h.release(engineBinary)
	h.answer = true
	h.out.Reset()
	if err := h.run("host", "prod"); err != nil {
		t.Fatal(err)
	}
	if len(*fetched) != 0 {
		t.Errorf("a release was downloaded to rewrite a service file: %v", *fetched)
	}
	if !callsInOrder(h.link.Calls, catUnit, "UPLOAD:/tmp/teploy-install.sandbox/teploy-sandbox.service", "install -o 0 -g 0 -m 0644 "+stagedUnit,
		"systemctl daemon-reload", "systemctl enable teploy-sandbox.service", "systemctl restart teploy-sandbox.service", "python3 -c ") {
		t.Errorf("the server was not asked in the order of a repair:\n%s", strings.Join(h.link.Calls, "\n"))
	}
	if !strings.Contains(h.out.String(), "Rewrote the sandbox engine's service file on prod (it had been changed) and restarted the engine") {
		t.Errorf("output %q", h.out.String())
	}

	h = newSandboxHarness(t, hostCommands(sandboxProbeLines(true, true, true, "active", pinned, true), tokenCommand(sandboxTokenFile, sandboxTestToken),
		ssh.MockCommand{Match: catUnit, Output: olderUnit})...)
	h.flags.JSON = true
	if err := h.run("host", "prod", "--yes"); err != nil {
		t.Fatal(err)
	}
	if doc := h.jsonDocument(); doc["action"] != "repair" || doc["version"] != pinned {
		t.Errorf("the envelope is %v", doc)
	}
	if _, present := h.jsonDocument()["from"]; present {
		t.Error("a repair has a from")
	}

	// Not running: the file is rewritten and the service started, with
	// nothing to ask (no sandbox is running to be lost).
	h = newSandboxHarness(t, hostCommands(sandboxProbeLines(true, true, true, "inactive", pinned, false),
		ssh.MockCommand{Match: catUnit, Output: olderUnit})...)
	engineStartsWhen(h)
	if err := h.run("host", "prod"); err != nil || len(h.prompts) != 0 {
		t.Errorf("got %v, prompts %q", err, h.prompts)
	}
	if !callsInOrder(h.link.Calls, "install -o 0 -g 0 -m 0644 "+stagedUnit, "systemctl daemon-reload", "systemctl enable teploy-sandbox.service", "systemctl start teploy-sandbox.service") || count(h.link.Calls, "systemctl restart") != 0 {
		t.Errorf("the server was not asked in the order of a repair of a stopped service:\n%s", strings.Join(h.link.Calls, "\n"))
	}
	if !strings.Contains(h.out.String(), "Rewrote the sandbox engine's service file on prod (it had been changed) and started the engine") {
		t.Errorf("output %q", h.out.String())
	}

	// A file this CLI cannot read is rewritten as well.
	h = newSandboxHarness(t, hostCommands(sandboxProbeLines(true, true, true, "inactive", pinned, false),
		ssh.MockCommand{Match: catUnit, Err: errors.New("exit status 1: permission denied")})...)
	engineStartsWhen(h)
	if err := h.run("host", "prod"); err != nil || !callsInOrder(h.link.Calls, "install -o 0 -g 0 -m 0644 "+stagedUnit) {
		t.Errorf("an unreadable service file was left as it was: %v\n%s", err, strings.Join(h.link.Calls, "\n"))
	}
}

// A service whose binary is gone has no version to upgrade from. The
// prompt and the output say that, not an empty "from", and there is no
// previous binary to keep or put back.
func TestSandboxHostInstallsAMissingBinary(t *testing.T) {
	server := func(t *testing.T) *sandboxHarness {
		return newSandboxHarness(t, hostCommands(sandboxProbeLines(true, true, true, "inactive", "", false))...)
	}
	h := server(t)
	engineStartsWhen(h)
	h.release(engineBinary)
	h.answer = false
	if err := h.run("host", "prod"); !errors.Is(err, errSandboxAborted) || len(h.prompts) != 1 || h.prompts[0] != "Install the sandbox engine (teploy-sandbox "+sandboxEngineVersion+") on prod? Its service is installed there, but its binary is missing from "+sandboxEnginePath+". The service will be started." {
		t.Errorf("got %v, prompts %q", err, h.prompts)
	}
	h.answer = true
	if err := h.run("host", "prod"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.out.String(), "Upgraded the sandbox engine on prod to "+sandboxEngineVersion+" (its binary was missing) and restarted it") || strings.Contains(h.out.String(), "from  to") {
		t.Errorf("output %q", h.out.String())
	}
	if count(h.link.Calls, keepPrevious) != 0 || !callsInOrder(h.link.Calls, "systemctl restart teploy-sandbox.service") {
		t.Errorf("a missing binary was kept as previous, or the service not restarted:\n%s", strings.Join(h.link.Calls, "\n"))
	}

	h = server(t)
	engineStartsWhen(h)
	h.release(engineBinary)
	h.flags.JSON = true
	if err := h.run("host", "prod", "--yes"); err != nil {
		t.Fatal(err)
	}
	doc := h.jsonDocument()
	if doc["action"] != "upgrade" || doc["version"] != strings.TrimPrefix(sandboxEngineVersion, "v") {
		t.Errorf("the envelope is %v", doc)
	}
	if _, present := doc["from"]; present {
		t.Errorf("an upgrade over a missing binary names a from: %v", doc)
	}

	// With the service running on a binary that is gone (deleted under
	// it), the restart still costs what is running.
	h = newSandboxHarness(t, hostCommands(sandboxProbeLines(true, true, true, "active", "", true), tokenCommand(sandboxTokenFile, sandboxTestToken))...)
	withRun(h, "running")
	h.release(engineBinary)
	h.answer = false
	if err := h.run("host", "prod"); !errors.Is(err, errSandboxAborted) || len(h.prompts) != 1 || h.prompts[0] != "Install the sandbox engine (teploy-sandbox "+sandboxEngineVersion+") on prod? Its service is installed there, but its binary is missing from "+sandboxEnginePath+". The service restarts, and 1 running sandbox(es) on it are destroyed; stopped ones are kept, and every command still running in one is cut off." {
		t.Errorf("got %v, prompts %q", err, h.prompts)
	}
}

// What a restart costs, in a prompt: the engine's rule, said with the
// counts when they could be read and without them when not, and nothing
// for a service that is not running.
func TestRestartCost(t *testing.T) {
	const cut = "; stopped ones are kept, and every command still running in one is cut off"
	for _, c := range []struct {
		state           string
		destroyed, kept int
		want            string
	}{
		{"inactive", 1, 1, ""},
		{"active", -1, -1, "running sandboxes on it are destroyed, except the ones made with `--on-expire stop`, which are stopped and kept" + cut},
		{"active", 0, 0, "no sandbox is lost (none is running on it)"},
		{"active", 2, 0, "2 running sandbox(es) on it are destroyed" + cut},
		{"active", 0, 1, "1 running sandbox(es) on it, the ones made with `--on-expire stop`, are stopped and kept" + cut},
		{"active", 2, 1, "2 running sandbox(es) on it are destroyed and 1, the ones made with `--on-expire stop`, are stopped and kept" + cut},
	} {
		if got := restartCost(c.state, c.destroyed, c.kept); got != c.want {
			t.Errorf("restartCost(%q, %d, %d) = %q, want %q", c.state, c.destroyed, c.kept, got, c.want)
		}
	}
}

func TestSandboxHostChangesNothingWhenItRefuses(t *testing.T) {
	// A probe with drop-ins: everything else as a current, running install.
	withDropins := func(probe string) string {
		return strings.Replace(probe, "dropins=none", "dropins=present", 1)
	}
	for _, c := range []struct {
		name   string
		probe  string
		tunnel string
		want   string
	}{
		{"a service it did not install", sandboxProbeLines(true, true, false, "active", "0.1.0", true), "refused",
			"teploy-sandbox is already installed on prod as /etc/systemd/system/teploy-sandbox.service, and `teploy sandbox host` did not install it. Nothing was changed: this command never replaces a service it did not write.\nTo use that daemon, pass --addr and --token-file to the other sandbox commands"},
		{"a newer release than this CLI's", sandboxProbeLines(true, true, true, "active", "9.9.9", true), "refused",
			"prod already has teploy-sandbox 9.9.9, which is newer than the " + sandboxEngineVersion + " this CLI installs. Nothing was changed: update the CLI (`teploy update`) rather than downgrading the engine"},
		{"a service with drop-ins", sandboxProbeLines(true, true, true, "active", "0.1.0", true), "refused",
			"teploy-sandbox's service on prod has drop-ins (/etc/systemd/system/teploy-sandbox.service.d/*.conf), which `teploy sandbox host` neither wrote nor reads: they change how the engine runs, whatever its service file says. Nothing was changed."},
		{"an engine outside systemd", sandboxProbeLines(true, false, false, "inactive", "dev", true), "refused",
			"teploy-sandbox is on prod but is not run by systemd (a binary at /usr/local/bin/teploy-sandbox, or a process running), so `teploy sandbox host` did not install it. Nothing was changed."},
		{"a hand-started engine at the address", nothingInstalled, "",
			"the sandbox engine (version 0.1.0) already listens at 127.0.0.1:7439 on prod, started outside `teploy sandbox host`. Nothing was changed: this command never replaces what it did not install."},
		{"another service at the address", nothingInstalled, "other",
			"something that is not the sandbox engine already listens at 127.0.0.1:7439 on prod. Nothing was changed: the engine this command installs listens at that address."},
	} {
		if c.name == "a service with drop-ins" {
			c.probe = withDropins(c.probe)
		}
		h := newSandboxHarness(t, hostCommands(c.probe)...)
		switch c.tunnel {
		case "refused":
			h.link.setTunnel("refused")
		case "other":
			h.link.engineAddr = notAnEngine(t)
		}
		fetched := h.release(engineBinary)
		h.answer = true // would have said yes: it was never asked
		err := h.run("host", "prod", "--yes")
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want it to say %q", c.name, err, c.want)
		}
		if len(*fetched) != 0 || len(h.prompts) != 0 {
			t.Errorf("%s: downloaded a release (%v) or asked (%q)", c.name, *fetched, h.prompts)
		}
		if call := changedNothing(h.link.Calls); call != "" {
			t.Errorf("%s: the server was changed: %s", c.name, call)
		}
		if !h.link.isClosed() {
			t.Errorf("%s: the connection was left open", c.name)
		}
	}
}

// notAnEngine is a service the tunnel can reach that is no engine.
func notAnEngine(t *testing.T) string {
	t.Helper()
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "login", http.StatusNotFound)
	}))
	t.Cleanup(other.Close)
	return strings.TrimPrefix(other.URL, "http://")
}

func TestSandboxHostRefusesServersItCannotInstallOn(t *testing.T) {
	for _, c := range []struct {
		name  string
		extra []ssh.MockCommand
		want  string
	}{
		{"without root or passwordless sudo",
			[]ssh.MockCommand{{Match: "id -u", Output: "1000"}},
			"installing the sandbox engine needs root or passwordless sudo on prod. Connect as root (--user root), or run `teploy setup prod` first"},
		{"without Docker",
			[]ssh.MockCommand{{Match: "docker info --format", Err: errors.New("exit status 1: Cannot connect to the Docker daemon")}},
			"Docker is not usable on prod: exit status 1: Cannot connect to the Docker daemon\nRun `teploy setup prod` first (it installs Docker)"},
		{"without systemd",
			[]ssh.MockCommand{{Match: "command -v systemctl", Err: errors.New("exit status 1")}},
			"prod has no systemd, and the sandbox engine runs there as a systemd service. It cannot be installed on this server"},
		{"that is not linux",
			[]ssh.MockCommand{{Match: "uname -s", Output: "Darwin"}},
			"the sandbox engine has releases for linux servers only, and this server is darwin"},
	} {
		h := newSandboxHarness(t, hostCommands(nothingInstalled, c.extra...)...)
		h.link.setTunnel("refused")
		fetched := h.release(engineBinary)
		if err := h.run("host", "prod", "--yes"); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want it to say %q", c.name, err, c.want)
		}
		if len(*fetched) != 0 {
			t.Errorf("%s: downloaded a release", c.name)
		}
		if call := changedNothing(h.link.Calls); call != "" {
			t.Errorf("%s: the server was changed: %s", c.name, call)
		}
	}

	// A server that forbids the forward the engine is reached through.
	h := newSandboxHarness(t, hostCommands(nothingInstalled)...)
	h.link.setTunnel("prohibited")
	if err := h.run("host", "prod", "--yes"); err == nil || !strings.Contains(err.Error(), "does not allow port forwarding") {
		t.Errorf("got %v, want the server's sshd named", err)
	}
	if call := changedNothing(h.link.Calls); call != "" {
		t.Errorf("the server was changed: %s", call)
	}
}

// The binary is published only after it matched the hash this CLI pins
// and the release's checksum, and the candidate ran on the server and
// reported this release. (A binary that is not the pinned one at all is
// TestSandboxHostInstallsOnlyThePinnedBinary.)
func TestSandboxHostRefusesAReleaseItCannotTrust(t *testing.T) {
	pinned := strings.TrimPrefix(sandboxEngineVersion, "v")
	checksumsOf := func(binary []byte, asset string) []byte {
		return []byte(fmt.Sprintf("%x  %s\n", sha256.Sum256(binary), asset))
	}
	for _, c := range []struct {
		name         string
		binary       []byte
		checksums    []byte
		notPublished bool
		candidate    ssh.MockCommand
		want         string
		// staged: the release passed its checks, so a candidate was staged.
		staged bool
	}{
		{"checksums.txt that do not match", engineBinary, checksumsOf([]byte("another binary"), "teploy-sandbox_linux_amd64"), false,
			ssh.MockCommand{Match: "'/tmp/teploy-install.sandbox/teploy-sandbox' version", Output: pinned},
			"teploy-sandbox_linux_amd64 of teploy-sandbox " + sandboxEngineVersion + " does not match the release's checksums.txt either: refusing to install (pinned and downloaded ", false},
		{"a release that was never published", nil, nil, true,
			ssh.MockCommand{Match: "'/tmp/teploy-install.sandbox/teploy-sandbox' version", Output: pinned},
			"downloading teploy-sandbox_linux_amd64: GET " + sandboxEngineURL("teploy-sandbox_linux_amd64") + " returned status 404\n(there is no such release yet if this version has not been published; see https://github.com/useteploy/teploy-sandbox/releases)", false},
		{"checksums that do not name the binary", engineBinary, checksumsOf(engineBinary, "teploy-sandbox_linux_arm64"), false,
			ssh.MockCommand{Match: "'/tmp/teploy-install.sandbox/teploy-sandbox' version", Output: pinned},
			"teploy-sandbox " + sandboxEngineVersion + ": no checksum entry for teploy-sandbox_linux_amd64", false},
		{"a binary that reports another version", engineBinary, checksumsOf(engineBinary, "teploy-sandbox_linux_amd64"), false,
			ssh.MockCommand{Match: "'/tmp/teploy-install.sandbox/teploy-sandbox' version", Output: "0.0.9"},
			"the " + sandboxEngineVersion + " release of teploy-sandbox does not run on this server: candidate version mismatch: expected " + pinned, true},
		{"a binary that does not run", engineBinary, checksumsOf(engineBinary, "teploy-sandbox_linux_amd64"), false,
			ssh.MockCommand{Match: "'/tmp/teploy-install.sandbox/teploy-sandbox' version", Err: errors.New("exit status 126: Permission denied")},
			"the " + sandboxEngineVersion + " release of teploy-sandbox does not run on this server: candidate binary failed to run", true},
	} {
		h := newSandboxHarness(t, hostCommands(nothingInstalled, c.candidate)...)
		engineStartsWhen(h)
		h.release(engineBinary) // the pin is this binary's hash
		h.env.fetch = func(ctx context.Context, url string) ([]byte, error) {
			if c.notPublished {
				return nil, fmt.Errorf("GET %s returned status 404", url)
			}
			if url == sandboxEngineURL("checksums.txt") {
				return c.checksums, nil
			}
			return c.binary, nil
		}
		if err := h.run("host", "prod", "--yes"); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want it to say %q", c.name, err, c.want)
		}
		if call := changedNothing(h.link.Calls); call != "" {
			t.Errorf("%s: the server was changed: %s", c.name, call)
		}
		// Nothing is staged for a release that failed its checksum; the
		// staging of a candidate that failed on the server is cleaned up.
		staged := callsInOrder(h.link.Calls, "umask 077; mktemp -d")
		if staged != c.staged || (staged && !callsInOrder(h.link.Calls, "umask 077; mktemp -d", "rm -rf -- '/tmp/teploy-install.sandbox'")) {
			t.Errorf("%s: staged %v (want %v), or left staged:\n%s", c.name, staged, c.staged, strings.Join(h.link.Calls, "\n"))
		}
	}
}

// A user with passwordless sudo installs the same way, with every command
// that needs root going through sudo -n, and none without it.
func TestSandboxHostWithPasswordlessSudo(t *testing.T) {
	h := newSandboxHarness(t, hostCommands(nothingInstalled,
		ssh.MockCommand{Match: "id -u", Output: "1000"},
		ssh.MockCommand{Match: "sudo -n true", Output: ""},
		ssh.MockCommand{Match: "sudo -n flock -w 30", Output: ""},
		ssh.MockCommand{Match: "sudo -n install -", Output: ""},
		ssh.MockCommand{Match: "sudo -n systemctl ", Output: ""},
		ssh.MockCommand{Match: "sudo -n docker info --format", Output: "27.3.1"},
	)...)
	h.link.user = "deploy"
	engineStartsWhen(h)
	h.release(engineBinary)
	if err := h.run("host", "prod", "--yes"); err != nil {
		t.Fatal(err)
	}
	if !callsInOrder(h.link.Calls, "sudo -n docker info --format", "sudo -n flock -w 30", "sudo -n install -d -o 0 -g 0 -m 0700 '/var/lib/teploy-sandbox'",
		"sudo -n install -o 0 -g 0 -m 0644", "sudo -n systemctl daemon-reload", "sudo -n systemctl enable teploy-sandbox.service", "sudo -n systemctl start teploy-sandbox.service") {
		t.Errorf("the server was not asked through sudo:\n%s", strings.Join(h.link.Calls, "\n"))
	}
	for _, call := range h.link.Calls {
		if strings.HasPrefix(call, "flock") || strings.HasPrefix(call, "install -") || strings.HasPrefix(call, "systemctl ") || strings.HasPrefix(call, "docker ") {
			t.Errorf("a command that needs root ran without sudo: %s", call)
		}
	}
}

// When the engine does not answer after the service is started, the wait
// is bounded and the error carries the service's own log.
func TestSandboxHostSaysWhyWhenTheEngineDoesNotAnswer(t *testing.T) {
	h := newSandboxHarness(t, hostCommands(nothingInstalled,
		ssh.MockCommand{Match: "journalctl -u teploy-sandbox.service", Output: "Nov 25 16:00:02 prod teploy-sandbox[992]: listen tcp 127.0.0.1:7439: bind: address already in use"},
	)...)
	h.link.setTunnel("refused") // it never comes up
	h.release(engineBinary)
	h.env.startTimeout = 3 * time.Second
	start := h.clock
	err := h.run("host", "prod", "--yes")
	if err == nil {
		t.Fatal("an engine that never answered read as installed and running")
	}
	for _, want := range []string{"did not answer at 127.0.0.1:7439 on prod within 3s (its service is inactive)", "systemctl status teploy-sandbox.service", "bind: address already in use"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not say %q:\n%v", want, err)
		}
	}
	if waited := h.clock.Sub(start); waited < 3*time.Second {
		t.Errorf("the wait lasted %s", waited)
	}
	if !callsInOrder(h.link.Calls, "systemctl start teploy-sandbox.service") {
		t.Errorf("the service was never started:\n%s", strings.Join(h.link.Calls, "\n"))
	}
	for _, call := range h.link.Calls {
		if strings.HasPrefix(call, "python3 -c ") {
			t.Error("a run whose engine never answered was written to the history as done")
		}
	}
}
