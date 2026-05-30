package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadApp_KeepVersionsYAML(t *testing.T) {
	dir := t.TempDir()
	content := `app: myapp
domain: myapp.com
keep_versions: 3
`
	if err := os.WriteFile(filepath.Join(dir, "teploy.yml"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadApp(dir)
	if err != nil {
		t.Fatalf("LoadApp: %v", err)
	}
	if cfg.KeepVersions != 3 {
		t.Errorf("KeepVersions = %d, want 3", cfg.KeepVersions)
	}
}

func TestLoadApp_KeepVersionsTOML(t *testing.T) {
	dir := t.TempDir()
	content := `app = "myapp"
domain = "myapp.com"
keep_versions = 2
`
	if err := os.WriteFile(filepath.Join(dir, "teploy.toml"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadApp(dir)
	if err != nil {
		t.Fatalf("LoadApp: %v", err)
	}
	if cfg.KeepVersions != 2 {
		t.Errorf("KeepVersions = %d, want 2", cfg.KeepVersions)
	}
}

func TestLoadApp_KeepVersionsNegativeRejected(t *testing.T) {
	dir := t.TempDir()
	content := `app: myapp
domain: myapp.com
keep_versions: -1
`
	if err := os.WriteFile(filepath.Join(dir, "teploy.yml"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadApp(dir)
	if err == nil {
		t.Fatal("expected error for negative keep_versions")
	}
	if !strings.Contains(err.Error(), "keep_versions") {
		t.Errorf("error should mention keep_versions, got: %v", err)
	}
}

// TestLoadApp_KeepVersionsRejectedForStatic guards against silent no-op:
// static deploys use keep_releases, not keep_versions.
func TestLoadApp_KeepVersionsRejectedForStatic(t *testing.T) {
	dir := t.TempDir()
	content := `app: myapp
domain: myapp.com
type: static
source: ./dist
keep_versions: 2
`
	if err := os.WriteFile(filepath.Join(dir, "teploy.yml"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadApp(dir)
	if err == nil {
		t.Fatal("expected error: keep_versions on static deploy")
	}
	if !strings.Contains(err.Error(), "container-deploy only") {
		t.Errorf("expected 'container-deploy only' in error, got: %v", err)
	}
}
