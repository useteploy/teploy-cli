package trigger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/useteploy/teploy/internal/state"
)

// Receipt survives listener restart and local delivery retention. Pending
// receipts are never pruned. Identity is persisted, not merely its hash.
type Receipt struct {
	Request   Request         `json:"request"`
	Evidence  json.RawMessage `json:"evidence,omitempty"`
	Result    Result          `json:"result"`
	Completed bool            `json:"completed"`
}
type Store interface {
	Load(context.Context) ([]Receipt, error)
	Save(context.Context, Receipt) error
}

// Observe must prove immutable release/image/private binding and actual
// authority. A nil observation means no completion is proven, not absence of
// effects. It must never convert transport/parse errors to absence.
type Observe func(context.Context, Receipt) (*Result, error)
type Adapter struct {
	Store    Store
	Observe  Observe
	Evidence func(context.Context, Request) (json.RawMessage, error)
}

var ErrDebt = errors.New("target operation requires reconciliation before preparation")

// Run is called AFTER acquiring the renewable app lease and BEFORE any
// preparation. The caller retains the SAME lease until Run returns.
func (a Adapter) Run(ctx context.Context, r Request, effect func(context.Context) error) (Result, error) {
	result := Initial(r)
	if err := r.Validate(); err != nil {
		result.ErrorCode = "invalid_request"
		return result, err
	}
	receipts, err := a.Store.Load(ctx)
	if err != nil {
		result.Publication = "unknown"
		result.ErrorCode = "journal_read_failed"
		return result, err
	}
	for _, prior := range receipts {
		if prior.Request.OperationKey == r.OperationKey && !sameIdentity(prior.Request, r) {
			result.ErrorCode = "identity_conflict"
			return result, fmt.Errorf("receipt identity mismatch")
		}
		if !prior.Completed && prior.Result.Publication == "not_committed" && prior.Result.Reconciliation == "complete" {
			continue
		}
		if prior.Completed {
			if prior.Request.OperationKey == r.OperationKey {
				return prior.Result, nil
			}
			continue
		}
		observed, readErr := a.Observe(ctx, prior)
		if readErr != nil || observed == nil || observed.Reconciliation != "complete" {
			result.Publication = "unknown"
			result.ErrorCode = "reconciliation_required"
			if prior.Request.OperationKey == r.OperationKey && observed != nil {
				result = *observed
				result.ErrorCode = "reconciliation_required"
			}
			return result, errors.Join(ErrDebt, readErr)
		}
		repaired := Receipt{Request: prior.Request, Evidence: prior.Evidence, Result: *observed, Completed: observed.Publication == "committed"}
		if err = a.Store.Save(ctx, repaired); err != nil {
			result.Publication = "unknown"
			result.ErrorCode = "journal_write_failed"
			return result, err
		}
		if prior.Request.OperationKey == r.OperationKey && observed.Publication == "committed" {
			return *observed, nil
		}
	}
	pending := Receipt{Request: r, Result: result}
	if a.Evidence != nil {
		pending.Evidence, err = a.Evidence(ctx, r)
		if err != nil {
			result.ErrorCode = "authority_admission_failed"
			return result, err
		}
	}
	if err = a.Store.Save(ctx, pending); err != nil {
		result.ErrorCode = "journal_write_failed"
		return result, err
	}
	// The durable pending frame precedes the callback, including builds, pulls,
	// migrations and candidate creation. A crash at any point leaves debt.
	effectErr := effect(ctx)
	recovery, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	observed, readErr := a.Observe(recovery, pending)
	if observed != nil {
		result = *observed
	} else {
		result.Publication = "unknown"
		result.Reconciliation = "required"
	}
	var publication *state.PublicationError
	if errors.As(effectErr, &publication) {
		if publication.Committed {
			result.Publication = "committed"
		} else if publication.Unknown {
			result.Publication = "unknown"
		}
		result.Reconciliation = "required"
		result.ErrorCode = "publication_reconciliation_required"
	} else if readErr != nil {
		result.Publication = "unknown"
		result.Reconciliation = "required"
		result.ErrorCode = "authority_read_failed"
	} else if effectErr != nil {
		// An ordinary precommit failure may be reported as not_committed only
		// when the observer proves the unchanged baseline. Observe owns that proof.
		result.ErrorCode = "execution_failed"
	} else if observed == nil {
		result.ErrorCode = "completion_unproven"
	}
	completed := result.Publication == "committed" && result.Reconciliation == "complete"
	// Even a clean precommit failure is kept as debt until the observer proves
	// no effects remain; retries cannot infer safe re-preparation from an error.
	if err = a.Store.Save(recovery, Receipt{Request: r, Evidence: pending.Evidence, Result: result, Completed: completed}); err != nil {
		result.Reconciliation = "required"
		result.ErrorCode = "journal_write_failed"
		if result.Publication == "committed" || result.Publication == "unknown" {
			return result, &state.PublicationError{Committed: result.Publication == "committed", Unknown: result.Publication == "unknown", Err: errors.Join(effectErr, readErr, err)}
		}
		return result, errors.Join(effectErr, readErr, err)
	}
	if effectErr != nil || readErr != nil {
		return result, errors.Join(effectErr, readErr)
	}
	if !completed {
		return result, ErrDebt
	}
	return result, nil
}
func sameIdentity(a, b Request) bool {
	a.ExpectedPreviewUpdatedAt = nil
	b.ExpectedPreviewUpdatedAt = nil
	return reflect.DeepEqual(a, b)
}
func ValidateReceipt(r Receipt) error {
	if err := r.Request.Validate(); err != nil {
		return err
	}
	if r.Result.SchemaVersion != 1 || r.Result.OperationKey != r.Request.OperationKey {
		return fmt.Errorf("invalid receipt result identity")
	}
	switch r.Result.Publication {
	case "not_started", "not_committed", "committed", "unknown":
	default:
		return fmt.Errorf("invalid receipt publication")
	}
	if r.Result.Reconciliation != "required" && r.Result.Reconciliation != "complete" {
		return fmt.Errorf("invalid receipt reconciliation")
	}
	if r.Completed && (r.Result.Publication != "committed" || r.Result.Reconciliation != "complete" || r.Result.AuthorityObservedAt == nil) {
		return fmt.Errorf("unproven completed receipt")
	}
	return nil
}
func EncodeReceipt(r Receipt) ([]byte, error) {
	if err := ValidateReceipt(r); err != nil {
		return nil, err
	}
	return json.Marshal(r)
}
