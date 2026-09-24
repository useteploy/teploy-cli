package deploy

import (
	"net"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// runTCPProbe executes the real probe command through a local shell, the
// way the remote session runs it.
func runTCPProbe(t *testing.T, port int) bool {
	t.Helper()
	cmd, ok := TCPProbeCommand("127.0.0.1", port)
	if !ok {
		t.Fatal("probe command rejected a valid host")
	}
	return exec.Command("sh", "-c", cmd).Run() == nil
}

// requireModernBash skips where bash predates 4.0: bash 3.2 (macOS's
// /bin/bash) returns 1 on a read timeout, indistinguishable from EOF, so a
// silent live listener reads as dead there (fail closed). Deploy targets
// are Linux with bash 4+; the Linux CI leg runs this pin.
func requireModernBash(t *testing.T) {
	t.Helper()
	out, err := exec.Command("bash", "-c", "echo ${BASH_VERSINFO[0]}").Output()
	if err != nil {
		t.Skip("bash not available")
	}
	if major, err := strconv.Atoi(strings.TrimSpace(string(out))); err != nil || major < 4 {
		t.Skipf("bash %q < 4: read timeout is indistinguishable from EOF", strings.TrimSpace(string(out)))
	}
}

// listen starts a local listener whose accepted connections are handled by
// onConn, and returns its port.
func listen(t *testing.T, onConn func(net.Conn)) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go onConn(c)
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

// TestTCPProbe_DeadBackendBehindProxyFails pins the C03 follow-up: Docker's
// userland proxy accepts on the published port and then closes the client
// at once when the container's listener is gone. A connect-only probe read
// that as ready; the gate must not.
func TestTCPProbe_DeadBackendBehindProxyFails(t *testing.T) {
	requireModernBash(t)
	proxyDeadBackend := listen(t, func(c net.Conn) { c.Close() })
	if runTCPProbe(t, proxyDeadBackend) {
		t.Fatal("tcp probe passed against an accept-then-close (docker-proxy, dead backend) port")
	}

	liveSilent := listen(t, func(c net.Conn) {
		time.Sleep(3 * time.Second)
		c.Close()
	})
	if !runTCPProbe(t, liveSilent) {
		t.Fatal("tcp probe failed a live client-speaks-first listener")
	}

	liveBanner := listen(t, func(c net.Conn) {
		c.Write([]byte("SSH-2.0-x\r\n"))
		time.Sleep(3 * time.Second)
		c.Close()
	})
	if !runTCPProbe(t, liveBanner) {
		t.Fatal("tcp probe failed a live server-speaks-first listener")
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	if runTCPProbe(t, closed) {
		t.Fatal("tcp probe passed against a closed port")
	}
}

func TestTCPProbeCommand_FailsClosedOnBadInput(t *testing.T) {
	for _, h := range []string{"box1", "a'b", "127.0.0.1;id"} {
		if _, ok := TCPProbeCommand(h, 80); ok {
			t.Errorf("host %q accepted", h)
		}
	}
	if _, ok := TCPProbeCommand("localhost", 0); ok {
		t.Error("port 0 accepted")
	}
	cmd, ok := TCPProbeCommand("[::1]", 8080)
	if !ok || !strings.Contains(cmd, "/dev/tcp/::1/8080") {
		t.Errorf("ipv6 probe = %q, %v", cmd, ok)
	}
}
