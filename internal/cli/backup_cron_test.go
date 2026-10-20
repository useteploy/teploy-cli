package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/backup"
	"github.com/useteploy/teploy/internal/ssh"
)

const testCrontab = `0 4 * * 0 /deployments/forgejo/scheduled-redeploy.sh >> /deployments/forgejo/scheduled-redeploy.log 2>&1
30 4 * * * /usr/local/bin/teploy-nucleus-backup ship # [teploy-accessory-backup:ship:nucleus]
0 3 * * * f=/tmp/myapp-backup-$(date +%Y%m%d-%H%M%S).tar.gz && tar -czf "$f" -C /deployments/myapp/volumes . && aws s3 cp "$f" s3://old-bucket/myapp/volumes/ --region us-east-1 && rm -f "$f" && for x in $(aws s3 ls s3://old-bucket/myapp/volumes/ --region us-east-1 | sed 's/.* //' | sort | head -n -7); do [ -n "$x" ] && aws s3 rm s3://old-bucket/myapp/volumes/$x --region us-east-1; done # [teploy-backup:myapp]
*/10 * * * * echo $(date +%s) >> /tmp/heartbeat # unrelated line, pct char must survive
0 5 * * * TEPLOY_S3_ACCESS_KEY=ak TEPLOY_S3_SECRET_KEY=sk /usr/local/bin/teploy accessory backup db --local --app other --bucket b --region us-east-1 # [teploy-accessory-backup:other:db]
`

// rewriteBackupCrons rewrites teploy-generated legacy lines to the run
// form, leaves hand-written commands (and unrelated lines, including
// their % characters) byte-identical, and migrates the retention baked
// into the legacy line.
func TestRewriteBackupCrons(t *testing.T) {
	t.Setenv("TEPLOY_SERVER_BINARY", "/usr/local/bin/teploy")
	mock := ssh.NewMockExecutor("srv",
		ssh.MockCommand{Match: "crontab -l", Output: testCrontab},
		ssh.MockCommand{Match: "'/usr/local/bin/teploy' backup run --help", Output: ""},
		ssh.MockCommand{Match: "crontab -", Output: ""},
		ssh.MockCommand{Match: "cat '/deployments/.teploy/apps/myapp", Output: ""},
		ssh.MockCommand{Match: "d=$(dirname", Output: ""},
	)
	ctx := context.Background()
	rewritten, kept, err := rewriteBackupCrons(ctx, mock)
	if err != nil {
		t.Fatalf("rewriteBackupCrons: %v", err)
	}
	if len(rewritten) != 2 || rewritten[0] != "teploy-backup:myapp" || rewritten[1] != "teploy-accessory-backup:other:db" {
		t.Fatalf("rewritten = %v", rewritten)
	}
	if len(kept) != 1 || kept[0] != "teploy-accessory-backup:ship:nucleus" {
		t.Fatalf("kept = %v (the hand-written nucleus line must not be touched)", kept)
	}

	// The new crontab is installed over stdin (after any policy write, so
	// find the payload that carries cron markers).
	var installed string
	for _, in := range mock.Inputs {
		if strings.Contains(in, "[teploy-backup:") || strings.Contains(in, "[teploy-accessory-backup:") {
			installed = in
		}
	}
	if installed == "" {
		t.Fatalf("no crontab payload installed; inputs: %q", mock.Inputs)
	}
	if !strings.Contains(installed, "0 3 * * * /usr/local/bin/teploy backup run --app 'myapp' # [teploy-backup:myapp]") {
		t.Errorf("volumes line not rewritten to the run form:\n%s", installed)
	}
	if !strings.Contains(installed, "0 5 * * * /usr/local/bin/teploy backup run --app 'other' --accessory 'db' # [teploy-accessory-backup:other:db]") {
		t.Errorf("accessory line not rewritten to the run form:\n%s", installed)
	}
	for _, banned := range []string{"old-bucket", "TEPLOY_S3", "AWS_SECRET"} {
		if strings.Contains(installed, banned) {
			t.Errorf("rewritten crontab still embeds %s:\n%s", banned, installed)
		}
	}
	if !strings.Contains(installed, "/usr/local/bin/teploy-nucleus-backup ship # [teploy-accessory-backup:ship:nucleus]") {
		t.Errorf("hand-written line altered:\n%s", installed)
	}
	if !strings.Contains(installed, "echo $(date +%s) >> /tmp/heartbeat # unrelated line, pct char must survive") {
		t.Errorf("unrelated line (with its percent character) not preserved byte-for-byte:\n%s", installed)
	}
	if !strings.Contains(installed, "/deployments/forgejo/scheduled-redeploy.sh") {
		t.Errorf("unrelated first line dropped:\n%s", installed)
	}
}

// Without binary support the rewrite refuses rather than installing a
// cron line the server cannot run.
func TestRewriteBackupCronsNeedsBinarySupport(t *testing.T) {
	t.Setenv("TEPLOY_SERVER_BINARY", "/opt/nope/teploy")
	mock := ssh.NewMockExecutor("srv",
		ssh.MockCommand{Match: "crontab -l", Output: testCrontab},
		ssh.MockCommand{Match: "'/opt/nope/teploy' backup run --help", Err: errFakeCommand},
	)
	rewritten, kept, err := rewriteBackupCrons(context.Background(), mock)
	if err != nil || len(rewritten) != 0 {
		t.Fatalf("rewrote without binary support: %v %v %v", rewritten, kept, err)
	}
	if len(kept) != 3 {
		t.Fatalf("kept = %v, want all three markers left alone", kept)
	}
}

var errFakeCommand = &fakeCmdError{}

type fakeCmdError struct{}

func (*fakeCmdError) Error() string { return "exit status 1" }

// Doctor's backup-cron check fails on embedded credentials, warns on
// legacy teploy lines, and never echoes raw line text.
func TestDoctorBackupCronCheck(t *testing.T) {
	ctx := context.Background()
	mock := ssh.NewMockExecutor("srv", ssh.MockCommand{Match: "crontab -l", Output: testCrontab})
	check := doctorBackupCronCheck(ctx, mock)
	if check.Result != doctorFail || !strings.Contains(check.Detail, "1 cron line") || !strings.Contains(check.Detail, "credentials") {
		t.Fatalf("embedded credentials not failed: %+v", check)
	}
	for _, secret := range []string{"TEPLOY_S3_SECRET_KEY=sk", "AWS_SECRET"} {
		if strings.Contains(check.Detail, secret) {
			t.Errorf("check detail echoes raw line text: %+v", check)
		}
	}

	clean := ssh.NewMockExecutor("srv", ssh.MockCommand{Match: "crontab -l", Output: "30 3 * * * /usr/local/bin/teploy backup run --app 'myapp' # [teploy-backup:myapp]\n"})
	check = doctorBackupCronCheck(ctx, clean)
	if check.Result != doctorOK {
		t.Fatalf("clean crontab not ok: %+v", check)
	}

	legacyOnly := strings.Split(testCrontab, "\n")[2]
	legacy := ssh.NewMockExecutor("srv", ssh.MockCommand{Match: "crontab -l", Output: legacyOnly})
	check = doctorBackupCronCheck(ctx, legacy)
	if check.Result != doctorWarn || !strings.Contains(check.Detail, "bucket") {
		t.Fatalf("legacy line not warned: %+v", check)
	}
}

// Schedules join the cron markers with the stored retention.
func TestDiscoverBackupSchedules(t *testing.T) {
	mock := ssh.NewMockExecutor("srv",
		ssh.MockCommand{Match: "crontab -l", Output: "30 3 * * * /usr/local/bin/teploy backup run --app 'myapp' # [teploy-backup:myapp]\n15 2 * * * /usr/local/bin/teploy backup run --app 'myapp' --accessory 'db' # [teploy-accessory-backup:myapp:db]\n"},
		ssh.MockCommand{Match: "cat '/deployments/.teploy/apps/myapp", Output: `{"volumes":{"cron":"30 3 * * *","keep_last":7},"accessories":{"db":{"cron":"15 2 * * *","keep_last":5,"max_age_days":30}}}`},
	)
	rows, err := discoverBackupSchedules(context.Background(), mock, "myapp")
	if err != nil {
		t.Fatalf("discoverBackupSchedules: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %+v", rows)
	}
	if rows[0].Kind != backup.KindVolumes || rows[0].KeepLast != 7 || rows[0].Form != backup.CronFormRun {
		t.Errorf("volumes row: %+v", rows[0])
	}
	if rows[1].Accessory != "db" || rows[1].KeepLast != 5 || rows[1].MaxAgeDays != 30 {
		t.Errorf("accessory row: %+v", rows[1])
	}
}

// A stored target backs the folderless commands; legacy flags win over it.
func TestResolveBackupS3(t *testing.T) {
	ctx := context.Background()
	stored := ssh.NewMockExecutor("srv",
		ssh.MockCommand{Match: "for p in /etc/teploy/backup-target.json", Output: "path /etc/teploy/backup-target.json\n" + `{"v":"1","bucket":"kept","region":"eu-west-1","endpoint":"http://127.0.0.1:9100","credentials":"AGE"}`},
	)
	cfg, err := resolveBackupS3(ctx, stored, &backupTargetFlags{})
	if err != nil {
		t.Fatalf("resolveBackupS3: %v", err)
	}
	if cfg.Bucket != "kept" || cfg.Region != "eu-west-1" || cfg.Endpoint != "http://127.0.0.1:9100" || cfg.StoredTarget != "/etc/teploy/backup-target.json" {
		t.Fatalf("stored target not applied: %+v", cfg)
	}

	legacy, err := resolveBackupS3(ctx, stored, &backupTargetFlags{bucket: "override", endpoint: "http://127.0.0.1:9999"})
	if err != nil {
		t.Fatalf("resolveBackupS3 legacy: %v", err)
	}
	if legacy.Bucket != "override" || legacy.StoredTarget != "" {
		t.Fatalf("legacy flags must bypass the stored target: %+v", legacy)
	}

	none := ssh.NewMockExecutor("srv",
		ssh.MockCommand{Match: "for p in /etc/teploy/backup-target.json", Output: "absent"},
	)
	if _, err := resolveBackupS3(ctx, none, &backupTargetFlags{}); err == nil || !strings.Contains(err.Error(), "no backup target") {
		t.Fatalf("err = %v, want the no-target refusal", err)
	}
}
