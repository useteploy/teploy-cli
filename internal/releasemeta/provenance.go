// Deploy provenance (programme workstream C04): the plan-time record of
// WHAT a deploy attempt resolved to build and run — the git revision and
// worktree cleanliness at plan time, the build context identity, the
// Dockerfile identity, the target platform, the requested image reference
// and the immutable digest it resolved to BEFORE execution, whether that
// reference was digest-pinned or mutable, and the effective-config
// (manifest) digest.
//
// Persistence follows the attempt-journal discipline: provenance.json is
// written into the F08 attempt namespace
// (/deployments/<app>/meta/att/<hash>.<id>/provenance.json) — write-once
// per attempt, atomic 0600 — so a response-loss retry lands BESIDE the
// first attempt's receipt (never over it), and the F14 record embeds the
// same struct at commit so the deployed receipt carries what the plan
// promised. Reads are identity-validated (T56 parity): a receipt that
// describes another attempt/app is refused, never guessed from.
package releasemeta

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/useteploy/teploy/internal/ssh"
	"github.com/useteploy/teploy/internal/state"
)

// ProvenanceSchemaVersion is the provenance record's schema version.
const ProvenanceSchemaVersion = 1

// provenanceFile is the receipt's name in the attempt namespace.
const provenanceFile = "provenance.json"

// Provenance is the plan-time provenance record of one deploy attempt.
// Field semantics:
//
//   - Revision: the full HEAD sha the source resolved to at plan time.
//   - Dirty: the worktree had uncommitted changes — the build baked bytes
//     no revision names.
//   - ContextPath / ContextFingerprint: the configured build context
//     (relative; "." when unset) and the sha256 fingerprint of the tree
//     that was synced into the attempt's build context (build-package
//     encoding, excludes applied).
//   - Dockerfile / DockerfileSHA256: the Dockerfile identity — path plus
//     content hash. Empty for Nixpacks and prebuilt-image deploys.
//   - Platform: the target platform the build targets ("" = the daemon's
//     default).
//   - ImageRef: the image reference as requested (prebuilt ref, or the
//     built tag).
//   - ImageDigest: the immutable identity resolved BEFORE execution — the
//     reference's own digest when digest-pinned, else docker's resolved
//     content ID. Empty when resolution was impossible.
//   - DigestPinned: the requested reference was digest-pinned (immutable)
//     rather than a mutable tag — the fact the changed-mutable-tag story
//     keys on.
//   - ManifestSHA256: the effective-config digest (config.
//     NormalizeAndDigest) — plan and receipt compare THIS too.
//   - PlanID: when the deploy executed a reviewed plan (C05
//     `teploy apply`), the plan record's id — the tie-back from the
//     receipt to the plan that was verified. Empty for direct deploys.
type Provenance struct {
	SchemaVersion int       `json:"schema_version"`
	App           string    `json:"app"`
	Release       string    `json:"release"`
	Attempt       string    `json:"attempt,omitempty"`
	WrittenAt     time.Time `json:"written_at,omitempty"`

	Revision           string `json:"revision,omitempty"`
	Dirty              bool   `json:"dirty,omitempty"`
	ContextPath        string `json:"context_path,omitempty"`
	ContextFingerprint string `json:"context_fingerprint,omitempty"`
	Dockerfile         string `json:"dockerfile,omitempty"`
	DockerfileSHA256   string `json:"dockerfile_sha256,omitempty"`
	Platform           string `json:"platform,omitempty"`
	Local              bool   `json:"local,omitempty"`

	ImageRef       string `json:"image_ref,omitempty"`
	ImageDigest    string `json:"image_digest,omitempty"`
	DigestPinned   bool   `json:"digest_pinned,omitempty"`
	ManifestSHA256 string `json:"manifest_sha256,omitempty"`
	PlanID         string `json:"plan_id,omitempty"`
}

// AttemptProvenancePath is the receipt's location in the attempt namespace.
func AttemptProvenancePath(att Attempt) string {
	return att.Dir() + "/" + provenanceFile
}

// WriteAttemptProvenance persists the receipt atomically into the attempt's
// immutable namespace (0600, sibling temp + rename). The record's identity
// must describe the attempt it is written for; the attempt name and write
// time are stamped here so the caller's struct stays a pure resolution
// result (retry-stable: nothing time- or attempt-dependent enters before
// this point).
func WriteAttemptProvenance(ctx context.Context, exec ssh.Executor, att Attempt, prov *Provenance) error {
	if prov == nil {
		return fmt.Errorf("provenance is required")
	}
	if prov.App != att.App || prov.Release != att.Hash {
		return fmt.Errorf("provenance identity mismatch: record describes %s@%s, attempt is %s@%s — refusing to file it under the wrong attempt", prov.App, prov.Release, att.App, att.Hash)
	}
	if prov.SchemaVersion == 0 {
		prov.SchemaVersion = ProvenanceSchemaVersion
	}
	if prov.SchemaVersion != ProvenanceSchemaVersion {
		return fmt.Errorf("cannot write provenance schema version %d", prov.SchemaVersion)
	}
	prov.Attempt = att.Name()
	prov.WrittenAt = time.Now().UTC()

	data, err := json.Marshal(prov)
	if err != nil {
		return fmt.Errorf("marshaling provenance: %w", err)
	}
	if _, err := exec.Run(ctx, "mkdir -p "+att.Dir()); err != nil {
		return fmt.Errorf("creating the attempt directory: %w", err)
	}
	return ssh.UploadAtomic(ctx, exec, bytes.NewReader(data), AttemptProvenancePath(att), "0600")
}

// ReadAttemptProvenance loads an attempt's provenance receipt. A confirmed
// missing file returns (nil, nil); every other failure (transport,
// malformed JSON, wrong schema, identity mismatch) is an error — callers
// surface the unreadable provenance instead of guessing from it.
func ReadAttemptProvenance(ctx context.Context, exec ssh.Executor, att Attempt) (*Provenance, error) {
	data, present, err := state.ReadRemoteFile(ctx, exec, AttemptProvenancePath(att))
	if err != nil {
		return nil, fmt.Errorf("reading provenance for %s: %w", att.Name(), err)
	}
	if !present {
		return nil, nil
	}
	var prov Provenance
	if err := json.Unmarshal(data, &prov); err != nil {
		return nil, fmt.Errorf("parsing provenance for %s: %w", att.Name(), err)
	}
	if prov.SchemaVersion != ProvenanceSchemaVersion {
		return nil, fmt.Errorf("unsupported provenance schema version %d for %s", prov.SchemaVersion, att.Name())
	}
	if prov.App != att.App || prov.Attempt != att.Name() {
		return nil, fmt.Errorf("provenance identity mismatch: requested %s@%s, receipt describes %s@%s — refusing to use it", att.App, att.Name(), prov.App, prov.Attempt)
	}
	return &prov, nil
}
