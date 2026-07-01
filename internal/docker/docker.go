package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/useteploy/teploy/internal/ssh"
)

// dockerTimeLayout matches docker ps's CreatedAt format, e.g.
// "2026-05-28 21:33:29 -0700 PDT".
const dockerTimeLayout = "2006-01-02 15:04:05 -0700 MST"

// parseDockerTime parses a docker CreatedAt timestamp. On failure it returns a
// far-future time so the caller treats the container as newest (never pruned).
func parseDockerTime(s string) time.Time {
	if t, err := time.Parse(dockerTimeLayout, strings.TrimSpace(s)); err == nil {
		return t
	}
	return time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
}

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
	BindHost      string            // host IP to publish the port on (default 127.0.0.1)
	ContainerPort int               // port the app listens on inside the container (default 80)
	EnvFiles      []string          // paths to env files on the server, applied in order (later files' keys win)
	Env           map[string]string // additional env vars — plaintext only; secrets belong in EnvFiles (see deploy.go), not here, since -e values are visible in this host's `ps aux`/`/proc/<pid>/cmdline` for the life of this docker run invocation
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

	// The full command is sent to a remote shell as one string, so every
	// interpolated value (name, app, image, env values, volume specs, …) is
	// single-quoted to prevent a space or shell metacharacter from breaking the
	// command or injecting. The fixed flags are literals and need no quoting;
	// the optional Cmd override is left raw on purpose (see below).
	q := ssh.ShellQuote
	args := []string{
		"docker", "run", "--detach",
		"--restart", "unless-stopped",
		"--name", q(name),
		"--network", "teploy",
	}

	// Network alias: web process gets the app name, others get app-process.
	if cfg.Process == "web" {
		args = append(args, "--network-alias", q(cfg.App))
	} else {
		args = append(args, "--network-alias", q(cfg.App+"-"+cfg.Process))
	}

	// Labels for filtering containers by app, process, and version.
	args = append(args,
		"--label", q("teploy.app="+cfg.App),
		"--label", q("teploy.process="+cfg.Process),
		"--label", q("teploy.version="+cfg.Version),
	)

	// Port publishing and PORT env var injection.
	if cfg.Port > 0 {
		hostPort := strconv.Itoa(cfg.Port)
		containerPort := cfg.ContainerPort
		if containerPort == 0 {
			containerPort = 80
		}
		cPortStr := strconv.Itoa(containerPort)
		// Default: bind the published port to localhost only. Caddy reaches the
		// container over the teploy network via its network alias (see
		// InternalPort), so this host mapping exists solely for local health
		// checks. Publishing on 0.0.0.0 would expose the app directly on a
		// high port — bypassing Caddy/TLS, and Docker bypasses UFW — so we
		// restrict it to 127.0.0.1 unless the caller opts into a wider bind
		// (ingress: host sets BindHost to 0.0.0.0 for a directly-reachable port).
		bindHost := cfg.BindHost
		if bindHost == "" {
			bindHost = "127.0.0.1"
		}
		args = append(args, "-p", bindHost+":"+hostPort+":"+cPortStr, "-e", "PORT="+cPortStr)
	}

	// Env files on server, in order — docker merges --env-file flags with
	// later files' keys winning over earlier ones, so callers put the
	// highest-priority file (e.g. decrypted secrets) last.
	for _, f := range cfg.EnvFiles {
		if f != "" {
			args = append(args, "--env-file", q(f))
		}
	}

	// Additional env vars, sorted for deterministic command output.
	if len(cfg.Env) > 0 {
		keys := sortedKeys(cfg.Env)
		for _, k := range keys {
			args = append(args, "-e", q(k+"="+cfg.Env[k]))
		}
	}

	// Volume mounts, sorted for deterministic command output.
	if len(cfg.Volumes) > 0 {
		keys := sortedKeys(cfg.Volumes)
		for _, k := range keys {
			args = append(args, "-v", q(k+":"+cfg.Volumes[k]))
		}
	}

	// Resource limits.
	if cfg.Memory != "" {
		args = append(args, "--memory", q(cfg.Memory))
	}
	if cfg.CPU != "" {
		args = append(args, "--cpus", q(cfg.CPU))
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
	args = append(args, q(cfg.Image))

	// Optional command override. Left RAW (unquoted) on purpose: Cmd is a
	// command line (e.g. "npm run start"), so the remote shell must word-split
	// it into the container's argv — quoting it as a single token would make
	// docker treat the whole string as one non-existent executable. Cmd is
	// operator-authored config (like a Dockerfile CMD), not external input.
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
	cmd := fmt.Sprintf("docker stop -t %d %s", timeout, ssh.ShellQuote(name))
	if _, err := c.exec.Run(ctx, cmd); err != nil {
		return fmt.Errorf("stopping container %s: %w", name, err)
	}
	return nil
}

// Exec runs a command inside a running container via docker exec.
func (c *Client) Exec(ctx context.Context, name, command string) (string, error) {
	// Single-quote for the REMOTE shell so it doesn't expand $/backticks before
	// docker sees the args; the container's `sh -c` then interprets command.
	// %q (double quotes) would let the remote shell expand the value first.
	cmd := fmt.Sprintf("docker exec %s sh -c %s", ssh.ShellQuote(name), ssh.ShellQuote(command))
	output, err := c.exec.Run(ctx, cmd)
	if err != nil {
		return output, fmt.Errorf("exec in container %s: %w", name, err)
	}
	return output, nil
}

// ExecStream runs a command inside a running container and streams its
// stdout/stderr to the given writers in real time. Unlike Exec it doesn't
// buffer output (suited to long-running commands like migrations) and the
// returned error carries the command's non-zero exit status. command is run
// through the container's `sh -c`; both name and command are single-quoted for
// the remote shell (see Exec).
func (c *Client) ExecStream(ctx context.Context, name, command string, stdout, stderr io.Writer) error {
	cmd := fmt.Sprintf("docker exec %s sh -c %s", ssh.ShellQuote(name), ssh.ShellQuote(command))
	return c.exec.RunStream(ctx, cmd, stdout, stderr)
}

// RunningContainer returns the name of a running container for the app's given
// process (e.g. "web"). For a multi-replica process it returns the first
// replica. Used by `app exec` to pick a target to run a one-off command in.
func (c *Client) RunningContainer(ctx context.Context, app, process string) (string, error) {
	containers, err := c.ListContainers(ctx, app)
	if err != nil {
		return "", err
	}
	for _, ct := range containers {
		if ct.State == "running" && ct.Labels["teploy.process"] == process {
			return ct.Name, nil
		}
	}
	return "", fmt.Errorf("no running %q container found for app %q — is it deployed and running?", process, app)
}

// Start starts a stopped container.
//
// NOTE: prefer Restart() for rollback flows. Docker (≥ 29) may silently fail
// to re-publish HostConfig.PortBindings on `docker start` if another container
// has taken+released the host port since this container was stopped. Restart()
// avoids that by force-removing + recreating with the same config.
func (c *Client) Start(ctx context.Context, name string) error {
	if _, err := c.exec.Run(ctx, "docker start "+ssh.ShellQuote(name)); err != nil {
		return fmt.Errorf("starting container %s: %w", name, err)
	}
	return nil
}

// containerInspect mirrors the subset of `docker inspect` JSON we use to
// reconstruct a container's `docker run` invocation.
type containerInspect struct {
	Config struct {
		Image       string
		Env         []string
		Cmd         []string
		WorkingDir  string
		User        string
		Labels      map[string]string
		Healthcheck *struct {
			Test []string
		}
	}
	HostConfig struct {
		NetworkMode  string
		PortBindings map[string][]struct {
			HostIp   string
			HostPort string
		}
		Binds  []string
		Mounts []struct {
			Type     string // "bind" | "volume" | "tmpfs"
			Source   string
			Target   string
			ReadOnly bool
		}
		RestartPolicy struct {
			Name string
		}
		Memory   int64 // bytes
		NanoCpus int64 // nano-CPUs (1 CPU = 1e9)
	}
	NetworkSettings struct {
		Networks map[string]struct {
			Aliases []string
		}
	}
}

// Restart fully recreates a container with its original configuration:
// inspect the existing container, force-remove it, then `docker run` with
// the same name + extracted args.
//
// Use this in rollback flows where plain Start() is insufficient. Docker
// 29 silently fails to re-publish HostConfig.PortBindings if another
// container has taken + released the host port since the original stop
// (the bind also detaches from custom networks). Recreating from scratch
// sidesteps that landmine.
//
// avoidPorts is the set of host ports currently held by containers this
// restart must not collide with — critically, the live container(s) being
// rolled back FROM, which are still running (and still holding their port)
// at the point Restart is called, since rollback starts the target before
// stopping the current version for zero-downtime. A single-hop rollback
// (to the immediately-previous version) can never collide: that version's
// port was freed by the deploy that superseded it and nothing has claimed
// it since. A --to rollback reaching back further can: Docker's ephemeral
// port allocator reuses freed ports, so an older version's original port
// may since have been handed to what is now the live container. Found via
// live testing (v1→49152, v1-tls→49153, v2 reused 49152 after v1-tls
// stopped; `rollback --to v1` then collided with the still-running v2).
// When a binding's original port is in avoidPorts, a fresh one is
// allocated instead — safe because HostPort() (not persisted state) is
// what the rest of the rollback reads back afterward. Pass nil to
// preserve every port binding exactly (no caller does today, but this
// keeps the zero-collision-checking path available for a non-rollback use
// of Restart in the future).
//
// Preserved across the recreate: image, network mode + aliases, port
// bindings (subject to the above), env, bind mounts, named-volume + tmpfs
// mounts, command, working dir, user, labels, restart policy, memory + CPU
// limits, and the --no-healthcheck NONE marker.
func (c *Client) Restart(ctx context.Context, name string, avoidPorts map[int]bool) error {
	raw, err := c.exec.Run(ctx, "docker inspect "+ssh.ShellQuote(name))
	if err != nil {
		return fmt.Errorf("inspecting %s: %w", name, err)
	}
	var arr []containerInspect
	if err := json.Unmarshal([]byte(raw), &arr); err != nil {
		return fmt.Errorf("parsing inspect of %s: %w", name, err)
	}
	if len(arr) == 0 {
		return fmt.Errorf("container %s not found", name)
	}
	spec := arr[0]

	args := []string{"-d", "--name", ssh.ShellQuote(name)}

	// Network mode (e.g. "teploy"). Skip docker's "default" / "bridge".
	if spec.HostConfig.NetworkMode != "" && spec.HostConfig.NetworkMode != "default" && spec.HostConfig.NetworkMode != "bridge" {
		args = append(args, "--network", spec.HostConfig.NetworkMode)
	}

	// Network aliases on the primary network. Skip the container-name auto-alias
	// and anything that's a prefix of the container ID (also auto-added).
	primary := spec.HostConfig.NetworkMode
	if netInfo, ok := spec.NetworkSettings.Networks[primary]; ok {
		seenAlias := map[string]bool{name: true}
		aliases := append([]string(nil), netInfo.Aliases...)
		sort.Strings(aliases)
		for _, alias := range aliases {
			if alias == "" || seenAlias[alias] {
				continue
			}
			seenAlias[alias] = true
			args = append(args, "--network-alias", alias)
		}
	}

	// Port bindings: sorted for deterministic args ordering.
	pbKeys := make([]string, 0, len(spec.HostConfig.PortBindings))
	for k := range spec.HostConfig.PortBindings {
		pbKeys = append(pbKeys, k)
	}
	sort.Strings(pbKeys)
	// Ports claimed so far this call, merged into avoidPorts as bindings are
	// resolved — so two colliding bindings on the same container (unusual,
	// but possible with multiple published ports) don't get handed the same
	// replacement port.
	claimed := make(map[int]bool, len(avoidPorts))
	for p := range avoidPorts {
		claimed[p] = true
	}
	for _, containerPort := range pbKeys {
		for _, b := range spec.HostConfig.PortBindings[containerPort] {
			host := b.HostIp
			if host == "" {
				host = "0.0.0.0"
			}
			hostPort := b.HostPort
			if origPort, err := strconv.Atoi(b.HostPort); err == nil && claimed[origPort] {
				newPort, ferr := c.FindAvailablePortExcluding(ctx, claimed)
				if ferr != nil {
					return fmt.Errorf("port %s for %s is held by another running container and no replacement port is available: %w", b.HostPort, name, ferr)
				}
				hostPort = strconv.Itoa(newPort)
				claimed[newPort] = true
			}
			args = append(args, "-p", fmt.Sprintf("%s:%s:%s", host, hostPort, containerPort))
		}
	}

	// Env vars.
	for _, e := range spec.Config.Env {
		args = append(args, "-e", ssh.ShellQuote(e))
	}

	// Bind mounts (already host:container[:mode] formatted by docker).
	for _, b := range spec.HostConfig.Binds {
		args = append(args, "-v", b)
	}

	// Named volume + tmpfs + non-bind mounts (Binds covers bind mounts; this
	// covers everything created via --mount). Use --mount syntax for fidelity.
	for _, m := range spec.HostConfig.Mounts {
		if m.Type == "" || m.Target == "" {
			continue
		}
		parts := []string{"type=" + m.Type, "target=" + m.Target}
		if m.Source != "" {
			parts = append(parts, "source="+m.Source)
		}
		if m.ReadOnly {
			parts = append(parts, "readonly")
		}
		args = append(args, "--mount", ssh.ShellQuote(strings.Join(parts, ",")))
	}

	// Resource limits.
	if spec.HostConfig.Memory > 0 {
		args = append(args, "--memory", fmt.Sprintf("%db", spec.HostConfig.Memory))
	}
	if spec.HostConfig.NanoCpus > 0 {
		// NanoCpus → fractional --cpus (1e9 nano = 1.0 cpu).
		cpus := float64(spec.HostConfig.NanoCpus) / 1e9
		args = append(args, "--cpus", strconv.FormatFloat(cpus, 'f', -1, 64))
	}

	// Labels (sorted for determinism).
	labelKeys := make([]string, 0, len(spec.Config.Labels))
	for k := range spec.Config.Labels {
		labelKeys = append(labelKeys, k)
	}
	sort.Strings(labelKeys)
	for _, k := range labelKeys {
		args = append(args, "--label", ssh.ShellQuote(k+"="+spec.Config.Labels[k]))
	}

	// Preserve the explicit-NONE healthcheck (set by --no-healthcheck at run).
	if spec.Config.Healthcheck != nil && len(spec.Config.Healthcheck.Test) == 1 && spec.Config.Healthcheck.Test[0] == "NONE" {
		args = append(args, "--no-healthcheck")
	}

	// Restart policy (skip docker default "no").
	if spec.HostConfig.RestartPolicy.Name != "" && spec.HostConfig.RestartPolicy.Name != "no" {
		args = append(args, "--restart", spec.HostConfig.RestartPolicy.Name)
	}

	if spec.Config.WorkingDir != "" {
		args = append(args, "-w", spec.Config.WorkingDir)
	}
	if spec.Config.User != "" {
		args = append(args, "-u", spec.Config.User)
	}

	// Image (last positional before cmd).
	args = append(args, ssh.ShellQuote(spec.Config.Image))

	// Cmd.
	for _, cmdPart := range spec.Config.Cmd {
		args = append(args, ssh.ShellQuote(cmdPart))
	}

	// Force-remove old container, then run fresh.
	if _, err := c.exec.Run(ctx, "docker rm -f "+ssh.ShellQuote(name)); err != nil {
		return fmt.Errorf("removing old %s: %w", name, err)
	}
	if _, err := c.exec.Run(ctx, "docker run "+strings.Join(args, " ")); err != nil {
		return fmt.Errorf("recreating %s: %w", name, err)
	}
	return nil
}

// Pull pulls a Docker image from a registry.
func (c *Client) Pull(ctx context.Context, image string) error {
	if _, err := c.exec.Run(ctx, "docker pull "+ssh.ShellQuote(image)); err != nil {
		return fmt.Errorf("pulling image %s: %w", image, err)
	}
	return nil
}

// Remove removes a stopped container.
func (c *Client) Remove(ctx context.Context, name string) error {
	if _, err := c.exec.Run(ctx, "docker rm "+ssh.ShellQuote(name)); err != nil {
		return fmt.Errorf("removing container %s: %w", name, err)
	}
	return nil
}

// HostPort returns the container's host-mapped port — the port a health
// check on this machine connects to at http://localhost:<port>, as opposed
// to InternalPort (the container-internal port Caddy dials over the
// Docker network). Used by Rollback to derive health-check ports by
// inspecting the actual container instead of relying on
// state.AppState.PreviousPort, which only ever remembers the single most
// recent previous version — inspection works for --to <hash> rolling back
// further than that.
func (c *Client) HostPort(ctx context.Context, name string) (int, error) {
	out, err := c.exec.Run(ctx, fmt.Sprintf(
		"docker inspect -f '{{range $p, $b := .NetworkSettings.Ports}}{{range $b}}{{.HostPort}} {{end}}{{end}}' %s",
		ssh.ShellQuote(name),
	))
	if err != nil {
		return 0, fmt.Errorf("inspecting container %s: %w", name, err)
	}
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return 0, fmt.Errorf("container %s has no host-mapped ports", name)
	}
	port, err := strconv.Atoi(fields[0])
	if err != nil {
		return 0, fmt.Errorf("parsing host port %q from container %s: %w", fields[0], name, err)
	}
	return port, nil
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
		ssh.ShellQuote(name),
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
	cmd := "docker ps --all --filter label=teploy.app=" + ssh.ShellQuote(app) + " --format '{{json .}}'"
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
		newestCreated  time.Time
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
		// Compare as instants, not strings. docker's CreatedAt is a localized
		// timestamp ("2006-01-02 15:04:05 -0700 MST"), so lexicographic ordering
		// is wrong across timezones / DST. On a parse failure, treat the version
		// as newest so cleanup never prunes something it can't date.
		created := parseDockerTime(ct.CreatedAt)
		if created.After(info.newestCreated) {
			info.newestCreated = created
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
		if !sorted[i].info.newestCreated.Equal(sorted[j].info.newestCreated) {
			return sorted[i].info.newestCreated.After(sorted[j].info.newestCreated)
		}
		// Deterministic tie-break when timestamps match (or both unparseable).
		return sorted[i].version > sorted[j].version
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
			_, _ = c.exec.Run(ctx, "docker rm -f "+ssh.ShellQuote(name))
		}
		// Best-effort image removal. Fails (silently) if another
		// container or tag still references the image, which is the
		// safe behavior.
		for img := range e.info.images {
			_, _ = c.exec.Run(ctx, "docker rmi "+ssh.ShellQuote(img))
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
	return c.FindAvailablePortExcluding(ctx, nil)
}

// FindAvailablePortExcluding returns the first port in the ephemeral range
// (49152-65535) that is neither currently listening nor in the claimed set.
//
// Multi-replica deploys allocate every replica's port up front, before any
// container starts — and `ss -tln` only reports ports that are actually bound.
// Without excluding the ports already handed out this round, every replica gets
// the same first-free port and replica 2's `docker run -p` fails with "port is
// already allocated", aborting the whole deploy. Callers in a multi-port loop
// must pass the ports they've already claimed.
func (c *Client) FindAvailablePortExcluding(ctx context.Context, claimed map[int]bool) (int, error) {
	output, err := c.exec.Run(ctx, "ss -tln")
	if err != nil {
		return 0, fmt.Errorf("checking listening ports: %w", err)
	}

	used := parseListeningPorts(output)
	for port := 49152; port <= 65535; port++ {
		if !used[port] && !claimed[port] {
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
