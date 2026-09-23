package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadApp_WithDrainSeconds: drain_seconds parses from teploy.yml and
// reaches AppConfig (the C03 request-drain policy).
func TestLoadApp_WithDrainSeconds(t *testing.T) {
	dir := t.TempDir()
	content := `app: myapp
domain: myapp.com
stop_timeout: 30
drain_seconds: 5
`
	if err := os.WriteFile(filepath.Join(dir, "teploy.yml"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadApp(dir)
	if err != nil {
		t.Fatalf("LoadApp: %v", err)
	}
	if cfg.DrainSeconds != 5 {
		t.Errorf("expected drain_seconds 5, got %d", cfg.DrainSeconds)
	}
}

// TestDrainSecondsValidation: the window is bounded — a negative or
// absurd value is rejected at config load, not mid-deploy.
func TestDrainSecondsValidation(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		value int
		ok    bool
	}{
		{0, true}, {1, true}, {600, true},
		{-1, false}, {601, false},
	} {
		content := "app: myapp\ndomain: myapp.com\ndrain_seconds: " + fmt.Sprint(tc.value) + "\n"
		if err := os.WriteFile(filepath.Join(dir, "teploy.yml"), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
		_, err := LoadApp(dir)
		if tc.ok && err != nil {
			t.Errorf("drain_seconds %d must validate, got %v", tc.value, err)
		}
		if !tc.ok && (err == nil || !strings.Contains(err.Error(), "drain_seconds")) {
			t.Errorf("drain_seconds %d must be rejected naming the field, got %v", tc.value, err)
		}
	}
}

// TestDrainSecondsOverlayAndManifest: a destination overlay carries the
// drain policy, and the effective-config manifest (drift identity)
// includes it — a deploy with a changed drain window is a config change.
func TestDrainSecondsOverlayAndManifest(t *testing.T) {
	base := &AppConfig{App: "myapp", Domain: "myapp.com", Image: "img:1"}
	overlay := &AppConfig{DrainSeconds: 7}
	merged := *base
	mergeConfigs(&merged, overlay)
	if merged.DrainSeconds != 7 {
		t.Fatalf("overlay must carry drain_seconds, got %d", merged.DrainSeconds)
	}

	manifest, _, err := NormalizeAndDigest(&AppConfig{App: "myapp", Domain: "myapp.com", Image: "img:1", DrainSeconds: 7}, "img:1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(manifest), `"drain_seconds":7`) {
		t.Errorf("manifest must carry drain_seconds for drift identity:\n%s", manifest)
	}
}
