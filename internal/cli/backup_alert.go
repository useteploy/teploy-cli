package cli

// The scheduled-backup failure alert.
//
// A scheduled backup runs headless from cron: teploy.yml is not on the server,
// the teploy binary is not on the server, and cron has no environment to read a
// secret from. The alert therefore has to be self-contained on the box.
//
// It used to be a bare `curl` inlined into the crontab line, which meant it was
// the one delivery path in this CLI that went out UNSIGNED — a receiver
// verifying signatures (see internal/notify/sign.go) rejects it, and rejects it
// in a way that looks like a misconfigured secret rather than a missing feature.
//
// Signing it inline is not really possible: the construction needs a timestamp,
// `date +%s` contains a `%`, and crontab treats `%` as a newline escape. That is
// the trap the old comment was avoiding.
//
// So the alert moves into a script file. Inside a file `%` has no special
// meaning, which makes both `date +%s` and `openssl dgst` available, and the
// crontab line shrinks to a path. The secret lives in that file at mode 0700 —
// the same trust boundary as /deployments itself, which already holds the app's
// env and its release history.

import (
	"fmt"
	"strings"

	"github.com/useteploy/teploy/internal/ssh"
)

// backupAlertScriptPath is where the alert script lives for an app. Under the
// app's own deployment directory so removing the app removes it too.
func backupAlertScriptPath(app string) string {
	return "/deployments/" + app + "/backup-alert.sh"
}

// buildBackupAlertScript renders the failure-alert script.
//
// secret may be empty, in which case the delivery goes out unsigned exactly as
// it always has — an install that has not opted into signing must keep working.
//
// Every interpolated value is shell-quoted: app and server come from config and
// the URL from a user, so none of them may be able to end a quoted string and
// append a command to a file that runs as root from cron.
func buildBackupAlertScript(app, server, webhookURL, secret string) string {
	payload := fmt.Sprintf(
		`{"app":%q,"server":%q,"type":"backup","success":false,"message":"Scheduled backup failed","duration_ms":0,"timestamp":""}`,
		app, server,
	)

	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("# Written by `teploy backup schedule`. Invoked by cron when a scheduled\n")
	b.WriteString("# backup fails. Do not edit — rescheduling overwrites this file.\n")
	// No `set -e`: this script's whole job is to report a failure, and exiting
	// early on a non-zero step would swallow the report it exists to send.
	b.WriteString("\n")
	b.WriteString("BODY=" + ssh.ShellQuote(payload) + "\n")
	b.WriteString("URL=" + ssh.ShellQuote(webhookURL) + "\n")
	b.WriteString("SECRET=" + ssh.ShellQuote(secret) + "\n")
	b.WriteString("\n")
	b.WriteString(`if [ -n "$SECRET" ] && command -v openssl >/dev/null 2>&1; then` + "\n")
	b.WriteString("  TS=$(date +%s)\n")
	// awk '{print $NF}' rather than a sed on "= ": openssl prints either
	// "HMAC-SHA256(stdin)= <hex>" or a bare hex depending on version, and the
	// last field is the digest in both.
	b.WriteString(`  SIG=$(printf '%s.%s' "$TS" "$BODY" | openssl dgst -sha256 -hmac "$SECRET" 2>/dev/null | awk '{print $NF}')` + "\n")
	b.WriteString(`  if [ -n "$SIG" ]; then` + "\n")
	b.WriteString("    exec curl -sf -m 10 -X POST \\\n")
	b.WriteString(`      -H 'Content-Type: application/json' \` + "\n")
	b.WriteString(`      -H "X-Teploy-Timestamp: $TS" \` + "\n")
	b.WriteString(`      -H "X-Teploy-Signature: sha256=$SIG" \` + "\n")
	b.WriteString(`      -d "$BODY" "$URL"` + "\n")
	b.WriteString("  fi\n")
	b.WriteString("fi\n")
	b.WriteString("\n")
	// Unsigned fallback. Reached when no secret is configured, or when openssl is
	// absent or failed: a delivery that arrives and is rejected tells the
	// operator something, and silence tells them nothing.
	b.WriteString("exec curl -sf -m 10 -X POST -H 'Content-Type: application/json' -d \"$BODY\" \"$URL\"\n")
	return b.String()
}
