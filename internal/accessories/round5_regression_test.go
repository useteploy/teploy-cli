package accessories

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/ssh"
)

func TestRound5LogsIsPureStreamingWrapper(t *testing.T) {
	mock := ssh.NewMockExecutor("host", ssh.MockCommand{Match: "docker logs", Output: "actual-log\n"})
	var out bytes.Buffer
	if err := NewManager(mock, &out).Logs(context.Background(), "demo", "db", 20); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "actual-log") {
		t.Fatal("log stream lost")
	}
	for _, cmd := range mock.Calls {
		if !strings.Contains(cmd, "docker logs") {
			t.Fatalf("logs provisioned/started: %s", cmd)
		}
	}
}

type round5UpgradeExecutor struct {
	*ssh.MockExecutor
	running bool
	refuse  bool
}

func (e *round5UpgradeExecutor) Run(ctx context.Context, cmd string) (string, error) {
	if e.refuse && strings.Contains(cmd, "' validate ") {
		return "", errors.New("populated ownership mismatch")
	}
	if strings.Contains(cmd, "docker inspect") && strings.Contains(cmd, "State.Status") {
		if e.running {
			return "running", nil
		}
		return "", errors.New("absent")
	}
	if strings.Contains(cmd, "docker rm") {
		e.running = false
	}
	return e.MockExecutor.Run(ctx, cmd)
}
func TestRound5UnfencedUpgradeUsesRequestedImageBeforeOwnershipEffects(t *testing.T) {
	for _, refuse := range []bool{false, true} {
		t.Run(map[bool]string{false: "new-image", true: "ownership-refusal"}[refuse], func(t *testing.T) {
			mock := ssh.NewMockExecutor("host", append(admissionStubs("demo"),
				ssh.MockCommand{Match: "docker pull", Output: ""},
				ssh.MockCommand{Match: "docker stop", Output: ""},
				ssh.MockCommand{Match: "docker rm", Output: ""},
				ssh.MockCommand{Match: "docker run", Output: strings.Repeat("a", 64)},
				ssh.MockCommand{Match: "mkdir -p", Output: ""})...)
			e := &round5UpgradeExecutor{MockExecutor: mock, running: true, refuse: refuse}
			cfg := config.AccessoryConfig{Image: "image:A"}
			if refuse {
				uid, gid := uint32(10001), uint32(10001)
				cfg.VolumeOwnership = map[string]config.VolumeOwnership{"data": {UID: &uid, GID: &gid, Mode: "0700"}}
				cfg.Volumes = map[string]string{"data": "/data"}
			}
			err := NewManager(e, io.Discard).Upgrade(context.Background(), "demo", "db", "image:B", cfg)
			if refuse && err == nil {
				t.Fatal("ownership refusal bypassed")
			}
			if !refuse && err != nil {
				t.Fatal(err)
			}
			pull, create := false, false
			for _, cmd := range mock.Calls {
				if strings.Contains(cmd, "docker pull 'image:B'") {
					pull = true
				}
				if strings.Contains(cmd, "docker run") && strings.Contains(cmd, "'image:B'") {
					create = true
				}
				if refuse && (strings.Contains(cmd, "docker stop") || strings.Contains(cmd, "docker rm") || strings.Contains(cmd, "docker run")) {
					t.Fatalf("mutation before ownership proof: %s", cmd)
				}
			}
			if !refuse && (!pull || !create) {
				t.Fatalf("requested B not upgraded: %v", mock.Calls)
			}
		})
	}
}
