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
	Health      HealthConfig
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
	CaddyExtra  string // mirrors Config.CaddyExtra — preserves user directives across rollback
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

// Rollback reverts to a previous deploy version — the immediately previous
// one by default, or cfg.ToHash if set (mirroring type:static's --to).
// Starts the target version's containers, health checks, re-routes
// traffic, and stops the current containers. Updates state so target
// becomes current and the version rolled back from becomes previous.
func Rollback(ctx context.Context, exec ssh.Executor, out io.Writer, cfg RollbackConfig) error {
	start := time.Now()
	dk := docker.NewClient(exec)
	cd := caddy.NewClient(exec)
	healthCfg := cfg.Health.withDefaults()

	stopTimeout := cfg.StopTimeout
	if stopTimeout == 0 {
		stopTimeout = 10
	}

	// 1. Read state and resolve the rollback target.
	current, err := state.Read(ctx, exec, cfg.App)
	if err != nil || current == nil {
		return fmt.Errorf("no deploy state found for %s — deploy first", cfg.App)
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

	fmt.Fprintf(out, "Rolling back %s from %s to %s...\n", cfg.App, current.CurrentHash, target)

	// 2. Find and start the target version's containers.
	containers, err := dk.ListContainers(ctx, cfg.App)
	if err != nil {
		return fmt.Errorf("listing containers: %w", err)
	}

	// Match by the teploy.version label, not a name suffix: replica web
	// containers are named "<app>-web-<version>-1/-2", which end in "-1"/"-2",
	// not "-<version>" — a suffix match silently skips every replica (leaving
	// them stopped on rollback / orphaned on the next deploy). Every teploy
	// container carries the version label.
	var started []string
	var targetWeb []docker.Container
	for _, c := range containers {
		if c.Labels["teploy.version"] != target {
			continue
		}
		fmt.Fprintf(out, "Starting %s...\n", c.Name)
		// Recreate rather than `docker start`: Docker 29 silently fails
		// to re-publish HostConfig.PortBindings on `docker start` when
		// another container has taken+released the host port in the
		// interim — a common case if rolling back after deploying a
		// neighboring app that reused the port. Restart() inspects the
		// stopped container, force-removes, and `docker run`s fresh
		// with the same config.
		if err := dk.Restart(ctx, c.Name); err != nil {
			return fmt.Errorf("restarting target container %s: %w", c.Name, err)
		}
		started = append(started, c.Name)
		if c.Labels["teploy.process"] == "web" {
			targetWeb = append(targetWeb, c)
		}
	}

	if len(started) == 0 {
		return fmt.Errorf("no containers found for version %s — they may have been pruned (keep_versions retention)", target)
	}
	sort.Slice(targetWeb, func(i, j int) bool { return targetWeb[i].Name < targetWeb[j].Name })
	if len(targetWeb) == 0 {
		return fmt.Errorf("no web container found for version %s", target)
	}

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
	for _, c := range targetWeb {
		p, err := dk.HostPort(ctx, c.Name)
		if err != nil {
			return fmt.Errorf("inspecting target container host port: %w", err)
		}
		healthPorts = append(healthPorts, p)
	}
	for _, p := range healthPorts {
		if err := deployer.healthCheck(ctx, p, healthCfg); err != nil {
			// Stop what we started and bail.
			for _, name := range started {
				dk.Stop(ctx, name, 5)
			}
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
		tls := caddy.TLS{Cert: cfg.TLSCert, Key: cfg.TLSKey, Internal: cfg.TLSInternal}
		if len(targetWeb) > 1 {
			upstreams := make([]caddy.Upstream, 0, len(targetWeb))
			for _, c := range targetWeb {
				port, err := dk.InternalPort(ctx, c.Name)
				if err != nil {
					return fmt.Errorf("inspecting target container port: %w", err)
				}
				upstreams = append(upstreams, caddy.Upstream{Dial: fmt.Sprintf("%s:%d", c.Name, port)})
			}
			if err := cd.SetLoadBalancer(ctx, cfg.App, cfg.Domain, upstreams, tls, cfg.CaddyExtra); err != nil {
				return fmt.Errorf("updating load balancer route: %w", err)
			}
			fmt.Fprintf(out, "  Traffic load-balanced across %d replicas\n", len(targetWeb))
		} else {
			port, err := dk.InternalPort(ctx, targetWeb[0].Name)
			if err != nil {
				return fmt.Errorf("inspecting target container port: %w", err)
			}
			if err := cd.SetRoute(ctx, cfg.App, cfg.Domain, targetWeb[0].Name, port, tls, cfg.CaddyExtra); err != nil {
				return fmt.Errorf("updating route: %w", err)
			}
			fmt.Fprintln(out, "  Traffic routed to target version")
		}
	} else {
		fmt.Fprintf(out, "Skipping Caddy route restore (ingress: %s)\n", cfg.Ingress)
	}

	// 5. Stop current containers (match by version label — see step 2).
	for _, c := range containers {
		if c.Labels["teploy.version"] == current.CurrentHash && c.State == "running" {
			fmt.Fprintf(out, "Stopping %s...\n", c.Name)
			dk.Stop(ctx, c.Name, stopTimeout)
		}
	}

	// 6. Swap state: target becomes current, the version rolled back from
	// becomes previous — mirrors type:static's Rollback (a plain 1-level
	// swap, not a deeper history stack: a --to rollback several versions
	// back still only remembers "the version just abandoned" as
	// PreviousHash, same as a normal rollback would). Carry the durable
	// Domain through (state.Write omits an empty domain, so dropping it
	// here blanked the persisted domain and broke the next rollback).
	newState := &state.AppState{
		CurrentPort:   healthPorts[0],
		CurrentHash:   target,
		PreviousPort:  current.CurrentPort,
		PreviousHash:  current.CurrentHash,
		Domain:        current.Domain,
		CurrentPorts:  healthPorts,
		PreviousPorts: current.CurrentPorts,
	}
	if err := state.Write(ctx, exec, cfg.App, newState); err != nil {
		return fmt.Errorf("writing state: %w", err)
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
