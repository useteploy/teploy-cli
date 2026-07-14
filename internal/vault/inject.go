package vault

import (
	"context"
	"fmt"
	"strings"
)

// VaultRefPrefix marks a teploy.yml env value that should be resolved from
// OpenBao at deploy time, e.g.  env: { DB_PASS: "vault:db#password" } fetches
// key `password` from secret/<app>/db.
const VaultRefPrefix = "vault:"

// ParseRef splits a "vault:<name>#<key>" reference into (name, key). A missing
// "#key" fetches the whole secret's sole/first field is NOT assumed — key is
// required so resolution is unambiguous.
func ParseRef(value string) (name, key string, ok bool) {
	if !strings.HasPrefix(value, VaultRefPrefix) {
		return "", "", false
	}
	rest := strings.TrimPrefix(value, VaultRefPrefix)
	name, key, found := strings.Cut(rest, "#")
	if !found || name == "" || key == "" {
		return "", "", false
	}
	return name, key, true
}

// CollectRefs returns the env keys whose values are vault references, mapped to
// their parsed (name, key). Non-references are ignored.
func CollectRefs(env map[string]string) map[string][2]string {
	refs := make(map[string][2]string)
	for envKey, val := range env {
		if name, key, ok := ParseRef(val); ok {
			refs[envKey] = [2]string{name, key}
		}
	}
	return refs
}

// ResolveEnvRefs fetches every vault:<name>#<key> reference in env from OpenBao
// and returns a map of envKey -> secret value. Returns an empty map (and no
// error) when there are no references, so callers can invoke it unconditionally.
// Secrets are fetched via the app's stored access (root token) and grouped by
// secret name so each secret is read once.
func (c *Client) ResolveEnvRefs(ctx context.Context, app, accessory string, env map[string]string) (map[string]string, error) {
	refs := CollectRefs(env)
	if len(refs) == 0 {
		return map[string]string{}, nil
	}
	if accessory == "" {
		accessory = defaultAccessory
	}

	// Group by secret name to read each secret once.
	byName := make(map[string][]string) // name -> envKeys
	for envKey, nk := range refs {
		byName[nk[0]] = append(byName[nk[0]], envKey)
	}

	out := make(map[string]string, len(refs))
	cache := make(map[string]map[string]any)
	for name := range byName {
		data, err := c.Get(ctx, app, accessory, name)
		if err != nil {
			return nil, fmt.Errorf("resolving vault secret %q: %w", name, err)
		}
		cache[name] = data
	}
	for envKey, nk := range refs {
		name, key := nk[0], nk[1]
		v, ok := cache[name][key]
		if !ok {
			return nil, fmt.Errorf("vault secret %q has no key %q (referenced by env %s)", name, key, envKey)
		}
		out[envKey] = fmt.Sprint(v)
	}
	return out, nil
}
