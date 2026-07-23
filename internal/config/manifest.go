package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"
)

// NormalizeAndDigest returns a canonical, redacted snapshot of the applied app
// manifest and its SHA-256. Secret-bearing values are represented only by key
// presence, so resolved environment values never enter release state.
func NormalizeAndDigest(cfg *AppConfig, appliedImage string) (json.RawMessage, string, error) {
	deploymentType := cfg.Type
	if deploymentType == "" {
		deploymentType = TypeContainer
	}
	ingress := cfg.Ingress
	if ingress == "" {
		ingress = IngressCaddy
	}
	port := cfg.Port
	if deploymentType == TypeContainer && port == 0 {
		port = 80
	}
	replicas := cfg.Replicas
	if deploymentType == TypeContainer && replicas == 0 {
		replicas = 1
	}
	stopTimeout := cfg.StopTimeout
	if deploymentType == TypeContainer && stopTimeout == 0 {
		stopTimeout = 10
	}

	manifest := map[string]any{
		"app":             cfg.App,
		"deployment_type": deploymentType,
		"domain":          normalizeDomains(cfg.Domain),
		"ingress_mode":    ingress,
	}
	if deploymentType == TypeContainer {
		manifest["container"] = map[string]any{
			"image":            appliedImage,
			"port":             port,
			"replicas":         replicas,
			"stop_timeout":     stopTimeout,
			"bind":             defaultString(cfg.Bind, ingress == IngressHost, "0.0.0.0"),
			"platform":         cfg.Platform,
			"build_local":      cfg.BuildLocal,
			"context":          cleanOptionalPath(cfg.Context),
			"dockerfile":       cleanOptionalPath(cfg.Dockerfile),
			"keep_versions":    cfg.KeepVersions,
			"processes":        sortedMapKeys(cfg.Processes),
			"env_keys":         sortedMapKeys(cfg.Env),
			"env_files":        append([]string(nil), cfg.EnvFiles...),
			"volumes":          cfg.Volumes,
			"health":           normalizedHealth(cfg.Health),
			"healthcheck":      cfg.Healthcheck,
			"pre_deploy_hook":  cfg.Hooks.PreDeploy != "",
			"post_deploy_hook": cfg.Hooks.PostDeploy != "",
			"asset_path":       cfg.Assets.Path,
			"asset_keep_days":  cfg.Assets.KeepDays,
		}
	} else {
		keepReleases := cfg.KeepReleases
		if keepReleases == 0 {
			keepReleases = 5
		}
		manifest["static"] = map[string]any{
			"source":        cleanOptionalPath(cfg.Source),
			"build_steps":   len(cfg.Build),
			"build_remote":  cfg.BuildRemote,
			"spa":           cfg.SPA,
			"spa_fallback":  defaultString(cfg.SPAFallback, cfg.SPA, "/index.html"),
			"cache":         cfg.Cache,
			"header_names":  sortedMapKeys(cfg.Headers),
			"keep_releases": keepReleases,
		}
	}

	manifest["accessories"] = normalizedAccessories(cfg.Accessories)
	manifest["tls"] = normalizedTLS(cfg.TLS)
	manifest["firewall"] = normalizedFirewall(cfg.Firewall)
	manifest["access"] = normalizedAccess(cfg.Access)
	manifest["caddy_extra_configured"] = cfg.CaddyExtra != ""
	manifest["secret_provider"] = cfg.Secret.Provider
	manifest["secret_agent"] = cfg.Secret.Agent

	data, err := json.Marshal(manifest)
	if err != nil {
		return nil, "", err
	}
	digest := sha256.Sum256(data)
	return json.RawMessage(data), hex.EncodeToString(digest[:]), nil
}

func normalizeDomains(domain string) string {
	if domain == "" {
		return ""
	}
	hosts := strings.Split(domain, ",")
	for i := range hosts {
		hosts[i] = strings.ToLower(strings.TrimSpace(hosts[i]))
	}
	sort.Strings(hosts)
	return strings.Join(hosts, ",")
}

func cleanOptionalPath(path string) string {
	if path == "" {
		return ""
	}
	return filepath.ToSlash(filepath.Clean(path))
}

func defaultString(value string, apply bool, fallback string) string {
	if value == "" && apply {
		return fallback
	}
	return value
}

func sortedMapKeys[V any](values map[string]V) []string {
	if len(values) == 0 {
		return nil
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func normalizedHealth(health AppHealthConfig) map[string]any {
	path := health.Path
	if path == "" {
		path = "/health"
	}
	timeout := health.TimeoutSeconds
	if timeout == 0 {
		timeout = 30
	}
	interval := health.IntervalSeconds
	if interval == 0 {
		interval = 1
	}
	return map[string]any{"path": path, "timeout_seconds": timeout, "interval_seconds": interval}
}

func normalizedAccessories(accessories map[string]AccessoryConfig) map[string]any {
	if len(accessories) == 0 {
		return nil
	}
	out := make(map[string]any, len(accessories))
	for name, accessory := range accessories {
		out[name] = map[string]any{
			"image":       accessory.Image,
			"port":        accessory.Port,
			"env_keys":    sortedMapKeys(accessory.Env),
			"volumes":     accessory.Volumes,
			"publish":     accessory.Publish,
			"has_command": accessory.Command != "",
		}
	}
	return out
}

func normalizedTLS(tls *TLSConfig) map[string]any {
	if tls == nil {
		return nil
	}
	return map[string]any{"internal": tls.Internal, "custom_certificate": tls.Cert != "" || tls.Key != ""}
}

func normalizedFirewall(firewall FirewallConfig) map[string]any {
	if firewall.IsZero() {
		return nil
	}
	allow := append([]string(nil), firewall.AllowIPs...)
	deny := append([]string(nil), firewall.DenyIPs...)
	agents := append([]string(nil), firewall.BlockUserAgents...)
	sort.Strings(allow)
	sort.Strings(deny)
	sort.Strings(agents)
	return map[string]any{
		"allow_ips": allow, "deny_ips": deny,
		"block_user_agents": agents, "max_body_size": strings.ToUpper(strings.ReplaceAll(firewall.MaxBodySize, " ", "")),
	}
}

func normalizedAccess(access AccessConfig) map[string]any {
	if access.IsZero() {
		return nil
	}
	out := map[string]any{"basic_auth_users": sortedMapKeys(access.BasicAuth)}
	if access.ForwardAuth != nil {
		out["forward_auth"] = map[string]any{
			"configured":   access.ForwardAuth.URL != "",
			"uri":          access.ForwardAuth.URI,
			"copy_headers": append([]string(nil), access.ForwardAuth.CopyHeaders...),
		}
	}
	return out
}
