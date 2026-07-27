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

	// Webhook wraps the chain in a failure alert. The alert itself is now a
	// script on the server rather than an inline curl (see backup_alert.go): the
	// payload and the request moved into buildBackupAlertScript, which is what
	// makes signing possible at all, so those assertions live in
	// TestBuildBackupAlertScript. What the cron line still has to get right is
	// the wrapping and the exit code.
	withHook := buildScheduledBackupCmd("myapp", "1.2.3.4", "my-bucket", "us-east-1", 7, "https://hooks.example.com/x")
	if !strings.HasPrefix(withHook, "( ") || !strings.Contains(withHook, ") || {") {
		t.Errorf("alert wrapping missing:\n%s", withHook)
	}
	if !strings.Contains(withHook, backupAlertScriptPath("myapp")) {
		t.Errorf("alert clause does not invoke the alert script:\n%s", withHook)
	}
	// `; false` preserves the non-zero exit so cron still logs the failure.
	if !strings.Contains(withHook, "false; }") {
		t.Errorf("alert clause must preserve the non-zero exit:\n%s", withHook)
	}
	// The alert clause must still be %-free: it is the part of the crontab line
	// that used to trip cron's %-escaping, and the reason the body moved to a
	// file is that a file has no such restriction.
	alertPart := withHook[strings.Index(withHook, ") || {"):]
	if strings.Contains(alertPart, "%") {
		t.Errorf("alert clause must contain no '%%':\n%s", alertPart)
	}
}

func TestBuildBackupAlertScript(t *testing.T) {
	const (
		app    = "akiroo-lite"
		server = "deploy-home2"
		url    = "https://akiroo.example.com/api/webhooks/teploy_cli/dlbs"
	)

	signed := buildBackupAlertScript(app, server, url, "s3cr3t")

	// The payload still has to match notify.Payload, since the receiver parses
	// the same shape from every teploy delivery.
	for _, want := range []string{
		`"app":"akiroo-lite"`, `"server":"deploy-home2"`,
		`"type":"backup"`, `"success":false`,
	} {
		if !strings.Contains(signed, want) {
			t.Errorf("script payload missing %q:\n%s", want, signed)
		}
	}
	// Signed with the same construction as internal/notify/sign.go: HMAC over
	// timestamp + "." + body, in the same two headers.
	for _, want := range []string{
		"date +%s",
		`printf '%s.%s' "$TS" "$BODY"`,
		"openssl dgst -sha256 -hmac",
		"X-Teploy-Timestamp: $TS",
		"X-Teploy-Signature: sha256=$SIG",
	} {
		if !strings.Contains(signed, want) {
			t.Errorf("signed script missing %q:\n%s", want, signed)
		}
	}
	// It is a script, so % is allowed here — that is the entire reason this
	// moved out of the crontab line.
	if !strings.HasPrefix(signed, "#!/bin/sh\n") {
		t.Error("script has no shebang, so cron cannot execute it")
	}

	// No secret: still delivers, unsigned, exactly as before.
	unsigned := buildBackupAlertScript(app, server, url, "")
	if !strings.Contains(unsigned, "curl -sf -m 10 -X POST") {
		t.Errorf("unsigned script does not deliver:\n%s", unsigned)
	}
	if strings.Contains(unsigned, "SECRET=''") == false {
		t.Errorf("unsigned script should carry an empty SECRET:\n%s", unsigned)
	}

	// An unsigned fallback must exist even in the signed script: if openssl is
	// missing on the box, a delivery that arrives and gets rejected tells the
	// operator something, and silence tells them nothing.
	if strings.Count(signed, "curl -sf -m 10 -X POST") < 2 {
		t.Errorf("signed script has no unsigned fallback path:\n%s", signed)
	}
	if !strings.Contains(signed, "command -v openssl") {
		t.Errorf("signed script does not check openssl is present:\n%s", signed)
	}
}

// Every interpolated value is shell-quoted. app, server and the URL all come
// from configuration, and this file runs as root from cron — a value able to
// close a quote and append a command would be a root shell.
func TestBackupAlertScriptQuotesHostileValues(t *testing.T) {
	script := buildBackupAlertScript(
		"app'; touch /tmp/pwned; '",
		"srv'; id; '",
		"https://x/'; whoami; '",
		"sec'; echo no; '",
	)
	for _, injected := range []string{"touch /tmp/pwned", "; id;", "whoami", "echo no"} {
		// The text may appear inside a quoted literal, but never as a command:
		// an unescaped quote followed by it is what makes it executable.
		if strings.Contains(script, "' "+injected) || strings.Contains(script, "';"+injected) {
			t.Errorf("value escaped its quoting and became a command (%q):\n%s", injected, script)
		}
	}
	// ShellQuote doubles single quotes; the payload's own quotes must survive.
	if !strings.Contains(script, "BODY='") || !strings.Contains(script, "URL='") || !strings.Contains(script, "SECRET='") {
		t.Errorf("script values are not single-quoted:\n%s", script)
	}
}

func TestBackupAlertScriptPath(t *testing.T) {
	// Under the app's own deployment directory, so removing the app removes it.
	if got := backupAlertScriptPath("myapp"); got != "/deployments/myapp/backup-alert.sh" {
		t.Errorf("backupAlertScriptPath = %q", got)
	}
}
