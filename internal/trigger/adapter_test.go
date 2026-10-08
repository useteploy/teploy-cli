package trigger

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/useteploy/teploy/internal/state"
)

// targetJournal is independent target storage: new Adapter instances and
// delivery ledgers have no shared local cache. Its mutex models the app lease
// held by production wrappers, not an imaginary delivery FIFO.
type targetJournal struct {
	rows      map[string]Receipt
	failWrite bool
}

func (s *targetJournal) Load(context.Context) ([]Receipt, error) {
	var rows []Receipt
	for _, r := range s.rows {
		rows = append(rows, r)
	}
	return rows, nil
}
func (s *targetJournal) Save(_ context.Context, r Receipt) error {
	if s.failWrite {
		return errors.New("fsync failed")
	}
	if s.rows == nil {
		s.rows = map[string]Receipt{}
	}
	s.rows[r.Request.OperationKey] = r
	return nil
}
func requestFixture() Request {
	r := Request{SchemaVersion: 1, TargetID: strings.Repeat("a", 32) + "/demo", SourceID: "source-stable", RepositoryID: `["generic","https://forge.example","url:https://forge.example/team/repo"]`, Ref: "refs/heads/main", Commit: strings.Repeat("b", 40), ExecutionBindingDigest: strings.Repeat("c", 64), Action: "deploy"}
	r.OperationKey = r.Key()
	return r
}
func completed(r Request) *Result {
	now := time.Now().UTC()
	gen := uint64(7)
	return &Result{SchemaVersion: 1, OperationKey: r.OperationKey, Publication: "committed", Reconciliation: "complete", Generation: &gen, ReleaseHash: "actual-release", ImageDigest: "sha256:" + strings.Repeat("d", 64), AuthorityObservedAt: &now}
}

func TestAdapterSixConcurrentTriggersRestartRetentionAndBinding(t *testing.T) {
	ctx := context.Background()
	store := &targetJournal{}
	var lease sync.Mutex
	var wg sync.WaitGroup
	effects := 0
	actual := map[string]*Result{}
	r := requestFixture()
	for _, kind := range []string{"manual", "webhook", "schedule", "PR", "CI", "resident-listener"} {
		wg.Add(1)
		go func(kind string) {
			defer wg.Done()
			lease.Lock()
			defer lease.Unlock()
			adapter := Adapter{Store: store, Observe: func(_ context.Context, receipt Receipt) (*Result, error) {
				return actual[receipt.Request.OperationKey], nil
			}}
			if _, err := adapter.Run(ctx, r, func(context.Context) error {
				if _, ok := store.rows[r.OperationKey]; !ok {
					t.Error("effect before durable admission")
				}
				effects++
				actual[r.OperationKey] = completed(r)
				return nil
			}); err != nil {
				t.Errorf("%s: %v", kind, err)
			}
		}(kind)
	}
	wg.Wait()
	if effects != 1 {
		t.Fatalf("effects=%d", effects)
	}
	// No delivery history survives; only the target journal and real authority.
	adapter := Adapter{Store: store, Observe: func(_ context.Context, receipt Receipt) (*Result, error) {
		return actual[receipt.Request.OperationKey], nil
	}}
	if _, err := adapter.Run(ctx, r, func(context.Context) error { t.Fatal("restart replay prepared"); return nil }); err != nil {
		t.Fatal(err)
	}
	changed := r
	changed.ExecutionBindingDigest = strings.Repeat("e", 64)
	changed.OperationKey = changed.Key()
	if _, err := adapter.Run(ctx, changed, func(context.Context) error { effects++; actual[changed.OperationKey] = completed(changed); return nil }); err != nil {
		t.Fatal(err)
	}
	if effects != 2 {
		t.Fatal("same commit changed binding was swallowed")
	}
	reused := r
	reused.TargetID = strings.Repeat("f", 32) + "/demo"
	if err := reused.Validate(); err == nil {
		t.Fatal("reused alias identity tamper accepted")
	}
}
func TestAdapterCrashPhasesAndUnknownNeverPrepare(t *testing.T) {
	for _, phase := range []string{"before-prepare", "after-candidate", "after-route", "after-state-rename"} {
		t.Run(phase, func(t *testing.T) {
			r := requestFixture()
			s := &targetJournal{}
			_ = s.Save(context.Background(), Receipt{Request: r, Result: Initial(r)})
			adapter := Adapter{Store: s, Observe: func(context.Context, Receipt) (*Result, error) {
				if phase == "after-state-rename" {
					return completed(r), nil
				}
				return nil, errors.New("actual authority unproven")
			}}
			result, err := adapter.Run(context.Background(), r, func(context.Context) error { t.Fatal("replay prepared before reconciliation"); return nil })
			if phase == "after-state-rename" {
				if err != nil || result.Publication != "committed" {
					t.Fatal(result, err)
				}
			} else if err == nil || result.Publication != "unknown" || result.Generation != nil {
				t.Fatal(result, err)
			}
			if len(s.rows) != 1 {
				t.Fatal("debt erased")
			}
		})
	}
}
func TestAdapterPublicationFailuresAndPrecommitHonesty(t *testing.T) {
	for _, outcome := range []string{"committed-repair-failed", "unknown-read", "ordinary-health-failed"} {
		t.Run(outcome, func(t *testing.T) {
			r := requestFixture()
			s := &targetJournal{}
			effectError := errors.New("health gate refused")
			observation := completed(r)
			switch outcome {
			case "committed-repair-failed":
				effectError = &state.PublicationError{Committed: true, Err: errors.New("sidecar fsync")}
			case "unknown-read":
				effectError = &state.PublicationError{Unknown: true, Err: errors.New("read transport")}
				observation = nil
			case "ordinary-health-failed":
				observation = &Result{SchemaVersion: 1, OperationKey: r.OperationKey, Publication: "not_committed", Reconciliation: "complete"}
			}
			adapter := Adapter{Store: s, Observe: func(context.Context, Receipt) (*Result, error) { return observation, nil }}
			result, err := adapter.Run(context.Background(), r, func(context.Context) error { return effectError })
			if err == nil {
				t.Fatal("nonzero lost")
			}
			if outcome == "ordinary-health-failed" {
				if result.Publication != "not_committed" {
					t.Fatal(result)
				}
			} else if result.Reconciliation != "required" || s.rows[r.OperationKey].Completed {
				t.Fatal("repair debt deduplicated", result)
			}
		})
	}
}
func TestStrictBoundedPrivateDecode(t *testing.T) {
	r := requestFixture()
	good, _ := json.Marshal(r)
	if _, err := Decode(strings.NewReader(string(good))); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{string(good) + " {}", strings.Replace(string(good), `"schema_version":1`, `"schema_version":1,"schema_version":1`, 1), strings.Replace(string(good), `"expected_generation":0`, `"expected_generation":null`, 1), strings.Replace(string(good), r.Commit, strings.Repeat("0", 40), 1), strings.Repeat("x", MaxRequestBytes+1)} {
		if _, err := Decode(strings.NewReader(bad)); err == nil {
			t.Fatal("ambiguous/malformed/deletion/oversize accepted")
		}
	}
	tampered := r
	tampered.Ref = "refs/heads/other"
	if tampered.Validate() == nil {
		t.Fatal("field tamper admitted")
	}
	invalid := r
	invalid.OperationKey = "dummy-secret"
	b, _ := json.Marshal(Initial(invalid))
	if strings.Contains(string(b), "dummy-secret") {
		t.Fatal("unvalidated private input echoed")
	}
}
