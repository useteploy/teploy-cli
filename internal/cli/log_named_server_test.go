package cli

import (
	"io"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/config"
)

// TestLog_HostFlagResolvesNamedServer pins the R02 docs-lane defect:
// `teploy log --app demo --host box1` dialed the literal hostname "box1"
// instead of the servers.yml entry registered under that name.
func TestLog_HostFlagResolvesNamedServer(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TEPLOY_HOST", "")
	t.Setenv("TEPLOY_USER", "")
	t.Setenv("TEPLOY_SSH_KEY", "")

	serversPath, err := config.DefaultServersPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := config.AddServer(serversPath, "box1", "127.0.0.1:1", "deploy", "", ""); err != nil {
		t.Fatal(err)
	}

	var runErr error
	out := captureStdout(t, func() {
		runErr = runLog(&Flags{Host: "box1"}, "demo", 20, io.Discard)
	})
	if runErr == nil {
		t.Fatal("expected a connect failure against the unreachable fixture address")
	}
	if !strings.Contains(out, "Connecting to deploy@127.0.0.1:1") {
		t.Fatalf("log did not resolve the named server; stdout=%q", out)
	}
}
