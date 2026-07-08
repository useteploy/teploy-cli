package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/config"
)

// TestNonPublicDomainWarnings_PlainDomainNoWarning confirms the
// overwhelmingly common case — a real public hostname — produces zero
// warnings, no regression for existing users.
func TestNonPublicDomainWarnings_PlainDomainNoWarning(t *testing.T) {
	warns := nonPublicDomainWarnings(&config.AppConfig{Domain: "example.com"})
	if len(warns) != 0 {
		t.Errorf("expected no warnings for a public domain, got: %v", warns)
	}
}

// TestNonPublicDomainWarnings_BareIP is the direct regression test for
// tonight's incident: `teploy validate` on investment-club's teploy.yml
// (domain: 192.168.1.114, no tls:) previously reported nothing wrong —
// dns.Validate's net.LookupHost short-circuits a literal IP straight back
// out, so DNS "matched." The deploy then hung on Caddy's automatic-HTTPS
// attempt. This must now surface as an explicit warning before the deploy
// ever runs.
func TestNonPublicDomainWarnings_BareIP(t *testing.T) {
	warns := nonPublicDomainWarnings(&config.AppConfig{Domain: "192.168.1.114"})
	if len(warns) != 1 {
		t.Fatalf("expected 1 warning, got %d: %v", len(warns), warns)
	}
	if !strings.Contains(warns[0], "192.168.1.114") || !strings.Contains(warns[0], "plain HTTP") {
		t.Errorf("warning should name the host and mention plain HTTP, got: %q", warns[0])
	}
}

func TestNonPublicDomainWarnings_TLSInternal(t *testing.T) {
	warns := nonPublicDomainWarnings(&config.AppConfig{
		Domain: "192.168.1.114",
		TLS:    &config.TLSConfig{Internal: true},
	})
	if len(warns) != 1 || !strings.Contains(warns[0], "self-signed") {
		t.Errorf("expected a self-signed HTTPS warning, got: %v", warns)
	}
}

func TestNonPublicDomainWarnings_CustomCert(t *testing.T) {
	warns := nonPublicDomainWarnings(&config.AppConfig{
		Domain: "192.168.1.114",
		TLS:    &config.TLSConfig{Cert: "./c.pem", Key: "./c.key"},
	})
	if len(warns) != 1 || !strings.Contains(warns[0], "custom certificate") {
		t.Errorf("expected a custom-certificate warning, got: %v", warns)
	}
}

// A mix of a public and a non-public host in one comma-separated domain
// list must only warn about the non-public one.
func TestNonPublicDomainWarnings_MixedHosts(t *testing.T) {
	warns := nonPublicDomainWarnings(&config.AppConfig{Domain: "example.com, 192.168.1.114"})
	if len(warns) != 1 || !strings.Contains(warns[0], "192.168.1.114") {
		t.Errorf("expected exactly one warning for the non-public host, got: %v", warns)
	}
}

// --- buildPrereqCheck: dockerfile:/context: aware build detection ----------

// mkDockerfile creates dir/rel (with parent dirs) as an empty Dockerfile and
// returns dir. Context is passed as an absolute path so DetectAt resolves the
// fixture without a chdir.
func mkDockerfile(t *testing.T, rel string) string {
	t.Helper()
	dir := t.TempDir()
	full := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte("FROM alpine\n"), 0644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// A configured subdir dockerfile: that exists must NOT warn about Nixpacks —
// the exact false positive this change fixes.
func TestBuildPrereqCheck_SubdirDockerfileNoWarning(t *testing.T) {
	dir := mkDockerfile(t, "server/monolith/Dockerfile")
	warns, errs := buildPrereqCheck(&config.AppConfig{
		Context:    dir,
		Dockerfile: "server/monolith/Dockerfile",
	})
	if len(warns) != 0 || len(errs) != 0 {
		t.Errorf("configured subdir dockerfile should be clean, got warns=%v errs=%v", warns, errs)
	}
}

// A configured dockerfile: that doesn't exist is an ERROR naming the path,
// not a silent Nixpacks fallback.
func TestBuildPrereqCheck_MissingConfiguredDockerfileErrors(t *testing.T) {
	dir := t.TempDir() // empty — the configured dockerfile is absent
	warns, errs := buildPrereqCheck(&config.AppConfig{
		Context:    dir,
		Dockerfile: "server/monolith/Dockerfile",
	})
	if len(errs) != 1 {
		t.Fatalf("expected 1 error for a missing configured dockerfile, got %d: %v", len(errs), errs)
	}
	if !strings.Contains(errs[0], "server/monolith/Dockerfile") {
		t.Errorf("error should name the missing dockerfile, got: %v", errs[0])
	}
	if len(warns) != 0 {
		t.Errorf("a missing configured dockerfile should not also warn Nixpacks, got: %v", warns)
	}
}

// No dockerfile: configured and no Dockerfile present → the Nixpacks WARN,
// unchanged from the old behavior.
func TestBuildPrereqCheck_NoDockerfileWarnsNixpacks(t *testing.T) {
	dir := t.TempDir() // no Dockerfile, no config
	warns, errs := buildPrereqCheck(&config.AppConfig{Context: dir})
	if len(errs) != 0 {
		t.Fatalf("no dockerfile is a warning, not an error, got errs=%v", errs)
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "Nixpacks") {
		t.Errorf("expected one Nixpacks warning, got: %v", warns)
	}
}

// A context: subdirectory holding a plain Dockerfile (default name) must
// resolve cleanly — no warning.
func TestBuildPrereqCheck_ContextSubdirNoWarning(t *testing.T) {
	dir := mkDockerfile(t, "Dockerfile") // Dockerfile at the context root
	warns, errs := buildPrereqCheck(&config.AppConfig{Context: dir})
	if len(warns) != 0 || len(errs) != 0 {
		t.Errorf("a Dockerfile at the context root should be clean, got warns=%v errs=%v", warns, errs)
	}
}

// A pre-built image: is pulled, not built — nothing to check, no warning.
func TestBuildPrereqCheck_ImageSkipsBuildCheck(t *testing.T) {
	warns, errs := buildPrereqCheck(&config.AppConfig{Image: "registry.example.com/app:latest"})
	if len(warns) != 0 || len(errs) != 0 {
		t.Errorf("a pre-built image needs no build check, got warns=%v errs=%v", warns, errs)
	}
}
