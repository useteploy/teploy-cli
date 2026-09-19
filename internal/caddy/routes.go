// Structured Caddyfile site-block representation (audits F48/F49).
//
// The managed-block machinery above this file works on string fragments:
// adoption of a foreign (hand-written) block is decided by brace counting
// and whole-block replacement, and maintenance mode cannot carry the site's
// TLS/access policy because there is no structure to extract it from. This
// file is the vendored structural parser both of those need — no caddy
// binary required. It is deliberately small: it recognizes top-level site
// blocks (address line + brace-balanced body), the global options block,
// named snippets, comments, quoted strings, and Caddyfile heredocs, and it
// FAILS LOUDLY (returns an error) on anything it cannot represent
// faithfully: unbalanced braces, top-level `import` (imports must be
// resolved against snippets/paths this parser does not model). A parse
// failure aborts the edit before anything is written — the same posture
// the whole-block-only rule had, one notch earlier.
//
// Known limitation, shared with every line-oriented consumer of the
// Caddyfile in this package: braces inside quoted arguments on a directive
// line are counted naively (quotes are recognized, but a brace inside a
// backtick heredoc opener is not). Teploy-rendered blocks never emit that
// shape; a hand-written one that does fails the balance check rather than
// mis-adopting.

package caddy

import (
	"fmt"
	"strings"
)

// SiteBlock is one top-level Caddyfile site block, kept verbatim.
type SiteBlock struct {
	// Addresses are the raw site-address tokens from the block's address
	// line, comma-split and whitespace-trimmed but otherwise untouched
	// (scheme, port, path included as written).
	Addresses []string
	// Lines is the block verbatim: address line, body, closing brace.
	Lines []string
}

// logicalLines merges physical lines while an unpaired backtick literal is
// open — Caddy backtick literals span lines, and the maintenance page body
// is one (it also carries CSS braces that must never be depth-counted).
// Merged entries keep their embedded newlines so re-emission stays
// verbatim.
func logicalLines(lines []string) []string {
	var out []string
	var buf []string
	open := false
	flush := func() {
		if len(buf) > 0 {
			out = append(out, strings.Join(buf, "\n"))
			buf = nil
		}
	}
	for _, l := range lines {
		if open {
			buf = append(buf, l)
			if oddBackticks(l) {
				open = false
				flush()
			}
			continue
		}
		if oddBackticks(l) {
			buf = []string{l}
			open = true
			continue
		}
		out = append(out, l)
	}
	flush()
	return out
}

// oddBackticks reports whether the line's backtick count is odd (a literal
// opened or closed but not both). Backticks inside single/double quotes are
// rare enough in Caddyfiles that parity is the honest heuristic; a mismatch
// degrades to a merged logical line, never to silently mis-parsed braces.
func oddBackticks(line string) bool {
	n := strings.Count(line, "`")
	return n%2 == 1
}

// ParseSites parses content into its top-level site blocks. Everything that
// is not a site block (global options, snippets, comments, blank lines) is
// skipped — callers only need blocks; re-rendering the whole Caddyfile is
// explicitly NOT a goal (managed edits stay surgical).
func ParseSites(content string) ([]SiteBlock, error) {
	lines := logicalLines(strings.Split(content, "\n"))
	var blocks []SiteBlock
	depth := 0
	var cur *SiteBlock
	for i := 0; i < len(lines); i++ {
		raw := lines[i]
		code, err := codeLine(raw)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", i+1, err)
		}
		// Heredoc bodies are verbatim: skip to the terminator.
		if term, ok := heredocTerminator(code); ok {
			for i+1 < len(lines) {
				i++
				if strings.TrimSpace(lines[i]) == term {
					break
				}
			}
			if cur != nil {
				cur.Lines = append(cur.Lines, raw)
			}
			continue
		}
		trimmed := strings.TrimSpace(code)
		net := braceNet(code)

		if depth == 0 {
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}
			if strings.HasPrefix(trimmed, "import") && (len(trimmed) == len("import") || trimmed[len("import")] == ' ') {
				return nil, fmt.Errorf("top-level %q cannot be structurally resolved — refusing to edit a Caddyfile whose site blocks may be defined elsewhere", trimmed)
			}
			if trimmed == "{" || strings.HasPrefix(trimmed, "(") {
				// Global options block or named snippet: not a site
				// block. Consume its body.
				depth = net
				continue
			}
			if !strings.HasSuffix(trimmed, "{") || net <= 0 {
				// A top-level bare directive (no site address, e.g. email
				// inside nothing) — Caddy rejects these outside blocks, so
				// treat as unparseable rather than silently skipping.
				return nil, fmt.Errorf("unrecognized top-level line %q — refusing to edit a Caddyfile that does not parse as site blocks", trimmed)
			}
			addr := strings.TrimSpace(strings.TrimSuffix(trimmed, "{"))
			cur = &SiteBlock{Addresses: splitAddressLine(addr), Lines: []string{raw}}
			depth = net
			continue
		}

		// Inside a block (site, global, or snippet — only site blocks
		// accumulate).
		if cur != nil {
			cur.Lines = append(cur.Lines, raw)
		}
		depth += net
		if depth == 0 {
			if cur != nil {
				blocks = append(blocks, *cur)
				cur = nil
			}
		}
		if depth < 0 {
			return nil, fmt.Errorf("unbalanced '}' at line %d", i+1)
		}
	}
	if depth != 0 {
		return nil, fmt.Errorf("unbalanced '{' — block never closed")
	}
	return blocks, nil
}

// codeLine strips a trailing comment from a line (an unquoted '#'), keeping
// the line's leading whitespace — indentation is part of the verbatim
// block. Quotes are respected so a '#' inside an argument does not start a
// comment.
func codeLine(raw string) (string, error) {
	var quote byte
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"' || c == '`':
			quote = c
		case c == '\\':
			i++
		case c == '#':
			return raw[:i], nil
		}
	}
	if quote != 0 {
		// An unterminated quote mid-line: the Caddyfile tokenizer would
		// fail; so do we.
		return "", fmt.Errorf("unterminated quote")
	}
	return raw, nil
}

// braceNet counts braces outside quotes on a comment-stripped line.
func braceNet(code string) int {
	var quote byte
	net := 0
	for i := 0; i < len(code); i++ {
		c := code[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"' || c == '`':
			quote = c
		case c == '\\':
			i++
		case c == '{':
			net++
		case c == '}':
			net--
		}
	}
	return net
}

// heredocTerminator reports whether a code line ends by opening a Caddyfile
// heredoc (a token starting with <<) and returns its terminator.
func heredocTerminator(code string) (string, bool) {
	fields := strings.Fields(code)
	if len(fields) == 0 {
		return "", false
	}
	last := fields[len(fields)-1]
	if !strings.HasPrefix(last, "<<") || len(last) == 2 {
		return "", false
	}
	return last[2:], true
}

// splitAddressLine splits a site-address line on commas, keeping each
// address as written.
func splitAddressLine(addr string) []string {
	var out []string
	for _, a := range strings.Split(addr, ",") {
		if a = strings.TrimSpace(a); a != "" {
			out = append(out, a)
		}
	}
	return out
}

// addressHosts strips schemes and ports-nothing-else from raw address
// tokens for host comparison (mirrors addressWithinHosts's normalization).
func addressHosts(addrs []string) []string {
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		a = strings.TrimPrefix(a, "https://")
		a = strings.TrimPrefix(a, "http://")
		if sp := strings.IndexAny(a, " \t"); sp >= 0 {
			a = a[:sp]
		}
		out = append(out, a)
	}
	return out
}

// hostsWithin reports whether every host of the address list is among the
// requested set (the whole-block adoption condition).
func hostsWithin(addrHosts, hosts []string) bool {
	seen := 0
	for _, a := range addrHosts {
		matched := false
		for _, h := range hosts {
			if a == h {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
		seen++
	}
	return seen > 0
}

// adoptForeignBlocks is F49's structured adoption: for every NON-managed
// top-level site block whose hosts overlap the requested set —
//
//   - all its hosts are requested → the whole block is removed (the
//     historical whole-block-only rule, now decided on parsed structure);
//   - some of its hosts are requested → the adopted hosts are REMOVED from
//     its address line and the block is kept for its remaining hosts
//     (previously the block was left alone and the duplicate site address
//     failed at reload — loud, but it took the whole edit down with it);
//   - the block's own directives are never touched — adoption rehomes
//     HOSTS, it does not merge policy.
//
// TEPLOY-managed regions are never adopted (their owner will rewrite them
// itself; deleting another app's managed block here would fight the next
// deploy of that app). An unparseable Caddyfile is an error: the caller
// aborts before writing.
func adoptForeignBlocks(content string, hosts []string) (string, error) {
	// Parse view: managed regions blanked (line count preserved), so their
	// site blocks neither parse into the candidate set nor match edits.
	view := strings.Split(content, "\n")
	inManaged := false
	for i, l := range view {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, markerBeginPrefix) {
			inManaged = true
		} else if strings.HasPrefix(t, markerEndPrefix) {
			inManaged = false
		}
		if inManaged && !strings.HasPrefix(t, markerBeginPrefix) {
			view[i] = ""
		}
	}
	blocks, err := ParseSites(strings.Join(view, "\n"))
	if err != nil {
		return "", fmt.Errorf("structuring the Caddyfile for foreign-block adoption: %w", err)
	}
	type edit struct {
		firstLine string // rewritten address line ("" = drop the block)
		block     SiteBlock
	}
	var edits []edit
	for _, b := range blocks {
		ah := addressHosts(b.Addresses)
		if !hostsWithin(ah, hosts) {
			// Partial overlap? Compute kept addresses.
			requested := map[string]bool{}
			for _, h := range hosts {
				requested[h] = true
			}
			kept := b.Addresses[:0:0]
			overlap := false
			for i, raw := range b.Addresses {
				if requested[ah[i]] {
					overlap = true
					continue
				}
				kept = append(kept, raw)
			}
			if !overlap {
				continue // not our business
			}
			if len(kept) == 0 {
				// Cannot happen (hostsWithin false means ≥1 kept), but the
				// invariant is load-bearing: guard it anyway.
				edits = append(edits, edit{block: b})
				continue
			}
			indent := leadingWhitespace(b.Lines[0])
			rewritten := indent + strings.Join(kept, ", ") + " {"
			edits = append(edits, edit{firstLine: rewritten, block: b})
			continue
		}
		edits = append(edits, edit{block: b})
	}
	if len(edits) == 0 {
		return content, nil
	}

	// Apply the edits by walking the original logical lines and matching
	// block address lines verbatim (first lines are unique per block by
	// construction — the same file cannot carry two identical site
	// addresses without failing at reload anyway).
	drop := map[string]bool{} // address code line → drop whole block
	rewrite := map[string]string{}
	for _, e := range edits {
		key, err := codeLine(e.block.Lines[0])
		if err != nil {
			return "", fmt.Errorf("structuring the Caddyfile: %w", err)
		}
		if e.firstLine == "" {
			drop[key] = true
		} else {
			rewrite[key] = e.firstLine
		}
	}
	var out []string
	skipping := false
	depth := 0
	for _, raw := range logicalLines(strings.Split(content, "\n")) {
		code, err := codeLine(raw)
		if err != nil {
			return "", fmt.Errorf("structuring the Caddyfile: %w", err)
		}
		net := braceNet(code)
		if skipping {
			depth += net
			if depth <= 0 {
				skipping = false
			}
			continue
		}
		if drop[code] && net > 0 {
			depth = net
			skipping = true
			continue
		}
		if repl, ok := rewrite[code]; ok {
			out = append(out, repl)
			continue
		}
		out = append(out, raw)
	}
	// Collapse any double blank lines the removals opened up.
	joined := strings.Join(out, "\n")
	for strings.Contains(joined, "\n\n\n") {
		joined = strings.ReplaceAll(joined, "\n\n\n", "\n\n")
	}
	return joined, nil
}

func leadingWhitespace(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] != ' ' && s[i] != '\t' {
			return s[:i]
		}
	}
	return s
}

// SitePolicy is the edge policy extracted from a site block (F48): the
// verbatim tls directive and access-gate directives, which maintenance mode
// must preserve so enabling it does not silently drop HTTPS or auth.
type SitePolicy struct {
	TLS    string   // verbatim "tls ..." line ("" when none)
	Access []string // verbatim basic_auth / forward_auth spans
}

// ExtractPolicy parses one site block (address line + body + closing brace,
// as extractCaddyfileBlock returns) and lifts its direct tls and access
// directives, with nested spans (basic_auth's user list, forward_auth's
// options) kept verbatim. Anything unparseable is an error — a maintenance
// block built from a misread policy is worse than one built from none.
func ExtractPolicy(block string) (SitePolicy, error) {
	var pol SitePolicy
	lines := logicalLines(strings.Split(block, "\n"))
	if len(lines) == 0 {
		return pol, nil
	}
	depth := braceNet(lines[0]) // the address line's opening brace
	if depth <= 0 {
		return pol, fmt.Errorf("not a site block: %q", lines[0])
	}
	for i := 1; i < len(lines); i++ {
		raw := lines[i]
		code, err := codeLine(raw)
		if err != nil {
			return pol, err
		}
		if term, ok := heredocTerminator(code); ok {
			for i+1 < len(lines) {
				i++
				if strings.TrimSpace(lines[i]) == term {
					break
				}
			}
			continue
		}
		trimmed := strings.TrimSpace(code)
		if depth == 1 && trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			switch {
			case trimmed == "tls" || strings.HasPrefix(trimmed, "tls "):
				if pol.TLS != "" {
					return pol, fmt.Errorf("duplicate tls directive in site block")
				}
				pol.TLS = raw
			case trimmed == "basic_auth" || strings.HasPrefix(trimmed, "basic_auth "),
				trimmed == "forward_auth" || strings.HasPrefix(trimmed, "forward_auth "):
				// The directive may open a nested span; capture it whole.
				span := []string{raw}
				net := braceNet(code)
				j := i
				for net > 0 && j+1 < len(lines) {
					j++
					span = append(span, lines[j])
					inner, err := codeLine(lines[j])
					if err != nil {
						return pol, err
					}
					net += braceNet(inner)
				}
				if net > 0 {
					return pol, fmt.Errorf("unbalanced %s span", strings.Fields(trimmed)[0])
				}
				pol.Access = append(pol.Access, strings.Join(span, "\n"))
				i = j
				continue
			}
		}
		depth += braceNet(code)
		if depth <= 0 {
			return pol, nil // closing brace of the site block
		}
	}
	return pol, fmt.Errorf("unbalanced site block — no closing brace")
}
