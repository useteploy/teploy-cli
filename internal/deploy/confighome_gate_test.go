package deploy

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy/internal/confighome"
	"github.com/useteploy/teploy/internal/ssh"
	"github.com/useteploy/teploy/internal/state"
)

const gateHead = "8f2c1d0e9b7a6f5e4d3c2b1a0f9e8d7c6b5a4f3e"

func gateMock(t *testing.T, app string, serverHome bool) (*ssh.MockExecutor, *state.Lock) {
	t.Helper()
	// State reads answer from the recorded files (the happy-path stub
	// pins "absent", which would hide the tombstone the gate writes).
	var mocks []ssh.MockCommand
	for _, m := range fenceHappyPathMocks(app) {
		if !strings.Contains(m.Match, "state.json' ]") {
			mocks = append(mocks, m)
		}
	}
	mocks = append(mocks, ssh.MockCommand{Match: "mkdir -p /deployments/" + app})
	mock := ssh.NewMockExecutor("1.2.3.4", mocks...)
	if serverHome {
		repo := "/deployments/.teploy/apps/" + app + "/config.git"
		mock.Files[repo+"/HEAD"] = []byte("ref: refs/heads/teploy\n")
		mock.Files[repo+"/refs/heads/teploy"] = []byte(gateHead + "\n")
	}
	lk, err := state.AcquireLockFenced(context.Background(), mock, app)
	if err != nil {
		t.Fatalf("AcquireLockFenced: %v", err)
	}
	return mock, lk
}

func gateConfig(app, commit string) Config {
	return Config{App: app, Domain: app + ".com", Image: app + ":latest", Version: "newhash", ConfigCommit: commit,
		Health: HealthConfig{Timeout: 5 * time.Second, Interval: 10 * time.Millisecond}}
}

func TestFolderDeployOfTeployHomeAppIsRefused(t *testing.T) {
	mock, lk := gateMock(t, "fency", true)
	err := NewDeployer(mock, &bytes.Buffer{}).DeployFenced(context.Background(), gateConfig("fency", ""), lk)
	if err == nil || !strings.Contains(err.Error(), "edited in Teploy") {
		t.Fatalf("folder deploy of a Teploy-home app = %v, want refusal", err)
	}
	// The gate repaired the missing state into a v3 tombstone on the way.
	st := string(mock.Files["/deployments/fency/state.json"])
	if !strings.Contains(st, `"tombstone":true`) || !strings.Contains(st, `"schema_version":3`) {
		t.Fatalf("state after the gate = %s, want a v3 tombstone", st)
	}
	for _, c := range mock.Calls {
		if strings.Contains(c, "docker run") {
			t.Fatalf("refused deploy started a container: %s", c)
		}
	}
}

func TestTeployHomeDeployCarriesV3AndConfigCommit(t *testing.T) {
	mock, lk := gateMock(t, "fency", true)
	if err := NewDeployer(mock, &bytes.Buffer{}).DeployFenced(context.Background(), gateConfig("fency", gateHead), lk); err != nil {
		t.Fatalf("Teploy-home deploy: %v", err)
	}
	st := string(mock.Files["/deployments/fency/state.json"])
	if !strings.Contains(st, `"schema_version":3`) || strings.Contains(st, `"tombstone":true`) {
		t.Fatalf("state after a Teploy-home deploy = %s, want v3 without tombstone", st)
	}
	var recorded bool
	for path, data := range mock.Files {
		if strings.HasPrefix(path, "/deployments/fency/meta/") && bytes.Contains(data, []byte(`"config_commit":"`+gateHead+`"`)) {
			recorded = true
		}
	}
	if !recorded {
		t.Fatal("release record does not carry config_commit")
	}
}

func TestStaleConfigCommitIsRefused(t *testing.T) {
	mock, lk := gateMock(t, "fency", true)
	err := NewDeployer(mock, &bytes.Buffer{}).DeployFenced(context.Background(), gateConfig("fency", strings.Repeat("a", 40)), lk)
	if err == nil || !strings.Contains(err.Error(), "config changed") {
		t.Fatalf("deploy of a superseded config commit = %v, want refusal", err)
	}
}

func TestOrphanedAppIsRefused(t *testing.T) {
	mock, lk := gateMock(t, "fency", false)
	mock.Files["/deployments/fency/state.json"] = []byte(`{"schema_version":3,"tombstone":true,"generation":1,"deployment_type":"container","ingress_mode":"caddy","updated_at":"2026-10-19T12:00:00Z","operation_id":"op"}`)
	err := NewDeployer(mock, &bytes.Buffer{}).DeployFenced(context.Background(), gateConfig("fency", ""), lk)
	if !errors.Is(err, confighome.ErrOrphaned) {
		t.Fatalf("deploy of an orphaned app = %v, want ErrOrphaned", err)
	}
}

// An older CLI deployed a Teploy-home app from a folder (state v2 next to
// config.git): the next new-CLI touch re-marks it v3 and warns, without
// touching containers.
func TestGateRepairsOldClientFolderDeploy(t *testing.T) {
	mock, lk := gateMock(t, "fency", true)
	mock.Files["/deployments/fency/state.json"] = []byte(`{"schema_version":2,"generation":3,"current_hash":"old","deployment_type":"container","ingress_mode":"caddy","updated_at":"2026-10-19T12:00:00Z","operation_id":"op"}`)
	var out bytes.Buffer
	gate, err := confighome.CheckGate(context.Background(), mock, "fency", lk, &out)
	if err != nil || !gate.Server || gate.Head != gateHead {
		t.Fatalf("gate = %+v, %v", gate, err)
	}
	st := string(mock.Files["/deployments/fency/state.json"])
	if !strings.Contains(st, `"schema_version":3`) || !strings.Contains(st, `"generation":4`) || !strings.Contains(st, `"current_hash":"old"`) {
		t.Fatalf("repaired state = %s, want v3 generation 4 keeping the release", st)
	}
	if !strings.Contains(out.String(), "older teploy CLI") {
		t.Fatalf("repair did not warn: %q", out.String())
	}
}
