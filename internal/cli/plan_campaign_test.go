package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/ssh"
)

func TestLockedPlanRejectsInterveningGeneration(t *testing.T) {
	cfg := &config.AppConfig{App: "demo", Image: "image:v1"}
	digest, err := config.ExecutionBindingDigest(cfg, cfg.Image)
	if err != nil {
		t.Fatal(err)
	}
	rec := &PlanRecord{App: cfg.App, Server: "host", ConfigDigest: digest, TargetVersion: "v1", VersionExplicit: true, TargetState: PlanTargetState{Deployed: true, Generation: 4}}
	mock := ssh.NewMockExecutor("host")
	mock.Files["/deployments/demo/state.json"] = []byte(`{"schema_version":2,"generation":5,"current_hash":"v2"}`)
	err = revalidateLockedPlan(context.Background(), mock, cfg, cfg.Image, "v1", rec)
	if !errors.Is(err, errPlanDrift) {
		t.Fatalf("stale reviewed generation accepted: %v", err)
	}
	for _, call := range mock.Calls {
		if call == "docker run" {
			t.Fatal("unexpected effect")
		}
	}
}

func TestStaticJSONPlanWritesAndBindsInputs(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.WriteFile("index.html", []byte("safe"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.AppConfig{App: "demo", Type: config.TypeStatic, Source: ".", Domain: "example.test"}
	mock := ssh.NewMockExecutor("host")
	// JSON is a presentation mode: the reviewed record must still be saved.
	original := os.Stdout
	file, err := os.Create(filepath.Join(t.TempDir(), "stdout"))
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = file
	defer func() { os.Stdout = original; file.Close() }()
	if err := planStatic(context.Background(), &Flags{JSON: true}, cfg, mock, "", "plans/plan.json"); err != nil {
		t.Fatal(err)
	}
	rec, err := loadPlanFile("plans/plan.json")
	if err != nil {
		t.Fatal(err)
	}
	fp, err := staticPlanFingerprint(rec.InputExclusions, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if fp == "" || fp != rec.Image.ContextFingerprint {
		t.Fatal("unchanged static inputs falsely drifted")
	}
	if err := os.WriteFile("index.html", []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	after, _ := staticPlanFingerprint(rec.InputExclusions, cfg)
	if after == fp {
		t.Fatal("static content change was not bound")
	}
	file.Sync()
	file.Seek(0, io.SeekStart)
	var result map[string]any
	if err := json.NewDecoder(file).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result["plan_id"] != rec.PlanID {
		t.Fatal("saved and displayed identity differ")
	}
}

func TestReviewedInputSnapshotIsIndependentAndMatchesAdmission(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	gitRun(t, dir, "init", "-q")
	os.WriteFile(".gitignore", []byte("private.txt\n"), 0600)
	os.WriteFile("private.txt", []byte("dummy confidential marker"), 0600)
	os.WriteFile("Dockerfile", []byte("FROM scratch\nCOPY app /app\n"), 0600)
	os.WriteFile("app", []byte("safe"), 0755)
	os.Mkdir("assets", 0755)
	os.WriteFile("assets/icon", []byte("asset"), 0644)
	cfg := &config.AppConfig{App: "demo"}
	before, err := staticPlanFingerprint(nil)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := freezePlanInputs(nil, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(filepath.Dir(snapshot))
	if _, err := os.Stat(filepath.Join(snapshot, "private.txt")); !os.IsNotExist(err) {
		t.Fatal("ignored private file copied")
	}
	os.WriteFile("app", []byte("changed after verification"), 0755)
	if err := os.Chdir(snapshot); err != nil {
		t.Fatal(err)
	}
	after, err := staticPlanFingerprint(nil)
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatal("snapshot does not preserve admitted input identity")
	}
	info, _ := os.Stat("assets")
	if info.Mode().Perm() != 0755 {
		t.Fatalf("image directory permissions changed: %o", info.Mode().Perm())
	}
	got, _ := os.ReadFile("app")
	if string(got) != "safe" {
		t.Fatal("source mutation changed reviewed bytes")
	}
}

func TestStaticReviewedSourceIncludesIgnoredDist(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	gitRun(t, dir, "init", "-q")
	os.WriteFile(".gitignore", []byte("dist/\n"), 0600)
	os.Mkdir("dist", 0755)
	os.WriteFile("dist/index.html", []byte("safe"), 0644)
	cfg := &config.AppConfig{App: "demo", Type: config.TypeStatic, Source: "dist"}
	before, err := staticPlanFingerprint(nil, cfg)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := freezePlanInputs(nil, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(filepath.Dir(snapshot))
	os.WriteFile("dist/index.html", []byte("changed"), 0644)
	changed, _ := staticPlanFingerprint(nil, cfg)
	if changed == before {
		t.Fatal("ignored serving source not bound")
	}
	os.Chdir(snapshot)
	frozen, err := staticPlanFingerprint(nil, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if frozen != before {
		t.Fatal("ignored serving source not frozen")
	}
}
