package backup

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/ssh"
)

func TestBackupVolumes(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "which aws", Output: "/usr/bin/aws\n"},
		ssh.MockCommand{Match: "tar -czf", Output: ""},
		ssh.MockCommand{Match: "aws s3 cp", Output: "upload: done\n"},
		ssh.MockCommand{Match: "rm -f", Output: ""},
	)

	var buf bytes.Buffer
	client := NewClient(mock, &buf)
	err := client.BackupVolumes(context.Background(), "myapp", S3Config{
		Bucket: "my-bucket",
		Region: "us-east-1",
	})
	if err != nil {
		t.Fatalf("BackupVolumes: %v", err)
	}

	if !strings.Contains(buf.String(), "Archiving volumes") {
		t.Error("expected archiving message")
	}
	if !strings.Contains(buf.String(), "Backup complete") {
		t.Error("expected completion message")
	}

	// Verify tar and aws commands were called.
	foundTar := false
	foundS3 := false
	for _, call := range mock.Calls {
		if strings.HasPrefix(call, "tar") {
			foundTar = true
		}
		if strings.HasPrefix(call, "aws s3 cp") {
			foundS3 = true
		}
	}
	if !foundTar {
		t.Error("expected tar command")
	}
	if !foundS3 {
		t.Error("expected aws s3 cp command")
	}
}

func TestAccessoryBackup_Postgres(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "which aws", Output: "/usr/bin/aws\n"},
		ssh.MockCommand{Match: "docker exec", Output: ""},
		ssh.MockCommand{Match: "aws s3 cp", Output: "done\n"},
		ssh.MockCommand{Match: "rm -f", Output: ""},
	)

	var buf bytes.Buffer
	client := NewClient(mock, &buf)
	err := client.AccessoryBackup(context.Background(), "myapp", "postgres", "postgres:16", nil, S3Config{
		Bucket: "my-bucket",
		Region: "us-east-1",
	})
	if err != nil {
		t.Fatalf("AccessoryBackup: %v", err)
	}

	// Verify pg_dump was used, targeting the app name (no POSTGRES_DB set,
	// matches connectionEnvVars' own app-name fallback).
	foundPgDump := false
	for _, call := range mock.Calls {
		if strings.Contains(call, "pg_dump") {
			foundPgDump = true
			if !strings.Contains(call, "-U 'postgres' 'myapp'") && !strings.Contains(call, "-U postgres myapp") {
				t.Errorf("expected pg_dump to target db 'myapp' (app-name fallback), got: %s", call)
			}
		}
	}
	if !foundPgDump {
		t.Error("expected pg_dump command for postgres")
	}
}

// TestAccessoryBackup_Postgres_CustomDBName reproduces a real, confirmed
// silent-data-loss bug found live: AccessoryBackup hardcoded db := app
// unconditionally, ignoring a custom POSTGRES_DB — connectionEnvVars (the
// app's own DATABASE_URL builder, internal/accessories/accessories.go)
// already correctly falls back to POSTGRES_DB with an app-name default,
// but the backup/restore path never applied that same resolution. Live,
// this produced a 20-byte gzip of nothing (pg_dump erroring "database
// \"myapp\" does not exist" to stderr) while the old `pg_dump | gzip`
// pipeline still reported "Backup complete" — see the exit-code-masking
// test below for that half of the bug.
func TestAccessoryBackup_Postgres_CustomDBName(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "which aws", Output: "/usr/bin/aws\n"},
		ssh.MockCommand{Match: "docker exec", Output: ""},
		ssh.MockCommand{Match: "aws s3 cp", Output: "done\n"},
		ssh.MockCommand{Match: "rm -f", Output: ""},
	)

	var buf bytes.Buffer
	client := NewClient(mock, &buf)
	env := map[string]string{"POSTGRES_DB": "realdbname", "POSTGRES_USER": "customuser"}
	err := client.AccessoryBackup(context.Background(), "myapp", "postgres", "postgres:16", env, S3Config{
		Bucket: "my-bucket",
		Region: "us-east-1",
	})
	if err != nil {
		t.Fatalf("AccessoryBackup: %v", err)
	}

	var pgDumpCall string
	for _, call := range mock.Calls {
		if strings.Contains(call, "pg_dump") {
			pgDumpCall = call
		}
	}
	if pgDumpCall == "" {
		t.Fatal("expected a pg_dump command")
	}
	// The container name and temp file paths legitimately contain "myapp"
	// (the app name) — check the actual pg_dump invocation (-U <user>
	// <db>) targets the configured POSTGRES_DB/POSTGRES_USER specifically,
	// not that "myapp" is absent from the whole command line.
	if !strings.Contains(pgDumpCall, "pg_dump -U 'customuser' 'realdbname'") {
		t.Errorf("expected pg_dump invocation to be `-U 'customuser' 'realdbname'`, got: %s", pgDumpCall)
	}
}

// TestAccessoryBackup_Postgres_DumpFailureIsNotSwallowed reproduces the
// other half of the silent-data-loss bug: the original `pg_dump | gzip >
// path` pipeline's exit status was gzip's (which succeeds compressing an
// empty stream even when pg_dump errors to stderr), so a real dump failure
// never surfaced as an error — "Backup complete" was reported for a
// 20-byte gzip of nothing. The fixed command redirects pg_dump's own
// output with `>` (not a pipe) so its actual exit code propagates.
func TestAccessoryBackup_Postgres_DumpFailureIsNotSwallowed(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "which aws", Output: "/usr/bin/aws\n"},
		ssh.MockCommand{Match: "docker exec", Err: errors.New(`exit status 1: pg_dump: error: connection to database "myapp" failed: FATAL:  database "myapp" does not exist`)},
	)

	var buf bytes.Buffer
	client := NewClient(mock, &buf)
	err := client.AccessoryBackup(context.Background(), "myapp", "postgres", "postgres:16", nil, S3Config{
		Bucket: "my-bucket",
		Region: "us-east-1",
	})
	if err == nil {
		t.Fatal("expected AccessoryBackup to fail when pg_dump fails, not report success")
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("expected the underlying pg_dump error to surface, got: %v", err)
	}
	if strings.Contains(buf.String(), "Backup complete") {
		t.Error("must not print success when the dump itself failed")
	}
}

func TestAccessoryBackup_MySQL(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "which aws", Output: "/usr/bin/aws\n"},
		ssh.MockCommand{Match: "docker exec", Output: ""},
		ssh.MockCommand{Match: "aws s3 cp", Output: "done\n"},
		ssh.MockCommand{Match: "rm -f", Output: ""},
	)

	var buf bytes.Buffer
	client := NewClient(mock, &buf)
	err := client.AccessoryBackup(context.Background(), "myapp", "mysql", "mysql:8", nil, S3Config{
		Bucket: "my-bucket",
		Region: "us-east-1",
	})
	if err != nil {
		t.Fatalf("AccessoryBackup: %v", err)
	}

	foundMysqlDump := false
	for _, call := range mock.Calls {
		if strings.Contains(call, "mysqldump") {
			foundMysqlDump = true
		}
	}
	if !foundMysqlDump {
		t.Error("expected mysqldump command for mysql")
	}
}

func TestAccessoryBackup_Generic(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "which aws", Output: "/usr/bin/aws\n"},
		ssh.MockCommand{Match: "tar -czf", Output: ""},
		ssh.MockCommand{Match: "aws s3 cp", Output: "done\n"},
		ssh.MockCommand{Match: "rm -f", Output: ""},
	)

	var buf bytes.Buffer
	client := NewClient(mock, &buf)
	err := client.AccessoryBackup(context.Background(), "myapp", "meilisearch", "meilisearch:latest", nil, S3Config{
		Bucket: "my-bucket",
		Region: "us-east-1",
	})
	if err != nil {
		t.Fatalf("AccessoryBackup: %v", err)
	}

	// Generic type should use tar.
	foundTar := false
	for _, call := range mock.Calls {
		if strings.HasPrefix(call, "tar") {
			foundTar = true
		}
	}
	if !foundTar {
		t.Error("expected tar command for generic accessory")
	}
}

func TestListBackups(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "which aws", Output: "/usr/bin/aws\n"},
		ssh.MockCommand{Match: "aws s3 ls", Output: "2026-03-11 00:00:00 1024 20260311-000000.tar.gz\n2026-03-10 12:00:00 2048 20260310-120000.tar.gz\n"},
	)

	var buf bytes.Buffer
	client := NewClient(mock, &buf)
	backups, err := client.ListBackups(context.Background(), "myapp", "volumes", S3Config{
		Bucket: "my-bucket",
		Region: "us-east-1",
	})
	if err != nil {
		t.Fatalf("ListBackups: %v", err)
	}
	if len(backups) != 2 {
		t.Fatalf("expected 2 backups, got %d", len(backups))
	}
}

func TestEnsureAWSCLI_Missing(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "which aws", Err: errNotFound},
	)

	var buf bytes.Buffer
	client := NewClient(mock, &buf)
	err := client.BackupVolumes(context.Background(), "myapp", S3Config{
		Bucket: "my-bucket",
		Region: "us-east-1",
	})
	if err == nil {
		t.Fatal("expected error when aws CLI missing")
	}
	if !strings.Contains(err.Error(), "aws CLI not found") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestIsDBType(t *testing.T) {
	tests := []struct {
		image, dbType string
		want          bool
	}{
		{"postgres:16", "postgres", true},
		{"mysql:8", "mysql", true},
		{"redis:7", "redis", true},
		{"mongo:latest", "mongo", true},
		{"library/postgres:16", "postgres", true},
		{"myapp:latest", "postgres", false},
	}

	for _, tt := range tests {
		if got := isDBType(tt.image, tt.dbType); got != tt.want {
			t.Errorf("isDBType(%q, %q) = %v, want %v", tt.image, tt.dbType, got, tt.want)
		}
	}
}

func TestValidateBucket(t *testing.T) {
	valid := []string{"my-bucket", "prod.backups", "my-app-2024", "a"}
	for _, b := range valid {
		if err := ValidateBucket(b); err != nil {
			t.Errorf("ValidateBucket(%q) = %v, want nil", b, err)
		}
	}

	invalid := []string{"", "my bucket", "bucket;rm -rf /", "bucket$(whoami)", "a/b"}
	for _, b := range invalid {
		if err := ValidateBucket(b); err == nil {
			t.Errorf("ValidateBucket(%q) = nil, want error", b)
		}
	}
}

func TestValidateRegion(t *testing.T) {
	valid := []string{"us-east-1", "eu-west-2", "ap-southeast-1"}
	for _, r := range valid {
		if err := ValidateRegion(r); err != nil {
			t.Errorf("ValidateRegion(%q) = %v, want nil", r, err)
		}
	}

	invalid := []string{"us east 1", "region;cmd", ""}
	for _, r := range invalid {
		if err := ValidateRegion(r); err == nil {
			t.Errorf("ValidateRegion(%q) = nil, want error", r)
		}
	}
}

func TestValidateDate(t *testing.T) {
	valid := []string{"20260312-150405", "20260101-000000"}
	for _, d := range valid {
		if err := ValidateDate(d); err != nil {
			t.Errorf("ValidateDate(%q) = %v, want nil", d, err)
		}
	}

	invalid := []string{"", "../../../etc/passwd", "date;rm -rf /"}
	for _, d := range invalid {
		if err := ValidateDate(d); err == nil {
			t.Errorf("ValidateDate(%q) = nil, want error", d)
		}
	}
}

func TestValidateSchedule(t *testing.T) {
	valid := []string{"0 3 * * *", "*/5 * * * *", "0 0 1 * *"}
	for _, s := range valid {
		if err := ValidateSchedule(s); err != nil {
			t.Errorf("ValidateSchedule(%q) = %v, want nil", s, err)
		}
	}

	invalid := []string{"0 3 * * *; rm -rf /", "$(whoami)", "0 3 * * * && cat /etc/passwd"}
	for _, s := range invalid {
		if err := ValidateSchedule(s); err == nil {
			t.Errorf("ValidateSchedule(%q) = nil, want error", s)
		}
	}
}

var errNotFound = &notFoundError{}

type notFoundError struct{}

func (e *notFoundError) Error() string { return "not found" }
