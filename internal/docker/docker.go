package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/useteploy/teploy/internal/ssh"
)

// Container represents a Docker container as reported by docker ps.
type Container struct {
	ID        string
	Name      string
	Image     string
	State     string // "running", "exited", "created"
	Status    string // human-readable, e.g. "Up 2 hours"
	CreatedAt string // raw docker timestamp, e.g. "2026-05-28 21:33:29 -0700 PDT" — lexicographically sortable for same-TZ comparisons
	Labels    map[string]string
}

// RunConfig holds the parameters for starting a new container.
type RunConfig struct {
	App           string            // app name (required)
	Process       string            // process type, e.g. "web" (required)
	Version       string            // short git hash (required)
	Image         string            // Docker image (required)
	Port          int               // host port for external access
	ContainerPort int               // port the app listens on inside the container (default 80)
	EnvFile       string            // path to env file on the server
	Env           map[string]string // additional env vars
	Volumes       map[string]string // host_path -> container_path
	Cmd           string            // command override (appended after image)
	Memory        string            // memory limit, e.g. "512m"
	CPU           string            // CPU limit, e.g. "1.0"
	Name          string            // explicit container name (overrides auto-generated)
	NoHealthcheck bool              // pass --no-healthcheck so the container ignores the image HEALTHCHECK
}

// ContainerName returns the standard teploy container name: {app}-{process}-{version}.
func ContainerName(app, process, version string) string {
	return app + "-" + process + "-" + version
}

// ReplicaContainerName returns a replica-indexed container name: {app}-{process}-{version}-{index}.
// Index is 1-based. If index is 0 or 1 with total replicas=1, falls back to standard name.
func ReplicaContainerName(app, process, version string, index, total int) string {
	if total <= 1 {
		return ContainerName(app, process, version)
	}
	return fmt.Sprintf("%s-%s-%s-%d", app, process, version, index)
}

// Client executes Docker commands on a remote server via SSH.
type Client struct {
	exec ssh.Executor
}

// NewClient creates a Docker client backed by the given SSH executor.
func NewClient(exec ssh.Executor) *Client {
	return &Client{exec: exec}
}

// Run starts a new container and returns its ID.
func (c *Client) Run(ctx context.Context, cfg RunConfig) (string, error) {
	if cfg.App == "" || cfg.Process == "" || cfg.Version == "" || cfg.Image == "" {
		return "", fmt.Errorf("run config requires app, process, version, and image")
	}

	name := cfg.Name
	if name == "" {
		name = ContainerName(cfg.App, cfg.Process, cfg.Version)
	}

	args := []string{
		"docker", "run", "--detach",
		"--restart", "unless-stopped",
		"--name", name,
		"--network", "teploy",
	}

	// Network alias: web process gets the app name, others get app-process.
	if cfg.Process == "web" {
		args = append(args, "--network-alias", cfg.App)
	} else {
		args = append(args, "--network-alias", cfg.App+"-"+cfg.Process)
	}

	// Labels for filtering containers by app, process, and version.
	args = append(args,
		"--label", "teploy.app="+cfg.App,
		"--label", "teploy.process="+cfg.Process,
		"--label", "teploy.version="+cfg.Version,
	)

	// Port publishing and PORT env var injection.
	if cfg.Port > 0 {
		hostPort := strconv.Itoa(cfg.Port)
		containerPort := cfg.ContainerPort
		if containerPort == 0 {
			containerPort = 80
		}
		cPortStr := strconv.Itoa(containerPort)
		// Bind the published port to localhost only. Caddy reaches the
		// container over the teploy network via its network alias (see
		// InternalPort), so this host mapping exists solely for local health
		// checks. Publishing on 0.0.0.0 would expose the app directly on a
		// high port — bypassing Caddy/TLS, and Docker bypasses UFW — so we
		// restrict it to 127.0.0.1.
		args = append(args, "-p", "127.0.0.1:"+hostPort+":"+cPortStr, "-e", "PORT="+cPortStr)
	}

	// Env file on server.
	if cfg.EnvFile != "" {
		args = append(args, "--env-file", cfg.EnvFile)
	}

	// Additional env vars, sorted for deterministic command output.
	if len(cfg.Env) > 0 {
		keys := sortedKeys(cfg.Env)
		for _, k := range keys {
			args = append(args, "-e", k+"="+cfg.Env[k])
		}
	}

	// Volume mounts, sorted for deterministic command output.
	if len(cfg.Volumes) > 0 {
		keys := sortedKeys(cfg.Volumes)
		for _, k := range keys {
			args = append(args, "-v", k+":"+cfg.Volumes[k])
		}
	}

	// Resource limits.
	if cfg.Memory != "" {
		args = append(args, "--memory", cfg.Memory)
	}
	if cfg.CPU != "" {
		args = append(args, "--cpus", cfg.CPU)
	}

	// Log rotation to prevent disk fill.
	args = append(args, "--log-opt", "max-size=10m")

	// Per-process HEALTHCHECK override. When set, the container ignores the
	// image's HEALTHCHECK directive. Useful for non-web processes (workers,
	// schedulers) that share a runner image with web but don't expose the
	// HTTP surface the image's probe assumes.
	if cfg.NoHealthcheck {
		args = append(args, "--no-healthcheck")
	}

	// Image must come after all flags.
	args = append(args, cfg.Image)

	// Optional command override.
	if cfg.Cmd != "" {
		args = append(args, cfg.Cmd)
	}

	output, err := c.exec.Run(ctx, strings.Join(args, " "))
	if err != nil {
		return "", fmt.Errorf("starting container %s: %w", name, err)
	}

	return strings.TrimSpace(output), nil
}

// Stop stops a container by name. Sends SIGTERM, then SIGKILL after timeout seconds.
func (c *Client) Stop(ctx context.Context, name string, timeout int) error {
	cmd := fmt.Sprintf("docker stop -t %d %s", timeout, name)
	if _, err := c.exec.Run(ctx, cmd); err != nil {
		return fmt.Errorf("stopping container %s: %w", name, err)
	}
	return nil
}

// Exec runs a command inside a running container via docker exec.
func (c *Client) Exec(ctx context.Context, name, command string) (string, error) {
	cmd := fmt.Sprintf("docker exec %s sh -c %q", name, command)
	output, err := c.exec.Run(ctx, cmd)
	if err != nil {
		return output, fmt.Errorf("exec in container %s: %w", name, err)
	}
	return output, nil
}

// Start starts a stopped container.
func (c *Client) Start(ctx context.Context, name string) error {
	if _, err := c.exec.Run(ctx, "docker start "+name); err != nil {
		return fmt.Errorf("starting container %s: %w", name, err)
	}
	return nil
}

// Pull pulls a Docker image from a registry.
func (c *Client) Pull(ctx context.Context, image string) error {
	if _, err := c.exec.Run(ctx, "docker pull "+image); err != nil {
		return fmt.Errorf("pulling image %s: %w", image, err)
	}
	return nil
}

// Remove removes a stopped container.
func (c *Client) Remove(ctx context.Context, name string) error {
	if _, err := c.exec.Run(ctx, "docker rm "+name); err != nil {
		return fmt.Errorf("removing container %s: %w", name, err)
	}
	return nil
}

// InternalPort returns the container's internal listening port — the port
// the app speaks HTTP on inside the Docker network, not the host-mapped
// port. Caddy dials this port when reverse-proxying over the teploy
// network. Returns an error if the container exposes zero or multiple
// ports (teploy containers always expose exactly one).
func (c *Client) InternalPort(ctx context.Context, name string) (int, error) {
	// `docker inspect` emits each exposed port once, e.g. "3000/tcp".
	out, err := c.exec.Run(ctx, fmt.Sprintf(
		"docker inspect -f '{{range $p, $_ := .NetworkSettings.Ports}}{{$p}} {{end}}' %s",
		name,
	))
	if err != nil {
		return 0, fmt.Errorf("inspecting container %s: %w", name, err)
	}
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return 0, fmt.Errorf("container %s has no exposed ports", name)
	}
	// Take the first port — teploy only publishes one per container.
	portStr, _, _ := strings.Cut(fields[0], "/")
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return 0, fmt.Errorf("parsing port %q from container %s: %w", fields[0], name, err)
	}
	return port, nil
}

// ListContainers returns all containers for the given app, including stopped ones.
func (c *Client) ListContainers(ctx context.Context, app string) ([]Container, error) {
	cmd := "docker ps --all --filter label=teploy.app=" + app + " --format '{{json .}}'"
	output, err := c.exec.Run(ctx, cmd)
	if err != nil {
		return nil, fmt.Errorf("listing containers for %s: %w", app, err)
	}

	output = strings.TrimSpace(output)
	if output == "" {
		return nil, nil
	}

	return ParseContainers(output)
}

// PruneVersions removes containers and images for app versions older than
// the `keep` most-recent (by newest container creation time), always
// preserving the explicitly-named protectedVersions even if their
// containers are stopped or older than other versions on disk. Use this
// to bound the disk footprint of past deploys while keeping the current
// version + a rollback window.
//
// Returns the list of pruned versions and a non-nil error only when the
// initial container listing fails. Per-container removal failures are
// non-fatal — this is best-effort disk cleanup, not a deploy gate.
func (c *Client) PruneVersions(ctx context.Context, app string, keep int, protectedVersions ...string) ([]string, error) {
	if keep < 0 {
		keep = 0
	}
	containers, err := c.ListContainers(ctx, app)
	if err != nil {
		return nil, fmt.Errorf("listing containers for prune: %w", err)
	}

	// Group containers by teploy.version label. Track newest creation
	// timestamp + container names + image names per version. Containers
	// without the label (e.g. caddy, postgres accessory) are ignored.
	type vinfo struct {
		newestCreated  string
		containerNames []string
		images         map[string]struct{}
	}
	versions := map[string]*vinfo{}
	for _, ct := range containers {
		v := ct.Labels["teploy.version"]
		if v == "" {
			continue
		}
		info, ok := versions[v]
		if !ok {
			info = &vinfo{images: map[string]struct{}{}}
			versions[v] = info
		}
		if ct.CreatedAt > info.newestCreated {
			info.newestCreated = ct.CreatedAt
		}
		info.containerNames = append(info.containerNames, ct.Name)
		if ct.Image != "" {
			info.images[ct.Image] = struct{}{}
		}
	}

	if len(versions) == 0 {
		return nil, nil
	}

	// Sort versions by recency (newest first).
	type entry struct {
		version string
		info    *vinfo
	}
	sorted := make([]entry, 0, len(versions))
	for v, info := range versions {
		sorted = append(sorted, entry{v, info})
	}
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].info.newestCreated > sorted[j].info.newestCreated
	})

	// Build the protected set: explicit names + top-`keep` by recency.
	protect := map[string]bool{}
	for _, v := range protectedVersions {
		if v != "" {
			protect[v] = true
		}
	}
	kept := 0
	for _, e := range sorted {
		if kept < keep {
			protect[e.version] = true
			kept++
		}
	}

	var pruned []string
	for _, e := range sorted {
		if protect[e.version] {
			continue
		}
		// Force-remove containers in case any are still running. We
		// already took ownership of cleanup; refusing to nuke a stray
		// running container from an older version defeats the point.
		for _, name := range e.info.containerNames {
			_, _ = c.exec.Run(ctx, "docker rm -f "+name)
		}
		// Best-effort image removal. Fails (silently) if another
		// container or tag still references the image, which is the
		// safe behavior.
		for img := range e.info.images {
			_, _ = c.exec.Run(ctx, "docker rmi "+img)
		}
		pruned = append(pruned, e.version)
	}
	return pruned, nil
}

// EnsureNetwork creates the "teploy" Docker network if it doesn't already exist.
func (c *Client) EnsureNetwork(ctx context.Context) error {
	cmd := "docker network inspect teploy >/dev/null 2>&1 || docker network create teploy"
	if _, err := c.exec.Run(ctx, cmd); err != nil {
		return fmt.Errorf("ensuring docker network: %w", err)
	}
	return nil
}

// FindAvailablePort returns the first unused port in the ephemeral range (49152-65535).
func (c *Client) FindAvailablePort(ctx context.Context) (int, error) {
	output, err := c.exec.Run(ctx, "ss -tln")
	if err != nil {
		return 0, fmt.Errorf("checking listening ports: %w", err)
	}

	used := parseListeningPorts(output)
	for port := 49152; port <= 65535; port++ {
		if !used[port] {
			return port, nil
		}
	}

	return 0, fmt.Errorf("no available ports in range 49152-65535")
}

// psEntry matches Docker's JSON output from docker ps --format '{{json .}}'.
type psEntry struct {
	ID        string `json:"ID"`
	Names     string `json:"Names"`
	Image     string `json:"Image"`
	State     string `json:"State"`
	Status    string `json:"Status"`
	CreatedAt string `json:"CreatedAt"`
	Labels    string `json:"Labels"` // comma-separated "k=v,k=v"
}

// ParseContainers parses Docker JSON output into Container structs.
func ParseContainers(output string) ([]Container, error) {
	var containers []Container
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		var entry psEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			return nil, fmt.Errorf("parsing container entry: %w", err)
		}

		containers = append(containers, Container{
			ID:        entry.ID,
			Name:      entry.Names,
			Image:     entry.Image,
			State:     entry.State,
			Status:    entry.Status,
			CreatedAt: entry.CreatedAt,
			Labels:    parseLabels(entry.Labels),
		})
	}
	return containers, nil
}

// parseLabels splits docker ps's comma-separated "k=v,k=v" label string
// into a map. Values containing commas would break this, but teploy labels
// (teploy.app, teploy.process, teploy.version) are safe and known.
func parseLabels(s string) map[string]string {
	if s == "" {
		return nil
	}
	out := make(map[string]string)
	for _, pair := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(pair, "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return out
}

// parseListeningPorts extracts port numbers from ss -tln output.
// Handles IPv4 (0.0.0.0:22), IPv6 ([::]:22), and wildcard (*:22) formats.
func parseListeningPorts(output string) map[int]bool {
	ports := make(map[int]bool)
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		local := fields[3]
		idx := strings.LastIndex(local, ":")
		if idx < 0 {
			continue
		}
		port, err := strconv.Atoi(local[idx+1:])
		if err != nil {
			continue
		}
		ports[port] = true
	}
	return ports
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
