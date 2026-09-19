package backup

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/ssh"
)

// CLI-007: restore used to tar -xzf straight into the live directory, so a
// truncated/corrupt archive could leave it in a mixed partial state. It must
// now extract into an isolated staging directory first and only touch the
// live directory (via a promote step) once that succeeds.

func TestRestoreVolumes_StagesBeforePromoting(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "which aws", Output: "/usr/bin/aws\n"},
		ssh.MockCommand{Match: "aws s3 cp", Output: "download: done\n"},
		ssh.MockCommand{Match: "umask 077", Output: "/tmp/teploy-volume-restore.abc123\n"},
		ssh.MockCommand{Match: "rm -rf", Output: ""},
		ssh.MockCommand{Match: "mkdir -p", Output: ""},
		ssh.MockCommand{Match: "mktemp -d", Output: "/deployments/myapp/volumes.restore-old.abc123\n"},
		ssh.MockCommand{Match: "find ", Output: ""},
		ssh.MockCommand{Match: "cp -a ", Output: ""},
		ssh.MockCommand{Match: "if [ -f", Output: ""},
		// A39's env commit is a set -eu script.
		ssh.MockCommand{Match: "set -eu", Output: "Restored app .env\n"},
	)

	var buf bytes.Buffer
	client := NewClient(mock, &buf)
	err := client.RestoreVolumes(context.Background(), "myapp", "20260101-000000", S3Config{
		Bucket: "my-bucket", Region: "us-east-1",
	})
	if err != nil {
		t.Fatalf("RestoreVolumes: %v", err)
	}

	const runDir = "/tmp/teploy-volume-restore.abc123"
	var stageExtract, moveAside, copyIn string
	for _, call := range mock.Calls {
		if strings.Contains(call, "tar -xzf") {
			stageExtract = call
		}
		if strings.Contains(call, "mv -t") && strings.Contains(call, "restore-old.abc123") {
			moveAside = call
		}
		if strings.Contains(call, "cp -a ") {
			copyIn = call
		}
	}
	if stageExtract == "" || !strings.Contains(stageExtract, runDir+"/stage") {
		t.Errorf("expected extraction into a staging directory under the private run dir, got calls: %v", mock.Calls)
	}
	if strings.Contains(stageExtract, "/deployments/myapp/volumes") {
		t.Errorf("extraction must not target the live volumes directory directly: %s", stageExtract)
	}
	// audit F02: the move-aside and the copy-in must be SEPARATE commands
	// with different recovery procedures, not one `&&` chain.
	if moveAside == "" || !strings.Contains(moveAside, "/deployments/myapp/volumes") {
		t.Errorf("expected a move-aside step into the recovery directory, got calls: %v", mock.Calls)
	}
	if strings.Contains(moveAside, "cp -a") {
		t.Errorf("move-aside must not be chained with the copy (one `&&` chain cannot recover a partial move): %s", moveAside)
	}
	if copyIn == "" || !strings.Contains(copyIn, "/deployments/myapp/volumes") {
		t.Errorf("expected a copy-in step into the live volumes directory, got calls: %v", mock.Calls)
	}
	if strings.Contains(copyIn, "mv -t") {
		t.Errorf("copy-in must not be chained with the move-aside: %s", copyIn)
	}

	// teploy-cli-09: the backup's .env member must be lifted out of the
	// staging tree BEFORE promotion (it belongs beside volumes/, not inside
	// them), then installed at /deployments/myapp/.env with the previous
	// file kept recoverable. The old-format nested member
	// (deployments/<app>/.env) must be recognized and cleared too.
	var setAside, envInstall string
	for _, call := range mock.Calls {
		if strings.Contains(call, runDir+"/stage/.env") {
			setAside = call
		}
		if strings.Contains(call, "/deployments/myapp/.env") {
			envInstall = call
		}
	}
	if setAside == "" {
		t.Fatalf("expected a set-aside step for the staged .env, got calls: %v", mock.Calls)
	}
	if !strings.Contains(setAside, runDir+"/new.env") {
		t.Errorf("set-aside must move the .env out of the staging tree, got: %s", setAside)
	}
	// audit F32: the legacy-layout probe must test the FILE
	// deployments/<app>/.env, not the deployments DIRECTORY (a directory
	// always failed -f, so the branch was dead and legacy envs were left
	// behind in staging).
	if !strings.Contains(setAside, runDir+"/stage/deployments/myapp/.env") {
		t.Errorf("set-aside must probe the legacy FILE path (deployments/<app>/.env), got: %s", setAside)
	}
	if strings.Contains(setAside, "[ -f '"+runDir+"/stage/deployments' ]") {
		t.Errorf("set-aside must not test the deployments DIRECTORY with -f: %s", setAside)
	}
	if envInstall == "" {
		t.Fatalf("expected an .env install step, got calls: %v", mock.Calls)
	}
	// A39: the new env is staged as a private sibling on the DESTINATION
	// filesystem (never a cross-filesystem mv from /tmp), secured at 0600
	// before publication, and the old file's recovery copy is mandatory.
	if !strings.Contains(envInstall, "mktemp '/deployments/myapp/.env-new.") {
		t.Errorf(".env must be staged beside the destination on the same filesystem, got: %s", envInstall)
	}
	if !strings.Contains(envInstall, "chmod 600 \"$new\"") || !strings.Contains(envInstall, "chmod 600 \"$old\"") {
		t.Errorf("both staged files must be secured before publication, got: %s", envInstall)
	}
	if !strings.Contains(envInstall, "mv -fT -- \"$new\" '/deployments/myapp/.env'") {
		t.Errorf(".env publication must be the atomic rename of the staged sibling, got: %s", envInstall)
	}
	if !strings.Contains(envInstall, ".env-old.") || !strings.Contains(envInstall, ".env.pre-restore") {
		t.Errorf("the old .env's recovery copy must be staged then renamed into place, got: %s", envInstall)
	}
	if !strings.Contains(envInstall, "set -eu") {
		t.Errorf("the env commit must abort on the first failed step, got: %s", envInstall)
	}
}

func TestRestoreVolumes_ExtractionFailureLeavesLiveDirUntouched(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "which aws", Output: "/usr/bin/aws\n"},
		ssh.MockCommand{Match: "aws s3 cp", Output: "download: done\n"},
		ssh.MockCommand{Match: "umask 077", Output: "/tmp/teploy-volume-restore.abc123\n"},
		ssh.MockCommand{Match: "rm -rf", Err: fmt.Errorf("tar: unexpected end of file")},
	)

	var buf bytes.Buffer
	client := NewClient(mock, &buf)
	err := client.RestoreVolumes(context.Background(), "myapp", "20260101-000000", S3Config{
		Bucket: "my-bucket", Region: "us-east-1",
	})
	if err == nil {
		t.Fatal("expected an error from a failed extraction")
	}
	for _, call := range mock.Calls {
		if strings.Contains(call, "-mindepth 1 -delete") {
			t.Errorf("live directory was touched despite a failed extraction: %s", call)
		}
	}
}

// audit F02 (P0): when the move-aside fails PARTWAY (some entries moved into
// the recovery dir, some still live), the recovery branch must move the saved
// entries back WITHOUT deleting anything still in the live directory. The
// old recovery unconditionally ran `find live -mindepth 1 -delete` first,
// destroying the only copy of the unmoved originals.
func TestRestoreVolumes_PartialMoveAsideNeverDeletesLiveEntries(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "which aws", Output: "/usr/bin/aws\n"},
		ssh.MockCommand{Match: "aws s3 cp", Output: "download: done\n"},
		ssh.MockCommand{Match: "umask 077", Output: "/tmp/teploy-volume-restore.abc123\n"},
		ssh.MockCommand{Match: "rm -rf", Output: ""},
		ssh.MockCommand{Match: "mkdir -p", Output: ""},
		ssh.MockCommand{Match: "mktemp -d", Output: "/deployments/myapp/volumes.restore-old.abc123\n"},
		// The move-aside fails (partial mv), and the move-back also runs.
		ssh.MockCommand{Match: "if [ -f", Output: ""},
		ssh.MockCommand{Match: "find '/deployments/myapp/volumes'", Err: fmt.Errorf("mv: failed")},
		ssh.MockCommand{Match: "find '/deployments/myapp/volumes.restore-old.abc123'", Output: ""},
	)

	var buf bytes.Buffer
	client := NewClient(mock, &buf)
	err := client.RestoreVolumes(context.Background(), "myapp", "20260101-000000", S3Config{
		Bucket: "my-bucket", Region: "us-east-1",
	})
	if err == nil {
		t.Fatal("expected an error from the partial move-aside")
	}
	if !strings.Contains(err.Error(), "previous contents restored") && !strings.Contains(err.Error(), "recovery INCOMPLETE") {
		t.Errorf("error must describe the real recovery state, got: %v", err)
	}

	var moveBack string
	for _, call := range mock.Calls {
		if strings.HasPrefix(call, "find '/deployments/myapp/volumes.restore-old.abc123'") {
			moveBack = call
		}
	}
	if moveBack == "" {
		t.Fatal("expected the saved entries to be moved back")
	}
	if strings.Contains(moveBack, "-delete") {
		t.Errorf("partial-move recovery must never delete live entries: %s", moveBack)
	}
	// The copy-in must never run after a failed move-aside.
	for _, call := range mock.Calls {
		if strings.Contains(call, "cp -a ") {
			t.Errorf("copy-in ran despite a failed move-aside: %s", call)
		}
	}
}

// audit F31: the MySQL/MariaDB restore branch never assigned s3Key or
// restorePath, so the download and restore operated on empty arguments —
// every MySQL restore was deterministically broken.
func TestAccessoryRestore_MySQL_DownloadsAndRestoresRealPaths(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "which aws", Output: "/usr/bin/aws\n"},
		ssh.MockCommand{Match: "aws s3 cp", Output: "download: done\n"},
		ssh.MockCommand{Match: "mktemp -d '/tmp/teploy-restore.XXXXXX'", Output: "/tmp/teploy-restore.abc123\n"},
		ssh.MockCommand{Match: "gunzip -c", Output: ""},
		ssh.MockCommand{Match: "docker exec", Output: ""},
		ssh.MockCommand{Match: "rm -rf", Output: ""},
	)

	var buf bytes.Buffer
	client := NewClient(mock, &buf)
	err := client.AccessoryRestore(context.Background(), "myapp", "mysql", "mysql:8",
		"20260101-000000", nil, S3Config{Bucket: "my-bucket", Region: "us-east-1"})
	if err != nil {
		t.Fatalf("AccessoryRestore: %v", err)
	}

	var download, restore string
	for _, call := range mock.Calls {
		if strings.HasPrefix(call, "aws s3 cp") {
			download = call
		}
		if strings.Contains(call, "mysql -u root") {
			restore = call
		}
	}
	if download == "" || !strings.Contains(download, "s3://my-bucket/myapp/accessories/mysql/20260101-000000.sql.gz") {
		t.Errorf("download must fetch the engine's .sql.gz key, got: %s", download)
	}
	if download == "" || !strings.Contains(download, "/tmp/teploy-restore.abc123/restore.sql.gz") {
		t.Errorf("download must target a real per-run path, got: %s", download)
	}
	if restore == "" || !strings.Contains(restore, "restore.sql.gz") {
		t.Errorf("restore must gunzip the downloaded artifact, got: %s", restore)
	}
}

func TestAccessoryRestore_GenericStagesBeforePromoting(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "which aws", Output: "/usr/bin/aws\n"},
		ssh.MockCommand{Match: "aws s3 cp", Output: "download: done\n"},
		ssh.MockCommand{Match: "rm -rf", Output: ""},
		ssh.MockCommand{Match: "mkdir -p", Output: ""},
		ssh.MockCommand{Match: "mktemp -d '/tmp/teploy-restore.XXXXXX'", Output: "/tmp/teploy-restore.abc123\n"},
		ssh.MockCommand{Match: "mktemp -d", Output: "/deployments/myapp/accessories/cache.restore-old.abc123\n"},
		ssh.MockCommand{Match: "find ", Output: ""},
		ssh.MockCommand{Match: "cp -a ", Output: ""},
	)

	var buf bytes.Buffer
	client := NewClient(mock, &buf)
	// "unknown" doesn't match postgres/mysql/mongo/redis, so it takes the
	// generic tar branch.
	err := client.AccessoryRestore(context.Background(), "myapp", "cache", "some/unknown:latest",
		"20260101-000000", nil, S3Config{Bucket: "my-bucket", Region: "us-east-1"})
	if err != nil {
		t.Fatalf("AccessoryRestore: %v", err)
	}

	var stageExtract, moveAside, copyIn string
	for _, call := range mock.Calls {
		if strings.Contains(call, "tar -xzf") {
			stageExtract = call
		}
		if strings.Contains(call, "mv -t") {
			moveAside = call
		}
		if strings.Contains(call, "cp -a ") {
			copyIn = call
		}
	}
	if stageExtract == "" || !strings.Contains(stageExtract, "/tmp/teploy-restore.abc123/restore-stage") {
		t.Errorf("expected extraction into a staging dir under the per-invocation temp dir, got calls: %v", mock.Calls)
	}
	if moveAside == "" || !strings.Contains(moveAside, "/deployments/myapp/accessories/cache") {
		t.Errorf("expected a move-aside step into the recovery directory, got calls: %v", mock.Calls)
	}
	if copyIn == "" || !strings.Contains(copyIn, "/deployments/myapp/accessories/cache") {
		t.Errorf("expected a copy-in step into the live accessory directory, got calls: %v", mock.Calls)
	}
}

// audit F38: the redis restore chained `stop && cp && start` — a failed copy
// after a successful stop left the accessory stopped. The restore must save
// the current dump, and restore + restart the original when the replacement
// copy fails.
func TestAccessoryRestore_RedisRestartsAfterCopyFailure(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "which aws", Output: "/usr/bin/aws\n"},
		ssh.MockCommand{Match: "aws s3 cp", Output: "download: done\n"},
		ssh.MockCommand{Match: "mktemp -d '/tmp/teploy-restore.XXXXXX'", Output: "/tmp/teploy-restore.abc123\n"},
		// A40's preflight must prove appendonly=no before the script runs.
		ssh.MockCommand{Match: "docker exec 'myapp-redis' redis-cli --raw config get appendonly", Output: "appendonly no"},
		ssh.MockCommand{Match: "set -eu", Err: fmt.Errorf("exit status 1: docker cp failed")},
	)

	var buf bytes.Buffer
	client := NewClient(mock, &buf)
	err := client.AccessoryRestore(context.Background(), "myapp", "redis", "redis:7",
		"20260101-000000", nil, S3Config{Bucket: "my-bucket", Region: "us-east-1"})
	if err == nil {
		t.Fatal("expected the redis restore to fail on a copy error")
	}

	var script string
	for _, call := range mock.Calls {
		if strings.HasPrefix(call, "set -eu") {
			script = call
		}
	}
	if script == "" {
		t.Fatal("expected the redis restore script")
	}
	if !strings.Contains(script, "old-dump.rdb") {
		t.Errorf("restore must save the current dump before stopping: %s", script)
	}
	if !strings.Contains(script, "docker start") {
		t.Errorf("restore must restart redis on failure: %s", script)
	}
	// The gunzip must happen BEFORE docker stop, so artifact validation
	// costs no downtime.
	if gz, st := strings.Index(script, "gunzip -c"), strings.Index(script, "docker stop"); gz < 0 || st < 0 || gz > st {
		t.Errorf("gunzip must precede docker stop:\n%s", script)
	}
}

// teploy-cli-07: restore scratch files used fixed paths (/tmp/restore.sql.gz
// etc.), shared by every restore — concurrent restores or a crash followed by
// a retry could pick up the previous run's files. Each restore must allocate
// its own temp dir, remove it on success, and keep (and name) it on failure.
func TestAccessoryRestore_UsesPerInvocationTempDir(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "which aws", Output: "/usr/bin/aws\n"},
		ssh.MockCommand{Match: "aws s3 cp", Output: "download: done\n"},
		ssh.MockCommand{Match: "mktemp -d '/tmp/teploy-restore.XXXXXX'", Output: "/tmp/teploy-restore.abc123\n"},
		ssh.MockCommand{Match: "gunzip -c", Output: ""},
		ssh.MockCommand{Match: "docker exec", Output: ""},
		ssh.MockCommand{Match: "rm -rf", Output: ""},
	)

	var buf bytes.Buffer
	client := NewClient(mock, &buf)
	err := client.AccessoryRestore(context.Background(), "myapp", "postgres", "postgres:16",
		"20260101-000000", nil, S3Config{Bucket: "my-bucket", Region: "us-east-1"})
	if err != nil {
		t.Fatalf("AccessoryRestore: %v", err)
	}

	var restoreCmd, cleanup string
	for _, call := range mock.Calls {
		if strings.Contains(call, "psql") {
			restoreCmd = call
		}
		if strings.HasPrefix(call, "rm -rf '/tmp/teploy-restore.") {
			cleanup = call
		}
	}
	if restoreCmd == "" {
		t.Fatal("expected a psql restore command")
	}
	if !strings.Contains(restoreCmd, "/tmp/teploy-restore.abc123/restore.sql") {
		t.Errorf("restore must use a per-invocation temp path, got: %s", restoreCmd)
	}
	if strings.Contains(restoreCmd, "'/tmp/restore.sql'") {
		t.Errorf("restore must not use the shared fixed path /tmp/restore.sql: %s", restoreCmd)
	}
	if cleanup == "" {
		t.Errorf("success must remove the temp dir, got calls: %v", mock.Calls)
	}
}

func TestAccessoryRestore_FailureKeepsTempDir(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "which aws", Output: "/usr/bin/aws\n"},
		ssh.MockCommand{Match: "aws s3 cp", Output: "download: done\n"},
		ssh.MockCommand{Match: "mktemp -d '/tmp/teploy-restore.XXXXXX'", Output: "/tmp/teploy-restore.abc123\n"},
		ssh.MockCommand{Match: "gunzip -c", Err: fmt.Errorf("exit status 1: gzip: stdin: not in gzip format")},
	)

	var buf bytes.Buffer
	client := NewClient(mock, &buf)
	err := client.AccessoryRestore(context.Background(), "myapp", "postgres", "postgres:16",
		"20260101-000000", nil, S3Config{Bucket: "my-bucket", Region: "us-east-1"})
	if err == nil {
		t.Fatal("expected AccessoryRestore to fail when gunzip fails")
	}
	if !strings.Contains(err.Error(), "/tmp/teploy-restore.abc123") {
		t.Errorf("error must name the kept temp dir for inspection, got: %v", err)
	}
	for _, call := range mock.Calls {
		if strings.HasPrefix(call, "rm -rf '/tmp/teploy-restore.") {
			t.Errorf("failure must keep the temp dir, got cleanup: %s", call)
		}
	}
}
