package accessories

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/url"
	"sort"
	"strconv"
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
		m.warnResourceDrift(ctx, name, containerName, cfg)
		return connectionEnvVars(app, name, cfg.Image, cfg.Port, env), nil
	}

	// The container exists but is not running — stopped by an operator, exited,
	// or crash-looping. `docker run` below would fail on the name conflict with
	// a raw daemon error that says nothing about accessories, and the whole
	// deploy would abort. Remove the dead container and recreate it from the
	// config, which is the authoritative description of how it should run.
	//
	// Non-destructive: an accessory's state lives in bind mounts under
	// /deployments/{app}/accessories/{name}, not in the container's writable
	// layer, so the data outlives the container. Recreating is also what lets a
	// changed memory/cpu limit take effect at all.
	if err == nil && strings.TrimSpace(status) != "" {
		fmt.Fprintf(m.out, "  %s exists but is %s — recreating from config\n", containerName, strings.TrimSpace(status))
		if _, rmErr := m.exec.Run(ctx, fmt.Sprintf("docker rm -f %s", ssh.ShellQuote(containerName))); rmErr != nil {
			return nil, fmt.Errorf("removing stopped accessory %s before recreate: %w", containerName, rmErr)
		}
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

	// Create the volume directories here and hand them to the UID the image
	// runs as. Left to docker, a missing bind-mount source is created
	// root-owned, and an image that drops privileges (Nucleus runs as 10001)
	// crash-loops on its very first start with a permission error from inside
	// the engine. That is the upgrade failure reconcileDataOwnership already
	// covers, hit on a fresh install by every deploy — `template install`
	// included.
	for _, hostPath := range sortedKeys(volumes) {
		if _, err := m.exec.Run(ctx, "mkdir -p "+hostPath); err != nil {
			return nil, fmt.Errorf("creating accessory volume directory %s: %w", hostPath, err)
		}
	}
	if err := m.reconcileDataOwnership(ctx, app, name, cfg); err != nil {
		return nil, err
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

	// Env rides a 0600 env-file under the accessory's own directory rather
	// than `-e KEY=value` argv: resolved credentials (generated passwords,
	// secret: references) would otherwise sit in the host's process list
	// for the life of the docker run (audit F22) — same reasoning as
	// openbao's seal-env and the app deploy env file.
	if len(env) > 0 {
		keys := sortedKeys(env)
		var b strings.Builder
		for _, k := range keys {
			fmt.Fprintf(&b, "%s=%s\n", k, env[k])
		}
		envFile := accDir + "/container.env"
		if err := m.exec.Upload(ctx, strings.NewReader(b.String()), envFile, "0600"); err != nil {
			return nil, fmt.Errorf("writing accessory env file: %w", err)
		}
		args = append(args, "--env-file", ssh.ShellQuote(envFile))
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

	// Resource limits. A cgroup cap is the only enforced bound on an
	// accessory's memory — an engine's own budget setting is advisory.
	if cfg.Memory != "" {
		args = append(args, "--memory", ssh.ShellQuote(cfg.Memory))
	}
	if cfg.CPU != "" {
		args = append(args, "--cpus", ssh.ShellQuote(cfg.CPU))
	}

	args = append(args, "--log-opt", "max-size=10m")
	args = append(args, ssh.ShellQuote(cfg.Image))

	// Image command override (docker run trailing args). CommandArgs is
	// the argv form and is quoted element-wise; the legacy string Command
	// is word-split (documented limitation — strings.Fields cannot express
	// an argument containing spaces; audit F72).
	if len(cfg.CommandArgs) > 0 {
		for _, w := range cfg.CommandArgs {
			args = append(args, ssh.ShellQuote(w))
		}
	} else if cfg.Command != "" {
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

// warnResourceDrift reports a memory/cpu limit in teploy.yml that the RUNNING
// container does not have.
//
// Docker fixes resource limits at container creation, and an already-running
// accessory is left alone by design (recreating a database because a config
// value moved is exactly the kind of surprise teploy exists to avoid). But
// silently ignoring the setting is worse than either choice: the operator adds
// `memory: 8g` to bound a leaky engine, teploy prints nothing, and the cap they
// think is protecting the host does not exist. Say so, and name the command
// that applies it.
//
// Best-effort: a failed inspect is not worth failing a deploy over.
func (m *Manager) warnResourceDrift(ctx context.Context, name, containerName string, cfg config.AccessoryConfig) {
	if cfg.Memory == "" && cfg.CPU == "" {
		return
	}
	out, err := m.exec.Run(ctx, fmt.Sprintf(
		"docker inspect -f '{{.HostConfig.Memory}} {{.HostConfig.NanoCpus}}' %s 2>/dev/null",
		ssh.ShellQuote(containerName),
	))
	if err != nil {
		return
	}
	fields := strings.Fields(strings.TrimSpace(out))
	if len(fields) != 2 {
		return
	}
	if cfg.Memory != "" && fields[0] == "0" {
		fmt.Fprintf(m.out, "  WARNING: %s has no memory limit, but teploy.yml sets memory: %s\n", containerName, cfg.Memory)
	}
	if cfg.CPU != "" && fields[1] == "0" {
		fmt.Fprintf(m.out, "  WARNING: %s has no cpu limit, but teploy.yml sets cpu: %s\n", containerName, cfg.CPU)
	}
	if (cfg.Memory != "" && fields[0] == "0") || (cfg.CPU != "" && fields[1] == "0") {
		fmt.Fprintf(m.out, "           Limits apply at container creation. To apply them now (data is on a\n")
		fmt.Fprintf(m.out, "           bind mount and survives; expect brief downtime for this accessory):\n")
		fmt.Fprintf(m.out, "             teploy accessory stop %s && teploy deploy\n", name)
	}
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
	stored, err := m.loadCredentials(ctx, credPath)
	if err != nil {
		// A failed credential read must not silently become an empty map:
		// resolveEnv would then REGENERATE 'auto' passwords over a database
		// that already exists (audit F69).
		return nil, fmt.Errorf("reading stored credentials for %s (refusing to regenerate over possibly-existing values): %w", name, err)
	}

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

// loadCredentials returns the stored credential map. Only a confirmed
// missing file yields an empty map; an existing-but-unreadable file is an
// error (audit F69).
func (m *Manager) loadCredentials(ctx context.Context, path string) (map[string]string, error) {
	creds := make(map[string]string)
	if _, statErr := m.exec.Run(ctx, "test -f "+ssh.ShellQuote(path)); statErr != nil {
		return creds, nil
	}
	output, err := m.exec.Run(ctx, "cat -- "+ssh.ShellQuote(path))
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
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
	return creds, nil
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
//
// The staging file is per-APP and private (/deployments/<app>/.env.append,
// mode 0600): the old global /tmp/teploy_env_append was shared by every
// concurrent deployment, so app B's connection credentials could land in
// app A's env between A's upload and append (audit F73). A read failure of
// the EXISTING .env aborts instead of being treated as an empty file.
func (m *Manager) InjectEnvVars(ctx context.Context, app string, vars map[string]string) error {
	if len(vars) == 0 {
		return nil
	}

	envPath := fmt.Sprintf("%s/%s/.env", deploymentsDir, app)

	// Read existing .env to find keys already set. A missing file is the
	// normal first-run case; a file that exists but cannot be read must
	// abort — treating it as empty used to let the write below replace the
	// app's env wholesale (audit F73).
	var output string
	if _, statErr := m.exec.Run(ctx, "test -f "+ssh.ShellQuote(envPath)); statErr == nil {
		out, catErr := m.exec.Run(ctx, "cat -- "+ssh.ShellQuote(envPath))
		if catErr != nil {
			return fmt.Errorf("reading existing %s (refusing to overwrite it as though empty): %w", envPath, catErr)
		}
		output = out
	}
	existing := make(map[string]bool)
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

	// Collect vars to add (skip existing keys), preserving the existing
	// file's content byte-for-byte. The whole file is then replaced via a
	// staged sibling + atomic rename — the old append through a staging file
	// could be interrupted mid-append, and the staging path itself used to
	// be a GLOBAL /tmp name shared across apps (audit F73).
	content := output
	if content != "" && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	added := false
	for _, k := range sortedKeys(vars) {
		if existing[k] {
			continue
		}
		content += k + "=" + vars[k] + "\n"
		added = true
	}
	if !added {
		return nil
	}
	if err := ssh.UploadAtomic(ctx, m.exec, strings.NewReader(content), envPath, "0600"); err != nil {
		return fmt.Errorf("writing %s: %w", envPath, err)
	}
	return nil
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
	// EnsureRunning reconciles data-directory ownership against the new image.
	_, err := m.EnsureRunning(ctx, app, name, cfg)
	return err
}

// reconcileDataOwnership chowns an accessory's data directories to the UID the
// NEW image runs as, when the two disagree.
//
// An image that changes its runtime user between versions leaves every existing
// deployment's data directory owned by the old UID, and the upgraded container
// cannot open its own files. The failure is loud but uninformative — the
// container crash-loops on a permission error from inside the engine, with
// nothing connecting it to the upgrade — and the data is fine the whole time.
// Nucleus did exactly this going from root to 10001.
//
// Only the accessory's own volume directories are touched, and only when the
// image declares a numeric non-root user that differs from what is on disk.
// Best-effort: if anything here cannot be determined, the upgrade proceeds as
// before rather than blocking on a guess.
func (m *Manager) reconcileDataOwnership(ctx context.Context, app, name string, cfg config.AccessoryConfig) error {
	if len(cfg.Volumes) == 0 {
		return nil
	}
	// The user the new image declares, e.g. "10001:10001" or "10001". A named
	// user (or empty) means root or an unknown mapping — nothing to reconcile.
	// On a first start the image is not local yet; pull it so the answer is
	// real rather than "unknown, skip" (`docker run` would pull it anyway).
	img := ssh.ShellQuote(cfg.Image)
	imageUser, err := m.exec.Run(ctx, fmt.Sprintf(
		"docker image inspect -f '{{.Config.User}}' %s 2>/dev/null || (docker pull %s >/dev/null 2>&1 && docker image inspect -f '{{.Config.User}}' %s 2>/dev/null)",
		img, img, img,
	))
	if err != nil {
		return nil
	}
	uid, gid, hasGid := strings.Cut(strings.TrimSpace(imageUser), ":")
	if uid == "" || uid == "0" {
		return nil
	}
	if _, convErr := strconv.Atoi(uid); convErr != nil {
		return nil
	}
	// Honor an explicitly declared GID. When the image declares only a UID,
	// chown changes the OWNER alone — inventing gid=uid rewrote group
	// ownership the image never asked for (audit F74).
	owner := uid
	if hasGid && gid != "" {
		if _, gErr := strconv.Atoi(gid); gErr != nil {
			return nil // named group: not safely reconciled from here
		}
		owner = uid + ":" + gid
	}

	accDir := fmt.Sprintf("%s/%s/accessories/%s", deploymentsDir, app, name)
	for _, volName := range sortedKeys(cfg.Volumes) {
		dir := fmt.Sprintf("%s/%s", accDir, volName)
		dirOwner, ownErr := m.exec.Run(ctx, fmt.Sprintf("stat -c '%%u' %s 2>/dev/null", ssh.ShellQuote(dir)))
		if ownErr != nil || strings.TrimSpace(dirOwner) == "" || strings.TrimSpace(dirOwner) == uid {
			continue
		}
		fmt.Fprintf(m.out, "  %s runs as %s; %s is owned by uid %s — reconciling\n",
			cfg.Image, owner, dir, strings.TrimSpace(dirOwner))
		// chown from inside a throwaway container, as root, so this works for
		// a non-root deploy user too (host-side chown of a root-owned directory
		// needs sudo, which teploy never assumes). Fall back to the host chown
		// for images without a chown binary.
		if _, chErr := m.exec.Run(ctx, fmt.Sprintf(
			"docker run --rm --user 0 --entrypoint chown -v %s %s -R %s /teploy-data",
			ssh.ShellQuote(dir+":/teploy-data"), img, owner,
		)); chErr != nil {
			if _, hostErr := m.exec.Run(ctx, "chown -R "+owner+" "+ssh.ShellQuote(dir)); hostErr != nil {
				return fmt.Errorf("chowning %s to %s (the image's user): %w", dir, owner, hostErr)
			}
		}
	}
	return nil
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
		vars["DATABASE_URL"] = dbURL("postgres", user, password, alias, port, db)

	case isImageType(image, "mysql"), isImageType(image, "mariadb"):
		password := env["MYSQL_ROOT_PASSWORD"]
		db := env["MYSQL_DATABASE"]
		if db == "" {
			db = app
		}
		if port == 0 {
			port = 3306
		}
		vars["DATABASE_URL"] = dbURL("mysql", "root", password, alias, port, db)

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

// dbURL builds a database URL with net/url so credentials containing URL
// reserved characters (@, :, /, ?, #, %) cannot change the connection's
// meaning — raw fmt.Sprintf interpolation used to silently point apps at
// the wrong host or mangle the password (audit F70).
func dbURL(scheme, user, password, host string, port int, db string) string {
	u := url.URL{
		Scheme: scheme,
		Host:   net.JoinHostPort(host, strconv.Itoa(port)),
		Path:   "/" + db,
	}
	if user != "" || password != "" {
		u.User = url.UserPassword(user, password)
	}
	return u.String()
}

// isImageType checks if a Docker image reference's final repository
// component matches a service type. Delegates to docker.ImageRepository,
// which strips registry hosts (including their port colons), tags, and
// digests — the local split-at-first-colon version classified
// "registry.example:5000/postgres" as "registry.example", so real
// databases started with no generated connection env at all (audit F71).
func isImageType(image, serviceType string) bool {
	return docker.ImageRepository(image) == serviceType
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
