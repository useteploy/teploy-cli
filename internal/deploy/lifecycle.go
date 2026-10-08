package deploy

import (
	"context"
	"fmt"
	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/releasemeta"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/useteploy/teploy/internal/docker"
	"github.com/useteploy/teploy/internal/ssh"
	"github.com/useteploy/teploy/internal/state"
)

// Lifecycle manages stop/start/restart of app containers.
type Lifecycle struct {
	exec   ssh.Executor
	docker *docker.Client
	out    io.Writer
}

// NewLifecycle creates a lifecycle manager.
func NewLifecycle(exec ssh.Executor, out io.Writer) *Lifecycle {
	return &Lifecycle{
		exec:   exec,
		docker: docker.NewClient(exec),
		out:    out,
	}
}

// Stop stops all running containers for the app.
func (l *Lifecycle) Stop(ctx context.Context, app string, timeout int) error {
	lk, err := state.AcquireLockFenced(ctx, l.exec, app)
	if err != nil {
		return err
	}
	lk.StartRenewal(l.exec)
	defer state.ReleaseLockFenced(l.exec, lk, app)

	containers, err := l.appContainers(ctx, app)
	if err != nil {
		return err
	}
	if len(containers) == 0 {
		return fmt.Errorf("no running containers found for %s", app)
	}

	for _, c := range containers {
		fmt.Fprintf(l.out, "Stopping %s...\n", c.Name)
		if err := l.stop(ctx, lk, c, timeout); err != nil {
			return err
		}
	}

	l.logAction(ctx, app, "stop")
	fmt.Fprintln(l.out, "Stopped")
	return nil
}

// Start starts all stopped containers for the app and runs a health check.
func (l *Lifecycle) Start(ctx context.Context, app string) error {
	lk, err := state.AcquireLockFenced(ctx, l.exec, app)
	if err != nil {
		return err
	}
	lk.StartRenewal(l.exec)
	defer state.ReleaseLockFenced(l.exec, lk, app)

	current, err := state.Read(ctx, l.exec, app)
	if err != nil || current == nil {
		return fmt.Errorf("no deploy state found for %s — deploy first", app)
	}

	containers, err := l.appContainersByVersion(ctx, app, current.CurrentHash)
	if err != nil {
		return err
	}
	if len(containers) == 0 {
		return fmt.Errorf("no containers found for %s version %s", app, current.CurrentHash)
	}

	for _, c := range containers {
		fmt.Fprintf(l.out, "Starting %s...\n", c.Name)
		if err := l.start(ctx, lk, c); err != nil {
			return err
		}
	}

	// Health check on web container.
	if current.CurrentPort > 0 {
		fmt.Fprintln(l.out, "Running health check...")
		deployer := &Deployer{exec: l.exec, out: l.out}
		// Probe the bound address, not localhost — see healthProbeHost.
		webName := ""
		for _, c := range containers {
			if c.Labels["teploy.process"] == "web" {
				webName = c.Name
				break
			}
		}
		if webName == "" {
			return fmt.Errorf("current release has no web container")
		}
		bindHost := l.docker.HostBindIP(ctx, webName)
		health := defaultHealthConfig()
		rec, recErr := releasemeta.Read(ctx, l.exec, app, current.CurrentHash)
		if recErr != nil {
			return recErr
		}
		if rec != nil {
			applyRecordToRollback(&RollbackConfig{}, rec, &health)
		}
		if err := deployer.healthCheck(ctx, current.CurrentPort, health, bindHost); err != nil {
			return fmt.Errorf("health check failed after start: %w", err)
		}
		fmt.Fprintln(l.out, "  Health check passed")
	}

	l.logAction(ctx, app, "start")
	fmt.Fprintln(l.out, "Started")
	return nil
}

// Restart stops then starts all containers for the app.
func (l *Lifecycle) Restart(ctx context.Context, app string, timeout int) error {
	lk, err := state.AcquireLockFenced(ctx, l.exec, app)
	if err != nil {
		return err
	}
	lk.StartRenewal(l.exec)
	defer state.ReleaseLockFenced(l.exec, lk, app)

	current, err := state.Read(ctx, l.exec, app)
	if err != nil || current == nil {
		return fmt.Errorf("no deploy state found for %s — deploy first", app)
	}

	containers, err := l.appContainersByVersion(ctx, app, current.CurrentHash)
	if err != nil {
		return err
	}
	if len(containers) == 0 {
		return fmt.Errorf("no containers found for %s version %s", app, current.CurrentHash)
	}

	// Stop all.
	for _, c := range containers {
		fmt.Fprintf(l.out, "Stopping %s...\n", c.Name)
		if err := l.stop(ctx, lk, c, timeout); err != nil {
			return err
		}
	}

	// Start all.
	for _, c := range containers {
		fmt.Fprintf(l.out, "Starting %s...\n", c.Name)
		if err := l.start(ctx, lk, c); err != nil {
			return err
		}
	}

	// Health check on web container.
	if current.CurrentPort > 0 {
		fmt.Fprintln(l.out, "Running health check...")
		deployer := &Deployer{exec: l.exec, out: l.out}
		// Probe the bound address, not localhost — see healthProbeHost.
		webName := ""
		for _, c := range containers {
			if c.Labels["teploy.process"] == "web" {
				webName = c.Name
				break
			}
		}
		if webName == "" {
			return fmt.Errorf("current release has no web container")
		}
		bindHost := l.docker.HostBindIP(ctx, webName)
		health := defaultHealthConfig()
		rec, recErr := releasemeta.Read(ctx, l.exec, app, current.CurrentHash)
		if recErr != nil {
			return recErr
		}
		if rec != nil {
			applyRecordToRollback(&RollbackConfig{}, rec, &health)
		}
		if err := deployer.healthCheck(ctx, current.CurrentPort, health, bindHost); err != nil {
			return fmt.Errorf("health check failed after restart: %w", err)
		}
		fmt.Fprintln(l.out, "  Health check passed")
	}

	l.logAction(ctx, app, "restart")
	fmt.Fprintln(l.out, "Restarted")
	return nil
}

// Only current recorded processes and currently configured accessories belong
// to lifecycle. Start activates configured accessories, including previously
// stopped ones; removed/unknown accessories and historical workloads stay alone.
func (l *Lifecycle) appContainers(ctx context.Context, app string) ([]docker.Container, error) {
	current, err := state.Read(ctx, l.exec, app)
	if err != nil {
		return nil, err
	}
	if current == nil {
		return nil, fmt.Errorf("no deploy state for %s", app)
	}
	containers, err := l.appContainersByVersion(ctx, app, current.CurrentHash)
	if err != nil {
		return nil, err
	}
	var running []docker.Container
	for _, container := range containers {
		if container.State == "running" {
			running = append(running, container)
		}
	}
	return running, nil
}
func (l *Lifecycle) appContainersByVersion(ctx context.Context, app, version string) ([]docker.Container, error) {
	current, err := state.Read(ctx, l.exec, app)
	if err != nil {
		return nil, err
	}
	if current == nil || current.CurrentHash != version {
		return nil, fmt.Errorf("current release changed")
	}
	record, err := releasemeta.Read(ctx, l.exec, app, version)
	if err != nil {
		return nil, err
	}
	if record == nil || record.App != app || record.Hash != version {
		return nil, fmt.Errorf("current release record required for lifecycle")
	}
	accessories := map[string]bool{}
	if len(current.AppliedManifest) > 0 {
		manifest, err := config.ParseAppliedManifest(current.AppliedManifest)
		if err != nil {
			return nil, err
		}
		for name := range manifest.Accessories {
			accessories[name] = true
		}
	}
	containers, err := l.docker.ListContainers(ctx, app)
	if err != nil {
		return nil, err
	}
	var matched []docker.Container
	for _, container := range containers {
		if currentLifecycleContainer(container, app, version, current.Generation, record.Generation, record.Processes, accessories) {
			matched = append(matched, container)
		}
	}
	sort.SliceStable(matched, func(i, j int) bool {
		return matched[i].Labels["teploy.role"] == "accessory" && matched[j].Labels["teploy.role"] != "accessory"
	})
	return matched, nil
}
func currentLifecycleContainer(container docker.Container, app, version string, generation, recordedGeneration uint64, processes map[string]string, accessories map[string]bool) bool {
	labels := container.Labels
	if container.ID == "" || labels["teploy.app"] != app {
		return false
	}
	if labels["teploy.preview"] != "" {
		return false
	}
	role := labels["teploy.role"]
	if role == "accessory" {
		name := labels["teploy.accessory"]
		return name != "" && accessories[name] && container.Name == app+"-"+name && labels["teploy.process"] == ""
	}
	if role != "" && role != "app" {
		return false
	}
	process := labels["teploy.process"]
	if labels["teploy.version"] != version {
		return false
	}
	if process != "web" {
		if _, recorded := processes[process]; !recorded || process == "" {
			return false
		}
	}
	if label := labels["teploy.generation"]; label != "" && label != strconv.FormatUint(generation, 10) && label != strconv.FormatUint(recordedGeneration, 10) {
		return false
	}
	name := app + "-" + process + "-" + version
	if container.Name == name {
		return true
	}
	suffix, ok := strings.CutPrefix(container.Name, name+"-")
	replica, err := strconv.Atoi(suffix)
	return ok && err == nil && replica > 0 && strconv.Itoa(replica) == suffix
}

func (l *Lifecycle) logAction(ctx context.Context, app, action string) {
	state.AppendLog(ctx, l.exec, state.LogEntry{
		Timestamp: time.Now().UTC(),
		App:       app,
		Type:      action,
		Success:   true,
	})
}

func (l *Lifecycle) stop(ctx context.Context, lk *state.Lock, c docker.Container, timeout int) error {
	ref := c.ID
	if ref == "" {
		return fmt.Errorf("immutable container ID required")
	}
	_, err := lk.Guarded(ctx, l.exec, fmt.Sprintf("docker stop --time %d %s", timeout, ssh.ShellQuote(ref)))
	return err
}
func (l *Lifecycle) start(ctx context.Context, lk *state.Lock, c docker.Container) error {
	ref := c.ID
	if ref == "" {
		return fmt.Errorf("immutable container ID required")
	}
	_, err := lk.Guarded(ctx, l.exec, "docker start "+ssh.ShellQuote(ref))
	return err
}
