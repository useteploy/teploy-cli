package deploy

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy/internal/ssh"
)

func TestDeploy_EmptyVersion(t *testing.T) {
	mock := ssh.NewMockExecutor("server1")
	d := NewDeployer(mock, &bytes.Buffer{})

	err := d.Deploy(context.Background(), Config{
		App:    "myapp",
		Domain: "myapp.com",
		Image:  "myapp:latest",
		// Version intentionally empty
	})
	if err == nil {
		t.Fatal("expected error for empty version")
	}
	if !strings.Contains(err.Error(), "required") {
		t.Errorf("expected 'required' in error, got: %v", err)
	}
}

func TestDeploy_EmptyApp(t *testing.T) {
	mock := ssh.NewMockExecutor("server1")
	d := NewDeployer(mock, &bytes.Buffer{})

	err := d.Deploy(context.Background(), Config{
		Domain:  "myapp.com",
		Image:   "myapp:latest",
		Version: "abc123",
	})
	if err == nil {
		t.Fatal("expected error for empty app")
	}
}

func TestDeploy_ContextCancellation(t *testing.T) {
	mock := ssh.NewMockExecutor("server1",
		ssh.MockCommand{Match: "mkdir", Output: ""},
		ssh.MockCommand{Match: "mkdir", Output: ""}, // acquire lock
		ssh.MockCommand{Match: "UPLOAD:", Output: ""},
		ssh.MockCommand{Match: "rm -rf", Output: ""}, // release lock
		ssh.MockCommand{Match: "cat /deployments", Output: ""},
		ssh.MockCommand{Match: "ss -tln", Output: ""},
	)

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Millisecond)
	defer cancel()

	time.Sleep(5 * time.Millisecond) // let context expire

	d := NewDeployer(mock, &bytes.Buffer{})
	err := d.Deploy(ctx, Config{
		App:     "myapp",
		Domain:  "myapp.com",
		Image:   "myapp:latest",
		Version: "abc123",
	})
	// Should fail (context cancelled) not panic.
	if err == nil {
		t.Error("expected error from cancelled context")
	}
}

func TestHealthConfig_Defaults(t *testing.T) {
	h := HealthConfig{}
	d := h.withDefaults()

	if d.Path != "/health" {
		t.Errorf("default path should be /health, got %q", d.Path)
	}
	if d.Timeout != 30*time.Second {
		t.Errorf("default timeout should be 30s, got %v", d.Timeout)
	}
	if d.Interval != time.Second {
		t.Errorf("default interval should be 1s, got %v", d.Interval)
	}
}

func TestHealthConfig_CustomValues(t *testing.T) {
	h := HealthConfig{
		Path:     "/up",
		Timeout:  60 * time.Second,
		Interval: 2 * time.Second,
	}
	d := h.withDefaults()

	if d.Path != "/up" {
		t.Errorf("custom path should be preserved, got %q", d.Path)
	}
	if d.Timeout != 60*time.Second {
		t.Errorf("custom timeout should be preserved, got %v", d.Timeout)
	}
}

func TestSortedProcessNames(t *testing.T) {
	procs := map[string]string{
		"worker": "npm run worker",
		"web":    "npm start",
		"cron":   "npm run cron",
		"mailer": "npm run mailer",
	}

	names := sortedProcessNames(procs)
	if names[0] != "web" {
		t.Errorf("web should be first, got %q", names[0])
	}
	// Rest should be alphabetical.
	if names[1] != "cron" {
		t.Errorf("cron should be second, got %q", names[1])
	}
	if names[2] != "mailer" {
		t.Errorf("mailer should be third, got %q", names[2])
	}
	if names[3] != "worker" {
		t.Errorf("worker should be fourth, got %q", names[3])
	}
}
