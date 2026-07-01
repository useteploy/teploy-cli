package ssh

import (
	"os/exec"
	"strings"
	"testing"
)

// TestShellQuote_PreventsInjection actually executes ShellQuote's output
// through a real POSIX shell (not just inspecting the quoted string) to
// prove injection payloads come out as inert literal text, not commands.
// This is the property every ssh.Executor.Run(ctx, cmd string) call site
// depends on — see internal/network/network.go, internal/docker/docker.go,
// internal/accessories/accessories.go for real command strings built this
// way.
func TestShellQuote_PreventsInjection(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no /bin/sh available")
	}

	payloads := []string{
		"$(touch /tmp/teploy_shellquote_pwned)",
		"`touch /tmp/teploy_shellquote_pwned`",
		"; touch /tmp/teploy_shellquote_pwned",
		"&& touch /tmp/teploy_shellquote_pwned",
		"| touch /tmp/teploy_shellquote_pwned",
		"' ; touch /tmp/teploy_shellquote_pwned; echo '",
		"normal-value",
		"has spaces",
		"",
	}

	for _, p := range payloads {
		cmd := "echo " + ShellQuote(p)
		out, err := exec.Command("sh", "-c", cmd).CombinedOutput()
		if err != nil {
			t.Errorf("ShellQuote(%q): command failed: %v (output: %s)", p, err, out)
			continue
		}
		got := strings.TrimRight(string(out), "\n")
		if got != p {
			t.Errorf("ShellQuote(%q): shell echoed %q, want the literal payload back unexecuted", p, got)
		}
	}
}

func TestShellQuote_EmbedsSafelyMidToken(t *testing.T) {
	// Several call sites concatenate a quoted value into the middle of a
	// larger unquoted token, e.g. "label=teploy.app=" + ShellQuote(app) in
	// internal/docker/docker.go's ListContainers. Confirm that pattern is
	// still safe: the quoted segment must not let shell metacharacters
	// escape into the surrounding unquoted context.
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no /bin/sh available")
	}

	malicious := "x' ; touch /tmp/teploy_shellquote_pwned #"
	cmd := "echo prefix=" + ShellQuote(malicious) + "=suffix"
	out, err := exec.Command("sh", "-c", cmd).CombinedOutput()
	if err != nil {
		t.Fatalf("command failed: %v (output: %s)", err, out)
	}
	got := strings.TrimRight(string(out), "\n")
	want := "prefix=" + malicious + "=suffix"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
