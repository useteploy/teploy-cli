package docker

import (
	"context"
	"errors"
	"github.com/useteploy/teploy/internal/ssh"
	"io"
	"strings"
	"testing"
)

func TestMigrationFailureResumesEveryWriter(t *testing.T) {
	mock := ssh.NewMockExecutor("test", ssh.MockCommand{Match: "docker ps -q", Output: "web1\nworker1\n"}, ssh.MockCommand{Match: "docker stop", Output: ""}, ssh.MockCommand{Match: "mkdir -p", Output: ""}, ssh.MockCommand{Match: "cp -a", Err: errors.New("copy failed")}, ssh.MockCommand{Match: "docker start", Output: ""})
	_, err := BeginVolumeMigration(context.Background(), mock, "demo", []VolumeMismatch{{ExistingSource: "/old", ExpectedSource: "/new"}}, io.Discard)
	if err == nil {
		t.Fatal("copy failure must fail")
	}
	all := strings.Join(mock.Calls, "\n")
	for _, id := range []string{"web1", "worker1"} {
		if !strings.Contains(all, "docker start '"+id+"'") {
			t.Fatalf("writer %s not recovered: %s", id, all)
		}
	}
}

func TestMigrationSuccessfulCopyStaysQuiescedUntilCommit(t *testing.T) {
	mock := ssh.NewMockExecutor("test", ssh.MockCommand{Match: "docker ps -q", Output: "web1"}, ssh.MockCommand{Match: "docker stop", Output: ""}, ssh.MockCommand{Match: "mkdir -p", Output: ""}, ssh.MockCommand{Match: "cp -a", Output: ""}, ssh.MockCommand{Match: "docker start", Output: ""})
	migration, err := BeginVolumeMigration(context.Background(), mock, "demo", []VolumeMismatch{{ExistingSource: "/old", ExpectedSource: "/new"}}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	for _, call := range mock.Calls {
		if strings.HasPrefix(call, "docker start") {
			t.Fatal("reopened writes before commit")
		}
	}
	migration.Commit()
	if err := migration.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, call := range mock.Calls {
		if strings.HasPrefix(call, "docker start") {
			t.Fatal("committed migration restarted displaced source")
		}
	}
}
