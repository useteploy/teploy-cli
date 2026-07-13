package harden

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/ssh"
)

func TestEnableAuditInstallsRulesAndSudoers(t *testing.T) {
	mock := ssh.NewMockExecutor("h",
		ssh.MockCommand{Match: "which auditctl", Output: "/usr/sbin/auditctl"},
		ssh.MockCommand{Match: "visudo -c", Output: "parsed OK"},
		ssh.MockCommand{Match: "mv /tmp/teploy-audit.rules", Output: ""},
		ssh.MockCommand{Match: "augenrules --load", Output: ""},
		ssh.MockCommand{Match: "systemctl enable --now auditd", Output: ""},
		ssh.MockCommand{Match: "mv /tmp/teploy-audit-sudoers", Output: ""},
	)
	if err := EnableAudit(context.Background(), mock, io.Discard, ""); err != nil {
		t.Fatal(err)
	}
	rules := string(mock.Files["/tmp/teploy-audit.rules"])
	if !strings.Contains(rules, "-k teploy-exec") || !strings.Contains(rules, "-w /deployments") {
		t.Errorf("audit rules missing keys: %s", rules)
	}
	sudoers := string(mock.Files["/tmp/teploy-audit-sudoers"])
	if !strings.Contains(sudoers, "log_input, log_output") {
		t.Errorf("sudoers must enable IO logging: %s", sudoers)
	}
	// visudo validation must run BEFORE the sudoers install (broken sudoers = locked-out box).
	visudoIdx, installIdx := -1, -1
	for i, call := range mock.Calls {
		if strings.HasPrefix(call, "visudo -c") {
			visudoIdx = i
		}
		if strings.Contains(call, "mv /tmp/teploy-audit-sudoers") {
			installIdx = i
		}
	}
	if visudoIdx == -1 || installIdx == -1 || visudoIdx > installIdx {
		t.Errorf("visudo -c must validate before install: visudo=%d install=%d", visudoIdx, installIdx)
	}
}

func TestEnableAuditRejectsBadSudoers(t *testing.T) {
	mock := ssh.NewMockExecutor("h",
		ssh.MockCommand{Match: "which auditctl", Output: "/usr/sbin/auditctl"},
		ssh.MockCommand{Match: "mv /tmp/teploy-audit.rules", Output: ""},
		ssh.MockCommand{Match: "augenrules --load", Output: ""},
		ssh.MockCommand{Match: "systemctl enable --now auditd", Output: ""},
		ssh.MockCommand{Match: "visudo -c", Output: "", Err: errBadSudoers{}},
	)
	err := EnableAudit(context.Background(), mock, io.Discard, "")
	if err == nil || !strings.Contains(err.Error(), "validation") {
		t.Fatalf("invalid sudoers must abort before install, got: %v", err)
	}
	for _, call := range mock.Calls {
		if strings.Contains(call, "mv /tmp/teploy-audit-sudoers") {
			t.Error("bad sudoers content must never be installed")
		}
	}
}

type errBadSudoers struct{}

func (errBadSudoers) Error() string { return "syntax error" }
