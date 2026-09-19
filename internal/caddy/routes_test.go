package caddy

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- Vendored parser (F48/F49) ---

func TestParseSites_Structure(t *testing.T) {
	in := "{\n\tadmin 127.0.0.1:2019\n}\n\n" +
		"(snippet) {\n\theader X-From Snippet\n}\n\n" +
		"# a comment about a.com\n" +
		"a.com, www.a.com {\n\treverse_proxy a:80\n}\n\n" +
		"b.com {\n\ttls internal\n\trespond \"hi {ok}\" 200\n}\n"
	blocks, err := ParseSites(in)
	if err != nil {
		t.Fatalf("ParseSites: %v", err)
	}
	if len(blocks) != 2 {
		t.Fatalf("expected 2 site blocks (global + snippet skipped), got %d: %+v", len(blocks), blocks)
	}
	if got := strings.Join(blocks[0].Addresses, ","); got != "a.com,www.a.com" {
		t.Errorf("block 0 addresses: %q", got)
	}
	if got := strings.Join(blocks[1].Addresses, ","); got != "b.com" {
		t.Errorf("block 1 addresses: %q", got)
	}
	// The quoted brace inside the respond body must not corrupt depth.
	if !strings.Contains(strings.Join(blocks[1].Lines, "\n"), `respond "hi {ok}" 200`) {
		t.Errorf("block 1 body mangled: %v", blocks[1].Lines)
	}
}

func TestParseSites_MultilineBacktickBody(t *testing.T) {
	// The maintenance block's own shape: a backtick literal spanning lines
	// with CSS braces inside — round-trip parsing must survive it.
	blk := maintenanceBlock([]string{"myapp.com"}, SitePolicy{})
	blocks, err := ParseSites("# TEPLOY BEGIN myapp\n" + blk + "\n# TEPLOY END myapp\n")
	if err != nil {
		t.Fatalf("ParseSites on a maintenance block: %v", err)
	}
	if len(blocks) != 1 {
		t.Fatalf("expected the maintenance site block to parse as one block, got %d", len(blocks))
	}
}

func TestParseSites_FailLoud(t *testing.T) {
	cases := map[string]string{
		"unbalanced open":  "a.com {\n\treverse_proxy a:80\n",
		"unbalanced close": "a.com {\n}\n}\n",
		"top-level import": "import sites/*.caddy\n\na.com {\n\trespond 200\n}\n",
		"stray directive":  "email me@example.com\n",
	}
	for name, in := range cases {
		if _, err := ParseSites(in); err == nil {
			t.Errorf("%s: expected a loud parse failure", name)
		}
	}
}

// --- Structured adoption (F49) ---

func TestAdoptForeignBlocks_PartialKeepsForeignDirectives(t *testing.T) {
	in := "old.com, keep.com, https://secure.org {\n\ttls /c.crt /c.key\n\treverse_proxy legacy:80\n}\n"
	got, err := adoptForeignBlocks(in, []string{"old.com", "secure.org"})
	if err != nil {
		t.Fatalf("adoptForeignBlocks: %v", err)
	}
	if !strings.Contains(got, "reverse_proxy legacy:80") {
		t.Errorf("the foreign block's directives must survive a partial adoption:\n%s", got)
	}
	if !strings.Contains(got, "tls /c.crt /c.key") {
		t.Errorf("the foreign block's tls directive must survive:\n%s", got)
	}
	if !strings.Contains(got, "keep.com {") {
		t.Errorf("the non-adopted host must remain in the address line:\n%s", got)
	}
	for _, gone := range []string{"old.com", "secure.org"} {
		if strings.Contains(got, gone) {
			t.Errorf("adopted host %s must be gone from the address line:\n%s", gone, got)
		}
	}
}

func TestAdoptForeignBlocks_UnparseableRefusedBeforeWrite(t *testing.T) {
	in := "a.com {\n\treverse_proxy a:80\n" // never closed
	if _, err := adoptForeignBlocks(in, []string{"a.com"}); err == nil {
		t.Fatal("an unparseable Caddyfile must fail the adoption loudly")
	}
}

// --- Policy extraction + maintenance preservation (F48) ---

func TestExtractPolicy(t *testing.T) {
	blk := "myapp.com {\n" +
		"\ttls /etc/caddy/tls/att/abc123.0000000000000001/myapp.crt /etc/caddy/tls/att/abc123.0000000000000001/myapp.key\n" +
		"\tbasic_auth {\n\t\talice $2a$14$hash\n\t}\n" +
		"\tforward_auth authelia:9091 {\n\t\turi /api/authz/forward-auth\n\t\tcopy_headers Remote-User Remote-Groups\n\t}\n" +
		"\treverse_proxy myapp-web-1:3000\n" +
		"}\n"
	pol, err := ExtractPolicy(blk)
	if err != nil {
		t.Fatalf("ExtractPolicy: %v", err)
	}
	if !strings.Contains(pol.TLS, "tls /etc/caddy/tls/att/") {
		t.Errorf("tls directive not extracted: %q", pol.TLS)
	}
	if len(pol.Access) != 2 {
		t.Fatalf("expected basic_auth + forward_auth spans, got %+v", pol.Access)
	}
	if !strings.Contains(pol.Access[0], "alice $2a$14$hash") || !strings.Contains(pol.Access[0], "basic_auth {") {
		t.Errorf("basic_auth span not verbatim: %q", pol.Access[0])
	}
	if !strings.Contains(pol.Access[1], "copy_headers Remote-User Remote-Groups") {
		t.Errorf("forward_auth span not verbatim: %q", pol.Access[1])
	}
}

func TestMaintenanceBlock_PreservesPolicy(t *testing.T) {
	pol := SitePolicy{
		TLS:    "\ttls internal",
		Access: []string{"\tbasic_auth {\n\t\talice $2a$14$hash\n\t}"},
	}
	got := maintenanceBlock([]string{"myapp.com"}, pol)
	if !strings.Contains(got, "tls internal") {
		t.Errorf("maintenance block dropped the TLS directive:\n%s", got)
	}
	if !strings.Contains(got, "basic_auth") || !strings.Contains(got, "alice $2a$14$hash") {
		t.Errorf("maintenance block dropped the access gate:\n%s", got)
	}
	if strings.HasPrefix(got, "http://myapp.com") {
		t.Errorf("a TLS-carrying maintenance block must not downgrade to http://:\n%s", got)
	}
}

// TestSetMaintenance_PreservesPolicy is the F48 end-to-end: enabling
// maintenance on an app with a custom cert and basic auth keeps both — the
// 503 page is served over the SAME TLS with the SAME gate, instead of
// silently downgrading security for the duration.
func TestSetMaintenance_PreservesPolicy(t *testing.T) {
	const existing = "# TEPLOY BEGIN myapp\nmyapp.com {\n" +
		"\ttls /etc/caddy/tls/att/abc123.0000000000000001/myapp.crt /etc/caddy/tls/att/abc123.0000000000000001/myapp.key\n" +
		"\tbasic_auth {\n\t\talice $2a$14$hash\n\t}\n" +
		"\treverse_proxy myapp-web-1:3000\n}\n# TEPLOY END myapp\n"
	exec := newFakeStatefulExecutor(map[string]string{caddyfilePath: existing})
	client := NewClient(exec)

	if err := client.SetMaintenance(context.Background(), "myapp", "myapp.com"); err != nil {
		t.Fatalf("SetMaintenance: %v", err)
	}
	written := exec.file(caddyfilePath)
	if !strings.Contains(written, "respond 503") {
		t.Fatalf("maintenance block not rendered:\n%s", written)
	}
	if !strings.Contains(written, "tls /etc/caddy/tls/att/abc123.0000000000000001/myapp.crt") {
		t.Errorf("TLS directive not carried into maintenance (F48):\n%s", written)
	}
	if !strings.Contains(written, "alice $2a$14$hash") {
		t.Errorf("access gate not carried into maintenance (F48):\n%s", written)
	}
	// The stash still holds the ORIGINAL route for restoration.
	stash := fmt.Sprintf(maintStashFmt, "myapp")
	if s := exec.file(stash); !strings.Contains(s, "reverse_proxy myapp-web-1:3000") {
		t.Errorf("stash did not capture the original route:\n%s", s)
	}
}

// --- Adapt API (F48/F49) ---
//
// No caddy binary exists in this environment (checked at session start),
// so the binary-plumbing tests drive a STUB executable that mimics
// `caddy adapt`'s contract (stdin Caddyfile → stdout JSON, non-zero exit
// with stderr on invalid input). This tests OUR plumbing honestly; the
// real binary's behavior is exercised only when one is installed.

func writeStubCaddy(t *testing.T, dir string) {
	t.Helper()
	script := `#!/bin/sh
# stub caddy adapt: rejects a Caddyfile containing BROKEN, adapts the rest
input=$(cat)
case "$input" in
  *BROKEN*)
    echo 'stub: adapt failed at line 1' >&2
    exit 1
    ;;
esac
printf '{"apps":{"http":{"servers":{}}}}'
`
	path := filepath.Join(dir, "caddy")
	if err := os.WriteFile(path, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
}

func TestAdapter_ResolveAndAdapt(t *testing.T) {
	dir := t.TempDir()
	writeStubCaddy(t, dir)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	adapter := ResolveAdapter()
	if adapter == nil {
		t.Fatal("stub caddy not resolved from PATH")
	}
	out, err := adapter.AdaptCaddyfile(context.Background(), "a.com {\n\trespond 200\n}\n")
	if err != nil {
		t.Fatalf("AdaptCaddyfile: %v", err)
	}
	if !strings.Contains(string(out), `"servers"`) {
		t.Errorf("adapted JSON not returned: %s", out)
	}
	if _, err := adapter.AdaptCaddyfile(context.Background(), "a.com {\n\tBROKEN\n}\n"); err == nil {
		t.Fatal("an unadaptable Caddyfile must error, carrying caddy's stderr")
	} else if !strings.Contains(err.Error(), "stub: adapt failed") {
		t.Errorf("error should carry the adapter's stderr, got: %v", err)
	}
}

func TestValidateViaAdapt_GateRefusesBrokenConfig(t *testing.T) {
	dir := t.TempDir()
	writeStubCaddy(t, dir)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	if err := validateViaAdapt(context.Background(), "a.com {\n\trespond 200\n}\n"); err != nil {
		t.Fatalf("valid config refused: %v", err)
	}
	err := validateViaAdapt(context.Background(), "a.com {\n\tBROKEN\n}\n")
	if err == nil {
		t.Fatal("the adapt gate must refuse a broken Caddyfile before it is written")
	}
}

func TestValidateViaAdapt_NoBinaryIsNoop(t *testing.T) {
	// PATH with no caddy anywhere: the gate is honestly absent, not fake.
	t.Setenv("PATH", t.TempDir())
	if adapter := ResolveAdapter(); adapter != nil {
		t.Fatal("expected no adapter in an empty PATH")
	}
	if err := validateViaAdapt(context.Background(), "anything at all"); err != nil {
		t.Fatalf("absent adapter must be a documented no-op, got %v", err)
	}
}

// TestAdaptCrossCheck_WhenCaddyPresent runs ONLY where a real caddy binary
// is in PATH (a developer machine with caddy installed; CI has none and
// skips). It cross-checks the vendored parser's host extraction against
// `caddy adapt`'s own JSON — the F48/F49 structured representation must
// agree with Caddy's, not just with itself — and proves the F48/F49 OUTPUTS
// (maintenance with preserved policy, partial adoption) adapt cleanly under
// the real binary. This session's run used caddy v2.10.2 built from source.
func TestAdaptCrossCheck_WhenCaddyPresent(t *testing.T) {
	adapter := ResolveAdapter()
	if adapter == nil {
		t.Skip("no caddy binary in PATH — vendored-parser unit tests + stub-binary plumbing tests cover the rest")
	}
	ctx := context.Background()
	realHosts := func(content string) map[string]bool {
		t.Helper()
		out, err := adapter.AdaptCaddyfile(ctx, content)
		if err != nil {
			t.Fatalf("real adapt failed: %v", err)
		}
		var cfg struct {
			Apps struct {
				HTTP struct {
					Servers map[string]struct {
						Routes []struct {
							Match []struct {
								Host []string `json:"host"`
							} `json:"match"`
						} `json:"routes"`
					} `json:"servers"`
				} `json:"http"`
			} `json:"apps"`
		}
		if err := json.Unmarshal(out, &cfg); err != nil {
			t.Fatalf("parsing adapted JSON: %v", err)
		}
		hosts := map[string]bool{}
		for _, srv := range cfg.Apps.HTTP.Servers {
			for _, r := range srv.Routes {
				for _, m := range r.Match {
					for _, h := range m.Host {
						hosts[h] = true
					}
				}
			}
		}
		return hosts
	}
	fixtures := map[string]string{
		"global+snippet+sites": "{\n\tadmin 127.0.0.1:2019\n}\n\n(s) {\n\theader X 1\n}\n\na.com, www.a.com {\n\treverse_proxy a:80\n}\n\nb.com {\n\ttls internal\n\trespond \"hi {ok}\" 200\n}\n",
		"tls+auth":             "myapp.com {\n\ttls /c/a.crt /c/a.key\n\tbasic_auth {\n\t\talice $2a$14$abc\n\t}\n\treverse_proxy x:3000\n}\n",
		"forward_auth":         "myapp.com {\n\tforward_auth auth:9091 {\n\t\turi /api/verify\n\t\tcopy_headers A B\n\t}\n\treverse_proxy x:3000\n}\n",
		"comments":             "# leading comment\na.com { # trailing\n\trespond 200 # done\n}\n",
		"http scheme":          "http://192.168.1.5 {\n\trespond 200\n}\n",
		"multisite":            "a.com {\n\trespond 200\n}\nb.com, c.com {\n\trespond 201\n}\n",
	}
	for name, fx := range fixtures {
		want := realHosts(fx)
		blocks, err := ParseSites(fx)
		if err != nil {
			t.Errorf("%s: parser failed: %v", name, err)
			continue
		}
		got := map[string]bool{}
		for _, b := range blocks {
			for _, h := range addressHosts(b.Addresses) {
				got[h] = true
			}
		}
		if len(got) != len(want) {
			t.Errorf("%s: host sets differ: parser=%v adapt=%v", name, got, want)
			continue
		}
		for h := range got {
			if !want[h] {
				t.Errorf("%s: parser found host %q the real adapter did not: %v", name, h, want)
			}
		}
	}

	// F48/F49 outputs must adapt cleanly under the real binary.
	maint := maintenanceBlock([]string{"myapp.com"}, SitePolicy{TLS: "\ttls internal"})
	if _, err := adapter.AdaptCaddyfile(ctx, maint); err != nil {
		t.Errorf("maintenance block with preserved tls does not adapt under real caddy: %v\n%s", err, maint)
	}
	maintAuth := maintenanceBlock([]string{"myapp.com"}, SitePolicy{Access: []string{"\tbasic_auth {\n\t\talice $2a$14$abc\n\t}"}})
	if _, err := adapter.AdaptCaddyfile(ctx, maintAuth); err != nil {
		t.Errorf("maintenance block with preserved basic_auth does not adapt under real caddy: %v\n%s", err, maintAuth)
	}
	adopted, err := adoptForeignBlocks("old.com, keep.com {\n\treverse_proxy legacy:80\n}\n", []string{"old.com"})
	if err != nil {
		t.Fatalf("adoptForeignBlocks: %v", err)
	}
	if _, err := adapter.AdaptCaddyfile(ctx, adopted); err != nil {
		t.Errorf("partial-adoption result does not adapt under real caddy: %v\n%s", err, adopted)
	}
}

// TestMutate_AdaptGateRefusesBrokenConfig: the SERVER-side adapt gate must
// refuse a transform whose output the server's own caddy cannot adapt —
// before anything is written to disk.
func TestMutate_AdaptGateRefusesBrokenConfig(t *testing.T) {
	exec := newFakeStatefulExecutor(map[string]string{caddyfilePath: "{\n\tadmin 127.0.0.1:2019\n}\n"})
	exec.adaptErr = fmt.Errorf("adapt: unrecognized directive: nonsense")
	client := NewClient(exec)
	err := client.SetRoute(context.Background(), "myapp", "myapp.com", "myapp-web-1:3000", 3000, TLS{}, "", nil, Firewall{}, Access{})
	if err == nil || !strings.Contains(err.Error(), "refusing to write") {
		t.Fatalf("expected the adapt gate to refuse the edit, got: %v", err)
	}
	if w := exec.file(caddyfilePath); strings.Contains(w, "myapp.com") {
		t.Errorf("a refused edit must not change the Caddyfile:\n%s", w)
	}
}
