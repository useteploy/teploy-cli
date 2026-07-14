package caddy

import (
	"strings"
	"testing"
)

func TestFirewall_EmptyIsNoOp(t *testing.T) {
	if !(Firewall{}).Empty() {
		t.Fatal("zero Firewall should be Empty")
	}
	// A block with no firewall must be byte-identical to the pre-firewall form.
	got := reverseProxyBlock([]string{"example.com"}, "app-v1", 8080, TLS{}, "", Firewall{}, Access{})
	want := "example.com {\n\treverse_proxy app-v1:8080\n}"
	if got != want {
		t.Errorf("empty firewall changed the block:\nwant %q\ngot  %q", want, got)
	}
}

func TestReverseProxyBlock_WithFirewall(t *testing.T) {
	fw := Firewall{
		AllowIPs:        []string{"10.0.0.0/8", "1.2.3.4"},
		DenyIPs:         []string{"9.9.9.9"},
		BlockUserAgents: []string{"badbot", "masscan"},
		MaxBodySize:     "10MB",
	}
	got := reverseProxyBlock([]string{"example.com"}, "app-v1", 8080, TLS{}, "", fw, Access{})

	// reverse_proxy must be inside the catch-all handle (runs AFTER the blocks).
	if !strings.Contains(got, "\thandle {\n\t\treverse_proxy app-v1:8080\n\t}\n") {
		t.Errorf("reverse_proxy not wrapped in catch-all handle:\n%s", got)
	}
	for _, want := range []string{
		"\trequest_body {\n\t\tmax_size 10MB\n\t}\n",
		"\t@teploy_fw_deny remote_ip 9.9.9.9\n",
		"\t@teploy_fw_bots header_regexp User-Agent (?i)(badbot|masscan)\n",
		"\t@teploy_fw_notallow not remote_ip 10.0.0.0/8 1.2.3.4\n",
		"\thandle @teploy_fw_deny {\n\t\trespond 403\n\t}\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	// The deny handle must appear before the catch-all reverse_proxy handle.
	if strings.Index(got, "@teploy_fw_deny") > strings.Index(got, "reverse_proxy app-v1") {
		t.Errorf("blocking handles must precede the proxy handle:\n%s", got)
	}
}

func TestFirewall_UserAgentRegexEscaped(t *testing.T) {
	fw := Firewall{BlockUserAgents: []string{"evil.bot", "a+b"}}
	got := fw.prelude()
	// Regex metachars must be escaped so they match literally, not as regex.
	if !strings.Contains(got, `(?i)(evil\.bot|a\+b)`) {
		t.Errorf("user-agent regex not escaped:\n%s", got)
	}
}

func TestLoadBalancerBlock_WithFirewall(t *testing.T) {
	fw := Firewall{DenyIPs: []string{"9.9.9.9"}}
	got := loadBalancerBlock([]string{"example.com"},
		[]Upstream{{Dial: "a:80"}, {Dial: "b:80"}}, TLS{}, "", fw, Access{})
	if !strings.Contains(got, "\thandle {\n\t\treverse_proxy a:80 b:80 {") {
		t.Errorf("LB reverse_proxy not wrapped in handle:\n%s", got)
	}
	if !strings.Contains(got, "@teploy_fw_deny remote_ip 9.9.9.9") {
		t.Errorf("deny matcher missing:\n%s", got)
	}
}
