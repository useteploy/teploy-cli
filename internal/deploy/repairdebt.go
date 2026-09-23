// The release-record repair-debt marker (programme workstream C01, finding
// C01-6): when a deploy's releasemeta record write fails AFTER the live
// commit, the deploy deliberately completes — a record failure must never
// roll back live traffic — but the degradation used to be invisible and
// unconverged: nothing retried the write and nothing surfaced the debt.
//
// The marker is a small JSON file in the app's state namespace
// (/deployments/<app>/repair-debt.json, atomic 0600) naming the app, the
// failed attempt, what failed, when, and how many repair attempts have
// failed since. It is the debt the crash-recovery table's transition 7
// promises to converge:
//
//   - the NEXT deploy of the app repairs it before its own work — the
//     record is rebuilt from the live containers (releasemeta.Backfill, the
//     same convergence rollback uses) and the marker is cleared on success;
//   - a repeated failure keeps the marker with an incremented attempt
//     count, so the debt can neither vanish nor reset silently;
//   - `teploy status` reports outstanding debt (internal/cli/status.go).
package deploy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/releasemeta"
	"github.com/useteploy/teploy/internal/ssh"
	"github.com/useteploy/teploy/internal/state"
)

// repairDebtSchemaVersion is the marker's schema version.
const repairDebtSchemaVersion = 1

// repairDebtFileName is the marker's name inside the app's state namespace.
const repairDebtFileName = "repair-debt.json"

// RepairDebt is the durable record of a release record that failed to
// persist after its deploy went live. Attempts counts write/repair attempts
// that have failed for the SAME release (1 at the first failure; every
// failed repair increments it).
type RepairDebt struct {
	SchemaVersion int       `json:"schema_version"`
	App           string    `json:"app"`
	Release       string    `json:"release"`
	Attempt       string    `json:"attempt,omitempty"`
	Reason        string    `json:"reason"`
	Attempts      int       `json:"attempts"`
	FirstFailedAt time.Time `json:"first_failed_at"`
	LastFailedAt  time.Time `json:"last_failed_at"`
}

// repairDebtPath is the marker's location. The app segment is grammar
// checked (A17) so it can never interpolate path metacharacters.
func repairDebtPath(app string) (string, error) {
	if err := config.ValidateName(app); err != nil {
		return "", fmt.Errorf("repair-debt marker requires a valid app: %w", err)
	}
	return "/deployments/" + app + "/" + repairDebtFileName, nil
}

// ReadRepairDebt loads the app's repair-debt marker. Confirmed absent
// returns (nil, nil) — absence is the quiet normal case; every other
// failure (transport, malformed JSON, wrong schema, identity mismatch) is
// an error, so callers surface the unhealable debt instead of guessing
// (T56 parity with the other identity-checked records).
func ReadRepairDebt(ctx context.Context, exec ssh.Executor, app string) (*RepairDebt, error) {
	path, err := repairDebtPath(app)
	if err != nil {
		return nil, err
	}
	data, present, err := state.ReadRemoteFile(ctx, exec, path)
	if err != nil {
		return nil, fmt.Errorf("reading repair debt for %s: %w", app, err)
	}
	if !present {
		return nil, nil
	}
	var debt RepairDebt
	if err := json.Unmarshal(data, &debt); err != nil {
		return nil, fmt.Errorf("parsing the repair-debt marker for %s at %s: %w", app, path, err)
	}
	if debt.SchemaVersion != repairDebtSchemaVersion {
		return nil, fmt.Errorf("unsupported repair-debt schema version %d for %s", debt.SchemaVersion, app)
	}
	if debt.App != app {
		return nil, fmt.Errorf("repair-debt identity mismatch: requested %s, marker describes %s — refusing to use it", app, debt.App)
	}
	return &debt, nil
}

// writeRepairDebt persists the marker atomically (sibling temp + rename,
// 0600 — the same discipline every state-namespace writer uses).
func (d *Deployer) writeRepairDebt(ctx context.Context, debt *RepairDebt) error {
	path, err := repairDebtPath(debt.App)
	if err != nil {
		return err
	}
	data, err := json.Marshal(debt)
	if err != nil {
		return fmt.Errorf("marshaling the repair-debt marker: %w", err)
	}
	return ssh.UploadAtomic(ctx, d.exec, bytes.NewReader(data), path, "0600")
}

// clearRepairDebt removes the marker after a successful repair.
func (d *Deployer) clearRepairDebt(ctx context.Context, app string) error {
	path, err := repairDebtPath(app)
	if err != nil {
		return err
	}
	if _, err := d.exec.Run(ctx, "rm -f -- "+ssh.ShellQuote(path)); err != nil {
		return fmt.Errorf("clearing the repair-debt marker for %s: %w", app, err)
	}
	return nil
}

// recordRepairDebt persists the initial debt when a deploy's record write
// fails (deploy.go recordRelease). A marker for the SAME release bumps its
// attempt count and keeps FirstFailedAt — the debt's history survives
// repeated failing deploys of one release; a marker for a DIFFERENT release
// is replaced (the newest unresolved debt is the one that matters). A
// persistence failure here is warned loudly: without the marker, the next
// deploy will not know to repair anything.
func (d *Deployer) recordRepairDebt(ctx context.Context, att releasemeta.Attempt, writeErr error) {
	now := time.Now().UTC()
	debt := &RepairDebt{
		SchemaVersion: repairDebtSchemaVersion,
		App:           att.App,
		Release:       att.Hash,
		Attempt:       att.Name(),
		Reason:        writeErr.Error(),
		Attempts:      1,
		FirstFailedAt: now,
		LastFailedAt:  now,
	}
	if prev, err := ReadRepairDebt(ctx, d.exec, att.App); err == nil && prev != nil && prev.Release == att.Hash {
		debt.Attempts = prev.Attempts + 1
		debt.FirstFailedAt = prev.FirstFailedAt
	}
	if err := d.writeRepairDebt(ctx, debt); err != nil {
		path, _ := repairDebtPath(att.App)
		fmt.Fprintf(d.out, "Warning: could not persist the repair-debt marker for %s@%s (%v) — the missing release record will NOT self-heal; resolve it manually at %s\n", att.App, att.Hash, err, path)
	}
}

// repairOutstandingRecordDebt is the C01-6 reconciler, run by the next
// deploy of the app BEFORE its own work: if a repair-debt marker exists,
// rebuild the failed record and clear the marker; on failure keep the
// marker with an incremented attempt count. Never a deploy failure — the
// debt is a degraded condition of the PREVIOUS deploy, and refusing to
// deploy because of it would trade a live fix for a bookkeeping gap.
func (d *Deployer) repairOutstandingRecordDebt(ctx context.Context, app string, current *state.AppState) {
	debt, err := ReadRepairDebt(ctx, d.exec, app)
	if err != nil {
		path, _ := repairDebtPath(app)
		fmt.Fprintf(d.out, "Warning: a repair-debt marker exists for %s but cannot be read (%v) — the release record it names will not self-heal; inspect or remove %s\n", app, err, path)
		return
	}
	if debt == nil {
		return
	}

	// Already converged (a rollback or recreate backfilled it in the
	// meantime): the debt is paid, only the marker is stale.
	if rec, rerr := releasemeta.Read(ctx, d.exec, app, debt.Release); rerr == nil && rec != nil {
		if cerr := d.clearRepairDebt(ctx, app); cerr != nil {
			fmt.Fprintf(d.out, "Warning: the repair debt for %s@%s is already resolved, but clearing the marker failed: %v\n", app, debt.Release, cerr)
			return
		}
		fmt.Fprintf(d.out, "Repair debt cleared: the release record for %s@%s already exists (converged by another operation)\n", app, debt.Release)
		return
	}

	// Re-run the record write via the live containers — the same
	// convergence rollback uses (releasemeta.Backfill).
	var repairErr error
	switch {
	case current == nil:
		repairErr = fmt.Errorf("no app state to rebuild the record from")
	default:
		inv, ierr := d.docker.ListContainers(ctx, app)
		if ierr != nil {
			repairErr = fmt.Errorf("listing containers: %w", ierr)
			break
		}
		if _, berr := releasemeta.Backfill(ctx, d.exec, d.docker, inv, app, debt.Release, current); berr != nil {
			repairErr = berr
			break
		}
		if cerr := d.clearRepairDebt(ctx, app); cerr != nil {
			fmt.Fprintf(d.out, "Warning: repaired the release record for %s@%s, but clearing the repair-debt marker failed: %v\n", app, debt.Release, cerr)
			return
		}
		fmt.Fprintf(d.out, "Repaired: release record for %s@%s rebuilt from the live containers — repair debt cleared\n", app, debt.Release)
		return
	}

	// The repair failed: the debt stays, visibly, with the count bumped.
	debt.Attempts++
	debt.LastFailedAt = time.Now().UTC()
	if werr := d.writeRepairDebt(ctx, debt); werr != nil {
		fmt.Fprintf(d.out, "Warning: could not update the repair-debt marker for %s@%s: %v\n", app, debt.Release, werr)
	}
	fmt.Fprintf(d.out, "Warning: repair debt remains for %s@%s — repair attempt %d failed: %v; the next deploy will retry\n", app, debt.Release, debt.Attempts, repairErr)
}
