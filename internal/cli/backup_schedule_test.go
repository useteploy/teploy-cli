package cli

import (
	"strings"
	"testing"

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

func TestBuildScheduledBackupCmd(t *testing.T) {
	// Base: archive + upload + cleanup, no retention, no alert.
	base := buildScheduledBackupCmd("myapp", "1.2.3.4", "my-bucket", "us-east-1", 0, "")
	for _, want := range []string{
		"tar -czf /tmp/myapp-backup-",
		"aws s3 cp /tmp/myapp-backup-*.tar.gz s3://my-bucket/myapp/volumes/ --region us-east-1",
		"rm -f /tmp/myapp-backup-*.tar.gz",
	} {
		if !strings.Contains(base, want) {
			t.Errorf("base cmd missing %q:\n%s", want, base)
		}
	}
	if strings.Contains(base, "head -n -") || strings.Contains(base, "curl") {
		t.Errorf("base cmd should have no retention/alert:\n%s", base)
	}

	// Keep-last bakes a prune clause.
	withKeep := buildScheduledBackupCmd("myapp", "1.2.3.4", "my-bucket", "us-east-1", 7, "")
	if !strings.Contains(withKeep, "head -n -7") {
		t.Errorf("keep-last prune clause missing:\n%s", withKeep)
	}

	// Webhook wraps the chain in a failure alert.
	withHook := buildScheduledBackupCmd("myapp", "1.2.3.4", "my-bucket", "us-east-1", 7, "https://hooks.example.com/x")
	if !strings.HasPrefix(withHook, "( ") || !strings.Contains(withHook, ") || {") {
		t.Errorf("alert wrapping missing:\n%s", withHook)
	}
	if !strings.Contains(withHook, `"success":false`) || !strings.Contains(withHook, `"type":"backup"`) {
		t.Errorf("alert payload malformed:\n%s", withHook)
	}
	if !strings.Contains(withHook, "curl -sf -m 10 -X POST") {
		t.Errorf("alert curl missing:\n%s", withHook)
	}
	// The alert must be %-free so it can't trip cron's %-escaping.
	alertPart := withHook[strings.Index(withHook, ") || {"):]
	if strings.Contains(alertPart, "%") {
		t.Errorf("alert clause must contain no '%%':\n%s", alertPart)
	}
}
