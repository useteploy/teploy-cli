package config

import (
	"encoding/json"
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
	if cfg.KeepVersions == nil || *cfg.KeepVersions != 3 {
		t.Errorf("KeepVersions = %v, want 3", cfg.KeepVersions)
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
	if cfg.KeepVersions == nil || *cfg.KeepVersions != 2 {
		t.Errorf("KeepVersions = %v, want 2", cfg.KeepVersions)
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

// D3: unset means the default 5 (not explicit), any set value — including 0,
// which keeps everything — is the operator's choice.
func TestEffectiveKeepVersions(t *testing.T) {
	if keep, explicit := EffectiveKeepVersions(nil); keep != DefaultKeepVersions || explicit {
		t.Errorf("unset = %d, explicit %v, want %d not explicit", keep, explicit, DefaultKeepVersions)
	}
	three := 3
	if keep, explicit := EffectiveKeepVersions(&three); keep != 3 || !explicit {
		t.Errorf("3 = %d, explicit %v", keep, explicit)
	}
	zero := 0
	if keep, explicit := EffectiveKeepVersions(&zero); keep != 0 || !explicit {
		t.Errorf("0 = %d, explicit %v, want 0 explicit (keep everything)", keep, explicit)
	}
}

// The applied manifest must record unset and N exactly as before (existing
// digests hold) and distinguish an explicit 0 with its own key.
func TestManifestRecordsKeepVersions(t *testing.T) {
	manifestFor := func(kv *int) map[string]any {
		cfg := &AppConfig{App: "myapp", Domain: "myapp.com", Image: "example/myapp:1"}
		cfg.KeepVersions = kv
		raw, _, err := NormalizeAndDigest(cfg, "example/myapp:1")
		if err != nil {
			t.Fatalf("NormalizeAndDigest: %v", err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		return m["container"].(map[string]any)
	}
	if _, ok := manifestFor(nil)["keep_versions"].(float64); !ok || manifestFor(nil)["keep_versions"].(float64) != 0 {
		t.Errorf("unset must record 0 (digest compatibility), got %v", manifestFor(nil)["keep_versions"])
	}
	if _, ok := manifestFor(nil)["keep_all_versions"]; ok {
		t.Error("unset must not record keep_all_versions")
	}
	three := 3
	if manifestFor(&three)["keep_versions"].(float64) != 3 {
		t.Errorf("3 must record 3, got %v", manifestFor(&three)["keep_versions"])
	}
	zero := 0
	if manifestFor(&zero)["keep_versions"].(float64) != 0 {
		t.Errorf("explicit 0 must still record 0, got %v", manifestFor(&zero)["keep_versions"])
	}
	if manifestFor(&zero)["keep_all_versions"] != true {
		t.Errorf("explicit 0 must record keep_all_versions true, got %v", manifestFor(&zero)["keep_all_versions"])
	}
}
