package docker

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/ssh"
)

func TestScanImagePassesWhenClean(t *testing.T) {
	mock := ssh.NewMockExecutor("h",
		ssh.MockCommand{Match: "docker run --rm", Output: "no vulns"},
	)
	c := NewClient(mock)
	if err := c.ScanImage(context.Background(), "myapp:abc", io.Discard); err != nil {
		t.Fatalf("clean scan must pass: %v", err)
	}
	if len(mock.Calls) != 2 {
		t.Fatalf("expected report + block passes, got %d calls", len(mock.Calls))
	}
	if !strings.Contains(mock.Calls[0], "--severity HIGH,CRITICAL") {
		t.Errorf("report pass severity wrong: %s", mock.Calls[0])
	}
	if !strings.Contains(mock.Calls[1], "--severity CRITICAL --exit-code 1") {
		t.Errorf("block pass wrong: %s", mock.Calls[1])
	}
	for _, call := range mock.Calls {
		if !strings.Contains(call, "'myapp:abc'") {
			t.Errorf("image not quoted into scan: %s", call)
		}
		if !strings.Contains(call, "--ignore-unfixed") {
			t.Errorf("scan must ignore unfixable CVEs: %s", call)
		}
	}
}

func TestScanImageBlocksOnCritical(t *testing.T) {
	mock := ssh.NewMockExecutor("h",
		ssh.MockCommand{Match: "docker run --rm -v /var/run/docker.sock:/var/run/docker.sock -v /deployments/.trivy-cache:/root/.cache aquasec/trivy:latest image --scanners vuln --ignore-unfixed --severity HIGH,CRITICAL", Output: "table"},
		ssh.MockCommand{Match: "docker run --rm", Output: "", Err: errExit1{}},
	)
	c := NewClient(mock)
	err := c.ScanImage(context.Background(), "myapp:abc", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "CRITICAL vulnerabilities") {
		t.Fatalf("critical findings must block the deploy, got: %v", err)
	}
}

type errExit1 struct{}

func (errExit1) Error() string { return "exit status 1" }
