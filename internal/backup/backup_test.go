package backup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy/internal/ssh"
)

func TestBackupVolumes(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "which aws", Output: "/usr/bin/aws\n"},
		ssh.MockCommand{Match: "umask 077", Output: "/tmp/teploy-backup.abc123\n"},
		ssh.MockCommand{Match: "if [ -f", Output: ""},
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

	// teploy-cli-09: the archive must add the app .env as a top-level
	// member from the app directory (restored beside volumes/), not as an
	// absolute host path (which tar stored under a stripped
	// deployments/<app>/ prefix that restore then buried inside volumes/).
	var tarCmd string
	for _, call := range mock.Calls {
		if strings.Contains(call, "tar -czf") {
			tarCmd = call
		}
	}
	foundS3 := false
	for _, call := range mock.Calls {
		if strings.HasPrefix(call, "aws s3 cp") {
			foundS3 = true
		}
	}
	if tarCmd == "" {
		t.Fatal("expected tar command")
	}
	if !foundS3 {
		t.Error("expected aws s3 cp command")
	}
	if !strings.Contains(tarCmd, "-C '/deployments/myapp/volumes' . -C '/deployments/myapp' .env") {
		t.Errorf("archive must carry .env as a top-level member via -C appdir, got: %s", tarCmd)
	}
	if strings.Contains(tarCmd, "'/deployments/myapp/.env' -C") ||
		strings.Contains(tarCmd, ".env 2>/dev/null") ||
		strings.Contains(tarCmd, "|| tar") {
		t.Errorf("archive command must not pass an absolute .env path or mask failures with a fallback tar: %s", tarCmd)
	}
}

func TestAccessoryBackup_Postgres(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "which aws", Output: "/usr/bin/aws\n"},
		ssh.MockCommand{Match: "umask 077", Output: "/tmp/teploy-backup.abc123\n"},
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
		ssh.MockCommand{Match: "umask 077", Output: "/tmp/teploy-backup.abc123\n"},
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
		ssh.MockCommand{Match: "umask 077", Output: "/tmp/teploy-backup.abc123\n"},
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
		ssh.MockCommand{Match: "umask 077", Output: "/tmp/teploy-backup.abc123\n"},
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

// teploy-cli-06: the redis backup assumed BGSAVE finishes within a fixed
// `sleep 2`; a slow snapshot copied the previous dump.rdb instead. The dump
// command must capture LASTSAVE before bgsave and poll until it changes.
func TestAccessoryBackup_RedisWaitsForBgsave(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "which aws", Output: "/usr/bin/aws\n"},
		ssh.MockCommand{Match: "umask 077", Output: "/tmp/teploy-backup.abc123\n"},
		ssh.MockCommand{Match: "set -eu", Output: ""},
		ssh.MockCommand{Match: "aws s3 cp", Output: "done\n"},
		ssh.MockCommand{Match: "rm -f", Output: ""},
	)

	var buf bytes.Buffer
	client := NewClient(mock, &buf)
	err := client.AccessoryBackup(context.Background(), "myapp", "redis", "redis:7", nil, S3Config{
		Bucket: "my-bucket",
		Region: "us-east-1",
	})
	if err != nil {
		t.Fatalf("AccessoryBackup: %v", err)
	}

	var dumpCmd string
	for _, call := range mock.Calls {
		if strings.Contains(call, "bgsave") {
			dumpCmd = call
		}
	}
	if dumpCmd == "" {
		t.Fatal("expected a redis dump command")
	}
	if i, j := strings.Index(dumpCmd, "lastsave"), strings.Index(dumpCmd, "bgsave"); i < 0 || j < 0 || i > j {
		t.Errorf("LASTSAVE must be captured before bgsave:\n%s", dumpCmd)
	}
	// audit F36: the poll must be fail-closed — a `saved` flag checked after
	// the loop, so exhaustion aborts instead of falling through to docker cp
	// with the previous dump.
	if !strings.Contains(dumpCmd, `[ "$cur" != "$ls" ]`) || !strings.Contains(dumpCmd, `"$i" -lt 60`) {
		t.Errorf("expected a bounded LASTSAVE poll loop before docker cp:\n%s", dumpCmd)
	}
	if !strings.Contains(dumpCmd, `[ "$saved" != yes ]`) {
		t.Errorf("poll exhaustion must abort the backup (saved-flag check missing):\n%s", dumpCmd)
	}
	if strings.Contains(dumpCmd, "sleep 2") {
		t.Errorf("fixed sleep assumes bgsave finishes in 2s:\n%s", dumpCmd)
	}
}

// teploy-cli-13: mysqldump/mysql ran as `-u root` with no credentials, so
// any accessory that sets a root password failed (or dumped nothing). The
// password must come from the app env (MYSQL_ROOT_PASSWORD, falling back to
// MYSQL_PASSWORD) and reach the container as MYSQL_PWD env — never argv,
// which `ps` inside the container would show.
func TestAccessoryBackup_MySQL_UsesRootPasswordEnv(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "which aws", Output: "/usr/bin/aws\n"},
		ssh.MockCommand{Match: "umask 077", Output: "/tmp/teploy-backup.abc123\n"},
		ssh.MockCommand{Match: "docker exec", Output: ""},
		ssh.MockCommand{Match: "aws s3 cp", Output: "done\n"},
		ssh.MockCommand{Match: "rm -f", Output: ""},
	)

	var buf bytes.Buffer
	client := NewClient(mock, &buf)
	env := map[string]string{"MYSQL_ROOT_PASSWORD": "sekret"}
	err := client.AccessoryBackup(context.Background(), "myapp", "mysql", "mysql:8", env, S3Config{
		Bucket: "my-bucket",
		Region: "us-east-1",
	})
	if err != nil {
		t.Fatalf("AccessoryBackup: %v", err)
	}

	var dumpCmd string
	for _, call := range mock.Calls {
		if strings.Contains(call, "mysqldump") {
			dumpCmd = call
		}
	}
	if dumpCmd == "" {
		t.Fatal("expected a mysqldump command")
	}
	if !strings.Contains(dumpCmd, "docker exec -e MYSQL_PWD='sekret' 'myapp-mysql' mysqldump -u root 'myapp'") {
		t.Errorf("password must ride as MYSQL_PWD env on docker exec, got: %s", dumpCmd)
	}
	if dump := dumpCmd[strings.Index(dumpCmd, "mysqldump"):]; strings.Contains(dump, "sekret") {
		t.Errorf("password must not appear on the mysqldump argv: %s", dumpCmd)
	}
}

func TestAccessoryBackup_MySQL_PasswordFallbackAndAbsence(t *testing.T) {
	var buf bytes.Buffer

	// MYSQL_PASSWORD is the fallback when MYSQL_ROOT_PASSWORD is unset.
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "which aws", Output: "/usr/bin/aws\n"},
		ssh.MockCommand{Match: "umask 077", Output: "/tmp/teploy-backup.abc123\n"},
		ssh.MockCommand{Match: "docker exec", Output: ""},
		ssh.MockCommand{Match: "aws s3 cp", Output: "done\n"},
		ssh.MockCommand{Match: "rm -f", Output: ""},
	)
	client := NewClient(mock, &buf)
	err := client.AccessoryBackup(context.Background(), "myapp", "mysql", "mysql:8",
		map[string]string{"MYSQL_PASSWORD": "fall"}, S3Config{Bucket: "my-bucket", Region: "us-east-1"})
	if err != nil {
		t.Fatalf("AccessoryBackup: %v", err)
	}
	found := false
	for _, call := range mock.Calls {
		if strings.Contains(call, "-e MYSQL_PWD='fall'") {
			found = true
		}
	}
	if !found {
		t.Errorf("MYSQL_PASSWORD must be used when MYSQL_ROOT_PASSWORD is absent, calls: %v", mock.Calls)
	}

	// No password configured: keep the bare command (passwordless root).
	mock = ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "which aws", Output: "/usr/bin/aws\n"},
		ssh.MockCommand{Match: "umask 077", Output: "/tmp/teploy-backup.abc123\n"},
		ssh.MockCommand{Match: "docker exec", Output: ""},
		ssh.MockCommand{Match: "aws s3 cp", Output: "done\n"},
		ssh.MockCommand{Match: "rm -f", Output: ""},
	)
	client = NewClient(mock, &buf)
	err = client.AccessoryBackup(context.Background(), "myapp", "mysql", "mysql:8", nil,
		S3Config{Bucket: "my-bucket", Region: "us-east-1"})
	if err != nil {
		t.Fatalf("AccessoryBackup: %v", err)
	}
	for _, call := range mock.Calls {
		if strings.Contains(call, "MYSQL_PWD") {
			t.Errorf("no password configured, so no MYSQL_PWD expected: %s", call)
		}
	}
}

// The password lands inside a shell command string: it must be single-quote
// wrapped (ShellQuote), so values with spaces or quotes neither break the
// command nor escape into something executable.
func TestAccessoryBackup_MySQL_QuotesHostilePassword(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "which aws", Output: "/usr/bin/aws\n"},
		ssh.MockCommand{Match: "umask 077", Output: "/tmp/teploy-backup.abc123\n"},
		ssh.MockCommand{Match: "docker exec", Output: ""},
		ssh.MockCommand{Match: "aws s3 cp", Output: "done\n"},
		ssh.MockCommand{Match: "rm -f", Output: ""},
	)

	var buf bytes.Buffer
	client := NewClient(mock, &buf)
	env := map[string]string{"MYSQL_ROOT_PASSWORD": "p@'ss word; id"}
	err := client.AccessoryBackup(context.Background(), "myapp", "mysql", "mysql:8", env, S3Config{
		Bucket: "my-bucket",
		Region: "us-east-1",
	})
	if err != nil {
		t.Fatalf("AccessoryBackup: %v", err)
	}

	var dumpCmd string
	for _, call := range mock.Calls {
		if strings.Contains(call, "mysqldump") {
			dumpCmd = call
		}
	}
	if !strings.Contains(dumpCmd, "-e MYSQL_PWD='p@'\"'\"'ss word; id'") {
		t.Errorf("password must be ShellQuote-wrapped, got: %s", dumpCmd)
	}
}

func TestAccessoryRestore_MySQL_UsesRootPasswordEnv(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "which aws", Output: "/usr/bin/aws\n"},
		ssh.MockCommand{Match: "aws s3 cp", Output: "download: done\n"},
		ssh.MockCommand{Match: "mktemp -d '/tmp/teploy-restore.XXXXXX'", Output: "/tmp/teploy-restore.abc123\n"},
		ssh.MockCommand{Match: "gunzip -c", Output: ""},
		ssh.MockCommand{Match: "rm -rf", Output: ""},
	)

	var buf bytes.Buffer
	client := NewClient(mock, &buf)
	env := map[string]string{"MYSQL_ROOT_PASSWORD": "sekret"}
	err := client.AccessoryRestore(context.Background(), "myapp", "mysql", "mysql:8",
		"20260101-000000", env, S3Config{Bucket: "my-bucket", Region: "us-east-1"})
	if err != nil {
		t.Fatalf("AccessoryRestore: %v", err)
	}

	var restoreCmd string
	for _, call := range mock.Calls {
		if strings.Contains(call, "mysql -u root") {
			restoreCmd = call
		}
	}
	if restoreCmd == "" {
		t.Fatal("expected a mysql restore command")
	}
	if !strings.Contains(restoreCmd, "docker exec -i -e MYSQL_PWD='sekret' 'myapp-mysql' mysql -u root 'myapp'") {
		t.Errorf("password must ride as MYSQL_PWD env on docker exec, got: %s", restoreCmd)
	}
	if mysql := restoreCmd[strings.Index(restoreCmd, "mysql -u root"):]; strings.Contains(mysql, "sekret") {
		t.Errorf("password must not appear on the mysql argv: %s", restoreCmd)
	}
}

// teploy-cli-04: the restore pipelines (`gunzip -c X | docker exec -i ...
// psql ...`) only reported the LAST command's exit status — psql without
// ON_ERROR_STOP exits 0 on SQL errors, and a failed gunzip alone fed the
// container an empty stdin and still "succeeded". The restore command must
// check gunzip's exit code itself and make psql stop on SQL errors.
func TestAccessoryRestore_PostgresFailsOnErrors(t *testing.T) {
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

	var restoreCmd string
	for _, call := range mock.Calls {
		if strings.Contains(call, "psql") {
			restoreCmd = call
		}
	}
	if restoreCmd == "" {
		t.Fatal("expected a psql restore command")
	}
	if !strings.Contains(restoreCmd, "-v ON_ERROR_STOP=1") {
		t.Errorf("psql must run with ON_ERROR_STOP so SQL errors fail the restore:\n%s", restoreCmd)
	}
	if strings.Contains(restoreCmd, "|") {
		t.Errorf("restore must not be a pipeline (last-command exit status masks gunzip failures):\n%s", restoreCmd)
	}
}

func TestAccessoryRestore_CorruptArchiveFails(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "which aws", Output: "/usr/bin/aws\n"},
		ssh.MockCommand{Match: "aws s3 cp", Output: "download: done\n"},
		ssh.MockCommand{Match: "mktemp -d '/tmp/teploy-restore.XXXXXX'", Output: "/tmp/teploy-restore.abc123\n"},
		ssh.MockCommand{Match: "gunzip -c", Err: errors.New("exit status 1: gzip: stdin: not in gzip format")},
	)

	var buf bytes.Buffer
	client := NewClient(mock, &buf)
	err := client.AccessoryRestore(context.Background(), "myapp", "postgres", "postgres:16",
		"20260101-000000", nil, S3Config{Bucket: "my-bucket", Region: "us-east-1"})
	if err == nil {
		t.Fatal("expected AccessoryRestore to fail when gunzip fails, not report success")
	}
	if !strings.Contains(err.Error(), "not in gzip format") {
		t.Errorf("expected the underlying gunzip error to surface, got: %v", err)
	}
}

func TestAccessoryBackup_Generic(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "which aws", Output: "/usr/bin/aws\n"},
		ssh.MockCommand{Match: "umask 077", Output: "/tmp/teploy-backup.abc123\n"},
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

// teploy-cli-10: cron treats the first unescaped % in a command as a
// newline/stdin marker, so the scheduled backup's `$(date +%Y%m%d-…)`
// truncated the job at the %. The installed line must escape the command's
// % signs while leaving the marker comment (and its grep -vF dedup) intact.
func TestSetSchedule_EscapesPercentForCron(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "raw=$(crontab -l", Output: ""},
	)

	var buf bytes.Buffer
	client := NewClient(mock, &buf)
	command := "tar -czf /tmp/app-backup-$(date +%Y%m%d-%H%M%S).tar.gz -C /deployments/app/volumes . && aws s3 cp /tmp/app-backup-*.tar.gz s3://b/app/volumes/"
	if err := client.SetSchedule(context.Background(), "0 3 * * *", command, "teploy-backup:myapp"); err != nil {
		t.Fatalf("SetSchedule: %v", err)
	}

	var installed string
	for _, call := range mock.Calls {
		if strings.Contains(call, "crontab -") {
			installed = call
		}
	}
	if installed == "" {
		t.Fatal("expected a crontab install command")
	}
	if !strings.Contains(installed, `$(date +\%Y\%m\%d-\%H\%M\%S)`) {
		t.Errorf("installed line must escape %% in the command portion:\n%s", installed)
	}
	if !strings.Contains(installed, "grep -vF '[teploy-backup:myapp]'") {
		t.Errorf("marker dedup must survive escaping:\n%s", installed)
	}
	if !strings.Contains(installed, `# [teploy-backup:myapp]'`) {
		t.Errorf("marker comment must stay intact:\n%s", installed)
	}
	// No raw, unescaped % may remain in the command portion of the line.
	if unquoted := strings.ReplaceAll(installed, `\%`, ""); strings.Count(unquoted, `%`) != 2 {
		// exactly two % left: the two printf formats themselves
		t.Errorf("line contains raw %% outside the printf formats:\n%s", installed)
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
		// A registry host's port colon is not a tag separator — the case
		// that used to collapse these to the registry hostname and pick
		// the generic tar branch for a real database (teploy-cli-08).
		{"registry.example:5000/postgres:16", "postgres", true},
		{"registry.example:5000/postgres", "postgres", true},
		{"registry.example:5000/namespace/mysql:8", "mysql", true},
		{"ghcr.io/registry:5000/redis:7", "redis", true},
		{"postgres@sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", "postgres", true},
		{"registry.example:5000/myapp:latest", "postgres", false},
		// Custom aliases do not name-match a known engine — inference
		// stays conservative by design.
		{"myorg/my-postgres:16", "postgres", false},
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

	invalid := []string{"0 3 * * *; rm -rf /", "$(whoami)", "0 3 * * * && cat /etc/passwd",
		// audit F39: a character-set-only validator accepted these.
		"* * * *", "0 3 * * * *", "99 99 * * *", "0 0 32 * *", "0 0 * 13 *", "*/0 * * * *"}
	for _, s := range invalid {
		if err := ValidateSchedule(s); err == nil {
			t.Errorf("ValidateSchedule(%q) = nil, want error", s)
		}
	}
}

var errNotFound = &notFoundError{}

type notFoundError struct{}

func (e *notFoundError) Error() string { return "not found" }

// TCL-46: descending ranges and wildcard range endpoints used to pass the
// character/bounds check and produce crontab entries cron never runs.
func TestValidateSchedule_RejectsMalformedRanges(t *testing.T) {
	for _, bad := range []string{
		"5-1 * * * *",  // descending
		"5-* * * * *",  // wildcard as range end
		"*-5 * * * *",  // wildcard as range start
		"0 22-3 * * *", // descending hours
		"0 0 * 12-2 *", // descending months
	} {
		if err := ValidateSchedule(bad); err == nil {
			t.Errorf("ValidateSchedule accepted malformed range %q", bad)
		}
	}
	for _, good := range []string{"*/5 * * * *", "0 3 * * 1-5", "30 8-18 * * *", "0 0 1 1 0"} {
		if err := ValidateSchedule(good); err != nil {
			t.Errorf("ValidateSchedule rejected valid schedule %q: %v", good, err)
		}
	}
}

// TCL-46: a crontab read failure other than the canonical "no crontab for
// <user>" must abort the install — the old pipeline masked it as empty
// input and deleted every unrelated job. The generated script is executed
// against a fake crontab binary so the failure happens where it really
// does: inside the shell.
func TestSetSchedule_CrontabReadFailureAborts(t *testing.T) {
	run := func(t *testing.T, readErrMsg string) (exitErr error, installed bool) {
		t.Helper()
		bin := t.TempDir()
		installMarker := filepath.Join(bin, "installed")
		crontab := fmt.Sprintf(`#!/bin/sh
marker=%s
case "$1" in
-l) echo %s >&2; exit 1 ;;
-) cat > /dev/null; touch "$marker"; exit 0 ;;
esac
`, ssh.ShellQuote(installMarker), ssh.ShellQuote(readErrMsg))
		if err := os.WriteFile(filepath.Join(bin, "crontab"), []byte(crontab), 0755); err != nil {
			t.Fatal(err)
		}

		mock := ssh.NewMockExecutor("1.2.3.4",
			ssh.MockCommand{Match: "raw=$(crontab -l", Err: errors.New("unused")},
		)
		var buf bytes.Buffer
		_ = NewClient(mock, &buf).SetSchedule(context.Background(), "0 3 * * *", "echo hi", "teploy-backup:myapp")
		if len(mock.Calls) == 0 {
			t.Fatal("no schedule command generated")
		}
		sh := exec.Command("sh", "-c", mock.Calls[0])
		sh.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
		exitErr = sh.Run()
		_, statErr := os.Stat(installMarker)
		installed = statErr == nil
		return exitErr, installed
	}

	if err, installed := run(t, "crontab: permission denied"); err == nil || installed {
		t.Errorf("real read failure: abort=%v installed=%v (want abort, no install)", err, installed)
	}
	if err, installed := run(t, "no crontab for root"); err != nil || !installed {
		t.Errorf("canonical no-crontab: abort=%v installed=%v (want clean first install)", err, installed)
	}
}

// TestNewBackupID_UniqueWithinSameSecond is the A44 regression: two ids
// minted in the same second differ, and both parse under ValidateDate.
func TestNewBackupID_UniqueWithinSameSecond(t *testing.T) {
	now := time.Date(2026, 9, 19, 1, 2, 3, 0, time.UTC)
	a, err := newBackupID(now)
	if err != nil {
		t.Fatal(err)
	}
	b, err := newBackupID(now)
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatalf("ids minted in the same second collided: %s", a)
	}
	for _, id := range []string{a, b} {
		if err := ValidateDate(id); err != nil {
			t.Errorf("ValidateDate(%q): %v", id, err)
		}
	}
	// Legacy ids remain valid (restore/list of old backups).
	if err := ValidateDate("20260101-000000"); err != nil {
		t.Errorf("legacy id rejected: %v", err)
	}
	for _, bad := range []string{"20261301-000000", "20260101-000000-extra", "20260101-000000-", "20260101-000000-ZZZZZZZZZZZZZZZZ"} {
		if err := ValidateDate(bad); err == nil {
			t.Errorf("ValidateDate(%q) accepted", bad)
		}
	}
}

// TestValidateSchedule_RejectsSignedValues is the A46 regression: cron
// fields are unsigned decimals — "+1" and negative steps are rejected.
func TestValidateSchedule_RejectsSignedValues(t *testing.T) {
	for _, s := range []string{"+1 * * * *", "*/+5 * * * *", "-1 * * * *"} {
		if err := ValidateSchedule(s); err == nil {
			t.Errorf("ValidateSchedule(%q) accepted a signed value", s)
		}
	}
}

// TestSetSchedule_EnforcesGrammarAtSink is the A46 regression: the sink
// itself validates the schedule and rejects line breaks in the command or
// marker — direct callers cannot bypass validation or split one job into
// several crontab lines.
func TestSetSchedule_EnforcesGrammarAtSink(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4")
	client := NewClient(mock, io.Discard)
	if err := client.SetSchedule(context.Background(), "99 * * * *", "cmd", "tag"); err == nil {
		t.Error("an out-of-range schedule must be rejected at the sink")
	}
	if err := client.SetSchedule(context.Background(), "* * * * *", "echo one\necho two", "tag"); err == nil {
		t.Error("a multi-line command must be rejected")
	}
	if err := client.SetSchedule(context.Background(), "* * * * *", "cmd", "tag\nother"); err == nil {
		t.Error("a multi-line marker must be rejected")
	}
	for _, c := range mock.Calls {
		if strings.Contains(c, "crontab -") {
			t.Errorf("a rejected schedule must not touch the crontab: %s", c)
		}
	}
}

// TestRedisRestore_AOFRequiresProvenNo is the A40 regression: an AOF
// preflight that errors (auth, transport) or replies unexpectedly must
// refuse BEFORE any stop/copy — "not proven yes" is not "proven no".
func TestRedisRestore_AOFRequiresProvenNo(t *testing.T) {
	for _, tc := range []struct {
		name, aofOut string
		aofErr       error
	}{
		{"auth failure", "", fmt.Errorf("NOAUTH Authentication required")},
		{"empty reply", "", nil},
		{"unexpected reply", "WRONGTYPE Operation against a key", nil},
		{"yes is refused", "appendonly yes", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mock := ssh.NewMockExecutor("1.2.3.4",
				ssh.MockCommand{Match: "which aws", Output: "/usr/bin/aws\n"},
				ssh.MockCommand{Match: "aws s3 cp", Output: "ok"},
				ssh.MockCommand{Match: "mktemp -d", Output: "/tmp/teploy-restore.abc\n"},
				ssh.MockCommand{Match: "gunzip -c", Output: ""},
				ssh.MockCommand{Match: "redis-cli --raw config get appendonly", Output: tc.aofOut, Err: tc.aofErr},
				ssh.MockCommand{Match: "docker stop", Output: ""},
				ssh.MockCommand{Match: "docker cp", Output: ""},
			)
			client := NewClient(mock, io.Discard)
			err := client.AccessoryRestore(context.Background(), "myapp", "cache", "redis:7", "20260101-000000", nil, S3Config{Bucket: "b", Region: "us-east-1"})
			if err == nil {
				t.Fatal("expected the restore to refuse")
			}
			for _, c := range mock.Calls {
				if strings.HasPrefix(c, "docker stop") {
					t.Errorf("an unproven AOF state must abort before stopping redis: %s", c)
				}
			}
		})
	}
}

// TestRedisRestore_SnapshotsAfterStopAndCompensatesStartFailure is the
// A41 regression: the restore script snapshots the original dump AFTER the
// stop (so the shutdown save is included) and defines a restore_original
// compensation invoked from BOTH failure branches — a failed install and a
// failed final docker start.
func TestRedisRestore_SnapshotsAfterStopAndCompensatesStartFailure(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "which aws", Output: "/usr/bin/aws\n"},
		ssh.MockCommand{Match: "aws s3 cp", Output: "ok"},
		ssh.MockCommand{Match: "mktemp -d", Output: "/tmp/teploy-restore.abc\n"},
		ssh.MockCommand{Match: "docker exec 'myapp-cache' redis-cli --raw config get appendonly", Output: "appendonly no"},
		ssh.MockCommand{Match: "set -eu", Output: ""},
	)
	client := NewClient(mock, io.Discard)
	if err := client.AccessoryRestore(context.Background(), "myapp", "cache", "redis:7", "20260101-000000", nil, S3Config{Bucket: "b", Region: "us-east-1"}); err != nil {
		t.Fatalf("AccessoryRestore: %v", err)
	}
	var script string
	for _, c := range mock.Calls {
		if strings.HasPrefix(c, "set -eu") {
			script = c
		}
	}
	if script == "" {
		t.Fatal("the redis restore script never ran")
	}
	stopIdx := strings.Index(script, "docker stop 'myapp-cache'")
	snapIdx := strings.Index(script, "docker cp 'myapp-cache':/data/dump.rdb")
	if stopIdx < 0 || snapIdx < 0 || snapIdx < stopIdx {
		t.Errorf("the original dump must be snapshotted AFTER the stop (stop=%d snapshot=%d)", stopIdx, snapIdx)
	}
	compIdx := strings.Index(script, "restore_original() {")
	if compIdx < 0 {
		t.Fatal("the script must define the restore_original compensation")
	}
	// Both failure branches invoke it.
	branches := strings.Count(script, "\trestore_original") + strings.Count(script, "  restore_original")
	if branches < 2 {
		t.Errorf("both the failed-install and failed-start branches must compensate (found %d): %q", branches, script)
	}
	if !strings.Contains(script, "if ! docker start 'myapp-cache'; then") {
		t.Error("a failed final docker start must be a handled branch")
	}
}
