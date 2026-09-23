package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/useteploy/teploy/internal/caddy"
	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/docker"
	"github.com/useteploy/teploy/internal/releasemeta"
	"github.com/useteploy/teploy/internal/ssh"
	"github.com/useteploy/teploy/internal/state"
)

// Config holds all parameters for a deploy.
type Config struct {
	App       string
	Domain    string
	Image     string
	Version   string   // short git hash or tag
	EnvFiles  []string // paths to env files on the server, applied in order (later files' keys win) — see docker.RunConfig.EnvFiles
	Env       map[string]string
	Volumes   map[string]string
	Cmd       string            // command override for single-process deploys
	Processes map[string]string // process_name -> command (overrides Cmd)
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
	// Publish adds extra verbatim docker -p mappings beyond ContainerPort's own
	// (e.g. "0.0.0.0:3001:3001"), for an app with a second listener that needs
	// its own host port. See AppConfig.Publish.
	Publish       []string
	StopTimeout   int // graceful shutdown seconds (default 10)
	Replicas      int // web process replicas per server (default 1)
	Health        HealthConfig
	PreDeploy     string // hook: runs in web container before traffic switch (failure aborts)
	PostDeploy    string // hook: runs in web container after traffic switch (failure warns)
	AssetPath     string // container path for asset bridging (e.g. "/app/public/assets")
	AssetKeepDays int    // cleanup bridged assets older than N days (default 7)
	// TLSCert / TLSKey are container-side cert/key paths for terminating TLS
	// on the Caddy site block (custom cert instead of ACME). Empty = ACME.
	// The CLI uploads the local cert files and sets these to their on-server
	// container paths (e.g. /etc/caddy/tls/<app>.crt).
	TLSCert string
	TLSKey  string
	// TLSInternal requests Caddy's own local CA (self-signed) instead of a
	// custom cert or ACME — see config.TLSConfig.Internal. Mutually
	// exclusive with TLSCert/TLSKey (enforced at config.validate() time).
	TLSInternal     bool
	CaddyExtra      string            // raw Caddy directives appended into the site block
	Cache           map[string]string // path glob -> Cache-Control, rendered into the site block
	Firewall        caddy.Firewall    // edge hardening (IP allow/deny, UA block, body cap)
	Access          caddy.Access      // inbound access gate (basic auth / forward auth)
	ManifestSHA256  string
	AppliedManifest json.RawMessage
	SourceRevision  string
	// Provenance is the plan-time provenance record (C04) the CLI
	// resolved before execution: revision, worktree cleanliness, build
	// context fingerprint, Dockerfile identity, platform, image digest
	// and mutability. Optional (direct construction without provenance
	// skips the plan/receipt equality machinery); when present its
	// identity must match the deploy's own.
	Provenance *releasemeta.Provenance
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

// validVersion matches release ids the deploy accepts (the same grammar
// releasemeta uses for meta file names — git short hashes, tags like
// v1.2.3, sha256-<hex> image labels).
var validVersion = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)

// validProcessName keeps process names safe as container-name segments.
var validProcessName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$`)

// validate checks the deploy config's required fields. This is the shared
// execution-plan validator: direct/ad-hoc construction (multideploy,
// preview, autodeploy) does not pass through config-file parsing, so the
// bounds enforced there cannot be assumed here (TCL-18). Identity grammar
// (app, version, process names) and the ingress enum are checked too, so
// no matter how a Config was built, its values are safe to interpolate
// into container names, remote paths, and shell text (audit A17).
func (c Config) validate() error {
	if err := config.ValidateName(c.App); err != nil {
		return err
	}
	if c.Image == "" {
		return fmt.Errorf("image is required")
	}
	if !validVersion.MatchString(c.Version) {
		return fmt.Errorf("invalid version %q — must be alphanumeric with . _ - (max 128 chars)", c.Version)
	}
	for process := range c.Processes {
		if !validProcessName.MatchString(process) {
			return fmt.Errorf("invalid process name %q", process)
		}
	}
	switch c.Ingress {
	case "", "caddy", "external", "host":
	default:
		return fmt.Errorf("unknown ingress mode %q (expected caddy, external, or host)", c.Ingress)
	}
	// Caddy/external ingress route by domain; host ingress publishes a raw
	// port and needs no domain.
	if c.Domain == "" && !c.ingressHost() {
		return fmt.Errorf("domain is required")
	}
	// 0 means "default" (80 at the docker layer); anything else must be a
	// real port.
	if c.ContainerPort < 0 || c.ContainerPort > 65535 {
		return fmt.Errorf("container port must be in 1..65535 (got %d)", c.ContainerPort)
	}
	if c.Replicas < 0 || c.Replicas > 1000 {
		return fmt.Errorf("replicas must be in 1..1000 (got %d)", c.Replicas)
	}
	// A fixed host port cannot be shared across containers — mirror the
	// config-layer rejection so a directly constructed Config cannot ask
	// for a deploy that self-collides. Publish entries are fixed ports for
	// the same reason host ingress is (A17): replicas>1 with publish would
	// die on "port is already allocated" mid-deploy.
	if (c.ingressHost() || len(c.Publish) > 0) && c.Replicas > 1 {
		return fmt.Errorf("host ingress and fixed publish ports support a single replica (a fixed host port can't be load-balanced across containers)")
	}
	// Publish grammar + duplicate-binding preflight (T23): direct
	// construction bypasses config-file parsing, so the shared validator
	// runs here too — malformed entries used to reach docker after the
	// fixed-port predecessor had already been stopped.
	if len(c.Publish) > 0 {
		if err := config.ValidatePublishEntries(c.Publish); err != nil {
			return err
		}
		if c.ingressHost() {
			for _, p := range c.Publish {
				if spec, err := config.ParsePublishSpec(p); err == nil && spec.HostPort != 0 && spec.HostPort == containerPort(c) {
					return fmt.Errorf("'publish' entry %q binds host port %d, which is already the app's fixed ingress:host port", p, spec.HostPort)
				}
			}
		}
	}
	if c.StopTimeout < 0 {
		return fmt.Errorf("stop timeout cannot be negative (got %ds)", c.StopTimeout)
	}
	// Health probe mode enum (C03): config-file parsing enforces the fuller
	// grammar (tcp rejects a path); the shared execution validator covers
	// the enum so directly constructed Configs (fleet, preview, autodeploy)
	// cannot carry an unknown mode into the gate dispatch. Empty = auto
	// (documented compat).
	switch c.Health.Mode {
	case "", HealthModeHTTP, HealthModeTCP, HealthModeAuto:
	default:
		return fmt.Errorf("unknown health mode %q (expected http, tcp, or auto)", c.Health.Mode)
	}
	// Provenance identity (C04): a record describing another deploy than
	// the Config it rides on is a lie that would corrupt the plan/receipt
	// equality surfaces — refused before any effect.
	if c.Provenance != nil && (c.Provenance.App != c.App || c.Provenance.Release != c.Version) {
		return fmt.Errorf("provenance identity mismatch: provenance describes %s@%s, deploy is %s@%s", c.Provenance.App, c.Provenance.Release, c.App, c.Version)
	}
	return nil
}

// Deploy performs a zero-downtime deploy, acquiring the app lock for the
// duration. Callers that already hold the app lock (the autodeploy path,
// which locks before fetching so the checkout can't race a concurrent
// trigger) must call DeployFenced with their lock handle instead — the mkdir
// lock is not reentrant, so acquiring it twice fails (audit TCL-01).
//
// Flow: lock → start web → health check → start workers → route traffic →
// write state → stop old containers → log → unlock.
func (d *Deployer) Deploy(ctx context.Context, cfg Config) error {
	if err := cfg.validate(); err != nil {
		return err
	}
	// The lock lives at /deployments/<app>/.lock — its parent must exist
	// before the lock can be acquired, and a first deploy has no app dir
	// yet (TCL-01).
	if err := state.EnsureAppDir(ctx, d.exec, cfg.App); err != nil {
		return fmt.Errorf("creating app directory: %w", err)
	}
	lk, err := state.AcquireLockFenced(ctx, d.exec, cfg.App)
	if err != nil {
		return err
	}
	defer state.ReleaseLockFenced(d.exec, lk, cfg.App)
	// Renewal is what makes the lock's TTL safe for a slow-but-live deploy
	// (F16): the heartbeat keeps the lock fresh, so only a dead holder's
	// lock is ever broken as stale.
	lk.StartRenewal(d.exec)
	return d.DeployFenced(ctx, cfg, lk)
}

// DeployLocked performs a zero-downtime deploy WITHOUT acquiring the app
// lock and WITHOUT fence checks — the pre-F16 shape, kept for callers and
// tests that hold no lock handle. Production callers that already own the
// lock pass the handle to DeployFenced instead, so their effects stay
// fenced.
func (d *Deployer) DeployLocked(ctx context.Context, cfg Config) error {
	return d.DeployFenced(ctx, cfg, nil)
}

// DeployFenced is the deploy body under an explicitly-held lock handle
// (audit F16). lk may be nil (no fencing — see DeployLocked). Fence checks
// precede every effectful phase; recovery paths (restoreDisplacedAndStarted,
// abortStateCommit) are deliberately NOT fenced — refusing to clean up this
// operation's own partial effects is how a fencing design strands an app
// mid-incident.
func (d *Deployer) DeployFenced(ctx context.Context, cfg Config, lk *state.Lock) error {
	if err := cfg.validate(); err != nil {
		return err
	}

	stopTimeout := cfg.StopTimeout
	if stopTimeout == 0 {
		stopTimeout = 10
	}

	// Normalize the execution defaults ONCE and use the normalized value
	// everywhere (A18): Config permits ContainerPort == 0 as "default 80",
	// and the docker layer normalizes it only inside its publishing branch —
	// using the raw zero for host ports or Caddy upstreams produced a ":0"
	// upstream and a publish-less host deploy.
	containerPort := cfg.ContainerPort
	if containerPort == 0 {
		containerPort = 80
	}

	// Pin every container creation this deploy makes to ONE immutable image
	// identity (A52): a mutable tag can be re-pointed by a concurrent
	// pull/build/tag between replica creates, mixing images within a
	// release. Resolution failure warns and falls back to the requested
	// reference (the create would fail against the same daemon anyway).
	runImage := cfg.Image
	if resolved, err := d.docker.ResolveImageID(ctx, cfg.Image); err == nil {
		runImage = resolved
	} else {
		fmt.Fprintf(d.out, "Warning: could not resolve an immutable image ID for %s — creating from the requested reference (%v)\n", cfg.Image, err)
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

	// recreateWeb selects the explicit recreate strategy (audit F21): stop
	// the current web container(s) BEFORE starting the replacement. Host
	// ingress needs it because the fixed published port cannot double-bind;
	// `publish:` entries need it for the same reason — their host ports are
	// operator-configured and identical across versions, so blue/green would
	// just die on "port is already allocated" mid-deploy (previously a clean
	// pre-mutation error; now a deliberate, brief-outage recreate with
	// restore-on-failure, matching host ingress). Validation upstream keeps
	// publish/host ingress single-replica.
	recreateWeb := cfg.ingressHost() || len(cfg.Publish) > 0

	start := time.Now()

	if replicas > 1 {
		fmt.Fprintf(d.out, "Deploying %s (version %s, %d replicas)...\n", cfg.App, cfg.Version, replicas)
	} else {
		fmt.Fprintf(d.out, "Deploying %s (version %s)...\n", cfg.App, cfg.Version)
	}

	// 0b. Surface the execution plan (C04): the immutable image identity
	// this deploy will create containers from, the source revision it
	// builds, and the effective-config digest — BEFORE any effect. The
	// same values are verified against the deployed receipt after the
	// live commit (step 17).
	planDigest := plannedImageDigest(cfg.Image, runImage, cfg.Provenance)
	printDeployPlan(d.out, cfg, planDigest)

	// 1. Read current state. A read failure must stop the deploy — treating
	// an unreadable state file as "no state" loses rollback bookkeeping and
	// makes a replacement deploy look like a first deploy (audit F15).
	// (The caller owns the app lock; DeployLocked never acquires it — a
	// mkdir lock is not reentrant, so a second acquisition fails, which is
	// what broke every Deploy() call until TCL-01.)
	current, err := state.Read(ctx, d.exec, cfg.App)
	if err != nil {
		return fmt.Errorf("refusing to deploy with unreadable state for %s: %w", cfg.App, err)
	}

	// 1b. Converge outstanding record repair debt (C01-6): a previous
	// deploy whose releasemeta record write failed after the live commit
	// left a repair-debt marker. Rebuild that record from the live
	// containers BEFORE this deploy's own work and clear the marker —
	// under the same lock every other state mutation here holds. Never a
	// deploy failure: the debt describes the previous deploy, and on its
	// own failure the marker stays (with the count bumped) for the next
	// one.
	d.repairOutstandingRecordDebt(ctx, cfg.App, current)

	// 1c. Replacement-owner reconciliation (C01-1): when this deploy's
	// lock acquisition BROKE a stale predecessor's lock, the previous
	// holder may have left in-flight effects on the target — acquisition
	// is never proof of quiescence. Decide over the observed evidence
	// BEFORE this deploy's first effect; anything but a clean retry
	// refuses with the evidence so the leftover generation is reconciled
	// deliberately, never blindly redeployed over.
	if lk.TookOver() {
		if err := d.ReconcileAfterTakeover(ctx, cfg, current); err != nil {
			return err
		}
	}

	// 4. Determine host ports for all web replicas.
	var ports []int
	if cfg.ingressHost() {
		// Host ingress publishes on a FIXED host port (= the container port) so
		// the app stays reachable at a stable bind:port. A fixed port can't be
		// blue/green (two containers can't bind it), so host mode recreates:
		// existing web containers are removed below before the new one starts.
		ports = []int{containerPort}
		fmt.Fprintf(d.out, "Publishing on %s:%d (host ingress)...\n", webBindHost, containerPort)
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

	// 5. Asset bridging: extract assets from the image into THIS attempt's
	// private tree before starting the container (audit A15). The old
	// implementation copied straight into the SHARED
	// /deployments/<app>/assets that the running release still reads — a
	// failed candidate had already mutated the live app's files, with no
	// compensation. The attempt-scoped tree is seeded from the previous
	// attempt's tree with a real copy (cp -a, not hardlinks: a later
	// extraction writing through a shared inode would truncate the previous
	// tree's files), and the completed tree is mounted into the candidate.
	// Extraction uses `docker create` + `docker cp`, so no image ENTRYPOINT
	// ever executes (the old `docker run … sh -c` let an image with an
	// ENTRYPOINT wrap or replace the copy command).
	assetAttempt := releasemeta.MustAttempt(cfg.App, cfg.Version)
	if cfg.AssetPath != "" {
		att := assetAttempt
		assetDir := att.Dir() + "/assets"
		fmt.Fprintln(d.out, "Bridging assets...")
		seed := ""
		if prev := releasemeta.PreviousAttemptAssetsDir(ctx, d.exec, cfg.App, att.ID); prev != "" {
			seed = prev
		}
		seedCmd := "mkdir -p " + ssh.ShellQuote(assetDir)
		if seed != "" {
			seedCmd += " && cp -a " + ssh.ShellQuote(seed+"/.") + " " + ssh.ShellQuote(assetDir+"/")
		}
		if _, err := d.exec.Run(ctx, seedCmd); err != nil {
			return fmt.Errorf("creating asset tree: %w", err)
		}

		// One-shot extraction container, removed on every exit. A stale
		// corpse from an interrupted deploy is cleared first (a create with
		// the same name would otherwise fail); the name carries the attempt
		// id, so it can never collide with another attempt's extraction.
		extractContainer := "teploy-assets-" + att.ID
		extractCmd := strings.Join([]string{
			"docker rm -f " + ssh.ShellQuote(extractContainer) + " 2>/dev/null || true",
			"docker create --name " + ssh.ShellQuote(extractContainer) + " " + ssh.ShellQuote(runImage),
			"docker cp " + ssh.ShellQuote(extractContainer+":"+cfg.AssetPath+"/.") + " " + ssh.ShellQuote(assetDir+"/"),
			"rc=$?",
			"docker rm -f " + ssh.ShellQuote(extractContainer) + " >/dev/null 2>&1 || true",
			"exit $rc",
		}, "; ")
		if _, err := d.exec.Run(ctx, extractCmd); err != nil {
			return fmt.Errorf("asset extraction failed: %w", err)
		}
		fmt.Fprintln(d.out, "  Assets extracted to the attempt's private tree")

		// Mount the private tree — into a CLONED volumes map: mutating the
		// caller's map through the Config value copy used to leak the mount
		// into every subsequent use of that map (A15).
		cfg.Volumes = maps.Clone(cfg.Volumes)
		if cfg.Volumes == nil {
			cfg.Volumes = map[string]string{}
		}
		cfg.Volumes[assetDir] = cfg.AssetPath
	}

	// 6. Handle same-version redeploy: rename existing containers to avoid name conflicts.
	// Force-remove any stale _replaced container first — a prior interrupted deploy
	// may have left one behind in Exited state, which would cause the rename to fail
	// silently and leave the live container unrenamed, causing a "name already in use"
	// conflict when docker run tries to start the new container.
	//
	// The candidate names are DEDUPLICATED first: with replicas==1 the replica
	// name and the non-indexed name are the SAME string, and processing both
	// used to (a) rename the live container to _replaced, then (b) remove
	// _replaced and rename again — deleting the running predecessor before its
	// replacement had even started, let alone passed its health check (audit
	// F03).
	sameVersion := current != nil && current.CurrentHash == cfg.Version
	if sameVersion {
		// Fence (F16): the renames below mutate the live workload — a
		// holder whose lock was broken must not touch it.
		if err := lk.Check(ctx, d.exec); err != nil {
			return err
		}
		seen := map[string]bool{}
		for _, process := range sortedProcessNames(processes) {
			for ri := 1; ri <= replicas; ri++ {
				seen[docker.ReplicaContainerName(cfg.App, process, cfg.Version, ri, replicas)] = true
			}
			// Also rename the non-indexed name (from pre-replica deploys).
			seen[docker.ContainerName(cfg.App, process, cfg.Version)] = true
		}
		for name := range seen {
			replaced := name + "_replaced"
			// A RUNNING _replaced container is the serving predecessor a
			// previous failed attempt renamed (audit A08): the unconditional
			// force-remove here used to delete the live workload before the
			// replacement had even started, so a failed same-version retry
			// took the app DOWN. Refuse and name the recovery path instead;
			// a stopped corpse (interrupted deploy, completed redeploy whose
			// remove failed) is still cleared as before.
			if stOut, stErr := d.exec.Run(ctx, "docker inspect -f '{{.State.Status}}' "+ssh.ShellQuote(replaced)+" 2>/dev/null || true"); stErr == nil && strings.TrimSpace(stOut) == "running" {
				return fmt.Errorf("container %s is still running — it is the previous same-version attempt's renamed (serving) workload; refusing to delete it. Restore it first (teploy rollback --app %s) or remove it deliberately", replaced, cfg.App)
			}
			d.exec.Run(ctx, "docker rm -f "+ssh.ShellQuote(replaced)+" 2>/dev/null || true")
			// The rename's failure must not be swallowed (A08): with the
			// predecessor still live under `name`, a silently ignored
			// rename failure made the snapshot and the candidate run disagree
			// about which container holds the workload. Only a confirmed
			// absence of the source is a no-op.
			if _, err := d.exec.Run(ctx, "docker rename "+ssh.ShellQuote(name)+" "+ssh.ShellQuote(replaced)); err != nil {
				if srcOut, srcErr := d.exec.Run(ctx, "docker inspect -f '{{.State.Status}}' "+ssh.ShellQuote(name)+" 2>/dev/null || true"); srcErr != nil || strings.TrimSpace(srcOut) != "" {
					return fmt.Errorf("renaming the current container %s to %s failed: %w — the workload is untouched; inspect the server before retrying", name, replaced, err)
				}
			}
		}
	}

	// 6b. Snapshot the PREDECESSOR workload now — after the same-version
	// renames, but BEFORE any new container starts. The post-commit cleanup
	// (step 14) stops exactly this snapshot. A fresh inventory taken after
	// the candidates are live cannot be selected by the teploy.version
	// label: during a same-version redeploy the replacement carries the
	// SAME label, so the old sweep stopped and removed the containers it had
	// just deployed while reporting success (TCL-02). The snapshot also
	// preserves the F06 property — a worker removed from the manifest is
	// still captured here, because it was running before this deploy.
	var predecessors []docker.Container
	predecessorsListed := false
	if current != nil && current.CurrentHash != "" {
		if inv, invErr := d.docker.ListContainers(ctx, cfg.App); invErr == nil {
			predecessorsListed = true
			predecessors = selectPredecessors(inv, current, sameVersion)
		} else {
			fmt.Fprintf(d.out, "Warning: could not list containers for the predecessor snapshot (%v); falling back to name matching\n", invErr)
		}
	}

	// 6c. Persist the predecessor snapshot into this attempt's immutable
	// artifact namespace (C01-10), still BEFORE any new container starts
	// and before the recreate-strategy displacement stops the fixed-port
	// workload: a crash after candidates start loses the in-memory
	// snapshot, and with it the exact knowledge of what this attempt must
	// compensate or retire — re-derivation from a post-crash inventory
	// selects by the NEW authoritative release (TCL-02's hazard) and the
	// name fallback cannot see removed workers (T63). The attempt dir is
	// write-once per F08, so the snapshot is immutable once written. A
	// persistence failure degrades crash-safety only (warned); the
	// in-memory snapshot keeps this deploy correct.
	if predecessorsListed {
		if err := d.persistPredecessorSnapshot(ctx, assetAttempt, current.CurrentHash, sameVersion, predecessors); err != nil {
			fmt.Fprintf(d.out, "Warning: could not persist the predecessor snapshot for crash recovery: %v\n", err)
		}
	}

	// Host ingress and publish-apps recreate rather than blue/green: the new
	// container reuses the old one's fixed host port(s), so stop the running
	// web container first to free them. Keep the stopped container until
	// state commits so a failed commit can recreate it with its original
	// configuration and fixed port.
	var displacedHostWeb []string
	if recreateWeb {
		// Fence (F16): stopping the fixed-port workload is the deploy's
		// first destructive effect; nothing of ours needs restoring yet,
		// so a lost fence is a plain abort.
		if err := lk.Check(ctx, d.exec); err != nil {
			return err
		}
		names, _ := d.exec.Run(ctx, fmt.Sprintf(
			"docker ps --filter label=teploy.app=%s --filter label=teploy.process=web --format '{{.Names}}'", cfg.App))
		for _, name := range strings.Fields(names) {
			if err := d.docker.Stop(ctx, name, stopTimeout); err != nil {
				// A later container's stop failed after earlier ones already
				// stopped: bring the displaced ones back before bailing (F05).
				recoveryCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				var lastErr error
				for _, old := range displacedHostWeb {
					if rerr := d.docker.Restart(recoveryCtx, old, nil); rerr != nil {
						lastErr = rerr
					}
				}
				cancel()
				if lastErr != nil {
					return fmt.Errorf("stopping current fixed-port container %s: %w — restoring the displaced workload also failed: %v; %s needs manual attention", name, err, lastErr, cfg.App)
				}
				return fmt.Errorf("stopping current fixed-port container %s: %w — the displaced workload was restored", name, err)
			}
			displacedHostWeb = append(displacedHostWeb, name)
		}
	}

	// Track started containers for cleanup on failure. The failure handler
	// is installed BEFORE the first container starts so an error at any
	// later step (second replica, worker, hook, health, route) cleans up
	// everything this deploy started AND restores the fixed-port workload
	// displaced by host ingress — previously an early docker-run failure
	// returned without either, orphaning the first replica and leaving a
	// host-ingress app down (audit F05).
	var started []string
	// candidateIDs collects the container IDs docker run returned for the
	// web candidates — the exact identities the readiness receipt records
	// (C01-4) and the strongest attribution evidence a recovery owner has.
	candidateIDs := make([]string, replicas)
	webContainerNames := make([]string, replicas)
	restoreDisplacedAndStarted := func(reason error) error {
		// Cleanup runs detached from the (possibly cancelled) deploy context,
		// bounded so a hung cleanup cannot outlive the process.
		recoveryCtx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		// Every intended compensation is reported (A13): a stop/remove
		// failure leaves a stray container, a failed restart leaves the app
		// (or one of its replicas) down — "at least one thing worked" must
		// never read as "recovered".
		var cleanupFailures []string
		for _, n := range started {
			if err := d.docker.Stop(recoveryCtx, n, 5); err != nil {
				cleanupFailures = append(cleanupFailures, fmt.Sprintf("stop %s: %v", n, err))
				continue
			}
			if err := d.docker.Remove(recoveryCtx, n); err != nil {
				cleanupFailures = append(cleanupFailures, fmt.Sprintf("remove %s: %v", n, err))
			}
		}
		// The displaced list is the in-memory one when this process did
		// the displacing; a process recovering a crashed attempt arrives
		// with it empty, and the durable predecessor snapshot (C01-10)
		// restores exactly that knowledge — snapshot web containers that
		// are no longer running were displaced by the attempt.
		displaced := displacedHostWeb
		if len(displaced) == 0 {
			displaced = d.displacedFromSnapshot(recoveryCtx, assetAttempt)
		}
		restored := len(displaced) == 0
		for _, old := range displaced {
			if err := d.docker.Restart(recoveryCtx, old, nil); err != nil {
				cleanupFailures = append(cleanupFailures, fmt.Sprintf("restore %s: %v", old, err))
				fmt.Fprintf(d.out, "  WARNING: could not restore displaced container %s: %v\n", old, err)
			} else {
				// A successful restart is proof of restoration (T07): the
				// flag used to stay false even when EVERY predecessor came
				// back, so an accurate "all restored" recovery reported
				// "no container is serving".
				restored = true
				fmt.Fprintf(d.out, "  Restored %s\n", old)
			}
		}
		if len(cleanupFailures) > 0 {
			fmt.Fprintf(d.out, "  WARNING: cleanup incomplete after failure — %s\n", strings.Join(cleanupFailures, "; "))
		}
		d.logDeploy(recoveryCtx, cfg, false, "", start)
		if len(displaced) > 0 && !restored {
			return fmt.Errorf("%w — recovery also failed: no predecessor could be restarted; %s needs manual attention (%s)", reason, cfg.App, strings.Join(cleanupFailures, "; "))
		}
		if len(cleanupFailures) > 0 {
			return fmt.Errorf("%w — recovery incomplete, %s needs attention: %s", reason, cfg.App, strings.Join(cleanupFailures, "; "))
		}
		return reason
	}

	// 6. Start web container(s).
	// Fence (F16, C01-2 composition): container creation runs as a guarded
	// effect — the holdership check and the docker run are ONE remote
	// command, so no transport window exists in which a takeover lets this
	// (possibly broken) holder's starts land inside a new owner's window.
	// A lost fence here must still restore whatever this deploy displaced
	// (recovery is never fenced — see DeployFenced's doc).
	guardPrefix := lk.GuardPrefix()
	for i := 0; i < replicas; i++ {
		name := docker.ReplicaContainerName(cfg.App, "web", cfg.Version, i+1, replicas)
		webContainerNames[i] = name
		fmt.Fprintf(d.out, "Starting container %s (port %d)...\n", name, ports[i])
		containerID, err := d.docker.RunGuarded(ctx, docker.RunConfig{
			App:           cfg.App,
			Process:       "web",
			Version:       cfg.Version,
			Image:         runImage,
			Port:          ports[i],
			BindHost:      webBindHost,
			ContainerPort: containerPort,
			Publish:       cfg.Publish,
			EnvFiles:      cfg.EnvFiles,
			Env:           cfg.Env,
			Volumes:       cfg.Volumes,
			Cmd:           processes["web"],
			Memory:        cfg.Memory,
			CPU:           cfg.CPU,
			Name:          name,
			NoHealthcheck: cfg.NoHealthcheck["web"],
		}, guardPrefix)
		if err != nil {
			if state.FenceLost(err) {
				// The guard refused the run: the lock no longer names this
				// operation, and docker never executed. Restore displaced
				// work (unfenced), exactly like the old pre-loop Check
				// failure — no partial-run reconciliation needed because
				// nothing ran.
				return restoreDisplacedAndStarted(fmt.Errorf("%w: refusing to start %s's containers", state.ErrFenceLost, cfg.App))
			}
			// Docker can CREATE a container and still fail the run (port
			// binding, for one) — that corpse is not in `started`, so it
			// would outlive this deploy and collide with the next one's
			// candidate name. Reconcile it: remove the name's container
			// only when it is NOT running (a running container under the
			// candidate name is not provably ours — never kill it, A14).
			d.reconcilePartialRun(name)
			return restoreDisplacedAndStarted(fmt.Errorf("starting container %s: %w", name, err))
		}
		started = append(started, name)
		candidateIDs[i] = containerID
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
		d.printDiagnosis(ctx, webContainerName, containerPort, reason, logs)
		return restoreDisplacedAndStarted(reason)
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

	// 9. Health check all web replicas. The gate is surfaced BEFORE it
	// runs: the operator sees which probe mode and what total deadline is
	// in effect (C03) — not just the verdict after the wait.
	fmt.Fprintln(d.out, "Running health check...")
	healthCfg := cfg.Health.withDefaults()
	if len(ports) > 0 {
		fmt.Fprintf(d.out, "  Readiness: %s\n", readinessSummary(healthCfg, ports[0]))
	}
	for i, p := range ports {
		if err := d.healthCheck(ctx, p, healthCfg, webBindHost); err != nil {
			fmt.Fprintf(d.out, "  Health check failed for replica %d (port %d): %v\n", i+1, p, err)
			return fail(fmt.Errorf("health check failed for replica %d: %w", i+1, err))
		}
	}
	if replicas > 1 {
		fmt.Fprintf(d.out, "  All %d replicas healthy\n", replicas)
	} else {
		fmt.Fprintln(d.out, "  Health check passed")
	}

	// 9b. Persist the readiness receipt (C01-4) — exactly now: the gate
	// has passed, the traffic switch has NOT begun. The receipt (exact
	// candidate container IDs + what was probed + when) is what lets a
	// recovery owner distinguish "crashed during readiness" (no receipt:
	// INSPECT) from "readiness held, crash before/while switching
	// traffic" (the COMPENSATE class) — see attemptReadinessState /
	// candidateAttribution for the Decide-side derivation. A write
	// failure warns: the receipt is recovery evidence, not a deploy
	// precondition.
	{
		probeHost := healthProbeHost(webBindHost)
		cands := make([]receiptCandidate, len(webContainerNames))
		probes := make([]readinessProbe, len(ports))
		for i, name := range webContainerNames {
			cands[i] = receiptCandidate{Name: name, ID: candidateIDs[i]}
		}
		for i, p := range ports {
			probes[i] = readinessProbe{Container: webContainerNames[i], Host: probeHost, Port: p, Path: healthCfg.Path, Mode: healthCfg.Mode}
		}
		if err := d.persistReadinessReceipt(ctx, assetAttempt, cfg.Version, cands, probes); err != nil {
			fmt.Fprintf(d.out, "Warning: could not persist the readiness receipt for crash recovery: %v\n", err)
		}
	}

	// 10. Start non-web process containers (workers, etc. — no replicas, one each).
	// Fence (F16, C01-2 composition): same guarded-effect shape as the web
	// candidates above — the run is refused in-shell when the fence is lost.
	for _, process := range sortedProcessNames(processes) {
		if process == "web" {
			continue
		}
		name := docker.ContainerName(cfg.App, process, cfg.Version)
		fmt.Fprintf(d.out, "Starting %s...\n", name)
		_, err := d.docker.RunGuarded(ctx, docker.RunConfig{
			App:           cfg.App,
			Process:       process,
			Version:       cfg.Version,
			Image:         runImage,
			Port:          0, // non-web processes don't get a port
			EnvFiles:      cfg.EnvFiles,
			Env:           cfg.Env,
			Volumes:       cfg.Volumes,
			Cmd:           processes[process],
			Memory:        cfg.Memory,
			CPU:           cfg.CPU,
			NoHealthcheck: cfg.NoHealthcheck[process],
		}, guardPrefix)
		if err != nil {
			if state.FenceLost(err) {
				return fail(fmt.Errorf("%w: refusing to start %s's %s process", state.ErrFenceLost, cfg.App, process))
			}
			d.reconcilePartialRun(name)
			return fail(fmt.Errorf("starting %s: %w", name, err))
		}
		started = append(started, name)
		// A detached `docker run` proves nothing about the worker's
		// viability — a bad command or an instantly-crashing process used
		// to be recorded as a successful deploy while no jobs were consumed
		// (A23). Require the worker to still be running one second later,
		// and treat an already-exited/restarting/unhealthy state as a
		// failed deploy (cleaned up with everything else via fail()).
		if err := d.workerRemainsRunning(ctx, name); err != nil {
			return fail(err)
		}
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
		// Fence (F16, C01-2 composition): the route switch commits traffic
		// to this deploy's containers. The Caddyfile COMMIT (the rename
		// that makes the new block authoritative) runs under this deploy's
		// fence guard in the same shell — a late write from a broken holder
		// cannot hijack a newer operation's route. The pre-switch Check is
		// superseded by the composed commit.
		cad := d.caddy.WithCommitGuard(guardPrefix)
		tls := caddy.TLS{Cert: cfg.TLSCert, Key: cfg.TLSKey, Internal: cfg.TLSInternal}
		if replicas > 1 {
			upstreams := make([]caddy.Upstream, replicas)
			for i := range replicas {
				upstreams[i] = caddy.Upstream{Dial: fmt.Sprintf("%s:%d", webContainerNames[i], containerPort)}
			}
			// Caddy's active upstream checks probe the SAME path the deploy
			// readiness gate used (F47) — the block used to hardcode /up.
			if err := cad.SetLoadBalancerHealth(ctx, cfg.App, cfg.Domain, upstreams, healthCfg.Path, tls, cfg.CaddyExtra, cfg.Cache, cfg.Firewall, cfg.Access); err != nil {
				if state.FenceLost(err) {
					return fail(fmt.Errorf("%w: refusing to switch %s's route", state.ErrFenceLost, cfg.App))
				}
				return fail(fmt.Errorf("updating load balancer route: %w", err))
			}
			fmt.Fprintf(d.out, "  Traffic load-balanced across %d replicas\n", replicas)
		} else {
			if err := cad.SetRoute(ctx, cfg.App, cfg.Domain, webContainerName, containerPort, tls, cfg.CaddyExtra, cfg.Cache, cfg.Firewall, cfg.Access); err != nil {
				if state.FenceLost(err) {
					return fail(fmt.Errorf("%w: refusing to switch %s's route", state.ErrFenceLost, cfg.App))
				}
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
	newState := state.NewAppliedState(current, "container", cfg.Ingress, cfg.Domain)
	newState.CurrentPort = port
	newState.CurrentPorts = ports
	newState.CurrentHash = cfg.Version
	newState.ManifestSHA256 = cfg.ManifestSHA256
	newState.AppliedManifest = append(json.RawMessage(nil), cfg.AppliedManifest...)
	newState.SourceRevision = cfg.SourceRevision
	newState.ImageRef = cfg.Image
	newState.ImageDigest = ImageDigestFromRef(cfg.Image)
	if newState.ImageDigest == "" {
		newState.ImageDigest, _ = d.docker.ContainerImageDigest(ctx, webContainerName)
	}
	if current != nil {
		newState.PreviousPort = current.CurrentPort
		newState.PreviousPorts = current.CurrentPorts
		newState.PreviousHash = current.CurrentHash
	}
	// The commit runs under the fence (F16): the atomic rename that makes
	// this deploy authoritative is a guarded effect, so a broken holder
	// commits nothing.
	if err := state.WriteFenced(ctx, d.exec, cfg.App, newState, lk); err != nil {
		return d.abortStateCommit(ctx, cfg, current, started, displacedHostWeb, assetAttempt, start, err)
	}

	// 13b. Record the release metadata (F14). The containers are live and
	// the route/state are committed — a record failure is a degraded
	// rollback window, not a failed deploy, and it converges on the next
	// deploy via the repair-debt marker (C01-6). Never abort into
	// abortStateCommit from here. The written record is returned for the
	// closing plan/receipt verification (17).
	rec := d.recordRelease(ctx, cfg, assetAttempt, newState, ports, webBindHost, webContainerName)

	// 13c. Prune superseded attempts (F08): attempt directories (build
	// contexts, env files, TLS certs) are dead weight once their release
	// is outside the rollback window — env is baked into containers at
	// create and recreate uses the inspect-derived resolved env, never
	// the file. The protection window is every release that still has
	// CONTAINERS on this server, plus current, previous, and pinned
	// (audit A02): keep_versions retention can hold releases far beyond
	// current+previous, and their records and Caddy routes reference
	// attempt-scoped TLS and env files — pruning those while the release
	// is retained silently invalidates its rollback target. A version
	// that falls out of the keep window loses its containers at step 15b
	// of THIS deploy, so its attempt dirs are first prunable on the next.
	{
		var prevHash string
		if current != nil {
			prevHash = current.CurrentHash
		}
		pins, pinsErr := state.ReadPins(ctx, d.exec, cfg.App)
		if pinsErr != nil {
			// Fail closed exactly like version pruning (15b): an
			// unreadable pin file means retention obligations cannot be
			// established, and pruning anyway can delete a pinned
			// release's artifacts precisely when the operator cannot see
			// the pin (audit A03).
			fmt.Fprintf(d.out, "Warning: attempt-artifact prune skipped — pin state could not be read: %v\n", pinsErr)
		} else {
			protected := append([]string{cfg.Version, prevHash}, pins...)
			if inv, invErr := d.docker.ListContainers(ctx, cfg.App); invErr != nil {
				fmt.Fprintf(d.out, "Warning: attempt-artifact prune skipped — cannot inventory retained versions: %v\n", invErr)
			} else {
				for _, ct := range inv {
					if v := ct.Labels["teploy.version"]; v != "" {
						protected = append(protected, v)
					}
				}
				if err := releasemeta.PruneAttempts(ctx, d.exec, cfg.App, protected...); err != nil {
					fmt.Fprintf(d.out, "Warning: could not prune superseded attempt artifacts: %v\n", err)
				}
			}
		}
	}

	// 14. Stop the predecessor workload snapshotted in step 6b (all
	// processes + all replicas). For same-version redeploys the old
	// containers were renamed to _replaced; remove them after stopping so
	// they don't block the next same-version deploy.
	//
	// Only the snapshotted predecessors are touched. Selecting by the
	// teploy.version label from a post-deploy inventory — the previous
	// implementation — also matched the just-deployed replacement during a
	// same-version redeploy and removed the live generation (TCL-02). When
	// the snapshot could not be listed, the cleanup RETRIES the inventory
	// first (T63): the name-derived fallback derives worker names from the
	// NEW config's processes, so a worker the operator REMOVED this deploy
	// is invisible to it and would keep consuming jobs while the deploy
	// reported success. Only a still-failing inventory degrades to names —
	// now with every stop/remove failure reported (T63's honest-retirement
	// half).
	//
	// Every incomplete retirement is collected (C01-5): the deploy stays
	// successful (traffic is switched, the app serves), but the terminal
	// log entry records the degraded outcome instead of clean success —
	// a fleet rollback keyed on the log must not skip a host still
	// running part of the superseded generation.
	var retireIncomplete []string
	if predecessorsListed {
		retireIncomplete = d.stopPredecessorSnapshot(ctx, predecessors, sameVersion, stopTimeout, lk)
	} else if current != nil && current.CurrentHash != "" {
		// Fence (F16): post-commit cleanup never interleaves with a new
		// holder.
		fenceOK := true
		if lk != nil {
			if err := lk.Check(ctx, d.exec); err != nil {
				fmt.Fprintf(d.out, "Warning: predecessor cleanup skipped — %v\n", err)
				retireIncomplete = append(retireIncomplete, fmt.Sprintf("cleanup skipped: %v", err))
				fenceOK = false
			}
		}
		if fenceOK {
			if inv, invErr := d.docker.ListContainers(ctx, cfg.App); invErr == nil {
				retireIncomplete = append(retireIncomplete, d.stopPredecessorSnapshot(ctx, selectPredecessors(inv, current, sameVersion), sameVersion, stopTimeout, lk)...)
			} else {
				fmt.Fprintf(d.out, "Warning: container inventory still unreadable (%v) — cleaning up by derived names; a removed worker process may escape retirement\n", invErr)
				if err := stopOldWorkloadsByName(ctx, d.docker, d.out, cfg, current, processes, stopTimeout); err != nil {
					fmt.Fprintf(d.out, "Warning: name-based cleanup incomplete: %v\n", err)
					retireIncomplete = append(retireIncomplete, err.Error())
				}
			}
		}
	}

	// 15. Clean up old bridged assets (asset_keep_days). The LIVE tree the
	// container mounts is the attempt's private tree (A15), so the expiry
	// runs THERE — the old cleanup targeted only the legacy shared
	// /deployments/<app>/assets path, which made asset_keep_days a no-op
	// for every deploy since F08 (audit T11). The legacy tree still serves
	// releases deployed before attempt scoping, so it keeps its sweep too.
	if cfg.AssetPath != "" {
		keepDays := cfg.AssetKeepDays
		if keepDays <= 0 {
			keepDays = 7
		}
		for _, assetRoot := range []string{
			assetAttempt.Dir() + "/assets",
			fmt.Sprintf("/deployments/%s/assets", cfg.App),
		} {
			cleanCmd := fmt.Sprintf(
				"find %s -type f -mtime +%d -delete 2>/dev/null || true",
				ssh.ShellQuote(assetRoot), keepDays,
			)
			d.exec.Run(ctx, cleanCmd)
		}
	}

	// 15b. Prune superseded app versions (containers + images) if the
	// operator opted in via keep_versions. Always protects the current
	// version and the immediately-previous version so a rollback target
	// is preserved regardless of timestamp ordering.
	if cfg.KeepVersions > 0 {
		// Fence (F16): pruning removes other versions' containers; a
		// stale holder must not delete under a new owner's feet.
		fenceOK := true
		if lk != nil {
			if err := lk.Check(ctx, d.exec); err != nil {
				fmt.Fprintf(d.out, "Warning: version prune skipped — %v\n", err)
				fenceOK = false
			}
		}
		if fenceOK {
			var prevHash string
			if current != nil {
				prevHash = current.CurrentHash
			}
			// Pinned versions are protected from pruning regardless of the keep
			// window (teploy pin). Read them off the server so terminal, dash,
			// and autodeploy all honor the same set. A read failure SKIPS
			// pruning entirely — treating an unreadable pin file as "no pins"
			// could delete versions the operator deliberately retained (F78).
			protected := []string{cfg.Version, prevHash}
			pins, pinsErr := state.ReadPins(ctx, d.exec, cfg.App)
			if pinsErr != nil {
				fmt.Fprintf(d.out, "Warning: version prune skipped — pin state could not be read: %v\n", pinsErr)
			} else {
				protected = append(protected, pins...)
				pruned, err := d.docker.PruneVersions(ctx, cfg.App, cfg.KeepVersions, protected...)
				switch {
				case err != nil:
					// Partial cleanup (A25): report what was left behind —
					// the old path printed nothing when any removal failed,
					// or reported failed removals as pruned.
					fmt.Fprintf(d.out, "Warning: version prune incomplete: %v\n", err)
					if len(pruned) > 0 {
						fmt.Fprintf(d.out, "Pruned %d superseded version(s): %s\n", len(pruned), strings.Join(pruned, ", "))
					}
				case len(pruned) > 0:
					fmt.Fprintf(d.out, "Pruned %d superseded version(s): %s\n", len(pruned), strings.Join(pruned, ", "))
				}
			}
		}
	}

	// 16. Log the real outcome (C01-5): clean success only when
	// retirement completed; a partial retirement is a degraded success.
	degradedReason := strings.Join(retireIncomplete, "; ")
	d.logDeploy(ctx, cfg, true, degradedReason, start)
	if degradedReason != "" {
		fmt.Fprintf(d.out, "Warning: deployed, but predecessor retirement is incomplete — %s\n", degradedReason)
	}

	duration := time.Since(start)

	// 17. Closing verification (C04): after the live commit, assert the
	// deployed record's image digest equals the digest the plan showed —
	// and report the receipt's identity triple explicitly. A mismatch is
	// a loud warning plus repair debt (the C01-6 marker the next deploy
	// reconciles); live traffic is never failed over a bookkeeping gap.
	d.verifyPlanReceiptEquality(ctx, cfg, rec, assetAttempt, planDigest)

	fmt.Fprintf(d.out, "\nDeployed %s version %s in %s\n", cfg.App, cfg.Version, duration.Round(time.Millisecond))
	printDeployReceipt(d.out, cfg, rec)
	return nil
}

// selectPredecessors picks the containers this deploy must retire from an
// app inventory: the authoritative current version's workload (accessories
// excluded — they have their own lifecycle), keeping stopped historical
// containers ONLY for a same-version redeploy (they were just renamed to
// _replaced and must be removed, TCL-02/A08's contract).
func selectPredecessors(inv []docker.Container, current *state.AppState, sameVersion bool) []docker.Container {
	var out []docker.Container
	for _, ct := range inv {
		if ct.Labels["teploy.role"] == "accessory" {
			continue // accessories have their own lifecycle
		}
		if ct.Labels["teploy.version"] != current.CurrentHash {
			continue
		}
		if !sameVersion && ct.State != "running" {
			continue // older stopped versions are kept as rollback targets
		}
		out = append(out, ct)
	}
	return out
}

// stopPredecessorSnapshot retires exactly the snapshotted predecessor set
// and returns what escaped retirement (C01-5: the caller records it as a
// degraded outcome — never clean success, never a failed deploy).
// Fence checks precede each stop: the deploy is already committed, and a
// fence loss mid-cleanup means another operation owns the app — refuse
// further stops (loudly) rather than interleaving.
func (d *Deployer) stopPredecessorSnapshot(ctx context.Context, predecessors []docker.Container, sameVersion bool, stopTimeout int, lk *state.Lock) []string {
	var incomplete []string
	for _, ct := range predecessors {
		if lk != nil {
			if err := lk.Check(ctx, d.exec); err != nil {
				fmt.Fprintf(d.out, "Warning: predecessor cleanup stopped — %v\n", err)
				incomplete = append(incomplete, fmt.Sprintf("cleanup interrupted before %s (fence lost)", ct.Name))
				break
			}
		}
		fmt.Fprintf(d.out, "Stopping old container %s...\n", ct.Name)
		if err := d.docker.Stop(ctx, ct.Name, stopTimeout); err != nil {
			// Traffic is already committed to the new generation; a
			// failed predecessor stop is degraded cleanup, not a failed
			// deploy — but it must be reported, never silent (TCL-19):
			// a leftover old worker keeps consuming jobs.
			fmt.Fprintf(d.out, "Warning: could not stop old container %s: %v\n", ct.Name, err)
			incomplete = append(incomplete, fmt.Sprintf("stop %s: %v", ct.Name, err))
			continue
		}
		if sameVersion {
			if err := d.docker.Remove(ctx, ct.Name); err != nil {
				fmt.Fprintf(d.out, "Warning: could not remove old container %s: %v\n", ct.Name, err)
				incomplete = append(incomplete, fmt.Sprintf("remove %s: %v", ct.Name, err))
			}
		}
	}
	return incomplete
}

// stopOldWorkloadsByName is the name-derived fallback for old-workload
// cleanup when the container inventory cannot be listed at all (see step
// 14). Every stop/remove failure is reported (T63): the old shape ignored
// them entirely, so an incomplete retirement read as a clean one.
func stopOldWorkloadsByName(ctx context.Context, dk *docker.Client, out io.Writer, cfg Config, current *state.AppState, processes map[string]string, stopTimeout int) error {
	if current == nil || current.CurrentHash == "" {
		return nil
	}
	sameVersion := current.CurrentHash == cfg.Version
	oldReplicas := len(current.CurrentPorts)
	if oldReplicas == 0 {
		oldReplicas = 1
	}
	var failures []error
	stop := func(name string) {
		if sameVersion {
			name += "_replaced"
		}
		fmt.Fprintf(out, "Stopping old container %s...\n", name)
		if err := dk.Stop(ctx, name, stopTimeout); err != nil {
			failures = append(failures, fmt.Errorf("stop %s: %w", name, err))
			return
		}
		if sameVersion {
			if err := dk.Remove(ctx, name); err != nil {
				failures = append(failures, fmt.Errorf("remove %s: %w", name, err))
			}
		}
	}
	for ri := 1; ri <= oldReplicas; ri++ {
		stop(docker.ReplicaContainerName(cfg.App, "web", current.CurrentHash, ri, oldReplicas))
	}
	if oldReplicas <= 1 {
		stop(docker.ContainerName(cfg.App, "web", current.CurrentHash))
	}
	for _, process := range sortedProcessNames(processes) {
		if process == "web" {
			continue
		}
		stop(docker.ContainerName(cfg.App, process, current.CurrentHash))
	}
	return errors.Join(failures...)
}

func (d *Deployer) abortStateCommit(ctx context.Context, cfg Config, current *state.AppState, started, displacedHostWeb []string, att releasemeta.Attempt, start time.Time, commitErr error) error {
	// Compensation runs on a DETACHED bounded context (A11): if the commit
	// failed because the deploy context was cancelled, reusing that context
	// would skip the very stops/restarts/route restores that undo the
	// deploy — leaving the app dark while the error text claims recovery.
	recoveryCtx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	d.logDeploy(recoveryCtx, cfg, false, "", start)

	// The displaced fixed-port workload: the in-memory list when this
	// process did the displacing; a process recovering a crashed attempt
	// reads it from the durable predecessor snapshot (C01-10) — exactly
	// the recorded container identities, never a re-derivation.
	displaced := displacedHostWeb
	if len(displaced) == 0 {
		displaced = d.displacedFromSnapshot(recoveryCtx, att)
	}

	if cfg.ingressHost() || len(cfg.Publish) > 0 {
		for _, name := range started {
			d.docker.Stop(recoveryCtx, name, 5)
		}
		for _, old := range displaced {
			if err := d.docker.Restart(recoveryCtx, old, nil); err != nil {
				for _, name := range started {
					d.docker.Start(recoveryCtx, name)
				}
				return fmt.Errorf("committing authoritative applied state after replacing the fixed-port host workload: %w; restoring the original workload failed: %v; Teploy attempted to restart the new workload to avoid an outage", commitErr, err)
			}
		}
		// A caddy-ingress app with publish entries entered this branch too
		// (its fixed ports forced the recreate strategy) and its route WAS
		// switched in step 11 — the commit failure used to return here
		// without restoring the route, leaving Caddy pointed at the removed
		// candidate names (A10). Restore it before removing the candidates;
		// if that fails, keep the candidates running rather than routing to
		// nothing.
		if cfg.usesCaddy() {
			if err := d.restorePreviousRoute(recoveryCtx, cfg, current); err != nil {
				for _, name := range started {
					d.docker.Start(recoveryCtx, name)
				}
				return fmt.Errorf("committing authoritative applied state after replacing the fixed-port host workload: %w; the original workload restarted but its route could not be restored: %v; the uncommitted workload was restarted to avoid an outage", commitErr, err)
			}
		}
		for _, name := range started {
			d.docker.Remove(recoveryCtx, name)
		}
		if len(displaced) == 0 {
			return fmt.Errorf("committing authoritative applied state after starting the first host-ingress workload: %w; the uncommitted workload was stopped and removed", commitErr)
		}
		return fmt.Errorf("committing authoritative applied state after replacing the fixed-port host workload: %w; the original workload was restored and the uncommitted workload was removed", commitErr)
	}

	if cfg.usesCaddy() {
		if err := d.restorePreviousRoute(recoveryCtx, cfg, current); err != nil {
			return fmt.Errorf("committing authoritative applied state after route switch: %w; restoring the previous route failed: %v; old and new workloads were left running to avoid routing to a stopped container", commitErr, err)
		}
	}

	for _, name := range started {
		d.docker.Stop(recoveryCtx, name, 5)
		d.docker.Remove(recoveryCtx, name)
	}
	if current == nil {
		return fmt.Errorf("committing authoritative applied state after route switch: %w; the new route was removed and the uncommitted workload was stopped", commitErr)
	}
	return fmt.Errorf("committing authoritative applied state after route switch: %w; the previous route was restored, the old workload was left running, and the uncommitted workload was stopped", commitErr)
}

// restorePreviousRoute compensates a failed deploy's traffic switch by
// putting the PREVIOUS release's route back (C01-7/A12/T05). The F14 record
// of the predecessor release is the receipt of what teploy switched away
// FROM, and it is AUTHORITATIVE: domain, replica upstream names, the
// recorded primary container port, TLS/extra/cache/firewall/access, and the
// LB health path all come from the record — never from the current config
// or a live inspect, which can disagree with the receipt precisely when
// config drifted (and compensating to a drifted block is compensating to
// the wrong route). Reconstruct-from-inspection remains only as the
// documented fallback for legacy installs without a record — and it says so.
func (d *Deployer) restorePreviousRoute(ctx context.Context, cfg Config, current *state.AppState) error {
	if current == nil || current.CurrentHash == "" {
		return d.caddy.RemoveRoute(ctx, cfg.App)
	}
	if current.IngressMode != "" && current.IngressMode != "caddy" {
		return d.caddy.RemoveRoute(ctx, cfg.App)
	}

	rec, recErr := releasemeta.Read(ctx, d.exec, cfg.App, current.CurrentHash)
	switch {
	case recErr != nil:
		fmt.Fprintf(d.out, "Warning: the release record for %s@%s could not be read (%v) — restoring the previous route from live inspection instead of the recorded receipt\n", cfg.App, current.CurrentHash, recErr)
	case rec == nil:
		fmt.Fprintf(d.out, "Warning: no release record for %s@%s (pre-F14 install) — restoring the previous route from live inspection instead of the recorded receipt\n", cfg.App, current.CurrentHash)
	default:
		if port, ok := releasemeta.PrimaryContainerPort(rec); ok {
			return d.restoreRouteFromReceipt(ctx, cfg, current, rec, port)
		}
		// A record without a designated primary port (a backfilled record
		// whose bindings identified none) cannot render the receipt's
		// upstream port; that piece falls back to inspection, loudly.
		fmt.Fprintf(d.out, "Warning: the release record for %s@%s names no primary container port — restoring the previous route from live inspection instead of the recorded receipt\n", cfg.App, current.CurrentHash)
	}
	return d.restoreRouteFromInspection(ctx, cfg, current)
}

// restoreRouteFromReceipt renders the predecessor route from the recorded
// receipt. The upstream NAMES are deterministic per release (the same
// derivation every deploy uses), so the record's replica count plus the
// recorded primary container port reproduce the exact upstreams without a
// single live inspect. Edge-config overlays come from the record when it
// carries them; a backfilled record cannot (nothing recoverable from
// containers), and the CLI-passed config stays the fallback for it exactly
// like rollback's applyRecordToRollback.
func (d *Deployer) restoreRouteFromReceipt(ctx context.Context, cfg Config, current *state.AppState, rec *releasemeta.Record, containerPort int) error {
	replicas := rec.Replicas
	if replicas <= 0 {
		replicas = 1
	}
	names := make([]string, replicas)
	upstreams := make([]caddy.Upstream, replicas)
	for i := range replicas {
		name := docker.ReplicaContainerName(cfg.App, "web", current.CurrentHash, i+1, replicas)
		if current.CurrentHash == cfg.Version {
			name += "_replaced"
		}
		names[i] = name
		upstreams[i] = caddy.Upstream{Dial: fmt.Sprintf("%s:%d", name, containerPort)}
	}

	domain := rec.Domain
	if domain == "" {
		domain = current.Domain
	}
	if domain == "" {
		domain = cfg.Domain
	}

	tls := caddy.TLS{Cert: cfg.TLSCert, Key: cfg.TLSKey, Internal: cfg.TLSInternal}
	caddyExtra := cfg.CaddyExtra
	cache := cfg.Cache
	fw := cfg.Firewall
	access := cfg.Access
	if rec.Caddy != nil {
		tls = caddy.TLS{Cert: rec.Caddy.TLSCert, Key: rec.Caddy.TLSKey, Internal: rec.Caddy.TLSInternal}
		caddyExtra = rec.Caddy.CaddyExtra
		cache = rec.Caddy.Cache
		if rec.Caddy.Firewall != nil {
			fw = *rec.Caddy.Firewall
		}
		if rec.Caddy.Access != nil {
			access = *rec.Caddy.Access
		}
	}
	healthPath := cfg.Health.withDefaults().Path
	if rec.Health != nil && rec.Health.Path != "" {
		healthPath = rec.Health.Path
	}

	if replicas > 1 {
		return d.caddy.SetLoadBalancerHealth(ctx, cfg.App, domain, upstreams, healthPath, tls, caddyExtra, cache, fw, access)
	}
	return d.caddy.SetRoute(ctx, cfg.App, domain, names[0], containerPort, tls, caddyExtra, cache, fw, access)
}

// restoreRouteFromInspection is the legacy fallback (pre-F14 installs, or a
// record that cannot name its route): reconstruct the previous block from
// the current config plus a live inspect of the predecessor containers.
func (d *Deployer) restoreRouteFromInspection(ctx context.Context, cfg Config, current *state.AppState) error {
	replicas := len(current.CurrentPorts)
	if replicas == 0 {
		replicas = 1
	}
	names := make([]string, replicas)
	upstreams := make([]caddy.Upstream, replicas)
	primaryPort := 0
	for i := range replicas {
		name := docker.ReplicaContainerName(cfg.App, "web", current.CurrentHash, i+1, replicas)
		if current.CurrentHash == cfg.Version {
			name += "_replaced"
		}
		port, err := d.docker.InternalPort(ctx, name)
		if err != nil {
			return err
		}
		names[i] = name
		upstreams[i] = caddy.Upstream{Dial: fmt.Sprintf("%s:%d", name, port)}
		if i == 0 {
			primaryPort = port
		}
	}

	domain := current.Domain
	if domain == "" {
		domain = cfg.Domain
	}
	tls := caddy.TLS{Cert: cfg.TLSCert, Key: cfg.TLSKey, Internal: cfg.TLSInternal}
	if replicas > 1 {
		return d.caddy.SetLoadBalancerHealth(ctx, cfg.App, domain, upstreams, cfg.Health.Path, tls, cfg.CaddyExtra, cfg.Cache, cfg.Firewall, cfg.Access)
	}
	return d.caddy.SetRoute(ctx, cfg.App, domain, names[0], primaryPort, tls, cfg.CaddyExtra, cfg.Cache, cfg.Firewall, cfg.Access)
}

// logDeploy appends the terminal receipt for a deploy attempt. success
// records whether traffic switched and committed; degradedReason (empty
// for clean outcomes and failures) itemizes post-commit retirement that
// partially failed — a degraded success is still serving the new
// generation, and the log must let Success-filtering consumers tell it
// apart from a clean one (C01-5).
func (d *Deployer) logDeploy(ctx context.Context, cfg Config, success bool, degradedReason string, start time.Time) {
	state.AppendLog(ctx, d.exec, state.LogEntry{
		Timestamp:      time.Now().UTC(),
		App:            cfg.App,
		Type:           "deploy",
		Hash:           cfg.Version,
		Image:          cfg.Image,
		Success:        success,
		Degraded:       success && degradedReason != "",
		DegradedReason: degradedReason,
		DurationMs:     time.Since(start).Milliseconds(),
	})
}

// recordRelease persists the F14 record for the release this deploy just
// committed. Ports are the resolved live allocation (the primary's host
// binding plus every publish entry); env records the references (server-side
// env-file paths + the plaintext env map), never resolved secrets. The
// primary web container's full RecreateSpec is embedded from docker's own
// view of it, and the plan-time provenance + effective-config digest ride
// along (C04) so the receipt names what the plan promised. A write failure
// is deliberate degradation (the deploy stays live) made durable and
// convergent: the repair-debt marker it records (C01-6) drives the next
// deploy's rebuild and `status`'s reporting. The in-memory record is
// returned even on a write failure — the closing verification compares
// against what was attempted.
func (d *Deployer) recordRelease(ctx context.Context, cfg Config, att releasemeta.Attempt, applied *state.AppState, ports []int, webBindHost, webContainerName string) *releasemeta.Record {
	containerPort := cfg.ContainerPort
	if containerPort == 0 {
		containerPort = 80
	}
	healthCfg := cfg.Health.withDefaults()

	rec := &releasemeta.Record{
		App:            cfg.App,
		Hash:           cfg.Version,
		Generation:     applied.Generation,
		DeploymentType: "container",
		IngressMode:    cfg.Ingress,
		Domain:         cfg.Domain,
		ImageRef:       cfg.Image,
		ImageDigest:    applied.ImageDigest,
		ManifestSHA256: cfg.ManifestSHA256,
		Replicas:       len(ports),
		Processes:      maps.Clone(cfg.Processes),
		Cmd:            cfg.Cmd,
		Env:            maps.Clone(cfg.Env),
		EnvFiles:       append([]string(nil), cfg.EnvFiles...),
		Volumes:        maps.Clone(cfg.Volumes),
		Publish:        append([]string(nil), cfg.Publish...),
		Memory:         cfg.Memory,
		CPU:            cfg.CPU,
		StopTimeout:    cfg.StopTimeout,
		Bind:           cfg.Bind,
		Health: &releasemeta.Health{
			Mode:            healthCfg.Mode,
			Path:            healthCfg.Path,
			TimeoutSeconds:  int(healthCfg.Timeout.Seconds()),
			IntervalSeconds: int(healthCfg.Interval.Seconds()),
		},
	}
	if rec.IngressMode == "" {
		rec.IngressMode = "caddy"
	}
	if len(ports) > 0 {
		rec.Ports = append(rec.Ports, releasemeta.Port{
			HostPort:      ports[0],
			ContainerPort: containerPort,
			Bind:          webBindHost,
			Primary:       true,
			Fixed:         cfg.ingressHost(),
		})
	}
	for _, pub := range cfg.Publish {
		if p, ok := releasemeta.PortFromPublishSpec(pub); ok {
			rec.Ports = append(rec.Ports, p)
		}
	}
	if cfg.usesCaddy() {
		rec.Caddy = &releasemeta.CaddyRoute{
			TLSCert:     cfg.TLSCert,
			TLSKey:      cfg.TLSKey,
			TLSInternal: cfg.TLSInternal,
			CaddyExtra:  cfg.CaddyExtra,
			Cache:       maps.Clone(cfg.Cache),
		}
		if !cfg.Firewall.Empty() {
			fw := cfg.Firewall
			rec.Caddy.Firewall = &fw
		}
		if !cfg.Access.Empty() {
			acc := cfg.Access
			rec.Caddy.Access = &acc
		}
	}

	if spec, err := d.docker.InspectRecreate(ctx, webContainerName); err == nil {
		rec.Recreate = spec
	} else {
		fmt.Fprintf(d.out, "Warning: could not capture the recreate spec for %s: %v (recreate falls back to live inspect)\n", webContainerName, err)
	}
	// Embed the plan-time provenance (C04): the record is the deployed
	// receipt — it must carry the digest, revision and config the plan
	// showed, verbatim. A copy, so the caller's struct is never aliased
	// into the persisted record.
	if cfg.Provenance != nil {
		provCopy := *cfg.Provenance
		rec.Provenance = &provCopy
	}
	if err := releasemeta.Write(ctx, d.exec, rec); err != nil {
		fmt.Fprintf(d.out, "Warning: could not record release metadata for %s@%s: %v — the deploy stays live; repair debt recorded (the next deploy rebuilds the record)\n", cfg.App, cfg.Version, err)
		d.recordRepairDebt(ctx, att, err)
	}
	return rec
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

// containerPort returns the effective internal container port (0 = the
// docker layer's 80 default).
func containerPort(c Config) int {
	if c.ContainerPort == 0 {
		return 80
	}
	return c.ContainerPort
}

// ImageDigestFromRef extracts the digest of a digest-pinned image
// reference ("repo@sha256:<64hex>"), or "" for every other reference
// shape. Exported for the CLI's plan-time provenance, which must apply
// the SAME like-for-like rule the deployed record applies (a pinned ref
// is identified by its manifest digest, a mutable ref by docker's
// resolved content ID) or plan/receipt equality compares apples to
// oranges.
func ImageDigestFromRef(image string) string {
	if _, digest, ok := strings.Cut(image, "@"); ok && strings.HasPrefix(digest, "sha256:") && len(digest) == len("sha256:")+64 {
		return digest
	}
	return ""
}

// reconcilePartialRun removes the container occupying a candidate name
// after a failed `docker run`, but ONLY when it is not running (A14):
// Docker can create a container and fail the start (port binding, for one),
// leaving a corpse under the candidate name that this deploy never tracked
// and the next deploy's candidate would collide with. A RUNNING container
// under the name is not provably this operation's — it is left alone.
func (d *Deployer) reconcilePartialRun(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := d.exec.Run(ctx, "docker inspect -f '{{.State.Status}}' "+ssh.ShellQuote(name)+" 2>/dev/null || true")
	if err != nil || strings.TrimSpace(out) == "" || strings.TrimSpace(out) == "running" {
		return
	}
	if _, rmErr := d.exec.Run(ctx, "docker rm -f "+ssh.ShellQuote(name)); rmErr != nil {
		fmt.Fprintf(d.out, "Warning: a partial container may remain under %s after the failed start: %v\n", name, rmErr)
	}
}

// workerRemainsRunning verifies a just-started worker process is actually
// viable: still running (not exited/dead/restarting) one second after the
// detached run, and not already flagged unhealthy by the image's
// healthcheck (A23). An inspect that stays unreadable across bounded
// retries FAILS the deploy (audit T21 — this reverses A23's deliberate
// degrade-to-warning, which let a deploy commit while unable to prove any
// worker existed): unknown is not readiness.
func (d *Deployer) workerRemainsRunning(ctx context.Context, name string) error {
	st, ok := d.inspectWorkerStateRetry(ctx, name)
	if !ok {
		return fmt.Errorf("cannot verify worker %s: its state is unreadable after repeated inspection — refusing to commit a deploy whose worker viability is unknown", name)
	}
	if err := workerStateViable(st); err != nil {
		return fmt.Errorf("worker %s is not viable: %w", name, err)
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(time.Second):
	}
	st, ok = d.inspectWorkerStateRetry(ctx, name)
	if !ok {
		return fmt.Errorf("cannot re-verify worker %s after the settling delay: its state is unreadable — refusing to commit a deploy whose worker viability is unknown", name)
	}
	return workerStateViable(st)
}

type workerStateJSON struct {
	Status     string `json:"Status"`
	Running    bool   `json:"Running"`
	Restarting bool   `json:"Restarting"`
	ExitCode   int    `json:"ExitCode"`
	Health     *struct {
		Status string `json:"Status"`
	} `json:"Health"`
}

func (d *Deployer) inspectWorkerState(ctx context.Context, name string) (workerStateJSON, bool) {
	out, err := d.exec.Run(ctx, "docker inspect -f '{{json .State}}' "+ssh.ShellQuote(name))
	if err != nil {
		return workerStateJSON{}, false
	}
	var st workerStateJSON
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &st); err != nil {
		return workerStateJSON{}, false
	}
	return st, true
}

// inspectWorkerStateRetry retries an unreadable inspect a few times with a
// short gap (a transport hiccup right after a detached run is common);
// persistently-unknown states stay unknown so callers fail closed.
func (d *Deployer) inspectWorkerStateRetry(ctx context.Context, name string) (workerStateJSON, bool) {
	for attempt := 0; attempt < 3; attempt++ {
		if st, ok := d.inspectWorkerState(ctx, name); ok {
			return st, true
		}
		select {
		case <-ctx.Done():
			return workerStateJSON{}, false
		case <-time.After(500 * time.Millisecond):
		}
	}
	return workerStateJSON{}, false
}

func workerStateViable(st workerStateJSON) error {
	if st.Restarting || st.Status == "exited" || st.Status == "dead" {
		return fmt.Errorf("container is %s (exit %d)", st.Status, st.ExitCode)
	}
	if st.Status != "running" {
		return fmt.Errorf("container status is %q", st.Status)
	}
	if st.Health != nil && st.Health.Status == "unhealthy" {
		return fmt.Errorf("image healthcheck reports unhealthy")
	}
	return nil
}
