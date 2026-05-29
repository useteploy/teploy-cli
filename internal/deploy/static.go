// Package deploy: static deploy path.
//
// Static deploys skip Docker entirely. The flow is:
//   1. Optionally run user-supplied build commands (locally by default).
//   2. Compute a content hash of the source directory.
//   3. rsync the source to /deployments/<app>/releases/<hash>.tmp/ and atomically
//      rename to releases/<hash>/. Skipped if a release with this hash already exists.
//   4. Atomically flip /deployments/<app>/current → releases/<hash>.
//   5. Upsert the Caddyfile block via internal/caddy + reload Caddy.
//   6. Write per-app state (current_hash, previous_hash) and append a log entry.
//   7. Prune retained releases past KeepReleases.
//
// Rollback is the inverse: read state, flip the symlink to previous_hash,
// re-upsert the Caddyfile block (in case headers/cache changed), reload.

package deploy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/useteploy/teploy/internal/caddy"
	"github.com/useteploy/teploy/internal/ssh"
	"github.com/useteploy/teploy/internal/state"
)

// StaticConfig is the input to a static deploy. Mirrors the fields on
// config.AppConfig that matter for static and translates them into the form
// the deployer needs (resolved hosts, container-side root path, etc.).
type StaticConfig struct {
	App          string
	Domain       string // raw "a.com, www.a.com" — caddy.SetStaticRoute splits it
	Source       string // local path to the directory to upload (the built dist/)
	Build        []string
	BuildRemote  bool
	SPA          bool
	SPAFallback  string
	Cache        map[string]string
	Headers      map[string]string
	KeepReleases int    // 0 = use default
	CaddyExtra   string

	// Server-side mount path (the directory Caddy is configured to serve from).
	// For the OVH box this is /srv/static/<app>/current — see DefaultStaticMount.
	MountBase string // default: /srv/static
	StateDir  string // default: /deployments
}

// DefaultKeepReleases is applied when StaticConfig.KeepReleases is 0.
const DefaultKeepReleases = 5

// DefaultStaticMount is the path inside the Caddy container where static
// content is served from. The host-side equivalent at the OVH box is
// /deployments/static/<app>/, bind-mounted as /srv/static/<app>/.
const DefaultStaticMount = "/srv/static"

// DefaultStateDir is where per-app deploy state and release directories live
// on the target server.
const DefaultStateDir = "/deployments"

// StaticDeployer handles type:static deploys. Constructed once per CLI run.
type StaticDeployer struct {
	exec  ssh.Executor
	caddy *caddy.Client
	out   io.Writer
}

// NewStaticDeployer wires the SSH executor + caddy client + output stream.
func NewStaticDeployer(exec ssh.Executor, out io.Writer) *StaticDeployer {
	return &StaticDeployer{
		exec:  exec,
		caddy: caddy.NewClient(exec),
		out:   out,
	}
}

// Deploy executes a single static deploy end-to-end. Returns nil on success,
// an error otherwise; callers should treat returned errors as deploy failure.
func (d *StaticDeployer) Deploy(ctx context.Context, cfg StaticConfig) error {
	if cfg.MountBase == "" {
		cfg.MountBase = DefaultStaticMount
	}
	if cfg.StateDir == "" {
		cfg.StateDir = DefaultStateDir
	}
	if cfg.KeepReleases == 0 {
		cfg.KeepReleases = DefaultKeepReleases
	}
	if cfg.Source == "" {
		return errors.New("static deploy: source is required")
	}

	start := time.Now()
	fmt.Fprintf(d.out, "Deploying %s (static)\n", cfg.App)

	// 1. Build (locally for now; remote build is a deferred feature).
	if len(cfg.Build) > 0 {
		if cfg.BuildRemote {
			return errors.New("static deploy: build_remote is not yet supported — run build locally")
		}
		if err := d.runLocalBuild(ctx, cfg); err != nil {
			return fmt.Errorf("build failed: %w", err)
		}
	}

	// 2. Verify the source directory now exists and is non-empty.
	srcAbs, err := filepath.Abs(cfg.Source)
	if err != nil {
		return fmt.Errorf("resolving source path: %w", err)
	}
	if info, err := os.Stat(srcAbs); err != nil || !info.IsDir() {
		return fmt.Errorf("source %q is not a directory (did your build step run?)", cfg.Source)
	}

	// 3. Hash the source so identical content reuses the same release dir.
	hash, err := hashDir(srcAbs)
	if err != nil {
		return fmt.Errorf("hashing source: %w", err)
	}
	shortHash := hash[:12]
	fmt.Fprintf(d.out, "  release %s\n", shortHash)

	// 4. Lock the app to avoid concurrent deploys racing on the symlink swap.
	if err := state.EnsureAppDir(ctx, d.exec, cfg.App); err != nil {
		return fmt.Errorf("ensure app dir: %w", err)
	}
	if err := state.AcquireLock(ctx, d.exec, cfg.App); err != nil {
		return fmt.Errorf("acquire lock: %w", err)
	}
	defer state.ReleaseLock(ctx, d.exec, cfg.App)

	// 5. Read prior state for rollback bookkeeping.
	prior, _ := state.Read(ctx, d.exec, cfg.App)

	// 6. Ensure the releases dir exists, then rsync to a temp release dir and
	//    atomically rename. If the release for this hash already exists we
	//    skip the upload (idempotent re-deploy).
	releasesDir := fmt.Sprintf("%s/%s/releases", cfg.StateDir, cfg.App)
	currentLink := fmt.Sprintf("%s/%s/current", cfg.StateDir, cfg.App)
	finalRelease := fmt.Sprintf("%s/%s", releasesDir, shortHash)
	tmpRelease := finalRelease + ".tmp"

	if _, err := d.exec.Run(ctx, "mkdir -p "+releasesDir); err != nil {
		return fmt.Errorf("mkdir releases: %w", err)
	}

	exists, _ := d.exec.Run(ctx, fmt.Sprintf("test -d %s && echo yes || true", finalRelease))
	if strings.TrimSpace(exists) == "yes" {
		fmt.Fprintf(d.out, "  release %s already on server, skipping upload\n", shortHash)
	} else {
		// Clean any stale temp dir from a prior interrupted run.
		_, _ = d.exec.Run(ctx, "rm -rf "+tmpRelease)
		if _, err := d.exec.Run(ctx, "mkdir -p "+tmpRelease); err != nil {
			return fmt.Errorf("mkdir tmp release: %w", err)
		}
		if err := d.rsyncTo(ctx, srcAbs, tmpRelease); err != nil {
			return fmt.Errorf("rsync: %w", err)
		}
		if _, err := d.exec.Run(ctx, fmt.Sprintf("mv %s %s", tmpRelease, finalRelease)); err != nil {
			return fmt.Errorf("rename release: %w", err)
		}
		fmt.Fprintf(d.out, "  uploaded release %s\n", shortHash)
	}

	// 7. Atomically flip the `current` symlink. Use ln -sfn so an existing
	//    symlink is replaced atomically.
	if _, err := d.exec.Run(ctx, fmt.Sprintf("ln -sfn releases/%s %s", shortHash, currentLink)); err != nil {
		return fmt.Errorf("symlink swap: %w", err)
	}

	// 8. Upsert Caddyfile block. The container-side root is the mount path,
	//    not the host path — Caddy reads through the bind mount.
	if err := d.caddy.SetStaticRoute(ctx, cfg.App, cfg.Domain, caddy.StaticBlockOpts{
		Root:        fmt.Sprintf("%s/%s/current", cfg.MountBase, cfg.App),
		SPA:         cfg.SPA,
		SPAFallback: cfg.SPAFallback,
		Cache:       cfg.Cache,
		Headers:     cfg.Headers,
		CaddyExtra:  cfg.CaddyExtra,
	}); err != nil {
		return fmt.Errorf("caddy route: %w", err)
	}
	// SetStaticRoute writes the Caddyfile block and reloads Caddy itself.

	// 10. Write new app state. Reuse the container state file format so the
	//     same `state.AppState` works for both deploy types — only the hash
	//     fields are populated for static.
	newState := &state.AppState{
		CurrentHash:  shortHash,
		PreviousHash: priorHashOrEmpty(prior, shortHash),
	}
	if err := state.Write(ctx, d.exec, cfg.App, newState); err != nil {
		return fmt.Errorf("write state: %w", err)
	}

	// 11. Prune old releases (keep the most recent KeepReleases including
	//     current). Best-effort; failure here doesn't fail the deploy.
	if err := d.pruneReleases(ctx, releasesDir, shortHash, cfg.KeepReleases); err != nil {
		fmt.Fprintf(d.out, "  warning: prune releases: %v\n", err)
	}

	// 12. Append deploy log entry.
	state.AppendLog(ctx, d.exec, state.LogEntry{
		Timestamp:  time.Now().UTC(),
		App:        cfg.App,
		Type:       "deploy",
		Hash:       shortHash,
		Success:    true,
		DurationMs: time.Since(start).Milliseconds(),
	})

	fmt.Fprintf(d.out, "Deployed %s in %dms\n", cfg.App, time.Since(start).Milliseconds())
	return nil
}

// runLocalBuild runs cfg.Build commands sequentially in the user's shell, with
// stdout/stderr forwarded so errors are visible. Each command runs in the
// directory containing the source path (typical frontend project layout).
func (d *StaticDeployer) runLocalBuild(ctx context.Context, cfg StaticConfig) error {
	wd, err := filepath.Abs(filepath.Dir(cfg.Source))
	if err != nil {
		return err
	}
	for _, line := range cfg.Build {
		fmt.Fprintf(d.out, "  build: %s\n", line)
		cmd := exec.CommandContext(ctx, "sh", "-c", line)
		cmd.Dir = wd
		cmd.Stdout = d.out
		cmd.Stderr = d.out
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("%q: %w", line, err)
		}
	}
	return nil
}

// rsyncTo shells out to local rsync to upload srcDir to the remote dest path.
// Uses -az (archive + compress) and --delete so the destination matches the
// source exactly. The remote path must already exist.
func (d *StaticDeployer) rsyncTo(ctx context.Context, srcDir, remoteDest string) error {
	src := strings.TrimRight(srcDir, "/") + "/"
	target := fmt.Sprintf("%s@%s:%s/", d.exec.User(), d.exec.Host(), remoteDest)
	cmd := exec.CommandContext(ctx, "rsync",
		"-az", "--delete",
		"-e", "ssh -o BatchMode=yes -o StrictHostKeyChecking=accept-new",
		src, target,
	)
	cmd.Stdout = d.out
	cmd.Stderr = d.out
	return cmd.Run()
}

// pruneReleases keeps the `keep` most recent release directories under
// releasesDir, always preserving the currently-active hash regardless of mtime.
// "Most recent" is determined by directory mtime; a fresh deploy is always at
// the top because rsync just touched it.
func (d *StaticDeployer) pruneReleases(ctx context.Context, releasesDir, keepHash string, keep int) error {
	if keep <= 0 {
		return nil
	}
	out, err := d.exec.Run(ctx, fmt.Sprintf("ls -1t %s 2>/dev/null", releasesDir))
	if err != nil {
		return err
	}
	var entries []string
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		if l != "" && !strings.HasSuffix(l, ".tmp") {
			entries = append(entries, l)
		}
	}
	if len(entries) <= keep {
		return nil
	}

	// Build the keep set: the N newest, plus the current hash unconditionally.
	keepSet := map[string]bool{keepHash: true}
	for i := 0; i < keep && i < len(entries); i++ {
		keepSet[entries[i]] = true
	}
	for _, e := range entries {
		if keepSet[e] {
			continue
		}
		_, _ = d.exec.Run(ctx, fmt.Sprintf("rm -rf %s/%s", releasesDir, e))
	}
	return nil
}

// hashDir returns the sha256 of the *contents* of dir — file paths and bytes
// — so deploys with identical output get identical hashes. Symlinks are
// followed; permission bits are intentionally ignored to keep the hash stable
// across umask differences between developer machines.
func hashDir(dir string) (string, error) {
	h := sha256.New()
	var paths []string
	err := filepath.WalkDir(dir, func(p string, dEnt fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if dEnt.IsDir() {
			return nil
		}
		paths = append(paths, p)
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(paths)
	for _, p := range paths {
		rel, _ := filepath.Rel(dir, p)
		h.Write([]byte(rel))
		h.Write([]byte{0})
		f, err := os.Open(p)
		if err != nil {
			return "", err
		}
		_, err = io.Copy(h, f)
		f.Close()
		if err != nil {
			return "", err
		}
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// priorHashOrEmpty returns the prior CurrentHash unless it equals the new hash
// (idempotent re-deploy), in which case keep PreviousHash unchanged.
func priorHashOrEmpty(prior *state.AppState, newHash string) string {
	if prior == nil {
		return ""
	}
	if prior.CurrentHash != "" && prior.CurrentHash != newHash {
		return prior.CurrentHash
	}
	return prior.PreviousHash
}

// StaticRollbackConfig is the input to RollbackStatic. Specify ToHash to roll
// back to a specific retained release; leave empty to roll back to the
// previous deploy recorded in state.
type StaticRollbackConfig struct {
	App       string
	Domain    string
	ToHash    string // optional: explicit release hash to roll back to
	MountBase string
	StateDir  string

	// Caddyfile-block options. Pass through whatever is in the current
	// teploy.yml; Caddyfile defaults stay sane if these are zero.
	SPA         bool
	SPAFallback string
	Cache       map[string]string
	Headers     map[string]string
	CaddyExtra  string
}

// RollbackStatic flips the `current` symlink back to a prior release and
// re-asserts the Caddyfile block (in case the user changed headers/cache
// between deploys). Returns an error if no eligible release exists.
func (d *StaticDeployer) Rollback(ctx context.Context, cfg StaticRollbackConfig) error {
	if cfg.MountBase == "" {
		cfg.MountBase = DefaultStaticMount
	}
	if cfg.StateDir == "" {
		cfg.StateDir = DefaultStateDir
	}

	if err := state.AcquireLock(ctx, d.exec, cfg.App); err != nil {
		return fmt.Errorf("acquire lock: %w", err)
	}
	defer state.ReleaseLock(ctx, d.exec, cfg.App)

	prior, _ := state.Read(ctx, d.exec, cfg.App)
	if prior == nil {
		return errors.New("no state found — has this app been deployed?")
	}

	target := cfg.ToHash
	if target == "" {
		target = prior.PreviousHash
	}
	if target == "" {
		return errors.New("no previous deploy to roll back to")
	}
	if target == prior.CurrentHash {
		return fmt.Errorf("target hash %s is already current", target)
	}

	releasesDir := fmt.Sprintf("%s/%s/releases", cfg.StateDir, cfg.App)
	currentLink := fmt.Sprintf("%s/%s/current", cfg.StateDir, cfg.App)

	// Verify the target release still exists on disk.
	if out, _ := d.exec.Run(ctx, fmt.Sprintf("test -d %s/%s && echo yes || true", releasesDir, target)); strings.TrimSpace(out) != "yes" {
		return fmt.Errorf("release %s no longer on server (may have been pruned)", target)
	}

	if _, err := d.exec.Run(ctx, fmt.Sprintf("ln -sfn releases/%s %s", target, currentLink)); err != nil {
		return fmt.Errorf("symlink swap: %w", err)
	}

	// Re-assert Caddyfile block so any header/cache changes in the rolled-
	// back-from version don't carry over.
	if err := d.caddy.SetStaticRoute(ctx, cfg.App, cfg.Domain, caddy.StaticBlockOpts{
		Root:        fmt.Sprintf("%s/%s/current", cfg.MountBase, cfg.App),
		SPA:         cfg.SPA,
		SPAFallback: cfg.SPAFallback,
		Cache:       cfg.Cache,
		Headers:     cfg.Headers,
		CaddyExtra:  cfg.CaddyExtra,
	}); err != nil {
		return fmt.Errorf("caddy route: %w", err)
	}
	// SetStaticRoute writes the Caddyfile block and reloads Caddy itself.

	// Swap state: previous becomes current, current becomes previous.
	newState := &state.AppState{
		CurrentHash:  target,
		PreviousHash: prior.CurrentHash,
	}
	if err := state.Write(ctx, d.exec, cfg.App, newState); err != nil {
		return fmt.Errorf("write state: %w", err)
	}
	state.AppendLog(ctx, d.exec, state.LogEntry{
		Timestamp: time.Now().UTC(),
		App:       cfg.App,
		Type:      "rollback",
		Hash:      target,
		Success:   true,
	})
	fmt.Fprintf(d.out, "Rolled back %s to %s\n", cfg.App, target)
	return nil
}

// ListReleases returns the retained releases on disk for an app, newest
// first. Used by `teploy releases <app>`.
func (d *StaticDeployer) ListReleases(ctx context.Context, app, stateDir string) ([]string, error) {
	if stateDir == "" {
		stateDir = DefaultStateDir
	}
	out, err := d.exec.Run(ctx, fmt.Sprintf("ls -1t %s/%s/releases 2>/dev/null", stateDir, app))
	if err != nil {
		return nil, err
	}
	var releases []string
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		if l != "" && !strings.HasSuffix(l, ".tmp") {
			releases = append(releases, l)
		}
	}
	return releases, nil
}
