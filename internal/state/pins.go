package state

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/useteploy/teploy/internal/ssh"
)

// Pins protect specific app versions from keep_versions auto-pruning. They
// live server-side at /deployments/<app>/pinned (one version per line), so —
// like every other piece of deploy state — they survive on the server, not in
// any local database, and every client (CLI, dash, autodeploy) sees the same
// set. A pinned version's containers and images are never removed by
// PruneVersions even when they fall outside the keep window.

func pinsPath(app string) string {
	return fmt.Sprintf("%s/%s/pinned", deploymentsDir, app)
}

// ReadPins returns the pinned versions for an app. Only a CONFIRMED-MISSING
// file yields (nil, nil); every other read failure is an error — the old
// version swallowed all failures, so a permission or transport problem
// silently removed pin protection from versions the operator deliberately
// retained (audit F78). Callers must fail CLOSED on error (skip pruning),
// never prune as though no pins existed.
func ReadPins(ctx context.Context, exec ssh.Executor, app string) ([]string, error) {
	data, present, err := readRemoteFile(ctx, exec, pinsPath(app))
	if err != nil {
		return nil, fmt.Errorf("reading pins for %s: %w", app, err)
	}
	if !present {
		return nil, nil
	}
	var pins []string
	for _, line := range strings.Split(string(data), "\n") {
		if v := strings.TrimSpace(line); v != "" {
			pins = append(pins, v)
		}
	}
	return pins, nil
}

// AddPin pins a version (idempotent).
func AddPin(ctx context.Context, exec ssh.Executor, app, version string) error {
	pins, err := ReadPins(ctx, exec, app)
	if err != nil {
		return err
	}
	for _, p := range pins {
		if p == version {
			return nil
		}
	}
	return writePins(ctx, exec, app, append(pins, version))
}

// RemovePin unpins a version (idempotent).
func RemovePin(ctx context.Context, exec ssh.Executor, app, version string) error {
	pins, err := ReadPins(ctx, exec, app)
	if err != nil {
		return err
	}
	kept := make([]string, 0, len(pins))
	for _, p := range pins {
		if p != version {
			kept = append(kept, p)
		}
	}
	return writePins(ctx, exec, app, kept)
}

// writePins commits the pin set atomically: a staged sibling file is renamed
// over the live one, so an interrupted write can no longer truncate the pin
// file and drop protection (audit F78).
func writePins(ctx context.Context, exec ssh.Executor, app string, pins []string) error {
	if err := EnsureAppDir(ctx, exec, app); err != nil {
		return err
	}
	sort.Strings(pins)
	content := strings.Join(pins, "\n")
	if content != "" {
		content += "\n"
	}
	if err := ssh.UploadAtomic(ctx, exec, strings.NewReader(content), pinsPath(app), "0600"); err != nil {
		return fmt.Errorf("writing pins: %w", err)
	}
	return nil
}
