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
		ssh.MockCommand{Match: "rm -rf", Output: ""},
		ssh.MockCommand{Match: "mkdir -p", Output: ""},
		ssh.MockCommand{Match: "mktemp -d", Output: "/deployments/myapp/volumes.restore-old.abc123\n"},
		ssh.MockCommand{Match: "find ", Output: ""},
	)

	var buf bytes.Buffer
	client := NewClient(mock, &buf)
	err := client.RestoreVolumes(context.Background(), "myapp", "20260101-000000", S3Config{
		Bucket: "my-bucket", Region: "us-east-1",
	})
	if err != nil {
		t.Fatalf("RestoreVolumes: %v", err)
	}

	var stageExtract, promote string
	for _, call := range mock.Calls {
		if strings.Contains(call, "tar -xzf") {
			stageExtract = call
		}
		if strings.Contains(call, "cp -a") && strings.Contains(call, "mv -t") {
			promote = call
		}
	}
	if stageExtract == "" || !strings.Contains(stageExtract, "restore-stage") {
		t.Errorf("expected extraction into a staging directory, got calls: %v", mock.Calls)
	}
	if strings.Contains(stageExtract, "/deployments/myapp/volumes") {
		t.Errorf("extraction must not target the live volumes directory directly: %s", stageExtract)
	}
	if promote == "" || !strings.Contains(promote, "/deployments/myapp/volumes") {
		t.Errorf("expected a promote step (aside + copy) into the live volumes directory, got calls: %v", mock.Calls)
	}
	if !strings.Contains(promote, "restore-old.abc123") {
		t.Errorf("promote must move live contents aside to the recovery directory: %s", promote)
	}
}

func TestRestoreVolumes_ExtractionFailureLeavesLiveDirUntouched(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "which aws", Output: "/usr/bin/aws\n"},
		ssh.MockCommand{Match: "aws s3 cp", Output: "download: done\n"},
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

func TestAccessoryRestore_GenericStagesBeforePromoting(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "which aws", Output: "/usr/bin/aws\n"},
		ssh.MockCommand{Match: "aws s3 cp", Output: "download: done\n"},
		ssh.MockCommand{Match: "rm -rf", Output: ""},
		ssh.MockCommand{Match: "mkdir -p", Output: ""},
		ssh.MockCommand{Match: "mktemp -d '/tmp/teploy-restore.XXXXXX'", Output: "/tmp/teploy-restore.abc123\n"},
		ssh.MockCommand{Match: "mktemp -d", Output: "/deployments/myapp/accessories/cache.restore-old.abc123\n"},
		ssh.MockCommand{Match: "find ", Output: ""},
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

	var stageExtract, promote string
	for _, call := range mock.Calls {
		if strings.Contains(call, "tar -xzf") {
			stageExtract = call
		}
		if strings.Contains(call, "cp -a") && strings.Contains(call, "mv -t") {
			promote = call
		}
	}
	if stageExtract == "" || !strings.Contains(stageExtract, "/tmp/teploy-restore.abc123/restore-stage") {
		t.Errorf("expected extraction into a staging dir under the per-invocation temp dir, got calls: %v", mock.Calls)
	}
	if promote == "" || !strings.Contains(promote, "/deployments/myapp/accessories/cache") {
		t.Errorf("expected a promote step into the live accessory directory, got calls: %v", mock.Calls)
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
