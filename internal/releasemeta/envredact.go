package releasemeta

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/useteploy/teploy/internal/docker"
	"github.com/useteploy/teploy/internal/ssh"
)

// Env provenance: where a container env value came from. The deploy path
// records it per key (Record.EnvProvenance) so the persisted record can keep
// plain configuration and drop everything secret-capable. Only the
// EnvSourceConfig* sources are ever persisted with a value.
const (
	// EnvSourceConfig is a plain teploy.yml `env:` value with no ${VAR}
	// interpolation and no secret/vault directive.
	EnvSourceConfig = "config"
	// EnvSourceConfigLiteral is a teploy.yml `env_literal:` value.
	EnvSourceConfigLiteral = "config_literal"
	// EnvSourceImage is a value identical to the image's own ENV default.
	EnvSourceImage = "image"
	// EnvSourceRuntime is a value teploy itself injects (PORT).
	EnvSourceRuntime = "runtime"

	EnvSourceSecretStore   = "secret_store"   // `teploy secret` (age store)
	EnvSourceVault         = "vault"          // OpenBao vault:/secret: references
	EnvSourcePersistedEnv  = "persisted_env"  // /deployments/<app>/.env (`teploy env set`)
	EnvSourceEnvFile       = "env_file"       // decrypted local env_files
	EnvSourceInterpolated  = "interpolated"   // ${VAR} expanded from the deployer's environment
	EnvSourceNamePattern   = "name_pattern"   // plain value redacted by key-name backstop
	EnvSourceCredentialURL = "credential_url" // plain value carrying URL userinfo
	EnvSourceUnclassified  = "unclassified"   // provenance unknown (backfill, migration)
)

// secretKeyName is the key-name backstop: a plain config value whose key
// looks like a credential is still not persisted. Deliberately broad — a
// false positive only drops a value nothing reads back.
var secretKeyName = regexp.MustCompile(`(?i)(PASS|PWD|SECRET|TOKEN|SALT|PRIVATE|CREDENTIAL|API_?KEY|ACCESS_?KEY|AUTH|COOKIE|SIGNING|JWT|DSN|SESSION|CERT|_KEY$|^KEY$)`)

// runtimeEnvKeys are injected by teploy's own docker run (docker.go: -e PORT).
var runtimeEnvKeys = map[string]bool{"PORT": true}

// hasURLCredentials reports a value carrying userinfo with a password, e.g.
// postgres://user:pass@host/db. Scheme-less forms are caught too.
func hasURLCredentials(v string) bool {
	if !strings.Contains(v, "@") {
		return false
	}
	u, err := url.Parse(v)
	if err == nil && u.User != nil {
		if _, set := u.User.Password(); set {
			return true
		}
	}
	// Fallback for strings url.Parse rejects (e.g. passwords with reserved
	// characters): "<scheme>://<x>:<y>@".
	if i := strings.Index(v, "://"); i >= 0 {
		rest := v[i+3:]
		if at := strings.Index(rest, "@"); at > 0 && strings.Contains(rest[:at], ":") {
			return true
		}
	}
	return false
}

// ClassifyEnv decides whether one env entry's value may be persisted.
// provenance maps key -> source for keys the deploy path knows; imageEnv is
// the image's own ENV (key -> value), nil when unknown. Unknown provenance is
// never persisted.
func ClassifyEnv(key, value string, provenance map[string]string, imageEnv map[string]string) (keep bool, source string) {
	src, known := provenance[key]
	if !known {
		switch {
		case imageEnv != nil && imageEnvHas(imageEnv, key, value):
			src = EnvSourceImage
		case runtimeEnvKeys[key]:
			src = EnvSourceRuntime
		default:
			return false, EnvSourceUnclassified
		}
	}
	switch src {
	case EnvSourceConfig, EnvSourceConfigLiteral, EnvSourceImage, EnvSourceRuntime:
		if src != EnvSourceRuntime && secretKeyName.MatchString(key) {
			return false, EnvSourceNamePattern
		}
		if hasURLCredentials(value) {
			return false, EnvSourceCredentialURL
		}
		return true, src
	default:
		return false, src
	}
}

func imageEnvHas(imageEnv map[string]string, key, value string) bool {
	v, ok := imageEnv[key]
	return ok && v == value
}

// redactEnvList filters KEY=VALUE entries, returning the kept entries and the
// redacted key references (sorted, deduplicated with any existing ones).
func redactEnvList(env []string, existing []docker.RedactedEnv, provenance, imageEnv map[string]string) ([]string, []docker.RedactedEnv) {
	var kept []string
	red := map[string]string{}
	for _, r := range existing {
		red[r.Key] = r.Source
	}
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			// A bare KEY (no '=') carries no value; keep it as-is.
			kept = append(kept, kv)
			continue
		}
		if keep, src := ClassifyEnv(k, v, provenance, imageEnv); keep {
			kept = append(kept, kv)
		} else {
			red[k] = src
		}
	}
	return kept, sortedRedactions(red)
}

func sortedRedactions(red map[string]string) []docker.RedactedEnv {
	if len(red) == 0 {
		return nil
	}
	out := make([]docker.RedactedEnv, 0, len(red))
	for k, s := range red {
		out = append(out, docker.RedactedEnv{Key: k, Source: s})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// redactRecord removes every secret-capable env value from the record in
// place: the recreate spec's env list and the record's env map. Idempotent —
// an already-redacted record is unchanged.
func redactRecord(rec *Record) {
	if rec.Recreate != nil {
		rec.Recreate.Env, rec.Recreate.EnvRedacted = redactEnvList(rec.Recreate.Env, rec.Recreate.EnvRedacted, rec.EnvProvenance, rec.ImageEnv)
	}
	if len(rec.Env) > 0 || len(rec.EnvRedacted) > 0 {
		kept := map[string]string{}
		red := map[string]string{}
		for _, r := range rec.EnvRedacted {
			red[r.Key] = r.Source
		}
		for k, v := range rec.Env {
			if keep, src := ClassifyEnv(k, v, rec.EnvProvenance, rec.ImageEnv); keep {
				kept[k] = v
			} else {
				red[k] = src
			}
		}
		if len(kept) == 0 {
			kept = nil
		}
		rec.Env = kept
		rec.EnvRedacted = sortedRedactions(red)
	}
}

// RedactRaw rewrites one serialized release record into the redacted shape,
// preserving every other field verbatim (unknown fields included). It is the
// path for records this CLI did not just build: DR bundle export/restore and
// `teploy meta scrub`. With no provenance, only image defaults (when imageEnv
// is known) and PORT keep their values. It returns the rewritten bytes, the
// redacted key names (never values), and whether anything changed.
func RedactRaw(raw []byte, imageEnv map[string]string) ([]byte, []string, bool, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, nil, false, fmt.Errorf("parsing release record: %w", err)
	}
	changed := false
	var redactedKeys []string

	if rc, ok := doc["recreate"]; ok && string(rc) != "null" {
		var spec map[string]json.RawMessage
		if err := json.Unmarshal(rc, &spec); err != nil {
			return nil, nil, false, fmt.Errorf("parsing recreate spec: %w", err)
		}
		var env []string
		var existing []docker.RedactedEnv
		if e, ok := spec["env"]; ok {
			if err := json.Unmarshal(e, &env); err != nil {
				return nil, nil, false, fmt.Errorf("parsing recreate env: %w", err)
			}
		}
		if e, ok := spec["env_redacted"]; ok {
			if err := json.Unmarshal(e, &existing); err != nil {
				return nil, nil, false, fmt.Errorf("parsing recreate env_redacted: %w", err)
			}
		}
		kept, red := redactEnvList(env, existing, nil, imageEnv)
		if len(kept) != len(env) {
			changed = true
			still := map[string]bool{}
			for _, kv := range kept {
				k, _, _ := strings.Cut(kv, "=")
				still[k] = true
			}
			for _, kv := range env {
				if k, _, _ := strings.Cut(kv, "="); !still[k] {
					redactedKeys = append(redactedKeys, k)
				}
			}
			setOrDelete(spec, "env", kept, len(kept) > 0)
			setOrDelete(spec, "env_redacted", red, len(red) > 0)
			b, err := json.Marshal(spec)
			if err != nil {
				return nil, nil, false, err
			}
			doc["recreate"] = b
		}
	}

	if e, ok := doc["env"]; ok && string(e) != "null" {
		var env map[string]string
		if err := json.Unmarshal(e, &env); err != nil {
			return nil, nil, false, fmt.Errorf("parsing record env: %w", err)
		}
		var existing []docker.RedactedEnv
		if r, ok := doc["env_redacted"]; ok {
			if err := json.Unmarshal(r, &existing); err != nil {
				return nil, nil, false, fmt.Errorf("parsing record env_redacted: %w", err)
			}
		}
		red := map[string]string{}
		for _, r := range existing {
			red[r.Key] = r.Source
		}
		kept := map[string]string{}
		for k, v := range env {
			if keep, src := ClassifyEnv(k, v, nil, imageEnv); keep {
				kept[k] = v
			} else {
				red[k] = src
				redactedKeys = append(redactedKeys, k)
			}
		}
		if len(kept) != len(env) {
			changed = true
			setOrDelete(doc, "env", kept, len(kept) > 0)
			setOrDelete(doc, "env_redacted", sortedRedactions(red), len(red) > 0)
		}
	}

	if !changed {
		return raw, nil, false, nil
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return nil, nil, false, err
	}
	sort.Strings(redactedKeys)
	return append(out, '\n'), dedupe(redactedKeys), true, nil
}

func setOrDelete(m map[string]json.RawMessage, key string, v any, set bool) {
	if !set {
		delete(m, key)
		return
	}
	b, _ := json.Marshal(v)
	m[key] = b
}

func dedupe(s []string) []string {
	out := s[:0]
	for i, v := range s {
		if i == 0 || v != s[i-1] {
			out = append(out, v)
		}
	}
	return out
}

// HostEnvSources reads, from the target, the NAMES (never values) of env
// keys that come from host-held secret-capable sources: the age secret store
// (/deployments/<app>/secrets/<KEY>.age) and the persisted .env written by
// `teploy env set`. Best-effort: a read failure returns what was read, and
// unknown keys are redacted anyway.
func HostEnvSources(ctx context.Context, exec ssh.Executor, app string) map[string]string {
	out := map[string]string{}
	base := fmt.Sprintf("%s/%s", deploymentsDir, app)
	if names, err := exec.Run(ctx, fmt.Sprintf("ls -1 %s/secrets 2>/dev/null | sed -n 's/\\.age$//p'", ssh.ShellQuote(base))); err == nil {
		for _, k := range strings.Fields(names) {
			out[k] = EnvSourceSecretStore
		}
	}
	if names, err := exec.Run(ctx, fmt.Sprintf("test -f %s/.env && sed -n 's/^\\([A-Za-z_][A-Za-z0-9_]*\\)=.*/\\1/p' %s/.env", ssh.ShellQuote(base), ssh.ShellQuote(base))); err == nil {
		for _, k := range strings.Fields(names) {
			if _, set := out[k]; !set {
				out[k] = EnvSourcePersistedEnv
			}
		}
	}
	return out
}
