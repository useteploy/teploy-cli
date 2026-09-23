package caddy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/useteploy/teploy/internal/ssh"
)

// fenceLostMarker mirrors state's guard marker (unexported there): the
// stderr sentinel a composed guard emits when refusing an effect. Shared
// by value so the caddy lock's guard is refusal-identifiable by the same
// string handling (state.FenceLost matches it in error text).
const fenceLostMarker = "TEPLOY_FENCE_LOST"

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
// When Cert/Key/Internal are all empty, Caddy uses automatic HTTPS (ACME)
// — the default.
//
// Custom certs are required when the public hostname is fronted by a proxy
// that hides the origin from ACME challenges (Cloudflare proxied DNS,
// behind a tunnel, etc.), so Caddy can't complete an ACME challenge and
// must present a pre-issued cert instead.
type TLS struct {
	Cert string // container path, e.g. /etc/caddy/tls/myapp.crt
	Key  string // container path, e.g. /etc/caddy/tls/myapp.key
	// Internal requests Caddy's own local CA (self-signed) instead of a
	// custom cert or ACME. Mutually exclusive with Cert/Key — see
	// config.TLSConfig.Internal, which is where this is actually set from
	// teploy.yml.
	Internal bool
}

// directive returns the indented `tls` line for a site block — `tls
// internal` for Internal, `tls <cert> <key>` for a custom cert, or "" when
// neither is configured (automatic HTTPS).
func (t TLS) directive() string {
	if t.Internal {
		return "\ttls internal\n"
	}
	if t.Cert == "" || t.Key == "" {
		return ""
	}
	return fmt.Sprintf("\ttls %s %s\n", t.Cert, t.Key)
}

// IsPubliclyRoutable reports whether host is a real hostname rather than a
// literal IP address. Caddy's automatic HTTPS can't complete an ACME
// challenge for a literal IP (Let's Encrypt's default issuance doesn't
// cover IP SANs, and a private/LAN address isn't reachable from a public
// CA at all) — left as the default, Caddy just hangs retrying forever.
// siteAddresses (below) gives these an explicit http:// scheme instead,
// which tells Caddy not to manage TLS for that address at all.
//
// Deliberately narrow: only literal IP addresses are checked, not
// heuristics on hostname shape (.local, bare no-dot names, etc.). Those
// are real non-public cases too, but less unambiguous — a false positive
// here would silently break HTTPS for what's actually a legitimate public
// domain, a worse failure mode than the hang this fixes.
func IsPubliclyRoutable(host string) bool {
	return net.ParseIP(host) == nil
}

// siteAddresses formats each host as a Caddyfile site address, adding an
// explicit http:// scheme to any host IsPubliclyRoutable can't vouch for —
// unless tls requests HTTPS anyway (a custom cert or tls.internal), in
// which case the operator has explicitly opted in and automatic-HTTPS
// avoidance would just be wrong. See IsPubliclyRoutable for why this
// matters: without it, Caddy attempts (and hangs on) a real ACME challenge
// for addresses that can never complete one.
func siteAddresses(hosts []string, tls TLS) []string {
	wantsTLS := tls.Internal || (tls.Cert != "" && tls.Key != "")
	out := make([]string, len(hosts))
	for i, h := range hosts {
		if !wantsTLS && !IsPubliclyRoutable(h) {
			out[i] = "http://" + h
		} else {
			out[i] = h
		}
	}
	return out
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
	// commitGuardPrefix, when set, composes a fence guard (the APP lock's,
	// state.Lock.GuardPrefix) into the same shell command as the Caddyfile
	// commit rename (C01-2): a deploy whose app lock was broken mid-mutate
	// has its route edit refused (exit 75) instead of landing inside the
	// new owner's window. Empty = unguarded commits (the receiver shared
	// by callers that hold no app fence). Set through WithCommitGuard.
	commitGuardPrefix string
}

// NewClient creates a Caddy client backed by the given SSH executor.
func NewClient(exec ssh.Executor) *Client {
	return &Client{exec: exec}
}

// WithCommitGuard returns a client whose Caddyfile commits run composed
// under the given fence-guard prefix in the same shell as the commit
// rename (C01-2). Callers that hold an app-level fence (deploy, rollback,
// static) pass their lock's GuardPrefix so the traffic switch — the
// finding's third check-then-act site — is guard+effect in one command.
// The receiver is unchanged: clients without a guard keep today's
// behavior.
func (c *Client) WithCommitGuard(guardPrefix string) *Client {
	cp := *c
	cp.commitGuardPrefix = guardPrefix
	return &cp
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
func (c *Client) SetRoute(ctx context.Context, app, domain, upstream string, containerPort int, tls TLS, caddyExtra string, cache map[string]string, fw Firewall, access Access) error {
	hosts, err := parseDomains(domain)
	if err != nil {
		return err
	}
	if len(hosts) == 0 {
		return fmt.Errorf("SetRoute: domain must be non-empty")
	}
	return c.applyManagedBlock(ctx, app, hosts, reverseProxyBlock(hosts, upstream, containerPort, tls, caddyExtra, cache, fw, access))
}

// SetLoadBalancer adds or updates a load-balanced reverse proxy route: traffic
// for the domain is distributed across upstreams via round-robin with active
// health checks on healthPath (empty = "/health", the same default the
// deploy-time readiness probe uses — the block used to hardcode /up, so a
// successfully deployed app with no /up route had every upstream marked
// unhealthy; audit F47). Replaces any prior route block for the same app.
func (c *Client) SetLoadBalancer(ctx context.Context, app, domain string, upstreams []Upstream, tls TLS, caddyExtra string, cache map[string]string, fw Firewall, access Access) error {
	return c.SetLoadBalancerHealth(ctx, app, domain, upstreams, "", tls, caddyExtra, cache, fw, access)
}

// validHealthURI constrains an active-check path before it is rendered
// into a Caddyfile: it must be a request-path-shaped URI with no
// whitespace or control characters, so it cannot break out of the
// health_uri directive (TCL-20).
func validHealthURI(p string) bool {
	if !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") ||
		strings.ContainsAny(p, " \t\r\n\x00{}#\"\\") {
		return false
	}
	if _, err := url.ParseRequestURI(p); err != nil {
		return false
	}
	return true
}

// SetLoadBalancerHealth is SetLoadBalancer with an explicit active-check
// path. Pass the app's configured health path so Caddy's upstream checks
// probe the same endpoint the deploy readiness gate used.
func (c *Client) SetLoadBalancerHealth(ctx context.Context, app, domain string, upstreams []Upstream, healthPath string, tls TLS, caddyExtra string, cache map[string]string, fw Firewall, access Access) error {
	hosts, err := parseDomains(domain)
	if err != nil {
		return err
	}
	if len(hosts) == 0 {
		return fmt.Errorf("SetLoadBalancer: domain must be non-empty")
	}
	if healthPath != "" && !validHealthURI(healthPath) {
		return fmt.Errorf("invalid health path %q for %s", healthPath, app)
	}
	return c.applyManagedBlock(ctx, app, hosts, loadBalancerBlock(hosts, upstreams, healthPath, tls, caddyExtra, cache, fw, access))
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

// HasCaddyfile reports whether the server has a Teploy-managed Caddyfile at
// all. Servers running only ingress:host apps never get one — callers use
// this to skip route edits instead of failing on the missing file.
func (c *Client) HasCaddyfile(ctx context.Context) bool {
	_, err := c.exec.Run(ctx, "[ -f "+caddyfilePath+" ]")
	return err == nil
}

// AppendRedirect replaces an app's route with a plain, UNMANAGED permanent
// redirect for its domains. Used by `teploy remove --redirect`: the app is
// gone, so the block deliberately carries no TEPLOY markers — future deploys
// and removals will never rewrite or delete it. The app's managed block and
// any other block already serving these hosts are dropped in the same atomic
// edit, so the redirect can't be shadowed.
func (c *Client) AppendRedirect(ctx context.Context, app string, hosts []string, target string) error {
	if len(hosts) == 0 {
		return fmt.Errorf("no domains recorded for app %q — cannot write a redirect", app)
	}
	return c.mutate(ctx, func(prev string) (string, error) {
		updated, err := renderUpdated(prev, app, hosts, "")
		if err != nil {
			return "", err
		}
		block := strings.Join(hosts, ", ") + " {\n\tredir " + target + " permanent\n}"
		return strings.TrimRight(updated, "\n") + "\n\n" + block + "\n", nil
	})
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
// restored by RemoveMaintenance without a redeploy. The current block's TLS
// directive and access gate are carried into the maintenance block (F48):
// enabling maintenance used to silently drop HTTPS termination and auth —
// the site downgraded to ACME/default and went PUBLIC for the duration.
// The policy is lifted from the PARSED current block; a block that cannot
// be parsed fails the maintenance toggle rather than guessing.
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
		var pol SitePolicy
		if cur := extractCaddyfileBlock(prev, begin, end); cur != "" {
			extracted, err := ExtractPolicy(cur)
			if err != nil {
				return "", fmt.Errorf("reading %s's TLS/access policy for maintenance (route left unchanged): %w", app, err)
			}
			pol = extracted
			stash := fmt.Sprintf(maintStashFmt, app)
			// Never overwrite an existing stash (TCL-25): a SECOND
			// maintenance-on extracts the app's CURRENT block — which by
			// then IS the maintenance block — so stash-on overwrote the
			// original route and maintenance-off restored maintenance
			// forever. The first stash wins; it is deleted only by a
			// successful RemoveMaintenance. Existence is CONFIRMED with a
			// framed read (T62): `test -f` treated a transport failure as
			// "missing" and overwrote a stash that might exist.
			_, stashed, err := readServerFile(ctx, c.exec, stash)
			if err != nil {
				return "", fmt.Errorf("checking the maintenance stash for %s: %w", app, err)
			}
			if !stashed {
				if err := c.exec.Upload(ctx, strings.NewReader(cur), stash, "0644"); err != nil {
					return "", fmt.Errorf("stashing route for maintenance: %w", err)
				}
			}
		}
		updated, err := renderUpdated(prev, app, hosts, maintenanceBlock(hosts, pol))
		if err != nil {
			return "", err
		}
		// Webhooks stay reachable THROUGH maintenance (the listener is
		// HMAC-authenticated and a deploy is how maintenance ends); the
		// persisted fragment is re-applied to the maintenance block under
		// the same transaction (webhook.go, audit T26).
		return c.applyWebhookToBlock(ctx, app, updated)
	})
}

// RemoveMaintenance disables maintenance mode, restoring the stashed route
// block. The stash is read INSIDE the mutation transaction (audit T62):
// the old shape read it before taking the Caddy lock and deleted it after,
// so a concurrent maintenance-on between the two could overwrite the stash
// the read had just captured, or the deletion could remove a stash a
// concurrent operation had just written. A missing stash is a no-op, and a
// stash that exists but can't be read (or is empty) aborts WITHOUT
// touching the route.
func (c *Client) RemoveMaintenance(ctx context.Context, app string) error {
	stash := fmt.Sprintf(maintStashFmt, app)

	stashRemoved := false
	if err := c.mutate(ctx, func(prev string) (string, error) {
		data, present, err := readServerFile(ctx, c.exec, stash)
		if err != nil {
			return "", fmt.Errorf("reading stashed maintenance route (route left unchanged): %w", err)
		}
		if !present {
			// Maintenance isn't active (or was already removed). No-op:
			// returning prev unchanged skips the write/reload entirely.
			return prev, nil
		}
		restored := strings.Trim(string(data), "\n")
		if restored == "" {
			return "", fmt.Errorf("stashed maintenance route for %s is empty — refusing to remove the route; delete %s manually if this is intended", app, stash)
		}
		updated, err := renderUpdated(prev, app, nil, restored)
		if err != nil {
			return "", err
		}
		// The stash may predate a webhook port change; normalize the
		// fragment against the CURRENT persisted descriptor (webhook.go).
		updated, err = c.applyWebhookToBlock(ctx, app, updated)
		if err != nil {
			return "", err
		}
		stashRemoved = true
		return updated, nil
	}); err != nil {
		return err
	}

	// Delete the stash only when the reload succeeded (a failed/rolled-back
	// reload can be retried), and only when this transaction actually
	// restored one — deleting a stash a concurrent maintenance-on wrote
	// would make THAT maintenance unexitable (audit T62).
	if stashRemoved {
		c.exec.Run(ctx, "rm -f -- "+ssh.ShellQuote(stash))
	}
	return nil
}

// mutate serializes a Caddyfile edit + reload behind the server lock. transform
// receives the current Caddyfile and returns the new contents. If the reload
// fails (e.g. the new config is invalid), the on-disk file is rolled back so
// Caddy never persists a config it can't boot from. The COMMIT (the rename
// that makes the new contents authoritative) runs composed under the client's
// fence guard when one is set (C01-2): a stale lock holder's edit is refused
// instead of interleaving; the rollback restore is deliberately never fenced
// (refusing to clean up one's own partial effects is how a fencing design
// strands an app).
func (c *Client) mutate(ctx context.Context, transform func(prev string) (string, error)) error {
	lockOwner, err := c.acquireLock(ctx)
	if err != nil {
		return err
	}
	defer c.releaseLock(ctx, lockOwner)

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

	// Pre-write validation gate (F48/F49): the transformed Caddyfile must
	// adapt under the SERVER's own caddy binary (docker exec, stdin) before
	// it is written — structured edits that break the file are refused
	// HERE, with the authoritative binary, instead of being discovered by
	// the reload below. The LOCAL caddy (adapt.go) is deliberately NOT the
	// gate: its version/modules can differ from the server's, and refusing
	// a legitimate caddy_extra directive (e.g. a custom-build module) on
	// binary drift would break deploys that work today.
	if err := c.adaptCheck(ctx, updated); err != nil {
		return fmt.Errorf("refusing to write a Caddyfile the server's caddy rejects: %w", err)
	}

	// Stage (inert) then commit under the fence guards in one shell — the
	// C01-2/C01-3 composition: the APP fence (when the caller holds one)
	// and the CADDY-LOCK fence (this mutation's own lock, owner-tagged)
	// both precede the rename, so a broken app holder's route edit AND a
	// stale-broken editor's late write are refused in-shell. A refused
	// commit propagates the fence error; the staged file is cleaned up
	// here (bounded).
	tmp, err := c.stageCaddyfile(ctx, updated)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			c.exec.Run(cleanupCtx, "rm -f -- "+ssh.ShellQuote(tmp))
			cancel()
		}
	}()
	if err := c.commitCaddyfile(ctx, tmp, c.commitGuardPrefix+caddyGuardFragment(lockOwner)); err != nil {
		return err
	}
	committed = true
	if err := c.reload(ctx); err != nil {
		// Roll back so a bad config is never left on disk to break the next
		// boot. The restore runs on a DETACHED bounded context — the deploy
		// context may be cancelled (the reason the reload failed), and a
		// restore that silently no-ops left the bad config live (audit F45).
		restoreCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if rbErr := c.writeCaddyfile(restoreCtx, prev); rbErr != nil {
			cancel()
			return fmt.Errorf("caddy reload failed (%w) AND restoring the previous Caddyfile failed (%v) — the on-disk config may break the next caddy boot; resolve manually at %s", err, rbErr, caddyfilePath)
		}
		if rbErr := c.reload(restoreCtx); rbErr != nil {
			cancel()
			return fmt.Errorf("caddy reload failed (%w); the previous Caddyfile was restored on disk but the reload-back failed (%v) — the running config may be stale until the next successful reload", err, rbErr)
		}
		cancel()
		return fmt.Errorf("caddy reload failed, rolled back: %w", err)
	}
	// Confirm the running container actually sees the config we just wrote.
	// A legacy single-file Caddyfile bind mount pins the container to a stale
	// inode, so `reload` can "succeed" against an old file while serving stale
	// routes — a silent 502 once the old app container stops. Fail loudly here
	// (the deploy aborts before the old container is torn down) instead, and
	// treat a delivery-verification failure like a reload failure: restore the
	// previous Caddyfile so the on-disk file matches what the container is
	// actually serving, rather than leaving disk and runtime disagreeing
	// (TCL-21).
	if err := c.verifyDelivered(ctx); err != nil {
		restoreCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if rbErr := c.writeCaddyfile(restoreCtx, prev); rbErr != nil {
			cancel()
			return fmt.Errorf("caddy delivery verification failed (%w) AND restoring the previous Caddyfile failed (%v) — the on-disk config may not match what the container serves; resolve manually at %s", err, rbErr, caddyfilePath)
		}
		if rbErr := c.reload(restoreCtx); rbErr != nil {
			cancel()
			return fmt.Errorf("caddy delivery verification failed (%w); the previous Caddyfile was restored on disk but the reload-back failed (%v) — the running config may be stale until the next successful reload", err, rbErr)
		}
		cancel()
		return fmt.Errorf("caddy delivery verification failed, rolled back: %w", err)
	}
	return nil
}

// singleFileMount reports whether the caddy container binds the Caddyfile as a
// single file (mount Destination == containerCaddyfile) rather than mounting
// its directory (/etc/caddy). A single-file bind mount pins the container to
// the file's inode at create time, so an atomic rename (tmp + mv) swaps in a
// verifyDelivered checks that the caddy container's view of the Caddyfile
// matches the file Teploy wrote on the host, comparing checksums on each side
// in a single command. A mismatch means the write never reached the running
// container — in practice a legacy single-file bind mount pinning a stale
// inode. A failure to run the check at all is not treated as fatal (older shell
// etc.); this is a safety net, not the primary path.
func (c *Client) verifyDelivered(ctx context.Context) error {
	// Both checksums are captured and required NON-EMPTY before comparison
	// (TCL-21): the old `[ "$(…)" = "$(…)" ]` shape let two failing
	// substitutions compare equal as empty strings and report delivery.
	check := fmt.Sprintf(
		"a=$(docker exec %s md5sum %s 2>/dev/null | cut -d' ' -f1); "+
			"b=$(md5sum %s 2>/dev/null | cut -d' ' -f1); "+
			"[ -n \"$a\" ] && [ \"$a\" = \"$b\" ] && echo %s || echo %s",
		caddyContainer, containerCaddyfile, caddyfilePath, deliveredOK, deliveredStale)
	out, err := c.exec.Run(ctx, check)
	// The check itself ends in `|| echo STALE`, so a non-nil error here is a
	// transport/execution failure. Both are treated as NOT delivered —
	// fail closed (audit F45).
	if err != nil {
		return fmt.Errorf("could not verify the caddy container is serving the config just written: %w", err)
	}
	if out != strings.TrimSpace(deliveredOK) && !strings.HasPrefix(out, deliveredOK) {
		return fmt.Errorf(
			"caddy reloaded but the container's %s does not match the config Teploy wrote — "+
				"the write did not reach the running container (legacy single-file Caddyfile bind mount "+
				"pinning a stale inode). The deploy was aborted before stopping the old container, so the "+
				"site keeps serving. Fix: recreate the caddy container with the directory mount "+
				"`-v /deployments/caddy:/etc/caddy` (keep its other volumes/networks); the on-disk "+
				"Caddyfile is preserved as-is, so no routes are lost",
			containerCaddyfile)
	}
	return nil
}

// adaptCheck runs the server's own caddy adapt on the proposed Caddyfile
// (streamed via stdin, never written) — the pre-write validation gate. A
// non-zero exit refuses the edit before anything changes on disk.
func (c *Client) adaptCheck(ctx context.Context, content string) error {
	err := c.exec.RunInput(ctx, fmt.Sprintf("docker exec -i %s caddy adapt --config - --adapter caddyfile", caddyContainer), strings.NewReader(content))
	if err != nil {
		return fmt.Errorf("caddy adapt: %w", err)
	}
	return nil
}

// applyManagedBlock upserts (block != "") or removes (block == "") the app's
// marker-delimited block, adopting any foreign block for the same hosts.
// The app's persisted webhook fragment (webhook.go) is re-applied to the
// rendered block, so deploys and rollbacks can no longer erase the webhook
// route the way they erased the old runtime-API injection (audit T26).
func (c *Client) applyManagedBlock(ctx context.Context, app string, hosts []string, block string) error {
	return c.mutate(ctx, func(prev string) (string, error) {
		updated, err := renderUpdated(prev, app, hosts, block)
		if err != nil {
			return "", err
		}
		return c.applyWebhookToBlock(ctx, app, updated)
	})
}

// renderUpdated produces new Caddyfile contents: it removes the app's previous
// Teploy block (and a legacy lb-<app> block it can prove is legacy), removes
// any non-Teploy block serving the same hosts (brownfield adoption), then
// appends the new block wrapped in per-app markers. An empty block just
// performs the removals. Marker matching is EXACT-LINE, so an app whose name
// is a prefix of another's can no longer rewrite the other's block (audit F44).
func renderUpdated(prev, app string, hosts []string, block string) (string, error) {
	updated, err := removeCaddyfileBlock(prev, fmt.Sprintf(markerBeginFmt, app), fmt.Sprintf(markerEndFmt, app))
	if err != nil {
		return "", err
	}
	// Legacy: older versions used a separate lb-<app> marker block for
	// multi-replica apps. Removing it unconditionally collided with a REAL
	// application legitimately named lb-<app> (a valid app name) — updating
	// or removing <app> deleted that other app's route (TCL-23). The legacy
	// block is removed only when its own site address proves it serves the
	// hosts this operation manages; anything else belongs to someone else.
	if legacyLBHosts := managedBlockHosts(prev, "lb-"+app); len(legacyLBHosts) > 0 {
		refHosts := hosts
		if len(refHosts) == 0 {
			// Removal path (no hosts given): compare against the hosts of
			// the app's own current block in prev — the legacy block served
			// the same app, so its hosts match the app's block, not another
			// app's.
			refHosts = managedBlockHosts(prev, app)
		}
		if addressWithinHosts(strings.Join(legacyLBHosts, ", "), refHosts) {
			updated, err = removeCaddyfileBlock(updated, fmt.Sprintf(markerBeginFmt, "lb-"+app), fmt.Sprintf(markerEndFmt, "lb-"+app))
			if err != nil {
				return "", err
			}
		}
	}
	if len(hosts) > 0 {
		updated, err = adoptForeignBlocks(updated, hosts)
		if err != nil {
			return "", err
		}
	}

	if block != "" {
		begin := fmt.Sprintf(markerBeginFmt, app)
		end := fmt.Sprintf(markerEndFmt, app)
		wrapped := begin + "\n" + strings.TrimRight(block, "\n") + "\n" + end
		return strings.TrimRight(updated, "\n") + "\n\n" + wrapped + "\n", nil
	}
	return strings.TrimRight(updated, "\n") + "\n", nil
}

// managedBlockHosts returns the site-address hosts of the app's managed
// block (its first non-empty line), or nil when the app has no managed
// block. Used to prove whether a legacy lb-<app> block belongs to this app
// rather than to a distinct application of the same name (TCL-23).
func managedBlockHosts(content, app string) []string {
	block := extractCaddyfileBlock(content, fmt.Sprintf(markerBeginFmt, app), fmt.Sprintf(markerEndFmt, app))
	for _, line := range strings.Split(block, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		addr := strings.TrimSpace(strings.TrimSuffix(line, "{"))
		var hosts []string
		for _, a := range strings.Split(addr, ",") {
			if a = strings.TrimSpace(a); a != "" {
				hosts = append(hosts, a)
			}
		}
		return hosts
	}
	return nil
}

// writeCaddyfile persists the Caddyfile atomically via a temp file + rename. The
// caddy container is mounted with the directory (-v /deployments/caddy:/etc/caddy,
// see cli/setup.go), so it resolves the file by path on each reload and picks up
// the swapped-in inode.
//
// Teploy deliberately does NOT special-case the legacy single-file Caddyfile
// bind mount here. That mount pins the container to one inode (so atomic renames
// never reach it) AND can't expose /etc/caddy/tls for custom certs — it's an
// anti-pattern new setups never create. Writing around the pin would only let a
// broken box limp along; the correct fix is to recreate caddy with the directory
// mount (-v /deployments/caddy:/etc/caddy). verifyDelivered catches an
// un-migrated box and aborts the deploy loudly before any damage.
func (c *Client) writeCaddyfile(ctx context.Context, content string) error {
	tmp, err := c.stageCaddyfile(ctx, content)
	if err != nil {
		return err
	}
	if err := c.commitCaddyfile(ctx, tmp, ""); err != nil {
		// The staged file is inert; leave nothing behind (bounded).
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		c.exec.Run(cleanupCtx, "rm -f -- "+ssh.ShellQuote(tmp))
		cancel()
		return err
	}
	return nil
}

// stageCaddyfile uploads content to a random inert SIBLING of the
// Caddyfile (F46's discipline: random sibling, same filesystem, no shared
// staging name). Staging has no effect: a file nothing commits is dead
// weight. The commit instant is commitCaddyfile's rename.
func (c *Client) stageCaddyfile(ctx context.Context, content string) (string, error) {
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", fmt.Errorf("generating staging path: %w", err)
	}
	tmpPath := caddyfilePath + ".tmp-" + hex.EncodeToString(suffix[:])
	if err := c.exec.Upload(ctx, strings.NewReader(content), tmpPath, "0644"); err != nil {
		return "", fmt.Errorf("staging caddyfile: %w", err)
	}
	return tmpPath, nil
}

// commitCaddyfile renames the staged file into place — the instant the
// new config becomes authoritative on disk. guardPrefix (when set)
// composes the holdership check into the SAME shell command (C01-2):
// between a separate check and this rename a lock takeover could occur,
// letting a broken holder's route edit land inside the new owner's
// window; under composition the stale holder's edit is refused (exit 75,
// the TEPLOY_FENCE_LOST marker) and the staged bytes never become
// authoritative. A refusal is identified and rewrapped so callers can
// match it (state.FenceLost over the error string, or the message).
func (c *Client) commitCaddyfile(ctx context.Context, tmpPath, guardPrefix string) error {
	cmd := guardPrefix + "mv -fT -- " + ssh.ShellQuote(tmpPath) + " " + ssh.ShellQuote(caddyfilePath)
	_, err := c.exec.Run(ctx, cmd)
	if err != nil && guardPrefix != "" && fenceRefused(err) {
		return fmt.Errorf("caddyfile commit refused — a lock fence was lost (app lock or caddy lock no longer names this operation), the route edit did not land: %w", err)
	}
	if err != nil {
		return fmt.Errorf("committing caddyfile: %w", err)
	}
	return nil
}

// fenceRefused reports whether a commit-command error is the composed
// guard refusing the effect (marker on stderr or exit status 75) rather
// than the rename itself failing.
func fenceRefused(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "TEPLOY_FENCE_LOST") ||
		strings.Contains(msg, "status 75") ||
		strings.Contains(msg, "exit status 75")
}

func (c *Client) reload(ctx context.Context) error {
	if _, err := c.exec.Run(ctx, reloadCmd); err != nil {
		return err
	}
	return nil
}

// caddyLockInfo is the shared-proxy commit lock's identity file (C01-3).
// Pre-C01-3 the lock was a bare mkdir with no owner: any mutator broke it
// after staleLockSeconds by DIRECTORY MTIME and a slow-but-alive orphaned
// editor could interleave its Caddyfile edit+reload with the new owner's
// mutate. The shape mirrors the app locks (state.LockInfo): an owner
// token, and staleness measured from the info's own timestamp. The TTL
// stays short (edits are seconds); there is no renewal — an edit session
// is far shorter than any plausible renewal interval.
type caddyLockInfo struct {
	Type  string `json:"type"`
	Owner string `json:"owner"`
	TS    string `json:"ts"`
}

// newCaddyOwner mints the lock's fencing token.
func newCaddyOwner() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is catastrophic-environment territory; a
		// time-derived token still unique-ifies this process's edits.
		return fmt.Sprintf("caddy-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// acquireLock takes the shared-proxy commit lock (mkdir + owner-tagged
// info), breaking a STALE holder's lock first. Staleness is measured from
// the info file's timestamp when one exists (C01-3); a legacy lock dir
// with no info falls back to the directory-mtime age check. The returned
// owner token fences the commit: a holder whose lock was broken (or whose
// info no longer names it) has its Caddyfile commit refused by the
// composed guard instead of interleaving with the new owner's edit.
func (c *Client) acquireLock(ctx context.Context) (string, error) {
	owner := newCaddyOwner()
	for i := 0; i < lockWaitTries; i++ {
		if _, err := c.exec.Run(ctx, "mkdir "+lockDir); err == nil {
			if err := c.writeCaddyLockInfo(ctx, owner); err != nil {
				// Our own fresh lock with no successor possible: an
				// unconditional release is correct here.
				c.exec.Run(ctx, "rm -rf "+lockDir)
				return "", err
			}
			return owner, nil
		}
		if c.caddyLockStale(ctx) {
			c.exec.Run(ctx, "rm -rf "+lockDir)
			continue
		}
		c.exec.Run(ctx, "sleep 0.5")
	}
	return "", fmt.Errorf("timed out acquiring caddy lock %s", lockDir)
}

// caddyLockStale reports whether the existing lock may be broken: an
// info-carrying lock (C01-3) is stale when its OWN timestamp is older
// than staleLockSeconds (unparseable timestamp = stale — the app locks'
// rule); a legacy no-info dir is stale by directory mtime, the pre-C01-3
// behavior.
func (c *Client) caddyLockStale(ctx context.Context) bool {
	if out, err := c.exec.Run(ctx, "cat "+lockDir+"/info 2>/dev/null"); err == nil && strings.TrimSpace(out) != "" {
		var info caddyLockInfo
		if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &info); err != nil {
			return true
		}
		ts, err := time.Parse(time.RFC3339, info.TS)
		if err != nil {
			return true
		}
		return time.Since(ts) > staleLockSeconds*time.Second
	}
	// Legacy dir (no info): the old mtime-based break.
	out, err := c.exec.Run(ctx, fmt.Sprintf(
		"[ -d %s ] && [ $(( $(date +%%s) - $(stat -c %%Y %s 2>/dev/null || echo 0) )) -gt %d ] && echo stale || echo fresh",
		lockDir, lockDir, staleLockSeconds,
	))
	return err == nil && strings.TrimSpace(out) == "stale"
}

func (c *Client) writeCaddyLockInfo(ctx context.Context, owner string) error {
	info, err := json.Marshal(caddyLockInfo{
		Type:  "caddy-edit",
		Owner: owner,
		TS:    time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return err
	}
	if err := c.exec.Upload(ctx, strings.NewReader(string(info)), lockDir+"/info", "0644"); err != nil {
		return fmt.Errorf("writing caddy lock info: %w", err)
	}
	return nil
}

// caddyGuardFragment is the caddy lock's holdership guard — the same
// shape as the app lock's (state.Lock.guardFragment) so transport-level
// refusal handling and the mock executor treat both identically.
func caddyGuardFragment(owner string) string {
	return fmt.Sprintf("grep -q %s %s || { printf '%s\\n' >&2; exit 75; }; ",
		ssh.ShellQuote(owner), ssh.ShellQuote(lockDir+"/info"), fenceLostMarker)
}

// releaseLock removes the caddy lock ONLY when its info still names this
// owner (C01-3, the app locks' A04 lesson): after a stale break and
// re-acquire, an unconditional rmdir would delete the SUCCESSOR's lock
// and admit a third editor. A lock naming someone else is left strictly
// alone; a detached bounded context keeps the release alive past caller
// cancellation (TCL-05).
func (c *Client) releaseLock(ctx context.Context, owner string) {
	rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c.exec.Run(rctx, fmt.Sprintf(
		"if [ -d %s ] && grep -q %s %s 2>/dev/null; then rm -rf -- %s; fi",
		ssh.ShellQuote(lockDir), ssh.ShellQuote(owner), ssh.ShellQuote(lockDir+"/info"), ssh.ShellQuote(lockDir),
	))
}

// removeForeignHostBlocks' whole-block rule moved to routes.go's
// adoptForeignBlocks (F49): adoption is decided on the PARSED structure, and
// a foreign block sharing only some of the requested hosts has those hosts
// removed from its address line instead of being left behind to fail at
// reload.

// addressWithinHosts reports whether EVERY host in a Caddyfile site-address
// (e.g. "example.com, www.example.com") is among the given hosts — the
// condition under which a foreign block may be adopted wholesale.
func addressWithinHosts(addr string, hosts []string) bool {
	seen := 0
	for _, a := range strings.Split(addr, ",") {
		a = strings.TrimSpace(a)
		a = strings.TrimPrefix(a, "https://")
		a = strings.TrimPrefix(a, "http://")
		if sp := strings.IndexAny(a, " \t"); sp >= 0 {
			a = a[:sp]
		}
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

func isSpaceByte(b byte) bool { return b == ' ' || b == '\t' }

// markerName constrains the app-name portion of a TEPLOY marker line: a
// complete marker is exactly "# TEPLOY BEGIN <app>" (nothing else on the
// line). Exact-line matching is what prevents app "web" from matching the
// marker of "web-staging" — the old substring search rewrote the wrong
// app's block whenever one app name was a prefix of another (audit F44).
var markerName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*[a-z0-9]$|^[a-z0-9]$`)

// beginMarkerApp returns the app named by a complete "# TEPLOY BEGIN <app>"
// line, or ok=false for anything else (including a longer line that merely
// starts with a prefix of the marker).
func beginMarkerApp(line string) (string, bool) {
	t := strings.TrimSpace(strings.TrimSuffix(line, "\r"))
	if !strings.HasPrefix(t, markerBeginPrefix) {
		return "", false
	}
	name := strings.TrimSpace(strings.TrimPrefix(t, markerBeginPrefix))
	if !markerName.MatchString(name) {
		return "", false
	}
	return name, true
}

// endMarkerApp is beginMarkerApp for "# TEPLOY END <app>" lines.
func endMarkerApp(line string) (string, bool) {
	t := strings.TrimSpace(strings.TrimSuffix(line, "\r"))
	if !strings.HasPrefix(t, markerEndPrefix) {
		return "", false
	}
	name := strings.TrimSpace(strings.TrimPrefix(t, markerEndPrefix))
	if !markerName.MatchString(name) {
		return "", false
	}
	return name, true
}

const (
	markerBeginPrefix = "# TEPLOY BEGIN "
	markerEndPrefix   = "# TEPLOY END "
)

// extractCaddyfileBlock returns the content between the app's exact begin
// and end marker lines (markers excluded), or "" if not found. Marker lines
// must match COMPLETELY — app "web" no longer matches inside
// "# TEPLOY BEGIN web-staging" (audit F44).
func extractCaddyfileBlock(content, begin, end string) string {
	target := strings.TrimSpace(strings.TrimPrefix(begin, markerBeginPrefix))
	var out []string
	active := false
	for _, line := range strings.Split(content, "\n") {
		if name, ok := beginMarkerApp(line); ok {
			if name == target {
				active = true
			}
			continue
		}
		if name, ok := endMarkerApp(line); ok {
			if active && name == target {
				return strings.Trim(strings.Join(out, "\n"), "\n")
			}
			continue
		}
		if active {
			out = append(out, line)
		}
	}
	return ""
}

// removeCaddyfileBlock removes the region bounded by the app's EXACT marker
// lines, collapsing surrounding blank lines so repeated upserts don't grow
// stray whitespace. An unterminated region trims to end-of-file (the
// app's own damaged region only — exact matching guarantees the begin
// marker named this app, not a longer one). Every end marker for a region
// that is not the target's is an error: silently accepting unpaired
// markers is how unrelated bytes used to get dropped (audit F44).
func removeCaddyfileBlock(content, begin, end string) (string, error) {
	target := strings.TrimSpace(strings.TrimPrefix(begin, markerBeginPrefix))
	if !markerName.MatchString(target) {
		return "", fmt.Errorf("invalid managed-block app name %q", target)
	}
	var out []string
	active := false
	removed := false
	for _, line := range strings.Split(content, "\n") {
		if name, ok := beginMarkerApp(line); ok {
			if active {
				return "", fmt.Errorf("nested TEPLOY BEGIN %q inside the %s block — refusing to edit a malformed Caddyfile", name, target)
			}
			if name == target {
				if removed {
					return "", fmt.Errorf("duplicate TEPLOY block for %s — refusing to edit a malformed Caddyfile", target)
				}
				active = true
				removed = true
				continue
			}
			out = append(out, line)
			continue
		}
		if name, ok := endMarkerApp(line); ok {
			if active && name == target {
				active = false
				continue
			}
			if !active && name == target {
				return "", fmt.Errorf("unmatched TEPLOY END %s — refusing to edit a malformed Caddyfile", target)
			}
			out = append(out, line)
			continue
		}
		if !active {
			out = append(out, line)
		}
	}
	if active {
		// Unterminated region: refuse the edit entirely (TCL-22). The old
		// behavior trimmed from the begin marker through end-of-file, which
		// a damaged or hand-edited marker turned into deletion of every
		// unrelated unmanaged route below it. Malformed ownership
		// boundaries fail closed before any rewrite.
		return "", fmt.Errorf("unterminated TEPLOY BEGIN %s (no matching END marker) — refusing to edit a malformed Caddyfile", target)
	}
	result := strings.Join(out, "\n")
	if removed {
		// Collapse runs of blank lines the removal may have created.
		for strings.Contains(result, "\n\n\n") {
			result = strings.ReplaceAll(result, "\n\n\n", "\n\n")
		}
		result = strings.TrimRight(result, "\n") + "\n"
		if result == "\n" {
			result = ""
		}
	}
	return result, nil
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
// renderCacheRules emits the `cache:` path -> Cache-Control rules from app
// config as Caddy matchers.
//
// Sorted by pattern so the rendered block is byte-stable. Go randomises map
// iteration order, so the previous inline loop produced a different Caddyfile on
// every run — which made the managed block churn between deploys and any diff of
// it meaningless.
func renderCacheRules(cache map[string]string) string {
	if len(cache) == 0 {
		return ""
	}
	patterns := make([]string, 0, len(cache))
	for pattern := range cache {
		patterns = append(patterns, pattern)
	}
	sort.Strings(patterns)

	var b strings.Builder
	for i, pattern := range patterns {
		matcher := fmt.Sprintf("@cache%d", i+1)
		b.WriteString(fmt.Sprintf("\t%s path %s\n", matcher, pattern))
		b.WriteString(fmt.Sprintf("\theader %s Cache-Control %q\n", matcher, cache[pattern]))
	}
	return b.String()
}

func reverseProxyBlock(hosts []string, upstream string, port int, tls TLS, caddyExtra string, cache map[string]string, fw Firewall, access Access) string {
	var b strings.Builder
	b.WriteString(strings.Join(siteAddresses(hosts, tls), ", "))
	b.WriteString(" {\n")
	b.WriteString(tls.directive())
	b.WriteString(access.render())
	b.WriteString(fw.wrapTerminal(fmt.Sprintf("\treverse_proxy %s:%d\n", upstream, port)))
	b.WriteString(renderCacheRules(cache))
	if extra := strings.TrimSpace(caddyExtra); extra != "" {
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

// loadBalancerBlock renders a round-robin reverse-proxy block with active
// health checks on healthPath (default /health).
func loadBalancerBlock(hosts []string, upstreams []Upstream, healthPath string, tls TLS, caddyExtra string, cache map[string]string, fw Firewall, access Access) string {
	if healthPath == "" {
		healthPath = "/health"
	}
	dials := make([]string, len(upstreams))
	for i, u := range upstreams {
		dials[i] = u.Dial
	}
	var b strings.Builder
	b.WriteString(strings.Join(siteAddresses(hosts, tls), ", "))
	b.WriteString(" {\n")
	b.WriteString(tls.directive())
	b.WriteString(access.render())
	b.WriteString(fw.wrapTerminal(fmt.Sprintf("\treverse_proxy %s {\n\t\tlb_policy round_robin\n\t\thealth_uri %s\n\t\thealth_interval 10s\n\t\thealth_timeout 5s\n\t}\n", strings.Join(dials, " "), healthPath)))
	b.WriteString(renderCacheRules(cache))
	if extra := strings.TrimSpace(caddyExtra); extra != "" {
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

// maintenanceBlock renders a site block that returns a 503 maintenance page
// for the given hosts, preserving the site's TLS directive and access gate
// (F48) — the extracted policy is verbatim, so maintenance holds the same
// security envelope the real route did. A TLS directive present means the
// operator explicitly opted into HTTPS for these hosts, so no http://
// scheme downgrade is applied to non-public addresses (mirrors
// siteAddresses' wantsTLS rule).
func maintenanceBlock(hosts []string, pol SitePolicy) string {
	var tlsLine string
	if t := strings.TrimSpace(pol.TLS); t != "" {
		tlsLine = t + "\n"
	}
	var accessSpan string
	for _, a := range pol.Access {
		accessSpan += a + "\n"
	}
	schemeHosts := hosts
	if tlsLine == "" {
		// Same fallback as a regular route with no TLS: a non-public host
		// gets an explicit http:// scheme so Caddy does not hang on an
		// ACME challenge that can never complete.
		schemeHosts = siteAddresses(hosts, TLS{})
	}
	return fmt.Sprintf(
		"%s {\n%s%s\theader Content-Type \"text/html; charset=utf-8\"\n\theader Retry-After \"3600\"\n\trespond 503 {\n\t\tbody `%s`\n\t}\n}",
		strings.Join(schemeHosts, ", "), tlsLine, accessSpan, maintenancePage,
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
	// No custom-TLS param — type:static rejects tls: at config.validate()
	// time, so zero-value TLS is always correct here: a non-public host
	// falls back to http://.
	b.WriteString(strings.Join(siteAddresses(opts.Hosts, TLS{}), ", "))
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

	b.WriteString(renderCacheRules(opts.Cache))

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
