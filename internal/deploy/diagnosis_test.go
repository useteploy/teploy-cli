package deploy

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/ssh"
)

func TestPrintDiagnosisCrashedContainer(t *testing.T) {
	exec := ssh.NewMockExecutor("test",
		ssh.MockCommand{Match: "docker inspect", Output: "exited|1|false\n"},
	)
	var out bytes.Buffer
	d := NewDeployer(exec, &out)

	logs := `Error: environment variable "API_KEY" is not set`
	d.printDiagnosis(context.Background(), "myapp-web-abc", 3000, errors.New("health check failed"), logs)

	got := out.String()
	if !strings.Contains(got, "Likely cause:") || !strings.Contains(got, "API_KEY") {
		t.Fatalf("expected env-var diagnosis, got:\n%s", got)
	}
}

func TestPrintDiagnosisPortMismatch(t *testing.T) {
	exec := ssh.NewMockExecutor("test",
		ssh.MockCommand{Match: "docker inspect", Output: "running|0|false\n"},
		ssh.MockCommand{Match: "docker exec", Output: "LISTEN 0 4096 0.0.0.0:8080 0.0.0.0:*\n"},
	)
	var out bytes.Buffer
	d := NewDeployer(exec, &out)

	d.printDiagnosis(context.Background(), "myapp-web-abc", 3000, errors.New("health check failed"), "")

	got := out.String()
	if !strings.Contains(got, "8080") || !strings.Contains(got, "port: 3000") {
		t.Fatalf("expected port-mismatch diagnosis, got:\n%s", got)
	}
}

func TestPrintDiagnosisSilentWhenNothingKnown(t *testing.T) {
	// Inspect fails (container gone) and there are no logs — the diagnosis
	// must stay quiet rather than guess.
	exec := ssh.NewMockExecutor("test",
		ssh.MockCommand{Match: "docker inspect", Err: errors.New("no such container")},
	)
	var out bytes.Buffer
	d := NewDeployer(exec, &out)

	d.printDiagnosis(context.Background(), "myapp-web-abc", 3000, errors.New("boom"), "")

	if out.Len() != 0 {
		t.Fatalf("expected no output, got:\n%s", out.String())
	}
}
