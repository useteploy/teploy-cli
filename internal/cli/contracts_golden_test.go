package cli

// X02 S2 contracts corpus generator (contracts/ skeleton + first goldens).
// Regenerates contracts/fixtures/ from the REAL encoders and fails on any
// diff against the committed corpus - the TestPreviewIDGolden discipline
// generalized to the machine interface. Set TEPLOY_UPDATE_CONTRACTS=1 to
// rewrite the corpus after a deliberate contract change, then commit the
// diff together with the code that caused it and bump MANIFEST.md.

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/useteploy/teploy/internal/releasemeta"
)

const contractsDir = "../../contracts"

func writeFixture(t *testing.T, path string, v any) {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		t.Fatalf("encode %s: %v", path, err)
	}
	full := filepath.Join(contractsDir, "fixtures", path)
	if os.Getenv("TEPLOY_UPDATE_CONTRACTS") == "1" {
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", full, err)
		}
		if err := os.WriteFile(full, buf.Bytes(), 0o644); err != nil {
			t.Fatalf("write %s: %v", full, err)
		}
		return
	}
	want, err := os.ReadFile(full)
	if err != nil {
		t.Fatalf("fixture %s missing (run with TEPLOY_UPDATE_CONTRACTS=1 to seed): %v", path, err)
	}
	if !bytes.Equal(bytes.TrimRight(want, "\n"), bytes.TrimRight(buf.Bytes(), "\n")) {
		t.Errorf("fixture %s drifted from the committed corpus - regenerate deliberately (TEPLOY_UPDATE_CONTRACTS=1), commit the diff WITH the code change, and bump contracts/MANIFEST.md", path)
	}
}

// TestContractsVersionHandshakeGolden drives the REAL writeVersion encoder.
func TestContractsVersionHandshakeGolden(t *testing.T) {
	var buf bytes.Buffer
	if err := writeVersion(&buf, "v0.0.0-contracts", true); err != nil {
		t.Fatalf("writeVersion: %v", err)
	}
	var v any
	if err := json.Unmarshal(buf.Bytes(), &v); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	writeFixture(t, "version-handshake/valid/mi1.json", v)
}

// TestContractsAppListEnvelopeGolden emits an appListDTO with one
// representative app through the same json tags the command marshals.
// Offline stand-in: the DTO values are constructed, the ENCODER is real.
func TestContractsAppListEnvelopeGolden(t *testing.T) {
	ts := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	writeFixture(t, "app-list-envelope/valid/mi1.json", appListDTO{
		MachineInterface: MachineInterface,
		Host:             "srv.example.com",
		Apps: []appStatusDTO{{
			App: "myapp", Domain: "myapp.example.com", Type: "container",
			Ingress: "caddy", CurrentRelease: releaseStatusDTO{Version: "3", Ports: []int{3000}},
			Containers: []containerDTO{{ID: "9f31c02", Name: "myapp-web-3", Image: "nginx:1.27", State: "running", Status: "Up 4 minutes", CreatedAt: "2026-09-23T11:55:00Z", Process: "web", Version: "3"}},
			Lock:       nil, ObservedAt: ts, Errors: nil,
		}},
		ObservedAt: ts,
	})

	// Legacy: pre-MI envelope (no machine_interface field) - the shape a
	// v0.1.36-or-older CLI emitted. Consumers must treat missing-MI as
	// legacy, not as MI 0.
	var legacy map[string]any
	raw, err := json.Marshal(appListDTO{
		Host: "srv.example.com", Apps: []appStatusDTO{}, ObservedAt: ts,
	})
	if err != nil {
		t.Fatalf("marshal legacy: %v", err)
	}
	if err := json.Unmarshal(raw, &legacy); err != nil {
		t.Fatalf("unmarshal legacy: %v", err)
	}
	delete(legacy, "machine_interface")
	writeFixture(t, "app-list-envelope/legacy/pre-mi.json", legacy)
}

// TestContractsErrorEnvelopeGolden pins the two wired error classes.
func TestContractsErrorEnvelopeGolden(t *testing.T) {
	writeFixture(t, "error-envelope/valid/config-invalid.json", machineErrorEnvelope{
		MachineInterface: MachineInterface, Code: "config-invalid",
		Message: "invalid teploy configuration", Detail: "teploy.yml: services.0.name: required",
	})
	writeFixture(t, "error-envelope/valid/internal.json", machineErrorEnvelope{
		MachineInterface: MachineInterface, Code: "internal",
		Message: "command failed", Detail: "dial tcp: connection refused",
	})
	// Invalid: a code outside the taxonomy (schema enum must reject).
	writeFixture(t, "error-envelope/invalid/unknown-code.json", machineErrorEnvelope{
		MachineInterface: MachineInterface, Code: "kaboom", Message: "x",
	})
}

// TestContractsReleaseRecordGolden pins the releasemeta Record shape.
func TestContractsReleaseRecordGolden(t *testing.T) {
	ts := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	writeFixture(t, "release-record/valid/container.json", releasemeta.Record{
		SchemaVersion: 1, App: "myapp", Hash: "abc1234.deadb17ecafef00d", CreatedAt: ts,
		DeploymentType: "container", IngressMode: "caddy", Domain: "myapp.example.com",
		ImageRef: "nginx:1.27", ImageDigest: "sha256:0000", ManifestSHA256: "sha256:beef",
	})
}

// TestContractsAttemptNameGolden pins the attempt-name grammar class via
// representative strings (the schema pattern is the contract; these are the
// examples a consumer tests against).
func TestContractsAttemptNameGolden(t *testing.T) {
	writeFixture(t, "attempt-name/valid/examples.json", []string{
		"abc1234.deadb17ecafef00d",
		"9f31c02.0123456789abcdef",
	})
	writeFixture(t, "attempt-name/invalid/examples.json", []string{
		"deadb17ecafef00d",        // missing the hash half
		"ABC1234.deadb17ecafef00d", // uppercase
		"abc1234.DeadB17eCafef00d", // uppercase hex half
		"abc1234.deadb17ecafef00",  // 15 hex chars
		"../escape.attempt0000000", // path characters
	})
}
