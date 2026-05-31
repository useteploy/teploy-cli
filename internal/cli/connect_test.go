package cli

import (
	"context"
	"strings"
	"testing"
)

// resolveApp's --app path requires --host. The guard must fire BEFORE any SSH
// connection attempt, so this is pure-logic testable without a live server.
// (The happy --app path does a real SSH connect + state.Read and is covered by
// end-to-end smoke tests against a live VM, not unit tests.)
func TestResolveApp_AppWithoutHostErrors(t *testing.T) {
	_, _, err := resolveApp(context.Background(), &Flags{}, "myapp")
	if err == nil {
		t.Fatal("expected error when --app is set without --host")
	}
	if !strings.Contains(err.Error(), "--host is required") {
		t.Errorf("error = %q, want it to mention --host is required", err.Error())
	}
}

// With no --app and no teploy.yml in the cwd, resolveApp should surface the
// config-load error (not panic, not silently succeed). t.Chdir to a temp dir
// guarantees there's no teploy.yml to accidentally pick up.
func TestResolveApp_NoAppNoConfigErrors(t *testing.T) {
	t.Chdir(t.TempDir())
	_, _, err := resolveApp(context.Background(), &Flags{}, "")
	if err == nil {
		t.Fatal("expected error when no --app and no teploy.yml present")
	}
}
