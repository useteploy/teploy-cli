package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/docker"
	"github.com/useteploy/teploy/internal/ssh"
)

func TestHealthConfigFrom(t *testing.T) {
	got := healthConfigFrom(config.AppHealthConfig{
		Path:            "/healthz",
		TimeoutSeconds:  120,
		IntervalSeconds: 5,
	})
	if got.Path != "/healthz" {
		t.Errorf("Path = %q, want /healthz", got.Path)
	}
	if got.Timeout != 120*time.Second {
		t.Errorf("Timeout = %s, want 120s", got.Timeout)
	}
	if got.Interval != 5*time.Second {
		t.Errorf("Interval = %s, want 5s", got.Interval)
	}
}

func TestHealthConfigFrom_UnsetFieldsStayZero(t *testing.T) {
	got := healthConfigFrom(config.AppHealthConfig{Path: "/health"})
	if got.Timeout != 0 {
		t.Errorf("Timeout = %s, want 0 (so HealthConfig.withDefaults applies 30s)", got.Timeout)
	}
	if got.Interval != 0 {
		t.Errorf("Interval = %s, want 0 (so HealthConfig.withDefaults applies 1s)", got.Interval)
	}
}

// TestEnsureImage_LocalPresent verifies a pre-built image already on the
// server is used as-is, with no docker pull attempted (the bug: pulling
// unconditionally fails for images that live in no registry).
func TestEnsureImage_LocalPresent(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "docker image inspect", Output: "exists\n"},
		ssh.MockCommand{Match: "docker pull", Output: ""},
	)
	dk := docker.NewClient(mock)

	var out bytes.Buffer
	if err := ensureImage(context.Background(), dk, "myapp:local", &out); err != nil {
		t.Fatalf("ensureImage: %v", err)
	}

	for _, c := range mock.Calls {
		if strings.HasPrefix(c, "docker pull") {
			t.Fatalf("pull attempted for a locally-present image: %q", c)
		}
	}
	if !strings.Contains(out.String(), "Using local image myapp:local") {
		t.Errorf("output = %q, want it to report using the local image", out.String())
	}
}

// TestEnsureImage_Missing verifies that an image absent from the local cache
// is pulled from its registry (unchanged behavior for real registry images).
func TestEnsureImage_Missing(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "docker image inspect", Output: "missing\n"},
		ssh.MockCommand{Match: "docker pull", Output: ""},
	)
	dk := docker.NewClient(mock)

	var out bytes.Buffer
	if err := ensureImage(context.Background(), dk, "registry.example.com/app:v1", &out); err != nil {
		t.Fatalf("ensureImage: %v", err)
	}

	pulled := false
	for _, c := range mock.Calls {
		if strings.HasPrefix(c, "docker pull") {
			pulled = true
		}
	}
	if !pulled {
		t.Error("expected a docker pull for an image absent from the local cache")
	}
}
