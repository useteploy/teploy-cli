package docker

import (
	"context"
	"github.com/useteploy/teploy/internal/ssh"
	"strings"
	"testing"
	"time"
)

type campaignSlowRecreate struct {
	*ssh.MockExecutor
	cleanupLive bool
}

func (e *campaignSlowRecreate) Run(ctx context.Context, command string) (string, error) {
	if strings.HasPrefix(command, "docker run ") {
		time.Sleep(5100 * time.Millisecond)
	}
	if strings.HasPrefix(command, "rm -f -- '/tmp/.teploy-recreate-env-") {
		_, bounded := ctx.Deadline()
		e.cleanupLive = ctx.Err() == nil && bounded
	}
	return e.MockExecutor.Run(ctx, command)
}
func TestCampaignRecreateCleanupLifetime(t *testing.T) {
	exec := &campaignSlowRecreate{MockExecutor: ssh.NewMockExecutor("dummy", ssh.MockCommand{Match: "docker rm -f"}, ssh.MockCommand{Match: "docker run"}, ssh.MockCommand{Match: "rm -f --"})}
	spec := &RecreateSpec{Name: "demo", ImageRef: "demo:latest", Env: []string{"DUMMY=value"}}
	if err := NewClient(exec).Recreate(context.Background(), spec, nil); err != nil {
		t.Fatal(err)
	}
	if !exec.cleanupLive {
		t.Fatal("secret cleanup context already expired")
	}
}
