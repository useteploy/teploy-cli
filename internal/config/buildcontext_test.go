package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeAndLoad writes a teploy.yml with the given body and returns the
// LoadApp result (config, error).
func writeAndLoad(t *testing.T, body string) (*AppConfig, error) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "teploy.yml"), []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	return LoadApp(dir)
}

func TestLoadApp_DockerfileAndContextParse(t *testing.T) {
	cfg, err := writeAndLoad(t, `app: myapp
domain: app.example.com
context: .
dockerfile: server/monolith/Dockerfile
`)
	if err != nil {
		t.Fatalf("valid dockerfile/context should load: %v", err)
	}
	if cfg.Context != "." {
		t.Errorf("context = %q, want .", cfg.Context)
	}
	if cfg.Dockerfile != "server/monolith/Dockerfile" {
		t.Errorf("dockerfile = %q, want server/monolith/Dockerfile", cfg.Dockerfile)
	}
}

func TestLoadApp_DockerfileWithImageRejected(t *testing.T) {
	_, err := writeAndLoad(t, `app: myapp
domain: app.example.com
image: registry.example.com/myapp:latest
dockerfile: server/Dockerfile
`)
	if err == nil {
		t.Fatal("expected error: dockerfile is meaningless with a pre-built image")
	}
	if !strings.Contains(err.Error(), "pulled, not built") {
		t.Errorf("error should explain image is pulled not built, got: %v", err)
	}
}

func TestLoadApp_ContextOnStaticRejected(t *testing.T) {
	_, err := writeAndLoad(t, `app: myapp
domain: app.example.com
type: static
source: ./dist
context: web
`)
	if err == nil {
		t.Fatal("expected error: context has no effect on type:static")
	}
	if !strings.Contains(err.Error(), "type:static") {
		t.Errorf("error should mention type:static, got: %v", err)
	}
}

func TestLoadApp_AbsoluteContextRejected(t *testing.T) {
	_, err := writeAndLoad(t, `app: myapp
domain: app.example.com
context: /etc
`)
	if err == nil {
		t.Fatal("expected error: absolute context path")
	}
	if !strings.Contains(err.Error(), "context") {
		t.Errorf("error should mention context, got: %v", err)
	}
}

func TestLoadApp_EscapingDockerfileRejected(t *testing.T) {
	_, err := writeAndLoad(t, `app: myapp
domain: app.example.com
dockerfile: ../../etc/Dockerfile
`)
	if err == nil {
		t.Fatal("expected error: dockerfile escaping the context via ..")
	}
	if !strings.Contains(err.Error(), "dockerfile") {
		t.Errorf("error should mention dockerfile, got: %v", err)
	}
}

func TestIsSafeSubPath(t *testing.T) {
	safe := []string{".", "server", "server/monolith", "a/b/c/Dockerfile", "./server", "a/../b"}
	for _, p := range safe {
		if !isSafeSubPath(p) {
			t.Errorf("isSafeSubPath(%q) = false, want true", p)
		}
	}
	unsafe := []string{"", "/etc", "/etc/passwd", "..", "../x", "a/../../b", "../"}
	for _, p := range unsafe {
		if isSafeSubPath(p) {
			t.Errorf("isSafeSubPath(%q) = true, want false", p)
		}
	}
}
