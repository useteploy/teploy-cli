package deploy

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/useteploy/teploy/internal/caddy"
	"github.com/useteploy/teploy/internal/docker"
	"github.com/useteploy/teploy/internal/ssh"
	"github.com/useteploy/teploy/internal/state"
)

// Config holds all parameters for a deploy.
type Config struct {
	App         string
	Domain      string
	Image       string
	Version     string // short git hash or tag
	EnvFile     string
	Env         map[string]string
	Volumes     map[string]string
	Cmd         string            // command override for single-process deploys
	Processes   map[string]string // process_name -> command (overrides Cmd)
	// NoHealthcheck disables the container HEALTHCHECK for specific processes.
	// Keyed by process name; true means pass --no-healthcheck to docker run.
	// Used when a process shouldn't be probed by the image's built-in
	// healthcheck (e.g. a worker that shares an image with web but has no
	// HTTP listener for the image's curl probe to hit).
	NoHealthcheck map[string]bool
	// KeepVersions caps the number of past app versions retained on the
	// server after a successful deploy. Zero (default) keeps every version
	// — the historical behavior, in which Teploy stopped but never removed
	// old containers, letting disk usage climb across deploys. Set this to
	// 2 or 3 to keep the current version + a small rollback window and
	// have superseded versions auto-pruned (containers + images).
	// Versions are scored by newest container creation timestamp; the
	// current and immediately-previous versions are always protected even
	// if older than other versions on disk.
	KeepVersions int
	// Ingress selects the routing layer; see config.IngressCaddy /
	// IngressExternal. Empty defaults to caddy. When external, the
	// deployer skips every Caddy interaction (route update, reload) —
	// the user's external ingress is responsible for getting traffic
	// to the container. The container still joins the teploy network
	// with its app-name alias and is reachable from cloudflared,
	// nginx, etc. inside the same network.
	Ingress string
	// Bind is the host IP that ingress:host publishes the fixed port on
	// (default 0.0.0.0 for host ingress). Ignored for caddy/external.
	Bind          string
	Memory        string
	CPU           string
	ContainerPort int // internal container port (default 80)
	StopTimeout   int // graceful shutdown seconds (default 10)
	Replicas      int // web process replicas per server (default 1)
	Health      HealthConfig
	PreDeploy   string // hook: runs in web container before traffic switch (failure aborts)
	PostDeploy  string // hook: runs in web container after traffic switch (failure warns)
	AssetPath     string // container path for asset bridging (e.g. "/app/public/assets")
	AssetKeepDays int    // cleanup bridged assets older than N days (default 7)
	// TLSCert / TLSKey are container-side cert/key paths for terminating TLS
	// on the Caddy site block (custom cert instead of ACME). Empty = ACME.
	// The CLI uploads the local cert files and sets these to their on-server
	// container paths (e.g. /etc/caddy/tls/<app>.crt).
	TLSCert string
	TLSKey  string
}

// Deployer orchestrates zero-downtime deploys.
type Deployer struct {
	exec   ssh.Executor
	docker *docker.Client
	caddy  *caddy.Client
	out    io.Writer
}

// NewDeployer creates a new deploy orchestrator.
func NewDeployer(exec ssh.Executor, out io.Writer) *Deployer {
	return &Deployer{
		exec:   exec,
		docker: docker.NewClient(exec),
		caddy:  caddy.NewClient(exec),
		out:    out,
	}
}

// Deploy performs a zero-downtime deploy.
//
// Flow: lock → start web → health check → start workers → route traffic →
// write state → stop old containers → log → unlock.
func (d *Deployer) Deploy(ctx context.Context, cfg Config) error {
	if cfg.App == "" || cfg.Image == "" || cfg.Version == "" {
		return fmt.Errorf("app, image, and version are required")
	}
	// Caddy/external ingress route by domain; host ingress publishes a raw
	// port and needs no domain.
	if cfg.Domain == "" && !cfg.ingressHost() {
		return fmt.Errorf("domain is required")
	}

	stopTimeout := cfg.StopTimeout
	if stopTimeout == 0 {
		stopTimeout = 10
	}

	// Determine processes. Default: single web process with image CMD.
	processes := cfg.Processes
	if len(processes) == 0 {
		processes = map[string]string{"web": cfg.Cmd}
	}

	replicas := cfg.Replicas
	if replicas <= 0 {
		replicas = 1
	}

	// Host ingress publishes the fixed port directly; default to all
	// interfaces so it's reachable at IP:port (caddy/external leave this empty
	// and fall back to the 127.0.0.1 health-check binding in docker.Run).
	webBindHost := cfg.Bind
	if cfg.ingressHost() && webBindHost == "" {
		webBindHost = "0.0.0.0"
	}

	start := time.Now()

	// 1. Ensure app directory exists.
	if replicas > 1 {
		fmt.Fprintf(d.out, "Deploying %s (version %s, %d replicas)...\n", cfg.App, cfg.Version, replicas)
	} else {
		fmt.Fprintf(d.out, "Deploying %s (version %s)...\n", cfg.App, cfg.Version)
	}
	if err := state.EnsureAppDir(ctx, d.exec, cfg.App); err != nil {
		return fmt.Errorf("creating app directory: %w", err)
	}

	// 2. Acquire deploy lock.
	if err := state.AcquireLock(ctx, d.exec, cfg.App); err != nil {
		return err
	}
	defer state.ReleaseLock(ctx, d.exec, cfg.App)

	// 3. Read current state.
	current, _ := state.Read(ctx, d.exec, cfg.App)

	// 4. Determine host ports for all web replicas.
	var ports []int
	if cfg.ingressHost() {
		// Host ingress publishes on a FIXED host port (= the container port) so
		// the app stays reachable at a stable bind:port. A fixed port can't be
		// blue/green (two containers can't bind it), so host mode recreates:
		// existing web containers are removed below before the new one starts.
		ports = []int{cfg.ContainerPort}
		fmt.Fprintf(d.out, "Publishing on %s:%d (host ingress)...\n", webBindHost, cfg.ContainerPort)
	} else {
		// Allocate ephemeral ports for blue/green. Track the ports claimed so
		// far — containers aren't started until step 6, so `ss` can't see them
		// yet; without excluding already-claimed ports every replica would get
		// the same one and the second replica's `docker run -p` would collide.
		fmt.Fprintf(d.out, "Allocating %d port(s)...\n", replicas)
		ports = make([]int, replicas)
		claimed := make(map[int]bool, replicas)
		for i := 0; i < replicas; i++ {
			port, err := d.docker.FindAvailablePortExcluding(ctx, claimed)
			if err != nil {
				return fmt.Errorf("allocating port %d/%d: %w", i+1, replicas, err)
			}
			ports[i] = port
			claimed[port] = true
		}
		fmt.Fprintf(d.out, "  Ports allocated: %v\n", ports)
	}
	port := ports[0] // primary port for health check, hooks, etc.

	// 5. Asset bridging: extract assets from image before starting the container.
	if cfg.AssetPath != "" {
		hostAssetDir := fmt.Sprintf("/deployments/%s/assets", cfg.App)
		fmt.Fprintln(d.out, "Bridging assets...")
		if _, err := d.exec.Run(ctx, "mkdir -p "+hostAssetDir); err != nil {
			return fmt.Errorf("creating asset bridge directory: %w", err)
		}

		// Extract assets from image using a one-shot container.
		// --user 0 (root) so the cp can write into the host-owned
		// /deployments/<app>/assets dir without permission denied,
		// regardless of the image's USER directive. Drop `2>/dev/null`
		// from cp so genuine failures surface in the deploy output —
		// the previous silent-fail mode meant an empty host volume
		// was bind-mounted over the in-image static dir, hiding all
		// files and serving 404 for every static asset.
		extractCmd := fmt.Sprintf(
			"docker run --rm --user 0 -v %s:/bridge %s sh -c 'cp -r %s/. /bridge/ && echo ok-bridge'",
			hostAssetDir, cfg.Image, cfg.AssetPath,
		)
		out, err := d.exec.Run(ctx, extractCmd)
		if err != nil || !strings.Contains(out, "ok-bridge") {
			return fmt.Errorf("asset extraction failed: %s", strings.TrimSpace(out))
		}
		fmt.Fprintln(d.out, "  Assets extracted to host")

		// Mount the shared asset directory into the container.
		if cfg.Volumes == nil {
			cfg.Volumes = map[string]string{}
		}
		cfg.Volumes[hostAssetDir] = cfg.AssetPath
	}

	// 6. Handle same-version redeploy: rename existing containers to avoid name conflicts.
	if current != nil && current.CurrentHash == cfg.Version {
		for _, process := range sortedProcessNames(processes) {
			for ri := 1; ri <= replicas; ri++ {
				name := docker.ReplicaContainerName(cfg.App, process, cfg.Version, ri, replicas)
				d.exec.Run(ctx, fmt.Sprintf("docker rename %s %s 2>/dev/null", name, name+"_replaced"))
			}
			// Also rename the non-indexed name (from pre-replica deploys).
			name := docker.ContainerName(cfg.App, process, cfg.Version)
			d.exec.Run(ctx, fmt.Sprintf("docker rename %s %s 2>/dev/null", name, name+"_replaced"))
		}
	}

	// Host ingress recreates rather than blue/greens: the new container reuses
	// the old one's fixed host port, so remove existing web containers for this
	// app first to free it. This is the ~few-seconds downtime window inherent
	// to a stable single host port.
	if cfg.ingressHost() {
		ids, _ := d.exec.Run(ctx, fmt.Sprintf(
			"docker ps -aq --filter label=teploy.app=%s --filter label=teploy.process=web", cfg.App))
		for _, id := range strings.Fields(ids) {
			d.docker.Stop(ctx, id, stopTimeout)
			d.docker.Remove(ctx, id)
		}
	}

	// Track started containers for cleanup on failure.
	var started []string
	webContainerNames := make([]string, replicas)

	// 6. Start web container(s).
	for i := 0; i < replicas; i++ {
		name := docker.ReplicaContainerName(cfg.App, "web", cfg.Version, i+1, replicas)
		webContainerNames[i] = name
		fmt.Fprintf(d.out, "Starting container %s (port %d)...\n", name, ports[i])
		containerID, err := d.docker.Run(ctx, docker.RunConfig{
			App:           cfg.App,
			Process:       "web",
			Version:       cfg.Version,
			Image:         cfg.Image,
			Port:          ports[i],
			BindHost:      webBindHost,
			ContainerPort: cfg.ContainerPort,
			EnvFile:       cfg.EnvFile,
			Env:           cfg.Env,
			Volumes:       cfg.Volumes,
			Cmd:           processes["web"],
			Memory:        cfg.Memory,
			CPU:           cfg.CPU,
			Name:          name,
			NoHealthcheck: cfg.NoHealthcheck["web"],
		})
		if err != nil {
			return fmt.Errorf("starting container %s: %w", name, err)
		}
		started = append(started, name)
		fmt.Fprintf(d.out, "  Container %s started\n", containerID[:min(12, len(containerID))])
	}

	// Primary web container (first replica) used for hooks.
	webContainerName := webContainerNames[0]

	// From here on, failures must clean up all started containers.
	fail := func(reason error) error {
		logs, _ := d.exec.Run(ctx, fmt.Sprintf("docker logs --tail 50 %s 2>&1", webContainerName))
		if logs != "" {
			fmt.Fprintf(d.out, "\n--- Container logs ---\n%s\n--- End logs ---\n", logs)
		}
		for _, n := range started {
			d.docker.Stop(ctx, n, 5)
			d.docker.Remove(ctx, n)
		}
		d.logDeploy(ctx, cfg, false, start)
		return reason
	}

	// 7. Verify all web containers are running (catch immediate crashes).
	for i, name := range webContainerNames {
		statusOut, err := d.exec.Run(ctx, fmt.Sprintf(
			"docker inspect -f '{{.State.Status}}' %s", name,
		))
		if err != nil || strings.TrimSpace(statusOut) != "running" {
			return fail(fmt.Errorf("container failed to start (status: %s, name: %s)", strings.TrimSpace(statusOut), name))
		}
		_ = i
	}

	// 8. Pre-deploy hook (runs in primary web container before health check).
	if cfg.PreDeploy != "" {
		fmt.Fprintf(d.out, "Running pre-deploy hook...\n")
		if output, err := d.docker.Exec(ctx, webContainerName, cfg.PreDeploy); err != nil {
			if output != "" {
				fmt.Fprintf(d.out, "  %s\n", output)
			}
			return fail(fmt.Errorf("pre-deploy hook failed: %w", err))
		}
		fmt.Fprintln(d.out, "  Pre-deploy hook passed")
	}

	// 9. Health check all web replicas.
	fmt.Fprintln(d.out, "Running health check...")
	healthCfg := cfg.Health.withDefaults()
	for i, p := range ports {
		if err := d.healthCheck(ctx, p, healthCfg); err != nil {
			fmt.Fprintf(d.out, "  Health check failed for replica %d (port %d): %v\n", i+1, p, err)
			return fail(fmt.Errorf("health check failed for replica %d: %w", i+1, err))
		}
	}
	if replicas > 1 {
		fmt.Fprintf(d.out, "  All %d replicas healthy\n", replicas)
	} else {
		fmt.Fprintln(d.out, "  Health check passed")
	}

	// 10. Start non-web process containers (workers, etc. — no replicas, one each).
	for _, process := range sortedProcessNames(processes) {
		if process == "web" {
			continue
		}
		name := docker.ContainerName(cfg.App, process, cfg.Version)
		fmt.Fprintf(d.out, "Starting %s...\n", name)
		_, err := d.docker.Run(ctx, docker.RunConfig{
			App:           cfg.App,
			Process:       process,
			Version:       cfg.Version,
			Image:         cfg.Image,
			Port:          0, // non-web processes don't get a port
			EnvFile:       cfg.EnvFile,
			Env:           cfg.Env,
			Volumes:       cfg.Volumes,
			Cmd:           processes[process],
			Memory:        cfg.Memory,
			CPU:           cfg.CPU,
			NoHealthcheck: cfg.NoHealthcheck[process],
		})
		if err != nil {
			return fail(fmt.Errorf("starting %s: %w", name, err))
		}
		started = append(started, name)
	}

	// 11. Update Caddy route to point at new web container(s).
	// Use container names with the internal container port (not host-mapped ports),
	// since Caddy and app containers communicate over the Docker network.
	//
	// Skipped entirely under ingress: external — the user's CF Tunnel /
	// nginx / etc. reaches the container by its app-name alias on the
	// teploy docker network, so Teploy has nothing to do here.
	if cfg.usesCaddy() {
		fmt.Fprintln(d.out, "Updating routes...")
		tls := caddy.TLS{Cert: cfg.TLSCert, Key: cfg.TLSKey}
		if replicas > 1 {
			upstreams := make([]caddy.Upstream, replicas)
			for i := range replicas {
				upstreams[i] = caddy.Upstream{Dial: fmt.Sprintf("%s:%d", webContainerNames[i], cfg.ContainerPort)}
			}
			if err := d.caddy.SetLoadBalancer(ctx, cfg.App, cfg.Domain, upstreams, tls); err != nil {
				return fail(fmt.Errorf("updating load balancer route: %w", err))
			}
			fmt.Fprintf(d.out, "  Traffic load-balanced across %d replicas\n", replicas)
		} else {
			if err := d.caddy.SetRoute(ctx, cfg.App, cfg.Domain, webContainerName, cfg.ContainerPort, tls); err != nil {
				return fail(fmt.Errorf("updating route: %w", err))
			}
			fmt.Fprintln(d.out, "  Traffic routed to new container")
		}
	} else {
		fmt.Fprintf(d.out, "Skipping Caddy route update (ingress: %s)\n", cfg.Ingress)
	}

	// 12. Post-deploy hook (runs in web container after traffic switch — failure warns, no rollback).
	if cfg.PostDeploy != "" {
		fmt.Fprintf(d.out, "Running post-deploy hook...\n")
		if output, err := d.docker.Exec(ctx, webContainerName, cfg.PostDeploy); err != nil {
			fmt.Fprintf(d.out, "  Warning: post-deploy hook failed: %v\n", err)
			if output != "" {
				fmt.Fprintf(d.out, "  %s\n", output)
			}
		} else {
			fmt.Fprintln(d.out, "  Post-deploy hook passed")
		}
	}

	// 13. Write new state.
	newState := &state.AppState{
		CurrentPort:  port,
		CurrentPorts: ports,
		CurrentHash:  cfg.Version,
		Domain:       cfg.Domain,
	}
	if current != nil {
		newState.PreviousPort = current.CurrentPort
		newState.PreviousPorts = current.CurrentPorts
		newState.PreviousHash = current.CurrentHash
	}
	stateErr := state.Write(ctx, d.exec, cfg.App, newState)
	if stateErr != nil {
		fmt.Fprintf(d.out, "Warning: writing state failed: %v\n", stateErr)
	}

	// 14. Stop old containers (all processes + all replicas).
	if current != nil && current.CurrentHash != "" {
		// Stop old web replicas.
		oldReplicas := len(current.CurrentPorts)
		if oldReplicas == 0 {
			oldReplicas = 1
		}
		for ri := 1; ri <= oldReplicas; ri++ {
			oldName := docker.ReplicaContainerName(cfg.App, "web", current.CurrentHash, ri, oldReplicas)
			if current.CurrentHash == cfg.Version {
				oldName += "_replaced"
			}
			fmt.Fprintf(d.out, "Stopping old container %s...\n", oldName)
			d.docker.Stop(ctx, oldName, stopTimeout)
		}
		// Also stop non-indexed name (from pre-replica deploys).
		if oldReplicas <= 1 {
			oldName := docker.ContainerName(cfg.App, "web", current.CurrentHash)
			if current.CurrentHash == cfg.Version {
				oldName += "_replaced"
			}
			d.docker.Stop(ctx, oldName, stopTimeout)
		}
		// Stop old worker processes (always 1 per type).
		for _, process := range sortedProcessNames(processes) {
			if process == "web" {
				continue
			}
			oldName := docker.ContainerName(cfg.App, process, current.CurrentHash)
			if current.CurrentHash == cfg.Version {
				oldName += "_replaced"
			}
			fmt.Fprintf(d.out, "Stopping old container %s...\n", oldName)
			d.docker.Stop(ctx, oldName, stopTimeout)
		}
	}

	if stateErr != nil {
		return fmt.Errorf("writing state: %w", stateErr)
	}

	// 15. Clean up old bridged assets.
	if cfg.AssetPath != "" {
		keepDays := cfg.AssetKeepDays
		if keepDays <= 0 {
			keepDays = 7
		}
		cleanCmd := fmt.Sprintf(
			"find /deployments/%s/assets -type f -mtime +%d -delete 2>/dev/null || true",
			cfg.App, keepDays,
		)
		d.exec.Run(ctx, cleanCmd)
	}

	// 15b. Prune superseded app versions (containers + images) if the
	// operator opted in via keep_versions. Always protects the current
	// version and the immediately-previous version so a rollback target
	// is preserved regardless of timestamp ordering.
	if cfg.KeepVersions > 0 {
		var prevHash string
		if current != nil {
			prevHash = current.CurrentHash
		}
		pruned, err := d.docker.PruneVersions(ctx, cfg.App, cfg.KeepVersions, cfg.Version, prevHash)
		if err != nil {
			fmt.Fprintf(d.out, "Warning: version prune failed: %v\n", err)
		} else if len(pruned) > 0 {
			fmt.Fprintf(d.out, "Pruned %d superseded version(s): %s\n", len(pruned), strings.Join(pruned, ", "))
		}
	}

	// 16. Log success.
	d.logDeploy(ctx, cfg, true, start)

	duration := time.Since(start)
	fmt.Fprintf(d.out, "\nDeployed %s version %s in %s\n", cfg.App, cfg.Version, duration.Round(time.Millisecond))
	return nil
}

func (d *Deployer) logDeploy(ctx context.Context, cfg Config, success bool, start time.Time) {
	state.AppendLog(ctx, d.exec, state.LogEntry{
		Timestamp:  time.Now().UTC(),
		App:        cfg.App,
		Type:       "deploy",
		Hash:       cfg.Version,
		Success:    success,
		DurationMs: time.Since(start).Milliseconds(),
	})
}

// sortedProcessNames returns process names with "web" first, then alphabetical.
func sortedProcessNames(processes map[string]string) []string {
	var others []string
	for name := range processes {
		if name != "web" {
			others = append(others, name)
		}
	}
	sort.Strings(others)
	return append([]string{"web"}, others...)
}

// usesCaddy reports whether Teploy should drive Caddy for this deploy.
// Default (empty Ingress) and "caddy" both return true; only "external"
// turns off all Caddy interactions.
func (c Config) usesCaddy() bool {
	return c.Ingress == "" || c.Ingress == "caddy"
}

// ingressHost reports whether the app publishes directly on a fixed host
// port (no Caddy, recreate instead of blue/green). See config.IngressHost.
func (c Config) ingressHost() bool {
	return c.Ingress == "host"
}
