package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/ssh"
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

func TestDNSCheckAppliesOnlyToCaddyRoutedDomains(t *testing.T) {
	for _, tc := range []struct {
		cfg  config.AppConfig
		want bool
	}{
		{config.AppConfig{Domain: "a.dev"}, true},
		{config.AppConfig{Domain: "a.dev", Ingress: config.IngressCaddy}, true},
		{config.AppConfig{Domain: "a.dev", Ingress: config.IngressHost}, false},
		{config.AppConfig{Domain: "a.dev", Ingress: config.IngressExternal}, false},
		{config.AppConfig{}, false},
		{config.AppConfig{Domain: "app.test", TLS: &config.TLSConfig{Internal: true}}, false},
		{config.AppConfig{Domain: "a.dev", TLS: &config.TLSConfig{HTTPOnly: true}}, false},
		{config.AppConfig{Domain: "a.dev", TLS: &config.TLSConfig{Cert: "c", Key: "k"}}, true},
	} {
		if got := dnsCheckApplies(&tc.cfg); got != tc.want {
			t.Errorf("dnsCheckApplies(%+v) = %v", tc.cfg, got)
		}
	}
}

// Every multi-host first deploy used to fail: the whole comma list went to the
// resolver as one name.
func TestValidateDomainDNS_ChecksEachHost(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4", ssh.MockCommand{Match: "hostname -I", Output: "127.0.0.1"})
	_, err := validateDomainDNS(context.Background(), mock, "localhost, localhost", "127.0.0.1")
	if err != nil {
		t.Fatalf("both hosts resolve to the server: %v", err)
	}
}

// A placeholder stops a deploy and a plan before any connection, and never a
// load: the refusal is deploy admission's.
func TestDeployAdmission_RefusesBeforeAnyEffect(t *testing.T) {
	cfg := &config.AppConfig{App: "a", Domain: "a.example.com", Image: "nginx:1", Server: "192.0.2.1"}
	err := deployAppConfig(&Flags{}, cfg, "192.0.2.1", "", "", false, false, "")
	if err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("deploy: %v", err)
	}
	if err := runMultiDeploy(&Flags{}, cfg, "", "", false, 1, false); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("multi deploy: %v", err)
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "teploy.yml"), []byte("app: a\ndomain: a.example.com\nimage: nginx:1\nserver: 192.0.2.1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := config.LoadApp(dir); err != nil {
		t.Fatalf("rollback and logs load this file: %v", err)
	}
	t.Chdir(dir)
	if err := runPlan(&Flags{}, "", "", "", ""); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("plan: %v", err)
	}
	flags := &Flags{JSON: true}
	if err := runValidate(flags); err == nil {
		t.Fatal("validate must report it invalid")
	}
}

func TestValidateDomainDNS_SkipsLocalhostNames(t *testing.T) {
	notes, err := validateDomainDNS(context.Background(), nil, "foo.localhost", "192.0.2.1")
	if err != nil || len(notes) != 0 {
		t.Fatalf("notes=%v err=%v", notes, err)
	}
}
