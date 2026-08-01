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
		if strings.Contains(call, "-mindepth 1 -delete") {
			promote = call
		}
	}
	if stageExtract == "" || !strings.Contains(stageExtract, "restore-stage") {
		t.Errorf("expected extraction into a staging directory, got calls: %v", mock.Calls)
	}
	if strings.Contains(stageExtract, "/deployments/myapp/volumes") {
		t.Errorf("extraction must not target the live volumes directory directly: %s", stageExtract)
	}
	if promote == "" || !strings.Contains(promote, "cp -a") || !strings.Contains(promote, "/deployments/myapp/volumes") {
		t.Errorf("expected a promote step (clear + copy) into the live volumes directory, got calls: %v", mock.Calls)
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
		if strings.Contains(call, "-mindepth 1 -delete") {
			promote = call
		}
	}
	if stageExtract == "" || !strings.Contains(stageExtract, "restore-stage") {
		t.Errorf("expected extraction into a staging directory, got calls: %v", mock.Calls)
	}
	if promote == "" || !strings.Contains(promote, "/deployments/myapp/accessories/cache") {
		t.Errorf("expected a promote step into the live accessory directory, got calls: %v", mock.Calls)
	}
}
