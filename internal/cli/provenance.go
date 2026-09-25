package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/useteploy/teploy/internal/build"
	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/deploy"
	"github.com/useteploy/teploy/internal/docker"
	"github.com/useteploy/teploy/internal/releasemeta"
	"github.com/useteploy/teploy/internal/ssh"
)

// resolveDeployProvenance captures the plan-time provenance of a deploy
// (programme C04): the resolved source identity, build inputs, and the
// immutable image identity — all BEFORE execution. The result is a PURE
// function of (app config, source tree, image): a response-loss retry of
// the same request resolves byte-identically, which is what the retry-
// stability contract pins. Callers persist it via releasemeta.
// WriteAttemptProvenance into the attempt namespace, which stamps the
// attempt-scoped fields.
//
// sourceRoot is the directory the source syncs from (the operator's cwd
// for manual deploys, the fetched checkout for autodeploy); empty or
// needsBuild=false skips build provenance (a prebuilt-image deploy has no
// build inputs — its provenance is the requested ref plus the digest it
// resolved to). Best-effort by contract: a provenance field that cannot
// be resolved warns and stays empty — provenance must never fail a
// deploy.
func resolveDeployProvenance(ctx context.Context, exec ssh.Executor, out io.Writer, appCfg *config.AppConfig, sourceRoot, image, version, manifestSHA256 string, needsBuild bool) *releasemeta.Provenance {
	prov := &releasemeta.Provenance{
		App:            appCfg.App,
		Release:        version,
		Revision:       appCfg.SourceRevision,
		ImageRef:       image,
		DigestPinned:   isDigestPinned(image),
		ManifestSHA256: manifestSHA256,
	}

	// Like-for-like with the deployed record's digest: a digest-pinned
	// reference is identified by its own digest; everything else by
	// docker's resolved content ID.
	if digest := deploy.ImageDigestFromRef(image); digest != "" {
		prov.ImageDigest = digest
	} else if resolved, err := docker.NewClient(exec).ResolveImageID(ctx, image); err == nil {
		prov.ImageDigest = resolved
	} else {
		fmt.Fprintf(out, "Warning: could not resolve an immutable image ID for %s before execution — plan/receipt digest equality will not be verifiable (%v)\n", image, err)
	}

	if !needsBuild || sourceRoot == "" {
		return prov
	}

	// Build provenance. Dirty first: the flag the plan surfaces as
	// "building uncommitted changes".
	prov.Dirty = gitDirtyIn(sourceRoot)

	prov.ContextPath = appCfg.Context
	if prov.ContextPath == "" {
		prov.ContextPath = "."
	}
	contextDir := sourceRoot
	if appCfg.Context != "" && appCfg.Context != "." {
		contextDir = filepath.Join(sourceRoot, appCfg.Context)
	}
	// The fingerprint covers exactly the selection a server build uploads
	// (build.ResolveSource), restricted to the configured context.
	if src, err := build.ResolveSource(sourceRoot); err != nil {
		fmt.Fprintf(out, "Warning: could not resolve the build context for the fingerprint: %v\n", err)
	} else if fp, err := src.Fingerprint(appCfg.Context); err != nil {
		fmt.Fprintf(out, "Warning: could not fingerprint the build context: %v\n", err)
	} else {
		prov.ContextFingerprint = fp
	}

	// Dockerfile identity: path + content hash. A tree without one is a
	// Nixpacks build — both fields stay empty.
	dockerfile := appCfg.Dockerfile
	if dockerfile == "" {
		dockerfile = "Dockerfile"
	}
	if data, err := os.ReadFile(filepath.Join(contextDir, dockerfile)); err == nil {
		prov.Dockerfile = dockerfile
		prov.DockerfileSHA256 = sha256Hex(data)
	}

	// Platform: the configured target; local Dockerfile builds get the
	// shared local-build default so the record names what the build ran.
	prov.Platform = appCfg.Platform
	prov.Local = appCfg.BuildLocal
	if prov.Platform == "" && appCfg.BuildLocal && prov.Dockerfile != "" {
		prov.Platform = build.EffectiveLocalPlatform("")
	}
	return prov
}

// gitDirtyIn reports whether dir's worktree has uncommitted changes. Not a
// git repository (or git unavailable) is not dirty — it is nothing this
// deploy would build over a named revision.
func gitDirtyIn(dir string) bool {
	out, err := exec.Command("git", "-C", dir, "status", "--porcelain").Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) != ""
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
