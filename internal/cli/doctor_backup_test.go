package cli

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy/internal/ssh"
)

func doctorPolicyMock(policy, target string, tools string, cron, events string) *ssh.MockExecutor {
	return ssh.NewMockExecutor("h", doctorPolicyCommands(policy, target, tools, cron, events)...)
}

func doctorPolicyCommands(policy, target string, tools string, cron, events string) []ssh.MockCommand {
	return []ssh.MockCommand{
		{Match: "if [ -d /etc/teploy ]", Output: policy},
		{Match: "for p in /etc/teploy/backup-target.json", Output: target},
		{Match: "command -v aws >/dev/null", Output: tools},
		{Match: "crontab -l", Output: cron},
		{Match: "ids=$(docker ps -aq", Output: ""},
		{Match: "which aws", Output: "/usr/bin/aws\n"},
		{Match: "( c=$(python3", Output: ""},
		{Match: "aws s3 ls", Output: ""},
		{Match: "aws s3api get-bucket-lifecycle-configuration", Output: `{"Rules":[{"Status":"Enabled","Filter":{"Prefix":".incoming/"},"Expiration":{"Days":1}}]}`},
		{Match: "python3 -c", Output: events},
		{Match: "for d in /deployments/*/volumes", Output: ""},
		{Match: "for d in '/var/tmp'/teploy-backup.", Output: ""},
	}
}

func TestDoctorBackupPolicyCheck(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	policy := func(extra string) string {
		return "path /etc/teploy/backup-policy.json\n" + `{"v":"1","interval":"1h"` + extra + `}`
	}
	target := "path /etc/teploy/backup-target.json\n" + `{"v":"1","bucket":"b","region":"r"}`
	cron := "5 * * * * /usr/local/bin/teploy backup run --all # [teploy-backup-all]\n"
	good := `{"v":1,"id":"1","seq":1,"ts":"2026-11-04T11:30:00Z","app":"_fleet","kind":"backup.all","outcome":"succeeded","detail":{"status":"ok","targets_ok":2,"targets_failed":0}}` + "\n"
	stale := strings.ReplaceAll(good, "2026-11-04T11:30:00Z", "2026-11-01T11:30:00Z")
	full := `,"recipient":"` + testRecipient + `","heartbeat_urls":["https://d/x"]`
	for _, tc := range []struct {
		name, policy, target, tools, cron, events string
		result, want                              string
	}{
		{"no policy, nothing configured", "absent\n", "absent\n", "age\n", "", "", doctorOK, "no backup policy"},
		{"all good", policy(full), target, "aws\nage\n", cron, good, doctorOK, "encrypted"},
		{"no target", policy(full), "absent\n", "aws\nage\n", cron, good, doctorFail, "no target"},
		{"no aws", policy(full), target, "age\n", cron, good, doctorFail, "aws CLI is missing"},
		{"no recipient", policy(`,"heartbeat_urls":["https://d/x"]`), target, "aws\nage\n", cron, good, doctorWarn, "unencrypted"},
		{"recipient but no age", policy(full), target, "aws\n", cron, good, doctorFail, "age is not installed"},
		{"no cron line", policy(full), target, "aws\nage\n", "", good, doctorFail, "cron line is not installed"},
		{"stale", policy(full), target, "aws\nage\n", cron, stale, doctorFail, "past twice"},
		{"never ran", policy(full), target, "aws\nage\n", cron, "", doctorWarn, "never run"},
		{"no heartbeat", policy(`,"recipient":"` + testRecipient + `"`), target, "aws\nage\n", cron, good, doctorWarn, "no heartbeat"},
		{"target only, no aws", "absent\n", target, "", "", "", doctorFail, "aws CLI is missing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := doctorBackupPolicyCheck(context.Background(), doctorPolicyMock(tc.policy, tc.target, tc.tools, tc.cron, tc.events), now)
			if c.Result != tc.result || !strings.Contains(c.Detail, tc.want) {
				t.Fatalf("%+v", c)
			}
			if c.Result != doctorOK && c.Remediation == "" {
				t.Fatalf("no remedy: %+v", c)
			}
		})
	}
	// The aws remedy is exact.
	c := doctorBackupPolicyCheck(context.Background(), doctorPolicyMock(policy(full), target, "age\n", cron, good), now)
	if !strings.Contains(c.Remediation, "AWS's installer") || !strings.Contains(c.Remediation, "teploy setup") {
		t.Fatalf("%s", c.Remediation)
	}
}

// No prefix in a bucket another server writes to: same-named apps would collide,
// and a server that has not upgraded carries no marker to refuse on.
func TestDoctorFlagsNoPrefixInASharedBucket(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	mk := func(prefix string) *ssh.MockExecutor {
		return ssh.NewMockExecutor("h",
			ssh.MockCommand{Match: "if [ -d /etc/teploy ]", Output: "path /etc/teploy/backup-policy.json\n" + `{"v":"1","interval":"1h","recipient":"` + testRecipient + `","heartbeat_urls":["https://d/x"]}`},
			ssh.MockCommand{Match: "for p in /etc/teploy/backup-target.json", Output: "path /etc/teploy/backup-target.json\n" + `{"v":"1","bucket":"b","region":"r","prefix":"` + prefix + `"}`},
			ssh.MockCommand{Match: "command -v aws >/dev/null", Output: "aws\nage\n"},
			ssh.MockCommand{Match: "crontab -l", Output: "5 * * * * /usr/local/bin/teploy backup run --all # [teploy-backup-all]\n"},
			ssh.MockCommand{Match: "ids=$(docker ps -aq", Output: ""},
			ssh.MockCommand{Match: "{ cat /etc/machine-id", Output: "0123456789abcdef0123456789abcdef  -\n"},
			ssh.MockCommand{Match: "aws s3 ls", Output: "d t 20 shop/.owners/aaaaaaaaaaaaaaaa\nd t 20 shop/.owners/0123456789abcdef\n"},
			ssh.MockCommand{Match: "python3 -c", Output: `{"v":1,"id":"1","seq":1,"ts":"2026-11-04T11:30:00Z","app":"_fleet","kind":"backup.all","outcome":"succeeded","detail":{"status":"ok"}}` + "\n"},
			ssh.MockCommand{Match: "for d in /deployments/*/volumes", Output: ""},
		)
	}
	c := doctorBackupPolicyCheck(context.Background(), mk(""), now)
	if c.Result != doctorWarn || !strings.Contains(c.Detail, "aaaaaaaaaaaaaaaa") || !strings.Contains(c.Remediation, "--prefix") || strings.Contains(c.Detail, "0123456789abcdef") {
		t.Fatalf("%+v", c)
	}
	// A prefix is the answer: not even looked for.
	if c := doctorBackupPolicyCheck(context.Background(), mk("srv1"), now); strings.Contains(c.Detail, "no prefix") {
		t.Fatalf("%+v", c)
	}
}

// On a store that keeps versions, nothing else stops pending uploads billing forever.
func TestDoctorChecksTheIncomingLifecycle(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	policy := "path /etc/teploy/backup-policy.json\n" + `{"v":"1","interval":"1h","recipient":"` + testRecipient + `","heartbeat_urls":["https://d/x"]}`
	target := "path /etc/teploy/backup-target.json\n" + `{"v":"1","bucket":"b","region":"r","prefix":"srv1"}`
	cron := "5 * * * * /usr/local/bin/teploy backup run --all # [teploy-backup-all]\n"
	good := `{"v":1,"id":"1","seq":1,"ts":"2026-11-04T11:30:00Z","app":"_fleet","kind":"backup.all","outcome":"succeeded","detail":{"status":"ok","targets_ok":2,"targets_failed":0}}` + "\n"
	mk := func(lifecycle ssh.MockCommand) *ssh.MockExecutor {
		return ssh.NewMockExecutor("h", append([]ssh.MockCommand{lifecycle}, doctorPolicyCommands(policy, target, "aws\nage\n", cron, good)...)...)
	}
	const op = "aws s3api get-bucket-lifecycle-configuration"
	c := doctorBackupPolicyCheck(context.Background(), mk(ssh.MockCommand{Match: op, Err: errors.New("exit status 254: An error occurred (NoSuchLifecycleConfiguration)")}), now)
	if c.Result != doctorWarn || !strings.Contains(c.Detail, "no lifecycle rule expires srv1/.incoming/") || !strings.Contains(c.Remediation, "after 1 day") {
		t.Fatalf("%+v", c)
	}
	c = doctorBackupPolicyCheck(context.Background(), mk(ssh.MockCommand{Match: op, Err: errors.New("exit status 254: AccessDenied")}), now)
	if c.Result != doctorWarn || !strings.Contains(c.Detail, "could not read the bucket's lifecycle rules") {
		t.Fatalf("%+v", c)
	}
	c = doctorBackupPolicyCheck(context.Background(), mk(ssh.MockCommand{Match: op, Output: `{"Rules":[{"Status":"Enabled","Filter":{"Prefix":"srv1/.incoming/"},"Expiration":{"Days":1}}]}`}), now)
	if c.Result != doctorOK {
		t.Fatalf("%+v", c)
	}
}

// An aws CLI too old to upload to a locked bucket fails every backup.
func TestDoctorFailsAnOldAWSCLI(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	policy := "path /etc/teploy/backup-policy.json\n" + `{"v":"1","interval":"1h","recipient":"` + testRecipient + `","heartbeat_urls":["https://d/x"]}`
	target := "path /etc/teploy/backup-target.json\n" + `{"v":"1","bucket":"b","region":"r"}`
	c := doctorBackupPolicyCheck(context.Background(), doctorPolicyMock(policy, target, "aws aws-cli/2.9.19 Python/3.11\nage\n", "", ""), now)
	if c.Result != doctorFail || !strings.Contains(c.Detail, "older than") || !strings.Contains(c.Remediation, "teploy setup") {
		t.Fatalf("%+v", c)
	}
}
