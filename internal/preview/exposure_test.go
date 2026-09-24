package preview

// Tailnet preview mode (DELEGATED_DECISIONS_2026-09-23 §10): an explicit
// base domain, HTTP-only routes and an IP allowlist, persisted in the
// record so updates, list, prune and destroy keep them — and records
// without the fields behave exactly as before.

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy/internal/ssh"
)

const (
	tailnetBase   = "100-64-1-2.sslip.io"
	tailnetCIDR   = "100.64.0.0/10"
	tailnetDomain = "preview-feature-login-" + loginIDHex + "." + tailnetBase
)

func boolPtr(b bool) *bool { return &b }

func tailnetCfg(branch, version string) DeployConfig {
	cfg := deployCfg(branch, version)
	cfg.BaseDomain = tailnetBase
	cfg.HTTPOnly = boolPtr(true)
	cfg.AllowIPs = []string{tailnetCIDR}
	return cfg
}

func readState(t *testing.T, mock *ssh.MockExecutor, branch string) State {
	t.Helper()
	var s State
	if err := json.Unmarshal(mock.Files[previewStatePath("myapp", branch)], &s); err != nil {
		t.Fatalf("reading record: %v", err)
	}
	return s
}

// managedBlock returns the Caddyfile region for a route key.
func managedBlock(t *testing.T, mock *ssh.MockExecutor, key string) string {
	t.Helper()
	caddyfile := string(mock.Files["/deployments/caddy/Caddyfile"])
	begin := strings.Index(caddyfile, "# TEPLOY BEGIN "+key+"\n")
	end := strings.Index(caddyfile, "# TEPLOY END "+key+"\n")
	if begin < 0 || end < begin {
		t.Fatalf("no managed block for %s in:\n%s", key, caddyfile)
	}
	return caddyfile[begin:end]
}

// The route is written HTTP-only (explicit http:// site address, no tls
// line) with the allowlist as its firewall, under the explicit base; the
// record carries the mode and the output names the http:// URL.
func TestDeploy_TailnetModeWritesHTTPOnlyGatedRoute(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4", previewDeployMocks()...)
	var buf bytes.Buffer
	mgr := NewManager(mock, &buf)

	mustDeploy(t, mgr, tailnetCfg(loginBranch, "v1"))

	block := managedBlock(t, mock, "myapp-preview-p-"+loginIDHex)
	for _, want := range []string{
		"http://" + tailnetDomain + " {",
		"@teploy_fw_notallow not remote_ip " + tailnetCIDR,
		"reverse_proxy myapp-preview-p-" + loginIDHex + "-v1:80",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("route missing %q:\n%s", want, block)
		}
	}
	if strings.Contains(block, "\ttls ") {
		t.Errorf("HTTP-only route must not carry a tls directive:\n%s", block)
	}

	s := readState(t, mock, loginBranch)
	if s.Domain != tailnetDomain || s.BaseDomain != tailnetBase || !s.HTTPOnly || !reflect.DeepEqual(s.AllowIPs, []string{tailnetCIDR}) {
		t.Errorf("record does not carry the mode: %+v", s)
	}
	// C06 identity untouched by the mode.
	if s.ID != "myapp-p-"+loginIDHex || s.Route != "myapp-preview-p-"+loginIDHex {
		t.Errorf("canonical identity changed: id=%q route=%q", s.ID, s.Route)
	}
	if !strings.Contains(buf.String(), "Preview deployed: http://"+tailnetDomain+"\n") {
		t.Errorf("output must name the http:// URL, got:\n%s", buf.String())
	}
}

// A blue/green update that does not repeat the flags keeps the mode: same
// hostname, still HTTP-only, still gated — never silently back to HTTPS or
// open.
func TestDeploy_UpdateInheritsTailnetMode(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4", previewDeployMocks()...)
	var buf bytes.Buffer
	mgr := NewManager(mock, &buf)

	mustDeploy(t, mgr, tailnetCfg(loginBranch, "v1"))
	mustDeploy(t, mgr, deployCfg(loginBranch, "v2")) // no exposure overrides

	block := managedBlock(t, mock, "myapp-preview-p-"+loginIDHex)
	if !strings.HasPrefix(strings.SplitN(block, "\n", 2)[1], "http://"+tailnetDomain+" {") {
		t.Errorf("update re-enabled HTTPS or moved the hostname:\n%s", block)
	}
	if !strings.Contains(block, "not remote_ip "+tailnetCIDR) {
		t.Errorf("update dropped the allowlist:\n%s", block)
	}
	if !strings.Contains(block, "-v2:80") {
		t.Errorf("route does not point at the v2 candidate:\n%s", block)
	}
	s := readState(t, mock, loginBranch)
	if s.Container != "myapp-preview-p-"+loginIDHex+"-v2" || s.Domain != tailnetDomain ||
		s.BaseDomain != tailnetBase || !s.HTTPOnly || !reflect.DeepEqual(s.AllowIPs, []string{tailnetCIDR}) {
		t.Errorf("update record lost the mode: %+v", s)
	}
}

// Explicit overrides on an update win field by field: turning HTTP-only
// off keeps the recorded base and allowlist; an empty non-nil allowlist
// clears it.
func TestDeploy_UpdateOverridesFieldByField(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4", previewDeployMocks()...)
	var buf bytes.Buffer
	mgr := NewManager(mock, &buf)

	mustDeploy(t, mgr, tailnetCfg(loginBranch, "v1"))
	cfg := deployCfg(loginBranch, "v2")
	cfg.HTTPOnly = boolPtr(false)
	mustDeploy(t, mgr, cfg)

	block := managedBlock(t, mock, "myapp-preview-p-"+loginIDHex)
	if strings.Contains(block, "http://") || !strings.Contains(block, tailnetDomain+" {") {
		t.Errorf("--http-only=false must serve the same host with automatic HTTPS:\n%s", block)
	}
	if !strings.Contains(block, "not remote_ip "+tailnetCIDR) {
		t.Errorf("allowlist not inherited:\n%s", block)
	}

	cfg = deployCfg(loginBranch, "v3")
	cfg.AllowIPs = []string{}
	mustDeploy(t, mgr, cfg)
	block = managedBlock(t, mock, "myapp-preview-p-"+loginIDHex)
	if strings.Contains(block, "remote_ip") {
		t.Errorf("empty allowlist override must clear the gate:\n%s", block)
	}
	s := readState(t, mock, loginBranch)
	if s.HTTPOnly || len(s.AllowIPs) != 0 || s.BaseDomain != tailnetBase {
		t.Errorf("record after overrides: %+v", s)
	}
}

// Back-compat: a default deploy writes none of the new keys (the record is
// byte-shaped like before), and updating a record written before the
// fields existed keeps automatic HTTPS, no gate, and the app-domain host.
func TestDeploy_DefaultAndLegacyRecordsUnchanged(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4", previewDeployMocks()...)
	var buf bytes.Buffer
	mgr := NewManager(mock, &buf)

	mustDeploy(t, mgr, deployCfg(loginBranch, "v1"))
	raw := string(mock.Files[previewStatePath("myapp", loginBranch)])
	for _, key := range []string{"base_domain", "http_only", "allow_ips"} {
		if strings.Contains(raw, key) {
			t.Errorf("default record must not carry %q:\n%s", key, raw)
		}
	}
	if !strings.Contains(buf.String(), "Preview deployed: https://preview-feature-login-"+loginIDHex+".myapp.com\n") {
		t.Errorf("default output must name the https:// URL, got:\n%s", buf.String())
	}

	// A pre-field canonical record on disk, then an update over it.
	domain := "preview-feature-login-" + loginIDHex + ".myapp.com"
	mock.Files[previewStatePath("myapp", loginBranch)] = []byte(canonicalRecordJSON("myapp", loginIDHex, loginBranch,
		"myapp-preview-p-"+loginIDHex+"-v1", domain, time.Now().Add(time.Hour)))
	mustDeploy(t, mgr, deployCfg(loginBranch, "v2"))

	block := managedBlock(t, mock, "myapp-preview-p-"+loginIDHex)
	if !strings.HasPrefix(strings.SplitN(block, "\n", 2)[1], domain+" {") {
		t.Errorf("legacy-record update must keep automatic HTTPS on the app-domain host:\n%s", block)
	}
	if strings.Contains(block, "remote_ip") {
		t.Errorf("legacy-record update grew a gate:\n%s", block)
	}
	s := readState(t, mock, loginBranch)
	if s.BaseDomain != "" || s.HTTPOnly || s.AllowIPs != nil || s.URL() != "https://"+domain {
		t.Errorf("legacy-record update changed the mode: %+v", s)
	}
}

// State round-trip: the mode survives marshal/unmarshal; a record written
// before the fields existed decodes to the default mode and an https URL;
// the slug-era legacy record likewise.
func TestState_ExposureRoundTrip(t *testing.T) {
	in := State{
		ID: "myapp-p-" + loginIDHex, Branch: loginBranch, Route: "myapp-preview-p-" + loginIDHex,
		Domain: tailnetDomain, Port: 49200, Container: "c", Image: "i",
		CreatedAt: time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC), ExpiresAt: time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC),
		BaseDomain: tailnetBase, HTTPOnly: true, AllowIPs: []string{tailnetCIDR, "fd7a:115c:a1e0::/48"},
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out State
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Errorf("round-trip lost data:\nin:  %+v\nout: %+v", in, out)
	}
	if out.URL() != "http://"+tailnetDomain {
		t.Errorf("URL = %q, want http://", out.URL())
	}

	for name, raw := range map[string]string{
		"canonical-pre-field": canonicalRecordJSON("myapp", loginIDHex, loginBranch, "c", "preview-x.myapp.com", time.Now()),
		"legacy-slug":         legacyRecordJSON(loginBranch, "c"),
	} {
		var s State
		if err := json.Unmarshal([]byte(raw), &s); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if s.BaseDomain != "" || s.HTTPOnly || s.AllowIPs != nil || !strings.HasPrefix(s.URL(), "https://") {
			t.Errorf("%s decoded with a non-default mode: %+v url=%s", name, s, s.URL())
		}
	}
}

// Invalid overrides refuse before anything is touched on the server.
func TestDeploy_InvalidExposureRefusesBeforeMutation(t *testing.T) {
	for name, mutate := range map[string]func(*DeployConfig){
		"allow-ip":      func(c *DeployConfig) { c.AllowIPs = []string{"100.64.0.0/33"} },
		"allow-ip-junk": func(c *DeployConfig) { c.AllowIPs = []string{"1.2.3.4 }"} },
		"base-domain":   func(c *DeployConfig) { c.BaseDomain = "Bad Domain {" },
		"no-base":       func(c *DeployConfig) { c.Domain = "" },
	} {
		t.Run(name, func(t *testing.T) {
			mock := ssh.NewMockExecutor("1.2.3.4", previewDeployMocks()...)
			mgr := NewManager(mock, &bytes.Buffer{})
			cfg := deployCfg(loginBranch, "v1")
			mutate(&cfg)
			if err := mgr.Deploy(context.Background(), cfg); err == nil {
				t.Fatal("invalid exposure must fail the deploy")
			}
			for _, c := range mock.Calls {
				if !strings.HasPrefix(c, "cat ") {
					t.Errorf("mutated before validating: %q", c)
				}
			}
		})
	}
}

// Both sslip.io spellings are accepted as a base; a base without a dot or
// with uppercase/space is refused.
func TestValidateBaseDomain(t *testing.T) {
	for _, ok := range []string{"100-64-1-2.sslip.io", "100.64.1.2.sslip.io", "myapp.com"} {
		if err := ValidateBaseDomain(ok); err != nil {
			t.Errorf("ValidateBaseDomain(%q): %v", ok, err)
		}
	}
	for _, bad := range []string{"", "localhost", "Upper.sslip.io", "a..b", "-a.b", "a.b.", "a b.c", "http://a.b"} {
		if err := ValidateBaseDomain(bad); err == nil {
			t.Errorf("ValidateBaseDomain(%q) accepted", bad)
		}
	}
}
