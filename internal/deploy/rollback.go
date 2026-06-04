package deploy

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/useteploy/teploy/internal/caddy"
	"github.com/useteploy/teploy/internal/docker"
	"github.com/useteploy/teploy/internal/ssh"
	"github.com/useteploy/teploy/internal/state"
)

// RollbackConfig holds parameters for a rollback operation.
type RollbackConfig struct {
	App         string
	Domain      string
	StopTimeout int
	Health      HealthConfig
	// TLSCert / TLSKey preserve custom-cert TLS termination across a
	// rollback (container-side paths, already on the server from deploy).
	// Empty = ACME. Without these, rolling back would regenerate the Caddy
	// block without the cert and break TLS the same way a deploy would.
	TLSCert string
	TLSKey  string
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

// Rollback reverts to the previous deploy version.
// Starts previous containers, health checks, re-routes traffic,
// and stops current containers. Updates state so current ↔ previous swap.
func Rollback(ctx context.Context, exec ssh.Executor, out io.Writer, cfg RollbackConfig) error {
	start := time.Now()
	dk := docker.NewClient(exec)
	cd := caddy.NewClient(exec)
	healthCfg := cfg.Health.withDefaults()

	stopTimeout := cfg.StopTimeout
	if stopTimeout == 0 {
		stopTimeout = 10
	}

	// 1. Read state.
	current, err := state.Read(ctx, exec, cfg.App)
	if err != nil || current == nil {
		return fmt.Errorf("no deploy state found for %s — deploy first", cfg.App)
	}
	if current.PreviousHash == "" {
		return fmt.Errorf("no previous deploy to roll back to")
	}
	if current.PreviousPort == 0 {
		return fmt.Errorf("previous deploy has no port — cannot roll back")
	}

	fmt.Fprintf(out, "Rolling back %s from %s to %s...\n", cfg.App, current.CurrentHash, current.PreviousHash)

	// 2. Find and start previous containers.
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
	for _, c := range containers {
		if c.Labels["teploy.version"] == current.PreviousHash {
			fmt.Fprintf(out, "Starting %s...\n", c.Name)
			// Recreate rather than `docker start`: Docker 29 silently fails
			// to re-publish HostConfig.PortBindings on `docker start` when
			// another container has taken+released the host port in the
			// interim — a common case if rolling back after deploying a
			// neighboring app that reused the port. Restart() inspects the
			// stopped container, force-removes, and `docker run`s fresh
			// with the same config.
			if err := dk.Restart(ctx, c.Name); err != nil {
				return fmt.Errorf("restarting previous container %s: %w", c.Name, err)
			}
			started = append(started, c.Name)
		}
	}

	if len(started) == 0 {
		return fmt.Errorf("no previous containers found for version %s — they may have been removed", current.PreviousHash)
	}

	// 3. Health check on previous port.
	fmt.Fprintln(out, "Running health check...")
	deployer := &Deployer{exec: exec, out: out}
	if err := deployer.healthCheck(ctx, current.PreviousPort, healthCfg); err != nil {
		// Stop what we started and bail.
		for _, name := range started {
			dk.Stop(ctx, name, 5)
		}
		return fmt.Errorf("health check failed on previous version: %w", err)
	}
	fmt.Fprintln(out, "  Health check passed")

	// 4. Route traffic to previous container.
	// Use the explicit previous container name rather than the app network
	// alias so Docker DNS doesn't briefly round-robin to the current
	// (about-to-be-stopped) container during the swap.
	//
	// Caddy dials the upstream over the Docker network, so it needs the
	// container's *internal* port — not the host-mapped port stored in
	// state as PreviousPort (used for health checks). The two often
	// match but can differ when the app's ContainerPort config has
	// changed. Inspect the live container so rollback routes correctly
	// regardless.
	//
	// Skipped under ingress: external — the external thing already
	// points at the app's network alias, which will resolve to whichever
	// container Teploy starts (in this case, the previous one).
	if cfg.usesCaddy() {
		fmt.Fprintln(out, "Updating routes...")
		previousContainer := docker.ContainerName(cfg.App, "web", current.PreviousHash)
		internalPort, err := dk.InternalPort(ctx, previousContainer)
		if err != nil {
			return fmt.Errorf("inspecting previous container port: %w", err)
		}
		if err := cd.SetRoute(ctx, cfg.App, cfg.Domain, previousContainer, internalPort, caddy.TLS{Cert: cfg.TLSCert, Key: cfg.TLSKey}); err != nil {
			return fmt.Errorf("updating route: %w", err)
		}
		fmt.Fprintln(out, "  Traffic routed to previous version")
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

	// 6. Swap state: previous becomes current, current becomes previous.
	// Carry the durable Domain through (state.Write omits an empty domain, so
	// dropping it here blanked the persisted domain and broke the next
	// rollback), and mirror the replica port arrays alongside the scalar swap
	// so a multi-replica app's port set stays consistent.
	newState := &state.AppState{
		CurrentPort:   current.PreviousPort,
		CurrentHash:   current.PreviousHash,
		PreviousPort:  current.CurrentPort,
		PreviousHash:  current.CurrentHash,
		Domain:        current.Domain,
		CurrentPorts:  current.PreviousPorts,
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
		Hash:       current.PreviousHash,
		Success:    true,
		DurationMs: time.Since(start).Milliseconds(),
	})

	duration := time.Since(start)
	fmt.Fprintf(out, "\nRolled back %s to version %s in %s\n", cfg.App, current.PreviousHash, duration.Round(time.Millisecond))
	return nil
}
