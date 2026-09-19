package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestOverlayStrictExplicitClearing is F57's opt-in semantics: under
// OverlayOptions{Strict: true}, a destination overlay that NAMES a
// map/list key with an empty value clears the base's field; the default
// (non-strict) behavior is untouched for compatibility.
func TestOverlayStrictExplicitClearing(t *testing.T) {
	dir := t.TempDir()
	write := func(t *testing.T, name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	write(t, "teploy.yml", `app: myapp
server: prod
domain: myapp.com
env:
  KEEP_ME: "yes"
  REPLACE_ME: "base"
publish:
  - "3001:3001"
volumes:
  data: /var/lib/data
`)
	write(t, "teploy.staging.yml", `app: myapp
env:
  REPLACE_ME: "staging"
publish: []
volumes: {}
`)

	t.Run("default keeps base (compat)", func(t *testing.T) {
		cfg, err := LoadAppWithDestination(dir, "staging", OverlayOptions{})
		if err != nil {
			t.Fatalf("LoadAppWithDestination: %v", err)
		}
		if cfg.Env["KEEP_ME"] != "yes" {
			t.Errorf("base env key must survive a default merge, got env=%v", cfg.Env)
		}
		if cfg.Env["REPLACE_ME"] != "staging" {
			t.Errorf("overlay env key must win, got %q", cfg.Env["REPLACE_ME"])
		}
		if len(cfg.Publish) != 1 || cfg.Publish[0] != "3001:3001" {
			t.Errorf("empty overlay publish must be ignored by default, got %v", cfg.Publish)
		}
		if len(cfg.Volumes) != 1 {
			t.Errorf("empty overlay volumes must be ignored by default, got %v", cfg.Volumes)
		}
	})

	t.Run("strict clears explicitly-empty fields", func(t *testing.T) {
		cfg, err := LoadAppWithDestination(dir, "staging", OverlayOptions{Strict: true})
		if err != nil {
			t.Fatalf("LoadAppWithDestination: %v", err)
		}
		// env was named with a NON-empty value: key-merge as before.
		if cfg.Env["REPLACE_ME"] != "staging" {
			t.Errorf("overlay env key must still win, got %q", cfg.Env["REPLACE_ME"])
		}
		if cfg.Env["KEEP_ME"] != "yes" {
			t.Errorf("non-empty env overlay key-merges (replace semantics not introduced), got env=%v", cfg.Env)
		}
		// publish/volumes were named EMPTY: cleared.
		if len(cfg.Publish) != 0 {
			t.Errorf("explicitly-empty publish must clear the base list, got %v", cfg.Publish)
		}
		if len(cfg.Volumes) != 0 {
			t.Errorf("explicitly-empty volumes must clear the base map, got %v", cfg.Volumes)
		}
	})
}

// A null overlay key (`env:` with no value) also clears under strict —
// presence, not the specific empty spelling, is the signal.
func TestOverlayStrictNullKeyClears(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "teploy.yml"), []byte("app: myapp\nserver: prod\ndomain: myapp.com\nenv:\n  A: \"1\"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "teploy.prod.yml"), []byte("app: myapp\nenv:\n"), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadAppWithDestination(dir, "prod", OverlayOptions{Strict: true})
	if err != nil {
		t.Fatalf("LoadAppWithDestination: %v", err)
	}
	if len(cfg.Env) != 0 {
		t.Errorf("a null env: key must clear under strict mode, got %v", cfg.Env)
	}
	// And must NOT clear in the default mode.
	cfg, err = LoadAppWithDestination(dir, "prod", OverlayOptions{})
	if err != nil {
		t.Fatalf("LoadAppWithDestination default: %v", err)
	}
	if cfg.Env["A"] != "1" {
		t.Errorf("default mode must keep base env, got %v", cfg.Env)
	}
}

// TOML overlays get the same semantics (`publish = []` clears).
func TestOverlayStrictTOML(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "teploy.yml"), []byte("app: myapp\nserver: prod\ndomain: myapp.com\npublish:\n  - \"3001:3001\"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "teploy.prod.toml"), []byte("app = \"myapp\"\npublish = []\n"), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadAppWithDestination(dir, "prod", OverlayOptions{Strict: true})
	if err != nil {
		t.Fatalf("LoadAppWithDestination: %v", err)
	}
	if len(cfg.Publish) != 0 {
		t.Errorf("TOML empty publish must clear under strict, got %v", cfg.Publish)
	}
}
