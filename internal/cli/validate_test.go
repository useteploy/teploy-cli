package cli

import (
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
