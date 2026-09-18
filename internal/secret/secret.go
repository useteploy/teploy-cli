package secret

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/useteploy/teploy/internal/ssh"
)

const (
	deploymentsDir = "/deployments"
	ageKeyPath     = deploymentsDir + "/.age-key"
)

// ErrNotFound reports a secret key that is confirmed absent from the store.
// It is the ONLY absence signal: every other read failure is a real error,
// so callers can distinguish "never set" (generate/create is appropriate)
// from "could not read" (must fail, never regenerate over existing data).
var ErrNotFound = errors.New("secret not found")

// managementSecretKeys are age-store keys that hold Teploy's own vault
// administration material (OpenBao root token, recovery/unseal keys). They
// live in the same per-app store as ordinary application secrets, but they
// must NEVER be injected into application containers: any workload that can
// read them owns the vault. DecryptAll filters them (see there); the
// long-term fix is a separate management namespace application enumeration
// cannot traverse (deferred — see AUDIT_OPEN.md F01).
var managementSecretKeys = map[string]bool{
	"VAULT_ROOT_TOKEN":    true,
	"VAULT_RECOVERY_KEYS": true,
	"VAULT_SEAL_KEY":      true,
	"VAULT_SEAL_KEY_ID":   true,
}

// IsManagementKey reports whether key is Teploy vault administration
// material rather than an application secret.
func IsManagementKey(key string) bool {
	return managementSecretKeys[key]
}

// validSecretKey constrains secret keys to a grammar that is safe as a path
// segment and unambiguous in env-file serialization: no path separators,
// control bytes, whitespace, or shell/Caddy metacharacters, and no leading
// dash or dot (which would make the value look like a flag or a hidden file
// in every `key.age` path operation).
var validSecretKey = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]*$`)

// ValidateKey checks a secret key against the store's key grammar.
func ValidateKey(key string) error {
	if key == "" {
		return fmt.Errorf("secret key must not be empty")
	}
	if len(key) > 255 {
		return fmt.Errorf("secret key too long (max 255 chars, got %d)", len(key))
	}
	if !validSecretKey.MatchString(key) {
		return fmt.Errorf("invalid secret key %q — letters, digits, underscore, hyphen and dot only; must not start with a dash or dot", key)
	}
	return nil
}

// Manager handles encrypted secrets on the server using age.
// Secrets are stored at /deployments/<app>/secrets/<KEY>.age.
// An age keypair is generated on first use and stored at /deployments/.age-key.
type Manager struct {
	exec ssh.Executor
}

// NewManager creates a secret manager backed by the given SSH executor.
func NewManager(exec ssh.Executor) *Manager {
	return &Manager{exec: exec}
}

// EnsureAge checks if age is installed on the server; installs if missing.
func (m *Manager) EnsureAge(ctx context.Context) error {
	if _, err := m.exec.Run(ctx, "which age"); err == nil {
		return nil
	}
	// Install age via package manager. Use sudo only if not root.
	prefix := "sudo "
	if id, err := m.exec.Run(ctx, "id -u"); err == nil && strings.TrimSpace(id) == "0" {
		prefix = ""
	}
	_, err := m.exec.Run(ctx, fmt.Sprintf("%sDEBIAN_FRONTEND=noninteractive apt-get update -qq && %sDEBIAN_FRONTEND=noninteractive apt-get install -y -qq age 2>/dev/null || %syum install -y age 2>/dev/null", prefix, prefix, prefix))
	if err != nil {
		return fmt.Errorf("installing age: %w", err)
	}
	// Verify install succeeded.
	if _, err := m.exec.Run(ctx, "which age"); err != nil {
		return fmt.Errorf("age installation failed — install manually: apt install age")
	}
	return nil
}

// remoteFileExists confirms a path's existence WITHOUT folding transport
// and permission failures into "missing" (TCL-29): the command always
// exits 0 and prints absent/present, so a non-zero exit or unrecognized
// output is a real error, never an absence signal.
func remoteFileExists(ctx context.Context, exec ssh.Executor, path string) (bool, error) {
	out, err := exec.Run(ctx, fmt.Sprintf("if [ ! -e %s ]; then printf 'absent\\n'; else printf 'present\\n'; fi", ssh.ShellQuote(path)))
	if err != nil {
		return false, fmt.Errorf("checking %s: %w", path, err)
	}
	switch strings.TrimSpace(out) {
	case "absent":
		return false, nil
	case "present":
		return true, nil
	}
	return false, fmt.Errorf("checking %s: unrecognized output framing", path)
}

// ensureKey creates an age keypair on the server if one doesn't exist.
// Returns the path to the key file.
func (m *Manager) ensureKey(ctx context.Context) (string, error) {
	exists, err := remoteFileExists(ctx, m.exec, ageKeyPath)
	if err != nil {
		return "", err
	}
	if exists {
		return ageKeyPath, nil
	}

	// Generate keypair.
	if _, err := m.exec.Run(ctx, fmt.Sprintf("age-keygen -o %s 2>/dev/null && chmod 600 %s", ageKeyPath, ageKeyPath)); err != nil {
		return "", fmt.Errorf("generating age key: %w", err)
	}
	return ageKeyPath, nil
}

// recipient extracts the public key (recipient) from the age key file.
func (m *Manager) recipient(ctx context.Context) (string, error) {
	out, err := m.exec.Run(ctx, fmt.Sprintf("grep 'public key:' %s | awk '{print $NF}'", ageKeyPath))
	if err != nil {
		return "", fmt.Errorf("reading age public key: %w", err)
	}
	r := strings.TrimSpace(out)
	if r == "" {
		return "", fmt.Errorf("age public key is empty")
	}
	return r, nil
}

func secretDir(app string) string {
	return fmt.Sprintf("%s/%s/secrets", deploymentsDir, app)
}

func secretPath(app, key string) string {
	return fmt.Sprintf("%s/%s.age", secretDir(app), key)
}

// Set encrypts a value and stores it on the server.
//
// The plaintext is streamed to the remote age process over stdin and never
// appears in the SSH command string (where it would sit in the host's
// process list and in command-bearing error messages), and the ciphertext is
// written to a private temporary sibling before an atomic rename — an
// interrupted or failed encryption can no longer truncate the previous
// ciphertext under the same path.
func (m *Manager) Set(ctx context.Context, app, key, value string) error {
	if err := ValidateKey(key); err != nil {
		return err
	}
	if err := m.EnsureAge(ctx); err != nil {
		return err
	}
	if _, err := m.ensureKey(ctx); err != nil {
		return err
	}

	recipient, err := m.recipient(ctx)
	if err != nil {
		return err
	}

	dir := secretDir(app)
	if _, err := m.exec.Run(ctx, "mkdir -p "+ssh.ShellQuote(dir)); err != nil {
		return fmt.Errorf("creating secrets directory: %w", err)
	}

	path := secretPath(app, key)
	// age reads the plaintext from stdin and writes ciphertext to the
	// temporary file; only after a successful encryption is it renamed over
	// the destination.
	script := fmt.Sprintf(
		`umask 077 && tmp=$(mktemp %s) && trap 'rm -f -- "$tmp"' EXIT HUP INT TERM && age -r %s -o "$tmp" && chmod 0600 "$tmp" && mv -f -- "$tmp" %s && trap - EXIT HUP INT TERM`,
		ssh.ShellQuote(dir+"/.teploy-secret.XXXXXXXX"),
		ssh.ShellQuote(recipient),
		ssh.ShellQuote(path),
	)
	if err := m.exec.RunInput(ctx, script, strings.NewReader(value)); err != nil {
		return fmt.Errorf("encrypting secret %s: %w", key, err)
	}
	return nil
}

// Get decrypts and returns a secret value. The decrypted bytes are returned
// exactly as stored — values that intentionally begin or end with
// whitespace (or contain interior blank lines) survive the round trip. Only
// a confirmed-absent key yields ErrNotFound; transport and decrypt failures
// are errors.
func (m *Manager) Get(ctx context.Context, app, key string) (string, error) {
	if err := ValidateKey(key); err != nil {
		return "", err
	}
	path := secretPath(app, key)
	exists, err := remoteFileExists(ctx, m.exec, path)
	if err != nil {
		return "", err
	}
	if !exists {
		return "", fmt.Errorf("%w: %s", ErrNotFound, key)
	}

	// RunStream (not Run) so the executor's output trimming cannot silently
	// alter the plaintext. stdout and stderr are captured SEPARATELY: the
	// old single-buffer form let any age warning on stderr contaminate the
	// secret value itself (TCL-29); diagnostics surface only in the error.
	var out, diag bytes.Buffer
	cmd := fmt.Sprintf("age -d -i %s %s", ageKeyPath, ssh.ShellQuote(path))
	if err := m.exec.RunStream(ctx, cmd, &out, &diag); err != nil {
		return "", fmt.Errorf("decrypting secret %s: %w%s", key, err, diagSuffix(diag.String()))
	}
	return out.String(), nil
}

// diagSuffix appends trimmed stderr output to an error message when present.
func diagSuffix(diag string) string {
	diag = strings.TrimSpace(diag)
	if diag == "" {
		return ""
	}
	return ": " + diag
}

// List returns all secret key names for the app. An absent secrets directory
// is the normal "no secrets" case (nil, nil); a directory that exists but
// cannot be listed is an error — silently treating it as empty would let a
// deployment proceed without secrets it actually has.
func (m *Manager) List(ctx context.Context, app string) ([]string, error) {
	dir := secretDir(app)
	exists, err := remoteFileExists(ctx, m.exec, dir)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}
	out, err := m.exec.Run(ctx, fmt.Sprintf("find %s -maxdepth 1 -name '*.age' -printf '%%f\\n' | sort", ssh.ShellQuote(dir)))
	if err != nil {
		return nil, fmt.Errorf("listing secrets for %s: %w", app, err)
	}

	var keys []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		name := strings.TrimSuffix(line, ".age")
		keys = append(keys, name)
	}
	return keys, nil
}

// Remove deletes a secret from the server. The bool reports whether a secret
// was actually there to delete, so callers can tell "removed" from "was never
// set" — deleting a revoked credential that turns out not to exist is worth
// saying out loud rather than reporting a removal that did nothing.
func (m *Manager) Remove(ctx context.Context, app, key string) (bool, error) {
	if err := ValidateKey(key); err != nil {
		return false, err
	}
	path := secretPath(app, key)
	exists, err := remoteFileExists(ctx, m.exec, path)
	if err != nil {
		return false, err
	}
	if !exists {
		return false, nil
	}
	if _, err := m.exec.Run(ctx, "rm -f "+ssh.ShellQuote(path)); err != nil {
		return false, fmt.Errorf("removing secret %s: %w", key, err)
	}
	return true, nil
}

// Rotate generates a new random value for a key and re-encrypts it.
// Returns the new value.
func (m *Manager) Rotate(ctx context.Context, app, key string) (string, error) {
	exists, err := remoteFileExists(ctx, m.exec, secretPath(app, key))
	if err != nil {
		return "", err
	}
	if !exists {
		return "", fmt.Errorf("secret %s does not exist — cannot rotate: %w", key, ErrNotFound)
	}

	newValue := randomHex(32)
	if err := m.Set(ctx, app, key, newValue); err != nil {
		return "", fmt.Errorf("rotating secret %s: %w", key, err)
	}
	return newValue, nil
}

// DecryptAll returns the app's application secrets as a map for injection
// into container env. Vault administration material (see
// managementSecretKeys) is deliberately excluded: it shares the store but
// must never reach a workload — an app that can read VAULT_ROOT_TOKEN owns
// the whole vault.
func (m *Manager) DecryptAll(ctx context.Context, app string) (map[string]string, error) {
	keys, err := m.List(ctx, app)
	if err != nil {
		return nil, err
	}
	if len(keys) == 0 {
		return nil, nil
	}

	result := make(map[string]string, len(keys))
	for _, key := range keys {
		if IsManagementKey(key) {
			continue
		}
		val, err := m.Get(ctx, app, key)
		if err != nil {
			return nil, err
		}
		result[key] = val
	}
	if len(result) == 0 {
		return nil, nil
	}
	return result, nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)
}
