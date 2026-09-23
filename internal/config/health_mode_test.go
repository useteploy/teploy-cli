package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeApp(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "teploy.yml"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestLoadApp_HealthModeParsed(t *testing.T) {
	for _, mode := range []string{"http", "tcp", "auto"} {
		dir := writeApp(t, "app: myapp\ndomain: myapp.com\nhealth:\n  mode: "+mode+"\n")
		cfg, err := LoadApp(dir)
		if err != nil {
			t.Fatalf("mode %s: LoadApp: %v", mode, err)
		}
		if cfg.Health.Mode != mode {
			t.Errorf("mode %s: Health.Mode = %q", mode, cfg.Health.Mode)
		}
	}
}

func TestLoadApp_HealthModeEmptyStaysEmpty(t *testing.T) {
	// Absent mode means "" — the documented `auto` compat default, applied
	// at deploy time (HealthConfig.withDefaults), so existing teploy.yml
	// files are behavior-unchanged.
	dir := writeApp(t, "app: myapp\ndomain: myapp.com\nhealth:\n  path: /healthz\n")
	cfg, err := LoadApp(dir)
	if err != nil {
		t.Fatalf("LoadApp: %v", err)
	}
	if cfg.Health.Mode != "" {
		t.Errorf("Health.Mode = %q, want empty (auto compat)", cfg.Health.Mode)
	}
}

func TestLoadApp_HealthModeUnknownRejected(t *testing.T) {
	dir := writeApp(t, "app: myapp\ndomain: myapp.com\nhealth:\n  mode: grpc\n")
	_, err := LoadApp(dir)
	if err == nil {
		t.Fatal("expected error for unknown health.mode")
	}
	if !strings.Contains(err.Error(), "health.mode") {
		t.Errorf("error should mention health.mode, got: %v", err)
	}
	if !strings.Contains(err.Error(), "http") || !strings.Contains(err.Error(), "tcp") || !strings.Contains(err.Error(), "auto") {
		t.Errorf("error should name the valid modes, got: %v", err)
	}
}

// tcp mode dials the port; a configured path is a field nothing fetches —
// reject it at load rather than deploying a config that lies about what
// the gate does.
func TestLoadApp_HealthTCPModeWithPathRejected(t *testing.T) {
	dir := writeApp(t, "app: myapp\ndomain: myapp.com\nhealth:\n  mode: tcp\n  path: /healthz\n")
	_, err := LoadApp(dir)
	if err == nil {
		t.Fatal("expected error for health.path set under mode tcp")
	}
	if !strings.Contains(err.Error(), "health.path") || !strings.Contains(err.Error(), "tcp") {
		t.Errorf("error should name path and tcp mode, got: %v", err)
	}
}

// http mode without a path is fine: the /health default applies.
func TestLoadApp_HealthHTTPModeWithoutPathAccepted(t *testing.T) {
	dir := writeApp(t, "app: myapp\ndomain: myapp.com\nhealth:\n  mode: http\n")
	if _, err := LoadApp(dir); err != nil {
		t.Fatalf("http mode with no path should default, got: %v", err)
	}
}

func TestNormalizedHealth_IncludesMode(t *testing.T) {
	if got := normalizedHealth(AppHealthConfig{})["mode"]; got != "auto" {
		t.Errorf("normalized mode default = %v, want auto", got)
	}
	if got := normalizedHealth(AppHealthConfig{Mode: "tcp"})["mode"]; got != "tcp" {
		t.Errorf("normalized mode = %v, want tcp", got)
	}
}

// A destination overlay that only sets health.mode must still replace the
// whole health block (F57 semantics) — a base path must not survive into a
// tcp-mode overlay.
func TestMergeConfigs_HealthModeAloneReplacesBlock(t *testing.T) {
	base := &AppConfig{App: "myapp", Domain: "myapp.com", Health: AppHealthConfig{Path: "/healthz", TimeoutSeconds: 60}}
	overlay := &AppConfig{Health: AppHealthConfig{Mode: "tcp"}}
	mergeConfigs(base, overlay)
	if base.Health.Mode != "tcp" || base.Health.Path != "" || base.Health.TimeoutSeconds != 0 {
		t.Errorf("overlay health must replace the whole block, got %+v", base.Health)
	}
}
