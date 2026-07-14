package caddy

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/ssh"
)

// lockCmds returns the standard command set for a single locked edit+reload:
// acquire lock, read the Caddyfile, write it back, reload, release lock. The
// mock matches by distinct prefix, so order is irrelevant and unused entries
// (e.g. mv/reload on a no-op) are harmless.
func lockCmds(caddyfile string) []ssh.MockCommand {
	return []ssh.MockCommand{
		{Match: "mkdir " + lockDir, Output: ""},
		{Match: "cat " + caddyfilePath, Output: caddyfile},
		{Match: "mv " + tmpCaddyfile, Output: ""},
		{Match: reloadCmd, Output: ""},
		// Post-reload delivery check: container's file matches what we wrote.
		{Match: "[ \"$(docker exec caddy md5sum", Output: deliveredOK},
		{Match: "rmdir " + lockDir, Output: ""},
	}
}

func calledWith(m *ssh.MockExecutor, prefix string) bool {
	for _, c := range m.Calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func TestSetRoute_WritesBlockAndReloads(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4", lockCmds("{\n\tadmin 127.0.0.1:2019\n}\n")...)

	client := NewClient(mock)
	if err := client.SetRoute(context.Background(), "myapp", "myapp.com", "myapp-v1", 80, TLS{}, "", Firewall{}, Access{}); err != nil {
		t.Fatalf("SetRoute: %v", err)
	}

	got := string(mock.Files[tmpCaddyfile])
	for _, want := range []string{"# TEPLOY BEGIN myapp", "# TEPLOY END myapp", "myapp.com {", "reverse_proxy myapp-v1:80"} {
		if !strings.Contains(got, want) {
			t.Errorf("expected Caddyfile to contain %q\nfull:\n%s", want, got)
		}
	}
	if !calledWith(mock, reloadCmd) {
		t.Error("expected caddy reload to be called")
	}
	if !calledWith(mock, "mkdir "+lockDir) || !calledWith(mock, "rmdir "+lockDir) {
		t.Error("expected the Caddy lock to be acquired and released")
	}
}

func TestSetRoute_UpdateExisting(t *testing.T) {
	existing := "{\n\tadmin 127.0.0.1:2019\n}\n\n" +
		"# TEPLOY BEGIN myapp\nmyapp.com {\n\treverse_proxy myapp-v1:80\n}\n# TEPLOY END myapp\n"
	mock := ssh.NewMockExecutor("1.2.3.4", lockCmds(existing)...)

	client := NewClient(mock)
	if err := client.SetRoute(context.Background(), "myapp", "myapp.com", "myapp-v2", 3000, TLS{}, "", Firewall{}, Access{}); err != nil {
		t.Fatalf("SetRoute: %v", err)
	}

	got := string(mock.Files[tmpCaddyfile])
	if strings.Count(got, "# TEPLOY BEGIN myapp") != 1 {
		t.Errorf("expected exactly one block for myapp:\n%s", got)
	}
	if !strings.Contains(got, "reverse_proxy myapp-v2:3000") {
		t.Errorf("expected updated upstream:\n%s", got)
	}
	if strings.Contains(got, "myapp-v1:80") {
		t.Errorf("old upstream not removed:\n%s", got)
	}
}

// TestSetRoute_AdoptsForeignBlock covers brownfield: a hand-written site block
// for the same host (no Teploy markers) must be replaced so Teploy's block is
// the single authority — no duplicate site address, no route-ordering bug.
func TestSetRoute_AdoptsForeignBlock(t *testing.T) {
	existing := "{\n\tadmin 127.0.0.1:2019\n}\n\n" +
		"other.com {\n\treverse_proxy other:80\n}\n\n" +
		"myapp.com, www.myapp.com {\n\treverse_proxy legacy-container:80\n}\n"
	mock := ssh.NewMockExecutor("1.2.3.4", lockCmds(existing)...)

	client := NewClient(mock)
	if err := client.SetRoute(context.Background(), "myapp", "myapp.com, www.myapp.com", "myapp-v1", 80, TLS{}, "", Firewall{}, Access{}); err != nil {
		t.Fatalf("SetRoute: %v", err)
	}

	got := string(mock.Files[tmpCaddyfile])
	if strings.Contains(got, "legacy-container:80") {
		t.Errorf("foreign block for the same host was not adopted/removed:\n%s", got)
	}
	if strings.Count(got, "myapp.com, www.myapp.com {") != 1 {
		t.Errorf("expected exactly one block for the hosts (no duplicate):\n%s", got)
	}
	if !strings.Contains(got, "# TEPLOY BEGIN myapp") || !strings.Contains(got, "reverse_proxy myapp-v1:80") {
		t.Errorf("expected teploy-managed block for myapp:\n%s", got)
	}
	if !strings.Contains(got, "other.com {") {
		t.Errorf("unrelated foreign block must be preserved:\n%s", got)
	}
}

func TestSetRoute_MultiHost(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4", lockCmds("{\n\tadmin 127.0.0.1:2019\n}\n")...)
	client := NewClient(mock)
	if err := client.SetRoute(context.Background(), "myapp", "myapp.com, www.myapp.com", "myapp-v1", 80, TLS{}, "", Firewall{}, Access{}); err != nil {
		t.Fatalf("SetRoute: %v", err)
	}
	if !strings.Contains(string(mock.Files[tmpCaddyfile]), "myapp.com, www.myapp.com {") {
		t.Errorf("expected multi-host site block:\n%s", mock.Files[tmpCaddyfile])
	}
}

func TestSetRoute_ReloadFailureRollsBack(t *testing.T) {
	initial := "{\n\tadmin 127.0.0.1:2019\n}\n"
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "mkdir " + lockDir, Output: ""},
		ssh.MockCommand{Match: "cat " + caddyfilePath, Output: initial},
		ssh.MockCommand{Match: "mv " + tmpCaddyfile, Output: ""},
		// First reload (new config) fails; rollback reload then succeeds.
		ssh.MockCommand{Match: reloadCmd, Err: fmt.Errorf("invalid config"), Once: true},
		ssh.MockCommand{Match: reloadCmd, Output: ""},
		ssh.MockCommand{Match: "rmdir " + lockDir, Output: ""},
	)

	client := NewClient(mock)
	err := client.SetRoute(context.Background(), "myapp", "myapp.com", "myapp-v1", 80, TLS{}, "", Firewall{}, Access{})
	if err == nil {
		t.Fatal("expected error on reload failure")
	}
	// The last write must restore the original contents (no broken config left on disk).
	if string(mock.Files[tmpCaddyfile]) != initial {
		t.Errorf("expected Caddyfile rolled back to original, got:\n%s", mock.Files[tmpCaddyfile])
	}
}

// TestSetRoute_StaleDeliveryFailsLoudly covers the backstop: the write reaches
// the host file but the running container does not see it (legacy single-file
// mount pinning a stale inode). The post-reload delivery check must turn that
// into a hard error (deploy aborts before the old container is torn down)
// rather than a silent 502, and point at the directory-mount recreate.
func TestSetRoute_StaleDeliveryFailsLoudly(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "mkdir " + lockDir, Output: ""},
		ssh.MockCommand{Match: "cat " + caddyfilePath, Output: "{\n\tadmin 127.0.0.1:2019\n}\n"},
		ssh.MockCommand{Match: "mv " + tmpCaddyfile, Output: ""},
		ssh.MockCommand{Match: reloadCmd, Output: ""},
		ssh.MockCommand{Match: "[ \"$(docker exec caddy md5sum", Output: deliveredStale},
		ssh.MockCommand{Match: "rmdir " + lockDir, Output: ""},
	)

	client := NewClient(mock)
	err := client.SetRoute(context.Background(), "myapp", "myapp.com", "myapp-v1", 80, TLS{}, "", Firewall{}, Access{})
	if err == nil {
		t.Fatal("expected SetRoute to fail when the config did not reach the container")
	}
	if !strings.Contains(err.Error(), "directory mount") {
		t.Errorf("expected a stale-delivery error pointing at the directory-mount recreate, got: %v", err)
	}
	if !calledWith(mock, "rmdir "+lockDir) {
		t.Error("expected the caddy lock to be released after a failed delivery check")
	}
}

func TestSetLoadBalancer(t *testing.T) {
	mock := ssh.NewMockExecutor("10.0.0.100", lockCmds("{\n\tadmin 127.0.0.1:2019\n}\n")...)
	client := NewClient(mock)
	upstreams := []Upstream{{Dial: "10.0.0.1:80"}, {Dial: "10.0.0.2:80"}, {Dial: "10.0.0.3:80"}}
	if err := client.SetLoadBalancer(context.Background(), "myapp", "myapp.com", upstreams, TLS{}, "", Firewall{}, Access{}); err != nil {
		t.Fatalf("SetLoadBalancer: %v", err)
	}

	got := string(mock.Files[tmpCaddyfile])
	for _, want := range []string{
		"# TEPLOY BEGIN myapp",
		"reverse_proxy 10.0.0.1:80 10.0.0.2:80 10.0.0.3:80",
		"lb_policy round_robin",
		"health_uri /up",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q in:\n%s", want, got)
		}
	}
}

func TestSetRoute_WithCaddyExtra(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4", lockCmds("{\n\tadmin 127.0.0.1:2019\n}\n")...)
	client := NewClient(mock)
	extra := "@waitlist path /api/waitlist*\nreverse_proxy @waitlist teploy-waitlist:8080"
	if err := client.SetRoute(context.Background(), "myapp", "myapp.com", "myapp-v1", 80, TLS{}, extra, Firewall{}, Access{}); err != nil {
		t.Fatalf("SetRoute: %v", err)
	}
	got := string(mock.Files[tmpCaddyfile])
	for _, want := range []string{
		"reverse_proxy myapp-v1:80",
		"# user-supplied caddy_extra:",
		"@waitlist path /api/waitlist*",
		"reverse_proxy @waitlist teploy-waitlist:8080",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q in:\n%s", want, got)
		}
	}
}

func TestSetLoadBalancer_WithCaddyExtra(t *testing.T) {
	mock := ssh.NewMockExecutor("10.0.0.100", lockCmds("{\n\tadmin 127.0.0.1:2019\n}\n")...)
	client := NewClient(mock)
	upstreams := []Upstream{{Dial: "10.0.0.1:80"}, {Dial: "10.0.0.2:80"}}
	extra := "rate_limit 100r/m"
	if err := client.SetLoadBalancer(context.Background(), "myapp", "myapp.com", upstreams, TLS{}, extra, Firewall{}, Access{}); err != nil {
		t.Fatalf("SetLoadBalancer: %v", err)
	}
	got := string(mock.Files[tmpCaddyfile])
	for _, want := range []string{
		"lb_policy round_robin",
		"# user-supplied caddy_extra:",
		"rate_limit 100r/m",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q in:\n%s", want, got)
		}
	}
}

func TestRemoveRoute(t *testing.T) {
	existing := "{\n\tadmin 127.0.0.1:2019\n}\n\n" +
		"other.com {\n\treverse_proxy other:80\n}\n\n" +
		"# TEPLOY BEGIN myapp\nmyapp.com {\n\treverse_proxy myapp-v1:80\n}\n# TEPLOY END myapp\n"
	mock := ssh.NewMockExecutor("1.2.3.4", lockCmds(existing)...)

	client := NewClient(mock)
	if err := client.RemoveRoute(context.Background(), "myapp"); err != nil {
		t.Fatalf("RemoveRoute: %v", err)
	}

	got := string(mock.Files[tmpCaddyfile])
	if strings.Contains(got, "# TEPLOY BEGIN myapp") {
		t.Errorf("expected myapp block removed:\n%s", got)
	}
	if !strings.Contains(got, "other.com") {
		t.Errorf("unrelated content not preserved:\n%s", got)
	}
}

func TestRemoveRoute_NoConfig(t *testing.T) {
	// No myapp block present → no change → mutate skips write+reload.
	mock := ssh.NewMockExecutor("1.2.3.4", lockCmds("{\n\tadmin 127.0.0.1:2019\n}\n")...)
	client := NewClient(mock)
	if err := client.RemoveRoute(context.Background(), "myapp"); err != nil {
		t.Fatalf("RemoveRoute should be a no-op when absent: %v", err)
	}
	if calledWith(mock, reloadCmd) {
		t.Error("expected no reload when nothing changed")
	}
}

func TestSetMaintenance(t *testing.T) {
	existing := "{\n\tadmin 127.0.0.1:2019\n}\n\n" +
		"# TEPLOY BEGIN myapp\nmyapp.com {\n\treverse_proxy myapp:80\n}\n# TEPLOY END myapp\n"
	mock := ssh.NewMockExecutor("1.2.3.4", lockCmds(existing)...)

	client := NewClient(mock)
	if err := client.SetMaintenance(context.Background(), "myapp", "myapp.com"); err != nil {
		t.Fatalf("SetMaintenance: %v", err)
	}

	got := string(mock.Files[tmpCaddyfile])
	if !strings.Contains(got, "respond 503") || !strings.Contains(got, "We'll be back soon") {
		t.Errorf("expected 503 maintenance block:\n%s", got)
	}
	// The prior route block must be stashed for restore.
	stash := string(mock.Files[fmt.Sprintf(maintStashFmt, "myapp")])
	if !strings.Contains(stash, "reverse_proxy myapp:80") {
		t.Errorf("expected original route stashed, got:\n%s", stash)
	}
}

func TestRemoveMaintenance(t *testing.T) {
	existing := "{\n\tadmin 127.0.0.1:2019\n}\n\n" +
		"# TEPLOY BEGIN myapp\nmyapp.com {\n\trespond 503\n}\n# TEPLOY END myapp\n"
	cmds := append(lockCmds(existing),
		ssh.MockCommand{Match: "test -f " + fmt.Sprintf(maintStashFmt, "myapp"), Output: ""},
		ssh.MockCommand{Match: "cat " + fmt.Sprintf(maintStashFmt, "myapp"), Output: "myapp.com {\n\treverse_proxy myapp:80\n}"},
		ssh.MockCommand{Match: "rm -f " + fmt.Sprintf(maintStashFmt, "myapp"), Output: ""},
	)
	mock := ssh.NewMockExecutor("1.2.3.4", cmds...)

	client := NewClient(mock)
	if err := client.RemoveMaintenance(context.Background(), "myapp"); err != nil {
		t.Fatalf("RemoveMaintenance: %v", err)
	}

	got := string(mock.Files[tmpCaddyfile])
	if !strings.Contains(got, "reverse_proxy myapp:80") {
		t.Errorf("expected route restored from stash:\n%s", got)
	}
	if strings.Contains(got, "respond 503") {
		t.Errorf("expected maintenance block removed:\n%s", got)
	}
	if !calledWith(mock, "rm -f "+fmt.Sprintf(maintStashFmt, "myapp")) {
		t.Error("expected the maintenance stash to be cleaned up")
	}
}

func TestRemoveForeignHostBlocks(t *testing.T) {
	in := "{\n\tadmin 127.0.0.1:2019\n}\n\n" +
		"keep.com {\n\treverse_proxy keep:80\n}\n\n" +
		"drop.com, www.drop.com {\n\treverse_proxy old:80\n}\n\n" +
		"# TEPLOY BEGIN protected\ndrop.com {\n\treverse_proxy managed:80\n}\n# TEPLOY END protected\n"
	got := removeForeignHostBlocks(in, []string{"drop.com"})
	if strings.Contains(got, "reverse_proxy old:80") {
		t.Errorf("foreign drop.com block not removed:\n%s", got)
	}
	if !strings.Contains(got, "keep.com {") {
		t.Errorf("unrelated block removed:\n%s", got)
	}
	if !strings.Contains(got, "# TEPLOY BEGIN protected") || !strings.Contains(got, "reverse_proxy managed:80") {
		t.Errorf("teploy-managed block inside markers must be preserved:\n%s", got)
	}
	if !strings.Contains(got, "admin 127.0.0.1:2019") {
		t.Errorf("global options block must be preserved:\n%s", got)
	}
}

func TestAddressMatchesHosts(t *testing.T) {
	cases := []struct {
		addr  string
		hosts []string
		want  bool
	}{
		{"example.com", []string{"example.com"}, true},
		{"example.com, www.example.com", []string{"www.example.com"}, true},
		{"https://example.com", []string{"example.com"}, true},
		{"example.com:8080", []string{"example.com"}, false}, // explicit port is a different address
		{"other.com", []string{"example.com"}, false},
	}
	for _, c := range cases {
		if got := addressMatchesHosts(c.addr, c.hosts); got != c.want {
			t.Errorf("addressMatchesHosts(%q, %v) = %v, want %v", c.addr, c.hosts, got, c.want)
		}
	}
}

func TestRemoveCaddyfileBlock(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		app      string
		expected string
	}{
		{"removes single block", "a\n\n# TEPLOY BEGIN x\nbody\n# TEPLOY END x\n\nb\n", "x", "a\n\nb\n"},
		{"no-op when marker absent", "a\n\nb\n", "x", "a\n\nb\n"},
		{"removes multiple blocks for same app", "# TEPLOY BEGIN x\n1\n# TEPLOY END x\nmid\n# TEPLOY BEGIN x\n2\n# TEPLOY END x\n", "x", "mid\n"},
		{"only removes matching app, not others", "# TEPLOY BEGIN x\nx\n# TEPLOY END x\n# TEPLOY BEGIN y\ny\n# TEPLOY END y\n", "x", "# TEPLOY BEGIN y\ny\n# TEPLOY END y\n"},
		{"malformed (missing end marker) truncates at begin", "a\n# TEPLOY BEGIN x\nb\nc\n", "x", "a\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			begin := fmt.Sprintf(markerBeginFmt, tt.app)
			end := fmt.Sprintf(markerEndFmt, tt.app)
			if got := removeCaddyfileBlock(tt.input, begin, end); got != tt.expected {
				t.Errorf("removeCaddyfileBlock:\ninput:    %q\nexpected: %q\ngot:      %q", tt.input, tt.expected, got)
			}
		})
	}
}

func TestReverseProxyBlock(t *testing.T) {
	got := reverseProxyBlock([]string{"example.com"}, "myapp-v1", 8080, TLS{}, "", Firewall{}, Access{})
	want := "example.com {\n\treverse_proxy myapp-v1:8080\n}"
	if got != want {
		t.Errorf("reverseProxyBlock:\nwant: %q\ngot:  %q", want, got)
	}
}

func TestReverseProxyBlockMultiHost(t *testing.T) {
	got := reverseProxyBlock([]string{"example.com", "www.example.com"}, "myapp", 80, TLS{}, "", Firewall{}, Access{})
	want := "example.com, www.example.com {\n\treverse_proxy myapp:80\n}"
	if got != want {
		t.Errorf("multi-host:\nwant: %q\ngot:  %q", want, got)
	}
}

func TestLoadBalancerBlock(t *testing.T) {
	got := loadBalancerBlock([]string{"example.com"}, []Upstream{{Dial: "a:80"}, {Dial: "b:80"}}, TLS{}, "", Firewall{}, Access{})
	for _, want := range []string{"example.com {", "reverse_proxy a:80 b:80 {", "lb_policy round_robin", "health_uri /up", "health_interval 10s", "health_timeout 5s"} {
		if !strings.Contains(got, want) {
			t.Errorf("loadBalancerBlock missing %q\nfull:\n%s", want, got)
		}
	}
}

func TestReverseProxyBlock_WithTLS(t *testing.T) {
	got := reverseProxyBlock([]string{"fylun.ai"}, "fylun-web-v1", 3000,
		TLS{Cert: "/etc/caddy/tls/fylun-web.crt", Key: "/etc/caddy/tls/fylun-web.key"}, "", Firewall{}, Access{})
	want := "fylun.ai {\n\ttls /etc/caddy/tls/fylun-web.crt /etc/caddy/tls/fylun-web.key\n\treverse_proxy fylun-web-v1:3000\n}"
	if got != want {
		t.Errorf("reverseProxyBlock with TLS:\nwant: %q\ngot:  %q", want, got)
	}
}

func TestLoadBalancerBlock_WithTLS(t *testing.T) {
	got := loadBalancerBlock([]string{"fylun.ai"}, []Upstream{{Dial: "a:3000"}, {Dial: "b:3000"}},
		TLS{Cert: "/etc/caddy/tls/fylun-web.crt", Key: "/etc/caddy/tls/fylun-web.key"}, "", Firewall{}, Access{})
	if !strings.Contains(got, "\ttls /etc/caddy/tls/fylun-web.crt /etc/caddy/tls/fylun-web.key\n") {
		t.Errorf("loadBalancerBlock with TLS missing tls directive\nfull:\n%s", got)
	}
	// The tls directive must come before the reverse_proxy directive.
	if strings.Index(got, "tls ") > strings.Index(got, "reverse_proxy") {
		t.Errorf("tls directive must precede reverse_proxy\nfull:\n%s", got)
	}
}

// TestIsPubliclyRoutable is the detection helper behind the non-public
// domain fix below: without it, a bare IP or LAN address in `domain:`
// makes Caddy attempt (and hang on) a real ACME challenge it can never
// complete.
func TestIsPubliclyRoutable(t *testing.T) {
	cases := []struct {
		host string
		want bool
	}{
		{"example.com", true},
		{"myapp.example.com", true},
		{"192.168.1.114", false}, // the investment-club incident this fix is for
		{"10.0.0.5", false},
		{"172.16.0.1", false},
		{"127.0.0.1", false},
		{"203.0.113.5", false}, // a real public IP still can't get an ACME cert bare
		{"::1", false},
		{"2001:db8::1", false},
	}
	for _, c := range cases {
		if got := IsPubliclyRoutable(c.host); got != c.want {
			t.Errorf("IsPubliclyRoutable(%q) = %v, want %v", c.host, got, c.want)
		}
	}
}

// TestReverseProxyBlock_NonPublicDomainGetsPlainHTTP is the direct
// regression test for tonight's incident: a bare-IP domain used to render
// as a plain `192.168.1.114 { ... }` site address, and Caddy's default
// automatic HTTPS then hung forever trying an ACME challenge it could
// never complete (HTTP 308 redirect to a TLS handshake that never
// finishes). It must now get an explicit http:// scheme instead, which
// tells Caddy not to manage TLS for that address.
func TestReverseProxyBlock_NonPublicDomainGetsPlainHTTP(t *testing.T) {
	got := reverseProxyBlock([]string{"192.168.1.114"}, "investment-club-v1", 3000, TLS{}, "", Firewall{}, Access{})
	want := "http://192.168.1.114 {\n\treverse_proxy investment-club-v1:3000\n}"
	if got != want {
		t.Errorf("reverseProxyBlock for a bare IP:\nwant: %q\ngot:  %q", want, got)
	}
}

func TestStaticBlock_NonPublicDomainGetsPlainHTTP(t *testing.T) {
	got := StaticBlock(StaticBlockOpts{Hosts: []string{"192.168.1.114"}, Root: "/srv/static/investment-club/current"})
	if !strings.HasPrefix(got, "http://192.168.1.114 {") {
		t.Errorf("StaticBlock for a bare IP should start with http://, got:\n%s", got)
	}
}

// TestReverseProxyBlock_TLSInternalKeepsRealHost confirms the opt-in
// escape hatch: a non-public host with tls.internal set should NOT get
// the http:// fallback — the operator explicitly asked for HTTPS via
// Caddy's local CA, so automatic-HTTPS avoidance would be wrong here.
func TestReverseProxyBlock_TLSInternalKeepsRealHost(t *testing.T) {
	got := reverseProxyBlock([]string{"192.168.1.114"}, "myapp-v1", 3000, TLS{Internal: true}, "", Firewall{}, Access{})
	want := "192.168.1.114 {\n\ttls internal\n\treverse_proxy myapp-v1:3000\n}"
	if got != want {
		t.Errorf("reverseProxyBlock with tls.internal:\nwant: %q\ngot:  %q", want, got)
	}
}

// A custom cert is also an explicit opt-in into managed TLS, same as
// tls.internal — must not get the http:// fallback either.
func TestReverseProxyBlock_CustomCertKeepsRealHost(t *testing.T) {
	got := reverseProxyBlock([]string{"192.168.1.114"}, "myapp-v1", 3000,
		TLS{Cert: "/etc/caddy/tls/myapp.crt", Key: "/etc/caddy/tls/myapp.key"}, "", Firewall{}, Access{})
	if strings.HasPrefix(got, "http://") {
		t.Errorf("a custom cert should keep the real host, not fall back to http://:\n%s", got)
	}
}

func TestMaintenanceBlock_NonPublicDomainGetsPlainHTTP(t *testing.T) {
	got := maintenanceBlock([]string{"192.168.1.114"})
	if !strings.HasPrefix(got, "http://192.168.1.114 {") {
		t.Errorf("maintenanceBlock for a bare IP should start with http://, got:\n%s", got)
	}
}

// Public hosts must be completely unaffected — no regression for the
// overwhelmingly common case of a real domain name.
func TestSiteAddresses_PublicHostUnchanged(t *testing.T) {
	got := siteAddresses([]string{"example.com", "www.example.com"}, TLS{})
	want := []string{"example.com", "www.example.com"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("siteAddresses[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestTLS_Directive_EmptyWhenUnset(t *testing.T) {
	if (TLS{}).directive() != "" {
		t.Error("empty TLS should produce no directive")
	}
	if (TLS{Cert: "x"}).directive() != "" {
		t.Error("TLS with only cert (no key) should produce no directive")
	}
	if (TLS{Key: "y"}).directive() != "" {
		t.Error("TLS with only key (no cert) should produce no directive")
	}
}

func TestParseDomains(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"example.com", []string{"example.com"}},
		{"example.com, www.example.com", []string{"example.com", "www.example.com"}},
		{"a,b,c", []string{"a", "b", "c"}},
		{"  apex.dev  ,  www.apex.dev  ", []string{"apex.dev", "www.apex.dev"}},
		{"", nil},
		{",,,", nil},
	}
	for _, tt := range tests {
		got, err := parseDomains(tt.in)
		if err != nil {
			t.Errorf("parseDomains(%q) unexpected error: %v", tt.in, err)
			continue
		}
		if len(got) != len(tt.want) {
			t.Errorf("parseDomains(%q) = %v, want %v", tt.in, got, tt.want)
			continue
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("parseDomains(%q)[%d] = %q, want %q", tt.in, i, got[i], tt.want[i])
			}
		}
	}

	// Injection attempts must be rejected.
	for _, bad := range []string{"ex ample.com", "evil.com {\n}", "a.com#c", "a\nb", `a"b`} {
		if _, err := parseDomains(bad); err == nil {
			t.Errorf("parseDomains(%q) should have rejected a Caddyfile-breaking character", bad)
		}
	}
}
