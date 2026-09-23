package deploy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy/internal/deploy/recovery"
	"github.com/useteploy/teploy/internal/ssh"
	"github.com/useteploy/teploy/internal/state"
)

// takeoverMocks is the happy-path deploy mock set with the lock acquisition
// shaped as a STALE-BREAK takeover (C01-1): the first mkdir fails against a
// dead holder's stale info file, the retry succeeds, and the resulting
// fence handle reports TookOver.
func takeoverMocks(app string, staleInfo string, extra ...ssh.MockCommand) (*ssh.MockExecutor, *state.Lock) {
	return takeoverMocksBase(app, staleInfo, fenceHappyPathMocks(app), extra)
}

// takeoverMocksBase is takeoverMocks with the deploy's base mock set
// overridable (tests that need the Files-backed framed state read drop the
// happy set's explicit absent registration).
func takeoverMocksBase(app, staleInfo string, base, extra []ssh.MockCommand) (*ssh.MockExecutor, *state.Lock) {
	mkFail := ssh.MockCommand{Match: "mkdir /deployments/" + app + "/.lock", Err: errors.New("mkdir: file exists"), Once: true}
	mkOK := ssh.MockCommand{Match: "mkdir /deployments/" + app + "/.lock", Output: ""}
	// ReadLock reads the dead holder's info through `cat ... 2>/dev/null`;
	// the mock's Files-backed cat only answers the bare form, so the stale
	// info is registered as an explicit response.
	staleCat := ssh.MockCommand{Match: "cat /deployments/" + app + "/.lock/info", Output: staleInfo, Once: true}
	mocks := append([]ssh.MockCommand{mkFail, mkOK, staleCat}, base...)
	mocks = append(mocks, extra...)
	mock := ssh.NewMockExecutor("1.2.3.4", mocks...)
	mock.Files["/deployments/"+app+"/.lock/info"] = []byte(staleInfo)
	lk, err := state.AcquireLockFenced(context.Background(), mock, app)
	if err != nil {
		panic(fmt.Sprintf("takeoverMocks: acquiring the stale-broken lock: %v", err))
	}
	return mock, lk
}

func staleInfoJSON(t *testing.T) string {
	t.Helper()
	old := time.Now().UTC().Add(-2 * staleLockTTLSafe).Format(time.RFC3339)
	return fmt.Sprintf(`{"type":"auto","owner":"deadholder","ts":%q}`, old)
}

// staleLockTTLSafe mirrors state.staleLockTTL (unexported here) for fixture
// timestamps only.
const staleLockTTLSafe = 30 * time.Minute

// TestAcquireLockFenced_ReportsTakeover pins the state-package contract the
// reconciliation keys on: a fresh acquisition is not a takeover, breaking a
// stale lock is.
func TestAcquireLockFenced_ReportsTakeover(t *testing.T) {
	fresh := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "mkdir /deployments/freshapp/.lock", Output: ""},
	)
	lk, err := state.AcquireLockFenced(context.Background(), fresh, "freshapp")
	if err != nil {
		t.Fatalf("fresh acquire: %v", err)
	}
	if lk.TookOver() {
		t.Error("a fresh acquisition must not report takeover")
	}
	var nilLock *state.Lock
	if nilLock.TookOver() {
		t.Error("nil lock must not report takeover")
	}
}

// TestDeployFenced_TakeoverRefusesOnForeignRunningWork is C01-1's core
// scenario through the production deploy path (the harness's (a), wired):
// a stale lock is broken, the dead holder's late container is running
// under a foreign generation, and the replacement owner must REFUSE —
// reconcile before any of its own effects, never treat acquisition as
// quiescence.
func TestDeployFenced_TakeoverRefusesOnForeignRunningWork(t *testing.T) {
	app := "fency"
	mock, lk := takeoverMocks(app, staleInfoJSON(t),
		ssh.MockCommand{Match: "docker ps --all --filter label=teploy.app='fency'", Output: containerJSON(app, "fency-web-deadgen", "deadgen", "running")},
	)
	var buf bytes.Buffer
	d := NewDeployer(mock, &buf)
	err := d.DeployFenced(context.Background(), Config{
		App:     app,
		Domain:  "fency.com",
		Image:   "fency:latest",
		Version: "newgen",
		Health:  HealthConfig{Timeout: 5 * time.Second, Interval: 10 * time.Millisecond},
	}, lk)
	if err == nil {
		t.Fatal("expected the takeover deploy to be refused")
	}
	if !strings.Contains(err.Error(), "MANUAL") {
		t.Fatalf("expected MANUAL disposition in the refusal, got: %v", err)
	}
	if !strings.Contains(err.Error(), "unattributable") {
		t.Fatalf("expected the refusal to name the unattributable workload, got: %v", err)
	}
	for _, c := range mock.Calls {
		if strings.HasPrefix(c, "docker run") {
			t.Errorf("refused takeover must not start containers, saw: %s", c)
		}
		if strings.Contains(c, "state.json.tmp-") {
			t.Errorf("refused takeover must not commit state, saw: %s", c)
		}
	}
}

// TestDeployFenced_TakeoverCleanWorldProceedes: a takeover whose observed
// world matches a clean pre-deploy state (no leftover effects, serving
// predecessor or fresh app) proceeds — RETRY is the table's answer and the
// deploy must not become unusable after a crash.
func TestDeployFenced_TakeoverCleanWorldProceedes(t *testing.T) {
	app := "fency"
	mock, lk := takeoverMocks(app, staleInfoJSON(t),
		ssh.MockCommand{Match: "docker ps --all --filter label=teploy.app='fency'", Output: ""},
	)
	var buf bytes.Buffer
	d := NewDeployer(mock, &buf)
	if err := d.DeployFenced(context.Background(), Config{
		App:     app,
		Domain:  "fency.com",
		Image:   "fency:latest",
		Version: "abc123",
		Health:  HealthConfig{Timeout: 5 * time.Second, Interval: 10 * time.Millisecond},
	}, lk); err != nil {
		t.Fatalf("clean-world takeover deploy: %v", err)
	}
	if !strings.Contains(buf.String(), "Stale lock taken over") {
		t.Error("expected the takeover reconciliation to be surfaced to the operator")
	}
}

// TestDeployFenced_TakeoverSameVersionDisagreementRefused: state.json
// already names the release being deployed (a same-version redeploy after a
// takeover) — R6's record/target disagreement: INSPECT, never a blind redo.
func TestDeployFenced_TakeoverSameVersionDisagreementRefused(t *testing.T) {
	app := "fency"
	// The happy-path set registers state.json as absent; this test needs
	// the Files-backed framed read to answer with a real state, so that
	// registration is dropped.
	happy := fenceHappyPathMocks(app)
	filtered := make([]ssh.MockCommand, 0, len(happy))
	for _, m := range happy {
		if strings.Contains(m.Match, "/state.json") {
			continue
		}
		filtered = append(filtered, m)
	}
	base := append([]ssh.MockCommand{
		ssh.MockCommand{Match: "docker ps --all --filter label=teploy.app='fency'", Output: ""},
	}, filtered...)
	mock, lk := takeoverMocksBase(app, staleInfoJSON(t), base, nil)
	mock.Files["/deployments/"+app+"/state.json"] = []byte(`{"schema_version":2,"deployment_type":"container","ingress_mode":"caddy","current_hash":"abc123","updated_at":"2026-09-23T00:00:00Z"}`)
	var buf bytes.Buffer
	d := NewDeployer(mock, &buf)
	err := d.DeployFenced(context.Background(), Config{
		App:     app,
		Domain:  "fency.com",
		Image:   "fency:latest",
		Version: "abc123",
		Health:  HealthConfig{Timeout: 5 * time.Second, Interval: 10 * time.Millisecond},
	}, lk)
	if err == nil || !strings.Contains(err.Error(), "INSPECT") {
		t.Fatalf("expected INSPECT refusal for same-version disagreement, got: %v", err)
	}
}

// TestReconcileAfterTakeover_Dispositions drives the production reconciler
// over the table's remaining crash-window worlds and pins the mapping:
// running candidate without receipts → INSPECT; traffic on an uncommitted
// generation with a restorable predecessor → COMPENSATE; unreadable
// evidence that stays unreadable → INSPECT after the bounded re-observe.
func TestReconcileAfterTakeover_Dispositions(t *testing.T) {
	origDelay := reconcileReobserveDelay
	reconcileReobserveDelay = 5 * time.Millisecond
	t.Cleanup(func() { reconcileReobserveDelay = origDelay })

	stateJSON := func(hash string) string {
		if hash == "" {
			return ""
		}
		return fmt.Sprintf(`{"schema_version":2,"deployment_type":"container","ingress_mode":"caddy","current_hash":%q,"updated_at":"2026-09-23T00:00:00Z"}`, hash)
	}

	t.Run("running candidate without receipts is INSPECT", func(t *testing.T) {
		app := "fency"
		mock := ssh.NewMockExecutor("1.2.3.4",
			ssh.MockCommand{Match: "docker ps --all --filter label=teploy.app='fency'", Output: containerJSON(app, "fency-web-c0ffee", "c0ffee", "running")},
		)
		var buf bytes.Buffer
		d := NewDeployer(mock, &buf)
		err := d.ReconcileAfterTakeover(context.Background(), Config{App: app, Version: "c0ffee"}, nil)
		if err == nil || !strings.Contains(err.Error(), "INSPECT") {
			t.Fatalf("expected INSPECT, got: %v", err)
		}
	})

	t.Run("traffic on uncommitted generation is COMPENSATE", func(t *testing.T) {
		app := "fency"
		mock := ssh.NewMockExecutor("1.2.3.4",
			ssh.MockCommand{Match: "docker ps --all --filter label=teploy.app='fency'", Output: containerJSON(app, "fency-web-old123", "old123", "running")},
		)
		mock.Files["/deployments/"+app+"/state.json"] = []byte(stateJSON("old123"))
		mock.Files["/deployments/caddy/Caddyfile"] = []byte("# TEPLOY BEGIN fency\nfency.com {\n reverse_proxy fency-web-c0ffee:80\n}\n# TEPLOY END fency\n")
		var buf bytes.Buffer
		d := NewDeployer(mock, &buf)
		err := d.ReconcileAfterTakeover(context.Background(), Config{App: app, Version: "c0ffee"}, mustReadState(t, mock, app))
		if err == nil || !strings.Contains(err.Error(), "COMPENSATION") {
			t.Fatalf("expected COMPENSATE, got: %v", err)
		}
	})

	t.Run("persistent unreadable evidence stays INSPECT", func(t *testing.T) {
		app := "fency"
		mock := ssh.NewMockExecutor("1.2.3.4",
			ssh.MockCommand{Match: "docker ps --all --filter label=teploy.app='fency'", Err: errors.New("docker: connection refused")},
		)
		var buf bytes.Buffer
		d := NewDeployer(mock, &buf)
		err := d.ReconcileAfterTakeover(context.Background(), Config{App: app, Version: "c0ffee"}, nil)
		if err == nil || !strings.Contains(err.Error(), "INSPECT") {
			t.Fatalf("expected INSPECT for unreadable evidence, got: %v", err)
		}
	})
}

// TestReconcileAfterTakeover_TransientReadRecovers: R4's unreadable classes
// are reconcile triggers — one bounded re-observation absorbs a transient
// inventory failure and the deploy proceeds on the clean re-observation.
func TestReconcileAfterTakeover_TransientReadRecovers(t *testing.T) {
	origDelay := reconcileReobserveDelay
	reconcileReobserveDelay = 5 * time.Millisecond
	t.Cleanup(func() { reconcileReobserveDelay = origDelay })

	app := "fency"
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "docker ps --all --filter label=teploy.app='fency'", Err: errors.New("docker: temporary failure"), Once: true},
		ssh.MockCommand{Match: "docker ps --all --filter label=teploy.app='fency'", Output: ""},
	)
	var buf bytes.Buffer
	d := NewDeployer(mock, &buf)
	if err := d.ReconcileAfterTakeover(context.Background(), Config{App: app, Version: "c0ffee"}, nil); err != nil {
		t.Fatalf("transient inventory failure must recover via re-observation: %v", err)
	}
	if !strings.Contains(buf.String(), "proceeding") {
		t.Error("expected the reconciled takeover to report proceeding")
	}
}

// TestObserve_Classification pins the evidence collector's exact-name
// classification: candidates, corpses, foreign workloads, predecessor
// serving (including a same-version _replaced rename) and stopped, over
// unreadable inventories.
func TestObserve_Classification(t *testing.T) {
	app := "fency"

	t.Run("predecessor serving under _replaced rename", func(t *testing.T) {
		mock := ssh.NewMockExecutor("1.2.3.4",
			ssh.MockCommand{Match: "docker ps --all --filter label=teploy.app='fency'", Output: containerJSON(app, "fency-web-old123_replaced", "old123", "running")},
		)
		o := Observe(context.Background(), mock, app, "newgen", "old123")
		if o.PredecessorServing != recovery.Present {
			t.Errorf("expected PredecessorServing for a running _replaced rename, got %v", o.PredecessorServing)
		}
	})

	t.Run("stopped predecessor is restorable evidence", func(t *testing.T) {
		mock := ssh.NewMockExecutor("1.2.3.4",
			ssh.MockCommand{Match: "docker ps --all --filter label=teploy.app='fency'", Output: containerJSON(app, "fency-web-old123", "old123", "exited")},
		)
		o := Observe(context.Background(), mock, app, "newgen", "old123")
		if o.PredecessorStopped != recovery.Present {
			t.Errorf("expected PredecessorStopped, got %v", o.PredecessorStopped)
		}
	})

	t.Run("candidate corpse is not a running candidate", func(t *testing.T) {
		mock := ssh.NewMockExecutor("1.2.3.4",
			ssh.MockCommand{Match: "docker ps --all --filter label=teploy.app='fency'", Output: containerJSON(app, "fency-web-newgen", "newgen", "created")},
		)
		o := Observe(context.Background(), mock, app, "newgen", "old123")
		if o.CandidateCorpses != recovery.Present || o.Candidates != recovery.Absent {
			t.Errorf("expected corpse evidence only, got candidates=%v corpses=%v", o.Candidates, o.CandidateCorpses)
		}
	})

	t.Run("unreadable inventory maps to unknown classes", func(t *testing.T) {
		mock := ssh.NewMockExecutor("1.2.3.4",
			ssh.MockCommand{Match: "docker ps --all --filter label=teploy.app='fency'", Err: errors.New("boom")},
		)
		o := Observe(context.Background(), mock, app, "newgen", "old123")
		if o.Candidates != recovery.Unknown {
			t.Errorf("expected Unknown candidates, got %v", o.Candidates)
		}
	})
}

// containerJSON renders one docker-ps --format JSON line the way
// ParseContainers consumes it (labels in docker's comma-joined display
// form).
func containerJSON(app, name, version, stateName string) string {
	return fmt.Sprintf(`{"ID":"deadbeefdead","Names":%q,"Image":"img:1","State":%q,"Status":"up","CreatedAt":"2026-05-28 21:33:29 -0700 PDT","Labels":"teploy.app=%s,teploy.process=web,teploy.version=%s"}`,
		name, stateName, app, version)
}

func mustReadState(t *testing.T, mock *ssh.MockExecutor, app string) *state.AppState {
	t.Helper()
	st, err := state.Read(context.Background(), mock, app)
	if err != nil {
		t.Fatalf("reading state fixture: %v", err)
	}
	return st
}
