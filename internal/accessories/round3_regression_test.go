package accessories

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/ssh"
)

type round3AccessoryExecutor struct{ *ssh.MockExecutor }

func (e *round3AccessoryExecutor) UnderFence() bool { return true }
func (e *round3AccessoryExecutor) Run(ctx context.Context, command string) (string, error) {
	if strings.Contains(command, "' validate ") {
		return "", errors.New("populated mismatch")
	}
	return e.MockExecutor.Run(ctx, command)
}
func TestRound3RunningAccessoryMustAdmitOwnershipBeforeAnyEffect(t *testing.T) {
	uid, gid := uint32(10001), uint32(10001)
	mock := ssh.NewMockExecutor("host", ssh.MockCommand{Match: "docker inspect", Output: "running"})
	executor := &round3AccessoryExecutor{mock}
	cfg := config.AccessoryConfig{Image: "postgres:16", Volumes: map[string]string{"data": "/var/lib/postgresql/data"}, VolumeOwnership: map[string]config.VolumeOwnership{"data": {UID: &uid, GID: &gid, Mode: "0700"}}}
	if _, err := NewManager(executor, io.Discard).EnsureRunning(context.Background(), "demo", "db", cfg); err == nil {
		t.Fatal("running fast path bypassed ownership")
	}
	if len(mock.Calls) != 0 {
		t.Fatalf("effect/inspection before ownership admission: %v", mock.Calls)
	}
}
