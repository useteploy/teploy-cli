package cli

import (
	"testing"
	"time"

	"github.com/useteploy/teploy/internal/config"
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
