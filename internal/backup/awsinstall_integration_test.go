//go:build integration

package backup

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy/internal/ssh"
)

// TestAWSCLIInstallScriptLive runs setup's installer on the fixture for real: AWS's
// bundle, its real signature, a real install, with the pinned key and without it.
// It replaces the VM's aws, so it needs TEPLOY_IT_INSTALL_AWS=1.
func TestAWSCLIInstallScriptLive(t *testing.T) {
	if os.Getenv("TEPLOY_IT_INSTALL_AWS") == "" {
		t.Skip("set TEPLOY_IT_INSTALL_AWS=1: this reinstalls the fixture's aws CLI")
	}
	exec := drFixtureEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	exec.Run(ctx, "sudo -n rm -rf /usr/local/aws-cli /usr/local/bin/aws /usr/local/bin/aws_completer; true")
	// A key that is not the pin: the real signature must not be accepted.
	tampered := strings.ReplaceAll(AWSCLIInstallScript, awsCLIKeyFingerprint, "0000000000000000000000000000000000000000")
	res := ssh.RunDetailed(ctx, exec, "sudo -n sh -c "+ssh.ShellQuote(tampered))
	if !res.Failed() || !strings.Contains(string(res.Stderr), "did not verify") {
		t.Fatalf("a wrong pin must refuse: exit %d\n%s%s", res.ExitCode, res.Stdout, res.Stderr)
	}
	if out, _ := exec.Run(ctx, "command -v aws; true"); strings.TrimSpace(out) != "" {
		t.Fatalf("installed after a failed verification: %s", out)
	}
	res = ssh.RunDetailed(ctx, exec, "sudo -n sh -c "+ssh.ShellQuote(AWSCLIInstallScript))
	if res.Failed() {
		t.Fatalf("install: exit %d\n%s%s", res.ExitCode, res.Stdout, res.Stderr)
	}
	// As a user other than root: cron's case, and where u+x modes used to fail.
	out, err := exec.Run(ctx, "aws --version")
	if err != nil {
		t.Fatalf("aws does not run: %v %s", err, out)
	}
	t.Log(strings.TrimSpace(out))
}
