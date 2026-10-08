package deploy

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/ssh"
)

func TestVolumeOwnershipFailurePrecedesContainerStart(t *testing.T) {
	uid, gid := uint32(10001), uint32(10001)
	m := ssh.NewMockExecutor("host",
		ssh.MockCommand{Match: "docker image inspect", Output: "sha256:" + strings.Repeat("a", 64)},
		ssh.MockCommand{Match: "if [ ! -e '/deployments/observe/state.json' ]", Output: "absent"},
		ssh.MockCommand{Match: "if [ ! -e '/deployments/observe/state' ]", Output: "absent"},
		ssh.MockCommand{Match: "teploy_volume_actor=", Err: errors.New("provision refused")},
	)
	cfg := Config{App: "observe", Image: "observe:test", Version: "v1", Ingress: "host", ContainerPort: 5080,
		Volumes:         map[string]string{"/deployments/observe/volumes/observe-state": "/var/lib/observe"},
		VolumeOwnership: map[string]config.VolumeOwnership{"observe-state": {UID: &uid, GID: &gid, Mode: "0700"}},
	}
	err := NewDeployer(m, &bytes.Buffer{}).DeployLocked(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), "admitting managed directory") {
		t.Fatalf("ownership refusal: %v calls=%v", err, m.Calls)
	}
	for _, command := range m.Calls {
		if strings.Contains(command, "docker run") || strings.Contains(command, "docker stop") {
			t.Fatalf("container affected before ownership established: %s", command)
		}
	}
}
