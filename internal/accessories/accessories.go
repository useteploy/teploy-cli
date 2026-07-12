package accessories

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/docker"
	"github.com/useteploy/teploy/internal/secret"
	"github.com/useteploy/teploy/internal/ssh"
)

const deploymentsDir = "/deployments"

// Manager manages accessory containers (databases, caches) for an app.
type Manager struct {
	exec   ssh.Executor
	docker *docker.Client
	out    io.Writer
}

// NewManager creates a new accessories manager.
func NewManager(exec ssh.Executor, out io.Writer) *Manager {
	return &Manager{
		exec:   exec,
		docker: docker.NewClient(exec),
		out:    out,
	}
}

// ContainerName returns the accessory container name: {app}-{name}.
func ContainerName(app, name string) string {
	return app + "-" + name
}

// EnsureRunning checks if an accessory is running and starts it if not.
// Returns env vars to inject into the app (e.g., DATABASE_URL for postgres).
func (m *Manager) EnsureRunning(ctx context.Context, app, name string, cfg config.AccessoryConfig) (map[string]string, error) {
	containerName := ContainerName(app, name)

	// Always resolve env (needed for connection strings even if already running).
	env, err := m.resolveEnv(ctx, app, name, cfg.Env)
	if err != nil {
		return nil, fmt.Errorf("resolving env vars: %w", err)
	}

	// Check if already running.
	status, err := m.exec.Run(ctx, fmt.Sprintf(
		"docker inspect -f '{{.State.Status}}' %s 2>/dev/null", ssh.ShellQuote(containerName),
	))
	if err == nil && strings.TrimSpace(status) == "running" {
		fmt.Fprintf(m.out, "  %s already running\n", containerName)
		return connectionEnvVars(app, name, cfg.Image, cfg.Port, env), nil
	}

	// Ensure directory structure.
	accDir := fmt.Sprintf("%s/%s/accessories/%s", deploymentsDir, app, name)
	if _, err := m.exec.Run(ctx, fmt.Sprintf("mkdir -p %s", accDir)); err != nil {
		return nil, fmt.Errorf("creating accessory directory: %w", err)
	}

	// Build volumes: /deployments/{app}/accessories/{name}/{key} -> container_path.
	volumes := make(map[string]string)
	for volName, containerPath := range cfg.Volumes {
		hostPath := fmt.Sprintf("%s/%s", accDir, volName)
		volumes[hostPath] = containerPath
	}

	// Build docker run command.
	fmt.Fprintf(m.out, "  Starting %s...\n", containerName)
	args := []string{
		"docker", "run", "--detach",
		"--restart", "always",
		"--name", ssh.ShellQuote(containerName),
		"--network", "teploy",
		"--network-alias", ssh.ShellQuote(containerName),
		"--label", "teploy.app=" + app,
		"--label", "teploy.role=accessory",
		"--label", "teploy.accessory=" + name,
	}

	if len(env) > 0 {
		for _, k := range sortedKeys(env) {
			args = append(args, "-e", ssh.ShellQuote(k+"="+env[k]))
		}
	}

	if len(volumes) > 0 {
		for _, k := range sortedKeys(volumes) {
			args = append(args, "-v", ssh.ShellQuote(k+":"+volumes[k]))
		}
	}

	// Host port mappings must precede the image (docker run flags).
	for _, pub := range cfg.Publish {
		args = append(args, "-p", ssh.ShellQuote(pub))
	}

	args = append(args, "--log-opt", "max-size=10m")
	args = append(args, ssh.ShellQuote(cfg.Image))

	// Image command override (docker run trailing args), word-split like a
	// shell command line and each word quoted for the remote shell.
	if cfg.Command != "" {
		for _, w := range strings.Fields(cfg.Command) {
			args = append(args, ssh.ShellQuote(w))
		}
	}

	if _, err := m.exec.Run(ctx, strings.Join(args, " ")); err != nil {
		return nil, fmt.Errorf("starting accessory %s: %w", containerName, err)
	}

	fmt.Fprintf(m.out, "  %s started\n", containerName)
	return connectionEnvVars(app, name, cfg.Image, cfg.Port, env), nil
}

// resolveEnv processes env vars, replacing "auto" values with generated
// passwords and "secret:KEY" references with values decrypted from the app's
// encrypted secret store (`teploy secret set KEY=value`).
//
// Generated ("auto") credentials are persisted to
// /deployments/{app}/accessories/{name}/credentials. Secret references are
// NOT persisted anywhere in plaintext — the age-encrypted store stays the
// single source of truth, and because app deploys inject the same store into
// the app container, one `teploy secret set` feeds both sides (e.g. a
// database password the accessory sets and the app connects with).
func (m *Manager) resolveEnv(ctx context.Context, app, name string, env map[string]string) (map[string]string, error) {
	if len(env) == 0 {
		return nil, nil
	}

	credPath := fmt.Sprintf("%s/%s/accessories/%s/credentials", deploymentsDir, app, name)
	stored := m.loadCredentials(ctx, credPath)

	result := make(map[string]string)
	needsWrite := false

	var secrets *secret.Manager
	for k, v := range env {
		switch {
		case v == "auto":
			if existing, ok := stored[k]; ok {
				result[k] = existing
			} else {
				password, err := generatePassword()
				if err != nil {
					return nil, fmt.Errorf("generating password for %s: %w", k, err)
				}
				result[k] = password
				stored[k] = password
				needsWrite = true
			}
		case strings.HasPrefix(v, "secret:"):
			key := strings.TrimSpace(strings.TrimPrefix(v, "secret:"))
			if key == "" {
				return nil, fmt.Errorf("accessory %s env %s: empty secret reference (expected secret:KEY)", name, k)
			}
			if secrets == nil {
				secrets = secret.NewManager(m.exec)
			}
			val, err := secrets.Get(ctx, app, key)
			if err != nil {
				return nil, fmt.Errorf("accessory %s env %s: %w — set it with: teploy secret set %s=<value>", name, k, err, key)
			}
			result[k] = val
		default:
			result[k] = v
		}
	}

	if needsWrite {
		if err := m.writeCredentials(ctx, credPath, stored); err != nil {
			return nil, err
		}
	}

	return result, nil
}

func (m *Manager) loadCredentials(ctx context.Context, path string) map[string]string {
	creds := make(map[string]string)
	output, err := m.exec.Run(ctx, fmt.Sprintf("cat %s 2>/dev/null", path))
	if err != nil || strings.TrimSpace(output) == "" {
		return creds
	}
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 {
			creds[parts[0]] = parts[1]
		}
	}
	return creds
}

func (m *Manager) writeCredentials(ctx context.Context, path string, creds map[string]string) error {
	var b strings.Builder
	for _, k := range sortedKeys(creds) {
		fmt.Fprintf(&b, "%s=%s\n", k, creds[k])
	}
	return m.exec.Upload(ctx, strings.NewReader(b.String()), path, "0600")
}

// InjectEnvVars writes accessory-generated env vars to /deployments/{app}/.env.
// Existing keys are never overwritten — user values always win.
func (m *Manager) InjectEnvVars(ctx context.Context, app string, vars map[string]string) error {
	if len(vars) == 0 {
		return nil
	}

	envPath := fmt.Sprintf("%s/%s/.env", deploymentsDir, app)

	// Read existing .env to find keys already set.
	existing := make(map[string]bool)
	output, _ := m.exec.Run(ctx, fmt.Sprintf("cat %s 2>/dev/null", envPath))
	if output != "" {
		for _, line := range strings.Split(output, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			parts := strings.SplitN(line, "=", 2)
			if len(parts) >= 1 {
				existing[parts[0]] = true
			}
		}
	}

	// Collect vars to add (skip existing keys).
	var toAdd strings.Builder
	for _, k := range sortedKeys(vars) {
		if !existing[k] {
			fmt.Fprintf(&toAdd, "%s=%s\n", k, vars[k])
		}
	}

	if toAdd.Len() == 0 {
		return nil
	}

	// Create or append to .env file.
	if output == "" {
		return m.exec.Upload(ctx, strings.NewReader(toAdd.String()), envPath, "0600")
	}

	tmpPath := "/tmp/teploy_env_append"
	if err := m.exec.Upload(ctx, strings.NewReader(toAdd.String()), tmpPath, "0600"); err != nil {
		return err
	}
	_, err := m.exec.Run(ctx, fmt.Sprintf("cat %s >> %s && rm -f %s", tmpPath, envPath, tmpPath))
	return err
}

// List returns all accessory containers for an app.
func (m *Manager) List(ctx context.Context, app string) ([]docker.Container, error) {
	cmd := fmt.Sprintf(
		"docker ps --all --filter label=teploy.app=%s --filter label=teploy.role=accessory --format '{{json .}}'",
		ssh.ShellQuote(app),
	)
	output, err := m.exec.Run(ctx, cmd)
	if err != nil {
		return nil, fmt.Errorf("listing accessories: %w", err)
	}
	output = strings.TrimSpace(output)
	if output == "" {
		return nil, nil
	}
	return docker.ParseContainers(output)
}

// Stop stops an accessory container.
func (m *Manager) Stop(ctx context.Context, app, name string) error {
	return m.docker.Stop(ctx, ContainerName(app, name), 10)
}

// Start starts a stopped accessory container.
func (m *Manager) Start(ctx context.Context, app, name string) error {
	return m.docker.Start(ctx, ContainerName(app, name))
}

// Logs streams accessory container logs.
func (m *Manager) Logs(ctx context.Context, app, name string, lines int) error {
	containerName := ContainerName(app, name)
	cmd := fmt.Sprintf("docker logs --tail %d %s 2>&1", lines, ssh.ShellQuote(containerName))
	return m.exec.RunStream(ctx, cmd, m.out, m.out)
}

// Upgrade stops the old container, pulls the new image, and starts a new one with same config.
func (m *Manager) Upgrade(ctx context.Context, app, name, newImage string, cfg config.AccessoryConfig) error {
	containerName := ContainerName(app, name)

	fmt.Fprintf(m.out, "Pulling %s...\n", newImage)
	if err := m.docker.Pull(ctx, newImage); err != nil {
		return err
	}

	fmt.Fprintf(m.out, "Stopping %s...\n", containerName)
	m.docker.Stop(ctx, containerName, 10)
	m.docker.Remove(ctx, containerName)

	cfg.Image = newImage
	_, err := m.EnsureRunning(ctx, app, name, cfg)
	return err
}

// connectionEnvVars generates app env vars based on the accessory image type.
// For example, a postgres accessory generates DATABASE_URL.
func connectionEnvVars(app, name, image string, port int, env map[string]string) map[string]string {
	vars := make(map[string]string)
	alias := ContainerName(app, name)

	switch {
	case isImageType(image, "postgres"):
		password := env["POSTGRES_PASSWORD"]
		db := env["POSTGRES_DB"]
		if db == "" {
			db = app
		}
		user := env["POSTGRES_USER"]
		if user == "" {
			user = "postgres"
		}
		if port == 0 {
			port = 5432
		}
		vars["DATABASE_URL"] = fmt.Sprintf("postgres://%s:%s@%s:%d/%s", user, password, alias, port, db)

	case isImageType(image, "mysql"), isImageType(image, "mariadb"):
		password := env["MYSQL_ROOT_PASSWORD"]
		db := env["MYSQL_DATABASE"]
		if db == "" {
			db = app
		}
		if port == 0 {
			port = 3306
		}
		vars["DATABASE_URL"] = fmt.Sprintf("mysql://root:%s@%s:%d/%s", password, alias, port, db)

	case isImageType(image, "redis"):
		if port == 0 {
			port = 6379
		}
		vars["REDIS_URL"] = fmt.Sprintf("redis://%s:%d", alias, port)

	case isImageType(image, "mongo"):
		if port == 0 {
			port = 27017
		}
		vars["MONGODB_URL"] = fmt.Sprintf("mongodb://%s:%d", alias, port)
	}

	return vars
}

// isImageType checks if a Docker image name matches a service type.
// Handles both "postgres:16" and "library/postgres:16" formats.
func isImageType(image, serviceType string) bool {
	image = strings.Split(image, ":")[0]
	return image == serviceType || strings.HasSuffix(image, "/"+serviceType)
}

func generatePassword() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
