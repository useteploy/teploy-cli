package deploy

import (
	"context"
	"fmt"
	"io"
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
	containers, err := l.appContainers(ctx, app)
	if err != nil {
		return err
	}
	if len(containers) == 0 {
		return fmt.Errorf("no running containers found for %s", app)
	}

	for _, c := range containers {
		fmt.Fprintf(l.out, "Stopping %s...\n", c.Name)
		if err := l.docker.Stop(ctx, c.Name, timeout); err != nil {
			return err
		}
	}

	l.logAction(ctx, app, "stop")
	fmt.Fprintln(l.out, "Stopped")
	return nil
}

// Start starts all stopped containers for the app and runs a health check.
func (l *Lifecycle) Start(ctx context.Context, app string) error {
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
		if err := l.docker.Start(ctx, c.Name); err != nil {
			return err
		}
	}

	// Health check on web container.
	if current.CurrentPort > 0 {
		fmt.Fprintln(l.out, "Running health check...")
		deployer := &Deployer{exec: l.exec, out: l.out}
		if err := deployer.healthCheck(ctx, current.CurrentPort, defaultHealthConfig()); err != nil {
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
		if err := l.docker.Stop(ctx, c.Name, timeout); err != nil {
			return err
		}
	}

	// Start all.
	for _, c := range containers {
		fmt.Fprintf(l.out, "Starting %s...\n", c.Name)
		if err := l.docker.Start(ctx, c.Name); err != nil {
			return err
		}
	}

	// Health check on web container.
	if current.CurrentPort > 0 {
		fmt.Fprintln(l.out, "Running health check...")
		deployer := &Deployer{exec: l.exec, out: l.out}
		if err := deployer.healthCheck(ctx, current.CurrentPort, defaultHealthConfig()); err != nil {
			return fmt.Errorf("health check failed after restart: %w", err)
		}
		fmt.Fprintln(l.out, "  Health check passed")
	}

	l.logAction(ctx, app, "restart")
	fmt.Fprintln(l.out, "Restarted")
	return nil
}

// appContainers returns all running containers for the app.
func (l *Lifecycle) appContainers(ctx context.Context, app string) ([]docker.Container, error) {
	containers, err := l.docker.ListContainers(ctx, app)
	if err != nil {
		return nil, err
	}

	var running []docker.Container
	for _, c := range containers {
		if c.State == "running" {
			running = append(running, c)
		}
	}
	return running, nil
}

// appContainersByVersion returns all containers (any state) matching the given version.
func (l *Lifecycle) appContainersByVersion(ctx context.Context, app, version string) ([]docker.Container, error) {
	containers, err := l.docker.ListContainers(ctx, app)
	if err != nil {
		return nil, err
	}

	// Match by the teploy.version label, not a name suffix — replica containers
	// ("<app>-<proc>-<version>-1") don't end in "-<version>", so a suffix match
	// silently skips them (restart/start would miss every web replica).
	var matched []docker.Container
	for _, c := range containers {
		if c.Labels["teploy.version"] == version {
			matched = append(matched, c)
		}
	}
	return matched, nil
}

func (l *Lifecycle) logAction(ctx context.Context, app, action string) {
	state.AppendLog(ctx, l.exec, state.LogEntry{
		Timestamp: time.Now().UTC(),
		App:       app,
		Type:      action,
		Success:   true,
	})
}
