package caddy

import (
	"strings"
	"testing"
)

func TestAccess_EmptyIsNoOp(t *testing.T) {
	if !(Access{}).Empty() {
		t.Fatal("zero Access should be Empty")
	}
	got := reverseProxyBlock([]string{"example.com"}, "app-v1", 8080, TLS{}, "", Firewall{}, Access{})
	if got != "example.com {\n\treverse_proxy app-v1:8080\n}" {
		t.Errorf("empty access changed the block:\n%s", got)
	}
}

func TestAccess_BasicAuth(t *testing.T) {
	acc := Access{BasicAuthUsers: map[string]string{
		"bob":   "$2a$14$bbbbbbbbbbbbbbbbbbbbbb",
		"alice": "$2a$14$aaaaaaaaaaaaaaaaaaaaaa",
	}}
	got := reverseProxyBlock([]string{"example.com"}, "app-v1", 8080, TLS{}, "", Firewall{}, acc)
	// Deterministic (sorted) user order + before reverse_proxy.
	want := "\tbasic_auth {\n\t\talice $2a$14$aaaaaaaaaaaaaaaaaaaaaa\n\t\tbob $2a$14$bbbbbbbbbbbbbbbbbbbbbb\n\t}\n"
	if !strings.Contains(got, want) {
		t.Errorf("basic_auth block missing/mis-ordered:\n%s", got)
	}
	if strings.Index(got, "basic_auth") > strings.Index(got, "reverse_proxy") {
		t.Errorf("basic_auth must precede reverse_proxy:\n%s", got)
	}
}

func TestAccess_ForwardAuth(t *testing.T) {
	acc := Access{
		ForwardAuthURL:         "authelia:9091",
		ForwardAuthURI:         "/api/authz/forward-auth",
		ForwardAuthCopyHeaders: []string{"Remote-User", "Remote-Email"},
	}
	got := reverseProxyBlock([]string{"example.com"}, "app-v1", 8080, TLS{}, "", Firewall{}, acc)
	for _, want := range []string{
		"\tforward_auth authelia:9091 {\n",
		"\t\turi /api/authz/forward-auth\n",
		"\t\tcopy_headers Remote-User Remote-Email\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}
