package targetguard

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/ssh"
)

func runWith(t *testing.T, response string) (*ssh.MockExecutor, string, error) {
	t.Helper()
	exec := ssh.NewMockExecutor("target",
		ssh.MockCommand{Match: "sh " + helperRemote, Output: response},
	)
	out, err := Run(context.Background(), exec, "web", 3, "true")
	return exec, out, err
}

func TestRunMapsProtocolOutcomes(t *testing.T) {
	exec, out, err := runWith(t, "GUARD_OK\nline1\nline2")
	if err != nil {
		t.Fatal(err)
	}
	if out != "line1\nline2" {
		t.Fatalf("ok output = %q", out)
	}
	if len(exec.Calls) != 2 || !strings.HasPrefix(exec.Calls[0], "UPLOAD:"+helperRemote) || !strings.HasPrefix(exec.Calls[1], "sh "+helperRemote+" 'web' 3 -- ") {
		t.Fatalf("call sequence (upload then invoke): %+v", exec.Calls)
	}

	if _, _, err := runWith(t, "GUARD_BUSY"); !errors.Is(err, ErrBusy) {
		t.Fatalf("busy: %v", err)
	}
	if _, _, err := runWith(t, "GUARD_FENCED 7 3"); !errors.Is(err, ErrFenced) || !strings.Contains(err.Error(), "7") {
		t.Fatalf("fenced: %v", err)
	}
	if _, _, err := runWith(t, "GUARD_UNFIT"); !errors.Is(err, ErrTargetUnfit) {
		t.Fatalf("unfit: %v", err)
	}
	if _, _, err := runWith(t, "GUARD_BADGEN"); err == nil || !strings.Contains(err.Error(), "unreadable") {
		t.Fatalf("badgen: %v", err)
	}
}

func TestRunEffectFailureCarriesOutput(t *testing.T) {
	_, _, err := runWith(t, "GUARD_EFFECT_FAILED 3\ndocker: no such image")
	if err == nil || !strings.Contains(err.Error(), "exit 3") || !strings.Contains(err.Error(), "no such image") {
		t.Fatalf("effect failure: %v", err)
	}
}

func TestRunProtocolViolationFailsClosed(t *testing.T) {
	if _, _, err := runWith(t, "some random output"); err == nil || !strings.Contains(err.Error(), "protocol violation") {
		t.Fatalf("protocol violation: %v", err)
	}
}

func TestHelperUploadedBeforeInvocation(t *testing.T) {
	exec, _, err := runWith(t, "GUARD_BUSY")
	if !errors.Is(err, ErrBusy) {
		t.Fatal(err)
	}
	body, ok := exec.Files[helperRemote]
	if !ok || !strings.Contains(string(body), "#!/bin/sh") {
		t.Fatalf("helper not uploaded: %+v", exec.Files)
	}
}

func TestRunQuotesAppName(t *testing.T) {
	exec := ssh.NewMockExecutor("target",
		ssh.MockCommand{Match: "sh " + helperRemote, Output: "GUARD_OK"},
	)
	if _, err := Run(context.Background(), exec, "we b'd", 0, "true"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(exec.Calls[1], `'we b'\''d'`) {
		t.Fatalf("app name not shell-quoted: %s", exec.Calls[1])
	}
}

// The embedded script carries the protocol words the wrapper maps — a
// drift here is a protocol break, not a test nit.
func TestEmbeddedScriptProtocol(t *testing.T) {
	for _, token := range []string{"GUARD_OK", "GUARD_BUSY", "GUARD_FENCED", "GUARD_UNFIT", "GUARD_BADGEN", "GUARD_EFFECT_FAILED", `flock "$LOCKFILE" -c`, "selftest-ok"} {
		if !strings.Contains(guardScript, token) {
			t.Fatalf("guard.sh missing %s", token)
		}
	}
}
