package cli

import (
	"bytes"
	"context"
	"errors"
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

func pullAttempted(mock *ssh.MockExecutor) bool {
	for _, c := range mock.Calls {
		if strings.HasPrefix(c, "docker pull") {
			return true
		}
	}
	return false
}

// TestEnsureImage_LocalPresent verifies a mutable reference is pulled even when
// a copy is already on the server. Skipping the pull here is what let an app
// serve a five-day-old `:latest` while every deploy reported success.
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

	if !pullAttempted(mock) {
		t.Error("no pull attempted for a locally-present mutable tag")
	}
	if !strings.Contains(out.String(), "Image pulled") {
		t.Errorf("output = %q, want it to report a fresh pull", out.String())
	}
}

// TestEnsureImage_LocalPresentUntaggedRegistry is the shape that hit production:
// a registry-qualified image with no tag, which Docker resolves to `:latest`,
// already present in the server's cache. It must still be pulled.
func TestEnsureImage_LocalPresentUntaggedRegistry(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "docker image inspect", Output: "exists\n"},
		ssh.MockCommand{Match: "docker pull", Output: ""},
	)
	dk := docker.NewClient(mock)

	var out bytes.Buffer
	if err := ensureImage(context.Background(), dk, "registry.example.com:5000/tyler/app", &out); err != nil {
		t.Fatalf("ensureImage: %v", err)
	}

	if !pullAttempted(mock) {
		t.Error("no pull attempted for a locally-present untagged registry image")
	}
}

// TestEnsureImage_LocalPresentDigestPinned verifies the one reference that is
// safe to serve from cache: a content-addressed digest can never move.
func TestEnsureImage_LocalPresentDigestPinned(t *testing.T) {
	image := "registry.example.com/app@sha256:" + strings.Repeat("a", 64)
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "docker image inspect", Output: "exists\n"},
		ssh.MockCommand{Match: "docker pull", Output: ""},
	)
	dk := docker.NewClient(mock)

	var out bytes.Buffer
	if err := ensureImage(context.Background(), dk, image, &out); err != nil {
		t.Fatalf("ensureImage: %v", err)
	}

	if pullAttempted(mock) {
		t.Error("pull attempted for a locally-present digest-pinned image")
	}
	if !strings.Contains(out.String(), "digest-pinned") {
		t.Errorf("output = %q, want it to report the digest-pinned skip", out.String())
	}
}

// TestEnsureImage_PullFailsLocalPresent covers the out-of-band case the old
// unconditional skip existed to protect: an image built or `docker load`ed on
// the server that lives in no registry. The deploy still proceeds, but the
// output must say the local copy may be stale rather than reporting a pull.
func TestEnsureImage_PullFailsLocalPresent(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "docker image inspect", Output: "exists\n"},
		ssh.MockCommand{Match: "docker pull", Err: errors.New("no such repository")},
	)
	dk := docker.NewClient(mock)

	var out bytes.Buffer
	if err := ensureImage(context.Background(), dk, "myapp:local", &out); err != nil {
		t.Fatalf("ensureImage: %v", err)
	}

	if !pullAttempted(mock) {
		t.Error("no pull attempted before falling back to the local copy")
	}
	got := out.String()
	if !strings.Contains(got, "WARNING") || !strings.Contains(got, "Falling back to the local copy") {
		t.Errorf("output = %q, want a warning that the local copy may be stale", got)
	}
	if strings.Contains(got, "Image pulled") {
		t.Errorf("output = %q, must not claim a successful pull", got)
	}
}

// TestEnsureImage_PullFailsNothingLocal verifies a failed pull with no local
// copy is still an error.
func TestEnsureImage_PullFailsNothingLocal(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "docker image inspect", Output: "missing\n"},
		ssh.MockCommand{Match: "docker pull", Err: errors.New("unauthorized")},
	)
	dk := docker.NewClient(mock)

	var out bytes.Buffer
	if err := ensureImage(context.Background(), dk, "registry.example.com/app:v1", &out); err == nil {
		t.Fatal("ensureImage: expected an error when the pull fails and nothing is cached")
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

	if !pullAttempted(mock) {
		t.Error("expected a docker pull for an image absent from the local cache")
	}
}

func TestIsDigestPinned(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	tests := []struct {
		image string
		want  bool
	}{
		{"registry.example.com/app@" + digest, true},
		{"app@" + digest, true},
		{"registry.example.com/app:v1", false},
		{"registry.example.com:5000/tyler/app", false},
		{"app:latest", false},
		{"app:511a079", false},
		{"app", false},
		{"app@", false},
		{"app@sha256:", false},
	}
	for _, tt := range tests {
		if got := isDigestPinned(tt.image); got != tt.want {
			t.Errorf("isDigestPinned(%q) = %v, want %v", tt.image, got, tt.want)
		}
	}
}
