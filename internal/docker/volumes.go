package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/useteploy/teploy/internal/ssh"
)

// VolumeMismatch describes a volume whose existing mount source on the
// running container doesn't match the teploy-expected host path. Returned
// by DetectVolumeMismatches so the caller can decide whether to abort or
// migrate before swapping traffic.
//
// The classic shape: an app was previously deployed using a Docker named
// volume (or a hand-rolled bind mount path), and the current teploy.yml
// resolves to /deployments/<app>/volumes/<name>. Without this check, teploy
// would create the new bind mount empty, swap traffic, and orphan the data.
type VolumeMismatch struct {
	ContainerPath  string // mount destination inside the container, e.g. "/data"
	ExistingSource string // host path of the running container's mount
	ExpectedSource string // host path teploy plans to mount
}

// DetectVolumeMismatches inspects the currently-running web container for
// the app and returns any expected volume whose destination is already
// mounted from a different source on the host. An empty result means the
// upcoming deploy will not orphan any data — it's safe to proceed.
//
// Returns (nil, nil) when no existing container is found (first deploy or
// the prior container was already removed).
//
// The expected map is keyed by host path (the teploy-resolved source) and
// valued by the container path — same shape as RunConfig.Volumes.
func (c *Client) DetectVolumeMismatches(ctx context.Context, app string, expected map[string]string) ([]VolumeMismatch, error) {
	if len(expected) == 0 {
		return nil, nil
	}

	cidCmd := fmt.Sprintf(
		"docker ps -aq --filter label=teploy.app=%s --filter label=teploy.process=web | head -n 1",
		shellEscape(app),
	)
	out, err := c.exec.Run(ctx, cidCmd)
	if err != nil {
		return nil, fmt.Errorf("looking up existing container: %w", err)
	}
	cid := strings.TrimSpace(out)
	if cid == "" {
		return nil, nil // first deploy
	}

	mountsCmd := fmt.Sprintf(
		`docker inspect %s --format='{{range .Mounts}}{{.Type}}|{{.Source}}|{{.Destination}}{{println}}{{end}}'`,
		cid,
	)
	mountsOut, err := c.exec.Run(ctx, mountsCmd)
	if err != nil {
		return nil, fmt.Errorf("inspecting container mounts: %w", err)
	}

	// Index existing mounts by their destination (container path).
	existingByDest := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(mountsOut), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "|", 3)
		if len(parts) != 3 {
			continue
		}
		// parts[0] = type (volume|bind|tmpfs); parts[1] = source; parts[2] = destination
		existingByDest[parts[2]] = parts[1]
	}

	var mismatches []VolumeMismatch
	for hostPath, containerPath := range expected {
		existingSource, ok := existingByDest[containerPath]
		if !ok {
			continue // existing container has nothing at this destination
		}
		if existingSource != hostPath {
			mismatches = append(mismatches, VolumeMismatch{
				ContainerPath:  containerPath,
				ExistingSource: existingSource,
				ExpectedSource: hostPath,
			})
		}
	}
	return mismatches, nil
}

// FormatMismatchError builds a multi-line error suitable for surfacing to
// the user when DetectVolumeMismatches returns non-empty results and the
// caller has chosen not to auto-migrate.
//
// The message lists every affected volume, includes the exact migration
// shell commands a user could run manually, and points at the
// --migrate-volumes flag for the automated path.
func FormatMismatchError(app string, mismatches []VolumeMismatch) error {
	var b strings.Builder
	fmt.Fprintf(&b, "deploy aborted: %d volume(s) for %s currently mount data from a different host path than teploy expects.\n", len(mismatches), app)
	fmt.Fprintln(&b, "Continuing would create empty bind mounts and orphan the data on traffic swap.")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "Mismatched volumes:")
	for _, m := range mismatches {
		fmt.Fprintf(&b, "  %s\n", m.ContainerPath)
		fmt.Fprintf(&b, "    currently mounted from:  %s\n", m.ExistingSource)
		fmt.Fprintf(&b, "    teploy expects:          %s\n", m.ExpectedSource)
	}
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "To proceed safely, either:")
	fmt.Fprintln(&b, "  (a) Re-run with --migrate-volumes to copy each existing source into the")
	fmt.Fprintln(&b, "      teploy-expected path before deploy. This stops the running container")
	fmt.Fprintln(&b, "      first and preserves file ownership via cp -a.")
	fmt.Fprintln(&b, "  (b) Migrate manually on the server, then re-run teploy deploy:")
	fmt.Fprintln(&b, "      Hold a manual app lock (teploy lock) and an exclusive administrator window first.")
	fmt.Fprintln(&b, "      Run this ONE checked script. A failed inventory/stop refuses every copy.")
	fmt.Fprintf(&b, "        sh -c %s\n", ssh.ShellQuote(manualMigrationScript(app, mismatches)))
	fmt.Fprintln(&b, "      Keep the lock until redeploy/recovery; use the printed immutable IDs to restart old writers if needed, then teploy unlock.")

	return fmt.Errorf("%s", b.String())
}

// MigrateVolumes performs the (a) flow above. For each mismatch it stops
// the running container (once, idempotent), copies data from the existing
// source into the teploy-expected path with cp -a (preserves ownership and
// timestamps), and leaves the container stopped so the deploy flow can
// take over.
//
// On any error the function returns immediately — the caller should treat
// a partial migration as a failure state and not proceed with the deploy.
func BeginVolumeMigration(ctx context.Context, exec ssh.Executor, app string, mismatches []VolumeMismatch, out io.Writer) (migration *VolumeMigration, retErr error) {
	if len(mismatches) == 0 {
		return nil, nil
	}

	cidCmd := fmt.Sprintf(
		"docker ps -q --filter label=teploy.app=%s",
		shellEscape(app),
	)
	cidOut, err := exec.Run(ctx, cidCmd)
	if err != nil {
		return nil, fmt.Errorf("looking up existing container: %w", err)
	}
	ids := strings.Fields(cidOut)
	cid := strings.Join(ids, " ")
	if cid == "" {
		// No running container to migrate from — strange, but means there's
		// nothing to copy. Treat as no-op rather than error.
		fmt.Fprintln(out, "  (no running container found — nothing to migrate)")
		return nil, nil
	}

	fmt.Fprintf(out, "Migrating %d volume(s) for %s...\n", len(mismatches), app)
	fmt.Fprintf(out, "  Stopping container %s before copy...\n", cid[:min(12, len(cid))])
	migration = &VolumeMigration{exec: exec, ids: ids}
	recoveryHandle := migration
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, recoveryHandle.Restore(ctx))
		}
	}()
	if _, err := exec.Run(ctx, fmt.Sprintf("docker stop %s", cid)); err != nil {
		return nil, fmt.Errorf("stopping container before migration: %w", err)
	}

	for _, m := range mismatches {
		fmt.Fprintf(out, "  %s -> %s\n", m.ExistingSource, m.ExpectedSource)
		mkdir := fmt.Sprintf("mkdir -p %s", shellEscape(m.ExpectedSource))
		if _, err := exec.Run(ctx, mkdir); err != nil {
			return nil, fmt.Errorf("creating destination %s: %w", m.ExpectedSource, err)
		}
		// cp -a preserves ownership/perms/times; using "/." copies contents
		// rather than the directory itself so the destination already-exists
		// case works correctly.
		copyCmd := fmt.Sprintf(
			"cp -a %s/. %s/",
			shellEscape(m.ExistingSource), shellEscape(m.ExpectedSource),
		)
		if _, err := exec.Run(ctx, copyCmd); err != nil {
			return nil, fmt.Errorf("copying %s -> %s: %w", m.ExistingSource, m.ExpectedSource, err)
		}
	}

	fmt.Fprintln(out, "  Migration complete. Proceeding with deploy.")
	return migration, nil
}

// shellEscape wraps a value in single quotes for safe shell embedding,
// escaping any embedded single quotes. App names are validated upstream
// but defense in depth — this function is also used on user-influenced
// host paths during migration.
func shellEscape(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

type VolumeMigration struct {
	exec      ssh.Executor
	ids       []string
	committed bool
}

func (m *VolumeMigration) Commit() {
	if m != nil {
		m.committed = true
	}
}
func (m *VolumeMigration) Restore(ctx context.Context) error {
	if m == nil || m.committed {
		return nil
	}
	recovery, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	var result error
	for _, id := range m.ids {
		if _, err := m.exec.Run(recovery, "docker start "+ssh.ShellQuote(id)); err != nil {
			result = errors.Join(result, err)
		}
	}
	return result
}
func MigrateVolumes(ctx context.Context, exec ssh.Executor, app string, mismatches []VolumeMismatch, out io.Writer) error {
	m, err := BeginVolumeMigration(ctx, exec, app, mismatches, out)
	if err != nil {
		return err
	}
	return m.Restore(ctx)
}

// manualMigrationScript inventories ALL app-labelled running writers, never
// the first web process. Every path is quoted and every stop is checked.
func manualMigrationScript(app string, mismatches []VolumeMismatch) string {
	var b strings.Builder
	fmt.Fprintf(&b, "set -eu\nids=$(docker ps --no-trunc -q --filter %s) || exit 1\nstopped=''\n", ssh.ShellQuote("label=teploy.app="+app))
	b.WriteString(`for id in $ids; do
 case $id in *[!0-9a-f]*|'') echo 'invalid writer ID' >&2; exit 1;; esac
 [ ${#id} -eq 64 ] || exit 1
 docker stop -- "$id" || { echo "stop failed; no copy performed; recovery IDs: $stopped $id" >&2; exit 1; }
 stopped="$stopped $id"
done
`)
	fmt.Fprintf(&b, `remaining=$(docker ps --no-trunc -q --filter %s) || exit 1
[ -z "$remaining" ] || { echo 'writers remain; no copy performed' >&2; exit 1; }
`, ssh.ShellQuote("label=teploy.app="+app))
	for _, m := range mismatches {
		fmt.Fprintf(&b, `mkdir -p -- %s && cp -a -- %s %s || { echo "copy failed; recovery IDs: $stopped" >&2; exit 1; }
`, ssh.ShellQuote(m.ExpectedSource), ssh.ShellQuote(m.ExistingSource+"/."), ssh.ShellQuote(m.ExpectedSource+"/"))
	}
	b.WriteString("echo \"copy complete; keep writers stopped until deploy; recovery IDs: $stopped\"\n")
	return b.String()
}
