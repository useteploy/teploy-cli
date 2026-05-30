package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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

// AccessoryConfig represents a stateful service container (database, cache, etc.).
type AccessoryConfig struct {
	Image   string            `yaml:"image" toml:"image"`
	Port    int               `yaml:"port,omitempty" toml:"port"`
	Env     map[string]string `yaml:"env,omitempty" toml:"env"`
	Volumes map[string]string `yaml:"volumes,omitempty" toml:"volumes"`
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
	Cert string `yaml:"cert" toml:"cert"` // local path to PEM certificate (chain)
	Key  string `yaml:"key" toml:"key"`   // local path to PEM private key
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

// AppConfig represents the teploy configuration (yml, yaml, or toml).
type AppConfig struct {
	App           string                     `yaml:"app" toml:"app"`
	Domain        string                     `yaml:"domain" toml:"domain"`
	Type          string                     `yaml:"type,omitempty" toml:"type"`
	Server        string                     `yaml:"server,omitempty" toml:"server"`
	Servers       []string                   `yaml:"servers,omitempty" toml:"servers"`
	Image         string                     `yaml:"image,omitempty" toml:"image"`
	Port          int                        `yaml:"port,omitempty" toml:"port"`
	Platform      string                     `yaml:"platform,omitempty" toml:"platform"`
	BuildLocal    bool                       `yaml:"build_local,omitempty" toml:"build_local"`
	StopTimeout   int                        `yaml:"stop_timeout,omitempty" toml:"stop_timeout"`
	Parallel      int                        `yaml:"parallel,omitempty" toml:"parallel"`
	Replicas      int                        `yaml:"replicas,omitempty" toml:"replicas"`
	// KeepVersions caps the number of past app versions retained after a
	// successful deploy (containers + images). Zero (default) keeps
	// everything — historical behavior. Set to 2 or 3 to enable auto-prune
	// with a rollback window. Container-deploy only; static deploy uses
	// KeepReleases instead.
	KeepVersions  int                        `yaml:"keep_versions,omitempty" toml:"keep_versions"`
	Hooks         HooksConfig                `yaml:"hooks,omitempty" toml:"hooks"`
	Volumes       map[string]string          `yaml:"volumes,omitempty" toml:"volumes"`
	Processes     map[string]string          `yaml:"processes,omitempty" toml:"processes"`
	// Healthcheck holds per-process overrides for the container HEALTHCHECK,
	// keyed by process name. Keys must match a key in Processes. Additive to
	// the scalar `processes:` schema — does not replace it.
	Healthcheck   map[string]ProcessHealth   `yaml:"healthcheck,omitempty" toml:"healthcheck"`
	Accessories   map[string]AccessoryConfig `yaml:"accessories,omitempty" toml:"accessories"`
	Assets        AssetsConfig               `yaml:"assets,omitempty" toml:"assets"`
	// TLS terminates the app's HTTPS with a custom cert instead of ACME.
	// Required behind Cloudflare proxy / Tunnel (origin cert). Nil = ACME.
	TLS           *TLSConfig                 `yaml:"tls,omitempty" toml:"tls"`
	Notifications NotificationsConfig        `yaml:"notifications,omitempty" toml:"notifications"`
	Network       NetworkConfig              `yaml:"network,omitempty" toml:"network"`

	// Static deploy fields. Only used when Type == "static".
	Source       string            `yaml:"source,omitempty" toml:"source"`             // local path to built files
	Build        []string          `yaml:"build,omitempty" toml:"build"`               // pre-rsync shell commands (run locally by default)
	BuildRemote  bool              `yaml:"build_remote,omitempty" toml:"build_remote"` // run Build commands on the target server instead of locally
	SPA          bool              `yaml:"spa,omitempty" toml:"spa"`                   // enable SPA fallback (try_files)
	SPAFallback  string            `yaml:"spa_fallback,omitempty" toml:"spa_fallback"` // fallback path (default /index.html)
	Cache        map[string]string `yaml:"cache,omitempty" toml:"cache"`               // path glob → Cache-Control value
	Headers      map[string]string `yaml:"headers,omitempty" toml:"headers"`           // arbitrary response headers
	KeepReleases int               `yaml:"keep_releases,omitempty" toml:"keep_releases"` // retention count (default 5)
	CaddyExtra   string            `yaml:"caddy_extra,omitempty" toml:"caddy_extra"`   // raw Caddy directives appended into the site block
}

// IsStatic reports whether this app deploys as static files (no container).
func (c *AppConfig) IsStatic() bool {
	return c.Type == TypeStatic
}

func (c *AppConfig) validate() error {
	if c.App == "" {
		return fmt.Errorf("'app' is required")
	}
	if !validName.MatchString(c.App) {
		return fmt.Errorf("'app' must be lowercase alphanumeric with hyphens (got %q)", c.App)
	}
	if len(c.App) > 63 {
		return fmt.Errorf("'app' name too long (max 63 chars, got %d)", len(c.App))
	}
	if c.Domain == "" {
		return fmt.Errorf("'domain' is required")
	}
	// A single comma-separated list is supported so one app can serve multiple
	// hosts (e.g. "example.com, www.example.com"). Each entry is validated
	// independently against the single-host regex.
	for _, host := range strings.Split(c.Domain, ",") {
		host = strings.TrimSpace(host)
		if host == "" {
			return fmt.Errorf("'domain' contains an empty entry (got %q)", c.Domain)
		}
		if !validDomain.MatchString(host) {
			return fmt.Errorf("'domain' contains invalid characters (got %q)", c.Domain)
		}
	}
	if c.Platform != "" && !validPlatform.MatchString(c.Platform) {
		return fmt.Errorf("'platform' must be os/arch (e.g. linux/amd64, linux/arm64), got %q", c.Platform)
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
		if c.TLS.Cert == "" || c.TLS.Key == "" {
			return fmt.Errorf("'tls' requires both 'cert' and 'key' paths")
		}
	}
	for name := range c.Volumes {
		if !validName.MatchString(name) {
			return fmt.Errorf("volume name %q must be lowercase alphanumeric with hyphens", name)
		}
	}
	for name := range c.Accessories {
		if !validName.MatchString(name) {
			return fmt.Errorf("accessory name %q must be lowercase alphanumeric with hyphens", name)
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
			if err := yaml.Unmarshal(data, &cfg); err != nil {
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
			if err := yaml.Unmarshal(data, &overlay); err != nil {
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
	if len(overlay.Servers) > 0 {
		base.Servers = overlay.Servers
	}
	if overlay.Image != "" {
		base.Image = overlay.Image
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
	if overlay.KeepVersions != 0 {
		base.KeepVersions = overlay.KeepVersions
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
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}
	return &cfg, nil
}
