package state

import (
	"context"
	"encoding/base64"
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

// ReadPins returns the pinned versions for an app (nil if none/unset).
func ReadPins(ctx context.Context, exec ssh.Executor, app string) ([]string, error) {
	out, err := exec.Run(ctx, fmt.Sprintf("cat %s 2>/dev/null", pinsPath(app)))
	if err != nil {
		// Missing file is the normal "no pins" case, not an error.
		return nil, nil
	}
	var pins []string
	for _, line := range strings.Split(out, "\n") {
		if v := strings.TrimSpace(line); v != "" {
			pins = append(pins, v)
		}
	}
	return pins, nil
}

// AddPin pins a version (idempotent).
func AddPin(ctx context.Context, exec ssh.Executor, app, version string) error {
	pins, _ := ReadPins(ctx, exec, app)
	for _, p := range pins {
		if p == version {
			return nil
		}
	}
	return writePins(ctx, exec, app, append(pins, version))
}

// RemovePin unpins a version (idempotent).
func RemovePin(ctx context.Context, exec ssh.Executor, app, version string) error {
	pins, _ := ReadPins(ctx, exec, app)
	kept := make([]string, 0, len(pins))
	for _, p := range pins {
		if p != version {
			kept = append(kept, p)
		}
	}
	return writePins(ctx, exec, app, kept)
}

func writePins(ctx context.Context, exec ssh.Executor, app string, pins []string) error {
	if err := EnsureAppDir(ctx, exec, app); err != nil {
		return err
	}
	sort.Strings(pins)
	content := strings.Join(pins, "\n")
	if content != "" {
		content += "\n"
	}
	// base64 round-trip keeps the write shell-safe and lets an empty set
	// truncate the file cleanly (no pins left => empty file).
	encoded := base64.StdEncoding.EncodeToString([]byte(content))
	cmd := fmt.Sprintf("printf %%s %s | base64 -d > %s", ssh.ShellQuote(encoded), pinsPath(app))
	if _, err := exec.Run(ctx, cmd); err != nil {
		return fmt.Errorf("writing pins: %w", err)
	}
	return nil
}
