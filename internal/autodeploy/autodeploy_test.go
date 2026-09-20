package autodeploy

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/ssh"
)

func TestSetup(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "mkdir -p /deployments/myapp", Output: ""},
		ssh.MockCommand{Match: "UPLOAD:", Output: ""},
		ssh.MockCommand{Match: "id -u", Output: "0"},
		ssh.MockCommand{Match: "mv /tmp/teploy-teploy-webhook-myapp.service", Output: ""},
		ssh.MockCommand{Match: "systemctl daemon-reload", Output: ""},
		ssh.MockCommand{Match: "systemctl enable", Output: ""},
		ssh.MockCommand{Match: "systemctl restart", Output: ""},
		ssh.MockCommand{Match: "systemctl is-active", Output: "active"},
	)

	var buf bytes.Buffer
	mgr := NewManager(mock, &buf)
	err := mgr.Setup(context.Background(), Config{
		App:              "myapp",
		Branch:           "main",
		Secret:           "mysecret",
		TeployBinaryPath: "/deployments/.bin/teploy",
	})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "Writing webhook secret") {
		t.Error("expected secret-write message")
	}
	if !strings.Contains(output, "webhook listener") {
		t.Error("expected webhook listener message")
	}

	// The secret must reach the server only via the dedicated secret file
	// (SecretPath) — never embedded in a generated script or the systemd
	// unit, either of which `systemctl cat`/`ps aux` could expose.
	secret, ok := mock.Files[SecretPath("myapp")]
	if !ok {
		t.Fatal("webhook secret not uploaded to SecretPath")
	}
	if string(secret) != "mysecret" {
		t.Errorf("secret file = %q, want %q", secret, "mysecret")
	}

	// The systemd unit's ExecStart must reference the uploaded teploy
	// binary and the app/branch/port — this is what actually gets
	// invoked, replacing the old bash listener script entirely.
	// Staged in /tmp first (not the final /etc/systemd/system path) — the
	// final move happens via `sudo mv`, which Upload's mock doesn't see;
	// asserting the mv command ran is covered by mock.Calls below.
	svc, ok := mock.Files["/tmp/teploy-teploy-webhook-myapp.service"]
	if !ok {
		t.Fatal("systemd service not staged for upload")
	}
	for _, want := range []string{
		"ExecStart=/deployments/.bin/teploy autodeploy serve",
		"--app myapp",
		"--branch main",
		"--port 9876",
	} {
		if !strings.Contains(string(svc), want) {
			t.Errorf("systemd unit missing %q, got:\n%s", want, svc)
		}
	}

	var movedIntoPlace bool
	for _, c := range mock.Calls {
		if c == "mv /tmp/teploy-teploy-webhook-myapp.service /etc/systemd/system/teploy-webhook-myapp.service" {
			movedIntoPlace = true
		}
	}
	if !movedIntoPlace {
		t.Error("expected the staged systemd unit to be moved into /etc/systemd/system")
	}
}

// TestSetup_OpensFirewallForWebhookPort reproduces a third real failure
// found live-testing on a fresh, fully-hardened server: even with the
// listener active on 0.0.0.0 and Caddy dialing host.docker.internal
// correctly, every webhook request timed out (curl exit 28) because
// teploy setup's hardening (internal/harden.ConfigureUFW) defaults to
// deny-incoming and nothing had ever allowed port 9876 through. Setup must
// add a UFW rule scoped to the "teploy" docker network's own subnet (not
// 0.0.0.0/0) so Caddy can reach it without exposing the port publicly.
func TestSetup_OpensFirewallForWebhookPort(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "mkdir -p /deployments/myapp", Output: ""},
		ssh.MockCommand{Match: "UPLOAD:", Output: ""},
		ssh.MockCommand{Match: "id -u", Output: "0"},
		ssh.MockCommand{Match: "mv /tmp/teploy-teploy-webhook-myapp.service", Output: ""},
		ssh.MockCommand{Match: "systemctl daemon-reload", Output: ""},
		ssh.MockCommand{Match: "systemctl enable", Output: ""},
		ssh.MockCommand{Match: "systemctl restart", Output: ""},
		ssh.MockCommand{Match: "systemctl is-active", Output: "active"},
		ssh.MockCommand{Match: "ufw status", Output: "Status: active"},
		ssh.MockCommand{Match: "docker network inspect teploy", Output: "172.18.0.0/16\n"},
		ssh.MockCommand{Match: "ufw allow from '172.18.0.0/16' to any port 9876 proto tcp", Output: "Rule added"},
	)

	var buf bytes.Buffer
	mgr := NewManager(mock, &buf)
	err := mgr.Setup(context.Background(), Config{
		App:              "myapp",
		Branch:           "main",
		Secret:           "mysecret",
		TeployBinaryPath: "/deployments/.bin/teploy",
	})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}

	var openedFirewall bool
	for _, c := range mock.Calls {
		if strings.HasPrefix(c, "ufw allow from '172.18.0.0/16' to any port 9876") {
			openedFirewall = true
		}
	}
	if !openedFirewall {
		t.Error("expected a UFW rule scoped to the teploy docker network's subnet")
	}
}

// TestSetup_MissingUFWIsNotFatal ensures a server without UFW (--no-harden,
// or a different firewall entirely) doesn't fail Setup — opening the
// webhook port in the firewall is a best-effort enhancement, not a
// requirement.
func TestSetup_MissingUFWIsNotFatal(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "mkdir -p /deployments/myapp", Output: ""},
		ssh.MockCommand{Match: "UPLOAD:", Output: ""},
		ssh.MockCommand{Match: "id -u", Output: "0"},
		ssh.MockCommand{Match: "mv /tmp/teploy-teploy-webhook-myapp.service", Output: ""},
		ssh.MockCommand{Match: "systemctl daemon-reload", Output: ""},
		ssh.MockCommand{Match: "systemctl enable", Output: ""},
		ssh.MockCommand{Match: "systemctl restart", Output: ""},
		ssh.MockCommand{Match: "systemctl is-active", Output: "active"},
		ssh.MockCommand{Match: "ufw status", Err: fmt.Errorf("exit status 127: command not found")},
	)

	var buf bytes.Buffer
	mgr := NewManager(mock, &buf)
	if err := mgr.Setup(context.Background(), Config{
		App:              "myapp",
		Branch:           "main",
		Secret:           "mysecret",
		TeployBinaryPath: "/deployments/.bin/teploy",
	}); err != nil {
		t.Fatalf("Setup should succeed even without ufw installed: %v", err)
	}
	if !strings.Contains(buf.String(), "Webhook listener running") {
		t.Error("expected the success message despite no ufw")
	}
}

func TestSetup_DefaultBranch(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "mkdir", Output: ""},
		ssh.MockCommand{Match: "UPLOAD:", Output: ""},
		ssh.MockCommand{Match: "id -u", Output: "0"},
		ssh.MockCommand{Match: "mv /tmp/teploy-teploy-webhook-myapp.service", Output: ""},
		ssh.MockCommand{Match: "systemctl is-active", Output: "active"},
		ssh.MockCommand{Match: "systemctl", Output: ""},
	)

	var buf bytes.Buffer
	mgr := NewManager(mock, &buf)
	err := mgr.Setup(context.Background(), Config{
		App:              "myapp",
		Secret:           "test-secret",
		TeployBinaryPath: "/deployments/.bin/teploy",
	})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}

	svc := string(mock.Files["/tmp/teploy-teploy-webhook-myapp.service"])
	if !strings.Contains(svc, "--branch main") {
		t.Error("default branch should be main")
	}
}

// TestSetup_NonRootUsesSudoMv reproduces a real failure found live-testing
// autodeploy setup on a fresh (non-root, sudo-group) server: the systemd
// unit upload originally wrote straight to /etc/systemd/system via SFTP,
// which silently isn't possible as a non-root user (SFTP doesn't go
// through sudo) — the setup failed with "uploading systemd service:
// Process exited with status 1" every time the connecting user wasn't
// root. Every existing Setup test used "id -u" -> "0" (root), which
// can write there directly and masked the bug entirely.
func TestSetup_NonRootUsesSudoMv(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "mkdir -p /deployments/myapp", Output: ""},
		ssh.MockCommand{Match: "UPLOAD:", Output: ""},
		ssh.MockCommand{Match: "id -u", Output: "1000"},
		ssh.MockCommand{Match: "sudo mv /tmp/teploy-teploy-webhook-myapp.service /etc/systemd/system/teploy-webhook-myapp.service", Output: ""},
		ssh.MockCommand{Match: "sudo systemctl daemon-reload", Output: ""},
		ssh.MockCommand{Match: "sudo systemctl enable", Output: ""},
		ssh.MockCommand{Match: "sudo systemctl restart", Output: ""},
		ssh.MockCommand{Match: "sudo systemctl is-active", Output: "active"},
	)

	var buf bytes.Buffer
	mgr := NewManager(mock, &buf)
	err := mgr.Setup(context.Background(), Config{
		App:              "myapp",
		Branch:           "main",
		Secret:           "mysecret",
		TeployBinaryPath: "/deployments/.bin/teploy",
	})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}

	// Staged in /tmp (writable by any user), never attempted directly
	// against the root-owned final path.
	if _, ok := mock.Files["/etc/systemd/system/teploy-webhook-myapp.service"]; ok {
		t.Error("should never Upload (SFTP) directly to /etc/systemd/system as a non-root user")
	}
	if _, ok := mock.Files["/tmp/teploy-teploy-webhook-myapp.service"]; !ok {
		t.Error("expected the systemd unit staged in /tmp")
	}
}

// TestSetup_CrashLoopingServiceReportsFailure reproduces a real failure
// found live-testing against a fresh server: deployTeployBinaryToServer
// always installs the latest *published* GitHub release
// (internal/cli/binarydist.go), which — for any teploy build newer than
// that release, e.g. a local dev build ahead of what's tagged — doesn't
// yet have the `autodeploy serve` subcommand at all. systemd started the
// unit fine (the binary exists and is executable), but the process itself
// exited immediately on "unknown flag: --app" and kept crash-looping via
// systemd's restart backoff. Setup previously only checked that
// `systemctl restart` was *accepted*, not that the service actually
// stayed up, and printed "Webhook listener running on port 9876" while it
// was crash-looping. Setup must now verify systemctl is-active reports
// "active" before declaring success.
func TestSetup_CrashLoopingServiceReportsFailure(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "mkdir -p /deployments/myapp", Output: ""},
		ssh.MockCommand{Match: "UPLOAD:", Output: ""},
		ssh.MockCommand{Match: "id -u", Output: "0"},
		ssh.MockCommand{Match: "mv /tmp/teploy-teploy-webhook-myapp.service", Output: ""},
		ssh.MockCommand{Match: "systemctl daemon-reload", Output: ""},
		ssh.MockCommand{Match: "systemctl enable", Output: ""},
		ssh.MockCommand{Match: "systemctl restart", Output: ""},
		ssh.MockCommand{Match: "systemctl is-active", Output: "activating"},
		ssh.MockCommand{Match: "journalctl -u teploy-webhook-myapp", Output: "unknown flag: --app"},
	)

	var buf bytes.Buffer
	mgr := NewManager(mock, &buf)
	err := mgr.Setup(context.Background(), Config{
		App:              "myapp",
		Branch:           "main",
		Secret:           "mysecret",
		TeployBinaryPath: "/deployments/.bin/teploy",
	})
	if err == nil {
		t.Fatal("expected Setup to report failure for a crash-looping service, not silently succeed")
	}
	if !strings.Contains(err.Error(), "failed to start") {
		t.Errorf("expected a clear failed-to-start error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "unknown flag: --app") {
		t.Errorf("expected the journalctl output surfaced in the error, got: %v", err)
	}
	if strings.Contains(buf.String(), "Webhook listener running") {
		t.Error("must not print the success message when the service isn't actually active")
	}
}

func TestSetup_RequiresTeployBinaryPath(t *testing.T) {
	mgr := NewManager(ssh.NewMockExecutor("1.2.3.4"), &bytes.Buffer{})
	err := mgr.Setup(context.Background(), Config{App: "myapp", Secret: "s3cret"})
	if err == nil {
		t.Fatal("expected error when TeployBinaryPath is not set")
	}
}

func TestStatus_Active(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "systemctl is-active", Output: "active"},
	)

	var buf bytes.Buffer
	mgr := NewManager(mock, &buf)
	active, status, err := mgr.Status(context.Background(), "myapp")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !active {
		t.Error("expected active")
	}
	if status != "active" {
		t.Errorf("expected status 'active', got %q", status)
	}
}

func TestStatus_Inactive(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "systemctl is-active", Output: "inactive", Err: nil},
	)

	var buf bytes.Buffer
	mgr := NewManager(mock, &buf)
	active, _, err := mgr.Status(context.Background(), "myapp")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if active {
		t.Error("expected inactive")
	}
}

func TestRemove(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "id -u", Output: "0"},
		ssh.MockCommand{Match: "systemctl stop", Output: ""},
		ssh.MockCommand{Match: "systemctl disable", Output: ""},
		ssh.MockCommand{Match: "rm -f", Output: ""},
		ssh.MockCommand{Match: "systemctl daemon-reload", Output: ""},
		ssh.MockCommand{Match: "raw=$(crontab -l 2>&1)", Output: "0 4 * * 0 /deployments/myapp/scheduled-redeploy.sh >> /deployments/myapp/scheduled-redeploy.log 2>&1"},
		ssh.MockCommand{Match: "mkdir /deployments/caddy/.lock", Output: ""},
		ssh.MockCommand{Match: "cat /deployments/caddy/Caddyfile", Output: "{\n\tadmin 0.0.0.0:2019\n}\n"},
		ssh.MockCommand{Match: "a=$(docker exec caddy md5sum", Output: "TEPLOY_CADDY_OK"},
		ssh.MockCommand{Match: "docker exec caddy caddy reload", Output: ""},
		ssh.MockCommand{Match: "rmdir /deployments/caddy/.lock", Output: ""},
	)

	var buf bytes.Buffer
	mgr := NewManager(mock, &buf)
	if err := mgr.Remove(context.Background(), "myapp"); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	if !strings.Contains(buf.String(), "Auto-deploy removed") {
		t.Error("expected removal message")
	}

	// The persisted webhook route must go too (T26): the descriptor file
	// and any fragment inside the app's Caddyfile block, in one
	// transaction — a stale runtime-API @id can no longer linger because
	// nothing lives in the runtime API anymore.
	var removedDescriptor, removedFragment bool
	for _, c := range mock.Calls {
		if strings.HasPrefix(c, "rm -f -- '/deployments/myapp/.webhook-route'") {
			removedDescriptor = true
		}
	}
	if _, ok := mock.Files["/deployments/myapp/.webhook-route"]; ok {
		t.Error("webhook route descriptor survived removal")
	}
	if !removedDescriptor {
		t.Error("expected Remove to delete the persisted webhook route descriptor")
	}
	if strings.Contains(string(mock.Files["/deployments/caddy/Caddyfile"]), "teploy-webhook") {
		t.Error("webhook fragment survived removal")
	}
	_ = removedFragment
}

// TestSetupCaddyRoute_RunsThroughDockerExec reproduces a real failure found
// live-testing against a fresh server: the admin-API call originally ran
// directly on the host (`curl http://localhost:2019/...`), but Caddy's
// admin API is never published to the host (only 80/443 are, in setup.go's
// `docker run` for the caddy container) — every call failed with curl exit
// 7, connection refused. It must run via `docker exec caddy`, the same way
// every other admin-API interaction in this codebase (Caddyfile reload)
// already does.
func TestSetupCaddyRoute_PersistsInCaddyfile(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "mkdir /deployments/caddy/.lock", Output: ""},
		ssh.MockCommand{Match: "cat /deployments/caddy/Caddyfile", Output: "{\n\tadmin 0.0.0.0:2019\n}\n"},
		ssh.MockCommand{Match: "a=$(docker exec caddy md5sum", Output: "TEPLOY_CADDY_OK"},
		ssh.MockCommand{Match: "docker exec caddy caddy reload", Output: ""},
		ssh.MockCommand{Match: "rmdir /deployments/caddy/.lock", Output: ""},
	)
	var buf bytes.Buffer
	mgr := NewManager(mock, &buf)
	if err := mgr.SetupCaddyRoute(context.Background(), "myapp", "myapp.com", 9876); err != nil {
		t.Fatalf("SetupCaddyRoute: %v", err)
	}
	// The persisted descriptor names the configured port and path.
	desc, ok := mock.Files["/deployments/myapp/.webhook-route"]
	if !ok {
		t.Fatal("webhook route descriptor not persisted")
	}
	if !strings.Contains(string(desc), "\"port\":9876") || !strings.Contains(string(desc), "/teploy-webhook/myapp") {
		t.Errorf("descriptor wrong: %s", desc)
	}
}

func TestGenerateService(t *testing.T) {
	svc := generateService("teploy-webhook-myapp", "/deployments/.bin/teploy autodeploy serve --app myapp --branch main --port 9876")

	if !strings.Contains(svc, "teploy-webhook-myapp") {
		t.Error("service should contain name")
	}
	if !strings.Contains(svc, "Restart=always") {
		t.Error("service should restart always")
	}
	if !strings.Contains(svc, "ExecStart=/deployments/.bin/teploy autodeploy serve --app myapp --branch main --port 9876") {
		t.Error("service should run the given execStart command line")
	}
}

func TestValidateSchedule(t *testing.T) {
	cases := []struct {
		in      string
		wantErr bool
	}{
		{"0 4 * * 0", false},
		{"*/5 * * * *", false},
		{"0,30 1-6 * * 1-5", false},
		{"0 4 * * 0; rm -rf /", true}, // shell metachar — must reject
		{"0 4 * * 0 && curl evil.com", true},
		{"$(whoami)", true},
		{"`whoami`", true},
	}
	for _, c := range cases {
		err := ValidateSchedule(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("ValidateSchedule(%q): wantErr=%v, got %v", c.in, c.wantErr, err)
		}
	}
}

func TestSchedule(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "mkdir -p /deployments/myapp", Output: ""},
		ssh.MockCommand{Match: "UPLOAD:", Output: ""},
		ssh.MockCommand{Match: "raw=$(crontab -l 2>&1)", Output: "5 5 * * * unrelated-job"},
	)

	var buf bytes.Buffer
	mgr := NewManager(mock, &buf)
	err := mgr.Schedule(context.Background(), "myapp", "0 4 * * 0")
	if err != nil {
		t.Fatalf("Schedule: %v", err)
	}

	script, ok := mock.Files["/deployments/myapp/scheduled-redeploy.sh"]
	if !ok {
		t.Fatal("scheduled-redeploy.sh not uploaded")
	}
	for _, want := range []string{
		`APP="myapp"`,
		"docker pull",
		"docker inspect",
		"teploy.app=$APP",
		"teploy.process=$PROCESS",
		"CURRENT_DIGEST",
		"NEW_DIGEST",
	} {
		if !strings.Contains(string(script), want) {
			t.Errorf("scheduled-redeploy.sh missing %q", want)
		}
	}

	if !strings.Contains(buf.String(), "Scheduled redeploy installed for myapp") {
		t.Error("expected install confirmation")
	}
	if !strings.Contains(buf.String(), "0 4 * * 0") {
		t.Error("expected cron schedule in output")
	}
}

func TestSchedule_RejectsBadCron(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4")
	mgr := NewManager(mock, &bytes.Buffer{})

	err := mgr.Schedule(context.Background(), "myapp", "0 4 * * 0; echo pwned")
	if err == nil {
		t.Fatal("expected validation error for shell-metachar cron string")
	}
}

func TestSchedule_RejectsEmptyApp(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4")
	mgr := NewManager(mock, &bytes.Buffer{})

	if err := mgr.Schedule(context.Background(), "", "0 4 * * 0"); err == nil {
		t.Fatal("expected error for empty app name")
	}
}

func TestUnschedule(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "raw=$(crontab -l 2>&1)", Output: "5 5 * * * unrelated-job"},
	)

	var buf bytes.Buffer
	mgr := NewManager(mock, &buf)
	if err := mgr.Unschedule(context.Background(), "myapp"); err != nil {
		t.Fatalf("Unschedule: %v", err)
	}
	if !strings.Contains(buf.String(), "Scheduled redeploy removed for myapp") {
		t.Error("expected removal message")
	}
}

func TestScheduleStatus_Inactive(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "crontab -l", Output: ""},
	)

	mgr := NewManager(mock, &bytes.Buffer{})
	got, err := mgr.ScheduleStatus(context.Background(), "myapp")
	if err != nil {
		t.Fatalf("ScheduleStatus: %v", err)
	}
	if got != "" {
		t.Errorf("expected empty schedule, got %q", got)
	}
}

func TestScheduleStatus_Active(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "crontab -l", Output: "0 4 * * 0 /deployments/myapp/scheduled-redeploy.sh >> /deployments/myapp/scheduled-redeploy.log 2>&1"},
	)

	mgr := NewManager(mock, &bytes.Buffer{})
	got, err := mgr.ScheduleStatus(context.Background(), "myapp")
	if err != nil {
		t.Fatalf("ScheduleStatus: %v", err)
	}
	if got != "0 4 * * 0" {
		t.Errorf("expected schedule '0 4 * * 0', got %q", got)
	}
}

func TestGenerateScheduledRedeployScript(t *testing.T) {
	script := generateScheduledRedeployScript("myapp")
	for _, want := range []string{
		`APP="myapp"`,
		`PROCESS="web"`,
		"docker pull",
		"teploy.version",
		"docker inspect",
		"docker run -d",
		"--name \"$CONTAINER\"", // preserve same name to avoid Caddy reconfig
		"date +%s",              // new version timestamp
		"$(ts) [redeploy]",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("scheduled redeploy script missing %q", want)
		}
	}
}

// TestSchedule_FailedCrontabReadAborts is the T29 regression: a failed
// `crontab -l` used to be masked as empty input, wiping every unrelated
// cron job and installing only teploy's entry.
func TestSchedule_FailedCrontabReadAborts(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "mkdir -p /deployments/myapp", Output: ""},
		ssh.MockCommand{Match: "UPLOAD:", Output: ""},
		ssh.MockCommand{Match: "raw=$(crontab -l 2>&1)", Err: fmt.Errorf("crontab: permission denied")},
	)
	var buf bytes.Buffer
	mgr := NewManager(mock, &buf)
	if err := mgr.Schedule(context.Background(), "myapp", "0 4 * * 0"); err == nil {
		t.Fatal("a failed crontab read must abort the install, never replace the crontab")
	}
	for _, c := range mock.Calls {
		if strings.Contains(c, "| crontab -") && strings.HasPrefix(c, "(printf") {
			t.Errorf("a new crontab was installed despite the failed read: %s", c)
		}
	}
}

// TestUnschedule_NeverRunsCrontabR is the T29 regression: the old fallback
// `|| crontab -r` removed the user's ENTIRE crontab when the replacement
// failed.
func TestUnschedule_NeverRunsCrontabR(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "raw=$(crontab -l 2>&1)", Err: fmt.Errorf("crontab: permission denied")},
	)
	var buf bytes.Buffer
	mgr := NewManager(mock, &buf)
	if err := mgr.Unschedule(context.Background(), "myapp"); err == nil {
		t.Fatal("a failed crontab read must abort")
	}
	for _, c := range mock.Calls {
		if strings.Contains(c, "crontab -r") {
			t.Errorf("crontab -r executed: %s", c)
		}
	}
}

// TestRemove_AggregatesStepFailures is the T30 regression: Remove used to
// ignore every failure and print "removed" — a live service could keep
// deploying after the operator believed it disabled.
func TestRemove_AggregatesStepFailures(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "id -u", Output: "0"},
		ssh.MockCommand{Match: "systemctl stop", Err: fmt.Errorf("systemctl: connection refused")},
		ssh.MockCommand{Match: "systemctl disable", Output: ""},
		ssh.MockCommand{Match: "rm -f --", Output: ""},
		ssh.MockCommand{Match: "systemctl daemon-reload", Output: ""},
		ssh.MockCommand{Match: "raw=$(crontab -l 2>&1)", Output: ""},
		ssh.MockCommand{Match: "mkdir /deployments/caddy/.lock", Output: ""},
		ssh.MockCommand{Match: "cat /deployments/caddy/Caddyfile", Output: "{\n\tadmin 0.0.0.0:2019\n}\n"},
		ssh.MockCommand{Match: "a=$(docker exec caddy md5sum", Output: "TEPLOY_CADDY_OK"},
		ssh.MockCommand{Match: "docker exec caddy caddy reload", Output: ""},
		ssh.MockCommand{Match: "rmdir /deployments/caddy/.lock", Output: ""},
	)
	var buf bytes.Buffer
	mgr := NewManager(mock, &buf)
	err := mgr.Remove(context.Background(), "myapp")
	if err == nil {
		t.Fatal("a failed removal step must surface, not print success")
	}
	if !strings.Contains(err.Error(), "incomplete") || !strings.Contains(err.Error(), "stopping the webhook service") {
		t.Fatalf("error must name the failed step: %v", err)
	}
}

// TestSetup_RejectsWhitespaceWrappedSecret is the T32 regression: the
// stored secret is HMAC-verified verbatim at both ends; a whitespace-wrapped
// secret would sign different bytes than the trimmed end expects.
func TestSetup_RejectsWhitespaceWrappedSecret(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4")
	var buf bytes.Buffer
	mgr := NewManager(mock, &buf)
	err := mgr.Setup(context.Background(), Config{App: "myapp", Branch: "main", Secret: " spaced ", TeployBinaryPath: "/deployments/.bin/teploy"})
	if err == nil || !strings.Contains(err.Error(), "whitespace") {
		t.Fatalf("whitespace-wrapped secret must be rejected at setup: %v", err)
	}
}
