package caddy

import (
	"fmt"
	"regexp"
	"strings"
)

// Firewall is the per-app edge hardening rendered into the Caddy site block:
// IP allow/deny, user-agent blocking, and a request-body size cap. It is the
// lightweight, Caddy-native slice — not a full WAF (rate limiting needs a
// custom Caddy build; true bot/DDoS defense is Cloudflare's job).
type Firewall struct {
	AllowIPs        []string // if non-empty, ONLY these IPs/CIDRs are allowed
	DenyIPs         []string // these IPs/CIDRs are blocked
	BlockUserAgents []string // requests whose User-Agent contains any of these are blocked
	MaxBodySize     string   // request body cap, e.g. "10MB" (empty = unlimited)
}

// Empty reports whether no firewall rules are configured.
func (f Firewall) Empty() bool {
	return len(f.AllowIPs) == 0 && len(f.DenyIPs) == 0 &&
		len(f.BlockUserAgents) == 0 && f.MaxBodySize == ""
}

// prelude renders the firewall directives that precede the terminal handler in
// a site block (each line tab-indented for a top-level block). Blocking is done
// with matched `handle { respond 403 }` blocks so it executes BEFORE the
// catch-all `handle` that holds reverse_proxy — Caddy evaluates handle blocks
// in source order, sidestepping the global directive-ordering footgun that
// makes a bare `respond`/`abort` fire after reverse_proxy.
func (f Firewall) prelude() string {
	var b strings.Builder

	if f.MaxBodySize != "" {
		b.WriteString("\trequest_body {\n")
		fmt.Fprintf(&b, "\t\tmax_size %s\n", f.MaxBodySize)
		b.WriteString("\t}\n")
	}

	if len(f.DenyIPs) > 0 {
		fmt.Fprintf(&b, "\t@teploy_fw_deny remote_ip %s\n", strings.Join(f.DenyIPs, " "))
		b.WriteString("\thandle @teploy_fw_deny {\n\t\trespond 403\n\t}\n")
	}

	if len(f.BlockUserAgents) > 0 {
		escaped := make([]string, len(f.BlockUserAgents))
		for i, ua := range f.BlockUserAgents {
			escaped[i] = regexp.QuoteMeta(ua)
		}
		fmt.Fprintf(&b, "\t@teploy_fw_bots header_regexp User-Agent (?i)(%s)\n", strings.Join(escaped, "|"))
		b.WriteString("\thandle @teploy_fw_bots {\n\t\trespond 403\n\t}\n")
	}

	if len(f.AllowIPs) > 0 {
		fmt.Fprintf(&b, "\t@teploy_fw_notallow not remote_ip %s\n", strings.Join(f.AllowIPs, " "))
		b.WriteString("\thandle @teploy_fw_notallow {\n\t\trespond 403\n\t}\n")
	}

	return b.String()
}

// wrapTerminal renders the site-block body for the given terminal handler
// lines (e.g. a reverse_proxy directive). When firewall rules exist, the
// terminal handler is placed in a catch-all `handle { }` after the blocking
// handles; otherwise the terminal lines are emitted as-is. terminalBody is
// already tab-indented for the site-block level.
func (f Firewall) wrapTerminal(terminalBody string) string {
	if f.Empty() {
		return terminalBody
	}
	var b strings.Builder
	b.WriteString(f.prelude())
	b.WriteString("\thandle {\n")
	for _, line := range strings.Split(strings.TrimRight(terminalBody, "\n"), "\n") {
		if line == "" {
			b.WriteString("\n")
		} else {
			b.WriteString("\t" + line + "\n")
		}
	}
	b.WriteString("\t}\n")
	return b.String()
}
