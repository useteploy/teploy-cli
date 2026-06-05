package caddy

import (
	"context"
	"fmt"
	"strings"

	"github.com/useteploy/teploy/internal/ssh"
)

const (
	caddyfilePath = "/deployments/caddy/Caddyfile"
	tmpCaddyfile  = "/tmp/teploy_caddyfile.tmp"

	markerBeginFmt = "# TEPLOY BEGIN %s"
	markerEndFmt   = "# TEPLOY END %s"

	// caddyContainer is the fixed name Teploy gives the front proxy (see
	// cli/setup.go). containerCaddyfile is where the Caddyfile is mounted
	// inside it — the path `caddy reload` reads.
	caddyContainer     = "caddy"
	containerCaddyfile = "/etc/caddy/Caddyfile"

	// reloadCmd hot-reloads Caddy from the on-disk Caddyfile (zero-downtime).
	// Run inside the container so it reaches Caddy's admin API on the
	// container's loopback — the admin API is never exposed off-box.
	reloadCmd = "docker exec caddy caddy reload --config /etc/caddy/Caddyfile --adapter caddyfile"

	// deliveredOK / deliveredStale are sentinels echoed by the post-reload
	// delivery check (verifyDelivered) so we can tell a config that actually
	// reached the running container from a stale-inode divergence.
	deliveredOK    = "TEPLOY_CADDY_OK"
	deliveredStale = "TEPLOY_CADDY_STALE"

	// lockDir serializes Caddyfile edits + reloads so concurrent deploys of
	// different apps to the same server can't clobber the shared file.
	lockDir          = "/deployments/caddy/.lock"
	staleLockSeconds = 120 // break a lock left behind by a crashed deploy
	lockWaitTries    = 60  // ~30s of contention before giving up

	// maintStashFmt holds an app's pre-maintenance route block so it can be
	// restored on RemoveMaintenance without re-deploying.
	maintStashFmt = "/deployments/%s/.maintenance-block"
)

// HTTPApp represents Caddy's HTTP application configuration. Kept for callers
// (e.g. the dashboard) that read the live admin API for read-only display.
type HTTPApp struct {
	Servers map[string]*HTTPServer `json:"servers"`
}

// HTTPServer is a Caddy HTTP server with listen addresses and routes.
type HTTPServer struct {
	Listen []string `json:"listen"`
	Routes []Route  `json:"routes"`
}

// Route is a single Caddy routing rule identified by an @id.
type Route struct {
	ID     string    `json:"@id,omitempty"`
	Match  []Match   `json:"match,omitempty"`
	Handle []Handler `json:"handle"`
}

// Match defines route matching criteria.
type Match struct {
	Host []string `json:"host"`
}

// Handler defines how a matched request is processed.
type Handler struct {
	Handler       string              `json:"handler"`
	Upstreams     []Upstream          `json:"upstreams,omitempty"`
	HealthChecks  *HealthChecks       `json:"health_checks,omitempty"`
	LoadBalancing *LoadBalancing      `json:"load_balancing,omitempty"`
	StatusCode    string              `json:"status_code,omitempty"`
	Headers       map[string][]string `json:"headers,omitempty"`
	Body          string              `json:"body,omitempty"`
}

// Upstream is a reverse proxy target address.
type Upstream struct {
	Dial string `json:"dial"`
}

// TLS carries container-side paths to a custom certificate + key for
// terminating TLS on a site block (e.g. a Cloudflare Origin Certificate).
// When both are empty, Caddy uses automatic HTTPS (ACME) — the default.
//
// Custom certs are required when the public hostname is fronted by a proxy
// that hides the origin from ACME challenges (Cloudflare proxied DNS,
// behind a tunnel, etc.), so Caddy can't complete an ACME challenge and
// must present a pre-issued cert instead.
type TLS struct {
	Cert string // container path, e.g. /etc/caddy/tls/myapp.crt
	Key  string // container path, e.g. /etc/caddy/tls/myapp.key
}

// directive returns the indented `tls <cert> <key>` line for a site block,
// or "" when no custom cert is configured (automatic HTTPS).
func (t TLS) directive() string {
	if t.Cert == "" || t.Key == "" {
		return ""
	}
	return fmt.Sprintf("\ttls %s %s\n", t.Cert, t.Key)
}

// HealthChecks configures active health checking for upstreams.
type HealthChecks struct {
	Active *ActiveHealthCheck `json:"active,omitempty"`
}

// ActiveHealthCheck configures how Caddy actively probes upstream health.
type ActiveHealthCheck struct {
	Path     string `json:"path,omitempty"`
	Interval string `json:"interval,omitempty"`
	Timeout  string `json:"timeout,omitempty"`
}

// LoadBalancing configures load balancing strategy.
type LoadBalancing struct {
	SelectionPolicy *SelectionPolicy `json:"selection_policy,omitempty"`
}

// SelectionPolicy defines how an upstream is selected.
type SelectionPolicy struct {
	Policy string `json:"policy,omitempty"`
}

// Client manages Caddy routing by editing the on-disk Caddyfile — the single
// source of truth — and hot-reloading Caddy. The Caddyfile is loaded on every
// boot and reload, so there is no hidden admin-API/autosave state to diverge
// from what's on disk. All mutations are serialized behind a server-side lock,
// and a failed reload rolls the file back so Caddy never persists a config it
// can't load.
type Client struct {
	exec ssh.Executor
}

// NewClient creates a Caddy client backed by the given SSH executor.
func NewClient(exec ssh.Executor) *Client {
	return &Client{exec: exec}
}

// SetRoute adds or updates a reverse proxy route for the given app, serving the
// (comma-separated) domain to the given upstream container:port.
//
// Callers should pass a specific container name as the upstream rather than a
// shared network alias: during deploys the alias can resolve to both old and
// new containers and Docker DNS round-robins between them.
//
// If a hand-written (non-Teploy) site block already serves any of these hosts —
// common when adopting a server that previously ran another proxy — it is
// replaced, so Teploy's block becomes the single authority for the domain.
func (c *Client) SetRoute(ctx context.Context, app, domain, upstream string, containerPort int, tls TLS) error {
	hosts, err := parseDomains(domain)
	if err != nil {
		return err
	}
	if len(hosts) == 0 {
		return fmt.Errorf("SetRoute: domain must be non-empty")
	}
	return c.applyManagedBlock(ctx, app, hosts, reverseProxyBlock(hosts, upstream, containerPort, tls))
}

// SetLoadBalancer adds or updates a load-balanced reverse proxy route: traffic
// for the domain is distributed across upstreams via round-robin with active
// /up health checks. Replaces any prior route block for the same app.
func (c *Client) SetLoadBalancer(ctx context.Context, app, domain string, upstreams []Upstream, tls TLS) error {
	hosts, err := parseDomains(domain)
	if err != nil {
		return err
	}
	if len(hosts) == 0 {
		return fmt.Errorf("SetLoadBalancer: domain must be non-empty")
	}
	return c.applyManagedBlock(ctx, app, hosts, loadBalancerBlock(hosts, upstreams, tls))
}

// SetStaticRoute upserts a Caddyfile block that serves a static deploy.
func (c *Client) SetStaticRoute(ctx context.Context, app, domain string, opts StaticBlockOpts) error {
	hosts, err := parseDomains(domain)
	if err != nil {
		return err
	}
	if len(hosts) == 0 {
		return fmt.Errorf("SetStaticRoute: domain must be non-empty")
	}
	opts.Hosts = hosts
	return c.applyManagedBlock(ctx, app, hosts, StaticBlock(opts))
}

// RemoveRoute removes the route block for the given app. No-op if absent.
func (c *Client) RemoveRoute(ctx context.Context, app string) error {
	return c.applyManagedBlock(ctx, app, nil, "")
}

// maintenancePage is the HTML returned during maintenance mode.
const maintenancePage = `<!DOCTYPE html>
<html><head>
<title>Maintenance</title>
<meta name="viewport" content="width=device-width, initial-scale=1">
<style>
body{font-family:-apple-system,sans-serif;display:flex;justify-content:center;align-items:center;min-height:100vh;margin:0;background:#f5f5f5}
.box{text-align:center;padding:2rem}
h1{font-size:1.5rem;color:#333}
p{color:#666}
</style>
</head><body>
<div class="box">
<h1>We'll be back soon</h1>
<p>This site is currently undergoing maintenance. Please check back shortly.</p>
</div>
</body></html>`

// SetMaintenance enables maintenance mode: the app's domain returns a 503
// maintenance page. The app's current route block is stashed so it can be
// restored by RemoveMaintenance without a redeploy.
func (c *Client) SetMaintenance(ctx context.Context, app, domain string) error {
	hosts, err := parseDomains(domain)
	if err != nil {
		return err
	}
	if len(hosts) == 0 {
		return fmt.Errorf("SetMaintenance: domain must be non-empty")
	}
	return c.mutate(ctx, func(prev string) (string, error) {
		begin := fmt.Sprintf(markerBeginFmt, app)
		end := fmt.Sprintf(markerEndFmt, app)
		if cur := extractCaddyfileBlock(prev, begin, end); cur != "" {
			stash := fmt.Sprintf(maintStashFmt, app)
			if err := c.exec.Upload(ctx, strings.NewReader(cur), stash, "0644"); err != nil {
				return "", fmt.Errorf("stashing route for maintenance: %w", err)
			}
		}
		return renderUpdated(prev, app, hosts, maintenanceBlock(hosts)), nil
	})
}

// RemoveMaintenance disables maintenance mode, restoring the stashed route
// block. It fails safe: a missing stash is a no-op, and a stash that exists but
// can't be read (or is empty) aborts WITHOUT touching the route — the previous
// version ignored the read error, so any transient SSH/read failure rendered an
// empty block and silently deleted the app's route, taking the domain offline.
func (c *Client) RemoveMaintenance(ctx context.Context, app string) error {
	stash := fmt.Sprintf(maintStashFmt, app)

	// Missing stash → maintenance isn't active (or was already removed). No-op.
	// `test -f` so a genuine read error below isn't masked by `cat 2>/dev/null`.
	if _, err := c.exec.Run(ctx, "test -f "+stash); err != nil {
		return nil
	}
	saved, err := c.exec.Run(ctx, "cat "+stash)
	if err != nil {
		return fmt.Errorf("reading stashed maintenance route (route left unchanged): %w", err)
	}
	restored := strings.Trim(saved, "\n")
	if restored == "" {
		return fmt.Errorf("stashed maintenance route for %s is empty — refusing to remove the route; delete %s manually if this is intended", app, stash)
	}

	if err := c.mutate(ctx, func(prev string) (string, error) {
		return renderUpdated(prev, app, nil, restored), nil
	}); err != nil {
		return err
	}

	// Delete the stash only after the reload succeeded, so a failed (rolled
	// back) reload can be retried.
	c.exec.Run(ctx, "rm -f "+stash)
	return nil
}

// mutate serializes a Caddyfile edit + reload behind the server lock. transform
// receives the current Caddyfile and returns the new contents. If the reload
// fails (e.g. the new config is invalid), the on-disk file is rolled back so
// Caddy never persists a config it can't boot from.
func (c *Client) mutate(ctx context.Context, transform func(prev string) (string, error)) error {
	if err := c.acquireLock(ctx); err != nil {
		return err
	}
	defer c.releaseLock(ctx)

	prev, err := c.exec.Run(ctx, "cat "+caddyfilePath)
	if err != nil {
		return fmt.Errorf("reading caddyfile (did setup run?): %w", err)
	}

	updated, err := transform(prev)
	if err != nil {
		return err
	}
	if updated == prev {
		return nil
	}

	if err := c.writeCaddyfile(ctx, updated); err != nil {
		return err
	}
	if err := c.reload(ctx); err != nil {
		// Roll back so a bad config is never left on disk to break the next boot.
		if rbErr := c.writeCaddyfile(ctx, prev); rbErr == nil {
			c.reload(ctx) // best-effort restore of the live config
		}
		return fmt.Errorf("caddy reload failed, rolled back: %w", err)
	}
	// Confirm the running container actually sees the config we just wrote.
	// A legacy single-file Caddyfile bind mount pins the container to a stale
	// inode, so `reload` can "succeed" against an old file while serving stale
	// routes — a silent 502 once the old app container stops. Fail loudly here
	// (the deploy aborts before the old container is torn down) instead.
	if err := c.verifyDelivered(ctx); err != nil {
		return err
	}
	return nil
}

// singleFileMount reports whether the caddy container binds the Caddyfile as a
// single file (mount Destination == containerCaddyfile) rather than mounting
// its directory (/etc/caddy). A single-file bind mount pins the container to
// the file's inode at create time, so an atomic rename (tmp + mv) swaps in a
// new inode the container never observes. Mirrors the detection in
// cli/setup.go. On inspect failure it returns false (the modern directory-mount
// default); verifyDelivered is the backstop if that guess is wrong.
func (c *Client) singleFileMount(ctx context.Context) bool {
	out, err := c.exec.Run(ctx, fmt.Sprintf(
		"docker inspect -f '{{range .Mounts}}{{.Destination}} {{end}}' %s", caddyContainer))
	if err != nil {
		return false
	}
	return strings.Contains(out, containerCaddyfile)
}

// verifyDelivered checks that the caddy container's view of the Caddyfile
// matches the file Teploy wrote on the host, comparing checksums on each side
// in a single command. A mismatch means the write never reached the running
// container (the legacy single-file-mount inode-pinning bug). A failure to run
// the check at all is not treated as fatal — the primary single-file handling
// in writeCaddyfile already ran; this is defense in depth.
func (c *Client) verifyDelivered(ctx context.Context) error {
	check := fmt.Sprintf(
		"[ \"$(docker exec %s md5sum %s 2>/dev/null | cut -d' ' -f1)\" = \"$(md5sum %s 2>/dev/null | cut -d' ' -f1)\" ] && echo %s || echo %s",
		caddyContainer, containerCaddyfile, caddyfilePath, deliveredOK, deliveredStale)
	out, err := c.exec.Run(ctx, check)
	if err != nil {
		return nil
	}
	if !strings.Contains(out, deliveredOK) {
		return fmt.Errorf(
			"caddy reloaded but the container's %s does not match the config Teploy wrote to %s — "+
				"the caddy container is likely using a legacy single-file bind mount that pins a stale inode; "+
				"run `teploy setup` to migrate it to the directory mount",
			containerCaddyfile, caddyfilePath)
	}
	return nil
}

// applyManagedBlock upserts (block != "") or removes (block == "") the app's
// marker-delimited block, adopting any foreign block for the same hosts.
func (c *Client) applyManagedBlock(ctx context.Context, app string, hosts []string, block string) error {
	return c.mutate(ctx, func(prev string) (string, error) {
		return renderUpdated(prev, app, hosts, block), nil
	})
}

// renderUpdated produces new Caddyfile contents: it removes the app's previous
// Teploy block (and a legacy lb-<app> block), removes any non-Teploy block
// serving the same hosts (brownfield adoption), then appends the new block
// wrapped in per-app markers. An empty block just performs the removals.
func renderUpdated(prev, app string, hosts []string, block string) string {
	updated := removeCaddyfileBlock(prev, fmt.Sprintf(markerBeginFmt, app), fmt.Sprintf(markerEndFmt, app))
	// Legacy: older versions used a separate lb-<app> marker block.
	updated = removeCaddyfileBlock(updated, fmt.Sprintf(markerBeginFmt, "lb-"+app), fmt.Sprintf(markerEndFmt, "lb-"+app))
	if len(hosts) > 0 {
		updated = removeForeignHostBlocks(updated, hosts)
	}

	if block != "" {
		begin := fmt.Sprintf(markerBeginFmt, app)
		end := fmt.Sprintf(markerEndFmt, app)
		wrapped := begin + "\n" + strings.TrimRight(block, "\n") + "\n" + end
		return strings.TrimRight(updated, "\n") + "\n\n" + wrapped + "\n"
	}
	return strings.TrimRight(updated, "\n") + "\n"
}

// writeCaddyfile persists the Caddyfile so the running caddy container sees it
// on the next reload. On a directory-mounted caddy (the modern default) it
// writes atomically via a temp file + rename: the container resolves the file
// by path on each reload, so the swapped-in inode is picked up. On a legacy
// single-file bind mount, that rename would swap an inode the pinned container
// can never see (silent-502 bug), so it writes in place instead — Upload's
// `cat > path` truncates the existing inode, the only write such a container
// observes. The atomicity trade-off is moot there: caddy reads the file only on
// the explicit reload Teploy triggers after the write completes.
func (c *Client) writeCaddyfile(ctx context.Context, content string) error {
	if c.singleFileMount(ctx) {
		if err := c.exec.Upload(ctx, strings.NewReader(content), caddyfilePath, "0644"); err != nil {
			return fmt.Errorf("writing caddyfile in place: %w", err)
		}
		return nil
	}
	if err := c.exec.Upload(ctx, strings.NewReader(content), tmpCaddyfile, "0644"); err != nil {
		return fmt.Errorf("uploading caddyfile: %w", err)
	}
	if _, err := c.exec.Run(ctx, "mv "+tmpCaddyfile+" "+caddyfilePath); err != nil {
		return fmt.Errorf("writing caddyfile: %w", err)
	}
	return nil
}

func (c *Client) reload(ctx context.Context) error {
	if _, err := c.exec.Run(ctx, reloadCmd); err != nil {
		return err
	}
	return nil
}

// acquireLock takes a mkdir-based mutex on the Caddyfile, breaking a stale lock
// left by a crashed deploy. Held only for the brief edit+reload, so contention
// is rare and short.
func (c *Client) acquireLock(ctx context.Context) error {
	for i := 0; i < lockWaitTries; i++ {
		if _, err := c.exec.Run(ctx, "mkdir "+lockDir); err == nil {
			return nil
		}
		// Break a stale lock (older than staleLockSeconds), then wait and retry.
		c.exec.Run(ctx, fmt.Sprintf(
			"[ -d %s ] && [ $(( $(date +%%s) - $(stat -c %%Y %s 2>/dev/null || echo 0) )) -gt %d ] && rm -rf %s || true",
			lockDir, lockDir, staleLockSeconds, lockDir,
		))
		c.exec.Run(ctx, "sleep 0.5")
	}
	return fmt.Errorf("timed out acquiring caddy lock %s", lockDir)
}

func (c *Client) releaseLock(ctx context.Context) {
	c.exec.Run(ctx, "rmdir "+lockDir+" 2>/dev/null || true")
}

// removeForeignHostBlocks strips top-level Caddyfile site blocks that serve any
// of the given hosts and are NOT inside a Teploy marker region. This lets a
// deploy adopt a domain previously served by a hand-written block, leaving a
// single authoritative block per host (no duplicate-address errors, no
// route-ordering ambiguity).
func removeForeignHostBlocks(content string, hosts []string) string {
	lines := strings.Split(content, "\n")
	out := make([]string, 0, len(lines))
	inMarker := false
	for i := 0; i < len(lines); {
		line := lines[i]
		trimmed := strings.TrimSpace(line)

		if strings.HasPrefix(trimmed, "# TEPLOY BEGIN ") {
			inMarker = true
			out = append(out, line)
			i++
			continue
		}
		if strings.HasPrefix(trimmed, "# TEPLOY END ") {
			inMarker = false
			out = append(out, line)
			i++
			continue
		}

		// A top-level site-block opener: column 0, not a comment, not the
		// global options block "{", not a snippet "(name) {", ends with "{".
		isOpener := !inMarker && len(line) > 0 && !isSpaceByte(line[0]) &&
			!strings.HasPrefix(trimmed, "#") && !strings.HasPrefix(trimmed, "{") &&
			!strings.HasPrefix(trimmed, "(") && strings.HasSuffix(trimmed, "{")

		if isOpener {
			depth := strings.Count(line, "{") - strings.Count(line, "}")
			block := []string{line}
			j := i + 1
			for j < len(lines) && depth > 0 {
				block = append(block, lines[j])
				depth += strings.Count(lines[j], "{") - strings.Count(lines[j], "}")
				j++
			}
			addr := strings.TrimSpace(strings.TrimSuffix(trimmed, "{"))
			if addressMatchesHosts(addr, hosts) {
				i = j // drop the foreign block
				continue
			}
			out = append(out, block...)
			i = j
			continue
		}

		out = append(out, line)
		i++
	}
	return strings.Join(out, "\n")
}

// addressMatchesHosts reports whether a Caddyfile site-address (e.g.
// "example.com, www.example.com") references any of the given hosts.
func addressMatchesHosts(addr string, hosts []string) bool {
	for _, a := range strings.Split(addr, ",") {
		a = strings.TrimSpace(a)
		a = strings.TrimPrefix(a, "https://")
		a = strings.TrimPrefix(a, "http://")
		if sp := strings.IndexAny(a, " \t"); sp >= 0 {
			a = a[:sp]
		}
		for _, h := range hosts {
			if a == h {
				return true
			}
		}
	}
	return false
}

func isSpaceByte(b byte) bool { return b == ' ' || b == '\t' }

// extractCaddyfileBlock returns the content between the begin and end markers
// (markers excluded), or "" if not found.
func extractCaddyfileBlock(content, begin, end string) string {
	bi := strings.Index(content, begin)
	if bi < 0 {
		return ""
	}
	tail := content[bi+len(begin):]
	ei := strings.Index(tail, end)
	if ei < 0 {
		return ""
	}
	return strings.Trim(tail[:ei], "\n")
}

// removeCaddyfileBlock removes every region bounded by `begin`..`end`,
// collapsing surrounding blank lines so repeated upserts don't grow stray
// whitespace. Tolerates a missing end marker by trimming to end-of-file.
func removeCaddyfileBlock(content, begin, end string) string {
	for {
		bi := strings.Index(content, begin)
		if bi < 0 {
			return content
		}
		tail := content[bi:]
		ei := strings.Index(tail, end)
		if ei < 0 {
			return strings.TrimRight(content[:bi], "\n") + "\n"
		}
		ei = bi + ei + len(end)
		before := content[:bi]
		after := strings.TrimLeft(content[ei:], "\n")
		switch {
		case before == "":
			content = after
		case after == "":
			content = strings.TrimRight(before, "\n") + "\n"
		default:
			content = strings.TrimRight(before, "\n") + "\n\n" + after
		}
	}
}

// parseDomains splits a Teploy config "domain" field into a normalized host
// list, tolerating comma-separated entries with incidental whitespace.
// parseDomains splits a comma-separated domain list and rejects any entry
// containing a character that would break out of a Caddyfile site address
// (whitespace, newline, braces, comment hash, quote, backslash). This is a
// defense-in-depth denylist at the sink: config-time validation already runs
// the strict validDomain regex, but routes can also arrive from callers that
// bypass it. A denylist (rather than re-applying the strict allowlist) blocks
// injection without rejecting legitimate wildcard or host:port site addresses.
func parseDomains(domain string) ([]string, error) {
	parts := strings.Split(domain, ",")
	hosts := make([]string, 0, len(parts))
	for _, p := range parts {
		h := strings.TrimSpace(p)
		if h == "" {
			continue
		}
		if strings.ContainsAny(h, " \t\r\n{}#\"\\") {
			return nil, fmt.Errorf("invalid domain %q: contains characters not allowed in a Caddy site address", h)
		}
		hosts = append(hosts, h)
	}
	return hosts, nil
}

// reverseProxyBlock renders a Caddyfile reverse-proxy site block.
func reverseProxyBlock(hosts []string, upstream string, port int, tls TLS) string {
	return fmt.Sprintf("%s {\n%s\treverse_proxy %s:%d\n}", strings.Join(hosts, ", "), tls.directive(), upstream, port)
}

// loadBalancerBlock renders a round-robin reverse-proxy block with active /up
// health checks.
func loadBalancerBlock(hosts []string, upstreams []Upstream, tls TLS) string {
	dials := make([]string, len(upstreams))
	for i, u := range upstreams {
		dials[i] = u.Dial
	}
	return fmt.Sprintf(
		"%s {\n%s\treverse_proxy %s {\n\t\tlb_policy round_robin\n\t\thealth_uri /up\n\t\thealth_interval 10s\n\t\thealth_timeout 5s\n\t}\n}",
		strings.Join(hosts, ", "), tls.directive(), strings.Join(dials, " "),
	)
}

// maintenanceBlock renders a site block that returns a 503 maintenance page for
// the given hosts. Caddy adapts the `respond` directive to a static_response
// handler, which the dashboard detects as maintenance mode.
func maintenanceBlock(hosts []string) string {
	return fmt.Sprintf(
		"%s {\n\theader Content-Type \"text/html; charset=utf-8\"\n\theader Retry-After \"3600\"\n\trespond 503 {\n\t\tbody `%s`\n\t}\n}",
		strings.Join(hosts, ", "), maintenancePage,
	)
}

// StaticBlockOpts configures the Caddyfile site block produced by StaticBlock
// for a type:static deploy.
type StaticBlockOpts struct {
	Hosts       []string
	Root        string
	SPA         bool
	SPAFallback string
	Cache       map[string]string
	Headers     map[string]string
	CaddyExtra  string
}

// StaticBlock renders the Caddyfile snippet that serves a static deploy, with
// sensible defaults: gzip, precompressed file_server, security headers, and
// immutable caching for hashed assets.
func StaticBlock(opts StaticBlockOpts) string {
	var b strings.Builder
	b.WriteString(strings.Join(opts.Hosts, ", "))
	b.WriteString(" {\n")
	b.WriteString("\tencode gzip\n")
	b.WriteString(fmt.Sprintf("\troot * %s\n", opts.Root))
	if opts.SPA {
		fallback := opts.SPAFallback
		if fallback == "" {
			fallback = "/index.html"
		}
		b.WriteString(fmt.Sprintf("\ttry_files {path} {path}/ {path}/index.html %s\n", fallback))
	}
	b.WriteString("\tfile_server {\n\t\tprecompressed gzip\n\t}\n")

	b.WriteString("\theader {\n")
	b.WriteString("\t\tX-Content-Type-Options \"nosniff\"\n")
	b.WriteString("\t\tX-Frame-Options \"SAMEORIGIN\"\n")
	b.WriteString("\t\tReferrer-Policy \"strict-origin-when-cross-origin\"\n")
	b.WriteString("\t\tPermissions-Policy \"camera=(), microphone=(), geolocation=()\"\n")
	for k, v := range opts.Headers {
		b.WriteString(fmt.Sprintf("\t\t%s %q\n", k, v))
	}
	b.WriteString("\t}\n")

	i := 0
	for pattern, value := range opts.Cache {
		i++
		matcher := fmt.Sprintf("@cache%d", i)
		b.WriteString(fmt.Sprintf("\t%s path %s\n", matcher, pattern))
		b.WriteString(fmt.Sprintf("\theader %s Cache-Control %q\n", matcher, value))
	}

	if extra := strings.TrimSpace(opts.CaddyExtra); extra != "" {
		b.WriteString("\n\t# user-supplied caddy_extra:\n")
		for _, line := range strings.Split(extra, "\n") {
			if line == "" {
				b.WriteString("\n")
			} else {
				b.WriteString("\t" + line + "\n")
			}
		}
	}

	b.WriteString("}")
	return b.String()
}
