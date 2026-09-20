package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
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
	Publish       []string          // extra verbatim docker -p specs (e.g. "0.0.0.0:3001:3001") for apps with a second listener; no PORT env is derived from these
	EnvFiles      []string          // paths to env files on the server, applied in order (later files' keys win)
	Env           map[string]string // additional env vars — plaintext only; secrets belong in EnvFiles (see deploy.go), not here, since -e values are visible in this host's `ps aux`/`/proc/<pid>/cmdline` for the life of this docker run invocation
	Volumes       map[string]string // host_path -> container_path
	Cmd           string            // command override (appended after image)
	Memory        string            // memory limit, e.g. "512m"
	CPU           string            // CPU limit, e.g. "1.0"
	Name          string            // explicit container name (overrides auto-generated)
	NoHealthcheck bool              // pass --no-healthcheck so the container ignores the image HEALTHCHECK
}

// publishBinding renders a docker -p binding "[ip:]host:container" with
// the bind IP correctly bracketed for IPv6 (A19) and both ports validated.
// The bind must be an IP literal — docker requires one for an explicit
// bind, and the health-probe builder already validates the same value, so
// a hostname here could only ever produce a deploy that fails its own
// health checks.
func publishBinding(bind string, hostPort, containerPort int) (string, error) {
	normalized := strings.TrimSuffix(strings.TrimPrefix(bind, "["), "]")
	if net.ParseIP(normalized) == nil {
		return "", fmt.Errorf("publish bind %q must be an IP address", bind)
	}
	if hostPort < 1 || hostPort > 65535 {
		return "", fmt.Errorf("host port %d must be in 1..65535", hostPort)
	}
	if containerPort < 1 || containerPort > 65535 {
		return "", fmt.Errorf("container port %d must be in 1..65535", containerPort)
	}
	return net.JoinHostPort(normalized, strconv.Itoa(hostPort)) + ":" + strconv.Itoa(containerPort), nil
}

// ContainerName returns the standard teploy container name: {app}-{process}-{version}.
func ContainerName(app, process, version string) string {
	return app + "-" + process + "-" + version
}

// ImageRepository returns the repository component of an image reference:
// "postgres:16" → "postgres", "library/postgres" → "postgres",
// "registry.example:5000/postgres:16" → "postgres",
// "postgres@sha256:..." → "postgres". The FIRST colon used to be treated as
// the tag separator, which collapsed "registry.example:5000/postgres" to
// "registry.example" — classification by final repository component avoids
// confusing a registry-port colon with a tag colon (audit F71). Not a full
// OCI reference validator; callers use it for engine-type matching only.
func ImageRepository(image string) string {
	if i := strings.LastIndex(image, "@"); i >= 0 {
		image = image[:i]
	}
	lastSlash := strings.LastIndex(image, "/")
	lastColon := strings.LastIndex(image, ":")
	if lastColon > lastSlash {
		image = image[:lastColon]
	}
	if i := strings.LastIndex(image, "/"); i >= 0 {
		image = image[i+1:]
	}
	return image
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
		containerPort := cfg.ContainerPort
		if containerPort == 0 {
			containerPort = 80
		}
		// Default: bind the published port to localhost only. Caddy reaches the
		// container over the teploy network via its network alias (see
		// InternalPort), so this host mapping exists solely for local health
		// checks. Publishing on 0.0.0.0 would expose the app directly on a
		// high port — bypassing Caddy/TLS, and Docker bypasses UFW — so we
		// restrict it to 127.0.0.1 unless the caller opts into a wider bind
		// (ingress: host sets BindHost to 0.0.0.0 for a directly-reachable port).
		//
		// The binding is built with net.JoinHostPort and validated as an IP
		// (A19): a bare IPv6 bind such as ::1 used to concatenate into an
		// ambiguous "::1:49152:80" that docker could only misparse, and the
		// result is now quoted like every other interpolated argument.
		bindHost := cfg.BindHost
		if bindHost == "" {
			bindHost = "127.0.0.1"
		}
		binding, err := publishBinding(bindHost, cfg.Port, containerPort)
		if err != nil {
			return "", err
		}
		args = append(args, "-p", q(binding), "-e", "PORT="+strconv.Itoa(containerPort))
	}

	// Extra host port mappings (AppConfig.Publish), kept separate from the
	// block above: these are verbatim -p specs and no PORT env is derived
	// from them. Used by apps that serve a second listener on its own port.
	for _, pub := range cfg.Publish {
		args = append(args, "-p", q(pub))
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

	cmd := strings.Join(args, " ")
	output, err := c.exec.Run(ctx, cmd)
	if err != nil && nameAlreadyInUse(output, err) {
		// A container already holds this exact name. That happens routinely after
		// a rollback: rollback stops the superseded version's containers but
		// deliberately keeps them, so redeploying that same version — the normal
		// "roll back, fix forward, deploy again" sequence — failed with docker's
		// raw "name is already in use" and left the operator to clean up by hand.
		//
		// Handled reactively rather than with a pre-flight inspect so the normal
		// path costs nothing: this only runs when docker has actually complained.
		// A STOPPED container is dead weight and is removed, then the run is
		// retried once. A RUNNING one is refused — that means this exact version
		// is already live, and tearing it down mid-deploy to replace it with
		// itself is not something to do silently.
		if clearErr := c.clearStoppedContainer(ctx, name); clearErr != nil {
			return "", clearErr
		}
		output, err = c.exec.Run(ctx, cmd)
	}
	if err != nil {
		return "", fmt.Errorf("starting container %s: %w", name, err)
	}

	return strings.TrimSpace(output), nil
}

// nameAlreadyInUse reports whether a failed `docker run` failed specifically
// because the container name is taken. Matched on the message because that is
// all docker gives us over a shell; deliberately narrow, so any other failure
// falls through untouched rather than triggering a removal.
func nameAlreadyInUse(output string, err error) bool {
	haystack := strings.ToLower(output)
	if err != nil {
		haystack += " " + strings.ToLower(err.Error())
	}
	return strings.Contains(haystack, "already in use")
}

// ResolveImageID returns the immutable local image ID for ref. Deploy
// resolves this ONCE and creates every replica/worker from the ID (audit
// A52): a mutable tag can be re-pointed by a concurrent pull/build/tag on
// the same host mid-deploy — an app-scoped lock does not own the global
// Docker tag namespace — silently mixing images within one release.
func (c *Client) ResolveImageID(ctx context.Context, ref string) (string, error) {
	out, err := c.exec.Run(ctx, "docker image inspect --format '{{.Id}}' "+ssh.ShellQuote(ref))
	if err != nil {
		return "", fmt.Errorf("resolving image identity for %s: %w", ref, err)
	}
	id := strings.TrimSpace(out)
	if !strings.HasPrefix(id, "sha256:") || len(id) != len("sha256:")+64 {
		return "", fmt.Errorf("docker returned no immutable image ID for %s (got %q)", ref, id)
	}
	return id, nil
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

// Pull pulls a Docker image from a registry.
// ScanImage runs a Trivy vulnerability scan against an image already
// present on the server, streaming findings to out. Two passes because
// trivy's --exit-code applies to every severity in --severity: a
// report-only HIGH+CRITICAL pass informs the operator, then a --quiet
// CRITICAL-only pass with --exit-code 1 decides — fixable CRITICALs block
// the deploy before the image ever serves traffic. --ignore-unfixed keeps
// unfixable base-image CVEs from wedging every deploy. The vulnerability
// DB caches under /deployments/.trivy-cache so only the first scan pays
// the download.
func (c *Client) ScanImage(ctx context.Context, image string, out io.Writer) error {
	const trivyBase = "docker run --rm" +
		" -v /var/run/docker.sock:/var/run/docker.sock" +
		" -v /deployments/.trivy-cache:/root/.cache" +
		" aquasec/trivy:latest image --scanners vuln --ignore-unfixed "
	reportCmd := trivyBase + "--severity HIGH,CRITICAL " + ssh.ShellQuote(image)
	blockCmd := trivyBase + "--severity CRITICAL --exit-code 1 --quiet " + ssh.ShellQuote(image)

	if err := c.exec.RunStream(ctx, reportCmd, out, out); err != nil {
		return fmt.Errorf("trivy scan failed to run: %w", err)
	}
	if err := c.exec.RunStream(ctx, blockCmd, io.Discard, out); err != nil {
		return fmt.Errorf("image %s has fixable CRITICAL vulnerabilities — deploy blocked (patch the base image, or remove scan: true to bypass): %w", image, err)
	}
	return nil
}

func (c *Client) Pull(ctx context.Context, image string) error {
	if _, err := c.exec.Run(ctx, "docker pull "+ssh.ShellQuote(image)); err != nil {
		return fmt.Errorf("pulling image %s: %w", image, err)
	}
	return nil
}

// ImageExists reports whether the named image is already present in the
// server's local Docker image cache. A plain cache miss is distinguished
// from every other inspect failure (audit T17): the old
// `inspect && echo exists || echo missing` shape turned a daemon outage or
// permission error into a convincing "missing", so callers pulled (or fell
// back to stale local copies) against a Docker connection that was broken
// to begin with. Only a stderr proving "no such image" is a miss now;
// anything else fails closed as an error.
func (c *Client) ImageExists(ctx context.Context, image string) (bool, error) {
	cmd := fmt.Sprintf(
		`err=$(mktemp); if docker image inspect %s >/dev/null 2>"$err"; then st=exists; elif grep -qi 'no such image' "$err"; then st=missing; else echo 'docker image inspect failed:' >&2; cat "$err" >&2; rm -f "$err"; exit 1; fi; rm -f "$err"; printf '%%s\n' "$st"`,
		ssh.ShellQuote(image),
	)
	out, err := c.exec.Run(ctx, cmd)
	if err != nil {
		return false, fmt.Errorf("checking for local image %s: %w", image, err)
	}
	return strings.TrimSpace(out) == "exists", nil
}

// ContainerImageDigest returns Docker's content-addressed image ID for a
// running or stopped container. A value is returned only when Docker proves a
// sha256 identity; callers should leave release metadata empty otherwise.
func (c *Client) ContainerImageDigest(ctx context.Context, container string) (string, error) {
	out, err := c.exec.Run(ctx, "docker inspect -f '{{.Image}}' "+ssh.ShellQuote(container))
	if err != nil {
		return "", fmt.Errorf("inspecting container image digest: %w", err)
	}
	digest := strings.TrimSpace(out)
	if !strings.HasPrefix(digest, "sha256:") || len(digest) != len("sha256:")+64 {
		return "", fmt.Errorf("docker returned no content-addressed image digest")
	}
	return digest, nil
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
//
// This is the LEGACY fallback for releases without a recorded primary port
// (TCL-14): a container publishing MULTIPLE distinct host ports (publish:
// entries) has no inspect-derived primary, and the old first-field pick
// could probe an auxiliary listener — that ambiguity is now an error, and
// callers prefer the record's designated primary (HostPortFor).
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
	distinct := map[string]bool{}
	for _, f := range fields {
		distinct[f] = true
	}
	if len(distinct) > 1 {
		return 0, fmt.Errorf("container %s publishes multiple host ports (%s) and has no recorded primary — its release record (TCL-14) is required to pick one", name, strings.Join(fields, ","))
	}
	port, err := strconv.Atoi(fields[0])
	if err != nil {
		return 0, fmt.Errorf("parsing host port %q from container %s: %w", fields[0], name, err)
	}
	return port, nil
}

// HostBindIP returns the host IP a container's first published port is bound
// to ("0.0.0.0" for all interfaces, or a specific address).
//
// Needed because a container published on a specific IP is not reachable at
// localhost, so anything probing it — health checks on rollback and restart —
// must dial the address docker actually bound. Returns "" when it cannot be
// determined, which callers treat as "assume localhost", the historical
// behavior.
func (c *Client) HostBindIP(ctx context.Context, name string) string {
	out, err := c.exec.Run(ctx, fmt.Sprintf(
		"docker inspect -f '{{range $p, $b := .NetworkSettings.Ports}}{{range $b}}{{.HostIp}} {{end}}{{end}}' %s",
		ssh.ShellQuote(name),
	))
	if err != nil {
		return ""
	}
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return ""
	}
	// A container whose ports bind DIFFERENT addresses has no single
	// answer (A21) — report "cannot determine" rather than picking one.
	for _, f := range fields {
		if f != fields[0] {
			return ""
		}
	}
	return fields[0]
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
	distinct := map[string]bool{}
	for _, f := range fields {
		distinct[f] = true
	}
	// teploy containers publish one primary port, but publish: entries and
	// multi-EXPOSE images make multi-port containers real — picking the
	// first field could route Caddy at an auxiliary listener (A21). The
	// record's designated primary (TCL-14) is the authority; inspection
	// alone must refuse the guess.
	if len(distinct) > 1 {
		return 0, fmt.Errorf("container %s exposes multiple ports (%s) and has no recorded primary — its release record (TCL-14) is required to pick one", name, strings.Join(fields, ","))
	}
	portStr, _, _ := strings.Cut(fields[0], "/")
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return 0, fmt.Errorf("parsing port %q from container %s: %w", fields[0], name, err)
	}
	return port, nil
}

// ListContainers returns all containers for the given app, including stopped ones.
func (c *Client) ListContainers(ctx context.Context, app string) ([]Container, error) {
	// Labels are requested as a structured JSON object ({{json .Labels}}),
	// not through `{{json .}}` — whose Labels field renders docker's
	// comma-joined DISPLAY string. Splitting that display at commas cannot
	// distinguish separators from commas inside values, so an unrelated
	// label like "note=text,teploy.version=bad" forged a reserved teploy
	// label in the parsed map and steered rollback/prune at the wrong
	// containers (audit T15).
	cmd := "docker ps --all --filter label=teploy.app=" + ssh.ShellQuote(app) +
		` --format '{"ID":{{json .ID}},"Names":{{json .Names}},"Image":{{json .Image}},"State":{{json .State}},"Status":{{json .Status}},"CreatedAt":{{json .CreatedAt}},"Labels":{{json .Labels}}}'`
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
// Returns the list of versions whose containers were all removed (A25: a
// version with a failed container removal is NOT reported as pruned) plus
// a joined error describing every failed removal. Per-version image
// removal stays best-effort (a shared image legitimately refuses).
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
	var failures []error
	for _, e := range sorted {
		if protect[e.version] {
			continue
		}
		// Force-remove containers in case any are still running. We
		// already took ownership of cleanup; refusing to nuke a stray
		// running container from an older version defeats the point.
		// A version counts as pruned ONLY when every container removal
		// succeeded (A25) — the old loop counted it regardless, so
		// "Pruned N" could report versions whose containers were still
		// running, hiding disk exhaustion and failed cleanup.
		complete := true
		for _, name := range e.info.containerNames {
			if _, err := c.exec.Run(ctx, "docker rm -f "+ssh.ShellQuote(name)); err != nil {
				complete = false
				failures = append(failures, fmt.Errorf("removing container %s (version %s): %w", name, e.version, err))
			}
		}
		// Best-effort image removal. Fails (silently) if another
		// container or tag still references the image, which is the
		// safe behavior.
		for img := range e.info.images {
			_, _ = c.exec.Run(ctx, "docker rmi "+ssh.ShellQuote(img))
		}
		if complete {
			pruned = append(pruned, e.version)
		}
	}
	return pruned, errors.Join(failures...)
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

// psEntry matches the structured per-container JSON emitted by
// ListContainers' custom --format. Labels arrive as a JSON OBJECT (or, for
// legacy callers/tests, docker's comma-separated display string).
type psEntry struct {
	ID        string          `json:"ID"`
	Names     string          `json:"Names"`
	Image     string          `json:"Image"`
	State     string          `json:"State"`
	Status    string          `json:"Status"`
	CreatedAt string          `json:"CreatedAt"`
	Labels    json.RawMessage `json:"Labels"`
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

		labels, err := parseEntryLabels(entry.Labels)
		if err != nil {
			return nil, fmt.Errorf("parsing labels of %s: %w", entry.Names, err)
		}

		containers = append(containers, Container{
			ID:        entry.ID,
			Name:      entry.Names,
			Image:     entry.Image,
			State:     entry.State,
			Status:    entry.Status,
			CreatedAt: entry.CreatedAt,
			Labels:    labels,
		})
	}
	return containers, nil
}

// parseEntryLabels decodes the Labels field, which is authoritative as a
// JSON object. The legacy string form (docker's comma-joined display) is
// still accepted for backward compatibility with old-format producers, but
// ListContainers itself never emits it — the display string is inherently
// ambiguous (audit T15).
func parseEntryLabels(raw json.RawMessage) (map[string]string, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}
	if trimmed[0] == '{' {
		var labels map[string]string
		if err := json.Unmarshal(raw, &labels); err != nil {
			return nil, err
		}
		return labels, nil
	}
	var display string
	if err := json.Unmarshal(raw, &display); err != nil {
		return nil, err
	}
	return parseLabelsDisplay(display), nil
}

// parseLabelsDisplay splits docker's legacy comma-separated "k=v,k=v"
// display string into a map. Values containing commas break this — which is
// exactly why ListContainers no longer produces this form (audit T15).
func parseLabelsDisplay(s string) map[string]string {
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

// clearStoppedContainer removes a container occupying name when it is stopped,
// and refuses when it is running.
//
// Named separately from Run so the distinction is testable: "there is nothing
// there", "there is a corpse — clear it", and "there is a live workload — stop"
// are three different answers, and the third must never be silently treated as
// the second.
func (c *Client) clearStoppedContainer(ctx context.Context, name string) error {
	out, err := c.exec.Run(ctx, "docker inspect -f '{{.State.Running}}' "+ssh.ShellQuote(name))
	if err != nil {
		// No such container — the common case, and nothing to do.
		return nil
	}
	switch strings.TrimSpace(out) {
	case "true":
		return fmt.Errorf("container %s is already running — this exact version is live; "+
			"stop it first, or deploy a different version", name)
	case "false":
		if _, err := c.exec.Run(ctx, "docker rm "+ssh.ShellQuote(name)); err != nil {
			return fmt.Errorf("removing the stopped container occupying the name %s: %w", name, err)
		}
		return nil
	default:
		// Inspect returned something unexpected. Leave it alone and let
		// `docker run` produce its own error rather than removing on a guess.
		return nil
	}
}
