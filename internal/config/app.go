package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
	"gopkg.in/yaml.v3"
)

// validName matches lowercase alphanumeric names with hyphens (no leading/trailing hyphen).
var validName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*[a-z0-9]$|^[a-z0-9]$`)

// validDomain matches valid domain names and IP-based domains (e.g. 192.168.1.1.nip.io).
var validDomain = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9.-]+[a-zA-Z0-9]$`)

// validPlatform matches Docker platform strings like linux/amd64, linux/arm64, linux/arm/v7.
var validPlatform = regexp.MustCompile(`^[a-z]+/[a-z0-9]+(/v[0-9]+)?$`)

// HooksConfig holds pre/post deploy hook commands.
type HooksConfig struct {
	PreDeploy  string `yaml:"pre_deploy,omitempty" toml:"pre_deploy"`
	PostDeploy string `yaml:"post_deploy,omitempty" toml:"post_deploy"`
}

// RolloutConfig controls staged multi-server deploys (the `rollout:` block).
//
// Canary is the size of the first wave — an integer count ("2") or a percent
// of the fleet ("10%"), minimum 1, deployed serially. Any canary failure
// halts the rollout and rolls the canary back: the rest of the fleet is
// never touched.
//
// MaxFailures is the failure tolerance for the main wave AFTER the canary
// passes. 0 (default): any failure halts and rolls the whole fleet back to
// the old version (no mixed-version fleet). N>0: every server is attempted;
// up to N failures are tolerated — succeeded servers KEEP the new version,
// the load balancer serves only them, and the command exits non-zero with an
// explicit per-server report so stragglers are converged deliberately, never
// silently.
type RolloutConfig struct {
	Canary      string `yaml:"canary,omitempty" toml:"canary"`
	MaxFailures int    `yaml:"max_failures,omitempty" toml:"max_failures"`
}

// CanaryCount resolves the canary spec against the fleet size: integer count
// or "N%" (rounded up, so a nonzero percent of a small fleet is never zero),
// clamped to [1, total-1] — a canary that is the whole fleet is not a canary.
func (r *RolloutConfig) CanaryCount(total int) (int, error) {
	spec := strings.TrimSpace(r.Canary)
	if spec == "" {
		spec = "1"
	}
	var n int
	if pct, ok := strings.CutSuffix(spec, "%"); ok {
		p, err := strconv.Atoi(strings.TrimSpace(pct))
		if err != nil || p <= 0 || p > 100 {
			return 0, fmt.Errorf("rollout.canary: invalid percent %q", r.Canary)
		}
		n = (total*p + 99) / 100
	} else {
		c, err := strconv.Atoi(spec)
		if err != nil || c <= 0 {
			return 0, fmt.Errorf("rollout.canary: invalid count %q", r.Canary)
		}
		n = c
	}
	if n < 1 {
		n = 1
	}
	if n >= total {
		n = total - 1
	}
	if n < 1 {
		n = 1
	}
	return n, nil
}

// AccessoryConfig represents a stateful service container (database, cache, etc.).
type AccessoryConfig struct {
	Image   string            `yaml:"image" toml:"image"`
	Port    int               `yaml:"port,omitempty" toml:"port"`
	Env     map[string]string `yaml:"env,omitempty" toml:"env"`
	Volumes map[string]string `yaml:"volumes,omitempty" toml:"volumes"`
	// Command overrides the image's default command (docker run trailing
	// args) — required by images whose entrypoint needs an explicit verb,
	// e.g. MinIO (`server /data --console-address :9001`) or ntfy (`serve`).
	Command string `yaml:"command,omitempty" toml:"command"`
	// Publish adds docker -p mappings (e.g. "127.0.0.1:9100:9000") for the
	// rare accessory that must be reachable from the HOST, not just the
	// teploy network — e.g. a MinIO backup target, since `teploy backup`
	// runs the aws CLI on the host where container DNS doesn't resolve.
	// Prefer loopback binds; a bare port exposes the accessory publicly.
	Publish []string `yaml:"publish,omitempty" toml:"publish"`
}

// TLSConfig declares a custom certificate for terminating TLS on the app's
// Caddy site block, instead of Caddy's automatic HTTPS (ACME). Use this when
// the public hostname is proxied so ACME challenges can't reach the origin —
// e.g. Cloudflare proxied DNS or a Cloudflare Tunnel, where you terminate
// with a Cloudflare Origin Certificate.
//
// Cert and Key are LOCAL file paths (on the machine running teploy). On deploy
// they are uploaded to the server and referenced from the generated Caddy
// block, so the cert survives every deploy (unlike a hand-edited Caddyfile,
// which the authoritative model overwrites).
type TLSConfig struct {
	Cert string `yaml:"cert,omitempty" toml:"cert"` // local path to PEM certificate (chain)
	Key  string `yaml:"key,omitempty" toml:"key"`   // local path to PEM private key
	// Internal requests Caddy's own local CA (self-signed, `tls internal`)
	// instead of a custom cert or automatic HTTPS. Mutually exclusive with
	// Cert/Key. Meaningful mainly for non-public domains (bare IPs, LAN
	// hostnames) where automatic HTTPS can't complete an ACME challenge —
	// see caddy.isPubliclyRoutable. Not needed for a real public domain,
	// which should just use automatic HTTPS (the default, empty tls:).
	Internal bool `yaml:"internal,omitempty" toml:"internal"`
}

// ProcessHealth overrides the container HEALTHCHECK behavior for a single
// process. Keyed by process name (matching a key in Processes) in the
// AppConfig.Healthcheck map.
//
// Used when a process shouldn't be probed by the image's built-in HEALTHCHECK
// directive — e.g. a worker container that shares its runner image with a web
// container and inherits the web's HTTP healthcheck, which then fails forever
// for the worker because the worker has no HTTP listener.
type ProcessHealth struct {
	// Disable, when true, passes --no-healthcheck to docker run so the
	// container ignores the image HEALTHCHECK directive.
	Disable bool `yaml:"disable,omitempty" toml:"disable"`
}

// AppHealthConfig configures the teploy-level deploy health check: the HTTP
// poll that gates the traffic switch after a deploy. Distinct from the
// container HEALTHCHECK directive (which is per-process, in Healthcheck map).
type AppHealthConfig struct {
	// Path is the URL path polled for a 200 response. Default: "/health".
	Path string `yaml:"path,omitempty" toml:"path"`
	// TimeoutSeconds is the total time to wait for a healthy response before
	// the deploy fails and rolls back. Default: 30. Raise this for
	// slow-starting apps (JVM boot, migrations that run before the app
	// binds its port) instead of them always failing health and rolling back.
	TimeoutSeconds int `yaml:"timeout_seconds,omitempty" toml:"timeout_seconds"`
	// IntervalSeconds is the time between health check polls. Default: 1.
	IntervalSeconds int `yaml:"interval_seconds,omitempty" toml:"interval_seconds"`
}

// NotificationChannelConfig represents a single notification channel.
type NotificationChannelConfig struct {
	Type   string   `yaml:"type,omitempty" toml:"type"`
	URL    string   `yaml:"url,omitempty" toml:"url"`
	To     string   `yaml:"to,omitempty" toml:"to"`
	Events []string `yaml:"events,omitempty" toml:"events"`
}

// NotificationsConfig holds notification settings.
// Supports both simple (single webhook) and multi-channel formats.
type NotificationsConfig struct {
	Webhook  string                      `yaml:"webhook,omitempty" toml:"webhook"`
	Channels []NotificationChannelConfig `yaml:"channels,omitempty" toml:"channels"`
}

// AssetsConfig holds asset bridging settings for zero-downtime static asset serving.
type AssetsConfig struct {
	Path     string `yaml:"path,omitempty" toml:"path"`           // container path, e.g. /app/public/assets
	KeepDays int    `yaml:"keep_days,omitempty" toml:"keep_days"` // cleanup after N days (default 7)
}

// NetworkConfig holds cross-server VPN mesh configuration.
type NetworkConfig struct {
	Provider string `yaml:"provider,omitempty" toml:"provider"`
	AuthKey  string `yaml:"auth_key,omitempty" toml:"auth_key"`
	Server   string `yaml:"server,omitempty" toml:"server"`
	SetupKey string `yaml:"setup_key,omitempty" toml:"setup_key"`
}

// Deploy types. Empty string and "container" both mean container; "static"
// switches to the file-based deploy path (no docker, just rsync + Caddy).
const (
	TypeContainer = "container"
	TypeStatic    = "static"
)

// Ingress modes. Empty string and "caddy" both mean Teploy manages the
// Caddyfile and reloads Caddy on every deploy / rollback / maintenance
// toggle (current behavior). "external" means the user is fronting the
// container with their own ingress (Cloudflare Tunnel, Tailscale Funnel,
// nginx, HAProxy, an AWS ALB, …) and Teploy must not touch Caddy for
// this app. With "external", Teploy still publishes the container port
// on 127.0.0.1 for local health checks and joins the container to the
// teploy network so the external thing can reach it by its app-name
// alias.
//
// "host" publishes the app directly on a fixed host port at the `bind`
// address (default 0.0.0.0), with no Caddy and no proxy in front — handy
// for a private/tailnet box where you reach the app at IP:port. A fixed
// host port can't be blue/green (two containers can't bind it), so host
// ingress deploys by recreate (stop old, start new): a few seconds of
// downtime per deploy in exchange for a stable, directly-reachable port.
const (
	IngressCaddy    = "caddy"
	IngressExternal = "external"
	IngressHost     = "host"
)

// AppConfig represents the teploy configuration (yml, yaml, or toml).
type AppConfig struct {
	App    string `yaml:"app" toml:"app"`
	Domain string `yaml:"domain" toml:"domain"`
	Type   string `yaml:"type,omitempty" toml:"type"`
	// Ingress selects the routing layer. Empty / "caddy" (default) means
	// Teploy manages the Caddyfile. "external" means the user fronts the
	// container with their own ingress (Cloudflare Tunnel, nginx, …) and
	// Teploy must not touch Caddy for this app. See the Ingress* consts.
	Ingress string `yaml:"ingress,omitempty" toml:"ingress"`
	// Bind is the host IP that `ingress: host` publishes the fixed port on
	// (default 0.0.0.0 — all interfaces). Only valid with ingress: host.
	Bind       string   `yaml:"bind,omitempty" toml:"bind"`
	Server     string   `yaml:"server,omitempty" toml:"server"`
	User       string   `yaml:"user,omitempty" toml:"user"`
	Servers    []string `yaml:"servers,omitempty" toml:"servers"`
	Image      string   `yaml:"image,omitempty" toml:"image"`
	Port       int      `yaml:"port,omitempty" toml:"port"`
	Platform   string   `yaml:"platform,omitempty" toml:"platform"`
	BuildLocal bool     `yaml:"build_local,omitempty" toml:"build_local"`
	// Dockerfile names the Dockerfile to build from, resolved relative to
	// Context (default "Dockerfile"). Set it when a monorepo's Dockerfile
	// lives in a subdirectory — e.g. dockerfile: server/monolith/Dockerfile
	// with context: . so the Dockerfile's COPY paths resolve against the
	// repo root. Ignored when Image is set (a pre-built image is pulled,
	// not built) and not valid for type:static.
	Dockerfile string `yaml:"dockerfile,omitempty" toml:"dockerfile"`
	// Context is the Docker build context directory, relative to the
	// teploy.yml location (default "."). Dockerfile is resolved relative to
	// it. Use it to build a subdirectory of a monorepo, or to hand a
	// subdir Dockerfile a wider context (the repo root).
	Context     string `yaml:"context,omitempty" toml:"context"`
	StopTimeout int    `yaml:"stop_timeout,omitempty" toml:"stop_timeout"`
	Parallel    int    `yaml:"parallel,omitempty" toml:"parallel"`
	Replicas    int    `yaml:"replicas,omitempty" toml:"replicas"`
	// Rollout gates multi-server deploys: a canary wave that must succeed
	// before the rest of the fleet deploys, and a bounded failure tolerance
	// for the main wave. Absent (nil) = existing behavior (parallel batches,
	// fail-fast + full-fleet rollback on any failure).
	Rollout *RolloutConfig `yaml:"rollout,omitempty" toml:"rollout"`
	// Scan runs a Trivy vulnerability scan on the image server-side before
	// containers start: HIGH+CRITICAL findings are reported, and fixable
	// CRITICALs block the deploy.
	Scan bool `yaml:"scan,omitempty" toml:"scan"`
	// EnvFiles are local dotenv/YAML files merged into the container env at
	// deploy, resolved relative to teploy.yml. Encrypted files are decrypted
	// on the operator's machine: *.age via the age identity, *.sops.*/*.enc.*
	// via `sops -d` — the GitOps pattern (secrets encrypted in the repo,
	// never plaintext on disk). Later files and explicit env: keys win.
	EnvFiles []string `yaml:"env_files,omitempty" toml:"env_files"`
	// KeepVersions caps the number of past app versions retained after a
	// successful deploy (containers + images). Zero (default) keeps
	// everything — historical behavior. Set to 2 or 3 to enable auto-prune
	// with a rollback window. Container-deploy only; static deploy uses
	// KeepReleases instead.
	KeepVersions int               `yaml:"keep_versions,omitempty" toml:"keep_versions"`
	Hooks        HooksConfig       `yaml:"hooks,omitempty" toml:"hooks"`
	Volumes      map[string]string `yaml:"volumes,omitempty" toml:"volumes"`
	Processes    map[string]string `yaml:"processes,omitempty" toml:"processes"`
	// Env are plain environment variables passed to the app container. Values
	// may reference ${VAR} from the local environment (expanded at deploy
	// time). Secrets set via `teploy secret` override these key-for-key.
	Env map[string]string `yaml:"env,omitempty" toml:"env"`
	// Health configures the teploy-level deploy health check (the HTTP poll
	// that gates traffic switch after a deploy, separate from the container
	// HEALTHCHECK). Path defaults to "/health" when unset.
	Health AppHealthConfig `yaml:"health,omitempty" toml:"health"`
	// Healthcheck holds per-process overrides for the container HEALTHCHECK,
	// keyed by process name. Keys must match a key in Processes. Additive to
	// the scalar `processes:` schema — does not replace it.
	Healthcheck map[string]ProcessHealth   `yaml:"healthcheck,omitempty" toml:"healthcheck"`
	Accessories map[string]AccessoryConfig `yaml:"accessories,omitempty" toml:"accessories"`
	Assets      AssetsConfig               `yaml:"assets,omitempty" toml:"assets"`
	// TLS terminates the app's HTTPS with a custom cert instead of ACME.
	// Required behind Cloudflare proxy / Tunnel (origin cert). Nil = ACME.
	TLS           *TLSConfig          `yaml:"tls,omitempty" toml:"tls"`
	Notifications NotificationsConfig `yaml:"notifications,omitempty" toml:"notifications"`
	Network       NetworkConfig       `yaml:"network,omitempty" toml:"network"`

	// Static deploy fields. Only used when Type == "static".
	Source       string            `yaml:"source,omitempty" toml:"source"`               // local path to built files
	Build        []string          `yaml:"build,omitempty" toml:"build"`                 // pre-rsync shell commands (run locally by default)
	BuildRemote  bool              `yaml:"build_remote,omitempty" toml:"build_remote"`   // run Build commands on the target server instead of locally
	SPA          bool              `yaml:"spa,omitempty" toml:"spa"`                     // enable SPA fallback (try_files)
	SPAFallback  string            `yaml:"spa_fallback,omitempty" toml:"spa_fallback"`   // fallback path (default /index.html)
	Cache        map[string]string `yaml:"cache,omitempty" toml:"cache"`                 // path glob → Cache-Control value
	Headers      map[string]string `yaml:"headers,omitempty" toml:"headers"`             // arbitrary response headers
	KeepReleases int               `yaml:"keep_releases,omitempty" toml:"keep_releases"` // retention count (default 5)
	CaddyExtra   string            `yaml:"caddy_extra,omitempty" toml:"caddy_extra"`     // raw Caddy directives appended into the site block
	Firewall     FirewallConfig    `yaml:"firewall,omitempty" toml:"firewall"`           // edge hardening: IP allow/deny, UA block, body-size cap
	Audit        AuditConfig       `yaml:"audit,omitempty" toml:"audit"`                 // emit deploy/rollback events to teploy-observe
}

// AuditConfig points the CLI at a teploy-observe instance so deploy/rollback/
// scale actions are recorded in the compliance/audit trail. Blank endpoint =
// disabled. The token is an observe editor+ credential.
type AuditConfig struct {
	Endpoint string `yaml:"endpoint,omitempty" toml:"endpoint"`
	Token    string `yaml:"token,omitempty" toml:"token"`
	Site     string `yaml:"site,omitempty" toml:"site"`
}

// FirewallConfig is the per-app Caddy edge hardening (reverse-proxy / load-
// balanced apps only). It is the lightweight, Caddy-native slice: IP allow/deny,
// user-agent blocking, and a request-body size cap — not rate limiting (needs a
// custom Caddy build) or a full WAF (that's Cloudflare's job).
type FirewallConfig struct {
	AllowIPs        []string `yaml:"allow_ips,omitempty" toml:"allow_ips"`                 // if set, ONLY these IPs/CIDRs may connect
	DenyIPs         []string `yaml:"deny_ips,omitempty" toml:"deny_ips"`                   // these IPs/CIDRs are blocked
	BlockUserAgents []string `yaml:"block_user_agents,omitempty" toml:"block_user_agents"` // block requests whose User-Agent contains any of these (case-insensitive)
	MaxBodySize     string   `yaml:"max_body_size,omitempty" toml:"max_body_size"`         // request body cap, e.g. "10MB"
}

// IsZero reports whether no firewall rules are configured.
func (f FirewallConfig) IsZero() bool {
	return len(f.AllowIPs) == 0 && len(f.DenyIPs) == 0 &&
		len(f.BlockUserAgents) == 0 && f.MaxBodySize == ""
}

var validBodySize = regexp.MustCompile(`(?i)^\d+(\.\d+)?\s*(b|kb|mb|gb|tb)?$`)

// validate checks the firewall rules are well-formed and safe to render into a
// Caddyfile. IPs must be a bare address or CIDR; user-agent tokens must not
// contain characters that could break out of the site block.
func (f FirewallConfig) validate() error {
	checkIPs := func(field string, ips []string) error {
		for _, ip := range ips {
			if strings.Contains(ip, "/") {
				if _, _, err := net.ParseCIDR(ip); err != nil {
					return fmt.Errorf("firewall.%s: %q is not a valid CIDR", field, ip)
				}
			} else if net.ParseIP(ip) == nil {
				return fmt.Errorf("firewall.%s: %q is not a valid IP address or CIDR", field, ip)
			}
		}
		return nil
	}
	if err := checkIPs("allow_ips", f.AllowIPs); err != nil {
		return err
	}
	if err := checkIPs("deny_ips", f.DenyIPs); err != nil {
		return err
	}
	for _, ua := range f.BlockUserAgents {
		if ua == "" {
			return fmt.Errorf("firewall.block_user_agents: entries must be non-empty")
		}
		// These would break out of the single-line matcher / site block.
		if strings.ContainsAny(ua, "\r\n{}\"\\") {
			return fmt.Errorf("firewall.block_user_agents: %q contains characters not allowed in a matcher", ua)
		}
	}
	if f.MaxBodySize != "" && !validBodySize.MatchString(strings.TrimSpace(f.MaxBodySize)) {
		return fmt.Errorf("firewall.max_body_size: %q is not a valid size (e.g. 10MB, 1GB, 500KB)", f.MaxBodySize)
	}
	return nil
}

// IsStatic reports whether this app deploys as static files (no container).
func (c *AppConfig) IsStatic() bool {
	return c.Type == TypeStatic
}

// UsesCaddy reports whether Teploy should manage Caddy for this app.
// Default (empty Ingress) is true; only explicit "external" turns it off.
func (c *AppConfig) UsesCaddy() bool {
	return c.Ingress == "" || c.Ingress == IngressCaddy
}

// ValidateName checks that an app name is safe to use as a container name,
// a state-directory path segment, and to interpolate into remote shell
// commands (every such call site quotes the value too — this is defense in
// depth, not the only protection). Exported so callers that build an
// AppConfig directly instead of loading teploy.yml — the ad-hoc `--app`
// deploy path (internal/cli/deploy.go's runAdHocDeploy), and the `--app`
// flag shared by resolveApp (internal/cli/connect.go) — can validate
// before the name reaches the network, since those paths never call
// validate() below.
func ValidateName(name string) error {
	return ValidateIdentifier("app", name)
}

// ValidateIdentifier is the generic form of ValidateName for any other
// value that ends up in the same places an app name does — most notably
// accessory names, which validate() below already constrains to this same
// pattern when they come from teploy.yml's `accessories:` block, but which
// arrive unvalidated as a raw CLI positional arg for `teploy accessory
// stop/start/logs/exec <name>` (internal/cli/accessory.go). label is used
// only to produce an accurate error message (e.g. "accessory name ..." vs
// "'app' ...").
func ValidateIdentifier(label, name string) error {
	if name == "" {
		return fmt.Errorf("'%s' is required", label)
	}
	if !validName.MatchString(name) {
		return fmt.Errorf("'%s' must be lowercase alphanumeric with hyphens (got %q)", label, name)
	}
	if len(name) > 63 {
		return fmt.Errorf("'%s' name too long (max 63 chars, got %d)", label, len(name))
	}
	return nil
}

// isSafeSubPath reports whether p is a relative path that stays inside its
// base — no absolute paths, and no ".." segment that escapes. Used for the
// build 'context'/'dockerfile' fields, which are joined onto a synced
// remote directory and must not reach outside it.
func isSafeSubPath(p string) bool {
	if p == "" || filepath.IsAbs(p) {
		return false
	}
	// filepath.Clean collapses "a/../b" etc.; a leading ".." after cleaning
	// means the path escapes its base.
	clean := filepath.Clean(p)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}

// ValidateDomain checks a (possibly comma-separated multi-host) domain
// value. allowEmpty should be true only for ingress: host, which publishes
// a raw port directly and needs no hostname. Exported for the same reason
// as ValidateName.
func ValidateDomain(domain string, allowEmpty bool) error {
	if domain == "" {
		if allowEmpty {
			return nil
		}
		return fmt.Errorf("'domain' is required")
	}
	// A single comma-separated list is supported so one app can serve
	// multiple hosts (e.g. "example.com, www.example.com"). Each entry is
	// validated independently against the single-host regex.
	for _, host := range strings.Split(domain, ",") {
		host = strings.TrimSpace(host)
		if host == "" {
			return fmt.Errorf("'domain' contains an empty entry (got %q)", domain)
		}
		if !validDomain.MatchString(host) {
			return fmt.Errorf("'domain' contains invalid characters (got %q)", domain)
		}
	}
	return nil
}

func (c *AppConfig) validate() error {
	if err := ValidateName(c.App); err != nil {
		return err
	}
	// Host ingress publishes a raw port and needs no domain; every other mode
	// routes by hostname and requires one.
	if err := ValidateDomain(c.Domain, c.Ingress == IngressHost); err != nil {
		return err
	}
	if c.Platform != "" && !validPlatform.MatchString(c.Platform) {
		return fmt.Errorf("'platform' must be os/arch (e.g. linux/amd64, linux/arm64), got %q", c.Platform)
	}
	// Validate ingress. Empty defaults to "caddy" at consumption time.
	switch c.Ingress {
	case "", IngressCaddy, IngressExternal, IngressHost:
	default:
		return fmt.Errorf("'ingress' must be one of: caddy, external, host (got %q)", c.Ingress)
	}
	// 'bind' is meaningful only for ingress: host.
	if c.Bind != "" {
		if c.Ingress != IngressHost {
			return fmt.Errorf("'bind' only applies to 'ingress: host'")
		}
		if net.ParseIP(c.Bind) == nil {
			return fmt.Errorf("'bind' must be a valid IP address (e.g. 0.0.0.0 or 127.0.0.1), got %q", c.Bind)
		}
	}
	// ingress: host publishes on a fixed host port via recreate.
	if c.Ingress == IngressHost {
		if c.Type == TypeStatic {
			return fmt.Errorf("'ingress: host' is not supported for type:static (static deploys require Caddy)")
		}
		if c.Port <= 0 {
			return fmt.Errorf("'ingress: host' requires 'port' (the fixed host port to publish on)")
		}
		if c.Replicas > 1 {
			return fmt.Errorf("'ingress: host' supports a single replica (a fixed host port can't be load-balanced across containers)")
		}
		if c.TLS != nil {
			return fmt.Errorf("'tls' has no effect with 'ingress: host' — terminate TLS in front of the published port")
		}
	}
	// Validate deploy type and its required fields. Empty type means container.
	switch c.Type {
	case "", TypeContainer:
		// container path uses Image / BuildLocal / Port / etc. — nothing extra to enforce here
	case TypeStatic:
		if c.Source == "" {
			return fmt.Errorf("'source' is required when type is 'static'")
		}
		if c.Image != "" {
			return fmt.Errorf("'image' cannot be set when type is 'static'")
		}
		// Static deploys ARE Caddy-served by definition (rsync + Caddy
		// file_server). External ingress wouldn't have anything to serve.
		if c.Ingress == IngressExternal {
			return fmt.Errorf("'ingress: external' is not supported for type:static (static deploys require Caddy)")
		}
		if c.BuildLocal {
			return fmt.Errorf("'build_local' is for container builds; use 'build_remote' for static deploys")
		}
		if c.Replicas > 0 {
			return fmt.Errorf("'replicas' is not supported for type:static (no processes to replicate)")
		}
		if c.SPAFallback != "" && !strings.HasPrefix(c.SPAFallback, "/") {
			return fmt.Errorf("'spa_fallback' must start with / (got %q)", c.SPAFallback)
		}
		if c.KeepReleases < 0 {
			return fmt.Errorf("'keep_releases' must be >= 0 (got %d)", c.KeepReleases)
		}
		if c.TLS != nil {
			// Static deploys generate a different Caddy block (file_server);
			// custom-cert support there is a separate change. Reject for now
			// rather than silently ignore.
			return fmt.Errorf("'tls' is not yet supported for type:static")
		}
		if c.KeepVersions != 0 {
			return fmt.Errorf("'keep_versions' is container-deploy only; static deploys use 'keep_releases'")
		}
	default:
		return fmt.Errorf("'type' must be 'container' or 'static' (got %q)", c.Type)
	}
	if c.TLS != nil {
		if c.TLS.Internal {
			if c.TLS.Cert != "" || c.TLS.Key != "" {
				return fmt.Errorf("'tls.internal' cannot be combined with 'cert'/'key' — pick one")
			}
		} else if c.TLS.Cert == "" || c.TLS.Key == "" {
			return fmt.Errorf("'tls' requires both 'cert' and 'key' paths (or 'internal: true' for a self-signed cert)")
		}
		// TLS only takes effect via the Caddy site block. With ingress:external
		// the user's external ingress (Cloudflare Tunnel, nginx, ALB, …)
		// handles TLS termination, so the cert/key would be uploaded but never
		// wired up — silent no-op that wastes bytes and confuses operators.
		if c.Ingress == IngressExternal {
			return fmt.Errorf("'tls' has no effect with 'ingress: external' — your external ingress should handle TLS termination")
		}
	}
	for _, ch := range c.Notifications.Channels {
		switch ch.Type {
		case "", "webhook", "slack", "ntfy", "email":
		default:
			return fmt.Errorf("'notifications.channels[].type' must be one of: webhook, slack, ntfy, email (got %q) — an unrecognized type silently never fires", ch.Type)
		}
	}
	if c.Health.TimeoutSeconds < 0 {
		return fmt.Errorf("'health.timeout_seconds' must be >= 0 (got %d)", c.Health.TimeoutSeconds)
	}
	if c.Health.IntervalSeconds < 0 {
		return fmt.Errorf("'health.interval_seconds' must be >= 0 (got %d)", c.Health.IntervalSeconds)
	}
	for name := range c.Volumes {
		if !validName.MatchString(name) {
			return fmt.Errorf("volume name %q must be lowercase alphanumeric with hyphens", name)
		}
	}
	for name, acc := range c.Accessories {
		if !validName.MatchString(name) {
			return fmt.Errorf("accessory name %q must be lowercase alphanumeric with hyphens", name)
		}
		for k, v := range acc.Env {
			if strings.HasPrefix(v, "secret:") && strings.TrimSpace(strings.TrimPrefix(v, "secret:")) == "" {
				return fmt.Errorf("accessory %s env %s: empty secret reference (expected secret:KEY)", name, k)
			}
		}
	}
	for name := range c.Processes {
		if !validName.MatchString(name) {
			return fmt.Errorf("process name %q must be lowercase alphanumeric with hyphens", name)
		}
	}
	for name := range c.Healthcheck {
		if !validName.MatchString(name) {
			return fmt.Errorf("healthcheck key %q must be lowercase alphanumeric with hyphens", name)
		}
		if _, ok := c.Processes[name]; !ok {
			return fmt.Errorf("healthcheck refers to unknown process %q (must match a key in processes)", name)
		}
	}
	if c.KeepVersions < 0 {
		return fmt.Errorf("'keep_versions' must be >= 0 (got %d)", c.KeepVersions)
	}
	// Build-context fields only apply when teploy builds the image itself.
	if c.Dockerfile != "" || c.Context != "" {
		if c.Image != "" {
			return fmt.Errorf("'dockerfile'/'context' apply only when teploy builds the image; a pre-built 'image' is pulled, not built — remove one")
		}
		if c.Type == TypeStatic {
			return fmt.Errorf("'dockerfile'/'context' are container-build fields with no effect on type:static")
		}
		if c.Context != "" && !isSafeSubPath(c.Context) {
			return fmt.Errorf("'context' must be a relative path inside the project (no absolute paths, no '..' escaping), got %q", c.Context)
		}
		if c.Dockerfile != "" && !isSafeSubPath(c.Dockerfile) {
			return fmt.Errorf("'dockerfile' must be a relative path inside the context (no absolute paths, no '..' escaping), got %q", c.Dockerfile)
		}
	}
	// Firewall renders into the Caddy site block, so it needs Caddy ingress and
	// a routed (non-static) app. Reject misuse loudly rather than silently drop.
	if !c.Firewall.IsZero() {
		if err := c.Firewall.validate(); err != nil {
			return err
		}
		if c.Type == TypeStatic {
			return fmt.Errorf("'firewall' is not supported for type:static (v1 covers reverse-proxied apps)")
		}
		if c.Ingress == IngressExternal || c.Ingress == IngressHost {
			return fmt.Errorf("'firewall' requires Caddy ingress — it has no effect with 'ingress: %s' (apply the rules in your fronting proxy/CDN instead)", c.Ingress)
		}
	}
	return nil
}

// unmarshalAppYAML strictly decodes a teploy.yml/teploy.<dest>.yml document.
// KnownFields(true) rejects unrecognized top-level (and nested struct) keys
// instead of silently dropping them. Teploy is Kamal-inspired but not
// schema-compatible, and copy-pasted Kamal fields (service, proxy, builder,
// registry, services, ...) are a common source of configs that parse clean
// but produce an empty/wrong AppConfig otherwise. On any decode error —
// unknown field or type mismatch alike — append a pointer at the schema
// divergence so the fix is discoverable without cross-referencing source.
func unmarshalAppYAML(data []byte, out *AppConfig) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil {
		if errors.Is(err, io.EOF) {
			return nil // empty document — same as yaml.Unmarshal's no-op
		}
		return fmt.Errorf("%w\nnote: teploy.yml is Kamal-inspired but has its own schema (not Kamal's deploy.yml) — see https://teploy.com/docs/reference/config. Common mismatches: 'app' not 'service'; 'build' is a list of shell commands, not a {command, output} map; no 'proxy'/'builder'/'services' blocks", err)
	}
	return nil
}

// LoadApp reads and parses teploy config (yml, yaml, or toml) from the given directory.
func LoadApp(dir string) (*AppConfig, error) {
	for _, name := range []string{"teploy.yml", "teploy.yaml", "teploy.toml"} {
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}

		var cfg AppConfig
		if strings.HasSuffix(name, ".toml") {
			if err := toml.Unmarshal(data, &cfg); err != nil {
				return nil, fmt.Errorf("parsing %s: %w", name, err)
			}
		} else {
			if err := unmarshalAppYAML(data, &cfg); err != nil {
				return nil, fmt.Errorf("parsing %s: %w", name, err)
			}
		}
		if err := cfg.validate(); err != nil {
			return nil, fmt.Errorf("invalid %s: %w", name, err)
		}
		return &cfg, nil
	}

	// No teploy config — try docker-compose auto-detection.
	composeCfg, err := LoadCompose(dir)
	if err != nil {
		return nil, err
	}
	if composeCfg != nil {
		return composeCfg, nil
	}

	return nil, fmt.Errorf("no teploy.yml, teploy.toml, or docker-compose file found in %s", dir)
}

// LoadAppWithDestination loads the base config and merges a destination overlay on top.
// For example, -d staging loads teploy.yml then merges teploy.staging.yml over it.
func LoadAppWithDestination(dir, dest string) (*AppConfig, error) {
	base, err := LoadApp(dir)
	if err != nil {
		return nil, err
	}

	// Try destination overlay files in order: yml, yaml, toml.
	for _, ext := range []string{".yml", ".yaml", ".toml"} {
		name := "teploy." + dest + ext
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}

		var overlay AppConfig
		if ext == ".toml" {
			if err := toml.Unmarshal(data, &overlay); err != nil {
				return nil, fmt.Errorf("parsing %s: %w", name, err)
			}
		} else {
			if err := unmarshalAppYAML(data, &overlay); err != nil {
				return nil, fmt.Errorf("parsing %s: %w", name, err)
			}
		}

		mergeConfigs(base, &overlay)
		if err := base.validate(); err != nil {
			return nil, fmt.Errorf("invalid config after merging %s: %w", name, err)
		}
		return base, nil
	}

	return nil, fmt.Errorf("destination %q not found — expected teploy.%s.yml or teploy.%s.toml", dest, dest, dest)
}

// mergeConfigs applies non-zero values from overlay onto base (mutates base).
func mergeConfigs(base, overlay *AppConfig) {
	if overlay.App != "" {
		base.App = overlay.App
	}
	if overlay.Domain != "" {
		base.Domain = overlay.Domain
	}
	if overlay.Server != "" {
		base.Server = overlay.Server
	}
	if overlay.User != "" {
		base.User = overlay.User
	}
	if len(overlay.Servers) > 0 {
		base.Servers = overlay.Servers
	}
	if overlay.Image != "" {
		base.Image = overlay.Image
	}
	if overlay.Ingress != "" {
		base.Ingress = overlay.Ingress
	}
	if overlay.Bind != "" {
		base.Bind = overlay.Bind
	}
	if overlay.Port != 0 {
		base.Port = overlay.Port
	}
	if overlay.Platform != "" {
		base.Platform = overlay.Platform
	}
	if overlay.BuildLocal {
		base.BuildLocal = overlay.BuildLocal
	}
	if overlay.StopTimeout != 0 {
		base.StopTimeout = overlay.StopTimeout
	}
	if overlay.Parallel != 0 {
		base.Parallel = overlay.Parallel
	}
	if overlay.Rollout != nil {
		base.Rollout = overlay.Rollout
	}
	if len(overlay.EnvFiles) > 0 {
		base.EnvFiles = append(base.EnvFiles, overlay.EnvFiles...)
	}
	if overlay.Scan {
		base.Scan = true
	}
	if overlay.KeepVersions != 0 {
		base.KeepVersions = overlay.KeepVersions
	}
	if overlay.Replicas != 0 {
		base.Replicas = overlay.Replicas
	}
	if overlay.Hooks.PreDeploy != "" {
		base.Hooks.PreDeploy = overlay.Hooks.PreDeploy
	}
	if overlay.Hooks.PostDeploy != "" {
		base.Hooks.PostDeploy = overlay.Hooks.PostDeploy
	}
	// Merge maps: overlay keys override base keys.
	if len(overlay.Volumes) > 0 {
		if base.Volumes == nil {
			base.Volumes = map[string]string{}
		}
		for k, v := range overlay.Volumes {
			base.Volumes[k] = v
		}
	}
	if len(overlay.Processes) > 0 {
		if base.Processes == nil {
			base.Processes = map[string]string{}
		}
		for k, v := range overlay.Processes {
			base.Processes[k] = v
		}
	}
	if len(overlay.Env) > 0 {
		if base.Env == nil {
			base.Env = map[string]string{}
		}
		for k, v := range overlay.Env {
			base.Env[k] = v
		}
	}
	if overlay.Health.Path != "" {
		base.Health.Path = overlay.Health.Path
	}
	if len(overlay.Healthcheck) > 0 {
		if base.Healthcheck == nil {
			base.Healthcheck = map[string]ProcessHealth{}
		}
		for k, v := range overlay.Healthcheck {
			base.Healthcheck[k] = v
		}
	}
	if len(overlay.Accessories) > 0 {
		if base.Accessories == nil {
			base.Accessories = map[string]AccessoryConfig{}
		}
		for k, v := range overlay.Accessories {
			base.Accessories[k] = mergeAccessory(base.Accessories[k], v)
		}
	}
	if overlay.Assets.Path != "" {
		base.Assets = overlay.Assets
	}
	if overlay.TLS != nil {
		base.TLS = overlay.TLS
	}
	if overlay.Notifications.Webhook != "" {
		base.Notifications.Webhook = overlay.Notifications.Webhook
	}
	if len(overlay.Notifications.Channels) > 0 {
		base.Notifications.Channels = overlay.Notifications.Channels
	}
	if overlay.Network.Provider != "" {
		base.Network = overlay.Network
	}
	// Static-deploy fields.
	if overlay.Type != "" {
		base.Type = overlay.Type
	}
	if overlay.Source != "" {
		base.Source = overlay.Source
	}
	if len(overlay.Build) > 0 {
		base.Build = overlay.Build
	}
	if overlay.BuildRemote {
		base.BuildRemote = overlay.BuildRemote
	}
	if overlay.SPA {
		base.SPA = overlay.SPA
	}
	if overlay.SPAFallback != "" {
		base.SPAFallback = overlay.SPAFallback
	}
	if len(overlay.Cache) > 0 {
		if base.Cache == nil {
			base.Cache = map[string]string{}
		}
		for k, v := range overlay.Cache {
			base.Cache[k] = v
		}
	}
	if len(overlay.Headers) > 0 {
		if base.Headers == nil {
			base.Headers = map[string]string{}
		}
		for k, v := range overlay.Headers {
			base.Headers[k] = v
		}
	}
	if overlay.KeepReleases > 0 {
		base.KeepReleases = overlay.KeepReleases
	}
	if overlay.CaddyExtra != "" {
		base.CaddyExtra = overlay.CaddyExtra
	}
}

// mergeAccessory deep-merges overlay fields onto base. Image / Port use
// the overlay value when non-zero; Env and Volumes are key-wise merged
// so an overlay can add just POSTGRES_PASSWORD without dropping the
// base's POSTGRES_USER / POSTGRES_DB. Previously the entire
// AccessoryConfig was replaced by the overlay value, which broke any
// partial overlay (e.g. a gitignored teploy.prod.yml that only carried
// the password while teploy.yml carried image / port / volumes).
func mergeAccessory(base, overlay AccessoryConfig) AccessoryConfig {
	out := base
	if overlay.Image != "" {
		out.Image = overlay.Image
	}
	if overlay.Port != 0 {
		out.Port = overlay.Port
	}
	if overlay.Command != "" {
		out.Command = overlay.Command
	}
	if len(overlay.Publish) > 0 {
		out.Publish = overlay.Publish
	}
	if len(overlay.Env) > 0 {
		if out.Env == nil {
			out.Env = map[string]string{}
		}
		for k, v := range overlay.Env {
			out.Env[k] = v
		}
	}
	if len(overlay.Volumes) > 0 {
		if out.Volumes == nil {
			out.Volumes = map[string]string{}
		}
		for k, v := range overlay.Volumes {
			out.Volumes[k] = v
		}
	}
	return out
}

// ParseAppBytes parses raw YAML bytes as an AppConfig.
// Used by template deploy to parse fetched template content.
func ParseAppBytes(data []byte) (*AppConfig, error) {
	var cfg AppConfig
	if err := unmarshalAppYAML(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}
	return &cfg, nil
}
