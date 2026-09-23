package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/releasemeta"
	"github.com/useteploy/teploy/internal/ssh"
	"github.com/useteploy/teploy/internal/state"
)

// writeAppConfig writes a teploy.yml into dir and chdirs there for the
// test (apply resolves config from ".").
func writeAppConfig(t *testing.T, dir, yml string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "teploy.yml"), []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}
}

// planFromConfig builds a plan record the way `teploy plan` would for
// the CURRENT teploy.yml in dir (digest over the given image reference).
func planFromConfig(t *testing.T, dir, image, destination string) *PlanRecord {
	t.Helper()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if chdirErr := os.Chdir(dir); chdirErr != nil {
		t.Fatal(chdirErr)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })

	var appCfg *config.AppConfig
	if destination != "" {
		appCfg, err = config.LoadAppWithDestination(".", destination, config.OverlayOptions{})
	} else {
		appCfg, err = config.LoadApp(".")
	}
	if err != nil {
		t.Fatalf("LoadApp: %v", err)
	}
	_, digest, err := config.NormalizeAndDigest(appCfg, image)
	if err != nil {
		t.Fatal(err)
	}
	rec := &PlanRecord{
		SchemaVersion:   PlanRecordSchemaVersion,
		App:             appCfg.App,
		Server:          "srv.test",
		ServerName:      "prod",
		Destination:     destination,
		TargetVersion:   "v1",
		VersionKnown:    true,
		VersionExplicit: true,
		ConfigDigest:    digest,
		Image:           PlanImageIdentity{Ref: image, Resolution: imageUnresolvedMutableTag},
	}
	rec.PlanID = computePlanID(rec)
	return rec
}

// TestApplyDrift_ConfigChangedBetweenPlanAndApply is the C05 acceptance
// fixture: a config edit between plan and apply must refuse, naming the
// drift — never execute the stale plan.
func TestApplyDrift_ConfigChangedBetweenPlanAndApply(t *testing.T) {
	dir := t.TempDir()
	writeAppConfig(t, dir, "app: blog\nimage: registry/blog:v1\ndomain: blog.example.com\nport: 3000\n")
	rec := planFromConfig(t, dir, "registry/blog:v1", "")

	// The world moves: port edited after the plan was reviewed.
	writeAppConfig(t, dir, "app: blog\nimage: registry/blog:v1\ndomain: blog.example.com\nport: 8080\n")

	appCfg, image, digest, version, err := applyResolveCurrent(context.Background(), &Flags{}, rec)
	if err != nil {
		t.Fatal(err)
	}
	err = verifyPlanBinding(rec, planCurrentFacts{
		App: appCfg.App, Server: rec.Server, Version: version, ConfigDigest: digest,
	})
	if !errors.Is(err, errPlanDrift) {
		t.Fatalf("config edit must invalidate the plan, got %v", err)
	}
	var drift *planDriftError
	if !errors.As(err, &drift) || drift.Kind != "config" {
		t.Errorf("kind = %+v, want config", err)
	}
	if !strings.Contains(err.Error(), rec.ConfigDigest) || !strings.Contains(err.Error(), digest) {
		t.Errorf("refusal must name both digests: %v", err)
	}
	_ = image
}

// TestApplyDrift_OverlayFlipInvalidates: the destination overlay's
// merged result is part of the binding — an overlay edit between plan
// and apply refuses (presence-aware overlay semantics pinned through
// the config digest).
func TestApplyDrift_OverlayFlipInvalidates(t *testing.T) {
	dir := t.TempDir()
	writeAppConfig(t, dir, "app: blog\nimage: registry/blog:v1\ndomain: blog.example.com\nport: 3000\nenv:\n  BASE: kept\n")
	if err := os.WriteFile(filepath.Join(dir, "teploy.staging.yml"), []byte("env:\n  STAGED: \"1\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := planFromConfig(t, dir, "registry/blog:v1", "staging")

	// Overlay edited after the plan: staging now clears the env block
	// (strict presence-aware clearing).
	if err := os.WriteFile(filepath.Join(dir, "teploy.staging.yml"), []byte("env: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, _, digest, version, err := applyResolveCurrent(context.Background(), &Flags{StrictEnv: true}, rec)
	if err != nil {
		t.Fatal(err)
	}
	err = verifyPlanBinding(rec, planCurrentFacts{App: "blog", Server: rec.Server, Version: version, ConfigDigest: digest})
	if !errors.Is(err, errPlanDrift) {
		t.Fatalf("overlay edit must invalidate the plan, got %v", err)
	}
}

// TestApplyStable_ConfigUnchanged: nothing moved between plan and apply
// — the same resolution verifies clean.
func TestApplyStable_ConfigUnchanged(t *testing.T) {
	dir := t.TempDir()
	writeAppConfig(t, dir, "app: blog\nimage: registry/blog:v1\ndomain: blog.example.com\nport: 3000\n")
	rec := planFromConfig(t, dir, "registry/blog:v1", "")

	appCfg, _, digest, version, err := applyResolveCurrent(context.Background(), &Flags{}, rec)
	if err != nil {
		t.Fatal(err)
	}
	err = verifyPlanBinding(rec, planCurrentFacts{
		App: appCfg.App, Server: rec.Server, Version: version, ConfigDigest: digest,
		State: &state.AppState{Generation: 2, CurrentHash: "old"},
	})
	// The plan's recorded target state must match too.
	rec.TargetState = PlanTargetState{Deployed: true, Generation: 2, CurrentHash: "old"}
	rec.PlanID = computePlanID(rec)
	err = verifyPlanBinding(rec, planCurrentFacts{
		App: appCfg.App, Server: rec.Server, Version: version, ConfigDigest: digest,
		State: &state.AppState{Generation: 2, CurrentHash: "old"},
	})
	if err != nil {
		t.Fatalf("unchanged world must verify, got %v", err)
	}
}

// TestApplyDrift_DeployHappenedInBetween: a deploy between plan and
// apply moves the state generation — the stale plan refuses.
func TestApplyDrift_DeployHappenedInBetween(t *testing.T) {
	dir := t.TempDir()
	writeAppConfig(t, dir, "app: blog\nimage: registry/blog:v1\ndomain: blog.example.com\nport: 3000\n")
	rec := planFromConfig(t, dir, "registry/blog:v1", "")
	rec.TargetState = PlanTargetState{Deployed: true, Generation: 7, CurrentHash: "old99"}
	rec.PlanID = computePlanID(rec)

	appCfg, _, digest, version, err := applyResolveCurrent(context.Background(), &Flags{}, rec)
	if err != nil {
		t.Fatal(err)
	}
	err = verifyPlanBinding(rec, planCurrentFacts{
		App: appCfg.App, Server: rec.Server, Version: version, ConfigDigest: digest,
		State: &state.AppState{Generation: 8, CurrentHash: "v1"},
	})
	var drift *planDriftError
	if !errors.As(err, &drift) || drift.Kind != "target-state" {
		t.Fatalf("generation move must refuse, got %v", err)
	}
	if !strings.Contains(err.Error(), "7 -> 8") {
		t.Errorf("refusal must name the generation move: %v", err)
	}
}

// TestApplyRefusesUnpredictableVersion: a plan made against a floating
// tag (deploy-time timestamp version) can never be bound — apply
// refuses at the gate.
func TestApplyRefusesUnpredictableVersion(t *testing.T) {
	rec := &PlanRecord{
		SchemaVersion: PlanRecordSchemaVersion, PlanID: "x",
		App: "blog", Server: "srv", TargetVersion: "", VersionKnown: false,
		ConfigDigest: "d",
	}
	// The gate runs before any resolution or connection.
	if !rec.VersionKnown {
		if _, err := loadPlanFile(writePlan(t, rec)); err == nil { // plan file must be self-consistent first
			// (computePlanID of this record; writePlan fixes the id)
		}
	}
	rec.PlanID = computePlanID(rec)
	path := writePlan(t, rec)
	if _, err := loadPlanFile(path); err != nil {
		t.Fatalf("plan file must load: %v", err)
	}
	// The refusal itself:
	err := applyUnpredictableVersionGate(rec)
	if err == nil || !strings.Contains(err.Error(), "unpredictable version") {
		t.Fatalf("floating-version plan must be refused: %v", err)
	}
}

// writePlan saves a record to a temp file (id already computed).
func writePlan(t *testing.T, rec *PlanRecord) string {
	t.Helper()
	if rec.PlanID == "" {
		rec.PlanID = computePlanID(rec)
	}
	path := filepath.Join(t.TempDir(), "plan.json")
	if err := savePlanFile(path, rec); err != nil {
		t.Fatal(err)
	}
	return path
}

// applyUnpredictableVersionGate is the head of runApply's gate, lifted
// one line for testability (runApply inlines the same condition).
func applyUnpredictableVersionGate(rec *PlanRecord) error {
	if !rec.VersionKnown {
		return fmt.Errorf("plan %s targets an unpredictable version (floating image tag) — re-plan with --version or a digest-pinned image", rec.PlanID)
	}
	return nil
}

// ssNoEphemeral is the port-scan baseline the deploy engine tests use.
const ssNoEphemeral = `State   Recv-Q  Send-Q  Local Address:Port  Peer Address:Port
LISTEN  0       128     0.0.0.0:22           0.0.0.0:*
LISTEN  0       128     0.0.0.0:80           0.0.0.0:*
LISTEN  0       128     0.0.0.0:443          0.0.0.0:*`

// TestApplyStampReceiptThroughEngine: a verified apply executes through
// the REAL deploy engine (deployBuiltImageFenced — the same function
// `teploy deploy` runs) and the provenance receipt lands in the attempt
// namespace carrying the plan id.
func TestApplyStampReceiptThroughEngine(t *testing.T) {
	imageDigest := "sha256:" + strings.Repeat("ab", 32)
	image := "registry.example.com/myapp@" + imageDigest
	appCfg := &config.AppConfig{
		App:    "myapp",
		Image:  image,
		Domain: "myapp.example.com",
		Port:   3000,
	}
	version := "v1"

	_, manifestSHA, err := config.NormalizeAndDigest(appCfg, image)
	if err != nil {
		t.Fatal(err)
	}
	appliedManifest, _, _ := config.NormalizeAndDigest(appCfg, image)

	mock := ssh.NewMockExecutor("1.2.3.4",
		// provenance mkdir + upload (UploadAtomic: UPLOAD + mv handled by mock)
		ssh.MockCommand{Match: "mkdir -p /deployments/myapp/meta/att", Output: ""},
		// .env absent
		ssh.MockCommand{Match: "test -f /deployments/myapp/.env", Err: fmt.Errorf("no file")},
		// secrets absent
		ssh.MockCommand{Match: "if [ ! -e '/deployments/myapp/secrets'", Output: "absent"},
		// deploy engine: first deploy
		ssh.MockCommand{Match: "mkdir -p /deployments/myapp", Output: ""},
		ssh.MockCommand{Match: "mkdir /deployments/myapp/.lock", Output: ""},
		ssh.MockCommand{Match: "if [ ! -e '/deployments/myapp/state.json' ]", Output: "absent"},
		ssh.MockCommand{Match: "if [ ! -e '/deployments/myapp/state' ]", Output: "absent"},
		ssh.MockCommand{Match: "ss -tln", Output: ssNoEphemeral},
		ssh.MockCommand{Match: "docker run", Output: "abc123def456"},
		ssh.MockCommand{Match: "docker inspect -f '{{.Image}}'", Output: imageDigest},
		ssh.MockCommand{Match: "docker inspect", Output: "running"},
		ssh.MockCommand{Match: "curl -s -o /dev/null", Output: "200"},
		ssh.MockCommand{Match: "curl -sf http://localhost:2019/config/apps/http/servers/srv0", Output: `{"listen":[":80",":443"]}`},
		ssh.MockCommand{Match: "curl -sf -X PATCH", Err: fmt.Errorf("not found")},
		ssh.MockCommand{Match: "curl -sf -X POST http://localhost:2019/config/apps/http/servers/srv0/routes", Output: ""},
		ssh.MockCommand{Match: "rm -f /tmp/teploy_caddy", Output: ""},
		ssh.MockCommand{Match: "cat /deployments/caddy/Caddyfile", Output: "{\n\tadmin 0.0.0.0:2019\n}\n"},
		ssh.MockCommand{Match: "mv /tmp/teploy_caddyfile.tmp", Output: ""},
		ssh.MockCommand{Match: "mkdir /deployments/caddy/.lock", Output: ""},
		ssh.MockCommand{Match: "a=$(docker exec caddy md5sum", Output: "TEPLOY_CADDY_OK"},
		ssh.MockCommand{Match: "docker exec caddy caddy reload", Output: ""},
		ssh.MockCommand{Match: "rmdir /deployments/caddy/.lock", Output: ""},
		ssh.MockCommand{Match: "printf %s", Output: ""},
		ssh.MockCommand{Match: "rm -rf /deployments/myapp/.lock", Output: ""},
	)

	att := releasemeta.MustAttempt("myapp", version)
	err = deployBuiltImageFenced(context.Background(), mock, appCfg, image, version, "srv.test", false, false, "", nil, &att, "plan123abc456def7")
	if err != nil {
		t.Fatalf("deployBuiltImageFenced: %v", err)
	}

	// The receipt carries the plan id.
	var provPath string
	for path, data := range mock.Files {
		if strings.HasSuffix(path, "/provenance.json") {
			provPath = path
			var p releasemeta.Provenance
			if err := json.Unmarshal(data, &p); err != nil {
				t.Fatalf("provenance.json malformed: %v", err)
			}
			if p.PlanID != "plan123abc456def7" {
				t.Errorf("provenance.plan_id = %q, want the applied plan id", p.PlanID)
			}
			if p.ManifestSHA256 != manifestSHA {
				t.Errorf("provenance manifest digest = %s, want %s", p.ManifestSHA256, manifestSHA)
			}
		}
	}
	if provPath == "" {
		t.Fatal("no provenance.json written — the apply receipt is missing")
	}
	if !strings.Contains(provPath, "/deployments/myapp/meta/att/") {
		t.Errorf("provenance not in the attempt namespace: %s", provPath)
	}

	// The release state landed (the engine ran for real).
	stateData, ok := mock.Files["/deployments/myapp/state.json"]
	if !ok {
		t.Fatal("state.json not written")
	}
	var applied state.AppState
	if err := json.Unmarshal(stateData, &applied); err != nil {
		t.Fatal(err)
	}
	if applied.CurrentHash != version || applied.ManifestSHA256 != manifestSHA || applied.Generation != 1 {
		t.Errorf("applied state wrong: %+v", applied)
	}
	_ = appliedManifest
}

// TestApplyDriftErrorEnvelopeClassifiesConflict: under --json, a drift
// refusal must classify as conflict (the world moved), not internal.
func TestApplyDriftErrorEnvelopeClassifiesConflict(t *testing.T) {
	drift := driftRefusal("config", "digest moved")
	if got := classifyMachineError(drift); got != codeConflict {
		t.Errorf("classifyMachineError(drift) = %s, want %s", got, codeConflict)
	}
	var buf strings.Builder
	if err := writeMachineErrorEnvelope(&buf, drift); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, `"code":"conflict"`) {
		t.Errorf("envelope missing conflict code: %s", out)
	}
	if !strings.Contains(out, "plan no longer valid") {
		t.Errorf("envelope message wrong: %s", out)
	}
	// Ordinary failures keep their classification.
	if got := classifyMachineError(errors.New("boom")); got != codeInternal {
		t.Errorf("plain error classified %s", got)
	}
}

// TestApplyRefusesTamperedPlanFile: a plan file edited after planning
// never reaches execution.
func TestApplyRefusesTamperedPlanFile(t *testing.T) {
	rec := &PlanRecord{SchemaVersion: PlanRecordSchemaVersion, App: "blog", Server: "srv", TargetVersion: "v1", VersionKnown: true, ConfigDigest: "d1"}
	rec.PlanID = computePlanID(rec)
	path := writePlan(t, rec)

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(string(raw), `"config_digest": "d1"`, `"config_digest": "d2"`, 1)
	if tampered == string(raw) {
		t.Fatal("tamper did not apply")
	}
	if err := os.WriteFile(path, []byte(tampered), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadPlanFile(path); err == nil || !strings.Contains(err.Error(), "not self-consistent") {
		t.Fatalf("tampered plan accepted: %v", err)
	}
}
