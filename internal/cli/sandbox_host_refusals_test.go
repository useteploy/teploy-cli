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

func TestSandboxHostUpgrades(t *testing.T) {
	// An older release is running, with a sandbox in it.
	h := newSandboxHarness(t, hostCommands(sandboxProbeLines(true, true, true, "active", "0.0.9", true), tokenCommand(sandboxTokenFile, sandboxTestToken))...)
	withRun(h, "running")
	fetched := h.release(engineBinary)
	h.answer = false
	err := h.run("host", "prod")
	if !errors.Is(err, errSandboxAborted) {
		t.Fatalf("got %v, want the refusal to upgrade without a confirmation", err)
	}
	// The prompt says what a restart costs: the running sandboxes.
	if len(h.prompts) != 1 || !strings.Contains(h.prompts[0], "Upgrade the sandbox engine on prod from 0.0.9 to "+sandboxEngineVersion+"? Its service restarts, and 1 running sandbox(es) on it are destroyed; stopped ones are kept.") {
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
	calls := h.link.Calls
	if !callsInOrder(calls, "'/tmp/teploy-install.sandbox/teploy-sandbox' version", "flock -w 30", "systemctl daemon-reload", "systemctl enable teploy-sandbox.service", "systemctl restart teploy-sandbox.service", "python3 -c ") {
		t.Errorf("the server was not asked in the order of an upgrade:\n%s", strings.Join(calls, "\n"))
	}
	if !strings.Contains(h.out.String(), "Upgraded the sandbox engine on prod from 0.0.9 to "+sandboxEngineVersion+" and restarted it") {
		t.Errorf("output %q", h.out.String())
	}

	// Under --json the document names the version that was there.
	h = newSandboxHarness(t, hostCommands(sandboxProbeLines(true, true, true, "active", "0.0.9", true), tokenCommand(sandboxTokenFile, sandboxTestToken))...)
	h.release(engineBinary)
	h.flags.JSON = true
	if err := h.run("host", "prod", "--yes"); err != nil {
		t.Fatal(err)
	}
	if doc := h.jsonDocument(); doc["action"] != "upgrade" || doc["from"] != "0.0.9" || doc["version"] != strings.TrimPrefix(sandboxEngineVersion, "v") {
		t.Errorf("the envelope is %v", doc)
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
}

func TestSandboxHostChangesNothingWhenItRefuses(t *testing.T) {
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
		{"an engine outside systemd", sandboxProbeLines(true, false, false, "inactive", "dev", true), "refused",
			"teploy-sandbox is on prod but is not run by systemd (a binary at /usr/local/bin/teploy-sandbox, or a process running), so `teploy sandbox host` did not install it. Nothing was changed."},
		{"a hand-started engine at the address", nothingInstalled, "",
			"the sandbox engine (version 0.1.0) already listens at 127.0.0.1:7439 on prod, started outside `teploy sandbox host`. Nothing was changed: this command never replaces what it did not install."},
		{"another service at the address", nothingInstalled, "other",
			"something that is not the sandbox engine already listens at 127.0.0.1:7439 on prod. Nothing was changed: the engine this command installs listens at that address."},
	} {
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

// The binary is published only after the release's checksum matched and the
// candidate ran on the server and reported this release.
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
		// staged: the release passed its checksum, so a candidate was staged.
		staged bool
	}{
		{"a checksum that does not match", engineBinary, checksumsOf([]byte("another binary"), "teploy-sandbox_linux_amd64"), false,
			ssh.MockCommand{Match: "'/tmp/teploy-install.sandbox/teploy-sandbox' version", Output: pinned},
			"checksum mismatch for teploy-sandbox_linux_amd64 of teploy-sandbox " + sandboxEngineVersion + ": refusing to install (want ", false},
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
