package cli

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/backup"
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

// A docker blip is not "no container": a purge must not go ahead unprotected.
func TestPreOperationBackupInspectBlipRefuses(t *testing.T) {
	mock := ssh.NewMockExecutor("h", ssh.MockCommand{Match: "docker inspect -f", Err: errors.New("exit status 1: Cannot connect to the Docker daemon")})
	_, err := preOperationBackup(context.Background(), backup.NewClient(mock, io.Discard), mock, "a", "db", backup.S3Config{Bucket: "b", Region: "r"}, "remove --purge", preBackupScope{All: true})
	if err == nil || !strings.Contains(err.Error(), "--no-backup") {
		t.Fatalf("err = %v", err)
	}
}

// An unreadable volumes tree is not an empty one.
func TestPreOperationBackupUnreadableVolumesRefuse(t *testing.T) {
	mock := ssh.NewMockExecutor("h", ssh.MockCommand{Match: "if [ ! -e '/deployments/a/volumes'", Err: errors.New("exit status 1: ls: permission denied")})
	_, err := preOperationBackup(context.Background(), backup.NewClient(mock, io.Discard), mock, "a", "", backup.S3Config{Bucket: "b", Region: "r"}, "this restore", preBackupScope{})
	if err == nil || !strings.Contains(err.Error(), "--no-backup") {
		t.Fatalf("err = %v", err)
	}
}

// Purge and upgrade destroy every database, so every one must be stored; a name
// the tools cannot address refuses. Mock mysql with one such schema.
func TestPreOperationBackupAllCoversEveryDatabase(t *testing.T) {
	calls := func(extra ...ssh.MockCommand) *ssh.MockExecutor {
		return ssh.NewMockExecutor("h", append(extra,
			ssh.MockCommand{Match: "which aws", Output: "/usr/bin/aws\n"},
			ssh.MockCommand{Match: "docker inspect -f '{{.Config.Image}}'", Output: "mysql:8\n"},
			ssh.MockCommand{Match: "docker inspect -f '{{range .Config.Env}}", Output: "MYSQL_VERSION=8\nMYSQL_DATABASE=app\nMYSQL_ROOT_PASSWORD=pw\n"},
			ssh.MockCommand{Match: "docker inspect -f", Output: "{}\n[]\n[]\n"},
			ssh.MockCommand{Match: "umask 077", Output: "/var/tmp/teploy-backup.t\n"},
			ssh.MockCommand{Match: "docker exec 'a-db' sh -c 'MYSQL_PWD=${MARIADB_ROOT_PASSWORD:-${MYSQL_ROOT_PASSWORD:-${MYSQL_PASSWORD:-}}}; export MYSQL_PWD; exec mysql -u root -N -B -e '\"'\"'SELECT COUNT(*)", Output: "1\n"},
			ssh.MockCommand{Match: "docker exec", Output: ""},
			ssh.MockCommand{Match: "sha256sum", Output: "abc  f\n4096\n"},
			ssh.MockCommand{Match: "{ cat /etc/machine-id", Output: "0123456789abcdef0123456789abcdef  -\n"},
			ssh.MockCommand{Match: "{ ", Output: "abc  -\n"},
			ssh.MockCommand{Match: "aws s3api head-object", Output: `{"ContentLength":4096,"Metadata":{"sha256":"abc"}}`},
			ssh.MockCommand{Match: "aws s3", Output: ""},
			ssh.MockCommand{Match: "mkdir -p", Output: ""},
			ssh.MockCommand{Match: "rm -rf", Output: ""},
			ssh.MockCommand{Match: "wc -c", Output: "4096\n"},
			ssh.MockCommand{Match: "df -Pk", Output: "Filesystem 1K-blocks Used Available Use% Mounted\n/dev/x 1 1 99999999 1% /\n"},
			ssh.MockCommand{Match: "printf", Output: ""},
			ssh.MockCommand{Match: "python3 -c", Output: `{"seq":1,"ts":"2026-01-01T00:00:00Z"}`},
		)...)
	}
	schemas := func(rows string) ssh.MockCommand {
		return ssh.MockCommand{Match: "docker exec 'a-db' sh -c 'MYSQL_PWD=${MARIADB_ROOT_PASSWORD:-${MYSQL_ROOT_PASSWORD:-${MYSQL_PASSWORD:-}}}; export MYSQL_PWD; exec mysql -u root -N -B -e '\"'\"'SELECT s.schema_name", Output: rows}
	}
	cfg := backup.S3Config{Bucket: "b", Region: "r"}
	mock := calls(schemas("app\t0\nanalytics\t0\n"))
	id, err := preOperationBackup(context.Background(), backup.NewClient(mock, io.Discard), mock, "a", "db", cfg, "remove --purge", preBackupScope{All: true})
	if err != nil || id == "" {
		t.Fatalf("%q %v", id, err)
	}
	joined := strings.Join(mock.Calls, "\n")
	if !strings.Contains(joined, ".incoming/a/accessories/db/databases/analytics/") {
		t.Fatalf("analytics was not stored: %s", joined)
	}
	if !strings.Contains(joined, "/safety/"+id+".safety") {
		t.Fatalf("no safety marker: %s", joined)
	}
	mock = calls(schemas("app\t0\nit's\t0\n"))
	if _, err := preOperationBackup(context.Background(), backup.NewClient(mock, io.Discard), mock, "a", "db", cfg, "remove --purge", preBackupScope{All: true}); err == nil || !strings.Contains(err.Error(), "it's") || !strings.Contains(err.Error(), "--no-backup") {
		t.Fatalf("err = %v", err)
	}
}
