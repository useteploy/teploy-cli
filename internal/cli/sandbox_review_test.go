package cli

// sandbox_review_test.go — the three tests the review of PR #77 wrote
// against the branch, as it stood: each failed there (a refused exec
// printed an exec document with a null exit_code, --addr took any host,
// a command's own exit status was classed internal). They pass with the
// fixes, and they stay as the review wrote them.

import (
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/sandbox"
)

// A refusal before the stream (404: nothing ran) must not put an exec
// document with exit_code null on stdout: the contract says null means
// "the stream ended before the exit was known" (unknown outcome).
func TestReviewExecJSONRefusalPrintsNoExecDocument(t *testing.T) {
	h := newInstalledSandboxHarness(t)
	withRun(h, "running")
	h.flags.JSON = true
	h.flags.Host = sandboxTestHost
	err := h.run("exec", sandboxTestID(9), "--", "true")
	if err == nil {
		t.Fatal("want an error")
	}
	if strings.TrimSpace(h.out.String()) != "" {
		t.Errorf("a refused exec printed an exec document on stdout: %s", h.out.String())
	}
}

// The bearer token must not be sent to an address off the server's loopback.
func TestReviewParseAddrRefusesNonLoopback(t *testing.T) {
	for _, addr := range []string{"203.0.113.7:80", "attacker.example:443", "10.0.0.5:7439"} {
		if _, _, err := sandbox.ParseAddr(addr); err == nil {
			t.Errorf("ParseAddr(%q) accepted a non-loopback address; the token would travel there in cleartext", addr)
		}
	}
	// 10.0.0.5 is taken with the opt-in (--allow-remote-addr: ParseAddrRemote
	// here), and only with it.
	if _, _, err := sandbox.ParseAddrRemote("10.0.0.5:7439"); err != nil {
		t.Errorf("with --allow-remote-addr, ParseAddrRemote(%q) refused the address: %v", "10.0.0.5:7439", err)
	}
}

// A command's own exit status is classified as an infrastructure failure.
func TestReviewCommandExitIsNotInternal(t *testing.T) {
	h := newInstalledSandboxHarness(t)
	id := withRun(h, "running")
	h.flags.JSON = true
	h.flags.Host = sandboxTestHost
	err := h.run("exec", id, "--", "exit", "3")
	if got := classifyMachineError(err); got == codeInternal {
		t.Errorf("a command exiting 3 is reported as %q in the machine error envelope", got)
	}
}
