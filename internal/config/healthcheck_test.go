package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadApp_HealthcheckYAML verifies that a healthcheck block in YAML
// parses into ProcessHealth and round-trips through validation.
func TestLoadApp_HealthcheckYAML(t *testing.T) {
	dir := t.TempDir()
	content := `app: myapp
domain: myapp.com
processes:
  web: "node server.js"
  worker: "node worker.js"
healthcheck:
  worker:
    disable: true
`
	if err := os.WriteFile(filepath.Join(dir, "teploy.yml"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadApp(dir)
	if err != nil {
		t.Fatalf("LoadApp failed: %v", err)
	}
	if len(cfg.Healthcheck) != 1 {
		t.Fatalf("expected 1 healthcheck entry, got %d", len(cfg.Healthcheck))
	}
	worker, ok := cfg.Healthcheck["worker"]
	if !ok {
		t.Fatalf("expected healthcheck['worker'] to exist")
	}
	if !worker.Disable {
		t.Errorf("expected worker.disable=true, got false")
	}
	// Web has no entry — should be the zero ProcessHealth.
	if _, ok := cfg.Healthcheck["web"]; ok {
		t.Errorf("expected no healthcheck entry for 'web'")
	}
}

// TestLoadApp_HealthcheckTOML mirrors the YAML test for TOML configs.
func TestLoadApp_HealthcheckTOML(t *testing.T) {
	dir := t.TempDir()
	content := `app = "myapp"
domain = "myapp.com"

[processes]
web = "node server.js"
worker = "node worker.js"

[healthcheck.worker]
disable = true
`
	if err := os.WriteFile(filepath.Join(dir, "teploy.toml"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadApp(dir)
	if err != nil {
		t.Fatalf("LoadApp failed: %v", err)
	}
	if !cfg.Healthcheck["worker"].Disable {
		t.Errorf("expected worker.disable=true from TOML")
	}
}

// TestLoadApp_HealthcheckUnknownProcess ensures a healthcheck key that
// doesn't correspond to a process name fails validation. This catches
// typos like `healthcheck.wokrer.disable: true`.
func TestLoadApp_HealthcheckUnknownProcess(t *testing.T) {
	dir := t.TempDir()
	content := `app: myapp
domain: myapp.com
processes:
  web: "node server.js"
  worker: "node worker.js"
healthcheck:
  wokrer:
    disable: true
`
	if err := os.WriteFile(filepath.Join(dir, "teploy.yml"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := LoadApp(dir)
	if err == nil {
		t.Fatal("expected error for healthcheck key pointing at unknown process")
	}
	if !strings.Contains(err.Error(), "unknown process") {
		t.Errorf("expected 'unknown process' in error, got: %v", err)
	}
}

// TestLoadApp_HealthcheckInvalidName ensures a healthcheck key with
// invalid characters fails validation before the unknown-process check.
func TestLoadApp_HealthcheckInvalidName(t *testing.T) {
	dir := t.TempDir()
	content := `app: myapp
domain: myapp.com
processes:
  web: "node server.js"
healthcheck:
  "BAD NAME":
    disable: true
`
	if err := os.WriteFile(filepath.Join(dir, "teploy.yml"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := LoadApp(dir)
	if err == nil {
		t.Fatal("expected error for invalid healthcheck key name")
	}
	if !strings.Contains(err.Error(), "lowercase alphanumeric") {
		t.Errorf("expected 'lowercase alphanumeric' in error, got: %v", err)
	}
}

// TestLoadApp_HealthcheckOverlayMerge verifies a destination overlay can
// add/override healthcheck entries on top of the base config.
func TestLoadApp_HealthcheckOverlayMerge(t *testing.T) {
	dir := t.TempDir()
	base := `app: myapp
domain: myapp.com
processes:
  web: "node server.js"
  worker: "node worker.js"
healthcheck:
  worker:
    disable: true
`
	overlay := `healthcheck:
  web:
    disable: true
`
	if err := os.WriteFile(filepath.Join(dir, "teploy.yml"), []byte(base), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "teploy.staging.yml"), []byte(overlay), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadAppWithDestination(dir, "staging")
	if err != nil {
		t.Fatalf("LoadAppWithDestination failed: %v", err)
	}
	if !cfg.Healthcheck["worker"].Disable {
		t.Errorf("expected base worker healthcheck preserved")
	}
	if !cfg.Healthcheck["web"].Disable {
		t.Errorf("expected overlay web healthcheck applied")
	}
}
