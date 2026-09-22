package recovery

import (
	"fmt"
	"testing"
)

// The tests are the point (the handoff explicitly rejects a doc-only
// table): the canonical table pins each state's crash-window disposition,
// the exhaustive product-space test pins the safety invariants over EVERY
// (state, observation) pair, and the conflict scenarios pin the handoff's
// named evidence conflicts to exact dispositions.

// quiescentPredecessorWorld is the canonical observation of an app whose
// authority names the predecessor, whose edge serves the predecessor, and
// whose predecessor is running — the "nothing of the attempt landed"
// world.
func quiescentPredecessorWorld() Observation {
	return Observation{
		Candidates:         Absent,
		CandidateCorpses:   Absent,
		ForeignCandidates:  Absent,
		RouteToCandidate:   Absent,
		RouteToPredecessor: Present,
		StateToCandidate:   Absent,
		StateToPredecessor: Present,
		PredecessorServing: Present,
		PredecessorStopped: Absent,
		ReleaseRecord:      Absent,
	}
}

// quiescentFirstDeploy is the canonical no-predecessor world: no state, no
// edge, nothing running.
func quiescentFirstDeploy() Observation {
	return Observation{
		Candidates:         Absent,
		CandidateCorpses:   Absent,
		ForeignCandidates:  Absent,
		RouteToCandidate:   Absent,
		RouteToPredecessor: Absent,
		StateToCandidate:   Absent,
		StateToPredecessor: Absent,
		PredecessorServing: Absent,
		PredecessorStopped: Absent,
		ReleaseRecord:      Absent,
	}
}

// candidateSideEffectNoReceipt is the handoff's scenario (b): candidate
// containers RUNNING, edge and authority untouched, no release record.
func candidateSideEffectNoReceipt() Observation {
	o := quiescentPredecessorWorld()
	o.Candidates = Present
	o.ReleaseRecord = Absent
	return o
}

// trafficOnUncommitted is the traffic-switched crash window: edge names
// the candidates, authority still names the predecessor, predecessor
// serving.
func trafficOnUncommitted() Observation {
	o := candidateSideEffectNoReceipt()
	o.RouteToCandidate = Present
	o.RouteToPredecessor = Absent
	return o
}

// committedWorld is the post-commit world: authority names the attempted
// release, edge follows it, candidates running, predecessor still
// serving (its retirement is the remaining tail).
func committedWorld() Observation {
	return Observation{
		Candidates:         Present,
		CandidateCorpses:   Absent,
		ForeignCandidates:  Absent,
		RouteToCandidate:   Present,
		RouteToPredecessor: Absent,
		StateToCandidate:   Present,
		StateToPredecessor: Absent,
		PredecessorServing: Present,
		PredecessorStopped: Absent,
		ReleaseRecord:      Absent,
	}
}

// TestCanonicalTable pins each state's canonical crash-window disposition:
// the decision an owner gets when the target looks EXACTLY like the
// canonical partial state of a crash just inside that state's window.
// These must match Lattice()'s CrashDisposition for the transition LEAVING
// each state.
func TestCanonicalTable(t *testing.T) {
	cases := []struct {
		from State
		obs  Observation
		want Disposition
		why  string
	}{
		// Admitted: crash right after acquiring the lock. Blue/green world
		// (predecessor serving) and first deploy both retry — nothing of
		// the attempt landed.
		{Admitted, quiescentPredecessorWorld(), Retry, "nothing landed, predecessor serving"},
		{Admitted, quiescentFirstDeploy(), Retry, "nothing landed, first deploy"},
		// Prepared: artifacts exist. Blue/green retries (the predecessor
		// was never touched); the recreate-strategy window (predecessor
		// DISPLACED to free the fixed port, no candidate yet) is dark and
		// must be compensated (restart the displaced workload).
		{Prepared, quiescentPredecessorWorld(), Retry, "artifacts only, predecessor serving"},
		{Prepared, func() Observation {
			o := quiescentPredecessorWorld()
			o.PredecessorServing = Absent
			o.PredecessorStopped = Present
			return o
		}(), Compensate, "recreate window: predecessor displaced, app dark"},
		// CandidatesRunning: containers started, readiness not passed, no
		// receipts. Never success; reconcile.
		{CandidatesRunning, candidateSideEffectNoReceipt(), Inspect, "side effect landed, no receipt"},
		{CandidatesRunning, quiescentPredecessorWorld(), Retry, "containers never landed (late docker run still pending would show as foreign/unknown — this row is the provably-empty case)"},
		// ReadinessPassed: the readiness gate has NO durable receipt, so
		// its canonical world is indistinguishable from CandidatesRunning's
		// — same disposition (ADR finding: the missing receipt collapses
		// the two windows).
		{ReadinessPassed, candidateSideEffectNoReceipt(), Inspect, "readiness is unobservable post-crash; reconcile"},
		// TrafficSwitched: edge on the uncommitted generation.
		{TrafficSwitched, trafficOnUncommitted(), Compensate, "traffic on uncommitted generation, predecessor restorable"},
		{TrafficSwitched, func() Observation {
			o := trafficOnUncommitted()
			o.PredecessorServing = Absent
			o.PredecessorStopped = Present
			return o
		}(), Compensate, "predecessor displaced but restorable"},
		{TrafficSwitched, func() Observation {
			o := trafficOnUncommitted()
			o.PredecessorServing = Absent
			o.PredecessorStopped = Absent
			return o
		}(), Manual, "predecessor gone: keep-or-rebuild is an operator decision"},
		// AuthoritativeStateCommitted: finish the tail (retirement,
		// receipts) — idempotent.
		{AuthoritativeStateCommitted, committedWorld(), Retry, "committed; tail converges"},
		{AuthoritativeStateCommitted, func() Observation {
			o := committedWorld()
			o.PredecessorServing = Absent
			o.PredecessorStopped = Absent
			o.ReleaseRecord = Absent
			return o
		}(), Retry, "committed; predecessor already gone, record still converges"},
		// PredecessorRetired: persist the receipts.
		{PredecessorRetired, func() Observation {
			o := committedWorld()
			o.PredecessorServing = Absent
			o.PredecessorStopped = Absent
			return o
		}(), Retry, "retired; write/verify receipts"},
		// TerminalReceiptPersisted: converged; RETRY here means verify-only
		// reconvergence (the redo does nothing).
		{TerminalReceiptPersisted, func() Observation {
			o := committedWorld()
			o.PredecessorServing = Absent
			o.PredecessorStopped = Absent
			o.ReleaseRecord = Present
			return o
		}(), Retry, "converged; verify-only"},
	}
	for _, tc := range cases {
		got := Decide(tc.from, tc.obs)
		if got != tc.want {
			t.Errorf("Decide(%s, %s) = %s, want %s (%s)", tc.from, tc.why, got, tc.want, tc.why)
		}
	}
}

// TestLatticeMatchesCanonicalWindows proves the data lattice and the
// decision function agree: for each forward transition, crashing inside
// its window (the To-state's effects may have begun, its receipt does not
// exist) with the canonical partial evidence yields the lattice's
// recorded CrashDisposition.
func TestLatticeMatchesCanonicalWindows(t *testing.T) {
	for _, tr := range Lattice() {
		var obs Observation
		switch tr.To {
		case Prepared:
			// Artifact generation window: candidates never started.
			obs = quiescentPredecessorWorld()
		case CandidatesRunning, ReadinessPassed:
			// Candidates may be landing/landed; readiness leaves no
			// receipt, so both windows share the side-effect world.
			obs = candidateSideEffectNoReceipt()
		case TrafficSwitched, AuthoritativeStateCommitted:
			// Edge may have switched; the state commit has not landed.
			obs = trafficOnUncommitted()
		case PredecessorRetired:
			obs = committedWorld()
		case TerminalReceiptPersisted:
			obs = committedWorld()
			obs.PredecessorServing = Absent
			obs.ReleaseRecord = Absent
		}
		if got := Decide(tr.From, obs); got != tr.CrashDisposition {
			t.Errorf("lattice says %s for %s→%s, Decide says %s", tr.CrashDisposition, tr.From, tr.To, got)
		}
	}
}

// TestHandoffConflictScenarios pins the handoff's named conflicting
// evidence classes to exact dispositions, from every state.
func TestHandoffConflictScenarios(t *testing.T) {
	// "Candidate container running but route never switched": INSPECT from
	// every state whose record is pre-commit; from post-commit records the
	// record/target disagreement rule also yields INSPECT — i.e. a running
	// uncommitted side effect is INSPECT no matter what the record claims.
	o := candidateSideEffectNoReceipt()
	for _, from := range AllStates() {
		if got := Decide(from, o); got != Inspect {
			t.Errorf("running candidate + untouched route from %s = %s, want INSPECT (never invented success)", from, got)
		}
	}

	// "Unknown container names" (a running app-labeled container neither
	// candidate nor predecessor): MANUAL from every state.
	o = quiescentPredecessorWorld()
	o.ForeignCandidates = Present
	for _, from := range AllStates() {
		if got := Decide(from, o); got != Manual {
			t.Errorf("foreign candidate from %s = %s, want MANUAL (unattributable)", from, got)
		}
	}

	// "Predecessor already retired" while traffic sits on the uncommitted
	// generation: MANUAL — there is nothing recorded to compensate to.
	o = trafficOnUncommitted()
	o.PredecessorServing = Absent
	for _, from := range AllStates() {
		if got := Decide(from, o); got != Manual {
			t.Errorf("traffic on uncommitted generation + predecessor gone from %s = %s, want MANUAL", from, got)
		}
	}

	// Crash after side effect before receipt, first deploy (no state at
	// all): INSPECT from every state — never success, never a blind retry.
	o = Observation{Candidates: Present}
	for _, from := range AllStates() {
		if got := Decide(from, o); got != Inspect {
			t.Errorf("candidate running with NO state file from %s = %s, want INSPECT", from, got)
		}
	}

	// A route naming BOTH generations (mixed upstreams / split evidence):
	// INSPECT — reconcile against the records.
	o = trafficOnUncommitted()
	o.RouteToPredecessor = Present
	for _, from := range AllStates() {
		if got := Decide(from, o); got != Inspect {
			t.Errorf("route naming both generations from %s = %s, want INSPECT", from, got)
		}
	}

	// An operator rolled back between the record and the recovery
	// (authority moved back to the predecessor while the recorded state
	// says committed): INSPECT — never a blind redeploy that would undo
	// the operator's decision.
	o = quiescentPredecessorWorld()
	for _, from := range []State{AuthoritativeStateCommitted, PredecessorRetired, TerminalReceiptPersisted} {
		if got := Decide(from, o); got != Inspect {
			t.Errorf("committed record + predecessor authority from %s = %s, want INSPECT (record/target disagreement)", from, got)
		}
	}
}

// TestExhaustiveProductSpace enumerates EVERY (state, observation) pair —
// 8 states × 3^10 evidence combinations — and asserts the safety
// invariants. A doc table cannot do this; this is the contract the C01
// implementation slices build on.
func TestExhaustiveProductSpace(t *testing.T) {
	states := AllStates()
	fields := 10
	total := 1
	for range fields {
		total *= 3
	}
	checked := 0
	for _, from := range states {
		for combo := 0; combo < total; combo++ {
			var o Observation
			decode(combo, &o)
			d := Decide(from, o)
			checkInvariants(t, from, o, d)
			checked++
		}
	}
	if checked != len(states)*total {
		t.Fatalf("enumerated %d pairs, expected %d", checked, len(states)*total)
	}
}

// decode fills o from a base-3 encoding of its ten Evidence fields.
func decode(combo int, o *Observation) {
	vals := []Evidence{Absent, Present, Unknown}
	fs := []*Evidence{
		&o.Candidates, &o.CandidateCorpses, &o.ForeignCandidates,
		&o.RouteToCandidate, &o.RouteToPredecessor,
		&o.StateToCandidate, &o.StateToPredecessor,
		&o.PredecessorServing, &o.PredecessorStopped, &o.ReleaseRecord,
	}
	for _, f := range fs {
		*f = vals[combo%3]
		combo /= 3
	}
}

// checkInvariants asserts the cross-cutting safety contract for one
// (state, observation, decision) triple. Every rule here is a property
// the ADR's disposition rules promise for ALL inputs.
func checkInvariants(t *testing.T, from State, o Observation, d Disposition) {
	t.Helper()
	desc := fmt.Sprintf("Decide(%s, %+v) = %s", from, o, d)

	// I1: unattributable workloads are never auto-decided.
	if o.ForeignCandidates == Present && d != Manual {
		t.Errorf("%s: foreign candidate must be MANUAL", desc)
	}
	// I2: unreadable authority is never auto-decided.
	if (o.StateToCandidate == Unknown || o.StateToPredecessor == Unknown) && d != Manual {
		t.Errorf("%s: unreadable state must be MANUAL", desc)
	}
	// I3: no redo or undo on unreadable non-authority evidence.
	if d == Retry || d == Compensate {
		for _, e := range []Evidence{o.Candidates, o.CandidateCorpses, o.ForeignCandidates,
			o.RouteToCandidate, o.RouteToPredecessor,
			o.PredecessorServing, o.PredecessorStopped} {
			if e == Unknown {
				t.Errorf("%s: RETRY/COMPENSATE on unreadable evidence", desc)
			}
		}
	}
	// I4: compensation always has a restorable predecessor and an
	// uncommitted authority — undo targets the recorded/serving
	// predecessor generation, never a guess.
	if d == Compensate &&
		!(o.StateToPredecessor == Present && (o.PredecessorServing == Present || o.PredecessorStopped == Present)) {
		t.Errorf("%s: COMPENSATE without a restorable predecessor under predecessor authority", desc)
	}
	// I5: retry requires fully readable, non-conflicting, attributable
	// evidence — including self-consistent authority encoding.
	if d == Retry {
		if o.ForeignCandidates == Present {
			t.Errorf("%s: RETRY with foreign candidate", desc)
		}
		if o.RouteToCandidate == Present && o.RouteToPredecessor == Present {
			t.Errorf("%s: RETRY with conflicting route evidence", desc)
		}
		if o.StateToCandidate == Present && o.StateToPredecessor == Present {
			t.Errorf("%s: RETRY with impossible state encoding", desc)
		}
		if o.PredecessorServing == Present && o.PredecessorStopped == Present {
			t.Errorf("%s: RETRY with impossible predecessor encoding", desc)
		}
	}
	// I6: traffic on the uncommitted generation with the predecessor gone
	// is never auto-decided (keep-or-rebuild is an operator call).
	if o.RouteToCandidate == Present && o.StateToPredecessor == Present &&
		o.PredecessorServing == Absent && o.PredecessorStopped == Absent &&
		o.StateToCandidate == Absent && o.ForeignCandidates == Absent &&
		d != Manual {
		t.Errorf("%s: uncommitted traffic + gone predecessor must be MANUAL", desc)
	}
	// I7: never invented success — a running candidate under uncommitted
	// or absent authority, with the edge NOT committed to it either, is
	// INSPECT at minimum (Inspect or Manual). When the edge IS on the
	// candidates the world is the explicit traffic-switched window and
	// COMPENSATE is the designed answer, so it is exempt.
	if o.Candidates == Present && o.StateToCandidate != Present &&
		o.RouteToCandidate != Present &&
		o.ForeignCandidates == Absent &&
		o.StateToCandidate != Unknown && o.StateToPredecessor != Unknown &&
		d != Inspect && d != Manual {
		t.Errorf("%s: uncommitted running candidate must INSPECT or MANUAL, got %s", desc, d)
	}
}

// TestExhaustiveCanonicalCoverage spot-proves the enumeration actually
// reaches the canonical worlds (guards a decode bug vacuously passing the
// invariant checks).
func TestExhaustiveCanonicalCoverage(t *testing.T) {
	// foreignWorld: a running app-labeled container under unknown names.
	foreignWorld := quiescentPredecessorWorld()
	foreignWorld.ForeignCandidates = Present
	// darkWorld: traffic on the uncommitted generation, predecessor gone.
	darkWorld := trafficOnUncommitted()
	darkWorld.PredecessorServing = Absent

	seen := map[Disposition]bool{}
	for _, from := range AllStates() {
		for _, o := range []Observation{
			quiescentPredecessorWorld(), quiescentFirstDeploy(),
			candidateSideEffectNoReceipt(), trafficOnUncommitted(),
			committedWorld(), foreignWorld, darkWorld,
		} {
			// Prove decode() can express it: find its combo and re-decode.
			if !comboExists(o) {
				t.Fatalf("decode space does not express %+v", o)
			}
			seen[Decide(from, o)] = true
		}
	}
	for _, d := range []Disposition{Retry, Inspect, Compensate, Manual} {
		if !seen[d] {
			t.Errorf("disposition %s never produced by canonical worlds", d)
		}
	}
}

func comboExists(o Observation) bool {
	total := 59049 // 3^10
	for combo := 0; combo < total; combo++ {
		var probe Observation
		decode(combo, &probe)
		if probe == o {
			return true
		}
	}
	return false
}
