package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadApp_TLSYAML(t *testing.T) {
	dir := t.TempDir()
	content := `app: fylun-web
domain: fylun.ai
tls:
  cert: ./certs/origin.pem
  key: ./certs/origin.key
`
	if err := os.WriteFile(filepath.Join(dir, "teploy.yml"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadApp(dir)
	if err != nil {
		t.Fatalf("LoadApp: %v", err)
	}
	if cfg.TLS == nil {
		t.Fatal("expected TLS config to be parsed")
	}
	if cfg.TLS.Cert != "./certs/origin.pem" || cfg.TLS.Key != "./certs/origin.key" {
		t.Errorf("TLS = %+v", cfg.TLS)
	}
}

func TestLoadApp_TLSTOML(t *testing.T) {
	dir := t.TempDir()
	content := `app = "fylun-web"
domain = "fylun.ai"

[tls]
cert = "./certs/origin.pem"
key = "./certs/origin.key"
`
	if err := os.WriteFile(filepath.Join(dir, "teploy.toml"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadApp(dir)
	if err != nil {
		t.Fatalf("LoadApp: %v", err)
	}
	if cfg.TLS == nil || cfg.TLS.Cert != "./certs/origin.pem" {
		t.Errorf("TLS not parsed from TOML: %+v", cfg.TLS)
	}
}

func TestLoadApp_TLSRequiresBoth(t *testing.T) {
	dir := t.TempDir()
	content := `app: fylun-web
domain: fylun.ai
tls:
  cert: ./certs/origin.pem
`
	if err := os.WriteFile(filepath.Join(dir, "teploy.yml"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadApp(dir)
	if err == nil {
		t.Fatal("expected error when tls.key is missing")
	}
	if !strings.Contains(err.Error(), "cert") && !strings.Contains(err.Error(), "key") {
		t.Errorf("error should mention cert/key, got: %v", err)
	}
}

func TestLoadApp_TLSRejectedForStatic(t *testing.T) {
	dir := t.TempDir()
	content := `app: site
domain: site.com
type: static
source: ./dist
tls:
  cert: ./c.pem
  key: ./c.key
`
	if err := os.WriteFile(filepath.Join(dir, "teploy.yml"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadApp(dir)
	if err == nil {
		t.Fatal("expected error: tls on static deploy")
	}
	if !strings.Contains(err.Error(), "static") {
		t.Errorf("error should mention static, got: %v", err)
	}
}

func TestLoadApp_TLSInternal(t *testing.T) {
	dir := t.TempDir()
	content := `app: myapp
domain: 192.168.1.114
tls:
  internal: true
`
	if err := os.WriteFile(filepath.Join(dir, "teploy.yml"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadApp(dir)
	if err != nil {
		t.Fatalf("LoadApp: %v", err)
	}
	if cfg.TLS == nil || !cfg.TLS.Internal {
		t.Fatalf("expected tls.internal=true, got: %+v", cfg.TLS)
	}
	if cfg.TLS.Cert != "" || cfg.TLS.Key != "" {
		t.Errorf("tls.internal should not require cert/key, got: %+v", cfg.TLS)
	}
}

func TestLoadApp_TLSInternalRejectsCertAndKey(t *testing.T) {
	dir := t.TempDir()
	content := `app: myapp
domain: 192.168.1.114
tls:
  internal: true
  cert: ./c.pem
  key: ./c.key
`
	if err := os.WriteFile(filepath.Join(dir, "teploy.yml"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadApp(dir)
	if err == nil {
		t.Fatal("expected error combining tls.internal with cert/key")
	}
	if !strings.Contains(err.Error(), "internal") {
		t.Errorf("error should mention internal, got: %v", err)
	}
}

func TestLoadApp_TLSOverlayMerge(t *testing.T) {
	dir := t.TempDir()
	base := `app: fylun-web
domain: fylun.ai
`
	overlay := `tls:
  cert: ./certs/origin.pem
  key: ./certs/origin.key
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
	if cfg.TLS == nil || cfg.TLS.Cert != "./certs/origin.pem" {
		t.Errorf("overlay TLS not applied: %+v", cfg.TLS)
	}
}
