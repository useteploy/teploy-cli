package cli

import (
	"bufio"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/config"
)

// scripted returns a reader that feeds the given prompt answers in order.
func scripted(answers ...string) *bufio.Reader {
	return bufio.NewReader(strings.NewReader(strings.Join(answers, "\n") + "\n"))
}

func TestInitFlowWritesValidConfigWithDomain(t *testing.T) {
	dir := t.TempDir()

	// Prompts: app name, domain, server.
	path, err := initFlow(scripted("myapp", "myapp.com", "1.2.3.4"), dir, false, false)
	if err != nil {
		t.Fatalf("initFlow: %v", err)
	}
	if filepath.Base(path) != "teploy.yml" {
		t.Fatalf("expected teploy.yml, got %s", path)
	}

	cfg, err := config.LoadApp(dir)
	if err != nil {
		t.Fatalf("generated config does not load: %v", err)
	}
	if cfg.App != "myapp" || cfg.Domain != "myapp.com" || cfg.Server != "1.2.3.4" {
		t.Fatalf("unexpected config: %+v", cfg)
	}
	if cfg.Ingress != "" {
		t.Fatalf("domain flow must not set ingress, got %q", cfg.Ingress)
	}
}

func TestInitFlowEmptyDomainBecomesHostIngress(t *testing.T) {
	dir := t.TempDir()

	// Prompts: app name, empty domain, bad port, good port, server.
	_, err := initFlow(scripted("api", "", "notaport", "8080", "1.2.3.4"), dir, false, false)
	if err != nil {
		t.Fatalf("initFlow: %v", err)
	}

	cfg, err := config.LoadApp(dir)
	if err != nil {
		t.Fatalf("generated config does not load: %v", err)
	}
	if cfg.Ingress != config.IngressHost {
		t.Fatalf("expected ingress:host for empty domain, got %q", cfg.Ingress)
	}
	if cfg.Port != 8080 {
		t.Fatalf("expected port 8080, got %d", cfg.Port)
	}
	if cfg.Domain != "" {
		t.Fatalf("expected empty domain, got %q", cfg.Domain)
	}
}

func TestInitFlowRefusesExistingConfig(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "teploy.yml"), []byte("app: x\n"), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := initFlow(scripted("a", "b.com", "1.2.3.4"), dir, false, false)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected already-exists error, got %v", err)
	}
}

func TestLoadAppNoConfigSentinel(t *testing.T) {
	dir := t.TempDir()

	_, err := config.LoadApp(dir)
	if !errors.Is(err, config.ErrNoConfig) {
		t.Fatalf("expected ErrNoConfig for empty dir, got %v", err)
	}

	// A present-but-malformed file must NOT match the sentinel — the
	// zero-config first run may only trigger when nothing exists at all.
	if err := os.WriteFile(filepath.Join(dir, "teploy.yml"), []byte("{{nope"), 0644); err != nil {
		t.Fatal(err)
	}
	_, err = config.LoadApp(dir)
	if err == nil {
		t.Fatal("expected parse error")
	}
	if errors.Is(err, config.ErrNoConfig) {
		t.Fatal("malformed config must not report ErrNoConfig")
	}
}
