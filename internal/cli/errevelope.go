package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/sandbox"
)

// Structured error envelope for machine mode (X02 §2.3, S3-lite wiring).
//
// When a command fails under --json output mode, the error is reported as
// one JSON document on STDERR — the stream a machine reader separates
// from the data channel — while stdout keeps carrying only successful
// output. Exit codes stay 0/1/2 exactly (D10): the envelope is the
// detail channel, not a new exit signal. Without --json the historical
// plain-text stderr line is unchanged. One command is the exception, by
// review: `teploy sandbox exec` exits with the command's own status and
// with 255 for a failure of its own, since a caller that runs a command
// wants its status (exitStatus, root.go); its envelope is as everywhere.

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
	// remedy. Migrated for the sandbox engine only (not installed on the
	// server, its service not running, without the driver asked for, or
	// with no hash pinned in this CLI build for the release `sandbox host`
	// would install); every other site stays internal until its migration.
	codeUnsupported = "unsupported"
	// Idempotency/ambiguity refusal — e.g. the ambiguous-preview class.
	// Migrated for a plan/apply drift, a config conflict and a sandbox in
	// the wrong state (the engine's 409); every other site stays internal
	// until its migration.
	codeConflict = "conflict"
	// The effect's fate is unknown pending reconciliation; never
	// rendered or recorded as failure. Wired for interrupted commands
	// (context canceled / deadline exceeded — SIGINT, automation
	// timeouts); deeper per-phase migration stays on the S2 list.
	codeUncertainOutcome = "uncertain-outcome"
	// Success with a flag — traffic switched but the outcome is not
	// clean (UNMIGRATED: currently internal).
	codeDegraded = "degraded"
	// A command run by `teploy sandbox exec` did not end well: it exited
	// nonzero, or its timeout ended it. It is the command's outcome, not a
	// failure of teploy's (those stay `internal` here and exit 255), and
	// teploy exits with the command's own status.
	codeCommandFailed = "command-failed"
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
// machine caller the same way a malformed one is. A plan/apply drift
// refusal (C05) → conflict — the request is coherent, the world moved.
// An interrupted command (context canceled or deadline exceeded — the
// SIGINT path, an automation timeout) → uncertain-outcome: whatever
// effect was in flight has an unknown fate until reconciled (the C01
// crash-window machinery treats every interrupted window as INSPECT),
// which is precisely the class automation must not render as a plain
// failure. A `cloud server create` whose request outcome could not be
// established (errCloudPurchaseUnknown) is the same class, and so is a
// sandbox create, fork or exec whose outcome could not be known
// (errSandboxOutcomeUnknown). A sandbox engine that cannot serve a request
// (not installed, not running, or without the driver asked for), and a CLI
// build with no hash pinned for the engine release it would install, are
// unsupported. Everything else is internal until its site is migrated (S2
// generalization).
func classifyMachineError(err error) string {
	var command *sandboxCommandError
	switch {
	case errors.As(err, &command):
		return codeCommandFailed
	case errors.Is(err, errPlanDrift), errors.Is(err, errConfigConflict),
		sandbox.IsConflict(err), errors.Is(err, errSandboxWrongState):
		return codeConflict
	case errors.Is(err, errSandboxEngineUnavailable):
		return codeUnsupported
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded),
		errors.Is(err, errCloudPurchaseUnknown), errors.Is(err, errSandboxOutcomeUnknown):
		return codeUncertainOutcome
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
	var command *sandboxCommandError
	switch code {
	case codeCommandFailed:
		message = "the command did not end well"
		if errors.As(err, &command) && command.envelopeMessage != "" {
			message = command.envelopeMessage
		}
	case codeConfigInvalid:
		message = "invalid teploy configuration"
		if errors.Is(err, errDeployAdmission) {
			message = "deploy request refused"
		}
	case codeConflict:
		message = "plan no longer valid"
		if errors.Is(err, errConfigConflict) {
			message = "config changed since it was read"
		}
		if sandbox.IsConflict(err) || errors.Is(err, errSandboxWrongState) {
			message = "the sandbox is in the wrong state for this request"
		}
	case codeUnsupported:
		message = "the sandbox engine cannot serve this request on this server"
		if errors.Is(err, errSandboxUnpinned) {
			message = "this CLI build has no pinned hash for the sandbox engine release it would install"
		}
	case codeUncertainOutcome:
		message = "interrupted — outcome unknown until reconciled"
		if errors.Is(err, errCloudPurchaseUnknown) {
			message = "server purchase outcome unknown — check `teploy cloud servers`"
		}
		if errors.Is(err, errSandboxOutcomeUnknown) {
			message = "sandbox request outcome unknown — list the sandboxes before trying again"
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
// Execute does before exiting: the machine envelope under --json, the
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
