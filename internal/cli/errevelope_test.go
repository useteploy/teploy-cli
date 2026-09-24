package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/config"
)

// driveRoot executes a real cobra invocation and returns its error, so
// envelope tests observe the exact error Execute would report.
func driveRoot(t *testing.T, args ...string) error {
	t.Helper()
	root := NewRootCmd("test")
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	return root.Execute()
}

const invalidModeConfig = "app: demo\ndomain: demo.example.com\nserver: prod\nhealth:\n  mode: bogus\n"

// TestMachineErrorEnvelopeConfigLoad drives a real failing deploy (an
// invalid teploy.yml) and asserts the machine error envelope: emitted on
// the error stream under --json, carrying the interface version and the
// config-invalid code.
func TestMachineErrorEnvelopeConfigLoad(t *testing.T) {
	chdirTemp(t, invalidModeConfig)

	err := driveRoot(t, "deploy", "--json")
	if err == nil {
		t.Fatal("invalid teploy.yml must fail the deploy")
	}
	if !errors.Is(err, config.ErrInvalidConfig) {
		t.Fatalf("config-load error is not classified: %v", err)
	}

	root := NewRootCmd("test")
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"deploy", "--json"})
	if err := root.Execute(); err != nil {
		reportExecutionError(root, err, &out)
	}

	var decoded map[string]any
	if jsonErr := json.Unmarshal(out.Bytes(), &decoded); jsonErr != nil {
		t.Fatalf("no machine error envelope under --json (got %q): %v", out.String(), jsonErr)
	}
	if decoded["machine_interface"] != float64(MachineInterface) {
		t.Fatalf("envelope machine_interface = %v, want %d", decoded["machine_interface"], MachineInterface)
	}
	if decoded["code"] != "config-invalid" {
		t.Fatalf("envelope code = %v, want config-invalid", decoded["code"])
	}
	if decoded["message"] != "invalid teploy configuration" {
		t.Fatalf("envelope message = %v", decoded["message"])
	}
	detail, _ := decoded["detail"].(string)
	if detail == "" || !strings.Contains(detail, "teploy.yml") {
		t.Fatalf("envelope detail must name the config: %q", detail)
	}
}

// TestMachineErrorEnvelopeAdmission drives the ad-hoc deploy path (the
// dash-hit admission surface) with an invalid app name and asserts the
// refusal classifies as config-invalid with the admission message.
func TestMachineErrorEnvelopeAdmission(t *testing.T) {
	err := driveRoot(t, "deploy", "prod", "--app", "bad app", "--image", "nginx:1", "--json")
	if err == nil {
		t.Fatal("invalid ad-hoc app name must be refused")
	}
	if !errors.Is(err, errDeployAdmission) {
		t.Fatalf("admission refusal is not marked: %v", err)
	}

	root := NewRootCmd("test")
	var out bytes.Buffer
	root.SetArgs([]string{"deploy", "prod", "--app", "bad app", "--image", "nginx:1", "--json"})
	if err := root.Execute(); err != nil {
		reportExecutionError(root, err, &out)
	}

	var decoded map[string]any
	if jsonErr := json.Unmarshal(out.Bytes(), &decoded); jsonErr != nil {
		t.Fatalf("no machine error envelope under --json (got %q): %v", out.String(), jsonErr)
	}
	if decoded["code"] != "config-invalid" {
		t.Fatalf("envelope code = %v, want config-invalid", decoded["code"])
	}
	if decoded["message"] != "deploy request refused" {
		t.Fatalf("envelope message = %v, want \"deploy request refused\"", decoded["message"])
	}
	detail, _ := decoded["detail"].(string)
	if detail == "" || !strings.Contains(detail, "app") {
		t.Fatalf("envelope detail must carry the refusal reason: %q", detail)
	}
}

// TestMachineErrorEnvelopeAbsentWithoutJSON: the same failure without
// --json keeps the historical plain-text stderr line — no envelope, no
// JSON, byte-identical message.
func TestMachineErrorEnvelopeAbsentWithoutJSON(t *testing.T) {
	chdirTemp(t, invalidModeConfig)

	err := driveRoot(t, "deploy")
	if err == nil {
		t.Fatal("invalid teploy.yml must fail the deploy")
	}

	root := NewRootCmd("test")
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"deploy"})
	execErr := root.Execute()
	if execErr == nil {
		t.Fatal("invalid teploy.yml must fail the deploy")
	}
	reportExecutionError(root, execErr, &out)
	if out.String() != execErr.Error()+"\n" {
		t.Fatalf("human error output = %q, want the plain error %q", out.String(), execErr.Error())
	}
	if strings.Contains(out.String(), "machine_interface") {
		t.Fatalf("envelope leaked into non-JSON output: %q", out.String())
	}
}

// TestMachineErrorEnvelopeUnclassifiedIsInternal: errors outside the
// wired classes classify as internal — the forward-safe default.
func TestMachineErrorEnvelopeUnclassifiedIsInternal(t *testing.T) {
	var out bytes.Buffer
	if err := writeMachineErrorEnvelope(&out, errors.New("dial tcp 192.0.2.10:22: i/o timeout")); err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("envelope not JSON: %q", out.String())
	}
	if decoded["code"] != "internal" {
		t.Fatalf("unclassified code = %v, want internal", decoded["code"])
	}
	if decoded["message"] != "command failed" {
		t.Fatalf("unclassified message = %v", decoded["message"])
	}
}

// TestMachineErrorEnvelopeInterruptedIsUncertain: an interrupted command
// (context canceled — the SIGINT path — or a deadline exceeded) is NOT a
// plain failure: its outcome is unknown until reconciled (C09's
// uncertain/canceled distinction for automation). Wrapped and chained
// errors must classify the same way.
func TestMachineErrorEnvelopeInterruptedIsUncertain(t *testing.T) {
	for name, err := range map[string]error{
		"canceled":       context.Canceled,
		"deadline":       context.DeadlineExceeded,
		"wrapped":        fmt.Errorf("deploying myapp: %w", context.Canceled),
		"double-wrapped": fmt.Errorf("running docker run: %w", fmt.Errorf("build: %w", context.DeadlineExceeded)),
	} {
		var out bytes.Buffer
		if writeErr := writeMachineErrorEnvelope(&out, err); writeErr != nil {
			t.Fatal(writeErr)
		}
		var decoded map[string]any
		if jsonErr := json.Unmarshal(out.Bytes(), &decoded); jsonErr != nil {
			t.Fatalf("%s: envelope not JSON: %q", name, out.String())
		}
		if decoded["code"] != "uncertain-outcome" {
			t.Fatalf("%s: code = %v, want uncertain-outcome", name, decoded["code"])
		}
		if decoded["message"] != "interrupted — outcome unknown until reconciled" {
			t.Fatalf("%s: message = %v", name, decoded["message"])
		}
	}
}

// TestDeployInterruptedErrorNamesRecovery: the human-path error for an
// interrupted deploy names the recovery action instead of a bare
// "context canceled" (which reads as a clean failure), while a plain
// deploy failure passes through unchanged.
func TestDeployInterruptedErrorNamesRecovery(t *testing.T) {
	interrupted := wrapDeployOutcomeError(fmt.Errorf("deploying myapp: %w", context.Canceled))
	msg := interrupted.Error()
	if !strings.Contains(msg, "deploy interrupted") || !strings.Contains(msg, "reconcil") || !strings.Contains(msg, "teploy status") {
		t.Fatalf("interrupted deploy error must name the recovery action: %q", msg)
	}
	var out bytes.Buffer
	if err := writeMachineErrorEnvelope(&out, interrupted); err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("envelope not JSON: %q", out.String())
	}
	if decoded["code"] != "uncertain-outcome" {
		t.Fatalf("interrupted deploy code = %v, want uncertain-outcome", decoded["code"])
	}

	plain := errors.New("image pull failed")
	if got := wrapDeployOutcomeError(plain); got != plain {
		t.Fatalf("plain failure must pass through unchanged: %v", got)
	}
}

// TestExitCodesPinned: 0/1/2 semantics are unchanged by the envelope —
// drift's 2 remains a signal (not a failure) gated on --exit-code, and
// every other failure exits 1 after the error is reported.
func TestExitCodesPinned(t *testing.T) {
	if got := driftExitCode(true, true); got != 2 {
		t.Fatalf("driftExitCode(true,true) = %d, want 2", got)
	}
	if got := driftExitCode(true, false); got != 0 {
		t.Fatalf("driftExitCode(true,false) = %d, want 0", got)
	}
	if got := driftExitCode(false, true); got != 0 {
		t.Fatalf("driftExitCode(false,true) = %d, want 0", got)
	}
}
