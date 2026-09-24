package docker

import (
	"context"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/ssh"
)

// TestRun_StampsGenerationLabel pins the C01-9 identity surface: a RunConfig
// with a generation labels the container teploy.generation=<G>; zero keeps
// the legacy unlabeled shape.
func TestRun_StampsGenerationLabel(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "docker run", Output: "abc123"},
	)
	dk := NewClient(mock)
	if _, err := dk.Run(context.Background(), RunConfig{
		App: "myapp", Process: "web", Version: "v1", Image: "img:1",
		Generation: 8,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var sawLabel bool
	for _, c := range mock.Calls {
		if strings.HasPrefix(c, "docker run") && strings.Contains(c, "--label 'teploy.generation=8'") {
			sawLabel = true
		}
	}
	if !sawLabel {
		t.Errorf("the run must stamp the generation label:\n%s", mock.Calls[0])
	}

	mock2 := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "docker run", Output: "abc123"},
	)
	if _, err := NewClient(mock2).Run(context.Background(), RunConfig{
		App: "myapp", Process: "web", Version: "v1", Image: "img:1",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.Contains(mock2.Calls[0], "teploy.generation") {
		t.Errorf("zero generation must keep the legacy unlabeled shape:\n%s", mock2.Calls[0])
	}
}

// TestStopGenerationFenced_ComposedCommand pins the C01-8/9 shape: the
// holdership guard, the generation label check, and the stop are ONE remote
// command — no transport window between check and effect.
func TestStopGenerationFenced_ComposedCommand(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "docker stop", Output: ""},
	)
	mock.Files["/deployments/myapp/.lock/info"] = []byte(`{"type":"auto","owner":"own"}`)
	guard := "grep -q 'own' '/deployments/myapp/.lock/info' || { printf 'TEPLOY_FENCE_LOST\\n' >&2; exit 75; }; "
	dk := NewClient(mock)
	if err := dk.StopGenerationFenced(context.Background(), "myapp-web-v1", 10, 7, guard); err != nil {
		t.Fatalf("StopGenerationFenced: %v", err)
	}
	cmd := mock.Calls[0]
	for _, want := range []string{
		"grep -q 'own'",
		`docker inspect -f '{{index .Config.Labels "teploy.generation"}}'`,
		`[ "$g" -le 7 ]`,
		"docker stop -t 10 'myapp-web-v1'",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("composed stop missing %q:\n%s", want, cmd)
		}
	}
	if !strings.HasPrefix(cmd, "grep -q ") {
		t.Errorf("the holdership guard must prefix the whole command:\n%s", cmd)
	}
}

// TestStopGenerationFenced_RefusesNewerGeneration is the delayed-effect
// acceptance at the docker boundary: the name now belongs to a container
// created by a NEWER generation (takeover + same-hash redeploy) — the stop
// is refused by the label check and never executes, no matter when the
// command lands. Legacy unlabeled containers keep stopping (compat).
func TestStopGenerationFenced_RefusesNewerGeneration(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "docker stop", Output: ""},
	)
	mock.GenerationLabels = map[string]string{"myapp-web-v1": "9"}
	dk := NewClient(mock)
	err := dk.StopGenerationFenced(context.Background(), "myapp-web-v1", 10, 7, "")
	if err == nil || !strings.Contains(err.Error(), "TEPLOY_GENERATION_FENCED 9 7") {
		t.Fatalf("expected a generation refusal naming both generations, got %v", err)
	}
	for _, c := range mock.Calls {
		if strings.HasPrefix(c, "docker stop") {
			t.Fatalf("a refused stop must never execute, saw: %s", c)
		}
	}

	// Legacy container (no label) stops as before.
	mock2 := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "docker stop", Output: ""},
	)
	if err := NewClient(mock2).StopGenerationFenced(context.Background(), "myapp-web-v1", 10, 7, ""); err != nil {
		t.Fatalf("a legacy unlabeled container must keep stopping: %v", err)
	}
}

// TestRestartFenced_GuardsTheRemove pins the recreate composition: the
// force-remove — the destructive half of a rollback's target restart —
// carries the generation label check in the same shell, so a stale
// rollback cannot destroy a successor's same-named container.
func TestRestartFenced_GuardsTheRemove(t *testing.T) {
	inspect := `[{"Config":{"Image":"myapp:latest","Labels":{"teploy.app":"myapp","teploy.generation":"3"}},"HostConfig":{"NetworkMode":"teploy","RestartPolicy":{"Name":"no"}},"NetworkSettings":{"Networks":{"teploy":{"Aliases":["myapp"]}}}}]`
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "docker inspect 'myapp-web-v1'", Output: inspect},
		ssh.MockCommand{Match: "docker rm -f 'myapp-web-v1'", Output: ""},
		ssh.MockCommand{Match: "docker run", Output: "newid"},
	)
	dk := NewClient(mock)
	if err := dk.RestartFenced(context.Background(), "myapp-web-v1", nil, 7, ""); err != nil {
		t.Fatalf("RestartFenced: %v", err)
	}
	var guardedRemove bool
	for _, c := range mock.Calls {
		if strings.HasPrefix(c, "g=$(docker inspect") && strings.Contains(c, "docker rm -f 'myapp-web-v1'") {
			guardedRemove = true
		}
	}
	if !guardedRemove {
		t.Error("the recreate's force-remove must run composed with the generation check")
	}
	// The recreated container PRESERVES its labels (generation 3 rides along).
	for _, c := range mock.Calls {
		if strings.HasPrefix(c, "docker run") && !strings.Contains(c, "--label 'teploy.generation=3'") {
			t.Errorf("recreate must preserve the generation label:\n%s", c)
		}
	}
}

// TestRestartFenced_RefusesNewerGeneration: the name was re-created by a
// newer generation; the remove is refused and never executes.
func TestRestartFenced_RefusesNewerGeneration(t *testing.T) {
	inspect := `[{"Config":{"Image":"myapp:latest","Labels":{"teploy.app":"myapp"}},"HostConfig":{"NetworkMode":"teploy","RestartPolicy":{"Name":"no"}},"NetworkSettings":{"Networks":{"teploy":{"Aliases":["myapp"]}}}}]`
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "docker inspect 'myapp-web-v1'", Output: inspect},
		ssh.MockCommand{Match: "docker rm -f 'myapp-web-v1'", Output: ""},
		ssh.MockCommand{Match: "docker run", Output: "newid"},
	)
	mock.GenerationLabels = map[string]string{"myapp-web-v1": "9"}
	dk := NewClient(mock)
	err := dk.RestartFenced(context.Background(), "myapp-web-v1", nil, 7, "")
	if err == nil || !strings.Contains(err.Error(), "TEPLOY_GENERATION_FENCED 9 7") {
		t.Fatalf("expected a generation refusal naming both generations, got %v", err)
	}
	for _, c := range mock.Calls {
		if strings.HasPrefix(c, "docker rm -f") || strings.HasPrefix(c, "docker run") {
			t.Fatalf("a refused recreate must never remove or run, saw: %s", c)
		}
	}
}
