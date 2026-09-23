package cli

// C05 plan/apply binding: the durable plan record `teploy plan --out`
// writes and `teploy apply` verifies against. The contract being pinned:
//
//   - a plan is computed against an effective-config digest (the C04
//     NormalizeAndDigest surface, evaluated with the image reference the
//     plan resolved — empty for a build, whose image does not exist yet),
//     a target identity (app + server), and the target's deployed-state
//     generation;
//   - apply re-derives every identity input and refuses, naming WHAT
//     drifted, when config, target version, build inputs, or target
//     state moved since the plan. A stale plan is never executed;
//   - what cannot be known at plan time is marked unresolved (build
//     image) rather than guessed, and the plan instead binds the build
//     INPUTS (context fingerprint + Dockerfile identity) that apply
//     re-verifies.
//
// The plan id is a pure function of the binding inputs (schema, app,
// server, user, destination, target version, config digest, build-input
// identities, state generation/hash). WrittenAt is stamped at save and
// deliberately outside the identity, so re-planning the same world
// yields the same id — the retry-stability rule provenance follows.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/useteploy/teploy/internal/state"
)

// PlanRecordSchemaVersion is the plan record's schema version.
const PlanRecordSchemaVersion = 1

// Image resolution classes: what the plan could and could not bind.
const (
	// imageResolvedByDigest: the reference is digest-pinned; the digest
	// is part of the reference itself, so the plan fully knows the bytes.
	imageResolvedByDigest = "resolved-by-digest"
	// imageResolvedByImageID: a mutable reference whose current content
	// ID docker resolved at plan time. The ID is recorded but NOT bound
	// (a mutable tag may legitimately move; the changed-mutable-tag
	// policy is C04's open tail) — the config digest binds the ref.
	imageResolvedByImageID = "resolved-by-image-id"
	// imageUnresolvedMutableTag: a mutable reference whose content could
	// not be resolved at plan time (registry unreachable, image absent).
	imageUnresolvedMutableTag = "unresolved-mutable-tag"
	// imageUnresolvedAwaitingBuild: no image yet — the deploy builds it.
	// The plan binds the build inputs instead (context fingerprint,
	// Dockerfile identity).
	imageUnresolvedAwaitingBuild = "unresolved-awaiting-build"
)

// PlanRecord is the durable artifact of one `teploy plan` run.
type PlanRecord struct {
	SchemaVersion int    `json:"schema_version"`
	PlanID        string `json:"plan_id"`
	App           string `json:"app"`
	Server        string `json:"server"`
	User          string `json:"user,omitempty"`
	// ServerName is the LOGICAL server (servers.yml key / teploy.yml
	// server field) the plan resolved, recorded so apply targets exactly
	// the server the plan connected to.
	ServerName string `json:"server_name,omitempty"`
	// Destination is the overlay the plan was computed with; apply must
	// resolve the same merged config, so a flipped overlay is drift.
	Destination string `json:"destination,omitempty"`

	TargetVersion string `json:"target_version"`
	VersionKnown  bool   `json:"version_known"`
	// VersionExplicit: the version came from --version (operator-pinned)
	// rather than derived (git hash / image tag). Explicit versions are
	// binding as-is; derived ones are re-derived at apply and compared.
	VersionExplicit bool `json:"version_explicit,omitempty"`

	// ConfigDigest is the effective-config digest
	// (config.NormalizeAndDigest) over the plan's app config with the
	// image reference the plan resolved — "" for a build deploy, whose
	// image does not exist at plan time. The DEPLOYED receipt carries
	// its own manifest digest computed with the built image; the plan id
	// (stamped into that receipt) is the tie between the two.
	ConfigDigest string `json:"config_digest"`

	Image       PlanImageIdentity `json:"image"`
	TargetState PlanTargetState   `json:"target_state"`
	Effects     PlanEffects       `json:"effects"`

	// Unresolved lists the data the plan could not know, in operator
	// terms (the KNOWN-vs-UNRESOLVED contract: effects above are known;
	// everything here is explicitly not).
	Unresolved []string `json:"unresolved,omitempty"`

	WrittenAt time.Time `json:"written_at,omitempty"`
}

// PlanImageIdentity is the plan's knowledge about the image to run.
type PlanImageIdentity struct {
	// Ref is the image reference the deploy would use ("" when building).
	Ref string `json:"ref,omitempty"`
	// NeedsBuild: no prebuilt image; the deploy builds from source.
	NeedsBuild bool `json:"needs_build,omitempty"`
	// Resolution is one of the image* classification constants.
	Resolution string `json:"resolution"`
	// Digest is the immutable identity when one was resolvable.
	Digest string `json:"digest,omitempty"`

	// Build-input identity (build deploys only) — what apply re-verifies.
	ContextPath        string `json:"context_path,omitempty"`
	ContextFingerprint string `json:"context_fingerprint,omitempty"`
	Dockerfile         string `json:"dockerfile,omitempty"`
	DockerfileSHA256   string `json:"dockerfile_sha256,omitempty"`
	Platform           string `json:"platform,omitempty"`
}

// PlanTargetState is the deployed-state identity the plan was computed
// against. Generation is the state counter every successful operation
// increments, so ANY deploy/rollback between plan and apply moves it.
type PlanTargetState struct {
	Deployed       bool   `json:"deployed"`
	Generation     uint64 `json:"generation"`
	CurrentHash    string `json:"current_hash,omitempty"`
	ManifestSHA256 string `json:"manifest_sha256,omitempty"`
}

// PlanEffects is the planned effect set, split by surface. Containers
// keep the pre-plan/apply shape (create/stop/unchanged); the field
// surfaces use add/remove/change with from/to.
type PlanEffects struct {
	Containers  []planChange `json:"containers"`
	Routing     []planEffect `json:"routing,omitempty"`
	Env         []planEffect `json:"env,omitempty"`
	Storage     []planEffect `json:"storage,omitempty"`
	Resources   []planEffect `json:"resources,omitempty"`
	Accessories []planEffect `json:"accessories,omitempty"`
}

// planEffect is one planned change to a config-driven surface.
type planEffect struct {
	Action string `json:"action"` // add | remove | change
	Name   string `json:"name"`
	From   string `json:"from,omitempty"`
	To     string `json:"to,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// computePlanID derives the plan id from the binding inputs. Pure: the
// same planned world always yields the same id.
func computePlanID(rec *PlanRecord) string {
	h := sha256.New()
	fmt.Fprintf(h, "teploy-plan-v%d\n", rec.SchemaVersion)
	fmt.Fprintf(h, "app=%s\n", rec.App)
	fmt.Fprintf(h, "server=%s\n", rec.Server)
	fmt.Fprintf(h, "user=%s\n", rec.User)
	fmt.Fprintf(h, "destination=%s\n", rec.Destination)
	fmt.Fprintf(h, "target_version=%s\n", rec.TargetVersion)
	fmt.Fprintf(h, "config_digest=%s\n", rec.ConfigDigest)
	fmt.Fprintf(h, "context_fingerprint=%s\n", rec.Image.ContextFingerprint)
	fmt.Fprintf(h, "dockerfile_sha256=%s\n", rec.Image.DockerfileSHA256)
	fmt.Fprintf(h, "generation=%d\n", rec.TargetState.Generation)
	fmt.Fprintf(h, "current_hash=%s\n", rec.TargetState.CurrentHash)
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// errPlanDrift marks a plan/apply binding refusal (X02 §2.3: classified
// conflict — the request is coherent, the world moved).
var errPlanDrift = errors.New("plan drift")

// planDriftError names which binding input moved and the remedy.
type planDriftError struct {
	Kind   string // "identity" | "config" | "target-version" | "build-inputs" | "target-state" | "unverifiable"
	Detail string
}

func (e *planDriftError) Error() string {
	return fmt.Sprintf("plan %s drifted: %s — re-plan (`teploy plan --out <file>`) and apply the fresh plan", e.Kind, e.Detail)
}
func (e *planDriftError) Is(target error) bool { return target == errPlanDrift }

func driftRefusal(kind, format string, args ...any) error {
	return &planDriftError{Kind: kind, Detail: fmt.Sprintf(format, args...)}
}

// planCurrentFacts is everything apply re-derives before executing.
type planCurrentFacts struct {
	App                string
	Server             string // resolved host
	Version            string // re-derived target version
	ConfigDigest       string
	ContextFingerprint string
	DockerfileSHA256   string
	State              *state.AppState
}

// verifyPlanBinding checks a plan record against the re-derived world.
// Every refusal names the drifted input; nil means the plan is still the
// reviewed truth and may execute.
func verifyPlanBinding(rec *PlanRecord, cur planCurrentFacts) error {
	if rec.App != cur.App {
		return driftRefusal("identity", "plan is for app %q, run is for %q", rec.App, cur.App)
	}
	if rec.Server != cur.Server {
		return driftRefusal("identity", "plan targets server %q, run resolved %q", rec.Server, cur.Server)
	}
	if rec.ConfigDigest != cur.ConfigDigest {
		return driftRefusal("config", "effective config digest is now %s, plan reviewed %s (config, overlay %q, or env-file references changed)", cur.ConfigDigest, rec.ConfigDigest, rec.Destination)
	}
	if !rec.VersionExplicit && rec.TargetVersion != cur.Version {
		return driftRefusal("target-version", "target version is now %s, plan reviewed %s", cur.Version, rec.TargetVersion)
	}
	if rec.Image.NeedsBuild {
		if rec.Image.ContextFingerprint != cur.ContextFingerprint {
			return driftRefusal("build-inputs", "build context fingerprint is now %s, plan reviewed %s (source changed without a version change)", cur.ContextFingerprint, rec.Image.ContextFingerprint)
		}
		if rec.Image.DockerfileSHA256 != cur.DockerfileSHA256 {
			return driftRefusal("build-inputs", "Dockerfile %s content is now sha256 %s, plan reviewed %s", rec.Image.Dockerfile, cur.DockerfileSHA256, rec.Image.DockerfileSHA256)
		}
	}
	// Target state: any successful operation bumps Generation, so this
	// one comparison catches every deploy/rollback/scale in between.
	switch {
	case rec.TargetState.Deployed && cur.State == nil:
		return driftRefusal("target-state", "app %s had generation %d deployed at plan time but has no state now (removed?)", rec.App, rec.TargetState.Generation)
	case !rec.TargetState.Deployed && cur.State != nil:
		return driftRefusal("target-state", "app %s was deployed since the plan (generation %d, hash %s)", rec.App, cur.State.Generation, cur.State.CurrentHash)
	case cur.State != nil && cur.State.Generation != rec.TargetState.Generation:
		return driftRefusal("target-state", "generation moved %d -> %d (a deploy or rollback happened in between)", rec.TargetState.Generation, cur.State.Generation)
	}
	return nil
}

// savePlanFile writes the record atomically (sibling temp + rename,
// 0600: a plan names a target and carries digests, not secrets, but a
// tampered plan must not be silently swapped into place either).
func savePlanFile(path string, rec *PlanRecord) error {
	rec.WrittenAt = time.Now().UTC()
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling plan: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating plan directory: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".plan-*")
	if err != nil {
		return fmt.Errorf("staging plan file: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return fmt.Errorf("writing plan file: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("restricting plan file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing plan file: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("publishing plan file: %w", err)
	}
	return nil
}

// loadPlanFile reads a plan record and refuses one that is not
// self-consistent: wrong schema version, or a plan id that does not
// recompute from the recorded identity (a tampered or hand-edited plan
// never reaches execution).
func loadPlanFile(path string) (*PlanRecord, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading plan file: %w", err)
	}
	var rec PlanRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("parsing plan file %s: %w", path, err)
	}
	if rec.SchemaVersion != PlanRecordSchemaVersion {
		return nil, fmt.Errorf("plan file %s has schema version %d, this binary writes %d — re-plan with the current binary", path, rec.SchemaVersion, PlanRecordSchemaVersion)
	}
	if rec.PlanID == "" {
		return nil, fmt.Errorf("plan file %s carries no plan id — refuse to apply an unidentified plan; re-plan", path)
	}
	if want := computePlanID(&rec); want != rec.PlanID {
		return nil, fmt.Errorf("plan file %s is not self-consistent: recorded id %s, identity recomputes to %s — the record was edited after planning; re-plan", path, rec.PlanID, want)
	}
	return &rec, nil
}
