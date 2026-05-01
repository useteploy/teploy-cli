package caddy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/useteploy/teploy/internal/ssh"
)

const (
	adminAPI        = "http://localhost:2019"
	tmpConfig       = "/tmp/teploy_caddy_config.json"
	caddyfilePath   = "/deployments/caddy/Caddyfile"
	tmpCaddyfile    = "/tmp/teploy_caddyfile.tmp"
	markerBeginFmt  = "# TEPLOY BEGIN %s"
	markerEndFmt    = "# TEPLOY END %s"
)

// HTTPApp represents Caddy's HTTP application configuration.
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

// HealthChecks configures active health checking for upstreams.
type HealthChecks struct {
	Active *ActiveHealthCheck `json:"active,omitempty"`
}

// ActiveHealthCheck configures how Caddy actively probes upstream health.
type ActiveHealthCheck struct {
	Path     string `json:"path,omitempty"`
	Interval string `json:"interval,omitempty"` // duration string, e.g. "10s"
	Timeout  string `json:"timeout,omitempty"`  // duration string, e.g. "5s"
}

// LoadBalancing configures load balancing strategy.
type LoadBalancing struct {
	SelectionPolicy *SelectionPolicy `json:"selection_policy,omitempty"`
}

// SelectionPolicy defines how an upstream is selected.
type SelectionPolicy struct {
	Policy string `json:"policy,omitempty"` // round_robin, least_conn, etc.
}

// Client communicates with the Caddy admin API on a remote server via SSH.
// The admin API is accessible at localhost:2019 on the server (bound to
// 127.0.0.1 only, never publicly exposed).
type Client struct {
	exec ssh.Executor
}

// NewClient creates a Caddy admin API client.
func NewClient(exec ssh.Executor) *Client {
	return &Client{exec: exec}
}

// SetRoute adds or updates a reverse proxy route for the given app.
// Routes traffic for the domain to the given upstream at the specified
// container port. Caddy provisions HTTPS certificates automatically.
//
// The `domain` argument may be a comma-separated list (e.g.
// "example.com, www.example.com") to serve the same app on multiple hosts.
//
// Callers should pass a specific container name as the upstream rather
// than a shared network alias: during deploys the alias can resolve to
// both old and new containers and Docker DNS round-robins between them,
// briefly routing traffic to a container that's about to be stopped.
//
// Uses Caddy's ID-based API to surgically upsert a single route without
// touching any other routes (including those defined in the Caddyfile).
func (c *Client) SetRoute(ctx context.Context, app, domain, upstream string, containerPort int) error {
	hosts := parseDomains(domain)
	if len(hosts) == 0 {
		return fmt.Errorf("SetRoute: domain must be non-empty")
	}
	routeID := "teploy-" + app
	newRoute := Route{
		ID:    routeID,
		Match: []Match{{Host: hosts}},
		Handle: []Handler{{
			Handler:   "reverse_proxy",
			Upstreams: []Upstream{{Dial: fmt.Sprintf("%s:%d", upstream, containerPort)}},
		}},
	}

	if err := c.ensureServer(ctx); err != nil {
		return err
	}
	if err := c.putRouteByID(ctx, routeID, newRoute); err != nil {
		return err
	}
	// Mirror to on-disk Caddyfile so the route survives a manual
	// `caddy reload --config <file>` (which would otherwise wipe admin-API
	// state not present in the Caddyfile).
	return c.upsertCaddyfileBlock(ctx, app, reverseProxyBlock(hosts, upstream, containerPort))
}

// SetLoadBalancer adds or updates a load-balanced reverse proxy route for the
// given app. Traffic for the domain is distributed across multiple upstreams
// using round-robin with active health checks. `domain` supports comma-
// separated lists like SetRoute.
func (c *Client) SetLoadBalancer(ctx context.Context, app, domain string, upstreams []Upstream) error {
	hosts := parseDomains(domain)
	if len(hosts) == 0 {
		return fmt.Errorf("SetLoadBalancer: domain must be non-empty")
	}
	routeID := "teploy-lb-" + app
	newRoute := Route{
		ID:    routeID,
		Match: []Match{{Host: hosts}},
		Handle: []Handler{{
			Handler:   "reverse_proxy",
			Upstreams: upstreams,
			HealthChecks: &HealthChecks{
				Active: &ActiveHealthCheck{
					Path:     "/up",
					Interval: "10s",
					Timeout:  "5s",
				},
			},
			LoadBalancing: &LoadBalancing{
				SelectionPolicy: &SelectionPolicy{
					Policy: "round_robin",
				},
			},
		}},
	}

	if err := c.ensureServer(ctx); err != nil {
		return err
	}
	if err := c.putRouteByID(ctx, routeID, newRoute); err != nil {
		return err
	}
	return c.upsertCaddyfileBlock(ctx, "lb-"+app, loadBalancerBlock(hosts, upstreams))
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

// SetMaintenance enables maintenance mode for the given app.
// Inserts a 503 static response route that intercepts traffic for the domain.
// The existing reverse proxy route is left in place. `domain` supports comma-
// separated lists like SetRoute.
func (c *Client) SetMaintenance(ctx context.Context, app, domain string) error {
	hosts := parseDomains(domain)
	if len(hosts) == 0 {
		return fmt.Errorf("SetMaintenance: domain must be non-empty")
	}
	routeID := "teploy-maint-" + app
	maintRoute := Route{
		ID:    routeID,
		Match: []Match{{Host: hosts}},
		Handle: []Handler{{
			Handler:    "static_response",
			StatusCode: "503",
			Headers: map[string][]string{
				"Content-Type": {"text/html; charset=utf-8"},
				"Retry-After":  {"3600"},
			},
			Body: maintenancePage,
		}},
	}

	if err := c.ensureServer(ctx); err != nil {
		return err
	}

	// Maintenance routes must appear before regular routes to intercept traffic.
	// Prepend by inserting at index 0 of the routes array.
	return c.prependRouteByID(ctx, routeID, maintRoute)
}

// RemoveMaintenance disables maintenance mode for the given app.
func (c *Client) RemoveMaintenance(ctx context.Context, app string) error {
	return c.deleteRouteByID(ctx, "teploy-maint-"+app)
}

// RemoveRoute removes the route for the given app. No-op if no route exists.
func (c *Client) RemoveRoute(ctx context.Context, app string) error {
	if err := c.deleteRouteByID(ctx, "teploy-"+app); err != nil {
		return err
	}
	// Remove both the regular and lb-variants on-disk in a single file
	// rewrite — an app only uses one mode at a time, and two separate reads
	// would race with the first rewrite on any real filesystem.
	return c.removeCaddyfileBlocks(ctx, app, "lb-"+app)
}

// ensureServer makes sure Caddy has an HTTP server (srv0) configured with
// listen addresses. On a fresh Caddy instance with only a minimal Caddyfile,
// the HTTP app may not exist yet. This creates the skeleton without touching
// any existing routes.
func (c *Client) ensureServer(ctx context.Context) error {
	// Check if srv0 already exists.
	_, err := c.exec.Run(ctx, "curl -sf "+adminAPI+"/config/apps/http/servers/srv0")
	if err == nil {
		return nil // server already exists
	}

	// Create the server skeleton. PUT to the specific path so we don't
	// overwrite any existing config at other paths.
	srv := HTTPServer{Listen: []string{":80", ":443"}, Routes: []Route{}}
	body, err := json.Marshal(srv)
	if err != nil {
		return fmt.Errorf("marshaling server config: %w", err)
	}

	if err := c.exec.Upload(ctx, bytes.NewReader(body), tmpConfig, "0644"); err != nil {
		return fmt.Errorf("uploading server config: %w", err)
	}

	cmd := fmt.Sprintf(
		"curl -sf -X PUT %s/config/apps/http/servers/srv0 -H 'Content-Type: application/json' -d @%s",
		adminAPI, tmpConfig,
	)
	_, err = c.exec.Run(ctx, cmd)
	c.exec.Run(ctx, "rm -f "+tmpConfig)
	if err != nil {
		return fmt.Errorf("creating caddy server: %w", err)
	}
	return nil
}

// putRouteByID upserts a route using Caddy's /id/ API endpoint.
// This only touches the targeted route — all other routes (including
// Caddyfile-defined routes) are left untouched.
func (c *Client) putRouteByID(ctx context.Context, routeID string, route Route) error {
	body, err := json.Marshal(route)
	if err != nil {
		return fmt.Errorf("marshaling route: %w", err)
	}

	if err := c.exec.Upload(ctx, bytes.NewReader(body), tmpConfig, "0644"); err != nil {
		return fmt.Errorf("uploading route config: %w", err)
	}

	// Try PATCH on existing route first (update in place).
	cmd := fmt.Sprintf(
		"curl -sf -X PATCH %s/id/%s -H 'Content-Type: application/json' -d @%s",
		adminAPI, routeID, tmpConfig,
	)
	_, err = c.exec.Run(ctx, cmd)

	if err != nil {
		// Route doesn't exist yet — append it to the routes array.
		cmd = fmt.Sprintf(
			"curl -sf -X POST %s/config/apps/http/servers/srv0/routes -H 'Content-Type: application/json' -d @%s",
			adminAPI, tmpConfig,
		)
		_, err = c.exec.Run(ctx, cmd)
	}

	c.exec.Run(ctx, "rm -f "+tmpConfig)
	if err != nil {
		return fmt.Errorf("setting route %s: %w", routeID, err)
	}
	return nil
}

// prependRouteByID inserts a route at the beginning of the routes array.
// Used for maintenance routes that must intercept before reverse proxy routes.
// If a route with the same ID already exists, it is removed first.
func (c *Client) prependRouteByID(ctx context.Context, routeID string, route Route) error {
	// Remove existing route with this ID if present.
	c.deleteRouteByID(ctx, routeID)

	body, err := json.Marshal(route)
	if err != nil {
		return fmt.Errorf("marshaling route: %w", err)
	}

	if err := c.exec.Upload(ctx, bytes.NewReader(body), tmpConfig, "0644"); err != nil {
		return fmt.Errorf("uploading route config: %w", err)
	}

	// Prepend: POST to routes array at index 0 isn't directly supported,
	// so we read current routes, prepend, and write the full routes array.
	httpApp, _ := c.getHTTPApp(ctx)
	if httpApp == nil || httpApp.Servers == nil || httpApp.Servers["srv0"] == nil {
		// No existing routes — just append.
		cmd := fmt.Sprintf(
			"curl -sf -X POST %s/config/apps/http/servers/srv0/routes -H 'Content-Type: application/json' -d @%s",
			adminAPI, tmpConfig,
		)
		_, err = c.exec.Run(ctx, cmd)
		c.exec.Run(ctx, "rm -f "+tmpConfig)
		if err != nil {
			return fmt.Errorf("prepending route %s: %w", routeID, err)
		}
		return nil
	}

	routes := append([]Route{route}, httpApp.Servers["srv0"].Routes...)
	routesBody, err := json.Marshal(routes)
	if err != nil {
		return fmt.Errorf("marshaling routes: %w", err)
	}

	if err := c.exec.Upload(ctx, bytes.NewReader(routesBody), tmpConfig, "0644"); err != nil {
		return fmt.Errorf("uploading routes: %w", err)
	}

	cmd := fmt.Sprintf(
		"curl -sf -X PUT %s/config/apps/http/servers/srv0/routes -H 'Content-Type: application/json' -d @%s",
		adminAPI, tmpConfig,
	)
	_, err = c.exec.Run(ctx, cmd)
	c.exec.Run(ctx, "rm -f "+tmpConfig)
	if err != nil {
		return fmt.Errorf("prepending route %s: %w", routeID, err)
	}
	return nil
}

// deleteRouteByID removes a route by its @id. No-op if the route doesn't exist.
func (c *Client) deleteRouteByID(ctx context.Context, routeID string) error {
	cmd := fmt.Sprintf("curl -sf -X DELETE %s/id/%s", adminAPI, routeID)
	c.exec.Run(ctx, cmd) // ignore error — route may not exist
	return nil
}

func (c *Client) getHTTPApp(ctx context.Context) (*HTTPApp, error) {
	output, err := c.exec.Run(ctx, "curl -sf "+adminAPI+"/config/apps/http")
	if err != nil {
		return nil, err
	}

	var app HTTPApp
	if err := json.Unmarshal([]byte(output), &app); err != nil {
		return nil, fmt.Errorf("parsing caddy HTTP config: %w", err)
	}
	return &app, nil
}

// upsertCaddyfileBlock idempotently writes a Teploy-managed snippet for `app`
// into the on-disk Caddyfile, wrapped in per-app markers. Passing block=""
// removes the block.
//
// This exists because Teploy routes live only in Caddy's admin API config.
// Without mirroring to disk, a manual `caddy reload --config <file>` loads
// the Caddyfile and wipes every Teploy route. Mirroring makes the on-disk
// Caddyfile the source of truth that survives any reload.
//
// The caller is expected to have already updated admin API state; this
// function only syncs the on-disk representation. Written atomically via a
// temp-file + mv to avoid a partial Caddyfile being observed by another
// reader mid-write.
func (c *Client) upsertCaddyfileBlock(ctx context.Context, app, block string) error {
	current, err := c.exec.Run(ctx, "cat "+caddyfilePath)
	if err != nil {
		return fmt.Errorf("reading caddyfile (did setup run?): %w", err)
	}

	begin := fmt.Sprintf(markerBeginFmt, app)
	end := fmt.Sprintf(markerEndFmt, app)
	updated := removeCaddyfileBlock(current, begin, end)

	if block != "" {
		wrapped := begin + "\n" + strings.TrimRight(block, "\n") + "\n" + end
		updated = strings.TrimRight(updated, "\n") + "\n\n" + wrapped + "\n"
	} else {
		// On removal, tidy trailing whitespace so repeated remove calls don't drift.
		updated = strings.TrimRight(updated, "\n") + "\n"
	}

	if err := c.exec.Upload(ctx, strings.NewReader(updated), tmpCaddyfile, "0644"); err != nil {
		return fmt.Errorf("uploading caddyfile: %w", err)
	}
	if _, err := c.exec.Run(ctx, "mv "+tmpCaddyfile+" "+caddyfilePath); err != nil {
		return fmt.Errorf("writing caddyfile: %w", err)
	}
	return nil
}

// removeCaddyfileBlock removes every region bounded by `begin`..`end` from the
// content, collapsing surrounding blank lines so repeated upserts don't cause
// the Caddyfile to grow stray whitespace. Tolerates a missing end marker by
// trimming from the begin marker to end-of-file.
func removeCaddyfileBlock(content, begin, end string) string {
	for {
		bi := strings.Index(content, begin)
		if bi < 0 {
			return content
		}
		tail := content[bi:]
		ei := strings.Index(tail, end)
		if ei < 0 {
			// Malformed (missing end marker) — best-effort truncate.
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

// removeCaddyfileBlocks strips Teploy-managed blocks for all given apps in a
// single file read/write. Used by paths like RemoveRoute that would otherwise
// issue overlapping reads and race on the on-disk state.
func (c *Client) removeCaddyfileBlocks(ctx context.Context, apps ...string) error {
	current, err := c.exec.Run(ctx, "cat "+caddyfilePath)
	if err != nil {
		return fmt.Errorf("reading caddyfile (did setup run?): %w", err)
	}

	updated := current
	for _, app := range apps {
		begin := fmt.Sprintf(markerBeginFmt, app)
		end := fmt.Sprintf(markerEndFmt, app)
		updated = removeCaddyfileBlock(updated, begin, end)
	}
	updated = strings.TrimRight(updated, "\n") + "\n"

	if err := c.exec.Upload(ctx, strings.NewReader(updated), tmpCaddyfile, "0644"); err != nil {
		return fmt.Errorf("uploading caddyfile: %w", err)
	}
	if _, err := c.exec.Run(ctx, "mv "+tmpCaddyfile+" "+caddyfilePath); err != nil {
		return fmt.Errorf("writing caddyfile: %w", err)
	}
	return nil
}

// parseDomains splits a Teploy config "domain" field into a normalized host
// list, tolerating comma-separated entries with incidental whitespace. Returns
// nil when the input yields no non-empty hosts.
func parseDomains(domain string) []string {
	parts := strings.Split(domain, ",")
	hosts := make([]string, 0, len(parts))
	for _, p := range parts {
		if h := strings.TrimSpace(p); h != "" {
			hosts = append(hosts, h)
		}
	}
	return hosts
}

// reverseProxyBlock renders a Caddyfile snippet equivalent to the admin-API
// route produced by SetRoute. Kept in lockstep with putRouteByID's payload.
func reverseProxyBlock(hosts []string, upstream string, port int) string {
	return fmt.Sprintf("%s {\n\treverse_proxy %s:%d\n}", strings.Join(hosts, ", "), upstream, port)
}

// loadBalancerBlock renders a Caddyfile snippet equivalent to the admin-API
// route produced by SetLoadBalancer. Round-robin + active /up health checks.
func loadBalancerBlock(hosts []string, upstreams []Upstream) string {
	dials := make([]string, len(upstreams))
	for i, u := range upstreams {
		dials[i] = u.Dial
	}
	return fmt.Sprintf(
		"%s {\n\treverse_proxy %s {\n\t\tlb_policy round_robin\n\t\thealth_uri /up\n\t\thealth_interval 10s\n\t\thealth_timeout 5s\n\t}\n}",
		strings.Join(hosts, ", "), strings.Join(dials, " "),
	)
}

// StaticBlockOpts configures the Caddyfile site block produced by StaticBlock
// for a type:static deploy. All fields are optional; defaults match the Vercel-
// style "drop a folder, get a working site" experience.
type StaticBlockOpts struct {
	Hosts       []string          // required: ["example.com", "www.example.com"]
	Root        string            // required: container-side path to the served files (e.g. /srv/static/myapp/current)
	SPA         bool              // try_files fallback for SPA routing
	SPAFallback string            // file to serve as fallback (default: /index.html)
	Cache       map[string]string // path glob → Cache-Control value
	Headers     map[string]string // arbitrary response headers
	CaddyExtra  string            // raw Caddy directives appended into the site block (escape hatch)
}

// StaticBlock renders the Caddyfile snippet that serves a static deploy. It
// pairs with SetStaticRoute() and the on-disk Caddyfile mirror so the result
// survives caddy reloads.
//
// Sensible defaults are applied: gzip encoding, file_server with
// precompressed gzip, common security headers, and no-cache-by-default for
// HTML alongside immutable cache for hashed asset paths Vite-style.
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

	// Default security headers. Users can override via opts.Headers (which we
	// emit afterwards so it wins on duplicate keys).
	b.WriteString("\theader {\n")
	b.WriteString("\t\tX-Content-Type-Options \"nosniff\"\n")
	b.WriteString("\t\tX-Frame-Options \"SAMEORIGIN\"\n")
	b.WriteString("\t\tReferrer-Policy \"strict-origin-when-cross-origin\"\n")
	b.WriteString("\t\tPermissions-Policy \"camera=(), microphone=(), geolocation=()\"\n")
	for k, v := range opts.Headers {
		b.WriteString(fmt.Sprintf("\t\t%s %q\n", k, v))
	}
	b.WriteString("\t}\n")

	// Per-path Cache-Control rules. Each becomes a named matcher + header.
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

// SetStaticRoute upserts a Caddyfile block for a static deploy and removes any
// previously-installed reverse_proxy/load-balancer block for the same app
// (covers the case of a container-to-static migration where the user changes
// `type:` in teploy.yml).
func (c *Client) SetStaticRoute(ctx context.Context, app, domain string, opts StaticBlockOpts) error {
	hosts := parseDomains(domain)
	if len(hosts) == 0 {
		return fmt.Errorf("SetStaticRoute: domain must be non-empty")
	}
	opts.Hosts = hosts

	// If the same app previously deployed as a container, the admin API still
	// holds its teploy-<app> route. Surgically remove it so the static block
	// in the Caddyfile becomes the only route for these hosts.
	_ = c.deleteRouteByID(ctx, "teploy-"+app)
	_ = c.deleteRouteByID(ctx, "teploy-lb-"+app)

	// Likewise drop any stale lb-<app> Caddyfile block.
	if err := c.upsertCaddyfileBlock(ctx, "lb-"+app, ""); err != nil {
		return err
	}

	return c.upsertCaddyfileBlock(ctx, app, StaticBlock(opts))
}
