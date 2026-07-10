package backup

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/ssh"
)

func TestLatestBackupDate_PicksNewest(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "which aws", Output: "/usr/bin/aws\n"},
		ssh.MockCommand{Match: "aws s3 ls", Output: "" +
			"2026-07-08 04:00:01     1000 20260708-040000.sql.gz\n" +
			"2026-07-10 04:00:02     2222 20260710-040000.sql.gz\n" +
			"2026-07-09 04:00:01     1500 20260709-040000.sql.gz\n"},
	)
	client := NewClient(mock, &bytes.Buffer{})
	date, size, err := client.LatestBackupDate(context.Background(), "myapp", "db", S3Config{Bucket: "b", Region: "us-east-1"})
	if err != nil {
		t.Fatalf("LatestBackupDate: %v", err)
	}
	if date != "20260710-040000" {
		t.Errorf("expected newest date 20260710-040000, got %s", date)
	}
	if size != 2222 {
		t.Errorf("expected size 2222, got %d", size)
	}
}

func TestLatestBackupDate_NoBackups(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "aws s3 ls", Output: "\n"},
	)
	client := NewClient(mock, &bytes.Buffer{})
	_, _, err := client.LatestBackupDate(context.Background(), "myapp", "db", S3Config{Bucket: "b", Region: "us-east-1"})
	if err == nil || !strings.Contains(err.Error(), "no backups") {
		t.Fatalf("expected no-backups error, got %v", err)
	}
}

// TestVerifyBackup_Postgres walks the full happy path and asserts the
// invariants that make verification trustworthy: image+env come from docker
// inspect (not config), the restore targets a SCRATCH container (never the
// real accessory), the verify query runs, and teardown removes the scratch.
func TestVerifyBackup_Postgres(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "which aws", Output: "/usr/bin/aws\n"},
		ssh.MockCommand{Match: "docker inspect -f '{{.Config.Image}}'", Output: "postgres:16\n"},
		ssh.MockCommand{Match: "docker inspect -f '{{range .Config.Env}}", Output: "POSTGRES_PASSWORD=sekret\nPOSTGRES_DB=appdb\nPATH=/usr/bin\n"},
		ssh.MockCommand{Match: "aws s3 ls", Output: "2026-07-10 04:00:02  2222 20260710-040000.sql.gz\n"},
		ssh.MockCommand{Match: "aws s3 cp", Output: "done\n"},
		ssh.MockCommand{Match: "docker run -d", Output: ""},
		ssh.MockCommand{Match: "for i in $(seq", Output: ""},
		ssh.MockCommand{Match: "gunzip -c", Output: ""},
		ssh.MockCommand{Match: "docker exec", Output: "42\n"},
		ssh.MockCommand{Match: "docker rm -f", Output: ""},
	)
	client := NewClient(mock, &bytes.Buffer{})
	res, err := client.VerifyBackup(context.Background(), "myapp", "db", "", S3Config{Bucket: "b", Region: "us-east-1"})
	if err != nil {
		t.Fatalf("VerifyBackup: %v", err)
	}
	if !res.OK {
		t.Fatalf("expected OK result, got detail=%s", res.Detail)
	}
	if res.Kind != "postgres" || res.Metric != "tables=42" || res.Date != "20260710-040000" {
		t.Errorf("unexpected result: %+v", res)
	}

	var sawScratchRun, sawRestore, sawTeardown bool
	for _, call := range mock.Calls {
		if strings.Contains(call, "docker run -d") {
			sawScratchRun = true
			if !strings.Contains(call, "myapp-db-verify") {
				t.Errorf("scratch run must use the -verify container name: %s", call)
			}
			if !strings.Contains(call, "POSTGRES_PASSWORD=sekret") {
				t.Errorf("scratch must clone source POSTGRES_* env: %s", call)
			}
			if strings.Contains(call, "PATH=") {
				t.Errorf("scratch must not clone unrelated env like PATH: %s", call)
			}
		}
		if strings.Contains(call, "gunzip -c") && strings.Contains(call, "psql") {
			sawRestore = true
			if !strings.Contains(call, "myapp-db-verify") {
				t.Errorf("restore must target the scratch container, got: %s", call)
			}
			if !strings.Contains(call, "'appdb'") {
				t.Errorf("restore must target POSTGRES_DB from inspected env: %s", call)
			}
		}
		if strings.Contains(call, "docker rm -f") && strings.Contains(call, "myapp-db-verify") {
			sawTeardown = true
		}
	}
	if !sawScratchRun || !sawRestore || !sawTeardown {
		t.Fatalf("missing steps: scratch=%v restore=%v teardown=%v", sawScratchRun, sawRestore, sawTeardown)
	}
}

// TestVerifyBackup_FailureIsResultAndTearsDown asserts a failed restore
// yields ok=false (not an operational error) and STILL removes the scratch.
func TestVerifyBackup_FailureIsResultAndTearsDown(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "which aws", Output: "/usr/bin/aws\n"},
		ssh.MockCommand{Match: "docker inspect -f '{{.Config.Image}}'", Output: "postgres:16\n"},
		ssh.MockCommand{Match: "docker inspect -f '{{range .Config.Env}}", Output: "POSTGRES_PASSWORD=x\n"},
		ssh.MockCommand{Match: "aws s3 cp", Output: "done\n"},
		ssh.MockCommand{Match: "docker run -d", Output: ""},
		ssh.MockCommand{Match: "for i in $(seq", Output: ""},
		ssh.MockCommand{Match: "gunzip -c", Err: fmt.Errorf("psql: ERROR: syntax error")},
		ssh.MockCommand{Match: "docker rm -f", Output: ""},
	)
	client := NewClient(mock, &bytes.Buffer{})
	res, err := client.VerifyBackup(context.Background(), "myapp", "db", "20260710-040000", S3Config{Bucket: "b", Region: "us-east-1"})
	if err != nil {
		t.Fatalf("failed verification must be a result, not an error: %v", err)
	}
	if res.OK {
		t.Fatal("expected ok=false")
	}
	if !strings.Contains(res.Detail, "restore into scratch failed") {
		t.Errorf("detail should carry the failure: %s", res.Detail)
	}
	teardown := false
	for _, call := range mock.Calls {
		if strings.Contains(call, "docker rm -f") && strings.Contains(call, "myapp-db-verify") {
			teardown = true
		}
	}
	if !teardown {
		t.Fatal("scratch container must be removed even on failure")
	}
}

// TestVerifyBackup_NucleusBootVerify covers the engine-boot verification for
// nucleus accessories (generic tar + scratch engine on the restored data dir).
func TestVerifyBackup_NucleusBootVerify(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "which aws", Output: "/usr/bin/aws\n"},
		ssh.MockCommand{Match: "docker inspect -f '{{.Config.Image}}'", Output: "ghcr.io/neutron-build/nucleus:latest\n"},
		ssh.MockCommand{Match: "docker inspect -f '{{range .Config.Env}}", Output: "NUCLEUS_ALLOW_NO_AUTH=1\n"},
		ssh.MockCommand{Match: "aws s3 ls", Output: "2026-07-10 04:00:02  99 20260710-040000.tar.gz\n"},
		ssh.MockCommand{Match: "aws s3 cp", Output: "done\n"},
		ssh.MockCommand{Match: "mkdir -p", Output: ""},
		ssh.MockCommand{Match: "docker run -d", Output: ""},
		ssh.MockCommand{Match: "for i in $(seq", Output: "Restored 57 table\n"},
		ssh.MockCommand{Match: "docker rm -f", Output: ""},
	)
	client := NewClient(mock, &bytes.Buffer{})
	res, err := client.VerifyBackup(context.Background(), "observe", "nucleus", "", S3Config{Bucket: "b", Region: "us-east-1"})
	if err != nil {
		t.Fatalf("VerifyBackup: %v", err)
	}
	if !res.OK || res.Kind != "nucleus" {
		t.Fatalf("unexpected result: %+v", res)
	}
	if res.Metric != "restored 57 tables" {
		t.Errorf("metric should parse the boot-restore line, got %q", res.Metric)
	}
	for _, call := range mock.Calls {
		if strings.Contains(call, "docker run -d") && !strings.Contains(call, "NUCLEUS_ALLOW_NO_AUTH") {
			t.Errorf("scratch nucleus must clone NUCLEUS_* env: %s", call)
		}
	}
}

// TestVerifyBackup_VolumeArchive covers the generic path: extraction is the
// integrity check, and an empty archive fails.
func TestVerifyBackup_VolumeArchive(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "which aws", Output: "/usr/bin/aws\n"},
		ssh.MockCommand{Match: "docker inspect -f '{{.Config.Image}}'", Output: "some/custom-thing:1\n"},
		ssh.MockCommand{Match: "docker inspect -f '{{range .Config.Env}}", Output: "\n"},
		ssh.MockCommand{Match: "aws s3 cp", Output: "done\n"},
		ssh.MockCommand{Match: "mkdir -p", Output: "12\n"},
		ssh.MockCommand{Match: "docker rm -f", Output: ""},
	)
	client := NewClient(mock, &bytes.Buffer{})
	res, err := client.VerifyBackup(context.Background(), "myapp", "thing", "20260710-040000", S3Config{Bucket: "b", Region: "us-east-1"})
	if err != nil {
		t.Fatalf("VerifyBackup: %v", err)
	}
	if !res.OK || res.Kind != "volume" || res.Metric != "files=12" {
		t.Fatalf("unexpected result: %+v", res)
	}
}
