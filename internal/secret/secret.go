package secret

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/useteploy/teploy/internal/ssh"
)

const (
	deploymentsDir = "/deployments"
	ageKeyPath     = deploymentsDir + "/.age-key"
)

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

// ensureKey creates an age keypair on the server if one doesn't exist.
// Returns the path to the key file.
func (m *Manager) ensureKey(ctx context.Context) (string, error) {
	if _, err := m.exec.Run(ctx, fmt.Sprintf("test -f %s", ageKeyPath)); err == nil {
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
func (m *Manager) Set(ctx context.Context, app, key, value string) error {
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
	if _, err := m.exec.Run(ctx, "mkdir -p "+dir); err != nil {
		return fmt.Errorf("creating secrets directory: %w", err)
	}

	path := secretPath(app, key)
	// Single-quote every interpolated value so the remote shell can't expand or
	// execute it. echo %q wraps in DOUBLE quotes, under which $, backticks and
	// backslashes are still interpreted — so a secret like $(...) or pa$word was
	// executed/corrupted at set-time as the SSH user. printf '%s' emits the value
	// literally (no echo escape processing), and ssh.ShellQuote blocks expansion.
	cmd := fmt.Sprintf("printf '%%s' %s | age -r %s -o %s",
		ssh.ShellQuote(value), ssh.ShellQuote(recipient), ssh.ShellQuote(path))
	if _, err := m.exec.Run(ctx, cmd); err != nil {
		return fmt.Errorf("encrypting secret %s: %w", key, err)
	}
	return nil
}

// Get decrypts and returns a secret value.
func (m *Manager) Get(ctx context.Context, app, key string) (string, error) {
	path := secretPath(app, key)
	if _, err := m.exec.Run(ctx, fmt.Sprintf("test -f %s", path)); err != nil {
		return "", fmt.Errorf("secret %s is not set", key)
	}

	cmd := fmt.Sprintf("age -d -i %s %s", ageKeyPath, path)
	out, err := m.exec.Run(ctx, cmd)
	if err != nil {
		return "", fmt.Errorf("decrypting secret %s: %w", key, err)
	}
	return strings.TrimSpace(out), nil
}

// List returns all secret key names for the app.
func (m *Manager) List(ctx context.Context, app string) ([]string, error) {
	dir := secretDir(app)
	out, err := m.exec.Run(ctx, fmt.Sprintf("ls %s/*.age 2>/dev/null", dir))
	if err != nil || strings.TrimSpace(out) == "" {
		return nil, nil
	}

	var keys []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Extract key name from path: /deployments/<app>/secrets/KEY.age -> KEY
		name := line
		if idx := strings.LastIndex(name, "/"); idx >= 0 {
			name = name[idx+1:]
		}
		name = strings.TrimSuffix(name, ".age")
		keys = append(keys, name)
	}
	return keys, nil
}

// Rotate generates a new random value for a key and re-encrypts it.
// Returns the new value.
func (m *Manager) Rotate(ctx context.Context, app, key string) (string, error) {
	// Verify key exists.
	path := secretPath(app, key)
	if _, err := m.exec.Run(ctx, fmt.Sprintf("test -f %s", path)); err != nil {
		return "", fmt.Errorf("secret %s does not exist — cannot rotate", key)
	}

	newValue := randomHex(32)
	if err := m.Set(ctx, app, key, newValue); err != nil {
		return "", fmt.Errorf("rotating secret %s: %w", key, err)
	}
	return newValue, nil
}

// DecryptAll returns all secrets as a map for injection into container env.
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
		val, err := m.Get(ctx, app, key)
		if err != nil {
			return nil, err
		}
		result[key] = val
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
