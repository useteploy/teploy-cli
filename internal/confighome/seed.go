package confighome

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/releasemeta"
)

// Seed rebuilds a teploy.yml for a deployed app from what its server
// recorded: the applied manifest (state.json) and the running release's
// record. It is offered only when it provably reproduces what runs (spec
// §2.1): the caller must check SeedMatches. Parts the server never keeps
// in full (secret-backed or redacted env values, hooks, accessories,
// custom TLS files, basic-auth hashes) make the app unseedable; the
// returned problems say which.
func Seed(rec *releasemeta.Record, manifest json.RawMessage) ([]byte, []string, error) {
	var m struct {
		App            string         `json:"app"`
		DeploymentType string         `json:"deployment_type"`
		Domain         string         `json:"domain"`
		Ingress        string         `json:"ingress_mode"`
		Container      map[string]any `json:"container"`
		VolumeOwner    map[string]any `json:"volume_ownership"`
		Accessories    map[string]any `json:"accessories"`
		TLS            map[string]any `json:"tls"`
		Firewall       map[string]any `json:"firewall"`
		Access         map[string]any `json:"access"`
		CaddyExtra     bool           `json:"caddy_extra_configured"`
		SecretProvider string         `json:"secret_provider"`
		SecretAgent    bool           `json:"secret_agent"`
	}
	if err := json.Unmarshal(manifest, &m); err != nil {
		return nil, nil, fmt.Errorf("reading the applied manifest: %w", err)
	}
	var problems []string
	if m.DeploymentType != config.TypeContainer {
		return nil, []string{"static sites cannot be rebuilt from the server yet"}, nil
	}
	c := m.Container
	seed := map[string]any{"app": m.App, "image": rec.ImageRef}
	if m.Domain != "" {
		seed["domain"] = m.Domain
	}
	if m.Ingress != "" && m.Ingress != config.IngressCaddy {
		seed["ingress"] = m.Ingress
	}
	copyKey := func(from, to string) {
		v, ok := c[from]
		if !ok || isZero(v) {
			return
		}
		seed[to] = v
	}
	for from, to := range map[string]string{
		"port": "port", "replicas": "replicas", "stop_timeout": "stop_timeout", "drain_seconds": "drain_seconds",
		"platform": "platform", "context": "context", "dockerfile": "dockerfile", "keep_versions": "keep_versions",
		"volumes": "volumes", "memory": "memory", "cpu": "cpu", "publish": "publish", "cache": "cache",
		"healthcheck": "healthcheck",
	} {
		copyKey(from, to)
	}
	if m.Ingress == config.IngressHost {
		copyKey("bind", "bind")
	}
	if b, _ := c["build_local"].(bool); b {
		seed["build_local"] = true
	}
	if h, ok := c["health"].(map[string]any); ok {
		defaults := map[string]any{"mode": config.HealthModeAuto, "path": "/health", "timeout_seconds": float64(30), "interval_seconds": float64(1)}
		custom := map[string]any{}
		for k, v := range h {
			if defaults[k] != v {
				custom[k] = v
			}
		}
		if len(custom) > 0 {
			seed["health"] = custom
		}
	}
	// Normalization fills these defaults back in; leaving them out keeps
	// the rebuilt file as short as a hand-written one.
	for k, def := range map[string]float64{"port": 80, "replicas": 1, "stop_timeout": 10} {
		if v, _ := seed[k].(float64); v == def {
			delete(seed, k)
		}
	}
	if p, _ := c["asset_path"].(string); p != "" {
		assets := map[string]any{"path": p}
		if d, ok := c["asset_keep_days"].(float64); ok && d != 0 {
			assets["keep_days"] = d
		}
		seed["assets"] = assets
	}
	if names, ok := c["processes"].([]any); ok && len(names) > 0 {
		procs := map[string]any{}
		for _, n := range names {
			name, _ := n.(string)
			cmd, ok := rec.Processes[name]
			if !ok {
				problems = append(problems, fmt.Sprintf("the command of process %q is not recorded", name))
				continue
			}
			procs[name] = cmd
		}
		seed["processes"] = procs
	}
	if keys, ok := c["env_keys"].([]any); ok && len(keys) > 0 {
		// Values the record kept as plain config: its env map, else the
		// recreated container's env list (deploys pass env through an env
		// file, so the map is often empty). Redacted values are in neither.
		recorded := map[string]string{}
		if rec.Recreate != nil {
			for _, kv := range rec.Recreate.Env {
				if k, v, ok := strings.Cut(kv, "="); ok {
					recorded[k] = v
				}
			}
		}
		for k, v := range rec.Env {
			recorded[k] = v
		}
		env := map[string]any{}
		var missing []string
		for _, k := range keys {
			key, _ := k.(string)
			v, ok := recorded[key]
			if !ok {
				missing = append(missing, key)
				continue
			}
			env[key] = strings.ReplaceAll(v, "$", "$$")
		}
		if len(missing) > 0 {
			sort.Strings(missing)
			problems = append(problems, fmt.Sprintf("the running release did not record the values of env %s (secret-looking, references, env files, or deployed by an older teploy) — redeploy once from the folder, or move the app from there", strings.Join(missing, ", ")))
		}
		seed["env"] = env
	}
	if files, ok := c["env_files"].([]any); ok && len(files) > 0 {
		problems = append(problems, "env_files are read on the deploying machine")
	}
	if b, _ := c["pre_deploy_hook"].(bool); b {
		problems = append(problems, "the pre_deploy hook command is not recorded")
	}
	if b, _ := c["post_deploy_hook"].(bool); b {
		problems = append(problems, "the post_deploy hook command is not recorded")
	}
	if len(m.VolumeOwner) > 0 {
		seed["volume_ownership"] = m.VolumeOwner
	}
	if len(m.Accessories) > 0 {
		problems = append(problems, "accessories (their env values are not recorded)")
	}
	if m.TLS != nil {
		if custom, _ := m.TLS["custom_certificate"].(bool); custom {
			problems = append(problems, "custom TLS certificate files live on the deploying machine")
		} else if internal, _ := m.TLS["internal"].(bool); internal {
			seed["tls"] = map[string]any{"internal": true}
		}
	}
	if m.Firewall != nil {
		fw := map[string]any{}
		for k, v := range m.Firewall {
			if !isZero(v) {
				fw[k] = v
			}
		}
		seed["firewall"] = fw
	}
	if m.Access != nil {
		problems = append(problems, "access rules (basic-auth hashes and forward_auth URLs are not recorded)")
	}
	if m.CaddyExtra {
		if rec.Caddy == nil || rec.Caddy.CaddyExtra == "" {
			problems = append(problems, "caddy_extra is not recorded")
		} else {
			seed["caddy_extra"] = rec.Caddy.CaddyExtra
		}
	}
	if m.SecretProvider != "" || m.SecretAgent {
		sec := map[string]any{}
		if m.SecretProvider != "" {
			sec["provider"] = m.SecretProvider
		}
		if m.SecretAgent {
			sec["agent"] = true
		}
		seed["secret"] = sec
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(seed); err != nil {
		return nil, nil, err
	}
	return buf.Bytes(), problems, nil
}

// SeedMatches reports whether seed, deployed with image, has exactly the
// recorded effective-config digest — the proof that no mount, port or
// setting is lost by editing it instead of the original folder.
func SeedMatches(seed []byte, image, manifestSHA256 string) (bool, error) {
	cfg, err := config.LoadAppBytes(seed)
	if err != nil {
		return false, err
	}
	_, digest, err := config.NormalizeAndDigest(cfg, image)
	if err != nil {
		return false, err
	}
	return digest == manifestSHA256, nil
}

func isZero(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return t == ""
	case bool:
		return !t
	case float64:
		return t == 0
	case []any:
		return len(t) == 0
	case map[string]any:
		return len(t) == 0
	}
	return false
}
