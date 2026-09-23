// Package recovery encodes the crash-recovery state table for teploy's
// deploy lifecycle — programme workstream C01, per the implementation
// handoff's "Crash-recovery design obligations": before building the full
// helper/journal, the state transition table (admitted, prepared,
// candidates running, readiness passed, traffic switched, authoritative
// state committed, predecessor retired, terminal receipt persisted) must
// exist as TESTED CODE, with each transition carrying the durable evidence
// that proves it landed and a crash-window disposition: RETRY (safe to
// redo), INSPECT (must reconcile against the target before acting),
// COMPENSATE (undo via known predecessor state), or MANUAL (surface to the
// operator; never auto-decide).
//
// The disposition is a PURE decision function over (from-state,
// observed-target-evidence). It is deliberately evidence-driven: a
// replacement owner that acquired the lock after a stale break must not
// assume lock acquisition proves quiescence — the dead holder's Docker
// effects can still be landing (the integration harness under
// harness_integration_test.go demonstrates exactly that). What the
// replacement owner OBSERVES, cross-checked against what its own records
// say was reached, is the only safe input to a recovery decision.
//
// Evidence names align with what the fenced-lock / releasemeta / Caddy
// machinery persists today, with file:line citations in the ADR
// (docs/C01_RECOVERY_STATE_TABLE.md). The package imports none of the
// effectful packages: it is the decision table, not the reconciler — the
// C01 implementation slices drive effects from these decisions.
package recovery

// State is one lifecycle state of a deploy attempt. The order is the
// commit order of internal/deploy/deploy.go's DeployFenced; the lattice
// and its evidence citations live in Lattice() and the ADR.
type State uint8

const (
	// Admitted: the fenced app lock is held — the owner token names this
	// operation in /deployments/<app>/.lock/info (internal/state/lock.go:86,
	// internal/state/state.go:479).
	Admitted State = iota
	// Prepared: attempt artifacts are generated and the predecessor
	// snapshotted/displaced — /deployments/<app>/meta/att/<hash>.<id>/
	// (internal/releasemeta/attempt.go:107), renames/displacement
	// (internal/deploy/deploy.go:414-509).
	Prepared
	// CandidatesRunning: candidate containers started under exact names
	// {app}-{process}-{version}[-{index}] with teploy.* labels
	// (internal/docker/docker.go:82-117, internal/deploy/deploy.go:573-607).
	CandidatesRunning
	// ReadinessPassed: the health gate passed. The durable receipt is the
	// attempt-scoped readiness.json (internal/deploy/journal.go, C01-4),
	// written exactly on pass and before the traffic switch; the evidence
	// derivation that turns it into this state and Candidates attribution
	// lives alongside it (attemptReadinessState / candidateAttribution).
	ReadinessPassed
	// TrafficSwitched: the edge routes name the candidates — the managed
	// marker block in /deployments/caddy/Caddyfile plus reload + delivery
	// verification receipts (internal/caddy/caddy.go:20-48, 584-625,
	// 523-550). Host ingress: the candidate holds the fixed port; external
	// ingress: no edge step at all.
	TrafficSwitched
	// AuthoritativeStateCommitted: state.json names the release — the
	// fenced rename (internal/state/lock.go:353-381), AppState.Generation
	// bumped (internal/state/state.go:68-95).
	AuthoritativeStateCommitted
	// PredecessorRetired: the snapshotted predecessor workload is
	// stopped/removed (internal/deploy/deploy.go:839-861, 963-989).
	PredecessorRetired
	// TerminalReceiptPersisted: the per-release record
	// /deployments/<app>/meta/<hash>.json (internal/releasemeta/
	// releasemeta.go:216-248) and the /deployments/teploy.log entry
	// (internal/state/state.go:662-679) exist.
	TerminalReceiptPersisted
)

// AllStates lists the lifecycle states in commit order.
func AllStates() []State {
	return []State{
		Admitted, Prepared, CandidatesRunning, ReadinessPassed,
		TrafficSwitched, AuthoritativeStateCommitted,
		PredecessorRetired, TerminalReceiptPersisted,
	}
}

func (s State) String() string {
	switch s {
	case Admitted:
		return "admitted"
	case Prepared:
		return "prepared"
	case CandidatesRunning:
		return "candidates-running"
	case ReadinessPassed:
		return "readiness-passed"
	case TrafficSwitched:
		return "traffic-switched"
	case AuthoritativeStateCommitted:
		return "authoritative-state-committed"
	case PredecessorRetired:
		return "predecessor-retired"
	case TerminalReceiptPersisted:
		return "terminal-receipt-persisted"
	default:
		return "unknown-state"
	}
}

// Disposition is the crash-window disposition of a recovery decision.
type Disposition uint8

const (
	// Retry: safe to redo. The transition's effects are idempotent, never
	// landed, or the remaining tail converges (record/backfill semantics).
	Retry Disposition = iota
	// Inspect: must reconcile against the target before acting. Evidence
	// is readable but does not match any single crash point (or is
	// unreadable-but-retryable, like a transient inventory failure) — the
	// owner re-observes and re-decides; it never treats a side effect as
	// a committed success.
	Inspect
	// Compensate: undo via known predecessor state. The predecessor is
	// running or restorable (stopped/displaced), so the partial effect can
	// be rolled back to a recorded, serving generation.
	Compensate
	// Manual: surface to the operator; never auto-decide. Unattributable
	// containers, unreadable authoritative bookkeeping, or a dark app with
	// no compensable predecessor.
	Manual
)

func (d Disposition) String() string {
	switch d {
	case Retry:
		return "RETRY"
	case Inspect:
		return "INSPECT"
	case Compensate:
		return "COMPENSATE"
	case Manual:
		return "MANUAL"
	default:
		return "unknown-disposition"
	}
}

// Evidence is the tri-state of one observation class. Absent and Present
// are PROVEN states of the target (confirmed by docker inspect / file
// read), never guesses: names and receipts are exact, per the handoff.
// Unknown is unreadable, unverifiable, or conflicting.
type Evidence uint8

const (
	Absent  Evidence = iota // provably not present
	Present                 // provably present
	Unknown                 // unreadable / unverifiable / conflicting
)

func (e Evidence) String() string {
	switch e {
	case Absent:
		return "absent"
	case Present:
		return "present"
	default:
		return "unknown"
	}
}

// Observation is what a recovery owner can observe about the target after
// acquiring the app lock. Every field is evidence about the ATTEMPTED
// release (the release the crashed operation was deploying) versus its
// known predecessor — the two identities the decision table reasons over.
// Collection of an Observation from a live host is the harness's job
// (harness_integration_test.go); the decision over it is pure.
type Observation struct {
	// Candidates: a RUNNING workload under the exact candidate names of
	// the attempted release ({app}-{process}-{version}[-{index}], labels
	// teploy.app/process/version — internal/docker/docker.go:82-117).
	Candidates Evidence
	// CandidateCorpses: candidate-named containers exist but are NOT
	// running (created-but-unstarted, exited) — the reconcilePartialRun
	// class (internal/deploy/deploy.go:1282-1292).
	CandidateCorpses Evidence
	// ForeignCandidates: RUNNING containers labeled teploy.app=<app> whose
	// names match NEITHER the attempted release's candidate names NOR the
	// predecessor's (a late effect from a dead owner landing under a name
	// the new owner never minted, an orphaned generation, a hand-run
	// container). Unattributable by construction — always MANUAL.
	ForeignCandidates Evidence
	// RouteToCandidate: the managed Caddy block / LB upstreams name the
	// attempted release's candidate containers (marker block
	// "# TEPLOY BEGIN <app>" — internal/caddy/caddy.go:20-21, 584-625).
	// Host ingress: the fixed port is held by the candidate. External
	// ingress: always Absent (no teploy-managed edge).
	RouteToCandidate Evidence
	// RouteToPredecessor: the managed block names the predecessor
	// generation's containers (or no managed block exists — Absent
	// together with RouteToCandidate means "no edge step", which is
	// normal for external ingress and a first deploy).
	RouteToPredecessor Evidence
	// StateToCandidate: state.json current_hash names the attempted
	// release — the fenced commit landed (internal/state/lock.go:353-381).
	StateToCandidate Evidence
	// StateToPredecessor: state.json names the predecessor release. Both
	// state fields Absent = no state file at all (first deploy, or wiped).
	StateToPredecessor Evidence
	// PredecessorServing: the predecessor workload (including a
	// same-version _replaced rename) is RUNNING — a live, known generation
	// to compensate back to.
	PredecessorServing Evidence
	// PredecessorStopped: predecessor containers exist but are stopped
	// (displaced to free a fixed port, or retirement half-done) —
	// restorable by restart, so still a compensation source.
	PredecessorStopped Evidence
	// ReleaseRecord: the per-release record
	// /deployments/<app>/meta/<hash>.json exists for the attempted
	// release (internal/releasemeta/releasemeta.go:168-176). Informational
	// for the decision: records converge (same-version rewrite, backfill),
	// so presence/absence never selects a disposition by itself.
	ReleaseRecord Evidence
}

// Decide is the transition table's crash-window decision function: given
// the state the recovery owner's own records say was reached, and the
// evidence observed on the target, what may automation do?
//
// Rules, in evaluation order (each is justified in the ADR):
//
//	R0  Encoding-impossible evidence (one state.json naming two releases,
//	    one predecessor both running and stopped) → MANUAL: never act on
//	    bookkeeping that contradicts itself.
//	R1  Unattributable workloads (ForeignCandidates present) → MANUAL.
//	R2  The authority is unreadable (either state field Unknown) → MANUAL:
//	    without state.json no action can know whether it is redoing,
//	    undoing, or hijacking.
//	R3  Any other unreadable class → INSPECT: re-observe (transient
//	    inventory/parse failures are reconcile triggers, not operator
//	    escalations), but never RETRY or COMPENSATE on unreadable evidence.
//	R3  Traffic PROVEN on a generation the authority does not name, with
//	    the predecessor PROVEN gone (not running, not stopped-restorable)
//	    → MANUAL, regardless of what the records claim (keep-or-rebuild is
//	    an operator decision, and a record that contradicts the readable
//	    authority is a reason NOT to act). Evaluated before the unreadable
//	    classes: when the dark-window fact is already proven, unreadable
//	    side evidence cannot make it actionable.
//	R4  Any other unreadable class → INSPECT: re-observe (transient
//	    inventory/parse failures are reconcile triggers, not operator
//	    escalations), but never RETRY or COMPENSATE on unreadable evidence.
//	R5  Conflicting edge evidence (route names BOTH generations) → INSPECT:
//	    reconcile against the records (ParseSites/ExtractPolicy exist for
//	    exactly this — internal/caddy/routes.go:89,429).
//	R6  Record/target disagreement (records say the commit was reached but
//	    state does not name the release, or vice versa) → INSPECT: the
//	    crash happened at a different point than recorded, or an operator
//	    moved authority (rollback rewrites state.json) — reconcile which
//	    generation is real before ANY action, even a redo that looks safe.
//	R7  Authority dispatch:
//	    - state names the ATTEMPTED release (committed):
//	        edge still serving the predecessor → INSPECT; otherwise RETRY —
//	        the tail (retirement, receipts) is idempotent/convergent.
//	    - state names the PREDECESSOR (uncommitted):
//	        edge on the candidates → COMPENSATE (the no-predecessor case
//	        was settled by R3);
//	        candidates running, edge unchanged → INSPECT (side effect
//	        landed, outcome unreconciled — never invented success);
//	        displaced/stopped predecessor with no live candidate →
//	        COMPENSATE (restart it; the app is dark in the recreate window);
//	        otherwise → RETRY (the effect never landed).
//	    - no state at all: candidates running → INSPECT; otherwise RETRY
//	      (clean first deploy; the edge-on-candidates case was settled by
//	      R3 — MANUAL).
//
// Decide is pure and total: every (State, Observation) yields a decision.
// The exhaustive test enumerates the full product space.
func Decide(from State, o Observation) Disposition {
	// R0: self-contradictory bookkeeping.
	if o.StateToCandidate == Present && o.StateToPredecessor == Present {
		return Manual
	}
	if o.PredecessorServing == Present && o.PredecessorStopped == Present {
		return Manual
	}
	// R1: unattributable workloads are the handoff's explicit conflict
	// class ("unknown container names") — never auto-decide.
	if o.ForeignCandidates == Present {
		return Manual
	}
	// R2: the authority is unreadable.
	if o.StateToCandidate == Unknown || o.StateToPredecessor == Unknown {
		return Manual
	}
	// R3: the proven-dark window. Traffic provably sits on a generation
	// the authority does not name, and the predecessor is provably not
	// restorable — keep-or-rebuild is an operator decision, whatever the
	// recovery owner's own records claim.
	if o.RouteToCandidate == Present && o.StateToCandidate == Absent &&
		o.PredecessorServing == Absent && o.PredecessorStopped == Absent {
		return Manual
	}
	// R4: any other unreadable class is a reconcile trigger, never a
	// license to redo or undo.
	if o.Candidates == Unknown || o.CandidateCorpses == Unknown ||
		o.ForeignCandidates == Unknown ||
		o.RouteToCandidate == Unknown || o.RouteToPredecessor == Unknown ||
		o.PredecessorServing == Unknown || o.PredecessorStopped == Unknown {
		return Inspect
	}
	// R5: the edge names both generations at once.
	if o.RouteToCandidate == Present && o.RouteToPredecessor == Present {
		return Inspect
	}
	// R6: the recorded state and the target's authority disagree on
	// whether the commit happened.
	recordedCommitted := from >= AuthoritativeStateCommitted
	observedCommitted := o.StateToCandidate == Present
	if recordedCommitted != observedCommitted {
		return Inspect
	}

	// R7: authority dispatch.
	switch {
	case o.StateToCandidate == Present:
		// Committed: the attempted release IS authoritative. Anything left
		// is tail convergence (retirement redo, record write/backfill,
		// log) — except an edge still serving the predecessor generation,
		// which is an authority/edge contradiction to reconcile.
		if o.RouteToPredecessor == Present {
			return Inspect
		}
		return Retry

	case o.StateToPredecessor == Present:
		// Uncommitted: dispatch on where partial effects stand.
		if o.RouteToCandidate == Present {
			// Traffic is on an uncommitted generation — the classic
			// traffic-switched crash window (abortStateCommit's shape).
			// The no-predecessor case was settled by R3 (MANUAL); a
			// restorable predecessor makes this COMPENSATE.
			return Compensate
		}
		if o.Candidates == Present {
			// Side effect landed, no receipt: reconcile — the handoff's
			// decisive "never invented success" case.
			return Inspect
		}
		if o.PredecessorStopped == Present {
			// No live candidate and the predecessor is displaced/stopped:
			// the recreate-strategy window — the app is dark until the
			// predecessor comes back.
			return Compensate
		}
		// Nothing landed (or only corpses under candidate names, which the
		// next attempt reconciles) and the predecessor is serving or never
		// existed: safe to redo.
		return Retry

	default:
		// No state file at all: a first deploy (or wiped bookkeeping).
		// Route-on-candidate with no restorable predecessor was settled by
		// R3 (MANUAL); a running candidate is a side effect to
		// reconcile; otherwise this is a clean first deploy.
		if o.Candidates == Present {
			return Inspect
		}
		return Retry
	}
}

// Transition is one edge of the deploy lifecycle lattice: the durable
// evidence that proves it landed, and the crash-window disposition for an
// owner that dies inside the transition's window (after the From state's
// effects began, before the To state's receipt exists).
type Transition struct {
	From State
	To   State
	// Evidence names the durable receipts proving the transition landed,
	// aligned with what the fenced-lock / releasemeta / Caddy code
	// persists today (file:line citations below and in the ADR).
	Evidence []string
	// CrashDisposition is the table's disposition for a crash inside this
	// transition's window when the observed evidence matches the window's
	// canonical partial state.
	CrashDisposition Disposition
	// Note records ingress-mode variants and where the current code's
	// machinery cannot yet produce or consume the evidence (the ADR's
	// disagreement findings).
	Note string
}

// Lattice returns the forward transition table. The crash dispositions
// here are the canonical per-window answers; Decide generalizes them over
// arbitrary observed evidence (and the exhaustive test proves the two
// agree on canonical observations).
func Lattice() []Transition {
	return []Transition{
		{
			From: Admitted, To: Prepared,
			Evidence: []string{
				"attempt dir /deployments/<app>/meta/att/<hash>.<id>/ (releasemeta/attempt.go:107-116)",
				"lock owner token in /deployments/<app>/.lock/info (state/lock.go:86-92, state.go:522-533)",
			},
			CrashDisposition: Retry,
			Note:             "retryable: artifacts are attempt-scoped (random id, write-once) and a fresh attempt collides with nothing",
		},
		{
			From: Prepared, To: CandidatesRunning,
			Evidence: []string{
				"container IDs returned by docker run (deploy/deploy.go:577-607)",
				"exact names {app}-{process}-{version}[-{index}] + labels teploy.app/process/version (docker/docker.go:82-117,160-165)",
			},
			CrashDisposition: Inspect,
			Note:             "inspect: a running candidate with no receipt is never success; corpses under candidate names are reconciled by the next attempt (deploy.go:1282-1292). Disagreeing today: names are version-keyed, so two attempts of one hash are not attributable (ADR finding; register F04/A09)",
		},
		{
			From: CandidatesRunning, To: ReadinessPassed,
			Evidence: []string{
				"readiness receipt /deployments/<app>/meta/att/<hash>.<id>/readiness.json — exact candidate IDs + probes + outcome, written on pass before the switch (deploy/journal.go, C01-4; internal/deploy/health.go probes remain ephemeral)",
			},
			CrashDisposition: Inspect,
			Note:             "receipt LANDED (C01-4): its presence derives ReadinessPassed and attributes the running candidates (attemptReadinessState/candidateAttribution → Decide); its absence keeps the window INSPECT. Wiring a recovery OWNER that reads it on acquisition is the C01-1 slice",
		},
		{
			From: ReadinessPassed, To: TrafficSwitched,
			Evidence: []string{
				"managed marker block '# TEPLOY BEGIN <app>'..'END' naming candidate upstreams in /deployments/caddy/Caddyfile (caddy/caddy.go:20-21,584-625)",
				"reload receipt: docker exec caddy caddy reload (caddy/caddy.go:29-32)",
				"delivery verification md5 host-vs-container (caddy/caddy.go:523-550)",
			},
			CrashDisposition: Compensate,
			Note:             "compensate via the recorded/serving predecessor (abortStateCommit's shape, deploy.go:1036-1095); MANUAL when the predecessor is gone. ingress:host variant: candidate holds the fixed port; ingress:external: no edge step (transition is a no-op)",
		},
		{
			From: TrafficSwitched, To: AuthoritativeStateCommitted,
			Evidence: []string{
				"fenced rename of /deployments/<app>/state.json naming the release (state/lock.go:353-381)",
				"AppState.Generation / OperationID (state/state.go:68-95)",
			},
			CrashDisposition: Compensate,
			Note:             "the commit is the single fenced atomic effect; a crash before it leaves traffic on an uncommitted generation",
		},
		{
			From: AuthoritativeStateCommitted, To: PredecessorRetired,
			Evidence: []string{
				"predecessor snapshot containers stopped (deploy/deploy.go:839-861,963-989)",
				"absence of predecessor names in docker ps label inventory (docker/docker.go:568-571)",
			},
			CrashDisposition: Retry,
			Note:             "retryable: retirement re-derives from the inventory; failures are reported, never silent — and since C01-5 the terminal log entry records them as a DEGRADED success instead of clean success; the durable predecessor snapshot (deploy/journal.go, C01-10) preserves the exact retirement set across crashes",
		},
		{
			From: PredecessorRetired, To: TerminalReceiptPersisted,
			Evidence: []string{
				"record /deployments/<app>/meta/<hash>.json, 0600, atomic (releasemeta/releasemeta.go:216-248)",
				"log entry appended to /deployments/teploy.log (state/state.go:662-679)",
			},
			CrashDisposition: Retry,
			Note:             "retryable/convergent: same-version record rewrite and live-container backfill (releasemeta.go:336-473) both heal a missing record. ADR finding: nothing reconciles it until the next deploy",
		},
	}
}
