package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadApp_HealthTimeoutIntervalYAML(t *testing.T) {
	dir := t.TempDir()
	content := `app: myapp
domain: myapp.com
health:
  path: /healthz
  timeout_seconds: 120
  interval_seconds: 5
`
	if err := os.WriteFile(filepath.Join(dir, "teploy.yml"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadApp(dir)
	if err != nil {
		t.Fatalf("LoadApp: %v", err)
	}
	if cfg.Health.Path != "/healthz" {
		t.Errorf("Health.Path = %q, want /healthz", cfg.Health.Path)
	}
	if cfg.Health.TimeoutSeconds != 120 {
		t.Errorf("Health.TimeoutSeconds = %d, want 120", cfg.Health.TimeoutSeconds)
	}
	if cfg.Health.IntervalSeconds != 5 {
		t.Errorf("Health.IntervalSeconds = %d, want 5", cfg.Health.IntervalSeconds)
	}
}

func TestLoadApp_HealthTimeoutUnsetDefaultsToZero(t *testing.T) {
	dir := t.TempDir()
	content := `app: myapp
domain: myapp.com
`
	if err := os.WriteFile(filepath.Join(dir, "teploy.yml"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadApp(dir)
	if err != nil {
		t.Fatalf("LoadApp: %v", err)
	}
	// Zero here means deploy.HealthConfig.withDefaults() fills in 30s/1s —
	// this is what keeps existing teploy.yml files behavior-unchanged.
	if cfg.Health.TimeoutSeconds != 0 {
		t.Errorf("Health.TimeoutSeconds = %d, want 0 (unset)", cfg.Health.TimeoutSeconds)
	}
	if cfg.Health.IntervalSeconds != 0 {
		t.Errorf("Health.IntervalSeconds = %d, want 0 (unset)", cfg.Health.IntervalSeconds)
	}
}

func TestLoadApp_HealthTimeoutSecondsNegativeRejected(t *testing.T) {
	dir := t.TempDir()
	content := `app: myapp
domain: myapp.com
health:
  timeout_seconds: -1
`
	if err := os.WriteFile(filepath.Join(dir, "teploy.yml"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadApp(dir)
	if err == nil {
		t.Fatal("expected error for negative health.timeout_seconds")
	}
	if !strings.Contains(err.Error(), "timeout_seconds") {
		t.Errorf("error should mention timeout_seconds, got: %v", err)
	}
}

func TestLoadApp_HealthIntervalSecondsNegativeRejected(t *testing.T) {
	dir := t.TempDir()
	content := `app: myapp
domain: myapp.com
health:
  interval_seconds: -1
`
	if err := os.WriteFile(filepath.Join(dir, "teploy.yml"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadApp(dir)
	if err == nil {
		t.Fatal("expected error for negative health.interval_seconds")
	}
	if !strings.Contains(err.Error(), "interval_seconds") {
		t.Errorf("error should mention interval_seconds, got: %v", err)
	}
}
