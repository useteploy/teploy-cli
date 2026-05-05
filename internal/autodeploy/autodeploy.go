package autodeploy

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/useteploy/teploy/internal/ssh"
)

const (
	deploymentsDir = "/deployments"
	scriptName     = "autodeploy.sh"
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

// Config holds auto-deploy configuration.
type Config struct {
	App    string
	Branch string // branch to watch (default "main")
	Secret string // webhook secret for validation
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

// Setup installs the auto-deploy webhook handler on the server.
// Creates a lightweight shell script that:
//  1. Validates the webhook secret
//  2. Checks if the push is to the watched branch
//  3. Pulls the latest code and rebuilds
//
// Caddy is configured to route POST /teploy-webhook/<app> to the script
// via a simple exec handler using a systemd service.
func (m *Manager) Setup(ctx context.Context, cfg Config) error {
	if cfg.Branch == "" {
		cfg.Branch = "main"
	}

	appDir := fmt.Sprintf("%s/%s", deploymentsDir, cfg.App)
	scriptPath := fmt.Sprintf("%s/%s", appDir, scriptName)

	// Ensure app directory exists.
	if _, err := m.exec.Run(ctx, "mkdir -p "+appDir); err != nil {
		return fmt.Errorf("creating app directory: %w", err)
	}

	// Write the auto-deploy script.
	fmt.Fprintln(m.out, "Installing auto-deploy script...")
	script := generateScript(cfg)
	if err := m.exec.Upload(ctx, strings.NewReader(script), scriptPath, "0755"); err != nil {
		return fmt.Errorf("uploading deploy script: %w", err)
	}

	// Create systemd service for the webhook listener.
	fmt.Fprintln(m.out, "Setting up webhook listener...")
	serviceName := fmt.Sprintf("teploy-webhook-%s", cfg.App)
	listenerScript := generateListener(cfg.App, cfg.Secret, scriptPath)
	listenerPath := fmt.Sprintf("%s/webhook-listener.sh", appDir)

	if err := m.exec.Upload(ctx, strings.NewReader(listenerScript), listenerPath, "0755"); err != nil {
		return fmt.Errorf("uploading listener script: %w", err)
	}

	// Install and start systemd service.
	serviceContent := generateService(serviceName, listenerPath)
	servicePath := fmt.Sprintf("/etc/systemd/system/%s.service", serviceName)
	if err := m.exec.Upload(ctx, strings.NewReader(serviceContent), servicePath, "0644"); err != nil {
		return fmt.Errorf("uploading systemd service: %w", err)
	}

	sudo := m.sudoPrefix(ctx)
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

	fmt.Fprintf(m.out, "  Webhook listener running on port 9876\n")
	return nil
}

// SetupCaddyRoute adds a Caddy route to proxy webhook requests to the listener.
func (m *Manager) SetupCaddyRoute(ctx context.Context, app, domain string) error {
	fmt.Fprintln(m.out, "Adding Caddy webhook route...")

	// We add a route that matches POST to /teploy-webhook/{app} and proxies to the local listener.
	// This uses a direct curl to the Caddy admin API to add a webhook-specific route.
	webhookRoute := fmt.Sprintf(`{
		"@id": "teploy-webhook-%s",
		"match": [{"host": ["%s"], "path": ["/teploy-webhook/%s"]}],
		"handle": [{"handler": "reverse_proxy", "upstreams": [{"dial": "localhost:9876"}]}]
	}`, app, domain, app)

	uploadPath := "/tmp/teploy_webhook_route.json"
	if err := m.exec.Upload(ctx, strings.NewReader(webhookRoute), uploadPath, "0644"); err != nil {
		return fmt.Errorf("uploading webhook route: %w", err)
	}

	// Try to add the route to existing Caddy config.
	cmd := fmt.Sprintf(
		"curl -sf -X POST http://localhost:2019/config/apps/http/servers/srv0/routes -H 'Content-Type: application/json' -d @%s",
		uploadPath,
	)
	if _, err := m.exec.Run(ctx, cmd); err != nil {
		return fmt.Errorf("adding Caddy webhook route: %w", err)
	}

	m.exec.Run(ctx, "rm -f "+uploadPath)
	return nil
}

// Status checks if auto-deploy is set up for the app.
func (m *Manager) Status(ctx context.Context, app string) (bool, string, error) {
	serviceName := fmt.Sprintf("teploy-webhook-%s", app)
	out, err := m.exec.Run(ctx, fmt.Sprintf("systemctl is-active %s 2>/dev/null", serviceName))
	if err != nil {
		return false, "", nil
	}
	status := strings.TrimSpace(out)
	return status == "active", status, nil
}

// ScheduleStatus reports whether a scheduled redeploy is configured for the
// app, returning the cron expression on success. Empty string means none.
func (m *Manager) ScheduleStatus(ctx context.Context, app string) (string, error) {
	scriptPath := fmt.Sprintf("%s/%s/%s", deploymentsDir, app, scheduledScriptName)
	// crontab -l exits non-zero when there is no crontab. Treat that as "no schedule".
	out, err := m.exec.Run(ctx, fmt.Sprintf("crontab -l 2>/dev/null | grep -F %q || true", scriptPath))
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
func (m *Manager) Schedule(ctx context.Context, app, schedule string) error {
	if app == "" {
		return fmt.Errorf("app name is required")
	}
	if err := ValidateSchedule(schedule); err != nil {
		return err
	}

	appDir := fmt.Sprintf("%s/%s", deploymentsDir, app)
	scriptPath := fmt.Sprintf("%s/%s", appDir, scheduledScriptName)

	if _, err := m.exec.Run(ctx, "mkdir -p "+appDir); err != nil {
		return fmt.Errorf("creating app directory: %w", err)
	}

	fmt.Fprintln(m.out, "Installing scheduled-redeploy script...")
	script := generateScheduledRedeployScript(app)
	if err := m.exec.Upload(ctx, strings.NewReader(script), scriptPath, "0755"); err != nil {
		return fmt.Errorf("uploading scheduled-redeploy script: %w", err)
	}

	// Replace any existing entry pointing at this script, then add the new one.
	// A bare && would short-circuit if there's no existing crontab; the
	// `crontab -l 2>/dev/null || true` form keeps the pipeline going from
	// a zero-state install.
	cronCmd := fmt.Sprintf(
		"(crontab -l 2>/dev/null | grep -vF %q; echo '%s %s >> %s/scheduled-redeploy.log 2>&1') | crontab -",
		scriptPath, schedule, scriptPath, appDir,
	)
	if _, err := m.exec.Run(ctx, cronCmd); err != nil {
		return fmt.Errorf("installing cron entry: %w", err)
	}

	fmt.Fprintf(m.out, "Scheduled redeploy installed for %s\n", app)
	fmt.Fprintf(m.out, "  Schedule: %s\n", schedule)
	fmt.Fprintf(m.out, "  Script:   %s\n", scriptPath)
	fmt.Fprintf(m.out, "  Log:      %s/scheduled-redeploy.log\n", appDir)
	return nil
}

// Unschedule removes the scheduled redeploy cron entry for the app.
// The on-server script file is left in place so a subsequent Schedule()
// call doesn't have to reupload it.
func (m *Manager) Unschedule(ctx context.Context, app string) error {
	if app == "" {
		return fmt.Errorf("app name is required")
	}
	scriptPath := fmt.Sprintf("%s/%s/%s", deploymentsDir, app, scheduledScriptName)

	cronCmd := fmt.Sprintf(
		"(crontab -l 2>/dev/null | grep -vF %q) | crontab - || crontab -r 2>/dev/null || true",
		scriptPath,
	)
	if _, err := m.exec.Run(ctx, cronCmd); err != nil {
		return fmt.Errorf("removing cron entry: %w", err)
	}
	fmt.Fprintf(m.out, "Scheduled redeploy removed for %s\n", app)
	return nil
}

// Remove disables and removes both the auto-deploy webhook and any
// scheduled redeploy for the app.
func (m *Manager) Remove(ctx context.Context, app string) error {
	serviceName := fmt.Sprintf("teploy-webhook-%s", app)

	sudo := m.sudoPrefix(ctx)
	cmds := []string{
		fmt.Sprintf("%ssystemctl stop %s 2>/dev/null", sudo, serviceName),
		fmt.Sprintf("%ssystemctl disable %s 2>/dev/null", sudo, serviceName),
		fmt.Sprintf("%srm -f /etc/systemd/system/%s.service", sudo, serviceName),
		sudo + "systemctl daemon-reload",
	}
	for _, cmd := range cmds {
		m.exec.Run(ctx, cmd)
	}

	// Remove scheduled redeploy too, if configured. Errors are non-fatal —
	// we're best-effort cleaning up.
	_ = m.Unschedule(ctx, app)

	fmt.Fprintf(m.out, "Auto-deploy removed for %s\n", app)
	return nil
}

func generateScript(cfg Config) string {
	return fmt.Sprintf(`#!/bin/bash
# Auto-deploy script for %s
# Triggered by webhook on push to %s
set -e

APP="%s"
BRANCH="%s"
DEPLOY_DIR="/deployments/$APP/build"
LOG="/deployments/$APP/autodeploy.log"

echo "$(date -u '+%%Y-%%m-%%dT%%H:%%M:%%SZ') Auto-deploy triggered for $APP (branch: $BRANCH)" >> "$LOG"

cd "$DEPLOY_DIR" 2>/dev/null || { echo "No build directory" >> "$LOG"; exit 1; }

# Pull latest changes.
git fetch origin "$BRANCH" >> "$LOG" 2>&1
git reset --hard "origin/$BRANCH" >> "$LOG" 2>&1

# Detect build method and build.
if [ -f Dockerfile ]; then
    VERSION=$(git rev-parse --short HEAD)
    docker build -t "${APP}-build-${VERSION}" . >> "$LOG" 2>&1
else
    echo "No Dockerfile found" >> "$LOG"
    exit 1
fi

echo "$(date -u '+%%Y-%%m-%%dT%%H:%%M:%%SZ') Build complete: ${APP}-build-${VERSION}" >> "$LOG"
`, cfg.App, cfg.Branch, cfg.App, cfg.Branch)
}

func generateListener(app, secret, scriptPath string) string {
	secretCheck := ""
	if secret != "" {
		// Escape single quotes in the secret to prevent shell injection.
		escaped := strings.ReplaceAll(secret, "'", "'\\''")
		secretCheck = fmt.Sprintf(`
    # Validate webhook secret.
    SIGNATURE=$(echo "$BODY" | openssl dgst -sha256 -hmac '%s' | awk '{print $2}')
    EXPECTED="sha256=$SIGNATURE"
    if [ "$HTTP_X_HUB_SIGNATURE_256" != "$EXPECTED" ]; then
        echo "HTTP/1.1 403 Forbidden\r\n\r\nInvalid signature"
        continue
    fi`, escaped)
	}

	return fmt.Sprintf(`#!/bin/bash
# Webhook listener for %s
# Listens on port 9876, validates requests, triggers deploy script

while true; do
    echo -e "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\n\r\nok" | \
    nc -l -p 9876 -q 1 | {
        read -r METHOD PATH VERSION
        BODY=""
        while IFS= read -r LINE; do
            LINE=$(echo "$LINE" | tr -d '\r')
            [ -z "$LINE" ] && break
        done
        read -r BODY
%s
        # Trigger deploy in background.
        nohup %s >> /deployments/%s/autodeploy.log 2>&1 &
    }
done
`, app, secretCheck, scriptPath, app)
}

// generateScheduledRedeployScript returns the bash script that the cron job
// invokes. It runs entirely on the server with no teploy-binary dependency:
//   1. Find the running container for this app's "web" process by labels.
//   2. Pull the image tag the container was started with.
//   3. Compare the running image digest with the just-pulled one. If they
//      match, exit silently — the container is already current.
//   4. Otherwise, capture the current container's env, volumes, ports,
//      labels, network, and restart policy via docker inspect, then
//      stop+rm and recreate it with the same name and the new image.
//
// We keep the same container name on purpose: Caddy's reverse_proxy upstream
// resolves containers by their network alias / DNS name, and re-using the
// name avoids a Caddy reconfigure step. The trade-off is a brief downtime
// window between stop and start (typically 1-3 seconds for Forgejo-class
// services). True zero-downtime swap belongs in a v2 that runs through
// teploy deploy on the server side.
func generateScheduledRedeployScript(app string) string {
	return fmt.Sprintf(`#!/bin/bash
# Scheduled redeploy script for %[1]s
# Pulls the image and recreates the container if (and only if) the digest changed.
set -e

APP=%[1]q
PROCESS="web"
LOG="/deployments/$APP/scheduled-redeploy.log"

ts() { date -u +%%Y-%%m-%%dT%%H:%%M:%%SZ; }

CONTAINER=$(docker ps --filter "label=teploy.app=$APP" --filter "label=teploy.process=$PROCESS" --format '{{.Names}}' | head -n 1)
if [ -z "$CONTAINER" ]; then
    echo "$(ts) [skip] no running container for $APP/$PROCESS" >> "$LOG"
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

echo "$(ts) [redeploy] new digest for $IMAGE — recreating $CONTAINER" >> "$LOG"

# Snapshot config from the running container before we tear it down.
ENV_FILE=$(mktemp)
docker inspect --format='{{range .Config.Env}}{{println .}}{{end}}' "$CONTAINER" > "$ENV_FILE"

VOL_ARGS=()
while IFS= read -r line; do
    [ -z "$line" ] && continue
    VOL_ARGS+=("-v" "$line")
done < <(docker inspect --format='{{range .Mounts}}{{if eq .Type "volume"}}{{.Name}}:{{.Destination}}
{{else if eq .Type "bind"}}{{.Source}}:{{.Destination}}
{{end}}{{end}}' "$CONTAINER")

LABEL_ARGS=()
NEW_VERSION=$(date +%%s)
while IFS= read -r line; do
    [ -z "$line" ] && continue
    KEY="${line%%%%=*}"
    VAL="${line#*=}"
    if [ "$KEY" = "teploy.version" ]; then
        VAL="$NEW_VERSION"
    fi
    LABEL_ARGS+=("--label" "$KEY=$VAL")
done < <(docker inspect --format='{{range $k, $v := .Config.Labels}}{{$k}}={{$v}}
{{end}}' "$CONTAINER")

PORT_ARGS=()
while IFS= read -r line; do
    [ -z "$line" ] && continue
    PORT_ARGS+=("-p" "$line")
done < <(docker inspect --format='{{range $port, $bindings := .NetworkSettings.Ports}}{{range $bindings}}{{.HostIp}}:{{.HostPort}}:{{$port}}
{{end}}{{end}}' "$CONTAINER" | sed 's|/tcp||;s|/udp||')

NETWORK=$(docker inspect --format='{{range $n, $v := .NetworkSettings.Networks}}{{$n}}{{end}}' "$CONTAINER" | head -n 1)
RESTART=$(docker inspect --format='{{.HostConfig.RestartPolicy.Name}}' "$CONTAINER")
[ -z "$RESTART" ] && RESTART=no

# Tear down the old container.
docker stop "$CONTAINER" >> "$LOG" 2>&1 || true
docker rm "$CONTAINER" >> "$LOG" 2>&1 || true

# Recreate with the same name and config + new image.
NEW_ID=$(docker run -d \
    --name "$CONTAINER" \
    --network "${NETWORK:-bridge}" \
    --restart "$RESTART" \
    --env-file "$ENV_FILE" \
    "${LABEL_ARGS[@]}" \
    "${VOL_ARGS[@]}" \
    "${PORT_ARGS[@]}" \
    "$IMAGE" 2>>"$LOG") || {
        echo "$(ts) [error] docker run failed — container is down" >> "$LOG"
        rm -f "$ENV_FILE"
        exit 1
    }

rm -f "$ENV_FILE"

echo "$(ts) [ok] $CONTAINER redeployed (id=${NEW_ID:0:12} version=$NEW_VERSION)" >> "$LOG"
`, app)
}

func generateService(name, listenerPath string) string {
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
`, name, listenerPath)
}
