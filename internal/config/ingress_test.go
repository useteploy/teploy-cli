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

func TestLoadApp_IngressHostValid(t *testing.T) {
	dir := t.TempDir()
	content := `app: myapp
ingress: host
bind: 0.0.0.0
port: 3000
`
	if err := os.WriteFile(filepath.Join(dir, "teploy.yml"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadApp(dir)
	if err != nil {
		t.Fatalf("LoadApp: %v", err)
	}
	if cfg.Ingress != "host" {
		t.Errorf("Ingress = %q, want 'host'", cfg.Ingress)
	}
	if cfg.Bind != "0.0.0.0" {
		t.Errorf("Bind = %q, want '0.0.0.0'", cfg.Bind)
	}
	if cfg.UsesCaddy() {
		t.Errorf("ingress: host should report UsesCaddy() = false")
	}
}

func TestLoadApp_IngressHostDefaultBindValid(t *testing.T) {
	// bind is optional; host mode defaults to 0.0.0.0 at deploy time.
	dir := t.TempDir()
	content := `app: myapp
ingress: host
port: 3000
`
	if err := os.WriteFile(filepath.Join(dir, "teploy.yml"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadApp(dir); err != nil {
		t.Fatalf("LoadApp: %v", err)
	}
}

func TestLoadApp_IngressHostRequiresPort(t *testing.T) {
	dir := t.TempDir()
	content := `app: myapp
ingress: host
`
	if err := os.WriteFile(filepath.Join(dir, "teploy.yml"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadApp(dir)
	if err == nil {
		t.Fatal("expected error: ingress=host requires port")
	}
	if !strings.Contains(err.Error(), "port") {
		t.Errorf("error should mention port, got: %v", err)
	}
}

func TestLoadApp_BindWithoutHostRejected(t *testing.T) {
	dir := t.TempDir()
	content := `app: myapp
domain: myapp.com
ingress: external
bind: 0.0.0.0
`
	if err := os.WriteFile(filepath.Join(dir, "teploy.yml"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadApp(dir)
	if err == nil {
		t.Fatal("expected error: bind requires ingress=host")
	}
	if !strings.Contains(err.Error(), "bind") {
		t.Errorf("error should mention bind, got: %v", err)
	}
}

func TestLoadApp_IngressHostInvalidBindRejected(t *testing.T) {
	dir := t.TempDir()
	content := `app: myapp
ingress: host
port: 3000
bind: not-an-ip
`
	if err := os.WriteFile(filepath.Join(dir, "teploy.yml"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadApp(dir)
	if err == nil {
		t.Fatal("expected error: invalid bind IP")
	}
	if !strings.Contains(err.Error(), "bind") {
		t.Errorf("error should mention bind, got: %v", err)
	}
}

func TestLoadApp_IngressHostMultiReplicaRejected(t *testing.T) {
	dir := t.TempDir()
	content := `app: myapp
ingress: host
port: 3000
replicas: 2
`
	if err := os.WriteFile(filepath.Join(dir, "teploy.yml"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadApp(dir)
	if err == nil {
		t.Fatal("expected error: ingress=host single replica only")
	}
	if !strings.Contains(err.Error(), "replica") {
		t.Errorf("error should mention replica, got: %v", err)
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
