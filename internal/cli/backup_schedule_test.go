package cli

import (
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/backup"
	"github.com/useteploy/teploy/internal/config"
)

func TestFirstWebhookURL(t *testing.T) {
	if got := firstWebhookURL(config.NotificationsConfig{}); got != "" {
		t.Errorf("empty config should yield no webhook, got %q", got)
	}
	legacy := config.NotificationsConfig{Webhook: "https://hooks.example.com/x"}
	if got := firstWebhookURL(legacy); got != "https://hooks.example.com/x" {
		t.Errorf("legacy webhook not returned: %q", got)
	}
	channels := config.NotificationsConfig{
		Channels: []config.NotificationChannelConfig{
			{Type: "smtp", URL: ""},
			{Type: "webhook", URL: "https://ch.example.com/y"},
		},
	}
	if got := firstWebhookURL(channels); got != "https://ch.example.com/y" {
		t.Errorf("channel webhook not returned: %q", got)
	}
}

// The generated cron line must be the bare run form (D3): absolute binary
// path, the app, nothing else. The bucket and the credentials live in the
// server's stored target and are read at run time, so a leaked crontab
// leaks nothing, and a target change never rewrites cron.
func TestBuildScheduledBackupRunCmd(t *testing.T) {
	vol := buildScheduledBackupRunCmd("/usr/local/bin/teploy", "myapp", "")
	for _, want := range []string{
		"/usr/local/bin/teploy backup run --app 'myapp'",
	} {
		if !strings.Contains(vol, want) {
			t.Errorf("volumes cmd missing %q: %s", want, vol)
		}
	}
	for _, banned := range []string{"bucket", "TEPLOY_S3", "AWS_SECRET", "AWS_ACCESS", "region"} {
		if strings.Contains(vol, banned) {
			t.Errorf("volumes cmd must carry no bucket/credentials/env (%s): %s", banned, vol)
		}
	}
	acc := buildScheduledBackupRunCmd("/usr/local/bin/teploy", "myapp", "db")
	if !strings.Contains(acc, "--accessory 'db'") {
		t.Errorf("accessory cmd missing the accessory flag: %s", acc)
	}
}

// Marker classification drives both the `backup-target set` rewrite and
// the doctor check: teploy-generated legacy lines rewrite, hand-written
// commands under a teploy marker (the nucleus-backup pattern) must be
// left alone, and the run form is already current.
func TestFindCronMarkerForms(t *testing.T) {
	cases := []struct {
		name string
		line string
		form string
		kind string
		app  string
		acc  string
		keep int
	}{
		{
			name: "run volumes",
			line: "30 3 * * * /usr/local/bin/teploy backup run --app myapp # [teploy-backup:myapp]",
			form: backup.CronFormRun, kind: "volumes", app: "myapp",
		},
		{
			name: "run accessory",
			line: "15 2 * * * /usr/local/bin/teploy backup run --app myapp --accessory db # [teploy-accessory-backup:myapp:db]",
			form: backup.CronFormRun, kind: "accessory", app: "myapp", acc: "db",
		},
		{
			name: "legacy volumes shell chain with retention",
			line: "0 3 * * * f=/tmp/myapp-backup-$(date +%Y%m%d-%H%M%S).tar.gz && tar -czf \"$f\" -C /deployments/myapp/volumes . && aws s3 cp \"$f\" s3://bucket/myapp/volumes/ --region us-east-1 && rm -f \"$f\" && for x in $(aws s3 ls s3://bucket/myapp/volumes/ | head -n -7); do :; done # [teploy-backup:myapp]",
			form: backup.CronFormLegacy, kind: "volumes", app: "myapp", keep: 7,
		},
		{
			name: "legacy accessory invocation with embedded creds",
			line: "30 4 * * * TEPLOY_S3_ACCESS_KEY=ak TEPLOY_S3_SECRET_KEY=sk /usr/local/bin/teploy accessory backup nucleus --local --app ship --bucket b --region us-east-1 # [teploy-accessory-backup:ship:nucleus]",
			form: backup.CronFormLegacy, kind: "accessory", app: "ship", acc: "nucleus",
		},
		{
			name: "hand-written script under a marker",
			line: "30 4 * * * /usr/local/bin/teploy-nucleus-backup ship # [teploy-accessory-backup:ship:nucleus]",
			form: backup.CronFormCustom, kind: "accessory", app: "ship", acc: "nucleus",
		},
	}
	for _, tc := range cases {
		m := backup.FindCronMarker(tc.line)
		if m == nil {
			t.Fatalf("%s: marker not found", tc.name)
		}
		if m.Form != tc.form || m.Kind != tc.kind || m.App != tc.app || m.Accessory != tc.acc {
			t.Errorf("%s: got form=%s kind=%s app=%s acc=%s, want %s/%s/%s/%s", tc.name, m.Form, m.Kind, m.App, m.Accessory, tc.form, tc.kind, tc.app, tc.acc)
		}
		if m.KeepLast != tc.keep {
			t.Errorf("%s: keep-last = %d, want %d", tc.name, m.KeepLast, tc.keep)
		}
		if m.Cron == "" || strings.Contains(m.Cron, "teploy") {
			t.Errorf("%s: cron fields wrong: %q", tc.name, m.Cron)
		}
	}

	if m := backup.FindCronMarker("17 */6 * * * /usr/local/bin/infra-maintenance.sh"); m != nil {
		t.Errorf("unrelated cron line classified as a backup marker: %+v", m)
	}
	if m := backup.FindCronMarker("30 4 * * * /usr/local/bin/teploy-nucleus-backup ship"); m != nil {
		t.Errorf("marker-less teploy line classified: %+v", m)
	}
}
