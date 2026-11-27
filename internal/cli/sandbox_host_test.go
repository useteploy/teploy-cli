package cli

// sandbox_host_test.go — `teploy sandbox host` against a server a mock
// executor answers: what it writes, in which order, what it refuses to
// touch, and what it says when the release or the server is not right.

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
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
		{Match: "cp -p -- '/usr/local/bin/teploy-sandbox' '/usr/local/bin/teploy-sandbox.prev'", Output: ""},
		{Match: "flock -w 30", Output: ""},
		{Match: "rm -rf --", Output: ""},
		// The service file as this CLI writes it: a re-run on it changes nothing.
		{Match: "cat -- '/etc/systemd/system/teploy-sandbox.service'", Output: sandboxSystemdUnit()},
		{Match: "install -", Output: ""},
		{Match: "systemctl ", Output: ""},
		{Match: "python3 -c ", Output: `{"seq":1,"ts":"2026-11-25T16:00:01Z"}`}, // the event log's writer
	}
	return append(append([]ssh.MockCommand{}, extra...), defaults...)
}

const (
	stagedBinary = "'/tmp/teploy-install.sandbox/teploy-sandbox'"
	stagedUnit   = "'/tmp/teploy-install.sandbox/teploy-sandbox.service'"
	keepPrevious = "cp -p -- '/usr/local/bin/teploy-sandbox' '/usr/local/bin/teploy-sandbox.prev'"
	putBackPrev  = "mv -f -- '/usr/local/bin/teploy-sandbox.prev' '/usr/local/bin/teploy-sandbox'"
	catUnit      = "cat -- '/etc/systemd/system/teploy-sandbox.service'"
)

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

// engineAnswersOnlyWithTheOldOne is a change whose new engine never comes
// up: from the first start of the service on (the restart that ran the
// change) the tunnel is refused, until what was there is put back.
func engineAnswersOnlyWithTheOldOne(h *sandboxHarness) {
	restarted, putBack := false, false
	h.link.setTunnel("refused")
	h.link.beforeRun = func(cmd string) {
		switch {
		case strings.Contains(cmd, "systemctl start "), strings.Contains(cmd, "systemctl restart "):
			if !putBack {
				h.link.setTunnel("refused")
			}
			restarted = true
		case restarted && (strings.HasPrefix(cmd, putBackPrev) || strings.HasPrefix(cmd, "install -o 0 -g 0 -m 0644 "+stagedUnit)):
			putBack = true
			h.link.setTunnel("") // what was there is back
		}
	}
}

// engineNeverAnswersAgain is the same, with an engine that does not answer
// when put back either.
func engineNeverAnswersAgain(h *sandboxHarness) {
	h.link.setTunnel("refused")
	h.link.beforeRun = func(cmd string) {
		if strings.Contains(cmd, "systemctl start ") || strings.Contains(cmd, "systemctl restart ") {
			h.link.setTunnel("refused")
		}
	}
}

// lastCall is the last command the server was asked.
func lastCall(calls []string) string {
	if len(calls) == 0 {
		return ""
	}
	return calls[len(calls)-1]
}

// release is the pinned release for a test: this CLI build pins the
// binary's hash, the release serves that binary and its checksums, and the
// URLs that were asked for are kept.
func (h *sandboxHarness) release(binary []byte) *[]string {
	pinEngine(h.t, "linux/amd64", fmt.Sprintf("%x", sha256.Sum256(binary)))
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

// pinEngine is what this CLI build pins for a platform, for one test: the
// table is empty until the release is published, and every test that gets
// as far as a download needs a pin of its own.
func pinEngine(t *testing.T, platform, sum string) {
	t.Helper()
	before, had := sandboxEngineSHA256[platform]
	sandboxEngineSHA256[platform] = sum
	t.Cleanup(func() {
		if had {
			sandboxEngineSHA256[platform] = before
			return
		}
		delete(sandboxEngineSHA256, platform)
	})
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

// The pinned hashes are one for each platform the release is published
// for, and no other: a platform without an entry is one `host` refuses.
// Each is empty (until the release is published) or a SHA-256.
func TestSandboxEnginePinnedHashes(t *testing.T) {
	platforms := make([]string, 0, len(sandboxEngineSHA256))
	sum := regexp.MustCompile(`^[0-9a-f]{64}$`)
	for platform, hash := range sandboxEngineSHA256 {
		platforms = append(platforms, platform)
		if hash != "" && !sum.MatchString(hash) {
			t.Errorf("the hash pinned for %s is %q: want a SHA-256 in lower-case hex, or empty", platform, hash)
		}
	}
	sort.Strings(platforms)
	if got := strings.Join(platforms, " "); got != "linux/amd64 linux/arm64" {
		t.Errorf("the platforms with a pinned hash are %q, want the two the release publishes", got)
	}
}

// A binary this CLI does not pin is refused, however consistent the
// release it came from is: a substituted binary with a checksums.txt
// rewritten for it passes the checksum and fails here. With no pin for
// the platform at all, nothing is even downloaded.
func TestSandboxHostInstallsOnlyThePinnedBinary(t *testing.T) {
	sumOf := func(binary []byte) string { return fmt.Sprintf("%x", sha256.Sum256(binary)) }
	substituted := []byte("#!/bin/sh\necho " + strings.TrimPrefix(sandboxEngineVersion, "v") + "\n# not the pinned one\n")

	h := newSandboxHarness(t, hostCommands(nothingInstalled)...)
	engineStartsWhen(h)
	h.release(engineBinary)
	// The release serves another binary, with a checksums.txt that agrees
	// with it.
	h.env.fetch = func(ctx context.Context, url string) ([]byte, error) {
		if url == sandboxEngineURL("checksums.txt") {
			return []byte(sumOf(substituted) + "  teploy-sandbox_linux_amd64\n"), nil
		}
		return substituted, nil
	}
	err := h.run("host", "prod", "--yes")
	if err == nil || !strings.Contains(err.Error(), "teploy-sandbox_linux_amd64 of teploy-sandbox "+sandboxEngineVersion+" is not the binary this CLI pins: refusing to install (pinned "+sumOf(engineBinary)+", downloaded "+sumOf(substituted)+")") {
		t.Errorf("got %v, want the substituted binary refused by the pinned hash", err)
	}
	if call := changedNothing(h.link.Calls); call != "" {
		t.Errorf("the server was changed: %s", call)
	}
	if callsInOrder(h.link.Calls, "umask 077; mktemp -d") {
		t.Error("a binary that failed its pinned hash was staged on the server")
	}

	// The pin and checksums.txt must agree: a release whose checksums.txt
	// names another binary than the pinned one is not installed either.
	h = newSandboxHarness(t, hostCommands(nothingInstalled)...)
	engineStartsWhen(h)
	h.release(engineBinary)
	h.env.fetch = func(ctx context.Context, url string) ([]byte, error) {
		if url == sandboxEngineURL("checksums.txt") {
			return []byte(sumOf(substituted) + "  teploy-sandbox_linux_amd64\n"), nil
		}
		return engineBinary, nil
	}
	if err := h.run("host", "prod", "--yes"); err == nil || !strings.Contains(err.Error(), "does not match the release's checksums.txt either: refusing to install") {
		t.Errorf("got %v, want a release whose checksums.txt disagrees with the pin refused", err)
	}
	if call := changedNothing(h.link.Calls); call != "" {
		t.Errorf("the server was changed: %s", call)
	}

	// Until the hash is pinned, `host` has nothing to check a download
	// against: it refuses before it downloads anything, and says why.
	h = newSandboxHarness(t, hostCommands(nothingInstalled)...)
	engineStartsWhen(h)
	pinEngine(t, "linux/amd64", "")
	fetched := []string{}
	h.env.fetch = func(ctx context.Context, url string) ([]byte, error) {
		fetched = append(fetched, url)
		return engineBinary, nil
	}
	err = h.run("host", "prod", "--yes")
	for _, want := range []string{"this CLI build has no pinned hash for the sandbox engine: " + sandboxEngineVersion + " on linux/amd64", "Nothing was changed", "teploy update"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not say %q:\n%v", want, err)
		}
	}
	if classifyMachineError(err) != codeUnsupported {
		t.Errorf("a CLI build without the pin is the unsupported class, got %s", classifyMachineError(err))
	}
	if len(fetched) != 0 {
		t.Errorf("with nothing to check a download against, %v was fetched", fetched)
	}
	if call := changedNothing(h.link.Calls); call != "" {
		t.Errorf("the server was changed: %s", call)
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

// The probe reads whether the service has drop-ins via systemd's
// DropInPaths (covering /etc/systemd/system, /run/systemd/system,
// /etc/systemd/system.control, /usr/lib/systemd/system, and top-level
// service.d/), always exits 0, and probeSandboxEngine parses empty vs
// non-empty DropInPaths.
func TestSandboxProbeReadsDropins(t *testing.T) {
	command := sandboxProbeCommand()
	wantDropinsCmd := `echo "dropins=$(systemctl show -p DropInPaths --value teploy-sandbox.service 2>/dev/null || true)"`
	if !strings.Contains(command, wantDropinsCmd) {
		t.Errorf("the probe does not read DropInPaths via systemctl show:\n%s", command)
	}
	if strings.Contains(command, "/etc/systemd/system/teploy-sandbox.service.d/*.conf") {
		t.Errorf("the probe still hardcodes a single drop-in directory:\n%s", command)
	}

	// Run the real probe command in sh against a stub systemctl on PATH to
	// confirm it always exits 0 and emits key=value lines for empty,
	// single, multi-location, and failing systemctl invocations.
	for _, c := range []struct {
		name        string
		script      string
		wantDropins bool
		wantPaths   string
	}{
		{
			name:        "empty DropInPaths",
			script:      "#!/bin/sh\nif [ \"$1\" = show ]; then echo ''; fi\nexit 0\n",
			wantDropins: false,
			wantPaths:   "",
		},
		{
			name:        "runtime drop-in",
			script:      "#!/bin/sh\nif [ \"$1\" = show ]; then echo '/run/systemd/system/teploy-sandbox.service.d/override.conf'; fi\nexit 0\n",
			wantDropins: true,
			wantPaths:   "/run/systemd/system/teploy-sandbox.service.d/override.conf",
		},
		{
			name:        "multiple drop-in locations",
			script:      "#!/bin/sh\nif [ \"$1\" = show ]; then echo '/etc/systemd/system.control/teploy-sandbox.service.d/50-MemoryMax.conf /usr/lib/systemd/system/service.d/10-env.conf'; fi\nexit 0\n",
			wantDropins: true,
			wantPaths:   "/etc/systemd/system.control/teploy-sandbox.service.d/50-MemoryMax.conf",
		},
		{
			// Fedora ships this for every service; it is the distribution's,
			// not a change to how this engine runs, and must not refuse.
			name:        "only a distro-wide drop-in for every service",
			script:      "#!/bin/sh\nif [ \"$1\" = show ]; then echo '/usr/lib/systemd/system/service.d/10-timeout-abort.conf'; fi\nexit 0\n",
			wantDropins: false,
			wantPaths:   "",
		},
		{
			name:        "systemctl show fails",
			script:      "#!/bin/sh\nif [ \"$1\" = show ]; then echo 'Failed to connect to bus' >&2; exit 1; fi\nexit 0\n",
			wantDropins: false,
			wantPaths:   "",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			binDir := t.TempDir()
			systemctlPath := filepath.Join(binDir, "systemctl")
			if err := os.WriteFile(systemctlPath, []byte(c.script), 0o755); err != nil {
				t.Fatal(err)
			}
			sh := exec.Command("sh", "-c", command)
			sh.Env = append(os.Environ(), "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			out, err := sh.Output()
			if err != nil {
				t.Fatalf("sandboxProbeCommand exited with error: %v\nout:\n%s", err, out)
			}
			h := newSandboxHarness(t, ssh.MockCommand{Match: "if command -v systemctl", Output: string(out)})
			probe := probeSandboxEngine(context.Background(), h.link)
			if !probe.ok {
				t.Fatalf("probe.ok = false; out:\n%s", out)
			}
			if probe.dropins != c.wantDropins || probe.dropinPaths != c.wantPaths {
				t.Errorf("probe dropins = (%v, %q), want (%v, %q)\nout:\n%s", probe.dropins, probe.dropinPaths, c.wantDropins, c.wantPaths, out)
			}
		})
	}

	// Direct parsing of empty, whitespace-only, and non-empty DropInPaths lines.
	for _, c := range []struct {
		line        string
		wantDropins bool
		wantPaths   string
	}{
		{"dropins=", false, ""},
		{"dropins=   \t ", false, ""},
		{"dropins=/etc/systemd/system/teploy-sandbox.service.d/override.conf", true, "/etc/systemd/system/teploy-sandbox.service.d/override.conf"},
		{"dropins=/etc/systemd/system/service.d/10-a.conf /run/systemd/system/teploy-sandbox.service.d/20-b.conf", true, "/run/systemd/system/teploy-sandbox.service.d/20-b.conf"},
		{"dropins=/usr/lib/systemd/system/service.d/10-timeout-abort.conf", false, ""},
	} {
		raw := strings.Replace(sandboxProbeLines(true, true, true, "active", "0.1.0", true), "dropins=", c.line, 1)
		h := newSandboxHarness(t, ssh.MockCommand{Match: "if command -v systemctl", Output: raw})
		probe := probeSandboxEngine(context.Background(), h.link)
		if !probe.ok || probe.dropins != c.wantDropins || probe.dropinPaths != c.wantPaths {
			t.Errorf("parse %q => ok=%v dropins=%v paths=%q, want ok=true dropins=%v paths=%q",
				c.line, probe.ok, probe.dropins, probe.dropinPaths, c.wantDropins, c.wantPaths)
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
			staged = h.link.Files["/tmp/teploy-install.sandbox/teploy-sandbox.service"] // while it is still on the server
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
		"install -d -o 0 -g 0 -m 0700 '/var/lib/teploy-sandbox'", "UPLOAD:/tmp/teploy-install.sandbox/teploy-sandbox.service",
		"install -o 0 -g 0 -m 0644 "+stagedUnit+" '/etc/systemd/system/teploy-sandbox.service'",
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
	// Everything was staged in the one private directory the binary was
	// (never at a fixed path in /tmp), and it is the last thing removed.
	for path := range h.link.Files {
		if path != "/tmp/teploy-install.sandbox/teploy-sandbox" && path != "/tmp/teploy-install.sandbox/teploy-sandbox.service" {
			t.Errorf("%s was uploaded outside the staging directory", path)
		}
	}
	// (h.link.Calls, read now that the run has returned: the removal is the
	// run's last act.)
	all := h.link.Calls
	if !callsInOrder(all, "systemctl enable teploy-sandbox.service", "rm -rf -- '/tmp/teploy-install.sandbox'") || lastCall(all) != "rm -rf -- '/tmp/teploy-install.sandbox'" {
		t.Errorf("the staging directory was not removed after everything else (last: %s):\n%s", lastCall(all), strings.Join(all, "\n"))
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
	for action, from := range map[sandboxAction]string{sandboxHostInstall: "", sandboxHostUpgrade: "0.1.0", sandboxHostRepair: "0.1.0", sandboxHostStart: "0.1.0"} {
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
	// An upgrade over a missing binary has no version to name.
	event := sandboxHostEvent(sandboxHostPlan{action: sandboxHostUpgrade})
	events.Sanitize(&event)
	if want := map[string]any{"action": "upgrade", "version": strings.TrimPrefix(sandboxEngineVersion, "v")}; fmt.Sprint(event.Detail) != fmt.Sprint(want) {
		t.Errorf("an upgrade with no previous version: the event is %+v, want detail %v", event.Detail, want)
	}
	if doc := sandboxHostEnvelope("prod", "prod", sandboxHostPlan{action: sandboxHostUpgrade}, "0.1.0", "127.0.0.1:7439", sandboxTokenFile); doc.From != "" {
		t.Errorf("an upgrade with no previous version names from %q", doc.From)
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
		if strings.HasPrefix(command, "flock") || strings.HasPrefix(command, "install -") || strings.HasPrefix(command, "UPLOAD:") || strings.Contains(command, "systemctl daemon-reload") || strings.Contains(command, "systemctl restart") || strings.Contains(command, "systemctl start") || strings.Contains(command, "python3 -c ") {
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
		if strings.HasPrefix(command, "flock") || strings.HasPrefix(command, "install -") || strings.HasPrefix(command, "UPLOAD:") || strings.Contains(command, "systemctl daemon-reload") || strings.Contains(command, "systemctl restart") {
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
