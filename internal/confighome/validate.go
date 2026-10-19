package confighome

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/releasemeta"
)

// Load parses and validates a Teploy-home teploy.yml: the normal schema
// (config.LoadAppBytes) plus the rules that make "same commit, same
// deploy" true no matter which client deploys it (spec §2.3):
//
//   - no ${VAR}/$VAR references in env: (no client's environment is
//     consulted; write $$ for a literal $),
//   - no env_files, no server:/servers: (the target is the server that
//     stores the config), no TLS cert/key paths (local files),
//   - no local static build: commands or source: path (they would run in
//     the deploying client's shell),
//   - no secret-looking literal values (use `teploy secret set`),
//   - container apps name an image (there is no folder to build from).
//
// Every problem is reported at once, each with its remedy.
func Load(raw []byte, app string) (*config.AppConfig, error) {
	cfg, err := config.LoadAppBytes(raw)
	if err != nil {
		return nil, err
	}
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	if app != "" && cfg.App != app {
		add("app: is %q but this config belongs to %q", cfg.App, app)
	}
	for _, ref := range dollarRefs(raw) {
		add("%s references %s — Teploy-home configs are not expanded from anyone's environment; store the value with `teploy secret set` (or write $$ for a literal $)", ref.path, ref.ref)
	}
	if len(cfg.EnvFiles) > 0 {
		add("env_files: reads files from the deploying machine — move the values to `teploy secret set` / `teploy env set`")
	}
	if cfg.Server != "" || len(cfg.Servers) > 0 {
		add("server:/servers: is not used in Teploy home — the app deploys to the server that stores its config (remove the key)")
	}
	if cfg.TLS != nil && (cfg.TLS.Cert != "" || cfg.TLS.Key != "") {
		add("tls.cert/tls.key are local file paths — use tls.internal or ACME, or keep this app in folder home")
	}
	if cfg.IsStatic() {
		if cfg.Source != "" {
			add("source: is a local path — static sites in Teploy home build from a code source with build_remote")
		}
		if len(cfg.Build) > 0 && !cfg.BuildRemote {
			add("build: commands would run in the deploying machine's shell — set build_remote: true")
		}
	} else if cfg.Image == "" {
		add("image: is required — a Teploy-home app has no folder to build from")
	}
	keys := make([]string, 0, len(cfg.Env))
	for k := range cfg.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := cfg.Env[k]
		if strings.HasPrefix(v, "secret:") || strings.HasPrefix(v, "vault:") {
			continue
		}
		if keep, src := releasemeta.ClassifyEnv(k, v, map[string]string{k: releasemeta.EnvSourceConfig}, nil); !keep && v != "" {
			add("env.%s looks like a secret (%s) — run `teploy secret set %s` and remove it from the config", k, src, k)
		}
	}
	for k, v := range cfg.EnvLiteral {
		if keep, src := releasemeta.ClassifyEnv(k, v, map[string]string{k: releasemeta.EnvSourceConfigLiteral}, nil); !keep && v != "" {
			add("env_literal.%s looks like a secret (%s) — run `teploy secret set %s` and remove it from the config", k, src, k)
		}
	}
	for name, acc := range cfg.Accessories {
		for k, v := range acc.Env {
			if strings.HasPrefix(v, "secret:") {
				continue
			}
			if keep, src := releasemeta.ClassifyEnv(k, v, map[string]string{k: releasemeta.EnvSourceConfig}, nil); !keep && v != "" {
				add("accessories.%s.env.%s looks like a secret (%s) — run `teploy secret set %s` and reference it as secret:%s", name, k, src, k, k)
			}
		}
	}
	for field, v := range map[string]string{
		"notifications.secret": cfg.Notifications.Secret,
		"audit.token":          cfg.Audit.Token,
		"network.auth_key":     cfg.Network.AuthKey,
		"network.setup_key":    cfg.Network.SetupKey,
	} {
		if v != "" {
			add("%s is a credential in plain text — Teploy-home configs cannot hold credentials; keep this app in folder home for now", field)
		}
	}
	for i, ch := range cfg.Notifications.Channels {
		if ch.Secret != "" {
			add("notifications.channels[%d].secret is a credential in plain text — Teploy-home configs cannot hold credentials", i)
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return nil, fmt.Errorf("this config cannot be stored in Teploy:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return cfg, nil
}

type dollarRef struct{ path, ref string }

var dollarRE = regexp.MustCompile(`\$+(\{[A-Za-z_][A-Za-z0-9_]*\}|[A-Za-z_][A-Za-z0-9_]*)`)

// dollarRefs finds $NAME / ${NAME} in env: values — the only field a
// folder deploy expands from the operator's environment, so the only one
// whose meaning would depend on who deploys. An even run of $ is escaped
// ($$NAME is the literal text $NAME). Other fields (cmd:, hooks) are never
// expanded by teploy and keep their shell $VARs as written.
func dollarRefs(raw []byte) []dollarRef {
	var doc struct {
		Env map[string]any `yaml:"env"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil
	}
	var out []dollarRef
	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch t := v.(type) {
		case map[string]any:
			for k, vv := range t {
				walk(path+"."+k, vv)
			}
		case []any:
			for i, vv := range t {
				walk(fmt.Sprintf("%s[%d]", path, i), vv)
			}
		case string:
			for _, m := range dollarRE.FindAllString(t, -1) {
				n := len(m) - len(strings.TrimLeft(m, "$"))
				if n%2 == 1 {
					out = append(out, dollarRef{path: path, ref: "$" + strings.TrimLeft(m, "$")})
				}
			}
		}
	}
	walk("env", map[string]any(doc.Env))
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out
}

// Unescape turns $$ into $ in Teploy-home env: values: no expansion ever
// runs for them, so $$ is how a literal $ is written. (A folder deploy has
// no escape: os.Expand turns $$ into an empty string, so `teploy config
// home` warns when it moves a value containing $$ either way.)
func Unescape(env map[string]string) {
	for k, v := range env {
		env[k] = strings.ReplaceAll(v, "$$", "$")
	}
}

// PrivilegedChange reports whether moving from prev to next touches config
// that reaches the host: host-bind volumes (app or accessory), caddy_extra,
// published ports, the host bind address, ingress mode, or network
// provider settings. Dash requires the admin capability for these; the CLI
// marks the config.set event privileged.
func PrivilegedChange(prev, next *config.AppConfig) bool {
	if prev == nil {
		prev = &config.AppConfig{}
	}
	if hostBinds(prev.Volumes) != hostBinds(next.Volumes) ||
		prev.CaddyExtra != next.CaddyExtra ||
		strings.Join(prev.Publish, ",") != strings.Join(next.Publish, ",") ||
		prev.Bind != next.Bind || prev.Ingress != next.Ingress ||
		prev.Network != next.Network {
		return true
	}
	if (prev.Ingress == config.IngressHost || next.Ingress == config.IngressHost) && prev.Port != next.Port {
		return true
	}
	names := map[string]bool{}
	for n := range prev.Accessories {
		names[n] = true
	}
	for n := range next.Accessories {
		names[n] = true
	}
	for n := range names {
		a, b := prev.Accessories[n], next.Accessories[n]
		if hostBinds(a.Volumes) != hostBinds(b.Volumes) || a.Port != b.Port {
			return true
		}
	}
	return false
}

func hostBinds(vols map[string]string) string {
	var out []string
	for src, dst := range vols {
		if config.IsHostBindVolume(src) {
			out = append(out, src+"="+dst)
		}
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}
