// The replacement owner's reconciliation (programme workstream C01, slice
// C01-1): when a deploy's lock acquisition BREAKS a stale predecessor, the
// dead holder may have left in-flight effects on the target — a late
// `docker run` landing under version-keyed names, a route switch half-done,
// a displaced fixed-port workload. Lock acquisition is never proof of
// quiescence (docs/C01_RECOVERY_STATE_TABLE.md, finding C01-1); what the
// replacement owner OBSERVES, decided through the tested table, is the only
// safe input to "may I proceed with my own deploy?".
//
// Observe is the productionized evidence collector: the same reads the
// fixture harness performs (docker label inventory, state.json, the managed
// Caddyfile, the per-release record), with exact names and no guesses.
// ReconcileAfterTakeover drives the decision: RETRY is the only disposition
// that lets the deploy proceed; every other one refuses with the observed
// evidence so an operator reconciles deliberately. This is deliberately NOT
// an auto-compensator — COMPENSATE's automation (restart/restore via the
// recorded receipts) is the recovery-owner continuation work keyed to F04
// generation identities; refusing with evidence is the safe subset the
// table permits today.
package deploy

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/useteploy/teploy/internal/deploy/recovery"
	"github.com/useteploy/teploy/internal/docker"
	"github.com/useteploy/teploy/internal/releasemeta"
	"github.com/useteploy/teploy/internal/ssh"
	"github.com/useteploy/teploy/internal/state"
)

// managedCaddyfile is the shared proxy's config path — the route evidence
// class reads the managed marker blocks from it (internal/caddy owns the
// write side; recovery only reads).
const managedCaddyfile = "/deployments/caddy/Caddyfile"

// reconcileReobserveDelay is the bounded pause before re-observing an
// INSPECT disposition (R4's transient-read reconcile trigger). A variable
// so tests can shorten it.
var reconcileReobserveDelay = 2 * time.Second

// Observe collects a recovery.Observation about (app, attempted) versus its
// known predecessor release with exact names and receipts: the teploy-labeled
// container inventory, the authoritative state.json, the managed Caddyfile,
// and the per-release record. Read failures map to Unknown classes — the
// decision table's never-auto-decide evidence grade — never to guesses.
//
// attempted is the release the caller is about to deploy; predecessor is the
// release state.json is expected to name ("" when there is no state yet).
// Containers under any OTHER teploy.app naming are ForeignCandidates — the
// dead holder's late effects land exactly there.
func Observe(ctx context.Context, exec ssh.Executor, app, attempted, predecessor string) recovery.Observation {
	var o recovery.Observation

	containers, err := docker.NewClient(exec).ListContainers(ctx, app)
	if err != nil {
		o.Candidates, o.CandidateCorpses, o.ForeignCandidates = recovery.Unknown, recovery.Unknown, recovery.Unknown
	} else {
		candPrefix := fmt.Sprintf("%s-web-%s", app, attempted)
		predName := ""
		if predecessor != "" {
			predName = fmt.Sprintf("%s-web-%s", app, predecessor)
		}
		for _, c := range containers {
			isCandidate := strings.HasPrefix(c.Name, candPrefix)
			isPredecessor := predName != "" &&
				(strings.HasPrefix(c.Name, predName) || strings.HasPrefix(c.Name, predName+"_replaced"))
			switch {
			case isCandidate:
				if c.State == "running" {
					o.Candidates = recovery.Present
				} else {
					o.CandidateCorpses = recovery.Present
				}
			case isPredecessor:
				if strings.HasSuffix(c.Name, "_replaced") || c.State == "running" {
					o.PredecessorServing = recovery.Present
				} else {
					o.PredecessorStopped = recovery.Present
				}
			default:
				// Neither the attempted release's candidate naming nor the
				// known predecessor's: an unattributable workload — only
				// RUNNING ones consume traffic/jobs.
				if c.State == "running" {
					o.ForeignCandidates = recovery.Present
				}
			}
		}
	}

	st, err := state.Read(ctx, exec, app)
	switch {
	case err != nil:
		o.StateToCandidate, o.StateToPredecessor = recovery.Unknown, recovery.Unknown
	case st == nil:
		o.StateToCandidate, o.StateToPredecessor = recovery.Absent, recovery.Absent
	case st.CurrentHash == attempted:
		o.StateToCandidate = recovery.Present
	default:
		o.StateToPredecessor = recovery.Present
	}

	caddyfile, present, err := state.ReadRemoteFile(ctx, exec, managedCaddyfile)
	switch {
	case err != nil:
		o.RouteToCandidate, o.RouteToPredecessor = recovery.Unknown, recovery.Unknown
	case !present:
		o.RouteToCandidate, o.RouteToPredecessor = recovery.Absent, recovery.Absent
	case strings.Contains(string(caddyfile), fmt.Sprintf("%s-web-%s", app, attempted)):
		o.RouteToCandidate = recovery.Present
	default:
		o.RouteToPredecessor = recovery.Present
	}

	rec, err := releasemeta.Read(ctx, exec, app, attempted)
	switch {
	case err != nil:
		o.ReleaseRecord = recovery.Unknown
	case rec != nil:
		o.ReleaseRecord = recovery.Present
	default:
		o.ReleaseRecord = recovery.Absent
	}
	return o
}

// describeEvidence renders the observation as the operator-facing evidence
// block: every non-absent class on its own line, so a refusal names exactly
// what was seen on the target.
func describeEvidence(o recovery.Observation) string {
	type line struct {
		name string
		ev   recovery.Evidence
	}
	lines := []line{
		{"running workload under this deploy's candidate names", o.Candidates},
		{"stopped corpses under this deploy's candidate names", o.CandidateCorpses},
		{"running unattributable teploy-labeled workload (foreign names)", o.ForeignCandidates},
		{"route (Caddyfile) naming this deploy's candidates", o.RouteToCandidate},
		{"route (Caddyfile) naming the predecessor generation", o.RouteToPredecessor},
		{"state.json naming this deploy's release", o.StateToCandidate},
		{"state.json naming the predecessor release", o.StateToPredecessor},
		{"predecessor workload serving", o.PredecessorServing},
		{"predecessor workload stopped (restorable)", o.PredecessorStopped},
		{"release record for this deploy's version", o.ReleaseRecord},
	}
	var out []string
	for _, l := range lines {
		if l.ev != recovery.Absent {
			out = append(out, fmt.Sprintf("  %s: %s", l.name, l.ev))
		}
	}
	if len(out) == 0 {
		out = append(out, "  (no leftover effects observed)")
	}
	return strings.Join(out, "\n")
}

// ReconcileAfterTakeover runs the replacement owner's decision over the
// observed target (C01-1). It is called by DeployFenced exactly when this
// deploy's lock acquisition broke a stale predecessor's lock, BEFORE any of
// its own effects. The disposition comes from the tested table
// (recovery.Decide):
//
//   - RETRY: nothing contradicts proceeding (no state, or a consistent
//     serving predecessor and no leftover candidate work). The deploy
//     proceeds, with the observed world summarized for the operator.
//   - INSPECT: evidence is readable but does not match a clean pre-deploy
//     world (a running candidate without receipts, record/target
//     disagreement, both generations on the edge) or is unreadable. One
//     re-observation absorbs transient read failures; a persistent INSPECT
//     refuses the deploy with the evidence.
//   - COMPENSATE: traffic sits on an uncommitted generation, or the
//     predecessor is displaced — restorable, but the undo is the operator's
//     call (the auto-compensating recovery owner is F04-keyed future work).
//   - MANUAL: unattributable workloads or unreadable authority — never
//     auto-decided.
//
// A refusal is a plain error: no effects of this deploy have run yet (the
// reconcile precedes the first docker run), so there is nothing to undo.
func (d *Deployer) ReconcileAfterTakeover(ctx context.Context, cfg Config, current *state.AppState) error {
	predecessor := ""
	if current != nil {
		predecessor = current.CurrentHash
	}
	observe := func() recovery.Observation {
		return Observe(ctx, d.exec, cfg.App, cfg.Version, predecessor)
	}
	o := observe()
	disp := recovery.Decide(recovery.Admitted, o)
	if disp == recovery.Inspect {
		// R4's unreadable classes are reconcile triggers: a transient
		// inventory/parse failure deserves one re-observation (bounded),
		// not an immediate operator escalation.
		select {
		case <-ctx.Done():
		case <-time.After(reconcileReobserveDelay):
		}
		o = observe()
		disp = recovery.Decide(recovery.Admitted, o)
	}
	evidence := describeEvidence(o)

	switch disp {
	case recovery.Retry:
		fmt.Fprintf(d.out, "Stale lock taken over; reconciled the observed target (no leftover deploy effects) — proceeding\n")
		return nil
	case recovery.Compensate:
		return fmt.Errorf(
			"refusing to deploy %s after taking over its stale lock: the observed target needs COMPENSATION before a new deploy\n"+
				"(traffic on an uncommitted generation, or a displaced predecessor that is restorable).\n"+
				"Observed evidence:\n%s\n"+
				"Reconcile the app first (teploy status / teploy rollback --app %s, or inspect the containers named by: docker ps --filter label=teploy.app=%s)",
			cfg.App, evidence, cfg.App, cfg.App)
	case recovery.Inspect:
		return fmt.Errorf(
			"refusing to deploy %s after taking over its stale lock: the observed target does not match a clean pre-deploy world (INSPECT)\n"+
				"— a previous deploy's effects landed without receipts, or evidence is unreadable.\n"+
				"Observed evidence:\n%s\n"+
				"Inspect the app first (teploy status; docker ps --filter label=teploy.app=%s) and reconcile the leftover generation before retrying",
			cfg.App, evidence, cfg.App)
	default: // recovery.Manual
		return fmt.Errorf(
			"refusing to deploy %s after taking over its stale lock: the observed target contains evidence automation must not decide (MANUAL)\n"+
				"— unattributable running workload(s) or unreadable authority.\n"+
				"Observed evidence:\n%s\n"+
				"Inspect the app manually (docker ps --filter label=teploy.app=%s; cat /deployments/%s/state.json) before retrying",
			cfg.App, evidence, cfg.App, cfg.App)
	}
}
