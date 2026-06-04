package autodeploy

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/ssh"
)

func TestSetup(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "mkdir -p /deployments/myapp", Output: ""},
		ssh.MockCommand{Match: "UPLOAD:", Output: ""},
		ssh.MockCommand{Match: "id -u", Output: "0"},
		ssh.MockCommand{Match: "systemctl daemon-reload", Output: ""},
		ssh.MockCommand{Match: "systemctl enable", Output: ""},
		ssh.MockCommand{Match: "systemctl restart", Output: ""},
	)

	var buf bytes.Buffer
	mgr := NewManager(mock, &buf)
	err := mgr.Setup(context.Background(), Config{
		App:    "myapp",
		Branch: "main",
		Secret: "mysecret",
	})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "Installing auto-deploy script") {
		t.Error("expected install message")
	}
	if !strings.Contains(output, "webhook listener") {
		t.Error("expected webhook listener message")
	}

	// Verify files were uploaded.
	if len(mock.Files) < 3 {
		t.Errorf("expected at least 3 file uploads, got %d", len(mock.Files))
	}

	// Verify deploy script was uploaded.
	script, ok := mock.Files["/deployments/myapp/autodeploy.sh"]
	if !ok {
		t.Fatal("deploy script not uploaded")
	}
	if !strings.Contains(string(script), "BRANCH=\"main\"") {
		t.Error("script should contain branch")
	}
}

func TestSetup_DefaultBranch(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "mkdir", Output: ""},
		ssh.MockCommand{Match: "UPLOAD:", Output: ""},
		ssh.MockCommand{Match: "id -u", Output: "0"},
		ssh.MockCommand{Match: "systemctl", Output: ""},
	)

	var buf bytes.Buffer
	mgr := NewManager(mock, &buf)
	err := mgr.Setup(context.Background(), Config{
		App:    "myapp",
		Secret: "test-secret",
	})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}

	script := string(mock.Files["/deployments/myapp/autodeploy.sh"])
	if !strings.Contains(script, "BRANCH=\"main\"") {
		t.Error("default branch should be main")
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
	)

	var buf bytes.Buffer
	mgr := NewManager(mock, &buf)
	if err := mgr.Remove(context.Background(), "myapp"); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	if !strings.Contains(buf.String(), "Auto-deploy removed") {
		t.Error("expected removal message")
	}
}

func TestGenerateScript(t *testing.T) {
	script := generateScript(Config{
		App:    "myapp",
		Branch: "develop",
	})

	for _, want := range []string{
		`APP="myapp"`,
		`BRANCH="develop"`,
		"git fetch origin",
		"docker build",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q", want)
		}
	}
}

func TestGenerateService(t *testing.T) {
	svc := generateService("teploy-webhook-myapp", "/deployments/myapp/webhook-listener.sh")

	if !strings.Contains(svc, "teploy-webhook-myapp") {
		t.Error("service should contain name")
	}
	if !strings.Contains(svc, "Restart=always") {
		t.Error("service should restart always")
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
		ssh.MockCommand{Match: "(crontab", Output: ""},
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
		ssh.MockCommand{Match: "(crontab", Output: ""},
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
		"--name \"$CONTAINER\"",   // preserve same name to avoid Caddy reconfig
		"date +%s",                // new version timestamp
		"$(ts) [redeploy]",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("scheduled redeploy script missing %q", want)
		}
	}
}
