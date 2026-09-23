package deploy

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy/internal/ssh"
	"github.com/useteploy/teploy/internal/state"
)

// TestDeployFenced_EffectsRunGuarded proves the C01-2 composition on the
// happy path: the candidate/worker docker runs and the Caddyfile commit
// execute as guard+effect in ONE remote command (never a separate
// check-then-act pair), so no transport window exists for a takeover to
// slip a broken holder's effect into.
func TestDeployFenced_EffectsRunGuarded(t *testing.T) {
	app := "fency"
	workerState := `{"Status":"running","Running":true}`
	mocks := append([]ssh.MockCommand{
		// Worker viability verification (A23) — must answer before the
		// generic "docker inspect" happy-path entry.
		ssh.MockCommand{Match: "docker inspect -f '{{json .State}}'", Output: workerState},
	}, fenceHappyPathMocks(app)...)
	mock := ssh.NewMockExecutor("1.2.3.4", mocks...)
	lk, err := state.AcquireLockFenced(context.Background(), mock, app)
	if err != nil {
		t.Fatalf("AcquireLockFenced: %v", err)
	}
	guard := lk.GuardPrefix()
	if guard == "" {
		t.Fatal("a held fence must produce a guard prefix")
	}
	var out strings.Builder
	d := NewDeployer(mock, &out)
	cfg := Config{
		App:       app,
		Domain:    "fency.com",
		Image:     "fency:latest",
		Version:   "abc123",
		Processes: map[string]string{"web": "", "worker": "node worker.js"},
		Health:    HealthConfig{Timeout: 5 * time.Second, Interval: 10 * time.Millisecond},
	}
	if err := d.DeployFenced(context.Background(), cfg, lk); err != nil {
		t.Fatalf("DeployFenced: %v", err)
	}

	composedRun := 0
	for _, c := range mock.Calls {
		if strings.HasPrefix(c, guard) && strings.Contains(c, "docker run") {
			composedRun++
		}
	}
	// web candidate + worker: two guarded container creations.
	if composedRun < 2 {
		t.Errorf("expected the web and worker starts to run composed under the fence guard, found %d", composedRun)
	}
	var composedCommit bool
	for _, c := range mock.Calls {
		if strings.HasPrefix(c, guard) && strings.Contains(c, "mv -fT -- ") && strings.Contains(c, "/deployments/caddy/Caddyfile") {
			composedCommit = true
		}
	}
	if !composedCommit {
		t.Error("expected the Caddyfile commit rename to run composed under the fence guard")
	}
}

// lockFlipper wraps the mock executor, rewriting the lock info to name
// another operation the moment a trigger command runs — modeling a
// takeover at an exact point mid-deploy.
type lockFlipper struct {
	*ssh.MockExecutor
	app      string
	trigger  string
	flipped  bool
	execOnly []string
}

func (f *lockFlipper) Run(ctx context.Context, cmd string) (string, error) {
	if !f.flipped && strings.Contains(cmd, f.trigger) {
		f.flipped = true
		f.MockExecutor.Files["/deployments/"+f.app+"/.lock/info"] = []byte(`{"type":"auto","owner":"someoneelse"}`)
	}
	out, err := f.MockExecutor.Run(ctx, cmd)
	// Track EXECUTED commands: the mock appends the effect-only form for
	// guard commands whose guard held, so filter those back out by
	// tracking what the underlying executor actually recorded.
	return out, err
}

// TestDeployFenced_RouteSwitchRefusedOnMidFlightTakeover: the lock breaks
// during the deploy (at the health gate); the route switch's Caddyfile
// COMMIT must be refused by the composed guard — the Caddyfile on disk
// never changes, no reload runs, and the deploy reports the fence loss.
func TestDeployFenced_RouteSwitchRefusedOnMidFlightTakeover(t *testing.T) {
	app := "fency"
	base := ssh.NewMockExecutor("1.2.3.4", fenceHappyPathMocks(app)...)
	lk, err := state.AcquireLockFenced(context.Background(), base, app)
	if err != nil {
		t.Fatalf("AcquireLockFenced: %v", err)
	}
	flip := &lockFlipper{MockExecutor: base, app: app, trigger: "curl -s -o /dev/null"}
	var out strings.Builder
	d := NewDeployer(flip, &out)
	err = d.DeployFenced(context.Background(), Config{
		App:     app,
		Domain:  "fency.com",
		Image:   "fency:latest",
		Version: "abc123",
		Health:  HealthConfig{Timeout: 5 * time.Second, Interval: 10 * time.Millisecond},
	}, lk)
	if !errors.Is(err, state.ErrFenceLost) {
		t.Fatalf("expected ErrFenceLost from the refused route switch, got: %v", err)
	}
	if !flip.flipped {
		t.Fatal("test bug: the takeover never fired")
	}
	// The Caddyfile must be untouched: the happy-path fixture has no
	// caddyfile in Files, and a landed commit would have created one via
	// the staged rename.
	if _, ok := base.Files["/deployments/caddy/Caddyfile"]; ok {
		t.Error("a fence-refused route switch must not commit the Caddyfile")
	}
	for _, c := range base.Calls {
		if strings.HasPrefix(c, "docker exec caddy caddy reload") {
			t.Errorf("no reload may run after a refused commit, saw: %s", c)
		}
	}
}

// TestDeployFenced_WorkerStartRefusedOnTakeover: the lock breaks after the
// web candidates are healthy; the worker start (the finding's second
// site) must be refused by the composed guard — no bare docker run for
// the worker executes.
func TestDeployFenced_WorkerStartRefusedOnTakeover(t *testing.T) {
	app := "fency"
	base := ssh.NewMockExecutor("1.2.3.4", fenceHappyPathMocks(app)...)
	lk, err := state.AcquireLockFenced(context.Background(), base, app)
	if err != nil {
		t.Fatalf("AcquireLockFenced: %v", err)
	}
	flip := &lockFlipper{MockExecutor: base, app: app, trigger: "curl -s -o /dev/null"}
	var out strings.Builder
	d := NewDeployer(flip, &out)
	err = d.DeployFenced(context.Background(), Config{
		App:       app,
		Domain:    "fency.com",
		Image:     "fency:latest",
		Version:   "abc123",
		Processes: map[string]string{"web": "", "worker": "node worker.js"},
		Health:    HealthConfig{Timeout: 5 * time.Second, Interval: 10 * time.Millisecond},
	}, lk)
	if !errors.Is(err, state.ErrFenceLost) {
		t.Fatalf("expected ErrFenceLost from the refused worker start, got: %v", err)
	}
	for _, c := range base.Calls {
		if strings.HasPrefix(c, "docker run") && strings.Contains(c, "fency-worker-abc123") {
			t.Errorf("the worker start must be refused in-shell, saw execution: %s", c)
		}
	}
}

// TestDeployFenced_CommitGuardOrder documents and pins the lock
// ACQUISITION ORDER on the traffic-switch commit (C01-3): the app-level
// fence guard precedes the short-lived shared-proxy (caddy) lock guard in
// the composed command — app lock held for the whole lifecycle, caddy
// commit lock held only for the brief edit+reload, never the inverse,
// never across hosts.
func TestDeployFenced_CommitGuardOrder(t *testing.T) {
	app := "fency"
	mock := ssh.NewMockExecutor("1.2.3.4", fenceHappyPathMocks(app)...)
	lk, err := state.AcquireLockFenced(context.Background(), mock, app)
	if err != nil {
		t.Fatalf("AcquireLockFenced: %v", err)
	}
	var out strings.Builder
	d := NewDeployer(mock, &out)
	if err := d.DeployFenced(context.Background(), Config{
		App:     app,
		Domain:  "fency.com",
		Image:   "fency:latest",
		Version: "abc123",
		Health:  HealthConfig{Timeout: 5 * time.Second, Interval: 10 * time.Millisecond},
	}, lk); err != nil {
		t.Fatalf("DeployFenced: %v", err)
	}
	guard := lk.GuardPrefix()
	for _, c := range mock.Calls {
		if !strings.Contains(c, "mv -fT -- ") || !strings.Contains(c, "/deployments/caddy/Caddyfile") {
			continue
		}
		appIdx := strings.Index(c, guard)
		caddyIdx := strings.Index(c, "/deployments/caddy/.lock/info")
		if appIdx < 0 || caddyIdx < 0 {
			t.Fatalf("commit command missing a guard:\n%s", c)
		}
		if appIdx > caddyIdx {
			t.Fatalf("caddy lock guard must FOLLOW the app fence guard (documented acquisition order):\n%s", c)
		}
		return
	}
	t.Fatal("no composed Caddyfile commit found")
}
