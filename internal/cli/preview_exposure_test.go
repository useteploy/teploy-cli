package cli

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy/internal/preview"
)

// parsePreviewDeployFlags runs the real `preview deploy` command's flag
// parsing and option finishing, stopping before anything connects.
func parsePreviewDeployFlags(t *testing.T, args ...string) (previewDeployOpts, error) {
	t.Helper()
	var got previewDeployOpts
	ran := false
	cmd := newPreviewDeployCmdWith(func(branch string, opts previewDeployOpts) error {
		ran, got = true, opts
		return nil
	})
	cmd.SetArgs(append([]string{"feature/login"}, args...))
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	err := cmd.Execute()
	if err == nil && !ran {
		t.Fatal("command did not run")
	}
	return got, err
}

func TestPreviewDeployFlags(t *testing.T) {
	// No exposure flags: every exposure field unset, so an update inherits.
	o, err := parsePreviewDeployFlags(t, "--image", "app-build-abc")
	if err != nil {
		t.Fatal(err)
	}
	if o.baseDomain != "" || o.httpOnly != nil || o.allowIPs != nil || o.image != "app-build-abc" || o.ttl != "72h" {
		t.Errorf("defaults: %+v", o)
	}

	// The tailnet invocation Ship sends.
	o, err = parsePreviewDeployFlags(t, "--base-domain", "100-64-1-2.sslip.io", "--http-only",
		"--allow-ip", "100.64.0.0/10", "--allow-ip", "fd7a:115c:a1e0::/48")
	if err != nil {
		t.Fatal(err)
	}
	if o.baseDomain != "100-64-1-2.sslip.io" || o.httpOnly == nil || !*o.httpOnly ||
		!reflect.DeepEqual(o.allowIPs, []string{"100.64.0.0/10", "fd7a:115c:a1e0::/48"}) {
		t.Errorf("tailnet flags: %+v", o)
	}

	// Dot form, uppercase normalized; comma list; explicit false; explicit
	// empty allowlist (clears on update).
	o, err = parsePreviewDeployFlags(t, "--base-domain", "100.64.1.2.SSLIP.io", "--http-only=false",
		"--allow-ip", "100.64.0.0/10,10.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if o.baseDomain != "100.64.1.2.sslip.io" || o.httpOnly == nil || *o.httpOnly ||
		!reflect.DeepEqual(o.allowIPs, []string{"100.64.0.0/10", "10.0.0.1"}) {
		t.Errorf("dot form / false / comma list: %+v", o)
	}
	o, err = parsePreviewDeployFlags(t, "--allow-ip", "")
	if err != nil {
		t.Fatal(err)
	}
	if o.allowIPs == nil || len(o.allowIPs) != 0 {
		t.Errorf(`--allow-ip "" must be an explicit empty list, got %#v`, o.allowIPs)
	}

	for _, bad := range [][]string{
		{"--allow-ip", "100.64.0.0/33"},
		{"--allow-ip", "not-an-ip"},
		{"--base-domain", "localhost"},
		{"--base-domain", "bad domain.io"},
		{"--base-domain", ""},
	} {
		if _, err := parsePreviewDeployFlags(t, bad...); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
}

// `preview list --json` rows carry url with the scheme the route serves,
// and keep domain; an empty list encodes as [], never null.
func TestPreviewListRowsURL(t *testing.T) {
	exp := time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC)
	rows := previewListRows([]preview.State{
		{ID: "myapp-p-08e81639", Branch: "feature/login", Domain: "preview-feature-login-08e81639.100-64-1-2.sslip.io",
			HTTPOnly: true, AllowIPs: []string{"100.64.0.0/10"}, BaseDomain: "100-64-1-2.sslip.io", ExpiresAt: exp},
		{ID: "myapp-p-563059ce", Branch: "main", Domain: "preview-main-563059ce.myapp.com", ExpiresAt: exp},
		{Branch: "old", Domain: "preview-old.myapp.com", ExpiresAt: exp}, // legacy slug-era record
	})
	data, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	var decoded []map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	want := []struct{ url, domain string }{
		{"http://preview-feature-login-08e81639.100-64-1-2.sslip.io", "preview-feature-login-08e81639.100-64-1-2.sslip.io"},
		{"https://preview-main-563059ce.myapp.com", "preview-main-563059ce.myapp.com"},
		{"https://preview-old.myapp.com", "preview-old.myapp.com"},
	}
	for i, w := range want {
		if decoded[i]["url"] != w.url || decoded[i]["domain"] != w.domain {
			t.Errorf("row %d: url=%v domain=%v, want %s / %s", i, decoded[i]["url"], decoded[i]["domain"], w.url, w.domain)
		}
	}
	if decoded[0]["http_only"] != true || decoded[0]["base_domain"] != "100-64-1-2.sslip.io" {
		t.Errorf("tailnet row lost its mode fields: %v", decoded[0])
	}
	for _, key := range []string{"http_only", "allow_ips", "base_domain"} {
		if _, ok := decoded[1][key]; ok {
			t.Errorf("default row must not carry %q: %v", key, decoded[1])
		}
	}

	empty, _ := json.Marshal(previewListRows(nil))
	if strings.TrimSpace(string(empty)) != "[]" {
		t.Errorf("empty list = %s, want []", empty)
	}
}
