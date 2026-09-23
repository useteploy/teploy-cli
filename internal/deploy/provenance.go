// Plan/receipt equality surfaces (programme workstream C04).
//
// The contract: "image digest and effective configuration shown in plan
// equal the deployed receipt". Three pieces live here:
//
//   - plannedImageDigest — the ONE rule both sides of the equality use to
//     name an image's immutable identity (a digest-pinned reference by its
//     manifest digest, everything else by docker's resolved content ID),
//     so plan and receipt can never disagree by construction;
//   - printDeployPlan / printDeployReceipt — the pre-execution plan and
//     the post-commit receipt, both showing image DIGEST (not just the
//     tag), source revision, and the effective-config (manifest) digest;
//   - verifyPlanReceiptEquality — the closing verification after the live
//     commit: record digest == plan digest, equality reported explicitly,
//     a mismatch a loud warning plus repair debt (the C01-6 marker the
//     next deploy reconciles). Never a failed live deploy.
package deploy

import (
	"context"
	"fmt"
	"io"

	"github.com/useteploy/teploy/internal/releasemeta"
)

// plannedImageDigest resolves the immutable image identity the plan
// commits to. Priority: the provenance record's digest (resolved by the
// CLI BEFORE execution — the plan's own promise), then the reference's
// own digest (digest-pinned refs), then the deployer's just-resolved
// content ID. Empty when no immutable identity could be established —
// callers treat that as "nothing to verify", not as a digest.
func plannedImageDigest(ref string, resolvedID string, prov *releasemeta.Provenance) string {
	if prov != nil && prov.ImageDigest != "" {
		return prov.ImageDigest
	}
	if digest := ImageDigestFromRef(ref); digest != "" {
		return digest
	}
	if resolvedID != "" && resolvedID != ref {
		return resolvedID
	}
	return ""
}

// printDeployPlan surfaces the deploy's immutable identity BEFORE any
// effect: image digest (not just the tag), source revision — flagging a
// dirty worktree as "building uncommitted changes" — build context
// fingerprint, Dockerfile identity, platform, and the effective-config
// digest. Fields the caller could not resolve print as unavailable rather
// than silently disappearing.
func printDeployPlan(out io.Writer, cfg Config, planDigest string) {
	fmt.Fprintln(out, "Plan:")
	digest := planDigest
	if digest == "" {
		digest = "(unresolved)"
	}
	pinned := ""
	if cfg.Provenance != nil && cfg.Provenance.DigestPinned {
		pinned = ", digest-pinned"
	} else if cfg.Provenance != nil {
		pinned = ", mutable tag"
	}
	fmt.Fprintf(out, "  Image:     %s (%s%s)\n", cfg.Image, digest, pinned)
	revision := cfg.SourceRevision
	if revision == "" && cfg.Provenance != nil {
		revision = cfg.Provenance.Revision
	}
	if revision != "" {
		note := ""
		if cfg.Provenance != nil && cfg.Provenance.Dirty {
			note = " — building uncommitted changes"
		}
		fmt.Fprintf(out, "  Revision:  %s%s\n", revision, note)
	}
	if cfg.Provenance != nil {
		if cfg.Provenance.ContextPath != "" || cfg.Provenance.ContextFingerprint != "" {
			fingerprint := cfg.Provenance.ContextFingerprint
			if fingerprint == "" {
				fingerprint = "(unavailable)"
			}
			fmt.Fprintf(out, "  Context:   %s (fingerprint %s)\n", cfg.Provenance.ContextPath, fingerprint)
		}
		if cfg.Provenance.Dockerfile != "" {
			fmt.Fprintf(out, "  Dockerfile: %s (sha256 %s)\n", cfg.Provenance.Dockerfile, cfg.Provenance.DockerfileSHA256)
		}
		if cfg.Provenance.Platform != "" {
			fmt.Fprintf(out, "  Platform:  %s\n", cfg.Provenance.Platform)
		}
	}
	if cfg.ManifestSHA256 != "" {
		fmt.Fprintf(out, "  Config:    manifest %s\n", cfg.ManifestSHA256)
	}
}

// printDeployReceipt prints the post-commit receipt's identity triple —
// image digest, revision, effective-config digest — the same values the
// plan showed, now sourced from the deployed record.
func printDeployReceipt(out io.Writer, cfg Config, rec *releasemeta.Record) {
	digest := ""
	if rec != nil {
		digest = rec.ImageDigest
	}
	if digest == "" && cfg.Provenance != nil {
		digest = cfg.Provenance.ImageDigest
	}
	if digest == "" {
		digest = "(unrecorded)"
	}
	revision := cfg.SourceRevision
	if revision == "" && cfg.Provenance != nil {
		revision = cfg.Provenance.Revision
	}
	if revision == "" {
		revision = "(none)"
	}
	manifest := cfg.ManifestSHA256
	if manifest == "" && cfg.Provenance != nil {
		manifest = cfg.Provenance.ManifestSHA256
	}
	if manifest == "" {
		manifest = "(none)"
	}
	fmt.Fprintf(out, "Receipt: image %s, revision %s, config manifest %s\n", digest, revision, manifest)
}

// verifyPlanReceiptEquality is the closing verification (C04): after the
// live commit, the deployed record's image digest must equal the digest
// the plan showed. Equality is reported explicitly; a mismatch is a loud
// warning plus repair debt via the C01-6 marker (the next deploy's
// reconciler rebuilds the record from the live containers — the deployed
// truth) — live traffic is never failed over the gap. A plan with no
// resolvable digest verifies nothing (legacy direct construction; the
// resolution failure already warned when it happened); a receipt with no
// digest is an honest "could not verify", never a silent pass.
func (d *Deployer) verifyPlanReceiptEquality(ctx context.Context, cfg Config, rec *releasemeta.Record, att releasemeta.Attempt, planDigest string) {
	if planDigest == "" {
		return
	}
	if rec == nil || rec.ImageDigest == "" {
		fmt.Fprintf(d.out, "Warning: plan/receipt equality NOT verified — the deployed record carries no image digest to compare against the plan's %s\n", planDigest)
		return
	}
	if rec.ImageDigest != planDigest {
		fmt.Fprintf(d.out, "WARNING: plan/receipt mismatch — the plan showed image %s but the deployed release records %s: the live image differs from what was planned (changed mutable tag or concurrent re-pointing). Repair debt recorded; the next deploy reconciles the record. Live traffic is untouched.\n", planDigest, rec.ImageDigest)
		d.recordRepairDebt(ctx, att, fmt.Errorf("plan/receipt image digest mismatch: planned %s for %s, deployed %s", planDigest, cfg.Image, rec.ImageDigest))
		return
	}
	fmt.Fprintf(d.out, "Verified: the deployed image digest equals the plan (%s)\n", planDigest)
}
