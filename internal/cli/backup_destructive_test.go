package cli

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/ssh"
)

func TestPurgeRefusesWithNoTarget(t *testing.T) {
	mock := ssh.NewMockExecutor("h", ssh.MockCommand{Match: "for p in /etc/teploy/backup-target.json", Output: "absent\n"})
	err := backupBeforePurge(context.Background(), mock, "a", []string{"db"}, true, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "--no-backup") || !strings.Contains(err.Error(), "no backup target") {
		t.Fatalf("err = %v", err)
	}
	// Nothing to destroy: no target needed.
	if err := backupBeforePurge(context.Background(), ssh.NewMockExecutor("h"), "a", nil, false, io.Discard); err != nil {
		t.Fatal(err)
	}
}

func TestPurgeRemovesContainersWithTheirVolumes(t *testing.T) {
	cmds := append([]ssh.MockCommand{
		removeTestContainers(),
		{Match: "docker stop -t 10", Output: ""},
		{Match: "docker rm ", Output: ""},
		{Match: "rm -rf '/deployments/scratch'", Output: ""},
		{Match: "rmdir '/deployments/scratch'", Output: ""},
	}, caddyMocks()...)
	exec := ssh.NewMockExecutor("h", append(cmds, removeAutomationMocks()...)...)
	if _, err := executeRemove(context.Background(), exec, "scratch", []string{"scratch.com"}, removeOptions{Purge: true}, io.Discard); err != nil {
		t.Fatal(err)
	}
	saw := false
	for _, c := range exec.Calls {
		if strings.HasPrefix(c, "docker rm ") {
			if !strings.HasPrefix(c, "docker rm -v ") {
				t.Fatalf("a purge left anonymous volumes behind: %s", c)
			}
			saw = true
		}
	}
	if !saw {
		t.Fatalf("no docker rm: %v", exec.Calls)
	}
	// Without --purge a container's anonymous volume is not this command's to take.
	cmds = append([]ssh.MockCommand{
		removeTestContainers(),
		{Match: "docker stop -t 10", Output: ""},
		{Match: "docker rm ", Output: ""},
		{Match: "rm -rf '/deployments/scratch", Output: ""},
		{Match: "rmdir '/deployments/scratch'", Output: ""},
		{Match: "find ", Output: ""},
		{Match: "rm -f", Output: ""},
	}, caddyMocks()...)
	exec = ssh.NewMockExecutor("h", append(cmds, removeAutomationMocks()...)...)
	_, _ = executeRemove(context.Background(), exec, "scratch", []string{"scratch.com"}, removeOptions{}, io.Discard)
	for _, c := range exec.Calls {
		if strings.HasPrefix(c, "docker rm -v ") {
			t.Fatalf("no purge, no -v: %s", c)
		}
	}
}

// The deploy hook backup needs no target until there is something to protect.
func TestBackupBeforeDeployHook(t *testing.T) {
	cfg := &config.AppConfig{App: "a", Accessories: map[string]config.AccessoryConfig{"db": {Image: "postgres:17"}}}
	none := ssh.NewMockExecutor("h", ssh.MockCommand{Match: "docker inspect", Err: errors.New("exit status 1: no such object")})
	if err := backupBeforeDeployHook(context.Background(), none, cfg, io.Discard); err != nil {
		t.Fatalf("a first deploy has nothing to protect: %v", err)
	}
	running := ssh.NewMockExecutor("h",
		ssh.MockCommand{Match: "docker inspect -f '{{.Config.Image}}'", Output: "postgres:17\n"},
		ssh.MockCommand{Match: "docker inspect -f '{{range .Config.Env}}", Output: "PG_MAJOR=17\n"},
		ssh.MockCommand{Match: "docker inspect -f", Output: "{}\n[]\n[]\n"},
		ssh.MockCommand{Match: "for p in /etc/teploy/backup-target.json", Output: "absent\n"},
	)
	err := backupBeforeDeployHook(context.Background(), running, cfg, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "--no-backup") {
		t.Fatalf("err = %v", err)
	}
}
