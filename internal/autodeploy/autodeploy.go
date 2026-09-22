package autodeploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/useteploy/teploy/internal/caddy"
	"github.com/useteploy/teploy/internal/ssh"
)

const (
	deploymentsDir = "/deployments"
	// scheduledScriptName is the per-app on-server script that the scheduled
	// redeploy cron job invokes. It checks for a newer image digest and
	// recreates the container if there is one — otherwise no-ops silently.
	scheduledScriptName = "scheduled-redeploy.sh"
)

// ValidateSchedule rejects cron strings containing characters outside the
// canonical 5-field grammar. Mirrors backup.ValidateSchedule so the two
// scheduled features behave identically. We're not enforcing the field count
// here (cron itself rejects malformed strings on install) — just blocking
// characters that could enable shell injection through the crontab call.
func ValidateSchedule(schedule string) error {
	for _, c := range schedule {
		if !((c >= '0' && c <= '9') || c == ' ' || c == '*' || c == '/' || c == '-' || c == ',') {
			return fmt.Errorf("invalid cron schedule %q — unexpected character %q", schedule, string(c))
		}
	}
	return nil
}

// validBranch matches real-world git branch names (alphanumeric, dot,
// underscore, slash, hyphen — e.g. "main", "feature/new-landing") while
// excluding whitespace and shell/systemd metacharacters.
var validBranch = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._/-]*$`)

// ValidateBranch checks that a branch name is safe to embed in a systemd
// ExecStart= line (Setup, below) and in the git commands `teploy autodeploy
// serve` runs against it. Unvalidated, this was a latent shell-injection
// vector in the previous generated-bash-script implementation too
// (`BRANCH="%s"` interpolated directly) — not new to this rewrite, but
// worth actually fixing now rather than carrying forward.
func ValidateBranch(branch string) error {
	if branch == "" {
		return fmt.Errorf("branch is required")
	}
	if !validBranch.MatchString(branch) {
		return fmt.Errorf("invalid branch %q — must be alphanumeric with .-_/ (no spaces or shell metacharacters)", branch)
	}
	return nil
}

// DefaultPort is the webhook listener's default local port.
const DefaultPort = 9876

// Config holds auto-deploy configuration.
type Config struct {
	App    string
	Branch string // branch to watch (default "main")
	Secret string // webhook secret for validation
	// TeployBinaryPath is where the teploy binary was uploaded on the
	// server (see internal/cli/binarydist.go's deployTeployBinaryToServer,
	// called by the CLI layer before Setup — this package doesn't fetch it
	// itself to avoid an import cycle with internal/cli, which already
	// imports this package). The systemd unit's ExecStart runs `<this>
	// autodeploy serve --app <app> --port <port>`.
	TeployBinaryPath string
	// Port is the local port `teploy autodeploy serve` listens on and
	// Caddy's webhook route (SetupCaddyRoute) proxies to (DefaultPort).
	Port int
}

// Manager handles webhook auto-deploy setup on the server.
type Manager struct {
	exec ssh.Executor
	out  io.Writer
}

// sudoPrefix returns "sudo " if not running as root, empty string otherwise.
func (m *Manager) sudoPrefix(ctx context.Context) string {
	if id, err := m.exec.Run(ctx, "id -u"); err == nil && strings.TrimSpace(id) == "0" {
		return ""
	}
	return "sudo "
}

// NewManager creates an auto-deploy manager.
func NewManager(exec ssh.Executor, out io.Writer) *Manager {
	return &Manager{exec: exec, out: out}
}

// SecretPath returns the on-server path where the webhook secret is
// stored (0600, root-owned) — written by Setup, read by `teploy autodeploy
// serve` at startup. A fixed, well-known path rather than a --secret CLI
// flag: an argument would be visible in this host's `ps aux` /
// `/proc/<pid>/cmdline` for the life of the serve process, the same class
// of exposure Phase 1/2 closed for shell command arguments elsewhere.
func SecretPath(app string) string {
	return fmt.Sprintf("%s/%s/.autodeploy-secret", deploymentsDir, app)
}

// BuildDir returns the on-server directory `teploy autodeploy serve`
// fetches and builds from — the same path deployAppConfig's server-build
// mode already uses for `teploy deploy` (internal/cli/deploy.go), so a
// webhook-triggered deploy and a manual one share one checkout instead of
// each maintaining their own.
func BuildDir(app string) string {
	return fmt.Sprintf("%s/%s/build", deploymentsDir, app)
}

// Setup installs the auto-deploy webhook listener on the server: writes
// the webhook secret to SecretPath, then installs and starts a systemd
// service running `<TeployBinaryPath> autodeploy serve` — a real Go HTTP
// server (internal/cli/autodeploy_serve.go) that verifies the webhook
// signature, dedups replayed deliveries, and calls the exact same deploy
// code `teploy deploy` uses. Caddy is configured separately (see
// SetupCaddyRoute) to proxy POST /teploy-webhook/<app> to it.
func (m *Manager) Setup(ctx context.Context, cfg Config) error {
	if cfg.Branch == "" {
		cfg.Branch = "main"
	}
	if err := ValidateBranch(cfg.Branch); err != nil {
		return err
	}
	if cfg.Port == 0 {
		cfg.Port = DefaultPort
	}
	if cfg.Port < 1 || cfg.Port > 65535 {
		return fmt.Errorf("auto-deploy listener port must be in 1..65535 (got %d)", cfg.Port)
	}
	// Require a secret. Without one the webhook listener accepts any POST and
	// becomes an unauthenticated remote deploy trigger. The CLI generates a
	// random secret when the user doesn't supply one, so an empty secret here
	// is a programming error, not a user choice. A secret with surrounding
	// whitespace is rejected TOO (audit T32): the value is stored verbatim
	// and HMAC-verified verbatim at both ends — a "helpfully" trimmed key at
	// one end only would sign with different bytes than the provider.
	if strings.TrimSpace(cfg.Secret) == "" {
		return fmt.Errorf("auto-deploy requires a webhook secret (refusing to install an unauthenticated listener)")
	}
	if cfg.Secret != strings.TrimSpace(cfg.Secret) {
		return fmt.Errorf("the webhook secret must not have leading or trailing whitespace — quote the value exactly as the git provider will send it")
	}
	if strings.TrimSpace(cfg.TeployBinaryPath) == "" {
		return fmt.Errorf("auto-deploy requires TeployBinaryPath (upload the teploy binary before calling Setup)")
	}

	appDir := fmt.Sprintf("%s/%s", deploymentsDir, cfg.App)
	if _, err := m.exec.Run(ctx, "mkdir -p "+appDir); err != nil {
		return fmt.Errorf("creating app directory: %w", err)
	}

	fmt.Fprintln(m.out, "Writing webhook secret...")
	if err := m.exec.Upload(ctx, strings.NewReader(cfg.Secret), SecretPath(cfg.App), "0600"); err != nil {
		return fmt.Errorf("uploading webhook secret: %w", err)
	}

	// Install and start systemd service running `teploy autodeploy serve`.
	fmt.Fprintln(m.out, "Setting up webhook listener...")
	serviceName := fmt.Sprintf("teploy-webhook-%s", cfg.App)
	// No shell-style quoting here: ExecStart= is parsed by systemd's own
	// (POSIX-shell-similar but not identical) rules, not a real shell — so
	// ssh.ShellQuote's escaping isn't guaranteed to apply correctly. Not
	// needed anyway: App is constrained to config.validName's [a-z0-9-]+ by
	// config.validate() before Config.App is ever set, Branch is checked
	// above by ValidateBranch, and TeployBinaryPath is a fixed path this
	// package controls (see internal/cli/autodeploy.go) — none of the three
	// can contain whitespace or metacharacters.
	execStart := fmt.Sprintf("%s autodeploy serve --app %s --branch %s --port %d",
		cfg.TeployBinaryPath, cfg.App, cfg.Branch, cfg.Port)
	// /etc/systemd/system is root-owned — Upload writes over SFTP as the
	// connected user (tyler, say), which can't write there directly even
	// with sudo group membership (SFTP doesn't go through sudo). Stage in
	// /tmp (world-writable) and move into place with sudo, the same
	// pattern internal/harden uses for every other root-owned file this
	// codebase writes (e.g. fail2ban's jail.d config). Found live: this
	// step silently failed on every non-root setup until fixed.
	serviceContent := generateService(serviceName, execStart)
	stagingPath := fmt.Sprintf("/tmp/teploy-%s.service", serviceName)
	servicePath := fmt.Sprintf("/etc/systemd/system/%s.service", serviceName)
	if err := m.exec.Upload(ctx, strings.NewReader(serviceContent), stagingPath, "0644"); err != nil {
		return fmt.Errorf("uploading systemd service: %w", err)
	}

	sudo := m.sudoPrefix(ctx)
	if _, err := m.exec.Run(ctx, fmt.Sprintf("%smv %s %s", sudo, stagingPath, servicePath)); err != nil {
		return fmt.Errorf("installing systemd service: %w", err)
	}
	cmds := []string{
		sudo + "systemctl daemon-reload",
		fmt.Sprintf("%ssystemctl enable %s", sudo, serviceName),
		fmt.Sprintf("%ssystemctl restart %s", sudo, serviceName),
	}
	for _, cmd := range cmds {
		if _, err := m.exec.Run(ctx, cmd); err != nil {
			return fmt.Errorf("setting up service: %w", err)
		}
	}

	// systemctl restart returning success only means systemd accepted the
	// request — it does NOT mean the process is actually running. Found
	// live: TeployBinaryPath can point at a binary from the last published
	// release (deployTeployBinaryToServer always fetches "latest"), which
	// doesn't yet have the `autodeploy serve` subcommand if it's newer
	// than that release. The unit then crash-loops on "unknown flag:
	// --app" every restart while Setup still reported "Webhook listener
	// running on port 9876" — a silent false success. Verify the service
	// actually reached "active" before declaring victory.
	time.Sleep(1 * time.Second)
	active, _ := m.exec.Run(ctx, sudo+"systemctl is-active "+serviceName)
	if strings.TrimSpace(active) != "active" {
		logs, _ := m.exec.Run(ctx, fmt.Sprintf("%sjournalctl -u %s --no-pager -n 20 --output=cat", sudo, serviceName))
		return fmt.Errorf("webhook listener failed to start (status: %s) — %s may not support 'autodeploy serve' yet (newer than the last published teploy release). Recent logs:\n%s",
			strings.TrimSpace(active), cfg.TeployBinaryPath, strings.TrimSpace(logs))
	}

	fmt.Fprintf(m.out, "  Webhook listener running on port %d\n", cfg.Port)

	// The listener binds 0.0.0.0 (see runAutoDeployServe) so Caddy's
	// container can reach it via host.docker.internal — but teploy setup's
	// hardening (internal/harden.ConfigureUFW) defaults to deny-incoming,
	// so without an explicit allow rule UFW silently drops Caddy's
	// requests before they ever reach this process. Found live: the
	// listener was demonstrably active and healthy, but every webhook
	// request timed out (curl exit 28) until this rule was added. Scoped
	// to the "teploy" docker network's own subnet, not 0.0.0.0/0 — the
	// public internet still can't reach this port directly. Best-effort:
	// UFW may not be installed/active (a server set up with --no-harden,
	// or one using a different firewall entirely), which is the user's
	// choice to manage, not a reason to fail Setup.
	if err := m.allowWebhookPortInFirewall(ctx, sudo, cfg.Port); err != nil {
		fmt.Fprintf(m.out, "  Warning: could not open port %d in the firewall for Caddy: %v\n", cfg.Port, err)
		fmt.Fprintln(m.out, "  If UFW (or another firewall) is active, webhook requests from Caddy may be silently dropped — allow the \"teploy\" docker network's subnet to reach this port manually.")
	}

	return nil
}

// allowWebhookPortInFirewall scopes a UFW allow rule to the "teploy" docker
// network's own subnet, so the webhook listener is reachable from Caddy's
// container without exposing it to the wider LAN/internet. No-op (not an
// error) if UFW isn't installed or isn't active.
func (m *Manager) allowWebhookPortInFirewall(ctx context.Context, sudo string, port int) error {
	// Detect via `sudo ufw status`, not `which ufw`: ufw lives in
	// /usr/sbin, which isn't on a non-interactive SSH session's default
	// PATH for a non-root user (only /usr/local/bin:/usr/bin:/bin:
	// /usr/games on stock Debian) — `which ufw` always failed here even
	// with ufw installed and active, silently skipping this entire step
	// on every server. sudo uses its own secure_path (includes
	// /usr/sbin), so `sudo ufw status` finds it reliably. Found live on a
	// fresh VM: the webhook route and listener were both fine, but every
	// request 502'd because this rule silently never got added.
	status, err := m.exec.Run(ctx, sudo+"ufw status")
	if err != nil {
		return nil // UFW not installed — nothing to configure.
	}
	if !strings.Contains(status, "Status: active") {
		return nil // UFW installed but not enabled.
	}

	subnet, err := m.exec.Run(ctx, "docker network inspect teploy --format '{{(index .IPAM.Config 0).Subnet}}'")
	if err != nil {
		return fmt.Errorf("could not determine the teploy docker network's subnet: %w", err)
	}
	subnet = strings.TrimSpace(subnet)
	if subnet == "" {
		return fmt.Errorf("teploy docker network has no configured subnet")
	}

	cmd := fmt.Sprintf("%sufw allow from %s to any port %d proto tcp comment 'teploy autodeploy webhook listener'",
		sudo, ssh.ShellQuote(subnet), port)
	if _, err := m.exec.Run(ctx, cmd); err != nil {
		return fmt.Errorf("adding ufw rule: %w", err)
	}
	return nil
}

// SetupCaddyRoute persists the webhook route INTO THE CADDYFILE, inside
// the app's managed site block (see internal/caddy/webhook.go — audit
// T26/T27). The old runtime admin-API injection lived only in Caddy's
// memory: the next ordinary deploy's Caddyfile regeneration and reload —
// including a webhook-triggered deploy — silently erased the webhook
// endpoint. The route also now honors the CONFIGURED listener port (the
// old renderer hardcoded 9876) and matches every configured domain (the
// old renderer inserted the comma-separated domain string as ONE host).
func (m *Manager) SetupCaddyRoute(ctx context.Context, app, domain string, port int) error {
	fmt.Fprintln(m.out, "Adding Caddy webhook route...")
	c := caddy.NewClient(m.exec)
	if err := c.SetWebhookRoute(ctx, app, domain, port); err != nil {
		return fmt.Errorf("persisting the webhook route: %w", err)
	}
	// The fragment renders inside the app's managed site block; an app
	// that was never deployed (or uses external ingress) has none yet —
	// say so instead of implying the endpoint is live.
	live, err := c.HasManagedBlock(ctx, app)
	if err != nil {
		return nil // the route IS persisted; block detection is advisory
	}
	if !live {
		fmt.Fprintln(m.out, "  Note: the app has no Caddy site block yet — the webhook route attaches on its first deploy")
	}
	return nil
}

// Status checks if auto-deploy is set up for the app. A transport/execution
// failure is an ERROR (audit T30): the old shape classified it as "not
// active", so a broken SSH connection read as "autodeploy removed".
func (m *Manager) Status(ctx context.Context, app string) (bool, string, error) {
	serviceName := fmt.Sprintf("teploy-webhook-%s", app)
	out, err := m.exec.Run(ctx, fmt.Sprintf("systemctl is-active %s 2>/dev/null", serviceName))
	if err != nil {
		return false, "", fmt.Errorf("checking the webhook listener service for %s: %w", app, err)
	}
	status := strings.TrimSpace(out)
	return status == "active", status, nil
}

// ScheduleStatus reports whether a scheduled redeploy is configured for the
// app, returning the cron expression on success. Empty string means none.
func (m *Manager) ScheduleStatus(ctx context.Context, app string) (string, error) {
	scriptPath := fmt.Sprintf("%s/%s/%s", deploymentsDir, app, scheduledScriptName)
	// crontab -l exits non-zero when there is no crontab. Treat that as "no schedule".
	// app is validated (config.ValidateName) by every caller today, but quote
	// scriptPath anyway rather than rely on that staying true forever — %q
	// (Go double-quote) doesn't stop shell $()/backtick expansion the way
	// ssh.ShellQuote's single-quoting does.
	out, err := m.exec.Run(ctx, fmt.Sprintf("crontab -l 2>/dev/null | grep -F %s || true", ssh.ShellQuote(scriptPath)))
	if err != nil {
		return "", nil
	}
	line := strings.TrimSpace(out)
	if line == "" {
		return "", nil
	}
	// A cron entry looks like: `<5 fields> <command>`. Split off the command.
	idx := strings.Index(line, scriptPath)
	if idx <= 0 {
		return line, nil
	}
	return strings.TrimSpace(line[:idx]), nil
}

// Schedule installs a server-side cron job that periodically checks for a
// newer image digest and redeploys the running container if one is found.
//
// The on-server script does the work entirely with docker CLI commands, so
// the teploy binary does not need to be installed on the server. It pulls
// the image referenced by the currently-running container, compares digests,
// and only recreates the container if the digest changed. No new image, no
// container restart — quiet no-op.
func (m *Manager) Schedule(ctx context.Context, app, schedule, branch, binaryPath string) error {
	if app == "" {
		return fmt.Errorf("app name is required")
	}
	if err := ValidateSchedule(schedule); err != nil {
		return err
	}
	if err := ValidateBranch(branch); err != nil {
		return err
	}
	if binaryPath == "" {
		return fmt.Errorf("teploy binary path is required (the scheduled redeploy invokes the deploy engine on the server)")
	}

	appDir := fmt.Sprintf("%s/%s", deploymentsDir, app)
	scriptPath := fmt.Sprintf("%s/%s", appDir, scheduledScriptName)

	if _, err := m.exec.Run(ctx, "mkdir -p "+appDir); err != nil {
		return fmt.Errorf("creating app directory: %w", err)
	}

	fmt.Fprintln(m.out, "Installing scheduled-redeploy script...")
	script := generateScheduledRedeployScript(app, branch, binaryPath)
	if err := m.exec.Upload(ctx, strings.NewReader(script), scriptPath, "0755"); err != nil {
		return fmt.Errorf("uploading scheduled-redeploy script: %w", err)
	}

	// Replace any existing entry pointing at this script, then add the new
	// one. The current crontab is read with its exit status CHECKED
	// (audit T29, mirroring backup.SetSchedule's TCL-46 shape): the old
	// `crontab -l 2>/dev/null | grep -vF …` pipeline masked any real read
	// failure as empty input and then installed ONLY teploy's entry —
	// silently deleting every unrelated cron job on the machine. Only the
	// canonical "no crontab for <user>" failure means "start from empty".
	entry := fmt.Sprintf("%s %s >> %s/scheduled-redeploy.log 2>&1", schedule, scriptPath, appDir)
	cronCmd := fmt.Sprintf(
		`raw=$(crontab -l 2>&1); rc=$?; `+
			`if [ "$rc" -ne 0 ]; then case "$raw" in *"no crontab"*) raw="";; *) `+
			`echo "reading crontab failed: $raw" >&2; exit 1;; esac; fi; `+
			`(printf '%%s\n' "$raw" | grep -vF %s; printf '%%s\n' %s) | crontab -`,
		ssh.ShellQuote(scriptPath), ssh.ShellQuote(entry),
	)
	if _, err := m.exec.Run(ctx, cronCmd); err != nil {
		return fmt.Errorf("installing cron entry (unrelated jobs are preserved): %w", err)
	}

	fmt.Fprintf(m.out, "Scheduled redeploy installed for %s\n", app)
	fmt.Fprintf(m.out, "  Schedule: %s\n", schedule)
	fmt.Fprintf(m.out, "  Branch:   %s\n", branch)
	fmt.Fprintf(m.out, "  Script:   %s\n", scriptPath)
	fmt.Fprintf(m.out, "  Engine:   %s autodeploy redeploy (locks, health gate, release record, rollback)\n", binaryPath)
	fmt.Fprintf(m.out, "  Log:      %s/scheduled-redeploy.log\n", appDir)
	return nil
}

// Unschedule removes the scheduled redeploy cron entry for the app.
// The on-server script file is left in place so a subsequent Schedule()
// call doesn't have to reupload it. The crontab read is status-checked
// like Schedule's, and the `crontab -r` fallback is GONE (audit T29): a
// failed replacement used to fall through to removing the user's ENTIRE
// crontab.
func (m *Manager) Unschedule(ctx context.Context, app string) error {
	if app == "" {
		return fmt.Errorf("app name is required")
	}
	scriptPath := fmt.Sprintf("%s/%s/%s", deploymentsDir, app, scheduledScriptName)

	cronCmd := fmt.Sprintf(
		`raw=$(crontab -l 2>&1); rc=$?; `+
			`if [ "$rc" -ne 0 ]; then case "$raw" in *"no crontab"*) raw="";; *) `+
			`echo "reading crontab failed: $raw" >&2; exit 1;; esac; fi; `+
			`printf '%%s\n' "$raw" | grep -vF %s | crontab -`,
		ssh.ShellQuote(scriptPath),
	)
	if _, err := m.exec.Run(ctx, cronCmd); err != nil {
		return fmt.Errorf("removing cron entry (unrelated jobs are preserved): %w", err)
	}
	fmt.Fprintf(m.out, "Scheduled redeploy removed for %s\n", app)
	return nil
}

// Remove disables and removes both the auto-deploy webhook and any
// scheduled redeploy for the app. Every step's failure is aggregated
// (audit T30): the old shape ignored stop/disable/unit-delete/reload,
// unschedule, and route-removal errors and then printed "removed" — a live
// service could keep deploying after the operator believed it disabled.
// Best-effort cleanup is fine; presenting incomplete cleanup as complete
// is not.
func (m *Manager) Remove(ctx context.Context, app string) error {
	serviceName := fmt.Sprintf("teploy-webhook-%s", app)

	sudo := m.sudoPrefix(ctx)
	var failures []error
	for _, step := range []struct {
		name string
		cmd  string
	}{
		{"stopping the webhook service", fmt.Sprintf("%ssystemctl stop %s 2>/dev/null", sudo, serviceName)},
		{"disabling the webhook service", fmt.Sprintf("%ssystemctl disable %s 2>/dev/null", sudo, serviceName)},
		{"deleting the unit file", fmt.Sprintf("%srm -f /etc/systemd/system/%s.service", sudo, serviceName)},
		{"reloading systemd", sudo + "systemctl daemon-reload"},
	} {
		if _, err := m.exec.Run(ctx, step.cmd); err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", step.name, err))
		}
	}
	if err := m.Unschedule(ctx, app); err != nil {
		failures = append(failures, fmt.Errorf("unscheduling: %w", err))
	}
	// Remove the persisted webhook route (webhook.go): the descriptor and
	// the fragment inside the app's site block go in one Caddyfile
	// transaction — a stale fragment would 502 after the listener dies.
	if err := caddy.NewClient(m.exec).RemoveWebhookRoute(ctx, app); err != nil {
		failures = append(failures, fmt.Errorf("removing the webhook route: %w", err))
	}
	if len(failures) > 0 {
		return fmt.Errorf("autodeploy removal incomplete for %s — %w", app, errors.Join(failures...))
	}
	fmt.Fprintf(m.out, "Auto-deploy removed for %s\n", app)
	return nil
}

// generateScheduledRedeployScript returns the bash script that the cron job
// invokes. It runs entirely on the server with no teploy-binary dependency:
//  1. Find the running container for this app's "web" process by labels.
//  2. Pull the image tag the container was started with.
//  3. Compare the running image digest with the just-pulled one. If they
//     match, exit silently — the container is already current.
//  4. Otherwise, capture the current container's env, volumes, ports,
//     labels, network, and restart policy via docker inspect, then
//     stop+rm and recreate it with the same name and the new image.
//
// We keep the same container name on purpose: Caddy's reverse_proxy upstream
// resolves containers by their network alias / DNS name, and re-using the
// engine entry. The script only performs the CHEAP part — find the web
// container, pull its image tag, compare digests, and exit 0 when nothing
// changed. When the digest DID move it invokes the on-server teploy binary's
// `autodeploy redeploy`, which runs the exact same fenced, health-gated,
// release-recorded deploy code as `teploy deploy` and the webhook listener
// (C02: one execution path for every trigger). The old version of this
// script reconstructed the container from docker inspect and did its own
// stop/rm/run — no lock, no health gate, no release record, no rollback,
// and a stop-to-start downtime window.
func generateScheduledRedeployScript(app, branch, binaryPath string) string {
	return fmt.Sprintf(`#!/bin/bash
# Scheduled redeploy for %[1]s (branch %[2]s) — digest pre-check, then the full deploy engine.
set -e

APP=%[4]q
BRANCH=%[5]q
LOG="/deployments/$APP/scheduled-redeploy.log"

ts() { date -u +%%Y-%%m-%%dT%%H:%%M:%%SZ; }

CONTAINER=$(docker ps --filter "label=teploy.app=$APP" --filter "label=teploy.process=web" --format '{{.Names}}' | head -n 1)
if [ -z "$CONTAINER" ]; then
    echo "$(ts) [skip] no running container for $APP/web" >> "$LOG"
    exit 0
fi

IMAGE=$(docker inspect --format='{{.Config.Image}}' "$CONTAINER")
if [ -z "$IMAGE" ]; then
    echo "$(ts) [error] could not read image for $CONTAINER" >> "$LOG"
    exit 1
fi

CURRENT_DIGEST=$(docker inspect --format='{{.Image}}' "$CONTAINER")

# Pull the same tag — gets the latest digest if the upstream was updated.
if ! docker pull "$IMAGE" >> "$LOG" 2>&1; then
    echo "$(ts) [error] docker pull failed for $IMAGE" >> "$LOG"
    exit 1
fi

NEW_DIGEST=$(docker image inspect --format='{{.Id}}' "$IMAGE")

if [ "$CURRENT_DIGEST" = "$NEW_DIGEST" ]; then
    # No-op silent exit. Don't spam the log on every cron tick.
    exit 0
fi

echo "$(ts) [redeploy] new digest for $IMAGE — running the deploy engine" >> "$LOG"
%[3]q autodeploy redeploy --app "$APP" --branch "$BRANCH" >> "$LOG" 2>&1
`, app, branch, binaryPath, app, branch)
}

// generateService renders the systemd unit that runs execStart (the full
// `<teploy binary> autodeploy serve ...` command line) as a resident
// process, restarting it if it crashes.
func generateService(name, execStart string) string {
	return fmt.Sprintf(`[Unit]
Description=Teploy webhook listener (%s)
After=network.target

[Service]
Type=simple
ExecStart=%s
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
`, name, execStart)
}
