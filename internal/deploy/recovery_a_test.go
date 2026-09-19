package deploy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy/internal/ssh"
	"github.com/useteploy/teploy/internal/state"
)

// TestSameVersion_RunningReplacedRefused is the A08 core regression: after
// a failed same-version attempt renamed the serving predecessor to
// _replaced, a retry must REFUSE to force-remove the running workload
// instead of deleting it before any healthy replacement exists.
func TestSameVersion_RunningReplacedRefused(t *testing.T) {
	existingState := "current_port=49152\ncurrent_hash=abc123\nprevious_port=0\nprevious_hash=\n"
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "mkdir -p /deployments/myapp", Output: ""},
		ssh.MockCommand{Match: "mkdir /deployments/myapp/.lock", Output: ""},
		ssh.MockCommand{Match: "if [ ! -e '/deployments/myapp/state.json' ]", Output: "absent"},
		ssh.MockCommand{Match: "if [ ! -e '/deployments/myapp/state' ]", Output: "present\n" + existingState},
		ssh.MockCommand{Match: "ss -tln", Output: ssOutput},
		// The prior failed attempt left the serving predecessor RUNNING
		// under the _replaced name.
		ssh.MockCommand{Match: "docker inspect -f '{{.State.Status}}' 'myapp-web-abc123_replaced'", Output: "running"},
		ssh.MockCommand{Match: "docker rm -f", Output: ""},
		ssh.MockCommand{Match: "docker rename", Output: ""},
	)
	var buf bytes.Buffer
	d := NewDeployer(mock, &buf)
	err := d.Deploy(context.Background(), Config{
		App:     "myapp",
		Domain:  "myapp.com",
		Image:   "myapp:latest",
		Version: "abc123",
		Health:  HealthConfig{Timeout: 5 * time.Second, Interval: 10 * time.Millisecond},
	})
	if err == nil || !strings.Contains(err.Error(), "refusing to delete") {
		t.Fatalf("expected the running _replaced workload to be refused, got %v", err)
	}
	for _, c := range mock.Calls {
		if strings.HasPrefix(c, "docker rm -f") && strings.Contains(c, "_replaced") {
			t.Errorf("the running serving predecessor must not be removed, saw: %s", c)
		}
		if strings.HasPrefix(c, "docker run") {
			t.Errorf("no candidate may start while the predecessor's fate is undecided, saw: %s", c)
		}
	}
}

// TestSameVersion_RenameFailureAborts: a rename failure with the source
// container still present must abort the deploy (A08) instead of being
// swallowed and leaving the snapshot and the candidate names disagreed.
func TestSameVersion_RenameFailureAborts(t *testing.T) {
	existingState := "current_port=49152\ncurrent_hash=abc123\nprevious_port=0\nprevious_hash=\n"
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "mkdir -p /deployments/myapp", Output: ""},
		ssh.MockCommand{Match: "mkdir /deployments/myapp/.lock", Output: ""},
		ssh.MockCommand{Match: "if [ ! -e '/deployments/myapp/state.json' ]", Output: "absent"},
		ssh.MockCommand{Match: "if [ ! -e '/deployments/myapp/state' ]", Output: "present\n" + existingState},
		ssh.MockCommand{Match: "ss -tln", Output: ssOutput},
		ssh.MockCommand{Match: "docker inspect -f '{{.State.Status}}' 'myapp-web-abc123_replaced'", Output: ""},
		ssh.MockCommand{Match: "docker rm -f", Output: ""},
		// The rename fails (e.g. transport) and the source provably still
		// exists — the deploy must abort rather than continue on a guess.
		ssh.MockCommand{Match: "docker rename", Err: errBoom},
		ssh.MockCommand{Match: "docker inspect -f '{{.State.Status}}' 'myapp-web-abc123'", Output: "running"},
		ssh.MockCommand{Match: "docker run", Output: "x"},
	)
	var buf bytes.Buffer
	d := NewDeployer(mock, &buf)
	err := d.Deploy(context.Background(), Config{
		App:     "myapp",
		Domain:  "myapp.com",
		Image:   "myapp:latest",
		Version: "abc123",
		Health:  HealthConfig{Timeout: 5 * time.Second, Interval: 10 * time.Millisecond},
	})
	if err == nil || !strings.Contains(err.Error(), "renaming the current container") {
		t.Fatalf("expected an abort on unclassified rename failure, got %v", err)
	}
	for _, c := range mock.Calls {
		if strings.HasPrefix(c, "docker run") {
			t.Errorf("no candidate may start after an unclassified rename failure, saw: %s", c)
		}
	}
}

// TestAbortStateCommit_CancelledContextStillRunsCompensation is the A11
// core regression: a state-commit failure caused by a CANCELLED deploy
// context must still run every compensating stop on a live, detached
// context. Host ingress keeps the flow free of Caddy so the assertion is
// exactly "compensation ran".
func TestAbortStateCommit_CancelledContextStillRunsCompensation(t *testing.T) {
	app := "fency"
	mock := ssh.NewMockExecutor("1.2.3.4", fenceHappyPathMocks(app)...)
	// The cancelled deploy context: every exec call on it would fail.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var buf bytes.Buffer
	d := NewDeployer(mock, &buf)
	cfg := Config{
		App:           app,
		Image:         "fency:latest",
		Version:       "abc123",
		Ingress:       "host",
		ContainerPort: 8080,
	}
	started := []string{"fency-web-abc123"}
	err := d.abortStateCommit(ctx, cfg, nil, started, nil, time.Now(), errBoom)
	if err == nil {
		t.Fatal("expected the commit error to be returned")
	}
	sawStop := false
	for _, c := range mock.Calls {
		if strings.HasPrefix(c, "docker stop") {
			sawStop = true
		}
	}
	if !sawStop {
		t.Error("compensation must run on a detached context even when the deploy context is cancelled")
	}
}

// TestAbortStateCommit_CaddyPublishAppRestoresRoute is the A10 core
// regression: a caddy-ingress app with publish entries takes the fixed-port
// recreate branch — its commit failure must restore the Caddy route too,
// not just the workload (the old code returned from the branch without the
// route restore, leaving Caddy pointed at the removed candidate names).
func TestAbortStateCommit_CaddyPublishAppRestoresRoute(t *testing.T) {
	app := "fency"
	current := &state.AppState{
		SchemaVersion: 2, DeploymentType: "container", IngressMode: "caddy",
		CurrentHash: "oldhash", CurrentPorts: []int{49152}, CurrentPort: 49152,
		Domain: "fency.com",
	}
	// restorePreviousRoute inspects the old container's internal port; this
	// stub must be registered BEFORE fenceHappyPathMocks' generic
	// "docker inspect" (first match wins).
	mocks := append([]ssh.MockCommand{
		ssh.MockCommand{Match: "docker inspect -f '{{range $p, $_ := .NetworkSettings.Ports}}", Output: "8080/tcp "},
	}, fenceHappyPathMocks(app)...)
	mock := ssh.NewMockExecutor("1.2.3.4", mocks...)

	var buf bytes.Buffer
	d := NewDeployer(mock, &buf)
	cfg := Config{
		App:     app,
		Domain:  "fency.com",
		Image:   "fency:latest",
		Version: "abc123",
		Publish: []string{"0.0.0.0:3001:3001"},
		Health:  HealthConfig{Timeout: 5 * time.Second, Interval: 10 * time.Millisecond},
	}
	started := []string{"fency-web-abc123"}
	err := d.abortStateCommit(context.Background(), cfg, current, started, nil, time.Now(), errBoom)
	if err == nil {
		t.Fatal("expected the commit error to surface")
	}
	if !strings.Contains(err.Error(), "uncommitted workload was stopped and removed") {
		t.Errorf("the failure must report the exact end state: %v", err)
	}
	sawRouteRestore := false
	for _, c := range mock.Calls {
		// applyManagedBlock renders the previous block into the Caddyfile
		// and reloads the server — either shape proves the route restore.
		if strings.Contains(c, "docker exec caddy caddy reload") || strings.Contains(c, "Caddyfile") {
			sawRouteRestore = true
		}
	}
	if !sawRouteRestore {
		t.Error("a caddy-ingress app's fixed-port compensation must restore the Caddy route")
	}
}

// TestDeploy_HostIngressRunFailure_ItemizesFailedRecovery is the A13
// regression: "at least one restart worked" must never read as
// "recovered" — every failed recovery appears in the outcome.
func TestDeploy_HostIngressRunFailure_ItemizesFailedRecovery(t *testing.T) {
	app := "fency"
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "mkdir -p /deployments/fency", Output: ""},
		ssh.MockCommand{Match: "mkdir /deployments/fency/.lock", Output: ""},
		ssh.MockCommand{Match: "if [ ! -e '/deployments/fency/state.json' ]", Output: "absent"},
		ssh.MockCommand{Match: "if [ ! -e '/deployments/fency/state' ]", Output: "absent"},
		ssh.MockCommand{Match: "ss -tln", Output: ssOutput},
		// The displaced web container stops fine...
		ssh.MockCommand{Match: "docker ps --filter label=teploy.app=fency", Output: "fency-web-oldhash"},
		ssh.MockCommand{Match: "docker stop -t", Output: ""},
		// ...the candidate run fails...
		ssh.MockCommand{Match: "docker run", Err: errBoom},
		// ...and EVERY recovery restart fails too (Restart = InspectRecreate
		// + Recreate; the inspect fails, so the restart fails).
		ssh.MockCommand{Match: "docker inspect", Err: errBoom},
		ssh.MockCommand{Match: "printf %s", Output: ""},
	)
	var buf bytes.Buffer
	d := NewDeployer(mock, &buf)
	err := d.Deploy(context.Background(), Config{
		App:           app,
		Image:         "fency:latest",
		Version:       "abc123",
		Ingress:       "host",
		ContainerPort: 8080,
	})
	if err == nil {
		t.Fatal("expected the deploy to fail")
	}
	if !strings.Contains(err.Error(), "no container is serving") {
		t.Fatalf("a total recovery failure must say no container is serving, got: %v", err)
	}
	if !strings.Contains(buf.String(), "cleanup incomplete") {
		t.Error("failed compensations must be itemized in the output")
	}
}

// TestLogDeploy_RecordsImage is the A26 regression (and the open half of
// TCL-19): the durable deploy log records the image identity, not just the
// version.
func TestLogDeploy_RecordsImage(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "printf %s", Output: ""},
	)
	d := &Deployer{exec: mock, out: &bytes.Buffer{}}
	d.logDeploy(context.Background(), Config{App: "myapp", Image: "myapp:latest", Version: "abc123"}, true, time.Now())
	var line string
	for _, c := range mock.Calls {
		if strings.HasPrefix(c, "printf %s '") {
			line = c
		}
	}
	if line == "" {
		t.Fatal("no log append issued")
	}
	enc := strings.TrimSuffix(strings.TrimPrefix(line, "printf %s '"), "' | base64 -d >> /deployments/teploy.log")
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		t.Fatalf("decoding log entry: %v", err)
	}
	var entry state.LogEntry
	if err := json.Unmarshal(raw, &entry); err != nil {
		t.Fatalf("parsing log entry: %v", err)
	}
	if entry.Image != "myapp:latest" {
		t.Errorf("log entry image: got %q want myapp:latest", entry.Image)
	}
}
