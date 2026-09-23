package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	"github.com/useteploy/teploy/internal/config"
)

// Structured error envelope for machine mode (X02 §2.3, S3-lite wiring).
//
// When a command fails under --json output mode, the error is reported as
// one JSON document on STDERR — the stream a machine reader separates
// from the data channel — while stdout keeps carrying only successful
// output. Exit codes stay 0/1/2 exactly (D10): the envelope is the
// detail channel, not a new exit signal. Without --json the historical
// plain-text stderr line is unchanged.

// Machine error codes — closed taxonomy v1 (X02 §2.3). Consumers must
// treat an unknown code as internal (forward-safe). Codes marked
// UNMIGRATED are part of the registry but no error site classifies into
// them yet; their migration is the recorded S2 follow-up list in
// AUDIT_OPEN.md.
const (
	// teploy.yml/TOML/destination/Compose failed to load, merge, or
	// validate — or a deploy request's parameters were refused before
	// any effect (admission).
	codeConfigInvalid = "config-invalid"
	// SSH/transport failure reaching the target (UNMIGRATED: currently
	// internal).
	codeTargetUnreachable = "target-unreachable"
	// The requested verb needs a capability this binary lacks; names the
	// remedy (UNMIGRATED: currently internal).
	codeUnsupported = "unsupported"
	// Idempotency/ambiguity refusal — e.g. the ambiguous-preview class
	// (UNMIGRATED: currently internal).
	codeConflict = "conflict"
	// The effect's fate is unknown pending reconciliation; never
	// rendered or recorded as failure (UNMIGRATED: currently internal).
	codeUncertainOutcome = "uncertain-outcome"
	// Success with a flag — traffic switched but the outcome is not
	// clean (UNMIGRATED: currently internal).
	codeDegraded = "degraded"
	// Everything else, and every failure whose site has not been
	// migrated to a specific code yet.
	codeInternal = "internal"
)

// machineErrorEnvelope is the wire shape: the interface version (so a
// consumer gates decoding the same way as data envelopes), the taxonomy
// code, a stable short message, and the full error text as detail.
type machineErrorEnvelope struct {
	MachineInterface int    `json:"machine_interface"`
	Code             string `json:"code"`
	Message          string `json:"message"`
	Detail           string `json:"detail,omitempty"`
}

// errDeployAdmission marks a deploy refusal issued before any effect:
// invalid request parameters (--app/--image/--domain/--version grammar,
// missing server), or a config the engine refused to load. The wrapped
// error's text and chain are preserved verbatim.
var errDeployAdmission = errors.New("deploy admission refused")

type admissionError struct{ err error }

func (e *admissionError) Error() string { return e.err.Error() }
func (e *admissionError) Unwrap() error { return e.err }
func (e *admissionError) Is(target error) bool {
	return target == errDeployAdmission
}

// refuseAdmission marks err as a pre-effect deploy refusal without
// altering its message.
func refuseAdmission(err error) error {
	if err == nil {
		return nil
	}
	return &admissionError{err: err}
}

// classifyMachineError maps a failed command's error to a taxonomy code.
// Wired classes (this slice): config-load failures and deploy admission
// refusals → config-invalid; an absent config is a config failure for a
// machine caller the same way a malformed one is. Everything else is
// internal until its site is migrated (S2 generalization).
func classifyMachineError(err error) string {
	switch {
	case errors.Is(err, config.ErrInvalidConfig),
		errors.Is(err, config.ErrNoConfig),
		errors.Is(err, errDeployAdmission):
		return codeConfigInvalid
	default:
		return codeInternal
	}
}

// writeMachineErrorEnvelope renders err as the machine error envelope.
func writeMachineErrorEnvelope(out io.Writer, err error) error {
	code := classifyMachineError(err)
	message := "command failed"
	switch code {
	case codeConfigInvalid:
		message = "invalid teploy configuration"
		if errors.Is(err, errDeployAdmission) {
			message = "deploy request refused"
		}
	}
	return json.NewEncoder(out).Encode(machineErrorEnvelope{
		MachineInterface: MachineInterface,
		Code:             code,
		Message:          message,
		Detail:           err.Error(),
	})
}

// reportExecutionError renders a failed root invocation's error the way
// Execute does before exiting 1: the machine envelope under --json, the
// plain error text otherwise. A flag-parse failure never reaches parsed
// --json state and keeps the plain form.
func reportExecutionError(root *cobra.Command, err error, out io.Writer) {
	if jsonMode(root) {
		_ = writeMachineErrorEnvelope(out, err)
		return
	}
	fmt.Fprintln(out, err)
}

// jsonMode reports whether the root invocation parsed --json. Read from
// the flag value (not the bound struct) so Execute can consult it after
// the fact.
func jsonMode(root *cobra.Command) bool {
	value, err := root.PersistentFlags().GetBool("json")
	return err == nil && value
}
