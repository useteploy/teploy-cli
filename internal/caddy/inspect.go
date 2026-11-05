package caddy

// Read-only view of the Caddyfile for `teploy domains check`: which sites exist,
// who owns them, how their TLS is served and what they proxy to.

import (
	"context"
	"net"
	"slices"
	"sort"
	"strings"
)

// TLS modes of a site.
const (
	SiteTLSACME     = "acme"      // Caddy obtains a public certificate
	SiteTLSInternal = "internal"  // Caddy's local CA
	SiteTLSCustom   = "custom"    // operator-supplied certificate
	SiteTLSHTTPOnly = "http_only" // served on plain HTTP
)

// RouteSite is one top-level site block as `domains check` sees it.
type RouteSite struct {
	// Owner is the app whose managed region holds the block ("" = hand-written).
	Owner string
	// Hosts are the names the block serves, lowercased, without scheme or port.
	Hosts []string
	TLS   string
	// Upstreams are the dials of the block's reverse_proxy directives
	// (host:port), excluding the managed webhook fragment.
	Upstreams []string
}

// ReadCaddyfile returns the server's Caddyfile; present=false when it has none.
func (c *Client) ReadCaddyfile(ctx context.Context) (content string, present bool, err error) {
	data, present, err := readServerFile(ctx, c.exec, caddyfilePath)
	return string(data), present, err
}

// InspectRoutes lists every site of a Caddyfile, attributing each to the managed
// region holding it. An unparseable file is an error: a route view built from
// half a file would report routes as missing.
func InspectRoutes(content string) ([]RouteSite, error) {
	lines := strings.Split(content, "\n")
	regions := map[string][]string{}
	var order []string
	manual := make([]string, 0, len(lines))
	current := ""
	for _, line := range lines {
		if current == "" {
			if name, ok := beginMarkerApp(line); ok {
				current = name
				if _, seen := regions[name]; !seen {
					order = append(order, name)
				}
				manual = append(manual, "")
				continue
			}
			manual = append(manual, line)
			continue
		}
		if name, ok := endMarkerApp(line); ok && name == current {
			current = ""
			manual = append(manual, "")
			continue
		}
		regions[current] = append(regions[current], line)
		manual = append(manual, "")
	}
	// An unterminated region is unreadable as a route; the editors refuse it too.
	if current != "" {
		return nil, &unterminatedRegionError{app: current}
	}

	var sites []RouteSite
	blocks, err := ParseSites(strings.Join(manual, "\n"))
	if err != nil {
		return nil, err
	}
	for _, b := range blocks {
		sites = append(sites, inspectSite("", b))
	}
	for _, app := range order {
		blocks, err := ParseSites(strings.Join(regions[app], "\n"))
		if err != nil {
			return nil, err
		}
		for _, b := range blocks {
			sites = append(sites, inspectSite(app, b))
		}
	}
	return sites, nil
}

type unterminatedRegionError struct{ app string }

func (e *unterminatedRegionError) Error() string {
	return "the managed block for " + e.app + " has no end marker"
}

// ManagedRegion is one app's marker-delimited region of a Caddyfile.
type ManagedRegion struct {
	App string
	// Text is the region including its markers, as ManagedRegionHash expects.
	Text string
	// Sites is empty for a region that holds no site block.
	Sites []RouteSite
}

// ManagedRegions lists a Caddyfile's managed regions in file order. An
// unparseable file is an error, as for InspectRoutes.
func ManagedRegions(content string) ([]ManagedRegion, error) {
	sites, err := InspectRoutes(content)
	if err != nil {
		return nil, err
	}
	var regions []ManagedRegion
	seen := map[string]bool{}
	for _, line := range strings.Split(content, "\n") {
		app, ok := beginMarkerApp(line)
		if !ok || seen[app] {
			continue
		}
		seen[app] = true
		region := ManagedRegion{App: app, Text: extractManagedRegion(content, app)}
		for _, site := range sites {
			if site.Owner == app {
				region.Sites = append(region.Sites, site)
			}
		}
		regions = append(regions, region)
	}
	return regions, nil
}

// RegionHasWebhook reports whether a region carries the autodeploy webhook route.
func RegionHasWebhook(region string) bool {
	return strings.Contains(region, "# TEPLOY WEBHOOK BEGIN")
}

// RegionRoots lists the paths a region's root directives serve files from.
func RegionRoots(region string) []string {
	var roots []string
	for _, line := range strings.Split(region, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || f[0] != "root" {
			continue
		}
		path := f[len(f)-1]
		if !slices.Contains(roots, path) {
			roots = append(roots, path)
		}
	}
	return roots
}

// RegionHosts lists the hosts a managed region serves.
func RegionHosts(region string) []string {
	body := make([]string, 0)
	for _, line := range strings.Split(region, "\n") {
		if _, ok := beginMarkerApp(line); ok {
			continue
		}
		if _, ok := endMarkerApp(line); ok {
			continue
		}
		body = append(body, line)
	}
	blocks, err := ParseSites(strings.Join(body, "\n"))
	if err != nil {
		return nil
	}
	var hosts []string
	for _, b := range blocks {
		hosts = append(hosts, inspectSite("", b).Hosts...)
	}
	return hosts
}

func inspectSite(owner string, b SiteBlock) RouteSite {
	site := RouteSite{Owner: owner, TLS: SiteTLSACME}
	allPlainHTTP := len(b.Addresses) > 0
	for _, addr := range b.Addresses {
		host, plainHTTP := addressHost(addr)
		if !plainHTTP {
			allPlainHTTP = false
		}
		if host != "" {
			site.Hosts = append(site.Hosts, host)
		}
	}
	if allPlainHTTP {
		site.TLS = SiteTLSHTTPOnly
	}
	inWebhook := false
	depth := 0
	inProxy := false
	proxyDepth := 0
	for i, raw := range b.Lines {
		if i == 0 {
			continue
		}
		trimmed := strings.TrimSpace(raw)
		if strings.HasPrefix(trimmed, "# TEPLOY WEBHOOK BEGIN") {
			inWebhook = true
			continue
		}
		if strings.HasPrefix(trimmed, "# TEPLOY WEBHOOK END") {
			inWebhook = false
			continue
		}
		code, err := codeLine(raw)
		if err != nil {
			continue
		}
		fields := strings.Fields(code)
		delta := braceNet(code)
		if len(fields) > 0 && !inWebhook {
			switch {
			case fields[0] == "reverse_proxy":
				site.Upstreams = append(site.Upstreams, dialTokens(fields[1:])...)
				if delta > 0 {
					inProxy, proxyDepth = true, depth+delta
				}
			case fields[0] == "to" && inProxy && depth == proxyDepth:
				site.Upstreams = append(site.Upstreams, dialTokens(fields[1:])...)
			case fields[0] == "tls" && depth == 0 && site.TLS == SiteTLSACME && len(fields) > 1:
				if fields[1] == "internal" {
					site.TLS = SiteTLSInternal
				} else if fields[1] != "{" && !strings.Contains(fields[1], "@") {
					site.TLS = SiteTLSCustom
				}
			}
		}
		depth += delta
		if inProxy && depth < proxyDepth {
			inProxy = false
		}
	}
	sort.Strings(site.Upstreams)
	site.Upstreams = slices.Compact(site.Upstreams)
	return site
}

// addressHost reduces a site address to its host and reports whether it is
// plain HTTP (scheme http:// or port 80). An address with no host (":8080")
// yields "".
func addressHost(addr string) (host string, plainHTTP bool) {
	a := strings.TrimSpace(addr)
	if i := strings.IndexAny(a, " \t"); i >= 0 {
		a = a[:i]
	}
	if rest, ok := strings.CutPrefix(a, "http://"); ok {
		a, plainHTTP = rest, true
	} else {
		a = strings.TrimPrefix(a, "https://")
	}
	if i := strings.IndexByte(a, '/'); i >= 0 {
		a = a[:i]
	}
	if h, p, err := net.SplitHostPort(a); err == nil {
		a = h
		if p == "80" {
			plainHTTP = true
		}
	}
	return strings.ToLower(strings.Trim(a, "[]")), plainHTTP
}

// dialTokens extracts upstream dials from directive arguments: matchers,
// paths and option braces are not dials.
func dialTokens(args []string) []string {
	var out []string
	for _, tok := range args {
		switch {
		case tok == "{", strings.HasPrefix(tok, "@"), strings.HasPrefix(tok, "/"), tok == "*":
			continue
		}
		if i := strings.Index(tok, "://"); i >= 0 {
			tok = tok[i+3:]
		}
		if tok != "" {
			out = append(out, tok)
		}
	}
	return out
}
