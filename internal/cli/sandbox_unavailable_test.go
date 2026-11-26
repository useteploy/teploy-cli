package cli

// sandbox_unavailable_test.go — what is said, and what class of error it
// is, for each way the engine cannot be reached. A person is told which of
// the things that can be wrong on the server is wrong, and what to run.

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/ssh"
)

func TestSandboxEngineNotInstalled(t *testing.T) {
	h := newSandboxHarness(t,
		ssh.MockCommand{Match: "if command -v systemctl", Output: sandboxProbeLines(true, false, false, "inactive", "", false)},
		tokenCommand(sandboxTokenFile, sandboxTokenMarker), // as on a server that never had the engine
	)
	h.link.tunnel = "refused"
	err := h.run("ls")
	if err == nil {
		t.Fatal("a server with no engine listed sandboxes")
	}
	for _, want := range []string{"the sandbox engine (teploy-sandbox) is not installed on prod", "teploy sandbox host prod"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not say %q:\n%v", want, err)
		}
	}
	if !errors.Is(err, errSandboxEngineUnavailable) || classifyMachineError(err) != codeUnsupported {
		t.Errorf("not installed is the engine-unavailable class (code %s): %v", classifyMachineError(err), err)
	}
	if !h.link.isClosed() {
		t.Error("the connection was left open")
	}
}

func TestSandboxEngineNotRunning(t *testing.T) {
	journal := "Nov 25 16:00:01 prod teploy-sandbox[991]: listen tcp 127.0.0.1:7439: bind: address already in use"
	for _, state := range []string{"inactive", "failed"} {
		h := newSandboxHarness(t,
			ssh.MockCommand{Match: "if command -v systemctl", Output: sandboxProbeLines(true, true, true, state, "0.1.0", false)},
			tokenCommand(sandboxTokenFile, sandboxTokenMarker),
			ssh.MockCommand{Match: "journalctl -u teploy-sandbox.service", Output: journal},
		)
		h.link.tunnel = "refused"
		err := h.run("ls")
		if err == nil {
			t.Fatalf("%s: a server whose engine is %s listed sandboxes", state, state)
		}
		for _, want := range []string{"is installed on prod but its service is " + state, "systemctl status teploy-sandbox.service", "systemctl start teploy-sandbox.service", journal} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: the error does not say %q:\n%v", state, want, err)
			}
		}
		if !errors.Is(err, errSandboxEngineUnavailable) || classifyMachineError(err) != codeUnsupported {
			t.Errorf("%s: not running is the engine-unavailable class: %v", state, err)
		}
		if !h.link.isClosed() {
			t.Errorf("%s: the connection was left open", state)
		}
	}
}

func TestSandboxEngineActiveButNotAtTheAddress(t *testing.T) {
	h := newInstalledSandboxHarness(t)
	h.link.tunnel = "refused"
	err := h.run("ls")
	if err == nil {
		t.Fatal("a tunnel to nothing listed sandboxes")
	}
	for _, want := range []string{"service is active on prod but nothing of its own answers at 127.0.0.1:7439", "pass --addr"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not say %q:\n%v", want, err)
		}
	}
}

func TestSandboxSomethingElseIsAtTheAddress(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "login", http.StatusNotFound)
	}))
	t.Cleanup(other.Close)
	h := newSandboxHarness(t,
		ssh.MockCommand{Match: "if command -v systemctl", Output: sandboxProbeLines(true, false, false, "inactive", "", false)},
		tokenCommand(sandboxTokenFile, sandboxTokenMarker),
	)
	h.link.engineAddr = strings.TrimPrefix(other.URL, "http://") // the tunnel reaches the wrong service
	err := h.run("ls")
	if err == nil {
		t.Fatal("another service's answer read as the engine")
	}
	for _, want := range []string{"something that is not the sandbox engine answers at 127.0.0.1:7439 on prod", "teploy sandbox host prod"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not say %q:\n%v", want, err)
		}
	}
}

func TestSandboxEngineTokenRefused(t *testing.T) {
	h := newSandboxHarness(t,
		ssh.MockCommand{Match: "if command -v systemctl", Output: sandboxProbeLines(true, true, false, "active", "0.1.0", true)},
		tokenCommand(sandboxTokenFile, "some-other-daemons-token"),
	)
	err := h.run("ls")
	if err == nil {
		t.Fatal("a wrong token listed sandboxes")
	}
	for _, want := range []string{"at 127.0.0.1:7439 on prod refused the token in /var/lib/teploy-sandbox/token", "--token-file"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not say %q:\n%v", want, err)
		}
	}
	if !errors.Is(err, errSandboxEngineUnavailable) {
		t.Errorf("a refused token is the engine-unavailable class: %v", err)
	}
	// The token itself is in neither the error nor the output.
	for _, text := range []string{err.Error(), h.out.String(), h.errOut.String()} {
		if strings.Contains(text, "some-other-daemons-token") || strings.Contains(text, sandboxTestToken) {
			t.Errorf("a token is in %q", text)
		}
	}
}

// A daemon `sandbox host` did not install keeps its token where it likes:
// the path is a flag.
func TestSandboxDaemonWithItsOwnTokenFile(t *testing.T) {
	const own = "/srv/sandbox/token"
	h := newSandboxHarness(t,
		ssh.MockCommand{Match: "if command -v systemctl", Output: sandboxProbeLines(true, true, false, "active", "0.1.0", true)},
		tokenCommand(sandboxTokenFile, sandboxTokenMarker),
		tokenCommand(own, sandboxTestToken),
	)
	err := h.run("ls")
	if err == nil {
		t.Fatal("a token that is not there listed sandboxes")
	}
	for _, want := range []string{"token is not at /var/lib/teploy-sandbox/token on prod", "--token-file"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not say %q:\n%v", want, err)
		}
	}
	h.out.Reset()
	if err := h.run("ls", "--token-file", own); err != nil {
		t.Fatalf("with the daemon's own token file: %v", err)
	}
	if !strings.Contains(h.out.String(), "No sandboxes on prod") {
		t.Errorf("output %q", h.out.String())
	}
}

func TestSandboxTokenNeedsRoot(t *testing.T) {
	h := newSandboxHarness(t,
		ssh.MockCommand{Match: "if command -v systemctl", Output: sandboxProbeLines(true, true, true, "active", "0.1.0", true)},
		ssh.MockCommand{Match: "f=" + ssh.ShellQuote(sandboxTokenFile), Err: errors.New("exit status 1: sudo: a password is required")},
	)
	err := h.run("ls")
	if err == nil || !strings.Contains(err.Error(), "reading the engine's token from /var/lib/teploy-sandbox/token needs root or passwordless sudo") {
		t.Errorf("got %v, want the missing root named", err)
	}
}

func TestSandboxForwardingProhibited(t *testing.T) {
	h := newInstalledSandboxHarness(t)
	h.link.tunnel = "prohibited"
	err := h.run("ls")
	if err == nil {
		t.Fatal("a server that forbids forwarding listed sandboxes")
	}
	for _, want := range []string{"does not allow port forwarding", "AllowTcpForwarding"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not say %q:\n%v", want, err)
		}
	}
}

func TestSandboxServerWithoutSystemd(t *testing.T) {
	h := newSandboxHarness(t, ssh.MockCommand{Match: "if command -v systemctl", Output: sandboxProbeLines(false, false, false, "", "", false)})
	h.link.tunnel = "refused"
	err := h.run("ls")
	if err == nil || !strings.Contains(err.Error(), "prod has no systemd") {
		t.Errorf("got %v, want the server's missing systemd named", err)
	}
}

func TestSandboxEngineOutsideSystemd(t *testing.T) {
	h := newSandboxHarness(t,
		ssh.MockCommand{Match: "if command -v systemctl", Output: sandboxProbeLines(true, false, false, "inactive", "dev", true)},
		tokenCommand(sandboxTokenFile, sandboxTokenMarker),
	)
	h.link.tunnel = "refused"
	err := h.run("ls")
	if err == nil {
		t.Fatal("a daemon no service runs listed sandboxes")
	}
	for _, want := range []string{"teploy-sandbox is on prod but is not run by systemd", "--addr and --token-file"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not say %q:\n%v", want, err)
		}
	}
}

// A server that does not answer the question of what is installed is said
// as such. It is not read as "not installed".
func TestSandboxServerDoesNotSayWhatIsInstalled(t *testing.T) {
	h := newSandboxHarness(t) // nothing registered: the server answers no command
	h.link.tunnel = "refused"
	err := h.run("ls")
	if err == nil {
		t.Fatal("a server that answered nothing listed sandboxes")
	}
	for _, want := range []string{"at 127.0.0.1:7439 on prod could not be reached", "the server would not say what is installed there"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not say %q:\n%v", want, err)
		}
	}
	if strings.Contains(err.Error(), "is not installed on prod") {
		t.Errorf("an unanswered question was read as not installed:\n%v", err)
	}
}
