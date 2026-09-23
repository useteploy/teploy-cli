package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/state"
)

func TestComputePlanID_StableAndSensitive(t *testing.T) {
	base := &PlanRecord{
		SchemaVersion: PlanRecordSchemaVersion,
		App:           "blog", Server: "srv.example.com", User: "root",
		TargetVersion: "abc1234", ConfigDigest: "digest-1",
		TargetState: PlanTargetState{Deployed: true, Generation: 3, CurrentHash: "old9999"},
	}
	want := computePlanID(base)
	if want == "" || len(want) != 16 {
		t.Fatalf("plan id %q is not 16 hex chars", want)
	}
	// Retry-stable: no time or attempt data participates.
	if got := computePlanID(base); got != want {
		t.Errorf("plan id not stable: %s vs %s", got, want)
	}
	// Every binding input moves it.
	mutations := map[string]func(*PlanRecord){
		"app":         func(r *PlanRecord) { r.App = "other" },
		"server":      func(r *PlanRecord) { r.Server = "other.example.com" },
		"user":        func(r *PlanRecord) { r.User = "deploy" },
		"destination": func(r *PlanRecord) { r.Destination = "staging" },
		"version":     func(r *PlanRecord) { r.TargetVersion = "def9876" },
		"config":      func(r *PlanRecord) { r.ConfigDigest = "digest-2" },
		"inputs":      func(r *PlanRecord) { r.Image.ContextFingerprint = "fp1" },
		"dockerfile":  func(r *PlanRecord) { r.Image.DockerfileSHA256 = "df1" },
		"generation":  func(r *PlanRecord) { r.TargetState.Generation = 4 },
		"hash":        func(r *PlanRecord) { r.TargetState.CurrentHash = "new1111" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			clone := *base
			mutate(&clone)
			if got := computePlanID(&clone); got == want {
				t.Errorf("plan id insensitive to %s", name)
			}
		})
	}
}

func TestSaveAndLoadPlanFile_RoundTrip(t *testing.T) {
	rec := &PlanRecord{
		SchemaVersion: PlanRecordSchemaVersion,
		App:           "blog", Server: "srv.example.com",
		TargetVersion: "abc1234", VersionKnown: true, ConfigDigest: "d1",
		Effects: PlanEffects{Containers: []planChange{{Action: "create", Name: "blog-web-abc1234"}}},
	}
	rec.PlanID = computePlanID(rec)

	path := filepath.Join(t.TempDir(), "plans", "plan.json")
	if err := savePlanFile(path, rec); err != nil {
		t.Fatalf("save: %v", err)
	}
	loaded, err := loadPlanFile(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.PlanID != rec.PlanID || loaded.App != rec.App || loaded.ConfigDigest != rec.ConfigDigest {
		t.Errorf("round trip lost identity: %+v", loaded)
	}
	if loaded.Effects.Containers[0].Name != "blog-web-abc1234" {
		t.Errorf("effects lost: %+v", loaded.Effects)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("plan file mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestLoadPlanFile_RefusesTampered(t *testing.T) {
	rec := &PlanRecord{
		SchemaVersion: PlanRecordSchemaVersion,
		App:           "blog", Server: "srv",
		TargetVersion: "v1", ConfigDigest: "d1",
	}
	rec.PlanID = computePlanID(rec)

	// Tamper: change a binding input without recomputing the id.
	tampered := *rec
	tampered.ConfigDigest = "d2"
	path := filepath.Join(t.TempDir(), "plan.json")
	if err := savePlanFile(path, &tampered); err != nil {
		t.Fatal(err)
	}
	if _, err := loadPlanFile(path); err == nil || !strings.Contains(err.Error(), "not self-consistent") {
		t.Errorf("tampered plan accepted: %v", err)
	}

	// Missing id.
	noID := *rec
	noID.PlanID = ""
	path2 := filepath.Join(t.TempDir(), "plan2.json")
	if err := savePlanFile(path2, &noID); err != nil {
		t.Fatal(err)
	}
	if _, err := loadPlanFile(path2); err == nil || !strings.Contains(err.Error(), "no plan id") {
		t.Errorf("id-less plan accepted: %v", err)
	}

	// Wrong schema version.
	future := *rec
	future.SchemaVersion = PlanRecordSchemaVersion + 1
	path3 := filepath.Join(t.TempDir(), "plan3.json")
	if err := savePlanFile(path3, &future); err != nil {
		t.Fatal(err)
	}
	if _, err := loadPlanFile(path3); err == nil || !strings.Contains(err.Error(), "schema version") {
		t.Errorf("future-schema plan accepted: %v", err)
	}
}

// facts builds the current-world view for binding tests.
func facts(from *PlanRecord, mutate func(*planCurrentFacts)) planCurrentFacts {
	cur := planCurrentFacts{
		App:          from.App,
		Server:       from.Server,
		Version:      from.TargetVersion,
		ConfigDigest: from.ConfigDigest,
	}
	if from.TargetState.Deployed {
		cur.State = &state.AppState{Generation: from.TargetState.Generation, CurrentHash: from.TargetState.CurrentHash}
	}
	if mutate != nil {
		mutate(&cur)
	}
	return cur
}

func TestVerifyPlanBinding_NothingMoved(t *testing.T) {
	rec := &PlanRecord{
		SchemaVersion: PlanRecordSchemaVersion,
		App:           "blog", Server: "srv",
		TargetVersion: "abc1234", ConfigDigest: "d1",
		TargetState: PlanTargetState{Deployed: true, Generation: 4, CurrentHash: "old1"},
	}
	rec.PlanID = computePlanID(rec)
	if err := verifyPlanBinding(rec, facts(rec, nil)); err != nil {
		t.Fatalf("stable world must verify, got %v", err)
	}
	// First-deploy plans verify against absent state.
	first := &PlanRecord{App: "blog", Server: "srv", TargetVersion: "v1", ConfigDigest: "d1"}
	first.PlanID = computePlanID(first)
	if err := verifyPlanBinding(first, facts(first, nil)); err != nil {
		t.Fatalf("first-deploy plan must verify against no state: %v", err)
	}
}

func TestVerifyPlanBinding_ConfigDrift(t *testing.T) {
	rec := &PlanRecord{App: "blog", Server: "srv", TargetVersion: "v1", ConfigDigest: "planned"}
	rec.PlanID = computePlanID(rec)
	err := verifyPlanBinding(rec, facts(rec, func(c *planCurrentFacts) { c.ConfigDigest = "changed" }))
	if !errors.Is(err, errPlanDrift) {
		t.Fatalf("config drift must be a plan-drift refusal, got %v", err)
	}
	drift := &planDriftError{}
	if !errors.As(err, &drift) || drift.Kind != "config" {
		t.Errorf("drift kind = %+v, want config", err)
	}
	if !strings.Contains(err.Error(), "re-plan") {
		t.Errorf("refusal must name the remedy: %v", err)
	}
}

func TestVerifyPlanBinding_TargetVersionDrift(t *testing.T) {
	rec := &PlanRecord{App: "blog", Server: "srv", TargetVersion: "abc1234", ConfigDigest: "d"}
	rec.PlanID = computePlanID(rec)
	err := verifyPlanBinding(rec, facts(rec, func(c *planCurrentFacts) { c.Version = "def9876" }))
	var drift *planDriftError
	if !errors.As(err, &drift) || drift.Kind != "target-version" {
		t.Fatalf("expected target-version drift, got %v", err)
	}
	// An explicit --version is binding as-is: no re-derivation mismatch.
	explicit := *rec
	explicit.VersionExplicit = true
	if err := verifyPlanBinding(&explicit, facts(rec, func(c *planCurrentFacts) { c.Version = "def9876" })); err != nil {
		t.Errorf("explicit version must not be re-derived: %v", err)
	}
}

func TestVerifyPlanBinding_BuildInputDrift(t *testing.T) {
	rec := &PlanRecord{
		App: "blog", Server: "srv", TargetVersion: "abc1234", ConfigDigest: "d",
		Image: PlanImageIdentity{NeedsBuild: true, ContextFingerprint: "fp-planned", DockerfileSHA256: "df-planned"},
	}
	rec.PlanID = computePlanID(rec)

	err := verifyPlanBinding(rec, facts(rec, func(c *planCurrentFacts) { c.ContextFingerprint = "fp-now" }))
	var drift *planDriftError
	if !errors.As(err, &drift) || drift.Kind != "build-inputs" {
		t.Fatalf("expected build-inputs drift, got %v", err)
	}
	if !strings.Contains(err.Error(), "fp-planned") || !strings.Contains(err.Error(), "fp-now") {
		t.Errorf("refusal must name both fingerprints: %v", err)
	}

	err = verifyPlanBinding(rec, facts(rec, func(c *planCurrentFacts) { c.DockerfileSHA256 = "df-now" }))
	if !errors.As(err, &drift) || drift.Kind != "build-inputs" {
		t.Fatalf("expected dockerfile drift, got %v", err)
	}
}

func TestVerifyPlanBinding_TargetStateDrift(t *testing.T) {
	rec := &PlanRecord{
		App: "blog", Server: "srv", TargetVersion: "v1", ConfigDigest: "d",
		TargetState: PlanTargetState{Deployed: true, Generation: 4, CurrentHash: "old1"},
	}
	rec.PlanID = computePlanID(rec)

	// A deploy happened in between: generation moved.
	err := verifyPlanBinding(rec, facts(rec, func(c *planCurrentFacts) { c.State.Generation = 5 }))
	var drift *planDriftError
	if !errors.As(err, &drift) || drift.Kind != "target-state" {
		t.Fatalf("generation move must be target-state drift, got %v", err)
	}
	if !strings.Contains(err.Error(), "4 -> 5") {
		t.Errorf("refusal must name the generation move: %v", err)
	}

	// App removed since the plan.
	err = verifyPlanBinding(rec, facts(rec, func(c *planCurrentFacts) { c.State = nil }))
	if !errors.As(err, &drift) || drift.Kind != "target-state" {
		t.Fatalf("removed app must be target-state drift, got %v", err)
	}

	// Deployed since a first-deploy plan.
	first := &PlanRecord{App: "blog", Server: "srv", TargetVersion: "v1", ConfigDigest: "d"}
	first.PlanID = computePlanID(first)
	err = verifyPlanBinding(first, facts(first, func(c *planCurrentFacts) {
		c.State = &state.AppState{Generation: 1, CurrentHash: "v0"}
	}))
	if !errors.As(err, &drift) || drift.Kind != "target-state" {
		t.Fatalf("deployed-since-plan must be target-state drift, got %v", err)
	}
}

func TestVerifyPlanBinding_IdentityDrift(t *testing.T) {
	rec := &PlanRecord{App: "blog", Server: "srv-a", TargetVersion: "v1", ConfigDigest: "d"}
	rec.PlanID = computePlanID(rec)
	err := verifyPlanBinding(rec, facts(rec, func(c *planCurrentFacts) { c.Server = "srv-b" }))
	var drift *planDriftError
	if !errors.As(err, &drift) || drift.Kind != "identity" {
		t.Fatalf("server change must be identity drift, got %v", err)
	}
	err = verifyPlanBinding(rec, facts(rec, func(c *planCurrentFacts) { c.App = "other" }))
	if !errors.As(err, &drift) || drift.Kind != "identity" {
		t.Fatalf("app change must be identity drift, got %v", err)
	}
}
