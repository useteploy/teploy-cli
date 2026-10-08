package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/deploy"
	"github.com/useteploy/teploy/internal/multideploy"
	"github.com/useteploy/teploy/internal/ssh"
	"github.com/useteploy/teploy/internal/state"
)

func TestRound3FleetMigrationPreservesPublication(t *testing.T) {
	for _, outcome := range []error{&state.PublicationError{Committed: true, Err: errors.New("sidecar repair failed")}, &state.PublicationError{Unknown: true, Err: errors.New("authority read lost")}, errors.New("before publication")} {
		t.Run(outcome.Error(), func(t *testing.T) {
			mock := ssh.NewMockExecutor("host", ssh.MockCommand{Match: "mkdir /deployments/demo/.lock"},
				ssh.MockCommand{Match: "teploy_volume_actor", Output: ""},
				ssh.MockCommand{Match: "err=$(mktemp)", Output: "exists"},
				ssh.MockCommand{Match: "docker image inspect", Output: "sha256:" + strings.Repeat("a", 64)},
				ssh.MockCommand{Match: "docker stop", Output: ""},
				ssh.MockCommand{Match: "docker start", Output: ""},
				ssh.MockCommand{Match: "mkdir -p", Output: ""},
				ssh.MockCommand{Match: "cp -a", Output: ""},
				ssh.MockCommand{Match: "docker ps -aq", Output: "old-web"},
				ssh.MockCommand{Match: "docker inspect old-web", Output: "bind|/old/data|/data"},
				ssh.MockCommand{Match: "docker ps -q", Output: "old-web old-worker old-db"})
			cfg := &config.AppConfig{App: "demo", Image: "image:v2", Ingress: "external", Volumes: map[string]string{"data": "/data"}}
			server := newSingleServerDeployer(mock, io.Discard, "", true)
			reached := false
			server.deployEngine = func(context.Context, deploy.Config, *state.Lock) error { reached = true; return outcome }
			err := server.deployApp(context.Background(), cfg, nil, "", "v2")
			if !reached || !errors.Is(err, outcome) {
				t.Fatalf("did not reach engine: %v calls=%v", err, mock.Calls)
			}
			restored := map[string]bool{}
			for _, command := range mock.Calls {
				if name, quoted := strings.CutPrefix(command, "docker start 'old-"); quoted {
					if end := strings.Index(name, "'"); end >= 0 {
						restored["old-"+name[:end]] = true
					}
				}
			}
			if state.PreservePublishedState(outcome) && len(restored) != 0 {
				t.Fatalf("published old writers resurrected: %v", mock.Calls)
			}
			if !state.PreservePublishedState(outcome) && len(restored) != 3 {
				t.Fatalf("ordinary failure lost recovery (restarted %v): %v", restored, mock.Calls)
			}
		})
	}
}
func TestRound3FleetWithholdsAutomaticConvergence(t *testing.T) {
	cause := &state.PublicationError{Unknown: true, Err: errors.New("lost authority")}
	results := []multideploy.Result{{Server: "healthy", Success: true}, {Server: "unknown", Error: cause}}
	if !state.PreservePublishedState(fleetPublicationError(results)) {
		t.Fatal("fleet discarded publication outcome")
	}
}
func TestRound3ReviewedModesFreezeAndLockedValidation(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.Chmod(".", 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir("src", 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("src/run", []byte("same bytes"), 0755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.AppConfig{App: "demo", Context: ".", Type: config.TypeStatic, Source: "."}
	original, err := staticPlanFingerprint(nil, cfg)
	if err != nil {
		t.Fatal(err)
	}
	digest, _ := config.ExecutionBindingDigest(cfg, "")
	record := &PlanRecord{App: "demo", Server: "host", TargetVersion: "v1", ConfigDigest: digest, Image: PlanImageIdentity{NeedsBuild: true, ContextFingerprint: original}}
	// Both file and directory chmod must move the authorization identity.
	for _, path := range []string{"src/run", "src", "."} {
		if err := os.Chmod(path, 0700); err != nil {
			t.Fatal(err)
		}
		frozen, err := freezePlanInputs(nil, cfg)
		if err != nil {
			t.Fatal(err)
		}
		work, _ := os.Getwd()
		if err := os.Chdir(frozen); err != nil {
			t.Fatal(err)
		}
		err = revalidateLockedPlan(context.Background(), ssh.NewMockExecutor("host"), cfg, "", "v1", record)
		os.Chdir(work)
		os.RemoveAll(filepath.Dir(frozen))
		if !errors.Is(err, errPlanDrift) {
			t.Fatalf("chmod %s accepted: %v", path, err)
		}
		if err := os.Chmod(path, 0755); err != nil {
			t.Fatal(err)
		}
	}
	frozen, err := freezePlanInputs(nil, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(filepath.Dir(frozen))
	work, _ := os.Getwd()
	os.Chdir(frozen)
	defer os.Chdir(work)
	got, err := staticPlanFingerprint(nil, cfg)
	if err != nil || got != original {
		t.Fatalf("unchanged frozen modes differ: %s %s %v", got, original, err)
	}
	if _, err := os.Stat(filepath.Join(frozen, "src/run")); err != nil {
		t.Fatal(err)
	}
}
func TestRound3OwnershipFailurePrecedesSharedAndFleetEffects(t *testing.T) {
	uid, gid := uint32(10001), uint32(10001)
	cfg := &config.AppConfig{App: "demo", Image: "image:v1", Volumes: map[string]string{"data": "/data"}, VolumeOwnership: map[string]config.VolumeOwnership{"data": {UID: &uid, GID: &gid, Mode: "0700"}}, Accessories: map[string]config.AccessoryConfig{"db": {Image: "postgres:16"}}}
	for _, fleet := range []bool{false, true} {
		mock := &round3VolumeExecutor{ssh.NewMockExecutor("host", ssh.MockCommand{Match: "mkdir /deployments/demo/.lock"})}
		var err error
		if fleet {
			err = newSingleServerDeployer(mock, io.Discard, "", true).deployApp(context.Background(), cfg, nil, "", "v1")
		} else {
			err = deployBuiltImageFenced(context.Background(), mock, cfg, cfg.Image, "v1", "host", true, false, "", nil, nil, "")
		}
		if err == nil {
			t.Fatal("ownership refusal lost")
		}
		for _, command := range mock.Calls {
			for _, effect := range []string{"docker run", "docker rm", "docker stop", "cp -a", "mkdir -p /deployments/demo/volumes"} {
				if strings.Contains(command, effect) {
					t.Fatalf("effect before admission: %s", command)
				}
			}
		}
	}
}

type round3VolumeExecutor struct{ *ssh.MockExecutor }

func (e *round3VolumeExecutor) Run(ctx context.Context, command string) (string, error) {
	if strings.Contains(command, "' validate ") {
		return "", errors.New("populated mismatch")
	}
	return e.MockExecutor.Run(ctx, command)
}
