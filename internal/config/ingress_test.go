package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadApp_IngressDefault(t *testing.T) {
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
	if cfg.Ingress != "" {
		t.Errorf("Ingress = %q, want empty (default)", cfg.Ingress)
	}
	if !cfg.UsesCaddy() {
		t.Errorf("default config should report UsesCaddy() = true")
	}
}

func TestLoadApp_IngressCaddyExplicit(t *testing.T) {
	dir := t.TempDir()
	content := `app: myapp
domain: myapp.com
ingress: caddy
`
	if err := os.WriteFile(filepath.Join(dir, "teploy.yml"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadApp(dir)
	if err != nil {
		t.Fatalf("LoadApp: %v", err)
	}
	if cfg.Ingress != "caddy" {
		t.Errorf("Ingress = %q, want 'caddy'", cfg.Ingress)
	}
	if !cfg.UsesCaddy() {
		t.Errorf("ingress: caddy should report UsesCaddy() = true")
	}
}

func TestLoadApp_IngressExternalYAML(t *testing.T) {
	dir := t.TempDir()
	content := `app: myapp
domain: myapp.com
ingress: external
`
	if err := os.WriteFile(filepath.Join(dir, "teploy.yml"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadApp(dir)
	if err != nil {
		t.Fatalf("LoadApp: %v", err)
	}
	if cfg.Ingress != "external" {
		t.Errorf("Ingress = %q, want 'external'", cfg.Ingress)
	}
	if cfg.UsesCaddy() {
		t.Errorf("ingress: external should report UsesCaddy() = false")
	}
}

func TestLoadApp_IngressExternalTOML(t *testing.T) {
	dir := t.TempDir()
	content := `app = "myapp"
domain = "myapp.com"
ingress = "external"
`
	if err := os.WriteFile(filepath.Join(dir, "teploy.toml"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadApp(dir)
	if err != nil {
		t.Fatalf("LoadApp: %v", err)
	}
	if cfg.Ingress != "external" {
		t.Errorf("Ingress = %q, want 'external'", cfg.Ingress)
	}
}

func TestLoadApp_IngressUnknownRejected(t *testing.T) {
	dir := t.TempDir()
	content := `app: myapp
domain: myapp.com
ingress: traefik
`
	if err := os.WriteFile(filepath.Join(dir, "teploy.yml"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadApp(dir)
	if err == nil {
		t.Fatal("expected error for unknown ingress mode")
	}
	if !strings.Contains(err.Error(), "ingress") {
		t.Errorf("error should mention ingress, got: %v", err)
	}
}

func TestLoadApp_IngressExternalForStaticRejected(t *testing.T) {
	// Static deploys ARE Caddy-served by definition; external ingress
	// would have nothing to serve. Must error early.
	dir := t.TempDir()
	content := `app: myapp
domain: myapp.com
type: static
source: ./dist
ingress: external
`
	if err := os.WriteFile(filepath.Join(dir, "teploy.yml"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadApp(dir)
	if err == nil {
		t.Fatal("expected error: ingress=external incompatible with type=static")
	}
	if !strings.Contains(err.Error(), "static") {
		t.Errorf("error should mention static, got: %v", err)
	}
}

func TestLoadApp_IngressOverlayMerge(t *testing.T) {
	dir := t.TempDir()
	base := `app: myapp
domain: myapp.com
`
	overlay := `ingress: external
`
	if err := os.WriteFile(filepath.Join(dir, "teploy.yml"), []byte(base), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "teploy.prod.yml"), []byte(overlay), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadAppWithDestination(dir, "prod")
	if err != nil {
		t.Fatalf("LoadAppWithDestination: %v", err)
	}
	if cfg.Ingress != "external" {
		t.Errorf("overlay should apply ingress=external, got %q", cfg.Ingress)
	}
}
