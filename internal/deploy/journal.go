// The deploy attempt journal (programme workstream C01): small durable
// receipts persisted into the F08 attempt namespace
// (/deployments/<app>/meta/att/<hash>.<id>/) so that a crash anywhere in
// the deploy lifecycle leaves the NEXT process — a recovery owner, an
// operator, a rollback — with the evidence the crash-recovery state table
// (internal/deploy/recovery, docs/C01_RECOVERY_STATE_TABLE.md) reasons
// over. The namespace is write-once per attempt (random id, immutable),
// which is exactly the durability contract these receipts need: nothing
// rewrites a landed receipt, and a fresh attempt can never collide with
// one.
//
// Receipts in this file:
//
//   - predecessors.json (C01-10): the exact predecessor workload the
//     attempt must retire or compensate, snapshotted at the rename phase
//     before any new container starts.
//   - readiness.json (C01-4): the health gate's receipt, written the
//     moment readiness passes and before the traffic switch begins.
//
// Every receipt carries the writing attempt's identity (app, release,
// attempt name) and is validated against the requested key on read (T56
// parity: a copied or corrupted-but-valid receipt must not drive effects
// at a different attempt's world).
package deploy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/useteploy/teploy/internal/deploy/recovery"
	"github.com/useteploy/teploy/internal/docker"
	"github.com/useteploy/teploy/internal/releasemeta"
	"github.com/useteploy/teploy/internal/ssh"
	"github.com/useteploy/teploy/internal/state"
)

// journalSchemaVersion is the schema of the attempt-journal receipts.
const journalSchemaVersion = 1

// predecessorSnapshotFile is the durable predecessor snapshot's name in
// the attempt namespace.
const predecessorSnapshotFile = "predecessors.json"

// readinessReceiptFile is the durable readiness receipt's name in the
// attempt namespace (C01-4).
const readinessReceiptFile = "readiness.json"

// predecessorSnapshot is the durable form of the in-memory predecessor
// snapshot deploy takes after the same-version renames and before any new
// container starts (C01-10). Release is the predecessor's releasemeta
// identity (the release whose record describes these containers);
// Containers are the EXACT identities docker reported — retirement and
// compensation address these names/IDs, never a re-derivation.
type predecessorSnapshot struct {
	SchemaVersion int                    `json:"schema_version"`
	App           string                 `json:"app"`
	Release       string                 `json:"release"`
	Attempt       string                 `json:"attempt"`
	SameVersion   bool                   `json:"same_version,omitempty"`
	Containers    []predecessorContainer `json:"containers"`
	WrittenAt     time.Time              `json:"written_at"`
}

// predecessorContainer mirrors docker.Container's identity fields.
type predecessorContainer struct {
	ID     string            `json:"id"`
	Name   string            `json:"name"`
	Image  string            `json:"image,omitempty"`
	State  string            `json:"state,omitempty"`
	Labels map[string]string `json:"labels,omitempty"`
}

// predecessorSnapshotPath is the snapshot's location in the attempt
// namespace.
func predecessorSnapshotPath(att releasemeta.Attempt) string {
	return att.Dir() + "/" + predecessorSnapshotFile
}

// persistPredecessorSnapshot writes the snapshot atomically into the
// attempt's immutable namespace (0600, sibling temp + rename — the same
// discipline releasemeta.Write uses).
func (d *Deployer) persistPredecessorSnapshot(ctx context.Context, att releasemeta.Attempt, release string, sameVersion bool, containers []docker.Container) error {
	snap := predecessorSnapshot{
		SchemaVersion: journalSchemaVersion,
		App:           att.App,
		Release:       release,
		Attempt:       att.Name(),
		SameVersion:   sameVersion,
		WrittenAt:     time.Now().UTC(),
	}
	for _, c := range containers {
		snap.Containers = append(snap.Containers, predecessorContainer{
			ID:     c.ID,
			Name:   c.Name,
			Image:  c.Image,
			State:  c.State,
			Labels: c.Labels,
		})
	}
	data, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("marshaling the predecessor snapshot: %w", err)
	}
	// The path segments are grammar-validated (app via releasemeta's
	// ValidateName, hash via validHash, random hex id), so — like
	// releasemeta.Write's own meta mkdir — the path needs no quoting.
	if _, err := d.exec.Run(ctx, "mkdir -p "+att.Dir()); err != nil {
		return fmt.Errorf("creating the attempt directory: %w", err)
	}
	return ssh.UploadAtomic(ctx, d.exec, bytes.NewReader(data), predecessorSnapshotPath(att), "0600")
}

// readPredecessorSnapshot loads the attempt's predecessor snapshot. A
// confirmed-missing file returns (nil, nil); every other failure
// (transport, malformed JSON, wrong schema, identity mismatch) is an
// error — recovery must not guess on unreadable compensation knowledge.
func readPredecessorSnapshot(ctx context.Context, exec ssh.Executor, att releasemeta.Attempt) (*predecessorSnapshot, error) {
	data, present, err := state.ReadRemoteFile(ctx, exec, predecessorSnapshotPath(att))
	if err != nil {
		return nil, fmt.Errorf("reading the predecessor snapshot for %s: %w", att.Name(), err)
	}
	if !present {
		return nil, nil
	}
	var snap predecessorSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, fmt.Errorf("parsing the predecessor snapshot for %s: %w", att.Name(), err)
	}
	if snap.SchemaVersion != journalSchemaVersion {
		return nil, fmt.Errorf("unsupported predecessor-snapshot schema version %d for %s", snap.SchemaVersion, att.Name())
	}
	if snap.App != att.App || snap.Attempt != att.Name() {
		return nil, fmt.Errorf("predecessor snapshot identity mismatch: requested %s@%s, snapshot describes %s@%s — refusing to use it", att.App, att.Name(), snap.App, snap.Attempt)
	}
	return &snap, nil
}

// displacedFromSnapshot recovers the displaced fixed-port workload from
// the durable predecessor snapshot when the in-memory list is absent
// (C01-10's crash-recovery entry, shared by restoreDisplacedAndStarted
// and abortStateCommit): every snapshot web container that is no longer
// running was stopped by this attempt's displacement. A blue/green
// predecessor was never stopped (inspect still shows it running, as does
// a same-version rename under its _replaced name), so only the actually
// displaced containers come back — exactly the recorded identities, never
// a re-derivation from the live inventory (which a post-commit world
// cannot attribute, TCL-02) or from derived names (which cannot see
// removed workers, T63).
func (d *Deployer) displacedFromSnapshot(ctx context.Context, att releasemeta.Attempt) []string {
	snap, err := readPredecessorSnapshot(ctx, d.exec, att)
	if err != nil {
		fmt.Fprintf(d.out, "Warning: could not read the predecessor snapshot for displaced-workload recovery: %v\n", err)
		return nil
	}
	if snap == nil {
		return nil
	}
	var displaced []string
	for _, c := range snap.Containers {
		if c.Labels["teploy.process"] != "web" {
			continue // displacement stops only the fixed-port web workload
		}
		out, err := d.exec.Run(ctx, "docker inspect -f '{{.State.Status}}' "+ssh.ShellQuote(c.Name)+" 2>/dev/null || true")
		if err != nil || strings.TrimSpace(out) == "" {
			continue // gone or unreadable: nothing restorable under this name
		}
		if strings.TrimSpace(out) != "running" {
			displaced = append(displaced, c.Name)
		}
	}
	return displaced
}

// readinessReceipt is the health gate's durable receipt (C01-4):
// persisted into the attempt namespace the moment readiness passes and
// BEFORE the traffic switch begins, so a recovery owner reading the
// journal can distinguish "crashed during readiness probing" (no
// receipt; the CandidatesRunning window — INSPECT) from "readiness held
// for these exact containers, crash before/while switching traffic" (the
// ReadinessPassed window — the COMPENSATE class). Containers records the
// exact candidate identities docker run returned; Probes records what
// was probed (host/port/path per replica); Outcome is "passed" (the
// receipt is never written otherwise).
type readinessReceipt struct {
	SchemaVersion int                `json:"schema_version"`
	App           string             `json:"app"`
	Release       string             `json:"release"`
	Attempt       string             `json:"attempt"`
	Containers    []receiptCandidate `json:"containers"`
	Probes        []readinessProbe   `json:"probes"`
	Outcome       string             `json:"outcome"`
	ProbedAt      time.Time          `json:"probed_at"`
}

// receiptCandidate is one readiness-passed candidate container: its exact
// name and the ID docker run returned — the strongest attribution
// evidence available (names alone are version-keyed and shared by every
// attempt of the same release, register F04/A09).
type receiptCandidate struct {
	Name string `json:"name"`
	ID   string `json:"id,omitempty"`
}

// readinessProbe records one readiness probe the gate ran.
type readinessProbe struct {
	Container string `json:"container,omitempty"`
	Host      string `json:"host,omitempty"`
	Port      int    `json:"port"`
	Path      string `json:"path,omitempty"`
}

// readinessReceiptPath is the receipt's location in the attempt
// namespace.
func readinessReceiptPath(att releasemeta.Attempt) string {
	return att.Dir() + "/" + readinessReceiptFile
}

// persistReadinessReceipt writes the receipt atomically (0600, sibling
// temp + rename). Called exactly once per attempt, after the health gate
// passes and before the traffic switch begins.
func (d *Deployer) persistReadinessReceipt(ctx context.Context, att releasemeta.Attempt, release string, candidates []receiptCandidate, probes []readinessProbe) error {
	r := readinessReceipt{
		SchemaVersion: journalSchemaVersion,
		App:           att.App,
		Release:       release,
		Attempt:       att.Name(),
		Containers:    candidates,
		Probes:        probes,
		Outcome:       "passed",
		ProbedAt:      time.Now().UTC(),
	}
	data, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("marshaling the readiness receipt: %w", err)
	}
	if _, err := d.exec.Run(ctx, "mkdir -p "+att.Dir()); err != nil {
		return fmt.Errorf("creating the attempt directory: %w", err)
	}
	return ssh.UploadAtomic(ctx, d.exec, bytes.NewReader(data), readinessReceiptPath(att), "0600")
}

// readReadinessReceipt loads the attempt's readiness receipt. Confirmed
// absent returns (nil, nil) — that absence IS the evidence (the gate
// never passed); every other failure is an error.
func readReadinessReceipt(ctx context.Context, exec ssh.Executor, att releasemeta.Attempt) (*readinessReceipt, error) {
	data, present, err := state.ReadRemoteFile(ctx, exec, readinessReceiptPath(att))
	if err != nil {
		return nil, fmt.Errorf("reading the readiness receipt for %s: %w", att.Name(), err)
	}
	if !present {
		return nil, nil
	}
	var r readinessReceipt
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("parsing the readiness receipt for %s: %w", att.Name(), err)
	}
	if r.SchemaVersion != journalSchemaVersion {
		return nil, fmt.Errorf("unsupported readiness-receipt schema version %d for %s", r.SchemaVersion, att.Name())
	}
	if r.App != att.App || r.Attempt != att.Name() {
		return nil, fmt.Errorf("readiness receipt identity mismatch: requested %s@%s, receipt describes %s@%s — refusing to use it", att.App, att.Name(), r.App, r.Attempt)
	}
	return &r, nil
}

// attemptReadinessState derives the lifecycle state a recovery owner may
// assume about a crashed attempt from its readiness receipt (C01-4): the
// receipt is written exactly when the gate passes (before the traffic
// switch begins), so its confirmed presence proves
// recovery.ReadinessPassed; its confirmed absence collapses the state to
// recovery.CandidatesRunning — the ADR's "ReadinessPassed is unobservable
// post-crash" finding, un-collapsed for owners that read the journal.
// Decide's semantics are unchanged; this is the evidence producer its
// inputs always modeled.
func attemptReadinessState(receipt *readinessReceipt) recovery.State {
	if receipt != nil {
		return recovery.ReadinessPassed
	}
	return recovery.CandidatesRunning
}

// candidateAttribution derives the recovery.Candidates evidence class
// for a crashed ATTEMPT from its readiness receipt and the live
// inventory: is a running workload provably THIS attempt's candidate?
// Nothing candidate-shaped running is provably Absent. A running
// candidate-shaped container is PROVABLY the attempt's only when its
// identity matches the receipt (the ID docker run returned; the name
// alone only when no ID exists to compare) — Present. Without a receipt,
// or when the running containers provably belong to a different attempt
// of the same release (version-keyed names are shared by every attempt
// of one release, register F04/A09 — the release-scoped reading of the
// class is that finding's deferred identity work), attribution is
// Unknown — the never-auto-decide class (R4: INSPECT).
func candidateAttribution(receipt *readinessReceipt, inv []docker.Container, app, release string) recovery.Evidence {
	prefix := fmt.Sprintf("%s-web-%s", app, release)
	isRunningCandidate := func(c docker.Container) bool {
		return c.State == "running" && strings.HasPrefix(c.Name, prefix)
	}
	running := false
	for _, c := range inv {
		if isRunningCandidate(c) {
			running = true
			break
		}
	}
	if !running {
		return recovery.Absent
	}
	if receipt == nil {
		return recovery.Unknown
	}
	ids := map[string]bool{}
	names := map[string]bool{}
	for _, c := range receipt.Containers {
		if c.ID != "" {
			ids[c.ID] = true
		}
		names[c.Name] = true
	}
	for _, c := range inv {
		if !isRunningCandidate(c) {
			continue
		}
		switch {
		case ids[c.ID]:
			return recovery.Present
		case c.ID == "" && names[c.Name]:
			// No ID on either side to disagree: the name is all the
			// evidence that exists.
			return recovery.Present
		}
	}
	return recovery.Unknown
}
