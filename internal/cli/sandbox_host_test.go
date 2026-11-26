package cli

// sandbox_host_test.go — `teploy sandbox host` against a server a mock
// executor answers: what it writes, in which order, what it refuses to
// touch, and what it says when the release or the server is not right.

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/events"
	"github.com/useteploy/teploy/internal/ssh"
)

// engineBinary stands in for a downloaded release: a script that reports
// the pinned version when run.
var engineBinary = []byte("#!/bin/sh\necho " + strings.TrimPrefix(sandboxEngineVersion, "v") + "\n")

// hostCommands answers a server `sandbox host` can work on. The probe is
// the caller's; everything else is a Linux x86-64 server with systemd and
// Docker, as root, where the pinned release runs and the commands succeed.
// The extra commands come first, so a test can override any of it.
func hostCommands(probe string, extra ...ssh.MockCommand) []ssh.MockCommand {
	defaults := []ssh.MockCommand{
		{Match: "if command -v systemctl", Output: probe},
		{Match: "id -u", Output: "0"},
		{Match: "command -v systemctl", Output: "/usr/bin/systemctl"},
		{Match: "docker info --format", Output: "27.3.1"},
		{Match: "uname -s", Output: "Linux"},
		{Match: "uname -m", Output: "x86_64"},
		{Match: "umask 077; mktemp -d", Output: "/tmp/teploy-install.sandbox"},
		{Match: "'/tmp/teploy-install.sandbox/teploy-sandbox' version", Output: strings.TrimPrefix(sandboxEngineVersion, "v")},
		{Match: "flock -w 30", Output: ""},
		{Match: "rm -f --", Output: ""},
		{Match: "rm -rf --", Output: ""},
		{Match: "install -", Output: ""},
		{Match: "systemctl ", Output: ""},
		{Match: "python3 -c ", Output: `{"seq":1,"ts":"2026-11-25T16:00:01Z"}`}, // the event log's writer
	}
	return append(append([]ssh.MockCommand{}, extra...), defaults...)
}

// engineStartsWhen makes the tunnel reach the engine once the service is
// started or restarted: before that nothing listens at the address.
func engineStartsWhen(h *sandboxHarness) {
	h.link.setTunnel("refused")
	h.link.beforeRun = func(cmd string) {
		if strings.Contains(cmd, "systemctl start ") || strings.Contains(cmd, "systemctl restart ") {
			h.link.setTunnel("")
		}
	}
}

// release is the pinned release for a test: a binary and its checksums,
// and the URLs that were asked for.
func (h *sandboxHarness) release(binary []byte) *[]string {
	fetched := []string{}
	h.env.fetch = func(ctx context.Context, url string) ([]byte, error) {
		fetched = append(fetched, url)
		switch url {
		case sandboxEngineURL("teploy-sandbox_linux_amd64"):
			return binary, nil
		case sandboxEngineURL("checksums.txt"):
			sum := sha256.Sum256(binary)
			return []byte(fmt.Sprintf("%x  %s\n", sum, "teploy-sandbox_linux_amd64")), nil
		}
		return nil, fmt.Errorf("GET %s returned status 404", url)
	}
	return &fetched
}

// callsInOrder reports whether each part begins a command the server was
// asked, each after the one before.
func callsInOrder(calls []string, parts ...string) bool {
	next := 0
	for _, part := range parts {
		found := -1
		for i := next; i < len(calls); i++ {
			if strings.HasPrefix(calls[i], part) {
				found = i
				break
			}
		}
		if found < 0 {
			return false
		}
		next = found + 1
	}
	return true
}

func TestSandboxSystemdUnit(t *testing.T) {
	unit := sandboxSystemdUnit()
	for _, want := range []string{
		"# " + sandboxUnitMarker,
		"ExecStart=" + sandboxEnginePath + " serve --addr 127.0.0.1:7439 --driver docker --token-file /var/lib/teploy-sandbox/token --state-dir /var/lib/teploy-sandbox/state --cache-root /var/lib/teploy-sandbox/cache",
		"User=root", "Restart=on-failure", "WantedBy=multi-user.target",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("the unit does not have %q:\n%s", want, unit)
		}
	}
	// The server's loopback, and no other address: nothing public.
	if strings.Contains(unit, "0.0.0.0") || strings.Contains(unit, "::") || strings.Count(unit, "--addr ") != 1 {
		t.Errorf("the unit listens somewhere other than the loopback once:\n%s", unit)
	}
}

// The pinned version is a release tag, and the assets are where the
// teploy-sandbox release workflow publishes them.
func TestSandboxEngineRelease(t *testing.T) {
	if !regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`).MatchString(sandboxEngineVersion) {
		t.Errorf("the pinned engine version %q is not a release tag (vMAJOR.MINOR.PATCH)", sandboxEngineVersion)
	}
	if got, want := sandboxEngineURL("teploy-sandbox_linux_amd64"), "https://github.com/useteploy/teploy-sandbox/releases/download/"+sandboxEngineVersion+"/teploy-sandbox_linux_amd64"; got != want {
		t.Errorf("the release URL is %s, want %s", got, want)
	}
}

func TestReleaseCompare(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"0.1.0", "0.1.0", 0}, {"0.1.1", "0.1.0", 1}, {"0.1.0", "0.2.0", -1}, {"1.0.0", "0.9.9", 1},
		{"0.1.0-rc.1", "0.1.0", -1}, {"0.1.0", "0.1.0-rc.1", 1}, {"0.1.0-rc.2", "0.1.0-rc.10", -1},
		{"0.1.0-rc.2", "0.1.0-rc.10", -1}, {"0.1.0-rc.1", "0.1.0-rc.1.1", -1}, {"0.1.0-1", "0.1.0-alpha", -1},
		{"0.1.0-alpha", "0.1.0-beta", -1}, {"0.1.0-beta.11", "0.1.0-beta.2", 1},
		{"dev", "0.1.0", 0}, {"0.1", "0.1.0", 0}, {"0.1.0", "x", 0}, {"01.1.0", "0.1.0", 0}, {"v0.1.0", "0.1.0", 0},
	} {
		if got := releaseCompare(c.a, c.b); got != c.want {
			t.Errorf("releaseCompare(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestSandboxHostInstalls(t *testing.T) {
	h := newSandboxHarness(t, hostCommands(sandboxProbeLines(true, false, false, "inactive", "", false))...)
	engineStartsWhen(h)
	var staged []byte
	start := h.link.beforeRun
	h.link.beforeRun = func(cmd string) {
		start(cmd)
		if strings.HasPrefix(cmd, "install -o 0 -g 0 -m 0644") {
			staged = h.link.Files["/tmp/teploy-sandbox.service"] // while it is still on the server
		}
	}
	fetched := h.release(engineBinary)
	h.answer = true
	if err := h.run("host", "prod"); err != nil {
		t.Fatal(err)
	}

	if len(h.prompts) != 1 || !strings.Contains(h.prompts[0], "Install the sandbox engine (teploy-sandbox "+sandboxEngineVersion+") on prod as a systemd service at 127.0.0.1:7439, running as root with access to Docker?") {
		t.Errorf("the prompt was %q", h.prompts)
	}
	if strings.Join(*fetched, " ") != sandboxEngineURL("teploy-sandbox_linux_amd64")+" "+sandboxEngineURL("checksums.txt") {
		t.Errorf("the release fetched was %v", *fetched)
	}

	// The order on the server: looked at, the candidate staged and run,
	// published, then the state directory, the unit, and the service.
	calls := h.link.Calls
	if !callsInOrder(calls,
		"id -u", "command -v systemctl", "docker info --format", "if command -v systemctl", "uname -s", "uname -m",
		"umask 077; mktemp -d", "'/tmp/teploy-install.sandbox/teploy-sandbox' version", "flock -w 30",
		"install -d -o 0 -g 0 -m 0700 '/var/lib/teploy-sandbox'", "UPLOAD:/tmp/teploy-sandbox.service",
		"install -o 0 -g 0 -m 0644 '/tmp/teploy-sandbox.service' '/etc/systemd/system/teploy-sandbox.service'",
		"systemctl daemon-reload", "systemctl enable teploy-sandbox.service", "systemctl start teploy-sandbox.service",
	) || !callsInOrder(calls, "systemctl start teploy-sandbox.service", "python3 -c ") {
		t.Errorf("the server was not asked in the order of an install:\n%s", strings.Join(calls, "\n"))
	}
	// The published candidate was the one that ran and reported its version.
	for _, call := range calls {
		if strings.HasPrefix(call, "flock -w 30") && (!strings.Contains(call, "'/usr/local/bin/teploy-sandbox.install.lock'") || !strings.Contains(call, "/tmp/teploy-install.sandbox/teploy-sandbox") || !strings.Contains(call, "/usr/local/bin/teploy-sandbox")) {
			t.Errorf("the publication is %s", call)
		}
	}
	// The unit that went up is the unit this CLI generates.
	if string(staged) != sandboxSystemdUnit() {
		t.Errorf("the unit staged on the server is %q", staged)
	}
	// Nothing is left staged before the service is touched, and the
	// connection is closed.
	if !callsInOrder(calls, "flock -w 30", "rm -rf -- '/tmp/teploy-install.sandbox'", "systemctl enable teploy-sandbox.service") || !callsInOrder(calls, "install -o 0 -g 0 -m 0644", "rm -f -- '/tmp/teploy-sandbox.service'", "systemctl enable teploy-sandbox.service") {
		t.Errorf("the staging was not cleaned up:\n%s", strings.Join(calls, "\n"))
	}
	if !h.link.isClosed() {
		t.Error("the connection was left open")
	}

	// The server's history got one append, where the fleet events go.
	appends := 0
	for _, call := range calls {
		if strings.HasPrefix(call, "python3 -c ") && strings.Contains(call, "append '_fleet'") {
			appends++
		}
	}
	if appends != 1 {
		t.Errorf("%d appends to the server's history (of %d commands)", appends, len(calls))
	}
	for _, want := range []string{"Installed the sandbox engine (teploy-sandbox " + sandboxEngineVersion + ") on prod and started it at 127.0.0.1:7439", "teploy sandbox new --host prod --image alpine:3.20"} {
		if !strings.Contains(h.out.String(), want) {
			t.Errorf("the output does not say %q:\n%s", want, h.out.String())
		}
	}
}

// What `sandbox host` leaves in the server's history: the kind, and only
// the detail the kind allows (events.Sanitize drops the rest).
func TestSandboxHostEvent(t *testing.T) {
	for action, from := range map[sandboxAction]string{sandboxHostInstall: "", sandboxHostUpgrade: "0.1.0", sandboxHostStart: "0.1.0"} {
		event := sandboxHostEvent(sandboxHostPlan{action: action, from: from})
		events.Sanitize(&event)
		want := map[string]any{"action": string(action), "version": strings.TrimPrefix(sandboxEngineVersion, "v")}
		if action == sandboxHostUpgrade {
			want["from"] = from
		}
		if event.App != "_fleet" || event.Kind != events.KindSandboxHost || event.Outcome != events.Succeeded || fmt.Sprint(event.Detail) != fmt.Sprint(want) {
			t.Errorf("%s: the event is %+v, want detail %v", action, event, want)
		}
	}
}

func TestSandboxHostInstallsJSON(t *testing.T) {
	h := newSandboxHarness(t, hostCommands(sandboxProbeLines(true, false, false, "inactive", "", false))...)
	engineStartsWhen(h)
	h.release(engineBinary)
	h.flags.JSON = true
	if err := h.run("host", "prod", "--yes"); err != nil {
		t.Fatal(err)
	}
	doc := h.jsonDocument()
	want := map[string]any{
		"machine_interface": float64(2), "server": "prod", "host": "prod", "ok": true,
		"action": "install", "version": strings.TrimPrefix(sandboxEngineVersion, "v"),
		"addr": "127.0.0.1:7439", "token_file": "/var/lib/teploy-sandbox/token",
	}
	for key, value := range want {
		if doc[key] != value {
			t.Errorf("%s is %v, want %v", key, doc[key], value)
		}
	}
	if _, present := doc["from"]; present || len(doc) != len(want) {
		t.Errorf("a fresh install has a from, or another key: %v", doc)
	}
	if len(h.prompts) != 0 {
		t.Errorf("--yes still asked: %q", h.prompts)
	}
	// Progress went to stderr: stdout was the document alone.
	if !strings.Contains(h.errOut.String(), "Waiting for the engine to answer") {
		t.Errorf("stderr held %q", h.errOut.String())
	}
	var whole any
	if err := json.Unmarshal(h.out.Bytes(), &whole); err != nil {
		t.Errorf("stdout is not one document: %v", err)
	}
}

func TestSandboxHostLeavesACurrentInstallAlone(t *testing.T) {
	h := newSandboxHarness(t, hostCommands(sandboxProbeLines(true, true, true, "active", strings.TrimPrefix(sandboxEngineVersion, "v"), true))...)
	fetched := h.release(engineBinary)
	if err := h.run("host", "prod"); err != nil {
		t.Fatal(err)
	}
	if len(*fetched) != 0 {
		t.Errorf("a server with this version had a release downloaded: %v", *fetched)
	}
	for _, command := range h.link.Calls {
		if strings.HasPrefix(command, "flock") || strings.HasPrefix(command, "install -") || strings.Contains(command, "systemctl restart") || strings.Contains(command, "systemctl start") || strings.Contains(command, "python3 -c ") {
			t.Errorf("a current install was changed: %s", command)
		}
	}
	if !callsInOrder(h.link.Calls, "systemctl enable teploy-sandbox.service") {
		t.Errorf("the service was not made sure enabled:\n%s", strings.Join(h.link.Calls, "\n"))
	}
	if len(h.prompts) != 0 {
		t.Errorf("nothing to ask about, but asked: %q", h.prompts)
	}
	if !strings.Contains(h.out.String(), "The sandbox engine on prod is up to date ("+sandboxEngineVersion+") and running") {
		t.Errorf("output %q", h.out.String())
	}

	h.out.Reset()
	h.flags.JSON = true
	if err := h.run("host", "prod", "--yes"); err != nil {
		t.Fatal(err)
	}
	if doc := h.jsonDocument(); doc["action"] != "unchanged" || doc["version"] != strings.TrimPrefix(sandboxEngineVersion, "v") {
		t.Errorf("the envelope is %v", doc)
	}
}

func TestSandboxHostStartsAStoppedService(t *testing.T) {
	h := newSandboxHarness(t, hostCommands(sandboxProbeLines(true, true, true, "inactive", strings.TrimPrefix(sandboxEngineVersion, "v"), false))...)
	engineStartsWhen(h)
	fetched := h.release(engineBinary)
	if err := h.run("host", "prod"); err != nil {
		t.Fatal(err)
	}
	if len(*fetched) != 0 || len(h.prompts) != 0 {
		t.Errorf("starting a stopped service downloaded a release (%v) or asked (%q)", *fetched, h.prompts)
	}
	for _, command := range h.link.Calls {
		if strings.HasPrefix(command, "flock") || strings.HasPrefix(command, "install -") || strings.Contains(command, "systemctl restart") {
			t.Errorf("starting a stopped service changed the install: %s", command)
		}
	}
	if !callsInOrder(h.link.Calls, "systemctl enable teploy-sandbox.service", "systemctl start teploy-sandbox.service") {
		t.Errorf("the service was not started:\n%s", strings.Join(h.link.Calls, "\n"))
	}
	if !strings.Contains(h.out.String(), "Started the sandbox engine's service on prod ("+sandboxEngineVersion+")") {
		t.Errorf("output %q", h.out.String())
	}
	if !callsInOrder(h.link.Calls, "systemctl start teploy-sandbox.service", "python3 -c ") {
		t.Errorf("the start was not written to the server's history:\n%s", strings.Join(h.link.Calls, "\n"))
	}
}
