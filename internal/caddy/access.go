package caddy

import (
	"fmt"
	"sort"
	"strings"
)

// Access is the per-app inbound access gate rendered into the Caddy site block:
// HTTP basic auth (a shared password) and/or forward-auth to an external
// identity provider (Authelia, oauth2-proxy, the customer's OIDC). This is the
// self-hostable half of "deployment protection" — put an app or preview behind
// a login without a managed service. IP allow/deny lives in Firewall.
type Access struct {
	// BasicAuthUsers maps username -> bcrypt hash (Caddy requires bcrypt).
	BasicAuthUsers map[string]string
	// ForwardAuth delegates authn to an external proxy; empty URL disables it.
	ForwardAuthURL         string
	ForwardAuthURI         string
	ForwardAuthCopyHeaders []string
}

// Empty reports whether no access gate is configured.
func (a Access) Empty() bool {
	return len(a.BasicAuthUsers) == 0 && a.ForwardAuthURL == ""
}

// render emits the access directives (tab-indented for a site block). These are
// authentication directives, which Caddy orders before reverse_proxy, so they
// gate every request regardless of where they sit textually.
func (a Access) render() string {
	if a.Empty() {
		return ""
	}
	var b strings.Builder

	if len(a.BasicAuthUsers) > 0 {
		b.WriteString("\tbasic_auth {\n")
		// Deterministic order for stable Caddyfile output.
		users := make([]string, 0, len(a.BasicAuthUsers))
		for u := range a.BasicAuthUsers {
			users = append(users, u)
		}
		sort.Strings(users)
		for _, u := range users {
			fmt.Fprintf(&b, "\t\t%s %s\n", u, a.BasicAuthUsers[u])
		}
		b.WriteString("\t}\n")
	}

	if a.ForwardAuthURL != "" {
		fmt.Fprintf(&b, "\tforward_auth %s {\n", a.ForwardAuthURL)
		if a.ForwardAuthURI != "" {
			fmt.Fprintf(&b, "\t\turi %s\n", a.ForwardAuthURI)
		}
		if len(a.ForwardAuthCopyHeaders) > 0 {
			fmt.Fprintf(&b, "\t\tcopy_headers %s\n", strings.Join(a.ForwardAuthCopyHeaders, " "))
		}
		b.WriteString("\t}\n")
	}

	return b.String()
}
