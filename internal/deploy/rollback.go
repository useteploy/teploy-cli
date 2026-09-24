package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/useteploy/teploy/internal/caddy"
	"github.com/useteploy/teploy/internal/docker"
	"github.com/useteploy/teploy/internal/releasemeta"
	"github.com/useteploy/teploy/internal/ssh"
	"github.com/useteploy/teploy/internal/state"
)

// ErrNoPreviousDeploy is returned by Rollback (and StaticDeployer.Rollback)
// when there's no prior version to revert to — most commonly a server's
// very first deploy. Exported so callers doing an automated rollback across
// several servers (see multi-server rollback-all on partial failure,
// internal/cli/deploy.go) can distinguish "nothing to roll back, this was
// the first deploy here" from a genuine rollback failure that needs manual
// attention — the former isn't an operational problem, the latter is.
var ErrNoPreviousDeploy = errors.New("no previous deploy to roll back to")

// RollbackConfig holds parameters for a rollback operation.
type RollbackConfig struct {
	App         string
	Domain      string
	StopTimeout int
	// DrainSeconds is the request-drain window between the route switch
	// back to the target and stopping the superseded generation (C03) —
	// the rollback-side twin of deploy.Config.DrainSeconds. Zero (default)
	// keeps the historical stop-immediately behavior.
	DrainSeconds int
	Health       HealthConfig
	// ToHash rolls back to a specific version instead of just the
	// immediately previous one, mirroring type:static's existing --to
	// support (internal/cli/rollback.go). Empty means the immediately
	// previous version (current.PreviousHash), the historical behavior.
	// Only reachable if containers for that version are still on this
	// server — keep_versions retention determines how far back that is.
	ToHash string
	// TLSCert / TLSKey preserve custom-cert TLS termination across a
	// rollback (container-side paths, already on the server from deploy).
	// Empty = ACME. Without these, rolling back would regenerate the Caddy
	// block without the cert and break TLS the same way a deploy would.
	TLSCert string
	TLSKey  string
	// TLSInternal mirrors Config.TLSInternal — preserves Caddy's local-CA
	// self-signed TLS across a rollback the same way TLSCert/TLSKey do.
	TLSInternal bool
	CaddyExtra  string            // mirrors Config.CaddyExtra — preserves user directives across rollback
	Cache       map[string]string // mirrors Config.Cache — preserves cache rules across rollback
	Firewall    caddy.Firewall    // mirrors Config.Firewall — preserves firewall rules across rollback
	Access      caddy.Access      // mirrors Config.Access — preserves the access gate across rollback
	// Ingress mirrors Config.Ingress — set to "external" to skip Caddy
	// route restoration on rollback. With external ingress, the user's
	// CF Tunnel / nginx / etc. already points at the app-name alias on
	// the teploy docker network; reverting the container set is enough.
	Ingress string
}

// usesCaddy reports whether the rollback should drive Caddy.
func (c RollbackConfig) usesCaddy() bool {
	return c.Ingress == "" || c.Ingress == "caddy"
}

// ingressHost reports whether this app publishes a FIXED host port. That single
// fact changes the whole shape of a rollback: with a fixed port there is only
// one port, every version shares it, and two containers cannot hold it at once —
// so the blue/green order used below (start target, health check, stop current)
// is impossible and the port must never be reallocated.
func (c RollbackConfig) ingressHost() bool { return c.Ingress == "host" }

// Rollback reverts to a previous deploy version — the immediately previous
// one by default, or cfg.ToHash if set (mirroring type:static's --to).
// Starts the target version's containers, health checks, re-routes
// traffic, and stops the current containers. Updates state so target
// becomes current and the version rolled back from becomes previous.
//
// The whole operation runs under the app lock (audit F11), and the lock is
// acquired BEFORE the state read and target resolution (TCL-06): reading
// first, then locking, let a deploy commit in between, leaving rollback
// executing against an obsolete current generation, predecessor set, and
// port-conflict set — serialized execution with a stale decision is still
// a stale decision.
func Rollback(ctx context.Context, exec ssh.Executor, out io.Writer, cfg RollbackConfig) error {
	start := time.Now()
	dk := docker.NewClient(exec)
	cd := caddy.NewClient(exec)
	healthCfg := cfg.Health.withDefaults()

	stopTimeout := cfg.StopTimeout
	if stopTimeout == 0 {
		stopTimeout = 10
	}

	// 1. Serialize against deploys and other rollbacks BEFORE reading state
	// or resolving the target (F11 + TCL-06), with the fencing handle so
	// effect sites can refuse a broken holder's late writes (F16).
	if err := state.EnsureAppDir(ctx, exec, cfg.App); err != nil {
		return fmt.Errorf("creating app directory: %w", err)
	}
	lk, err := state.AcquireLockFenced(ctx, exec, cfg.App)
	if err != nil {
		return fmt.Errorf("acquiring deploy lock: %w", err)
	}
	defer state.ReleaseLockFenced(exec, lk, cfg.App)
	lk.StartRenewal(exec)
	// The traffic switch back runs as a guarded Caddyfile commit (C01-2):
	// a rollback whose lock was broken mid-flight must not land its route
	// edit inside the new owner's window.
	cd = cd.WithCommitGuard(lk.GuardPrefix())

	// 2. Read state and resolve the rollback target — under the lock.
	current, err := state.Read(ctx, exec, cfg.App)
	if err != nil || current == nil {
		return fmt.Errorf("no deploy state found for %s — deploy first", cfg.App)
	}
	if current.IngressMode != "" {
		cfg.Ingress = current.IngressMode
	}
	target := cfg.ToHash
	if target == "" {
		target = current.PreviousHash
	}
	if target == "" {
		return ErrNoPreviousDeploy
	}
	if target == current.CurrentHash {
		return fmt.Errorf("target version %s is already current", target)
	}

	// 2b. The rollback's GENERATION identity (C01-8/9): everything this
	// rollback does is prepared against `fromGeneration` — the generation
	// state.json names right now — and creates `nextGeneration`. Container
	// names stay version-keyed, so a takeover + same-hash redeploy can put
	// a NEWER generation's container under a name this rollback resolved;
	// every destructive effect below (route switch, state commit, target
	// restarts, retirement stops) is fenced on the generation identity —
	// label checks and sidecar/CAS compares composed into the same remote
	// command as the effect — so a stale rollback can never stop, remove,
	// or overwrite a newer generation, no matter when its SSH commands
	// land.
	fromGeneration := current.Generation
	nextGeneration := fromGeneration + 1
	// The exact-block route CAS expectation (A12/T05): the app's managed
	// region as resolved NOW, under the lock. The route switch commits
	// only if this is still the live region; a successor's block refuses
	// the switch with both generations named.
	resolvedRouteHash := ""
	if cfg.usesCaddy() {
		region, _, rerr := cd.ReadManagedBlock(ctx, cfg.App)
		if rerr != nil {
			return fmt.Errorf("refusing to roll back %s with an unreadable route authority: %w", cfg.App, rerr)
		}
		resolvedRouteHash = caddy.ManagedRegionHash(region)
	}
	// switchedRouteHash is re-resolved after a SUCCESSFUL switch: the block
	// this rollback wrote but has not committed.
	switchedRouteHash := resolvedRouteHash

	fmt.Fprintf(out, "Rolling back %s from %s to %s...\n", cfg.App, current.CurrentHash, target)

	// 3. Find the target version's containers. This happens BEFORE anything
	// is stopped: a missing/pruned target must fail while the current
	// workload is still serving, not after host ingress already freed its
	// fixed port (audit F12).
	containers, err := dk.ListContainers(ctx, cfg.App)
	if err != nil {
		return fmt.Errorf("listing containers: %w", err)
	}
	var targetContainers, targetWebCandidates []docker.Container
	for _, c := range containers {
		if c.Labels["teploy.version"] != target {
			continue
		}
		targetContainers = append(targetContainers, c)
		if c.Labels["teploy.process"] == "web" {
			targetWebCandidates = append(targetWebCandidates, c)
		}
	}
	if len(targetContainers) == 0 {
		return fmt.Errorf("no containers found for version %s — they may have been pruned (keep_versions retention)", target)
	}
	if len(targetWebCandidates) == 0 {
		return fmt.Errorf("no web container found for version %s", target)
	}

	// 3b. Load the target release's recorded spec (F14) — the rollback
	// restores FROM THE RECORD, not from whatever the current teploy.yml
	// happens to say. A confirmed-missing record is backfilled from the
	// live containers (release-0 migration). The store must never be why a
	// rollback that worked before stops working: an UNREADABLE record (or a
	// failed backfill) degrades to the historical inspect-driven path with a
	// warning, and only the genuinely recoverable overlays are skipped.
	rec, recWarn := loadReleaseRecord(ctx, exec, dk, containers, cfg.App, target, current)
	if recWarn != nil {
		fmt.Fprintf(out, "Warning: %v — proceeding from live container inspection\n", recWarn)
	}
	if rec != nil {
		applyRecordToRollback(&cfg, rec, &healthCfg)
	}

	// The recorded primary container port (TCL-14): with more than one
	// published port, a fields[0] pick cannot tell the HTTP surface from an
	// auxiliary listener. Health checks probe the primary's host binding and
	// Caddy dials the primary container port.
	primaryContainerPort := 0
	if rec != nil {
		primaryContainerPort, _ = releasemeta.PrimaryContainerPort(rec)
	}

	// Ports the current (about-to-be-stopped) version is live on right now —
	// must not be handed to the target's recreated containers. A single-hop
	// rollback could never collide (the immediately-previous version's port
	// was freed by the deploy that superseded it, and nothing's claimed it
	// since), but a --to rollback reaching back further can: Docker's
	// ephemeral port allocator reuses freed ports, so an older version's
	// original port may since have been reassigned to what's now the live
	// container. See docker.Client.Restart's doc comment for how this is
	// resolved (fresh port allocated instead of reusing a colliding one).
	//
	// EXCEPT under fixed host ports (host ingress, or an app with publish
	// entries — F21), where this reasoning inverts. There the ports are
	// fixed by config, so every version shares them and the current
	// version's port ALWAYS equals the target's — avoiding it would
	// reallocate on every single rollback, silently republishing the app on
	// a random ephemeral port. The fixed ports are freed instead, by
	// stopping the current web containers before the target starts (below).
	fixedPorts := cfg.ingressHost() || releasemeta.HasFixedHostPorts(rec)
	avoidPorts := make(map[int]bool, len(current.CurrentPorts)+1)
	if !fixedPorts {
		for _, p := range current.CurrentPorts {
			avoidPorts[p] = true
		}
		if current.CurrentPort != 0 {
			avoidPorts[current.CurrentPort] = true
		}
	}

	// Match by the teploy.version label, not a name suffix: replica web
	// containers are named "<app>-web-<version>-1/-2", which end in "-1"/"-2",
	// not "-<version>" — a suffix match silently skips every replica (leaving
	// them stopped on rollback / orphaned on the next deploy). Every teploy
	// container carries the version label.
	// Fixed host ports (host ingress or publish entries — F21) recreate
	// rather than blue/green — the target reuses the same fixed port(s), so
	// the current web containers must stop before it starts. Deploy does
	// exactly this (see deploy.go's displacedHostWeb); rollback did not,
	// which is why it only ever "worked" by reallocating the port.
	//
	// Recorded so a failed health check can bring them back: with a fixed port
	// there is no moment where both versions are live, so the window between
	// stopping the current one and proving the target healthy is unavoidable.
	var displacedHostWeb []string
	// restoreDisplaced brings back the fixed-port workload displaced by host
	// ingress. Every failure path after the displacement runs it — an error
	// that returns while the displaced workload is still stopped leaves a
	// host-ingress app down (audit F12). The recreate's force-remove is
	// generation-checked (C01-9): a successor that re-created the name at a
	// newer generation is left strictly alone; recovery of OUR effects must
	// not destroy the new owner's workload. Holdership is deliberately NOT
	// checked here (A07: recovery is never fenced).
	restoreDisplaced := func() {
		recoveryCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		for _, name := range displacedHostWeb {
			if rerr := dk.RestartFenced(recoveryCtx, name, nil, fromGeneration, ""); rerr != nil {
				if state.GenerationFenced(rerr) {
					fmt.Fprintf(out, "  WARNING: could not restore %s after the failed rollback — a newer generation owns the name\n", name)
					continue
				}
				fmt.Fprintf(out, "  WARNING: could not restore %s after the failed rollback: %v\n", name, rerr)
			} else {
				fmt.Fprintf(out, "  Restored %s\n", name)
			}
		}
	}
	if fixedPorts {
		// Fence + generation check composed into the stop (F16 + C01-8/9):
		// stopping the fixed-port workload is this rollback's first
		// destructive effect — the composed command refuses when the lock
		// was broken (holdership) or the name now belongs to a newer
		// generation's container (identity). Nothing of ours needs
		// restoring yet, so a refusal is a plain abort.
		for _, c := range containers {
			// Displace only the RUNNING web containers of the AUTHORITATIVE
			// current generation (TCL-07). The old filter (any non-target
			// web container) also captured stopped historical containers;
			// they were recorded as "displaced live instances" and a failed
			// rollback would Restart them — resurrecting obsolete versions
			// and colliding with the real current generation on the fixed
			// port. A stopped historical container was not serving traffic
			// when the operation began; it must stay stopped.
			if c.Labels["teploy.process"] != "web" || c.Labels["teploy.version"] != current.CurrentHash {
				continue
			}
			if c.State != "running" {
				continue
			}
			ref := c.Name
			if c.ID != "" {
				ref = c.ID
			}
			if err := dk.StopGenerationFenced(ctx, ref, 10, fromGeneration, lk.GuardPrefix()); err != nil {
				// Earlier containers may already be stopped — restore them
				// before bailing (F12).
				restoreDisplaced()
				return fmt.Errorf("stopping current host-ingress container %s to free its fixed port: %w", c.Name, err)
			}
			fmt.Fprintf(out, "Freed the fixed port from %s\n", c.Name)
			displacedHostWeb = append(displacedHostWeb, c.Name)
		}
	}

	var started []string
	var targetWeb []docker.Container
	for _, c := range targetContainers {
		// Fence + generation identity composed into the recreate (F16 +
		// C01-8/9): the holdership guard and the generation label check
		// ride the same remote command as the force-remove — the
		// destructive half of Restart. A lost fence unwinds what this
		// rollback started and restores the displaced workload before
		// bailing (recovery is never holdership-fenced); a generation
		// refusal means a newer generation now owns the name (takeover +
		// same-hash redeploy) and the recreate is refused instead of
		// destroying the successor's container. The recreated container
		// PRESERVES its labels (Recreate re-emits them), so the target
		// workload keeps the generation identity it was created under.
		//
		// Recreate rather than `docker start`: Docker 29 silently fails
		// to re-publish HostConfig.PortBindings on `docker start` when
		// another container has taken+released the host port in the
		// interim — see docker.Client.Restart's doc comment.
		fmt.Fprintf(out, "Starting %s...\n", c.Name)
		if err := dk.RestartFenced(ctx, c.Name, avoidPorts, fromGeneration, lk.GuardPrefix()); err != nil {
			if state.FenceLost(err) || state.GenerationFenced(err) {
				for _, name := range started {
					dk.Stop(ctx, name, 5)
				}
				restoreDisplaced()
				return err
			}
			// Stop anything this rollback already (re)started, then put the
			// displaced fixed-port workload back (F12).
			for _, name := range started {
				dk.Stop(ctx, name, 5)
			}
			restoreDisplaced()
			return fmt.Errorf("restarting target container %s: %w", c.Name, err)
		}
		started = append(started, c.Name)
		if c.Labels["teploy.process"] == "web" {
			targetWeb = append(targetWeb, c)
		}
	}

	sort.Slice(targetWeb, func(i, j int) bool { return targetWeb[i].Name < targetWeb[j].Name })

	// 3. Health check every target replica's host port (not just the
	// primary), so a replica that comes back unhealthy after recreation is
	// caught before the traffic swap — mirrors the deploy path.
	//
	// Derived by inspecting the just-restarted containers (HostPort) rather
	// than state.AppState.PreviousPort/PreviousPorts: those only ever
	// remember the single most recent previous version, which doesn't
	// cover a --to <hash> rollback that goes back further than one step.
	// Inspection works uniformly for both cases and can't drift from the
	// containers' actual live config the way a persisted port number could.
	fmt.Fprintln(out, "Running health check...")
	deployer := &Deployer{exec: exec, out: out}
	healthPorts := make([]int, 0, len(targetWeb))
	// Probe the address docker actually bound, not localhost: a container
	// published on a specific IP is unreachable there, which would fail every
	// rollback of a host-ingress app with a bind address.
	healthBindHost := ""
	for _, c := range targetWeb {
		var p int
		// TCL-14: prefer the binding of the recorded PRIMARY container port
		// over the first field docker's map happens to print — a multi-port
		// container's auxiliary listeners must not steal the health probe.
		if primaryContainerPort > 0 {
			if hp, herr := dk.HostPortFor(ctx, c.Name, primaryContainerPort); herr == nil {
				p = hp
			} else {
				p, err = dk.HostPort(ctx, c.Name)
			}
		} else {
			p, err = dk.HostPort(ctx, c.Name)
		}
		if err != nil {
			// A failed inspect must not strand the displaced fixed-port
			// workload (F12).
			for _, name := range started {
				dk.Stop(ctx, name, 5)
			}
			restoreDisplaced()
			return fmt.Errorf("inspecting target container host port: %w", err)
		}
		healthPorts = append(healthPorts, p)
		if healthBindHost == "" {
			healthBindHost = dk.HostBindIP(ctx, c.Name)
		}
	}
	// Surface the gate before it runs (C03): which probe mode — from the
	// target release's recorded spec when one exists — and what deadline.
	if len(healthPorts) > 0 {
		fmt.Fprintf(out, "  Readiness: %s\n", readinessSummary(healthCfg, healthPorts[0]))
	}
	for _, p := range healthPorts {
		if err := deployer.healthCheck(ctx, p, healthCfg, healthBindHost); err != nil {
			// Stop what we started and bail.
			for _, name := range started {
				dk.Stop(ctx, name, 5)
			}
			// Under host ingress the current version was stopped to free the
			// fixed port. Put it back, or a failed rollback leaves the app down
			// rather than where it started. Best-effort: there is nothing better
			// to do if this also fails, and the original error is the useful one.
			restoreDisplaced()
			return fmt.Errorf("health check failed on target version: %w", err)
		}
	}
	fmt.Fprintln(out, "  Health check passed")
	// 4. Route traffic to the target container(s).
	// Use the explicit container name(s) rather than the app network alias
	// so Docker DNS doesn't briefly round-robin to the current (about-to-
	// be-stopped) container during the swap.
	//
	// Caddy dials the upstream over the Docker network, so it needs each
	// container's *internal* port, inspected live (see targetWeb above for
	// why this isn't sourced from persisted state).
	//
	// Skipped under ingress: external — the external thing already
	// points at the app's network alias, which will resolve to whichever
	// container Teploy starts (in this case, the target version's).
	if cfg.usesCaddy() {
		fmt.Fprintln(out, "Updating routes...")
		// Fence (F16, C01-2 composition) + the exact-block CAS (A12/T05):
		// the Caddyfile commit runs under this rollback's holdership guard
		// AND a compare-and-swap on the managed region resolved at step 2b
		// — stamped with the generation this rollback creates. A late
		// write from a broken holder cannot hijack a newer operation's
		// route, and a newer writer's block (a successor that already
		// switched) refuses this switch with BOTH generations named
		// instead of being overwritten.
		cad := cd.WithGeneration(nextGeneration).
			WithRouteCAS(cfg.App, []string{resolvedRouteHash}, fromGeneration)
		// failRoutePhase unwinds a route-phase failure the same way a
		// health/start failure unwinds (audit T06): the upstream-port
		// inspections and SetRoute/SetLoadBalancerHealth used to return
		// directly, leaving the uncommitted target running and — under
		// fixed ports, where Caddy still points at the STOPPED current
		// container — the app dark.
		failRoutePhase := func(reason error) error {
			for _, name := range started {
				dk.Stop(ctx, name, 5)
			}
			restoreDisplaced()
			return reason
		}
		tls := caddy.TLS{Cert: cfg.TLSCert, Key: cfg.TLSKey, Internal: cfg.TLSInternal}
		// The Caddy upstream port is the recorded primary container port
		// when there is one (TCL-14); without a record the first exposed
		// port remains the historical best-effort pick.
		upstreamPort := func(name string) (int, error) {
			if primaryContainerPort > 0 {
				return primaryContainerPort, nil
			}
			return dk.InternalPort(ctx, name)
		}
		if len(targetWeb) > 1 {
			upstreams := make([]caddy.Upstream, 0, len(targetWeb))
			for _, c := range targetWeb {
				port, err := upstreamPort(c.Name)
				if err != nil {
					return failRoutePhase(fmt.Errorf("inspecting target container port: %w", err))
				}
				upstreams = append(upstreams, caddy.Upstream{Dial: fmt.Sprintf("%s:%d", c.Name, port)})
			}
			if err := cad.SetLoadBalancerHealth(ctx, cfg.App, cfg.Domain, upstreams, healthCfg.Path, tls, cfg.CaddyExtra, cfg.Cache, cfg.Firewall, cfg.Access); err != nil {
				return failRoutePhase(routePhaseErr(err))
			}
			fmt.Fprintf(out, "  Traffic load-balanced across %d replicas\n", len(targetWeb))
		} else {
			port, err := upstreamPort(targetWeb[0].Name)
			if err != nil {
				return failRoutePhase(fmt.Errorf("inspecting target container port: %w", err))
			}
			if err := cad.SetRoute(ctx, cfg.App, cfg.Domain, targetWeb[0].Name, port, tls, cfg.CaddyExtra, cfg.Cache, cfg.Firewall, cfg.Access); err != nil {
				return failRoutePhase(routePhaseErr(err))
			}
			fmt.Fprintln(out, "  Traffic routed to target version")
		}
		// The switched-to region is this rollback's own uncommitted block —
		// the restore path's CAS accepts exactly {resolved, this}.
		if region, _, rerr := cd.ReadManagedBlock(ctx, cfg.App); rerr == nil {
			switchedRouteHash = caddy.ManagedRegionHash(region)
		}
	} else {
		fmt.Fprintf(out, "Skipping Caddy route restore (ingress: %s)\n", cfg.Ingress)
	}

	// 5. Commit state before stopping the current workload. If the commit
	// fails, route back to the still-running current containers and stop only
	// the uncommitted rollback target.
	//
	// Swap state: target becomes current, the version rolled back from
	// becomes previous — mirrors type:static's Rollback (a plain 1-level
	// swap, not a deeper history stack: a --to rollback several versions
	// back still only remembers "the version just abandoned" as
	// PreviousHash, same as a normal rollback would). Carry the durable
	// Domain through (state.Write omits an empty domain, so dropping it
	// here blanked the persisted domain and broke the next rollback).
	deploymentType := current.DeploymentType
	if deploymentType == "" {
		deploymentType = "container"
	}
	ingress := cfg.Ingress
	if ingress == "" && current.IngressMode != "" {
		ingress = current.IngressMode
	}
	domain := current.Domain
	if domain == "" {
		domain = cfg.Domain
	}
	newState := state.NewAppliedState(current, deploymentType, ingress, domain)
	newState.CurrentPort = healthPorts[0]
	newState.CurrentHash = target
	newState.PreviousPort = current.CurrentPort
	newState.PreviousHash = current.CurrentHash
	newState.CurrentPorts = healthPorts
	newState.PreviousPorts = current.CurrentPorts
	if current.PreviousRelease != nil && current.PreviousRelease.Hash == target {
		newState.ApplyRelease(current.PreviousRelease)
	}
	// The target release record's ImageRef (applied just above) is the
	// requested reference and wins. Without one, fall back to the
	// container's image — resolved to its tag, because web containers are
	// created by immutable image ID (A52) and docker ps then reports the
	// bare short ID: recording that made ImageRef an unpullable 12-hex
	// string after a rollback (a DR restore on a fresh host resolves the
	// image from ImageRef).
	if newState.ImageRef == "" {
		newState.ImageRef = dk.ResolveImageTags(ctx, targetWeb[:1])[0].Image
	}
	if digest, digestErr := dk.ContainerImageDigest(ctx, targetWeb[0].Name); digestErr == nil {
		newState.ImageDigest = digest
	}
	// The commit runs under the fence (F16) and the generation CAS
	// (C01-8/9): the atomic rename that makes the rollback authoritative
	// is a guarded effect chained with a compare-and-swap on the committed
	// generation — a rollback that resolved the world at fromGeneration
	// cannot commit over a successor's newer generation (ErrGenerationFenced
	// naming both), so a stale rollback can never become authority.
	if err := state.WriteFencedGeneration(ctx, exec, cfg.App, newState, lk, fromGeneration); err != nil {
		// Fixed host ports: the target holds them. Stop it, restore the
		// displaced workload, then remove the uncommitted target — previously
		// this branch skipped the restore and still claimed "the original
		// workload was left running" (audit F12).
		if fixedPorts {
			for _, name := range started {
				dk.Stop(ctx, name, 5)
			}
			restoreDisplaced()
			for _, name := range started {
				dk.Remove(ctx, name)
			}
			return fmt.Errorf("committing authoritative applied state after rollback: %w; the fixed-port workload was restored and the uncommitted target was removed", err)
		}
		if cfg.usesCaddy() {
			// The restore is CAS-fenced (A12/T05): it may only overwrite
			// the block this rollback resolved or the one it switched to.
			// When the commit was refused because a NEWER generation
			// committed (ErrGenerationFenced) and that successor also
			// switched the route, the restore refuses too — the newer
			// generation's route is never clobbered by this rollback's
			// compensation. When the route is still ours to undo, it is
			// undone.
			if restoreErr := restoreRollbackRoute(ctx, exec, out, cd, dk, cfg, current, containers, restoreRouteCAS(resolvedRouteHash, switchedRouteHash), fromGeneration); restoreErr != nil {
				var routeCAS *caddy.ErrRouteCAS
				if errors.As(restoreErr, &routeCAS) {
					return fmt.Errorf("committing authoritative applied state after rollback route switch: %w; the route was left to the newer generation that owns it (%v)", err, routeCAS)
				}
				return fmt.Errorf("committing authoritative applied state after rollback route switch: %w; restoring the original route failed: %v; original and target workloads were left running to avoid routing to a stopped container", err, restoreErr)
			}
		}
		for _, name := range started {
			dk.Stop(ctx, name, 5)
		}
		return fmt.Errorf("committing authoritative applied state after rollback route switch: %w; the original route was restored, the original workload was left running, and the uncommitted target was stopped", err)
	}

	// 6. The authoritative commit succeeded; the prior current workload can
	// now be stopped (match by version label — see step 2). A fence loss
	// here (F16) means another operation owns the app: refuse further
	// stops loudly rather than interleave — leaving the superseded workload
	// running is degraded but visible. The request-drain window (C03)
	// applies first when configured: the route already switched back to
	// the target, and the superseded generation may still hold in-flight
	// requests on its connections.
	if cfg.DrainSeconds > 0 && cfg.usesCaddy() {
		fmt.Fprintf(out, "Draining %s's superseded generation for %ds (in-flight requests complete; traffic serves %s)...\n", cfg.App, cfg.DrainSeconds, target)
		select {
		case <-ctx.Done():
			fmt.Fprintf(out, "  Drain window cut short (%v) — proceeding to retirement\n", ctx.Err())
		case <-time.After(time.Duration(cfg.DrainSeconds) * time.Second):
		}
	}
	for _, c := range containers {
		if c.Labels["teploy.version"] != current.CurrentHash || c.State != "running" {
			continue
		}
		// C01-8/9: the retirement stop is a composed guarded effect —
		// holdership guard + generation label check + stop, addressing the
		// container by its exact ID from this rollback's under-lock
		// inventory. A delayed command landing after a takeover reads the
		// live container's immutable generation label: a newer
		// generation's container (same names, takeover + same-hash
		// redeploy) is REFUSED, never stopped — the property this slice
		// exists for. A fence loss stops the sweep loudly (degraded,
		// visible), as before.
		ref := c.Name
		if c.ID != "" {
			ref = c.ID
		}
		fmt.Fprintf(out, "Stopping %s...\n", c.Name)
		if err := dk.StopGenerationFenced(ctx, ref, stopTimeout, fromGeneration, lk.GuardPrefix()); err != nil {
			if state.FenceLost(err) {
				fmt.Fprintf(out, "Warning: current-workload cleanup stopped — %v\n", err)
				break
			}
			if state.GenerationFenced(err) {
				fmt.Fprintf(out, "Warning: retirement of %s refused — the container belongs to a newer generation (%v)\n", c.Name, err)
				continue
			}
			fmt.Fprintf(out, "Warning: could not stop %s: %v\n", c.Name, err)
		}
	}

	// 7. Log.
	state.AppendLog(ctx, exec, state.LogEntry{
		Timestamp:  time.Now().UTC(),
		App:        cfg.App,
		Type:       "rollback",
		Hash:       target,
		Success:    true,
		DurationMs: time.Since(start).Milliseconds(),
	})

	duration := time.Since(start)
	fmt.Fprintf(out, "\nRolled back %s to version %s in %s\n", cfg.App, target, duration.Round(time.Millisecond))
	return nil
}

// restoreRollbackRoute compensates a failed rollback's route switch by
// putting the CURRENT (rolled-back-from) release's route back — the
// failed-rollback twin of deploy's restorePreviousRoute (C01-7's A12/T05
// remainder). The F14 record of the version being rolled back FROM is the
// receipt of what was serving before the switch and is AUTHORITATIVE for
// the restore: domain, replica upstream names, the recorded primary
// container port, TLS/extra/cache/firewall/access, and the LB health path.
// Reconstruct-from-inspection remains only as the announced legacy
// fallback (pre-F14 installs, unreadable records, no primary port).
//
// C01-8/9 + A12/T05: the restored block is stamped with fromGeneration and
// the commit runs under the exact-block CAS over routeCAS — {the region
// this rollback resolved, the region it switched to}. The compensation is
// deliberately UNFENCED (A07), which is why its write must be
// identity-fenced: a successor's block is in NEITHER set and the restore
// refuses (caddy.ErrRouteCAS) instead of clobbering the newer generation's
// route — the acceptance shape for the delayed-effect race.
func restoreRollbackRoute(ctx context.Context, exec ssh.Executor, out io.Writer, cd *caddy.Client, dk *docker.Client, cfg RollbackConfig, current *state.AppState, containers []docker.Container, routeCAS []string, generation uint64) error {
	restoreCaddy := cd.WithGeneration(generation)
	if len(routeCAS) > 0 {
		restoreCaddy = restoreCaddy.WithRouteCAS(cfg.App, routeCAS, generation)
	}

	rec, recErr := releasemeta.Read(ctx, exec, cfg.App, current.CurrentHash)
	switch {
	case recErr != nil:
		fmt.Fprintf(out, "Warning: the release record for %s@%s could not be read (%v) — restoring the original route from live inspection instead of the recorded receipt\n", cfg.App, current.CurrentHash, recErr)
	case rec == nil:
		fmt.Fprintf(out, "Warning: no release record for %s@%s (pre-F14 install) — restoring the original route from live inspection instead of the recorded receipt\n", cfg.App, current.CurrentHash)
	default:
		if port, ok := releasemeta.PrimaryContainerPort(rec); ok {
			return restoreRollbackRouteFromReceipt(ctx, restoreCaddy, cfg, current, rec, port)
		}
		fmt.Fprintf(out, "Warning: the release record for %s@%s names no primary container port — restoring the original route from live inspection instead of the recorded receipt\n", cfg.App, current.CurrentHash)
	}
	return restoreRollbackRouteFromInspection(ctx, restoreCaddy, dk, cfg, current, containers)
}

// routePhaseErr keeps fence/CAS refusals classified instead of wrapping
// them into generic route failures the caller cannot distinguish.
func routePhaseErr(err error) error {
	var routeCAS *caddy.ErrRouteCAS
	if errors.As(err, &routeCAS) {
		return routeCAS
	}
	if state.FenceLost(err) {
		return fmt.Errorf("switching the route back: %w", err)
	}
	return fmt.Errorf("updating route: %w", err)
}

// restoreRollbackRouteFromReceipt renders the rolled-back-from release's
// route from its F14 record — zero live inspect. The upstream NAMES are
// deterministic per release; rollback does not rename the from-version's
// containers, so no _replaced suffix applies here.
func restoreRollbackRouteFromReceipt(ctx context.Context, cd *caddy.Client, cfg RollbackConfig, current *state.AppState, rec *releasemeta.Record, containerPort int) error {
	replicas := rec.Replicas
	if replicas <= 0 {
		replicas = 1
	}
	names := make([]string, replicas)
	upstreams := make([]caddy.Upstream, replicas)
	for i := range replicas {
		name := docker.ReplicaContainerName(cfg.App, "web", current.CurrentHash, i+1, replicas)
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
		return cd.SetLoadBalancerHealth(ctx, cfg.App, domain, upstreams, healthPath, tls, caddyExtra, cache, fw, access)
	}
	return cd.SetRoute(ctx, cfg.App, domain, names[0], containerPort, tls, caddyExtra, cache, fw, access)
}

// restoreRollbackRouteFromInspection is the announced legacy fallback:
// reconstruct the original block from the running from-version containers.
func restoreRollbackRouteFromInspection(ctx context.Context, cd *caddy.Client, dk *docker.Client, cfg RollbackConfig, current *state.AppState, containers []docker.Container) error {
	var currentWeb []docker.Container
	for _, container := range containers {
		if container.Labels["teploy.version"] == current.CurrentHash && container.Labels["teploy.process"] == "web" && container.State == "running" {
			currentWeb = append(currentWeb, container)
		}
	}
	if len(currentWeb) == 0 {
		return fmt.Errorf("no running web containers found for original version %s", current.CurrentHash)
	}
	sort.Slice(currentWeb, func(i, j int) bool { return currentWeb[i].Name < currentWeb[j].Name })

	domain := current.Domain
	if domain == "" {
		domain = cfg.Domain
	}
	tls := caddy.TLS{Cert: cfg.TLSCert, Key: cfg.TLSKey, Internal: cfg.TLSInternal}
	if len(currentWeb) == 1 {
		port, err := dk.InternalPort(ctx, currentWeb[0].Name)
		if err != nil {
			return err
		}
		return cd.SetRoute(ctx, cfg.App, domain, currentWeb[0].Name, port, tls, cfg.CaddyExtra, cfg.Cache, cfg.Firewall, cfg.Access)
	}

	upstreams := make([]caddy.Upstream, len(currentWeb))
	for i, container := range currentWeb {
		port, err := dk.InternalPort(ctx, container.Name)
		if err != nil {
			return err
		}
		upstreams[i] = caddy.Upstream{Dial: fmt.Sprintf("%s:%d", container.Name, port)}
	}
	return cd.SetLoadBalancerHealth(ctx, cfg.App, domain, upstreams, cfg.Health.withDefaults().Path, tls, cfg.CaddyExtra, cfg.Cache, cfg.Firewall, cfg.Access)
}

// loadReleaseRecord resolves the F14 record for the rollback target,
// backfilling a "release-0" record from the live containers when the release
// predates the store (the convergence migration). The returned warning is
// non-fatal by contract: rollback worked before the store existed and must
// keep working without it — an unreadable record or failed backfill means
// the overlays are skipped, never that the rollback refuses.
func loadReleaseRecord(ctx context.Context, exec ssh.Executor, dk *docker.Client, containers []docker.Container, app, hash string, current *state.AppState) (*releasemeta.Record, error) {
	rec, err := releasemeta.Read(ctx, exec, app, hash)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		rec, err = releasemeta.Backfill(ctx, exec, dk, containers, app, hash, current)
		if err != nil {
			return nil, fmt.Errorf("backfilling release metadata for %s@%s: %w", app, hash, err)
		}
	}
	return rec, nil
}

// applyRecordToRollback overlays the recorded release spec onto the rollback
// config: the record is the target release's truth, the CLI-passed values
// (from the current teploy.yml) are the fallback. Wholesale replacement per
// field group — an empty recorded TLS block MEANS "this release had no
// custom TLS" and must not be silently upgraded to whatever the current
// config says. Backfilled records carry no health/caddy data (unrecoverable
// from containers), so those overlays simply don't fire for them.
func applyRecordToRollback(cfg *RollbackConfig, rec *releasemeta.Record, healthCfg *HealthConfig) {
	if rec.IngressMode != "" {
		cfg.Ingress = rec.IngressMode
	}
	if rec.Domain != "" {
		cfg.Domain = rec.Domain
	}
	if rec.Health != nil {
		if rec.Health.Mode != "" {
			healthCfg.Mode = rec.Health.Mode
		}
		if rec.Health.Path != "" {
			healthCfg.Path = rec.Health.Path
		}
		if rec.Health.TimeoutSeconds > 0 {
			healthCfg.Timeout = time.Duration(rec.Health.TimeoutSeconds) * time.Second
		}
		if rec.Health.IntervalSeconds > 0 {
			healthCfg.Interval = time.Duration(rec.Health.IntervalSeconds) * time.Second
		}
	}
	if rec.Caddy != nil {
		cfg.TLSCert = rec.Caddy.TLSCert
		cfg.TLSKey = rec.Caddy.TLSKey
		cfg.TLSInternal = rec.Caddy.TLSInternal
		cfg.CaddyExtra = rec.Caddy.CaddyExtra
		cfg.Cache = rec.Caddy.Cache
		if rec.Caddy.Firewall != nil {
			cfg.Firewall = *rec.Caddy.Firewall
		}
		if rec.Caddy.Access != nil {
			cfg.Access = *rec.Caddy.Access
		}
	}
}
