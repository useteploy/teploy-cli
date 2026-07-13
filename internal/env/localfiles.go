package env

// Local env files with at-rest encryption (the SOPS+age GitOps pattern):
// secrets live encrypted in the repo, get decrypted on the operator's
// machine at deploy time, and ride the existing deploy-env path to the
// container. Nothing new is persisted anywhere — the encrypted file stays
// the source of truth.
//
// Decryption shells out to the local `age` / `sops` binaries (mirroring how
// the server-side secret store shells out to age on the host) so teploy
// carries no crypto dependencies and interoperates with files produced by
// the standard tools.

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// LoadLocalEnvFiles reads each file (paths relative to dir), decrypting as
// needed, and returns the merged KEY=value map (later files win).
//
//   - `*.age`                     — age-encrypted dotenv; decrypted with the
//     identity file from $TEPLOY_AGE_IDENTITY, $SOPS_AGE_KEY_FILE, or
//     ~/.config/teploy/age.txt (first that exists).
//   - `*.sops.*` / `*.enc.*`      — SOPS-encrypted (any age/KMS backend
//     configured in .sops.yaml); decrypted via `sops -d`. dotenv, YAML, and
//     JSON payloads are supported — YAML/JSON contribute their top-level
//     scalar keys.
//   - anything else              — plain dotenv, read as-is.
func LoadLocalEnvFiles(dir string, paths []string) (map[string]string, error) {
	merged := make(map[string]string)
	for _, p := range paths {
		full := p
		if !filepath.IsAbs(full) {
			full = filepath.Join(dir, p)
		}
		var (
			content string
			err     error
		)
		switch {
		case strings.HasSuffix(full, ".age"):
			content, err = decryptAgeFile(full)
		case isSopsName(full):
			content, err = decryptSopsFile(full)
		default:
			var raw []byte
			raw, err = os.ReadFile(full)
			content = string(raw)
		}
		if err != nil {
			return nil, fmt.Errorf("env file %s: %w", p, err)
		}
		vars, err := parseEnvContent(full, content)
		if err != nil {
			return nil, fmt.Errorf("env file %s: %w", p, err)
		}
		for k, v := range vars {
			merged[k] = v
		}
	}
	return merged, nil
}

func isSopsName(path string) bool {
	base := filepath.Base(path)
	return strings.Contains(base, ".sops.") || strings.Contains(base, ".enc.")
}

// ageIdentityFile resolves the age identity (private key) file to decrypt
// with. Explicit env vars first, then the teploy-conventional location.
func ageIdentityFile() (string, error) {
	for _, envVar := range []string{"TEPLOY_AGE_IDENTITY", "SOPS_AGE_KEY_FILE"} {
		if p := os.Getenv(envVar); p != "" {
			return p, nil
		}
	}
	home, err := os.UserHomeDir()
	if err == nil {
		conventional := filepath.Join(home, ".config", "teploy", "age.txt")
		if _, statErr := os.Stat(conventional); statErr == nil {
			return conventional, nil
		}
	}
	return "", fmt.Errorf("no age identity found — set TEPLOY_AGE_IDENTITY (or SOPS_AGE_KEY_FILE), or put the key at ~/.config/teploy/age.txt")
}

func decryptAgeFile(path string) (string, error) {
	if _, err := exec.LookPath("age"); err != nil {
		return "", fmt.Errorf("`age` binary not found — install age (https://age-encryption.org) to use .age env files")
	}
	identity, err := ageIdentityFile()
	if err != nil {
		return "", err
	}
	out, err := exec.Command("age", "-d", "-i", identity, path).Output()
	if err != nil {
		return "", fmt.Errorf("age decrypt failed: %w%s", err, stderrOf(err))
	}
	return string(out), nil
}

func decryptSopsFile(path string) (string, error) {
	if _, err := exec.LookPath("sops"); err != nil {
		return "", fmt.Errorf("`sops` binary not found — install sops (https://github.com/getsops/sops) to use SOPS env files")
	}
	out, err := exec.Command("sops", "-d", path).Output()
	if err != nil {
		return "", fmt.Errorf("sops decrypt failed: %w%s", err, stderrOf(err))
	}
	return string(out), nil
}

func stderrOf(err error) string {
	var ee *exec.ExitError
	if errors.As(err, &ee) && len(ee.Stderr) > 0 {
		return ": " + strings.TrimSpace(string(ee.Stderr))
	}
	return ""
}

// parseEnvContent interprets decrypted payloads: YAML/JSON files contribute
// their top-level scalar keys; everything else is parsed as dotenv.
func parseEnvContent(path, content string) (map[string]string, error) {
	name := strings.ToLower(filepath.Base(path))
	// Strip encryption suffixes to find the underlying format:
	// secrets.yaml.age -> secrets.yaml, secrets.sops.json -> secrets.json.
	name = strings.TrimSuffix(name, ".age")
	name = strings.ReplaceAll(name, ".sops.", ".")
	name = strings.ReplaceAll(name, ".enc.", ".")
	if strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml") || strings.HasSuffix(name, ".json") {
		var doc map[string]any
		if err := yaml.Unmarshal([]byte(content), &doc); err != nil {
			return nil, fmt.Errorf("parsing %s: %w", filepath.Base(path), err)
		}
		vars := make(map[string]string, len(doc))
		for k, v := range doc {
			switch val := v.(type) {
			case string:
				vars[k] = val
			case int, int64, float64, bool:
				vars[k] = fmt.Sprintf("%v", val)
			default:
				// Nested structures (and SOPS's own `sops:` metadata block,
				// which survives in the plaintext of some formats) are not
				// env material.
			}
		}
		delete(vars, "sops")
		return vars, nil
	}
	return parseEnv(content), nil
}
