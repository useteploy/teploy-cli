package docker

import (
	"context"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/ssh"
)

// A stopped container holding the name must be removed. This is the normal
// "roll back, fix forward, deploy the same version again" sequence: rollback
// deliberately keeps the superseded containers, so the redeploy used to fail on
// docker's raw "name is already in use".
func TestClearStoppedContainer_RemovesACorpse(t *testing.T) {
	exec := ssh.NewMockExecutor("h",
		ssh.MockCommand{Match: "docker inspect -f '{{.State.Running}}'", Output: "false\n"},
		ssh.MockCommand{Match: "docker rm", Output: ""},
	)
	c := NewClient(exec)

	if err := c.clearStoppedContainer(context.Background(), "app-web-abc123"); err != nil {
		t.Fatalf("clearStoppedContainer: %v", err)
	}
	if !calledWith(exec, "docker rm 'app-web-abc123'") {
		t.Errorf("the stopped container was not removed; calls: %v", exec.Calls)
	}
}

// A RUNNING container of the same name means this exact version is already live.
// Removing it would tear down a live workload to replace it with itself, so it
// is refused — the one case that must never be silently treated as a corpse.
func TestClearStoppedContainer_RefusesALiveWorkload(t *testing.T) {
	exec := ssh.NewMockExecutor("h",
		ssh.MockCommand{Match: "docker inspect -f '{{.State.Running}}'", Output: "true\n"},
	)
	c := NewClient(exec)

	err := c.clearStoppedContainer(context.Background(), "app-web-abc123")
	if err == nil {
		t.Fatal("a running container was treated as removable")
	}
	if !strings.Contains(err.Error(), "already running") {
		t.Errorf("error should say the version is live, got: %v", err)
	}
	for _, call := range exec.Calls {
		if strings.HasPrefix(call, "docker rm") {
			t.Fatalf("a running container was removed: %q", call)
		}
	}
}

// Nothing there is the common case and must be a silent no-op, not an error.
func TestClearStoppedContainer_NoContainerIsFine(t *testing.T) {
	exec := ssh.NewMockExecutor("h",
		ssh.MockCommand{Match: "docker inspect -f '{{.State.Running}}'", Err: errNotFound{}},
	)
	c := NewClient(exec)

	if err := c.clearStoppedContainer(context.Background(), "app-web-abc123"); err != nil {
		t.Errorf("a missing container should be a no-op, got: %v", err)
	}
	for _, call := range exec.Calls {
		if strings.HasPrefix(call, "docker rm") {
			t.Fatalf("removed something that does not exist: %q", call)
		}
	}
}

// Unexpected inspect output must not trigger a removal on a guess — let
// `docker run` report its own error instead.
func TestClearStoppedContainer_UnexpectedOutputRemovesNothing(t *testing.T) {
	exec := ssh.NewMockExecutor("h",
		ssh.MockCommand{Match: "docker inspect -f '{{.State.Running}}'", Output: "<no value>\n"},
	)
	c := NewClient(exec)

	if err := c.clearStoppedContainer(context.Background(), "app-web-abc123"); err != nil {
		t.Errorf("unexpected output should be tolerated, got: %v", err)
	}
	for _, call := range exec.Calls {
		if strings.HasPrefix(call, "docker rm") {
			t.Fatalf("removed on a guess: %q", call)
		}
	}
}

func calledWith(exec *ssh.MockExecutor, want string) bool {
	for _, c := range exec.Calls {
		if strings.Contains(c, want) {
			return true
		}
	}
	return false
}

type errNotFound struct{}

func (errNotFound) Error() string { return "Error: No such object" }

// The reactive path: `docker run` fails on a taken name, the stopped container
// is cleared, and the run is retried once. This is the "roll back, fix forward,
// deploy the same version again" sequence, which used to fail outright.
func TestRun_ClearsAStoppedNameCollisionAndRetries(t *testing.T) {
	exec := ssh.NewMockExecutor("h",
		// First run fails the way docker actually reports it.
		ssh.MockCommand{
			Match: "docker run", Once: true,
			Output: `docker: Error response from daemon: Conflict. The container name "/myapp-web-abc123" is already in use`,
			Err:    errConflict{},
		},
		ssh.MockCommand{Match: "docker inspect -f '{{.State.Running}}'", Output: "false\n"},
		ssh.MockCommand{Match: "docker rm", Output: ""},
		// Retry succeeds.
		ssh.MockCommand{Match: "docker run", Output: "newcontainerid"},
	)
	c := NewClient(exec)

	id, err := c.Run(context.Background(), RunConfig{
		App: "myapp", Process: "web", Version: "abc123", Image: "nginx:latest",
	})
	if err != nil {
		t.Fatalf("Run: %v\ncalls: %v", err, exec.Calls)
	}
	if id != "newcontainerid" {
		t.Errorf("id = %q, want the retried container's id", id)
	}
	if !calledWith(exec, "docker rm 'myapp-web-abc123'") {
		t.Errorf("the stopped container was not cleared; calls: %v", exec.Calls)
	}
	runs := 0
	for _, call := range exec.Calls {
		if strings.HasPrefix(call, "docker run") {
			runs++
		}
	}
	if runs != 2 {
		t.Errorf("expected exactly one retry (2 runs), got %d", runs)
	}
}

// A collision against a RUNNING container must NOT be cleared or retried —
// that would tear down a live workload to replace it with itself.
func TestRun_RefusesToClearALiveCollision(t *testing.T) {
	exec := ssh.NewMockExecutor("h",
		ssh.MockCommand{
			Match: "docker run", Once: true,
			Output: `Conflict. The container name "/myapp-web-abc123" is already in use`,
			Err:    errConflict{},
		},
		ssh.MockCommand{Match: "docker inspect -f '{{.State.Running}}'", Output: "true\n"},
	)
	c := NewClient(exec)

	_, err := c.Run(context.Background(), RunConfig{
		App: "myapp", Process: "web", Version: "abc123", Image: "nginx:latest",
	})
	if err == nil {
		t.Fatal("a live collision was silently resolved")
	}
	if !strings.Contains(err.Error(), "already running") {
		t.Errorf("error should say the version is live, got: %v", err)
	}
	for _, call := range exec.Calls {
		if strings.HasPrefix(call, "docker rm") {
			t.Fatalf("removed a running container: %q", call)
		}
	}
}

// Any other run failure must fall through untouched — the recovery is narrow on
// purpose, so an unrelated error never triggers a container removal.
func TestRun_OtherFailuresAreNotTreatedAsCollisions(t *testing.T) {
	exec := ssh.NewMockExecutor("h",
		ssh.MockCommand{Match: "docker run", Output: "no such image: nginx:latest", Err: errConflict{}},
	)
	c := NewClient(exec)

	if _, err := c.Run(context.Background(), RunConfig{
		App: "myapp", Process: "web", Version: "abc123", Image: "nginx:latest",
	}); err == nil {
		t.Fatal("expected the original failure to surface")
	}
	for _, call := range exec.Calls {
		if strings.HasPrefix(call, "docker rm") || strings.Contains(call, "State.Running") {
			t.Fatalf("an unrelated failure triggered collision recovery: %q", call)
		}
	}
}

func TestNameAlreadyInUse(t *testing.T) {
	// The realistic case: the message is in docker's output, and the error is a
	// bare exit status.
	if !nameAlreadyInUse(`Conflict. The container name "/x" is already in use`, errConflict{}) {
		t.Error("did not recognise docker's collision message in the output")
	}
	// Some executors fold the output into the error instead, so the error text is
	// checked too.
	if !nameAlreadyInUse("", errWrapped{}) {
		t.Error("did not recognise a collision carried in the error text")
	}
	// A bare exit status is NOT a collision — matching it would make every
	// non-zero `docker run` trigger a container removal.
	if nameAlreadyInUse("", errConflict{}) {
		t.Error("a bare exit status was misread as a name collision")
	}
	for _, other := range []string{"no such image", "permission denied", ""} {
		if nameAlreadyInUse(other, nil) {
			t.Errorf("%q was misread as a name collision", other)
		}
	}
}

type errConflict struct{}

func (errConflict) Error() string { return "exit status 125" }

type errWrapped struct{}

func (errWrapped) Error() string {
	return `exit status 125: Conflict. The container name "/x" is already in use`
}
