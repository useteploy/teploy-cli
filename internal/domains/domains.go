// Package domains implements `teploy domains check`: for each (server, app,
// host) it reports what public DNS says, whether Caddy can get a certificate,
// and what routes the name, so Dash and a person at a terminal read the same
// evidence.
//
// Everything here is read-only and needs nothing on the server beyond what a
// deploy already needs (docker, a shell, read access to /deployments). The rule
// throughout: what could not be established reads "unknown", never healthy.
package domains

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/useteploy/teploy/internal/caddy"
	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/dns"
	"github.com/useteploy/teploy/internal/ssh"
	"github.com/useteploy/teploy/internal/state"
)

// Route kinds.
const (
	RouteManaged       = "managed"        // the owning app's managed block
	RouteManual        = "manual"         // a hand-written block
	RouteStaleManaged  = "stale_managed"  // managed block of an app that is not caddy-routed
	RouteOrphanManaged = "orphan_managed" // managed block of an app with no state on this server
	RouteNone          = "none"
	RouteUnknown       = "unknown"
)

// Skip reasons: why a row is not (wholly) a health signal.
const (
	SkipExternalLabel = "external_label" // ingress external, no route: the domain is a label
	SkipHostLabel     = "host_label"     // ingress host, nothing fronts it
	SkipIPLiteral     = "ip_literal"
	SkipWildcard      = "wildcard"
	SkipPrivateName   = "private_name" // only resolves inside a network
	SkipReservedName  = "reserved_name"
	SkipInvalidName   = "invalid_name"
)

// TLS outcomes.
const (
	TLSOK            = "ok"
	TLSFailing       = "failing"
	TLSUnknown       = "unknown"
	TLSNotApplicable = "not_applicable"
)

// Upstream resolution outcomes, per upstream and aggregated per route.
const (
	ResolvesYes           = "yes"
	ResolvesNo            = "no"
	ResolvesPartial       = "partial" // aggregate only: some upstreams do not resolve
	ResolvesUnknown       = "unknown"
	ResolvesNotApplicable = "not_applicable"
)

// Log coverage.
const (
	LogsOK          = "ok"
	LogsShort       = "short"       // under a day: not enough to call zero failures
	LogsPartial     = "partial"     // a matched line could not be parsed: same
	LogsUnavailable = "unavailable" // could not be read
	LogsNoCaddy     = "no_caddy"    // no caddy container here
)

// Server states.
const (
	ServerOK          = "ok"
	ServerPartial     = "partial"     // reached, but part of the evidence is missing
	ServerUnreachable = "unreachable" // nothing was established
)

// Envelope is the `domains check --json` document (contracts/schema/
// domains-check-envelope.schema.json).
type Envelope struct {
	MachineInterface     int       `json:"machine_interface"`
	CheckedAt            time.Time `json:"checked_at"`
	LogWindowSeconds     int64     `json:"log_window_seconds"`
	Resolvers            []string  `json:"resolvers"`
	CloudflareRangesAsOf string    `json:"cloudflare_ranges_as_of"`
	Servers              []Server  `json:"servers"`
	Domains              []Domain  `json:"domains"`
}

// Issue is one piece of evidence that could not be collected.
type Issue struct {
	Scope   string `json:"scope"`
	Message string `json:"message"`
}

// CaddyStatus is what was learned about the server's Caddy.
type CaddyStatus struct {
	// Container is present | absent | stopped | unknown.
	Container string `json:"container"`
	// Caddyfile is present | absent | unparseable | unreadable.
	Caddyfile string `json:"caddyfile"`
	Logs      string `json:"logs"`
	// LogCoverageSeconds is how far back the logs reach, capped at the window.
	LogCoverageSeconds int64 `json:"log_coverage_seconds"`
	// LogsTruncated is true when more failure lines existed than were read, so
	// counts are lower bounds.
	LogsTruncated bool `json:"logs_truncated,omitempty"`
}

// Server is one server's outcome. Rows exist only for what was established.
type Server struct {
	Server    string    `json:"server"`
	Host      string    `json:"host"`
	State     string    `json:"state"`
	CheckedAt time.Time `json:"checked_at"`
	Error     string    `json:"error,omitempty"`
	// Addresses are every address counted as this server. PublicAddressKnown is
	// false when none of them is public; the per-family flags decide DNS: an
	// answer in a family with no known public address proves nothing and reads
	// unknown.
	Addresses          []string    `json:"addresses"`
	PublicAddressKnown bool        `json:"public_address_known"`
	PublicIPv4Known    bool        `json:"public_ipv4_known"`
	PublicIPv6Known    bool        `json:"public_ipv6_known"`
	Caddy              CaddyStatus `json:"caddy"`
	Issues             []Issue     `json:"issues"`
}

// DNSResult is the public-resolver view of a host.
type DNSResult struct {
	// Status is ok | nxdomain | wrong_target | proxied | unknown | skipped.
	Status  string   `json:"status"`
	Answers []string `json:"answers"`
	Detail  string   `json:"detail,omitempty"`
}

// TLSResult is Caddy's certificate-obtain history for a host. Failures is null
// when it is unknown; it is never 0 on short or unavailable logs.
type TLSResult struct {
	Status        string     `json:"status"`
	Failures      *int       `json:"failures"`
	FailingSince  *time.Time `json:"failing_since,omitempty"`
	LastFailureAt *time.Time `json:"last_failure_at,omitempty"`
	LastSuccessAt *time.Time `json:"last_success_at,omitempty"`
	LastError     string     `json:"last_error,omitempty"`
}

// Upstream is one proxied dial and whether it resolves in Caddy's network.
type Upstream struct {
	Dial     string `json:"dial"`
	Resolves string `json:"resolves"`
}

// RouteResult is what serves a host in this server's Caddy.
type RouteResult struct {
	Kind string `json:"kind"`
	// Owner is the app whose managed block holds the route ("" when manual).
	Owner string `json:"owner,omitempty"`
	// TLS is acme | internal | custom | http_only; "" with no route.
	TLS       string     `json:"tls,omitempty"`
	Upstreams []Upstream `json:"upstreams"`
	// UpstreamResolves aggregates Upstreams: yes | no | partial | unknown |
	// not_applicable (no proxy upstream to check).
	UpstreamResolves string `json:"upstream_resolves"`
}

// Domain is one (server, app, host) row. App is "" for a hand-written block no
// deployed app claims.
type Domain struct {
	Server  string      `json:"server"`
	App     string      `json:"app"`
	Host    string      `json:"host"`
	Ingress string      `json:"ingress"`
	Skip    string      `json:"skip,omitempty"`
	DNS     DNSResult   `json:"dns"`
	TLS     TLSResult   `json:"tls"`
	Route   RouteResult `json:"route"`
}

// Options bounds one check.
type Options struct {
	// LogWindow is how far back Caddy's logs are read.
	LogWindow time.Duration
	// App limits rows to one app ("" = all).
	App string
	// DNSBudget bounds the DNS phase of one server; what has not been answered
	// by then reads unknown.
	DNSBudget time.Duration
	// Now is the clock (tests).
	Now func() time.Time
}

const (
	// DefaultLogWindow reaches the "failing for 14 days" evidence.
	DefaultLogWindow = 14 * 24 * time.Hour
	// minLogCoverage is how much log history makes "zero failures" believable:
	// Caddy retries a failing obtain at least every 6 hours.
	minLogCoverage = 24 * time.Hour
	// maxLogLines caps what one server sends back.
	maxLogLines = 2000
	// dnsPerResolver bounds one resolver for one name.
	dnsPerResolver = 4 * time.Second
	// dnsConcurrency bounds lookups across the whole sweep.
	dnsConcurrency = 16
	// maxErrorChars bounds a stored Caddy error.
	maxErrorChars = 300
)

// DNSCache shares lookups across servers and bounds their concurrency, so one
// name on two servers is asked once and a slow resolver cannot pile up.
type DNSCache struct {
	lookup func(ctx context.Context, host string) dns.PublicAnswer
	sem    chan struct{}
	mu     sync.Mutex
	done   map[string]*dnsEntry
}

type dnsEntry struct {
	once sync.Once
	ans  dns.PublicAnswer
}

// NewDNSCache returns a cache over the fixed public resolvers.
func NewDNSCache() *DNSCache {
	return NewDNSCacheWith(func(ctx context.Context, host string) dns.PublicAnswer {
		return dns.LookupPublic(ctx, host, dnsPerResolver)
	})
}

// NewDNSCacheWith returns a cache over a given lookup (tests).
func NewDNSCacheWith(lookup func(ctx context.Context, host string) dns.PublicAnswer) *DNSCache {
	return &DNSCache{lookup: lookup, sem: make(chan struct{}, dnsConcurrency), done: map[string]*dnsEntry{}}
}

func (c *DNSCache) get(ctx context.Context, host string) dns.PublicAnswer {
	c.mu.Lock()
	e := c.done[host]
	if e == nil {
		e = &dnsEntry{}
		c.done[host] = e
	}
	c.mu.Unlock()
	e.once.Do(func() {
		select {
		case c.sem <- struct{}{}:
			defer func() { <-c.sem }()
			e.ans = c.lookup(ctx, host)
		case <-ctx.Done():
			e.ans = dns.PublicAnswer{Err: ctx.Err()}
		}
	})
	return e.ans
}

// AppInfo is the part of one app's state a check needs.
type AppInfo struct {
	Name    string
	Domain  string
	Ingress string
}

// CheckServer runs every read against one connected server. It never fails as a
// whole: what it could not read lands in Server.Issues and reads unknown.
func CheckServer(ctx context.Context, exec ssh.Executor, name string, opts Options, cache *DNSCache) (Server, []Domain) {
	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}
	if opts.LogWindow <= 0 {
		opts.LogWindow = DefaultLogWindow
	}
	if opts.DNSBudget <= 0 {
		opts.DNSBudget = 30 * time.Second
	}
	srv := Server{Server: name, Host: exec.Host(), State: ServerOK, CheckedAt: now().UTC(), Addresses: []string{}, Issues: []Issue{}}
	issue := func(scope string, err error) {
		srv.Issues = append(srv.Issues, Issue{Scope: scope, Message: cleanText(err.Error(), maxErrorChars)})
	}

	apps, stateKnown := readApps(ctx, exec, &srv, issue)

	sites, routesKnown := readSites(ctx, exec, &srv, issue)
	srv.Addresses, srv.PublicIPv4Known, srv.PublicIPv6Known = readAddresses(ctx, exec)
	srv.PublicAddressKnown = srv.PublicIPv4Known || srv.PublicIPv6Known

	obtain := readObtainLog(ctx, exec, &srv, opts.LogWindow, now(), issue)

	rows := buildRows(name, apps, sites, routesKnown, stateKnown, opts.App)

	resolveUpstreams(ctx, exec, rows, &srv, issue)
	applyTLS(rows, obtain, srv.Caddy)

	dnsCtx, cancel := context.WithTimeout(ctx, opts.DNSBudget)
	defer cancel()
	applyDNS(dnsCtx, rows, srv, cache)

	if len(srv.Issues) > 0 {
		srv.State = ServerPartial
	}
	return srv, rows
}

// readApps lists the deployed apps with their ingress and domain. known=false
// means the app list itself could not be read, so ownership cannot be judged.
func readApps(ctx context.Context, exec ssh.Executor, srv *Server, issue func(string, error)) (apps []AppInfo, known bool) {
	out, err := exec.Run(ctx, `for f in /deployments/*/state.json /deployments/*/state; do [ -f "$f" ] && basename "$(dirname "$f")"; done | sort -u`)
	if err != nil {
		issue("apps", err)
		return nil, false
	}
	var names []string
	for _, n := range strings.Fields(out) {
		if config.ValidateName(n) == nil {
			names = append(names, n)
		}
	}
	snaps, err := state.ReadAppSnapshots(ctx, exec, names)
	if err != nil {
		issue("apps", err)
		return nil, false
	}
	known = true
	for _, snap := range snaps {
		if snap.StateErr != nil {
			issue("app:"+snap.App, snap.StateErr)
			known = false
			continue
		}
		if snap.State == nil || snap.State.Tombstone {
			continue
		}
		ingress := snap.State.IngressMode
		if ingress == "" {
			ingress = config.IngressCaddy
		}
		apps = append(apps, AppInfo{Name: snap.App, Domain: snap.State.Domain, Ingress: ingress})
	}
	return apps, known
}

func readSites(ctx context.Context, exec ssh.Executor, srv *Server, issue func(string, error)) (sites []caddy.RouteSite, known bool) {
	content, present, err := caddy.NewClient(exec).ReadCaddyfile(ctx)
	switch {
	case err != nil:
		srv.Caddy.Caddyfile = "unreadable"
		issue("caddyfile", err)
		return nil, false
	case !present:
		srv.Caddy.Caddyfile = "absent"
		return nil, true
	}
	sites, err = caddy.InspectRoutes(content)
	if err != nil {
		srv.Caddy.Caddyfile = "unparseable"
		issue("caddyfile", err)
		return nil, false
	}
	srv.Caddy.Caddyfile = "present"
	return sites, true
}

// readAddresses collects what counts as this server: every interface address
// plus the address it reaches the internet from (a NAT'd box's public IP is on
// no interface). The probe goes to the same operator as the first resolver, and
// is best effort: without it, and with no public interface, a non-matching DNS
// answer reads unknown rather than wrong_target.
func readAddresses(ctx context.Context, exec ssh.Executor) (addrs []string, publicV4, publicV6 bool) {
	seen := map[string]bool{}
	add := func(raw string) {
		ip := net.ParseIP(strings.TrimSpace(raw))
		if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
			return
		}
		s := ip.String()
		if !seen[s] {
			seen[s] = true
			addrs = append(addrs, s)
		}
		if isPublicIP(ip) {
			if ip.To4() != nil {
				publicV4 = true
			} else {
				publicV6 = true
			}
		}
	}
	add(exec.Host())
	if out, err := exec.Run(ctx, "hostname -I 2>/dev/null || true"); err == nil {
		for _, f := range strings.Fields(out) {
			add(f)
		}
	}
	probe := "curl -4 -fsS --max-time 4 https://1.1.1.1/cdn-cgi/trace 2>/dev/null | grep '^ip=' || true"
	if out, err := exec.Run(ctx, probe); err == nil {
		for _, line := range strings.Split(out, "\n") {
			if v, ok := strings.CutPrefix(strings.TrimSpace(line), "ip="); ok {
				add(v)
			}
		}
	}
	sort.Strings(addrs)
	return addrs, publicV4, publicV6
}

var cgnat = func() *net.IPNet { _, n, _ := net.ParseCIDR("100.64.0.0/10"); return n }()

func isPublicIP(ip net.IP) bool {
	return ip.IsGlobalUnicast() && !ip.IsPrivate() && !cgnat.Contains(ip)
}

// logCommand reads Caddy's certificate failures and successes, bounded in time
// and size, and frames what it found. Every value in it is a constant or a
// validated duration.
func logCommand(window time.Duration) string {
	since := strconv.FormatInt(int64(window/time.Second), 10) + "s"
	return `o=$(docker container inspect -f '{{.State.Running}}' caddy 2>&1) || { case "$o" in *"No such"*) echo TEPLOY_CADDY_ABSENT;; *) echo TEPLOY_CADDY_UNKNOWN;; esac; exit 0; }; ` +
		`echo "TEPLOY_CADDY_RUNNING=$o"; ` +
		`t=; command -v timeout >/dev/null 2>&1 && t='timeout 30'; ` +
		`printf 'TEPLOY_LOG_FIRST %s\n' "$(docker logs caddy 2>&1 | head -n 1 | cut -c1-300)"; ` +
		`{ $t docker logs --since ` + since + ` caddy 2>&1; echo "TEPLOY_LOG_RC=$?"; } | ` +
		`grep -E '"logger":"tls\.(obtain|renew)"|^TEPLOY_LOG_RC=' | ` +
		`grep -E '"level":"error"|obtained successfully|renewed successfully|^TEPLOY_LOG_RC=' | ` +
		`tail -n ` + strconv.Itoa(maxLogLines) + ` | cut -c1-4000`
}

func readObtainLog(ctx context.Context, exec ssh.Executor, srv *Server, window time.Duration, now time.Time, issue func(string, error)) map[string]*ObtainStats {
	out, err := exec.Run(ctx, logCommand(window))
	if err != nil {
		srv.Caddy.Container = "unknown"
		srv.Caddy.Logs = LogsUnavailable
		issue("caddy_logs", err)
		return nil
	}
	parsed := parseObtainOutput(out, now, window)
	srv.Caddy.Container = parsed.container
	srv.Caddy.Logs = parsed.logs
	srv.Caddy.LogCoverageSeconds = parsed.coverageSeconds
	srv.Caddy.LogsTruncated = parsed.truncated
	if parsed.problem != "" {
		issue("caddy_logs", fmt.Errorf("%s", parsed.problem))
	}
	return parsed.stats
}

// ObtainStats is one identifier's certificate history in the log window.
type ObtainStats struct {
	Failures      int
	Failing       bool
	FailingSince  time.Time
	LastFailureAt time.Time
	LastSuccessAt time.Time
	LastError     string
}

type obtainOutput struct {
	container       string
	logs            string
	coverageSeconds int64
	truncated       bool
	problem         string
	stats           map[string]*ObtainStats
}

var bracketIdentifier = regexp.MustCompile(`^\[([^\]\s]+)\]`)

// parseObtainOutput turns logCommand's framed output into per-identifier stats.
// Coverage is the age of the oldest retained line, so a quiet Caddy with old
// logs is not mistaken for a freshly recreated one.
func parseObtainOutput(out string, now time.Time, window time.Duration) obtainOutput {
	res := obtainOutput{container: "unknown", logs: LogsUnavailable, stats: map[string]*ObtainStats{}}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	first := strings.TrimSpace(lines[0])
	switch {
	case first == "TEPLOY_CADDY_ABSENT":
		res.container, res.logs = "absent", LogsNoCaddy
		return res
	case first == "TEPLOY_CADDY_UNKNOWN":
		res.problem = "could not inspect the caddy container"
		return res
	case strings.HasPrefix(first, "TEPLOY_CADDY_RUNNING="):
		if strings.HasSuffix(first, "=true") {
			res.container = "present"
		} else {
			res.container = "stopped"
		}
	default:
		res.problem = "unrecognized log output"
		return res
	}

	rc := -1
	logLines := 0
	unparsed := 0
	haveFirst := false
	for _, line := range lines[1:] {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "TEPLOY_LOG_FIRST"):
			raw := strings.TrimSpace(strings.TrimPrefix(line, "TEPLOY_LOG_FIRST"))
			if raw == "" {
				continue // an empty log: nothing retained yet
			}
			var head struct {
				TS float64 `json:"ts"`
			}
			if json.Unmarshal([]byte(raw), &head) == nil && head.TS > 0 {
				haveFirst = true
				age := now.Sub(unixFloat(head.TS))
				if age > window {
					age = window
				}
				if age < 0 {
					age = 0
				}
				res.coverageSeconds = int64(age / time.Second)
			} else if !strings.HasPrefix(raw, "{") {
				// A configured console log format: nothing below would match,
				// and zero matches must not read as zero failures.
				res.problem = "caddy's log format is not JSON, so certificate failures cannot be counted"
				return res
			}
		case strings.HasPrefix(line, "TEPLOY_LOG_RC="):
			rc, _ = strconv.Atoi(strings.TrimPrefix(line, "TEPLOY_LOG_RC="))
		case line != "":
			logLines++
			if !recordObtainLine(res.stats, line) {
				unparsed++
			}
		}
	}
	if rc != 0 {
		res.problem = fmt.Sprintf("reading caddy's logs did not finish (exit %d)", rc)
		return res
	}
	res.truncated = logLines >= maxLogLines
	if haveFirst && time.Duration(res.coverageSeconds)*time.Second >= minDuration(minLogCoverage, window) {
		res.logs = LogsOK
	} else {
		res.logs = LogsShort
	}
	// A failure line that cannot be attributed could be anyone's: zero failures
	// is not believable for any name.
	if unparsed > 0 {
		if res.logs == LogsOK {
			res.logs = LogsPartial
		}
		res.problem = fmt.Sprintf("%d certificate log line(s) could not be read", unparsed)
	}
	return res
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

func unixFloat(ts float64) time.Time {
	sec := int64(ts)
	return time.Unix(sec, int64((ts-float64(sec))*1e9)).UTC()
}

// recordObtainLine reports false for a matched line it could not attribute.
func recordObtainLine(stats map[string]*ObtainStats, line string) bool {
	var rec struct {
		Level      string  `json:"level"`
		TS         float64 `json:"ts"`
		Msg        string  `json:"msg"`
		Identifier string  `json:"identifier"`
		Error      string  `json:"error"`
		Elapsed    float64 `json:"elapsed"`
	}
	if json.Unmarshal([]byte(line), &rec) != nil || rec.TS == 0 {
		return false
	}
	id := rec.Identifier
	if id == "" {
		if m := bracketIdentifier.FindStringSubmatch(rec.Error); m != nil {
			id = m[1]
		}
	}
	id = strings.ToLower(strings.TrimSpace(id))
	if id == "" {
		return false
	}
	s := stats[id]
	if s == nil {
		s = &ObtainStats{}
		stats[id] = s
	}
	at := unixFloat(rec.TS)
	if rec.Level != "error" {
		if strings.Contains(rec.Msg, "successfully") {
			s.Failing = false
			s.FailingSince = time.Time{}
			s.LastSuccessAt = at
		}
		return true
	}
	// Each attempt logs one line per issuer plus one "will retry"; the retry
	// line is the attempt count and carries how long the obtain has been failing.
	if rec.Msg == "will retry" {
		s.Failures++
	}
	since := at
	if rec.Elapsed > 0 {
		since = at.Add(-time.Duration(rec.Elapsed * float64(time.Second)))
	}
	if !s.Failing || since.Before(s.FailingSince) {
		s.FailingSince = since
	}
	s.Failing = true
	s.LastFailureAt = at
	if rec.Error != "" {
		s.LastError = cleanText(rec.Error, maxErrorChars)
	}
	return true
}

// statsFor finds a host's certificate history: its own, or the wildcard
// certificate that covers it.
func statsFor(stats map[string]*ObtainStats, host string) *ObtainStats {
	if s := stats[host]; s != nil {
		return s
	}
	if i := strings.IndexByte(host, '.'); i >= 0 {
		return stats["*"+host[i:]]
	}
	return nil
}

// credentialURL strips userinfo from anything URL-shaped in a stored error.
var credentialURL = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)[^/\s@"]+@`)

// cleanText makes untrusted log text safe to store and print: no control
// characters, no URL credentials, bounded length.
func cleanText(s string, max int) string {
	s = credentialURL.ReplaceAllString(s, "$1")
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > max {
		s = string(r[:max]) + "..."
	}
	return s
}

var privateSuffixes = []string{".internal", ".lan", ".home.arpa", ".ts.net", ".intranet", ".corp", ".local"}

// classifyName decides whether a host can have public DNS at all.
func classifyName(host string) string {
	if len(host) > 253 {
		return SkipInvalidName
	}
	if net.ParseIP(host) != nil {
		return SkipIPLiteral
	}
	if strings.HasPrefix(host, "*.") {
		return SkipWildcard
	}
	if config.ValidateDomain(host, false) != nil {
		return SkipInvalidName
	}
	if config.IsReservedDomain(host) {
		return SkipReservedName
	}
	if !strings.Contains(host, ".") {
		return SkipPrivateName
	}
	for _, suffix := range privateSuffixes {
		if strings.HasSuffix(host, suffix) {
			return SkipPrivateName
		}
	}
	return ""
}

// buildRows joins apps and sites into rows: every host an app's state names,
// plus every host a site serves that no app names.
func buildRows(server string, apps []AppInfo, sites []caddy.RouteSite, routesKnown, stateKnown bool, onlyApp string) []Domain {
	ingressOf := make(map[string]string, len(apps))
	for _, a := range apps {
		ingressOf[a.Name] = a.Ingress
	}
	siteByHost := map[string]caddy.RouteSite{}
	var wildcards []caddy.RouteSite
	for _, site := range sites {
		for _, h := range site.Hosts {
			if strings.HasPrefix(h, "*.") {
				wildcards = append(wildcards, site)
				continue
			}
			if _, dup := siteByHost[h]; !dup {
				siteByHost[h] = site
			}
		}
	}
	lookup := func(host string) (caddy.RouteSite, bool) {
		if s, ok := siteByHost[host]; ok {
			return s, true
		}
		if i := strings.IndexByte(host, '.'); i >= 0 {
			pattern := "*" + host[i:]
			for _, s := range wildcards {
				for _, h := range s.Hosts {
					if h == pattern {
						return s, true
					}
				}
			}
		}
		return caddy.RouteSite{}, false
	}

	claimed := map[string]bool{}
	hostSeen := map[string]bool{}
	var rows []Domain
	add := func(app, ingress, host string) {
		key := app + "\x00" + host
		if claimed[key] {
			return
		}
		claimed[key] = true
		hostSeen[host] = true
		row := Domain{Server: server, App: app, Host: host, Ingress: ingress,
			DNS: DNSResult{Status: dns.StatusSkipped, Answers: []string{}}, TLS: TLSResult{Status: TLSNotApplicable}}
		switch site, ok := lookup(host); {
		case !routesKnown:
			row.Route = RouteResult{Kind: RouteUnknown, Upstreams: []Upstream{}, UpstreamResolves: ResolvesUnknown}
		case !ok:
			row.Route = RouteResult{Kind: RouteNone, Upstreams: []Upstream{}, UpstreamResolves: ResolvesNotApplicable}
		default:
			row.Route = routeFor(site, ingressOf, stateKnown)
		}
		row.Skip = skipFor(row, host)
		rows = append(rows, row)
	}

	for _, a := range apps {
		for _, h := range strings.Split(a.Domain, ",") {
			if h = normalizeHost(h); h != "" {
				add(a.Name, a.Ingress, h)
			}
		}
	}
	// Hosts only the Caddyfile knows: a managed block keeps its owner as the
	// row's app; a hand-written block has none.
	for _, site := range sites {
		for _, h := range site.Hosts {
			h = normalizeHost(h)
			if h == "" || hostSeen[h] {
				continue
			}
			add(site.Owner, ingressOf[site.Owner], h)
		}
	}
	if onlyApp != "" {
		kept := rows[:0]
		for _, r := range rows {
			if r.App == onlyApp || r.Route.Owner == onlyApp {
				kept = append(kept, r)
			}
		}
		rows = kept
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Host != rows[j].Host {
			return rows[i].Host < rows[j].Host
		}
		return rows[i].App < rows[j].App
	})
	return rows
}

// normalizeHost lowercases a name and refuses anything that cannot be one, with
// the raw text kept out of rows that a terminal or page will print.
func normalizeHost(raw string) string {
	h := strings.ToLower(strings.TrimSpace(raw))
	h = strings.TrimSuffix(h, ".")
	return cleanText(h, 300)
}

func routeFor(site caddy.RouteSite, ingressOf map[string]string, stateKnown bool) RouteResult {
	r := RouteResult{Owner: site.Owner, TLS: site.TLS, Upstreams: []Upstream{}, UpstreamResolves: ResolvesNotApplicable}
	for _, dial := range site.Upstreams {
		r.Upstreams = append(r.Upstreams, Upstream{Dial: cleanText(dial, 300), Resolves: ResolvesUnknown})
	}
	if len(r.Upstreams) > 0 {
		r.UpstreamResolves = ResolvesUnknown
	}
	switch {
	case site.Owner == "":
		r.Kind = RouteManual
	default:
		ingress, deployed := ingressOf[site.Owner]
		switch {
		case !deployed && stateKnown:
			r.Kind = RouteOrphanManaged
		case !deployed:
			r.Kind = RouteUnknown
		case ingress != config.IngressCaddy && ingress != "":
			r.Kind = RouteStaleManaged
		default:
			r.Kind = RouteManaged
		}
	}
	return r
}

// skipFor checks the label case first: an unrouted host or external domain is
// a label whatever its name looks like, including a reserved placeholder.
func skipFor(row Domain, host string) string {
	if row.Route.Kind == RouteNone {
		switch row.Ingress {
		case config.IngressExternal:
			return SkipExternalLabel
		case config.IngressHost:
			return SkipHostLabel
		}
	}
	return classifyName(host)
}

var upstreamHostRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// upstreamHost returns the name to resolve for a dial, or why not: an IP literal
// has nothing to resolve; a placeholder or odd token cannot be checked. Names
// are validated before they reach a shell, and quoted there.
func upstreamHost(dial string) (name, verdict string) {
	host := dial
	if h, _, err := net.SplitHostPort(dial); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if net.ParseIP(host) != nil {
		return "", ResolvesNotApplicable
	}
	if !upstreamHostRE.MatchString(host) {
		return "", ResolvesUnknown
	}
	return host, ""
}

// resolveUpstreams asks Caddy's own network, in one docker exec, whether each
// upstream name resolves: a name teploy no longer has a container for fails
// here even though the route still reads as configured.
func resolveUpstreams(ctx context.Context, exec ssh.Executor, rows []Domain, srv *Server, issue func(string, error)) {
	wanted := map[string]bool{}
	for i := range rows {
		for _, u := range rows[i].Route.Upstreams {
			if name, _ := upstreamHost(u.Dial); name != "" {
				wanted[name] = true
			}
		}
	}
	answers := map[string]string{}
	if len(wanted) > 0 && srv.Caddy.Container == "present" {
		names := make([]string, 0, len(wanted))
		for n := range wanted {
			names = append(names, ssh.ShellQuote(n))
		}
		sort.Strings(names)
		cmd := "printf '%s\\n' " + strings.Join(names, " ") + ` | docker exec -i caddy sh -c 'while read -r h; do getent hosts "$h" >/dev/null 2>&1; echo "$? $h"; done'`
		out, err := exec.Run(ctx, cmd)
		if err != nil {
			issue("upstreams", err)
		}
		for _, line := range strings.Split(out, "\n") {
			rc, name, ok := strings.Cut(strings.TrimSpace(line), " ")
			if !ok || !wanted[name] {
				continue
			}
			switch rc {
			case "0":
				answers[name] = ResolvesYes
			case "2":
				answers[name] = ResolvesNo
			}
		}
	}
	for i := range rows {
		r := &rows[i].Route
		yes, no, unknown := 0, 0, 0
		for j := range r.Upstreams {
			name, verdict := upstreamHost(r.Upstreams[j].Dial)
			if name != "" {
				if verdict = answers[name]; verdict == "" {
					verdict = ResolvesUnknown
				}
			}
			r.Upstreams[j].Resolves = verdict
			switch verdict {
			case ResolvesYes, ResolvesNotApplicable:
				yes++
			case ResolvesNo:
				no++
			default:
				unknown++
			}
		}
		// Preview routes have no app state of their own; a preview whose
		// containers still resolve is live, and only a dead one is left behind.
		if r.Kind == RouteOrphanManaged && strings.Contains(r.Owner, "-preview-") && len(r.Upstreams) > 0 && yes == len(r.Upstreams) {
			r.Kind = RouteManaged
		}
		switch {
		case len(r.Upstreams) == 0:
		case no > 0 && yes == 0 && unknown == 0:
			r.UpstreamResolves = ResolvesNo
		case no > 0:
			r.UpstreamResolves = ResolvesPartial
		case unknown > 0:
			r.UpstreamResolves = ResolvesUnknown
		default:
			r.UpstreamResolves = ResolvesYes
		}
	}
}

// applyTLS fills certificate evidence on rows Caddy would obtain for. Unknown
// when the logs cannot back a claim; zero failures only on logs long enough.
func applyTLS(rows []Domain, stats map[string]*ObtainStats, status CaddyStatus) {
	for i := range rows {
		row := &rows[i]
		if row.Route.TLS != caddy.SiteTLSACME || row.Skip == SkipIPLiteral || row.Skip == SkipPrivateName {
			continue
		}
		s := statsFor(stats, row.Host)
		if s == nil {
			if status.Logs == LogsOK {
				zero := 0
				row.TLS = TLSResult{Status: TLSOK, Failures: &zero}
			} else {
				row.TLS = TLSResult{Status: TLSUnknown}
			}
			continue
		}
		failures := s.Failures
		row.TLS = TLSResult{Failures: &failures, LastError: s.LastError}
		if !s.LastFailureAt.IsZero() {
			at := s.LastFailureAt
			row.TLS.LastFailureAt = &at
		}
		if !s.LastSuccessAt.IsZero() {
			at := s.LastSuccessAt
			row.TLS.LastSuccessAt = &at
		}
		switch {
		case s.Failing:
			row.TLS.Status = TLSFailing
			since := s.FailingSince
			row.TLS.FailingSince = &since
		case status.Logs == LogsOK || !s.LastSuccessAt.IsZero():
			row.TLS.Status = TLSOK
		default:
			row.TLS.Status = TLSUnknown
		}
	}
}

// applyDNS runs the public lookups concurrently, each row bounded by the DNS
// budget and the shared limiter.
func applyDNS(ctx context.Context, rows []Domain, srv Server, cache *DNSCache) {
	var wg sync.WaitGroup
	for i := range rows {
		row := &rows[i]
		if row.Skip == SkipExternalLabel {
			continue
		}
		// Name rules apply to a label too: a host-ingress placeholder has no
		// public record to look for.
		switch classifyName(row.Host) {
		case SkipReservedName:
			row.DNS.Detail = "reserved name: never publicly resolvable"
			continue
		case SkipPrivateName:
			row.DNS.Detail = "private name: only resolves inside a network"
			continue
		case SkipIPLiteral, SkipWildcard, SkipInvalidName:
			continue
		}
		wg.Add(1)
		go func(row *Domain) {
			defer wg.Done()
			ans := cache.get(ctx, row.Host)
			row.DNS.Answers = append([]string{}, ans.Answers...)
			row.DNS.Status = dns.Classify(ans, srv.Addresses, srv.PublicIPv4Known, srv.PublicIPv6Known)
			switch row.DNS.Status {
			case dns.StatusNXDomain:
				row.DNS.Detail = "no address record at the public resolvers"
			case dns.StatusProxied:
				row.DNS.Detail = "answers from Cloudflare: origin cannot be read from DNS"
			case dns.StatusUnknown:
				if ans.Err != nil {
					row.DNS.Detail = "public resolvers did not answer"
				} else {
					row.DNS.Detail = "this server's public address for that record type could not be determined"
				}
			}
		}(row)
	}
	wg.Wait()
}
