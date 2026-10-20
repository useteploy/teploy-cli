package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/backup"
	"github.com/useteploy/teploy/internal/ssh"
)

// Target round-trip: what WriteTarget sends is what ReadTarget parses, and
// the creds flag survives the framing. The mock answers the reader script
// from the payload the writer recorded on stdin.
func TestBackupTargetRoundTrip(t *testing.T) {
	mock := ssh.NewMockExecutor("srv",
		ssh.MockCommand{Match: "d=\n", Output: "written /etc/teploy/backup-target.json"},
	)
	ctx := context.Background()
	path, fallback, err := backup.WriteTarget(ctx, mock, backup.Target{
		Bucket: "b", Region: "us-east-1", Endpoint: "http://127.0.0.1:9100", Prefix: "prod",
	}, "AGE ENCRYPTED")
	if err != nil {
		t.Fatalf("WriteTarget: %v", err)
	}
	if path != "/etc/teploy/backup-target.json" || fallback {
		t.Fatalf("path=%s fallback=%v", path, fallback)
	}
	payload := mock.Inputs[len(mock.Inputs)-1]
	for _, want := range []string{`"bucket":"b"`, `"region":"us-east-1"`, `"endpoint":"http://127.0.0.1:9100"`, `"prefix":"prod"`, `"credentials":"AGE ENCRYPTED"`} {
		if !strings.Contains(payload, want) {
			t.Errorf("stored JSON missing %s: %s", want, payload)
		}
	}

	reader := ssh.NewMockExecutor("srv",
		ssh.MockCommand{Match: "for p in /etc/teploy/backup-target.json", Output: "path /etc/teploy/backup-target.json\n" + payload},
	)
	target, err := backup.ReadTarget(ctx, reader)
	if err != nil {
		t.Fatalf("ReadTarget: %v", err)
	}
	if !TargetConfigured(target) || target.Bucket != "b" || target.Region != "us-east-1" ||
		target.Endpoint != "http://127.0.0.1:9100" || target.Prefix != "prod" || !target.HasCredentials {
		t.Fatalf("round-trip mismatch: %+v", target)
	}
	if target.Path != "/etc/teploy/backup-target.json" {
		t.Fatalf("path not reported: %+v", target)
	}
}

// An absent target reads as unconfigured, never as an error.
func TestBackupTargetAbsent(t *testing.T) {
	mock := ssh.NewMockExecutor("srv",
		ssh.MockCommand{Match: "for p in /etc/teploy/backup-target.json", Output: "absent"},
	)
	target, err := backup.ReadTarget(context.Background(), mock)
	if err != nil {
		t.Fatalf("ReadTarget: %v", err)
	}
	if TargetConfigured(target) {
		t.Fatalf("absent target read as configured: %+v", target)
	}
}

// S3Config.AWS with a stored target decrypts inside the invocation and
// fails closed: the aws command only runs after a non-empty decryption.
func TestS3ConfigAWSStoredTarget(t *testing.T) {
	cfg := TargetS3(backup.Target{
		Bucket: "b", Region: "us-east-1", Endpoint: "http://127.0.0.1:9100",
		Path: "/etc/teploy/backup-target.json", HasCredentials: true,
	})
	cmd := cfg.AWS("s3 ls s3://b/")
	for _, want := range []string{
		"python3 -c 'import json,sys; sys.stdout.write(json.load(open(sys.argv[1])).get(\"credentials\") or \"\")' '/etc/teploy/backup-target.json'",
		"age -d -i",
		`[ -n "$c" ]`,
		`eval "$c"`,
		"exec aws s3 ls s3://b/ --region 'us-east-1' --endpoint-url 'http://127.0.0.1:9100'",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("stored-creds invocation missing %q:\n%s", want, cmd)
		}
	}
	if !strings.HasPrefix(cmd, "( ") || !strings.HasSuffix(cmd, " )") {
		t.Errorf("stored-creds invocation must be a self-contained subshell:\n%s", cmd)
	}
	// No plaintext credential material anywhere.
	if strings.Contains(cmd, "AWS_ACCESS_KEY_ID='") || strings.Contains(cmd, "AWS_SECRET_ACCESS_KEY='") {
		t.Errorf("stored-creds invocation must not inline credential values:\n%s", cmd)
	}
}

// The prefix reaches every key the CLI builds.
func TestS3ConfigBase(t *testing.T) {
	cfg := backup.S3Config{Bucket: "b", Prefix: "prod"}
	if cfg.Base() != "b/prod" {
		t.Fatalf("Base = %q", cfg.Base())
	}
	bare := backup.S3Config{Bucket: "b"}
	if bare.Base() != "b" {
		t.Fatalf("bare bucket Base changed")
	}
}

// Credential validation refuses anything the run-time eval could misuse.
func TestValidateCredential(t *testing.T) {
	for _, ok := range []string{"AKIA1234", "a+b/c=d_e:", "x" + strings.Repeat("y", 200)} {
		if err := backup.ValidateCredential("access key", ok); err != nil {
			t.Errorf("valid credential rejected: %q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "with space", "semi;colon", "$(pwned)", "'quoted'", "new\nline", strings.Repeat("x", 256)} {
		if err := backup.ValidateCredential("secret key", bad); err == nil {
			t.Errorf("dangerous credential accepted: %q", bad)
		}
	}
}

func TestValidateEndpointAndPrefix(t *testing.T) {
	for _, ok := range []string{"http://127.0.0.1:9100", "https://s3.example.com", "https://s3.example.com:8443/path"} {
		if err := backup.ValidateEndpoint(ok); err != nil {
			t.Errorf("endpoint %q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"ftp://x", "http://host with space", "https://x'quote", "notaurl"} {
		if err := backup.ValidateEndpoint(bad); err == nil {
			t.Errorf("endpoint %q accepted", bad)
		}
	}
	for _, ok := range []string{"", "prod", "prod/apps", "a.b-c_d"} {
		if err := backup.ValidatePrefix(ok); err != nil {
			t.Errorf("prefix %q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"/lead", "trail/", "a//b", "..", "a/../b"} {
		if err := backup.ValidatePrefix(bad); err == nil {
			t.Errorf("prefix %q accepted", bad)
		}
	}
}

// `backup run` without a stored target fails with the remedy, before any
// docker or aws work.
func TestBackupRunNeedsTarget(t *testing.T) {
	mock := ssh.NewMockExecutor("localhost",
		ssh.MockCommand{Match: "for p in /etc/teploy/backup-target.json", Output: "absent"},
	)
	err := runBackupRunOn(context.Background(), mock, "myapp", "")
	if err == nil || !strings.Contains(err.Error(), "no backup target") {
		t.Fatalf("err = %v, want the no-target refusal", err)
	}
	for _, call := range mock.Calls {
		if strings.Contains(call, "docker") || strings.Contains(call, "aws s3") {
			t.Errorf("no docker/aws work may run without a target: %s", call)
		}
	}
}
