package cli

import (
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/config"
)

// The ingress/domain pairing every template render is validated against: a
// host-ingress template publishes on bind:port and needs no domain; everything
// else routes by domain and requires one.
func TestApplyTemplateOverrides(t *testing.T) {
	hostCfg := func() *config.AppConfig {
		return &config.AppConfig{App: "ship", Ingress: config.IngressHost, Port: 7460}
	}
	domainCfg := func() *config.AppConfig {
		return &config.AppConfig{App: "blog", Ingress: config.IngressCaddy}
	}

	// A host-ingress template with no domain: legal, port from the template.
	cfg := hostCfg()
	if err := applyTemplateOverrides(cfg, "", "box", 0); err != nil {
		t.Fatalf("host template without domain should pass: %v", err)
	}
	if cfg.Port != 7460 {
		t.Errorf("template port should stand: got %d", cfg.Port)
	}
	if cfg.Server != "box" {
		t.Errorf("server override not applied: %q", cfg.Server)
	}

	// --port overrides the template's port.
	cfg = hostCfg()
	if err := applyTemplateOverrides(cfg, "", "box", 8000); err != nil {
		t.Fatalf("--port override should pass: %v", err)
	}
	if cfg.Port != 8000 {
		t.Errorf("port override not applied: got %d", cfg.Port)
	}

	// A domain template without --domain: refused, naming the rule.
	if err := applyTemplateOverrides(domainCfg(), "", "", 0); err == nil || !strings.Contains(err.Error(), "--domain is required") {
		t.Errorf("domain template without --domain should fail with the rule, got: %v", err)
	}

	// A domain template with --domain: applied.
	cfg = domainCfg()
	if err := applyTemplateOverrides(cfg, "blog.example.com", "", 0); err != nil {
		t.Fatalf("domain template with --domain should pass: %v", err)
	}
	if cfg.Domain != "blog.example.com" {
		t.Errorf("domain override not applied: %q", cfg.Domain)
	}

	// A host template declaring no port at all and no --port: refused rather
	// than deployed on port 0.
	bare := &config.AppConfig{App: "x", Ingress: config.IngressHost}
	if err := applyTemplateOverrides(bare, "", "box", 0); err == nil || !strings.Contains(err.Error(), "--port") {
		t.Errorf("host template with no port anywhere should fail, got: %v", err)
	}
}

func TestReplaceTopLevelPort(t *testing.T) {
	in := "app: ship\ningress: host\nport: 7460\naccessories:\n  nucleus:\n    port: 5432\n"
	out := replaceTopLevelPort(in, 8000)
	if !strings.Contains(out, "port: 8000") {
		t.Errorf("top-level port not replaced:\n%s", out)
	}
	if !strings.Contains(out, "    port: 5432") {
		t.Errorf("accessory port must be untouched:\n%s", out)
	}

	// No top-level port line: appended, not inserted mid-file.
	out = replaceTopLevelPort("app: ship\ningress: host\n", 8000)
	if !strings.HasSuffix(strings.TrimSpace(out), "port: 8000") {
		t.Errorf("port should be appended at the end:\n%s", out)
	}
}
