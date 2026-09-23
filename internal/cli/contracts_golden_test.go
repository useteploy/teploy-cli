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
	"strings"
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
		Errors:           []machineError{},
		Apps: []appStatusDTO{{
			App: "myapp", Domain: "myapp.example.com", Type: "container",
			Ingress: "caddy", CurrentRelease: releaseStatusDTO{Version: "3", Ports: []int{3000}},
			PreviousRelease: releaseStatusDTO{Version: "2", Ports: []int{3000}},
			Containers: []containerDTO{{ID: "9f31c02", Name: "myapp-web-3", Image: "nginx:1.27", State: "running", Status: "Up 4 minutes", CreatedAt: "2026-09-23T11:55:00Z", Process: "web", Version: "3"}},
			Processes:  []processDTO{},
			Lock:       nil, ObservedAt: ts, Errors: []machineError{},
		}},
		ObservedAt: ts,
	})

	// Legacy: pre-MI envelope (no machine_interface field) - the shape a
	// v0.1.36-or-older CLI emitted. Consumers must treat missing-MI as
	// legacy, not as MI 0.
	var legacy map[string]any
	raw, err := json.Marshal(appListDTO{
		Host: "srv.example.com", Apps: []appStatusDTO{}, ObservedAt: ts, Errors: []machineError{},
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

// TestContractsPlanRecordGolden pins the C05 plan-record shape through
// the REAL record construction: plan id computed by computePlanID,
// marshaled with the PlanRecord's own tags (the same encoder savePlanFile
// uses). Two representative records: a build plan (image
// unresolved-awaiting-build, bound by build inputs) and a prebuilt
// digest-pinned plan (resolved-by-digest) — the known-vs-unresolved
// classification a consumer reads before trusting an apply.
func TestContractsPlanRecordGolden(t *testing.T) {
	buildPlan := &PlanRecord{
		SchemaVersion: PlanRecordSchemaVersion,
		App:           "myapp",
		Server:        "srv.example.com",
		User:          "root",
		ServerName:    "prod",
		TargetVersion: "abc1234",
		VersionKnown:  true,
		ConfigDigest:  "3f2a9c11d8e4b7065a1c9f0e2b8d7a64c5e3f1b9a0d8c7e6f5a4b3c2d1e0f9a8",
		Image: PlanImageIdentity{
			NeedsBuild:         true,
			Resolution:         imageUnresolvedAwaitingBuild,
			ContextPath:        ".",
			ContextFingerprint: "c0ffee11aa22bb33",
			Dockerfile:         "Dockerfile",
			DockerfileSHA256:   "deadbeef11",
			Platform:           "linux/amd64",
		},
		TargetState: PlanTargetState{Deployed: true, Generation: 4, CurrentHash: "old1234", ManifestSHA256: "aa11bb22"},
		Effects: PlanEffects{
			Containers: []planChange{{Action: "create", Name: "myapp-web-abc1234", Detail: "web container"}},
			Routing:    []planEffect{{Action: "change", Name: "domain", From: "old.example.com", To: "new.example.com", Detail: "routes served by this deployment"}},
		},
		Unresolved: []string{"image unresolved — built at deploy time; the plan binds the build inputs (context . fingerprint c0ffee11aa22bb..., Dockerfile Dockerfile sha deadbeef11...)"},
	}
	buildPlan.PlanID = computePlanID(buildPlan)
	writeFixture(t, "plan-record/valid/build.json", buildPlan)

	digest := "sha256:" + strings.Repeat("ab", 32)
	digestPlan := &PlanRecord{
		SchemaVersion: PlanRecordSchemaVersion,
		App:           "myapp",
		Server:        "srv.example.com",
		ServerName:    "prod",
		TargetVersion: "sha256-aaaaaaaaaaaa",
		VersionKnown:  true,
		ConfigDigest:  "7d1e0f9a8b7c6d5e4f3a2b1c0d9e8f7a6b5c4d3e2f1a0b9c8d7e6f5a4b3c2d10",
		Image: PlanImageIdentity{
			Ref:        "registry.example.com/myapp@" + digest,
			Resolution: imageResolvedByDigest,
			Digest:     digest,
		},
		TargetState: PlanTargetState{Deployed: false},
		Effects:     PlanEffects{Containers: []planChange{{Action: "create", Name: "myapp-web-sha256-aaaaaaaaaaaa", Detail: "web container"}}},
	}
	digestPlan.PlanID = computePlanID(digestPlan)
	writeFixture(t, "plan-record/valid/prebuilt-digest.json", digestPlan)

	// Invalid: a record whose plan id does not recompute from its
	// identity — loadPlanFile refuses it (the schema's pattern cannot
	// see inside the hash, so this fixture pins the REFUSAL, not just
	// the shape).
	tampered := *digestPlan
	tampered.ConfigDigest = "0000000000000000000000000000000000000000000000000000000000000000"
	writeFixture(t, "plan-record/invalid/tampered-id.json", tampered)
}
