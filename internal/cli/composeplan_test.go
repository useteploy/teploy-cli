package cli

// C05 conformance: Compose fixtures survive import → plan with the
// known-vs-unresolved image classification asserted. This is the plan
// leg of the "import/render/plan or reject safely" acceptance: every
// supported Compose shape must either classify honestly in a plan or
// have been refused at import (the importer's own suite pins the
// refusals; this suite pins what PLANS say about the survivors).

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/ssh"
)

// composePlanFixture writes a compose file and returns the imported
// config (import-time refusals are the importer suite's contract —
// here every fixture must import).
func composePlanFixture(t *testing.T, yml string) *config.AppConfig {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "compose.yml"), []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadCompose(dir)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if cfg == nil {
		t.Fatal("no config imported")
	}
	return cfg
}

func TestComposePlan_ImageClassification(t *testing.T) {
	tests := []struct {
		name           string
		compose        string
		wantImage      string
		wantNeedsBuild bool
		wantResolution string
	}{
		{
			name: "build service is unresolved-awaiting-build",
			compose: `services:
  web:
    build: .
    ports: ["3000:3000"]
`,
			wantNeedsBuild: true,
			wantResolution: imageUnresolvedAwaitingBuild,
		},
		{
			name: "digest-pinned image is resolved-by-digest",
			compose: `services:
  web:
    image: registry.example.com/app@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
    ports: ["3000:3000"]
`,
			wantImage:      "registry.example.com/app@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			wantResolution: imageResolvedByDigest,
		},
		{
			name: "tagged image without server resolution is unresolved-mutable-tag",
			compose: `services:
  web:
    image: nginx:1.27
    ports: ["8080:80"]
`,
			wantImage:      "nginx:1.27",
			wantResolution: imageUnresolvedMutableTag,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := composePlanFixture(t, tt.compose)

			image := cfg.Image
			needsBuild := image == ""
			// The classification the plan records. An empty mock executor
			// models "docker cannot resolve the ref" — exactly the live
			// degradation for an unreachable registry.
			prov := resolveDeployProvenance(context.Background(), ssh.NewMockExecutor("srv"), os.Stderr, cfg, ".", image, "v1", "d", needsBuild)
			img := planImageIdentity(prov, image, needsBuild)

			if img.NeedsBuild != tt.wantNeedsBuild {
				t.Errorf("needs_build = %v, want %v", img.NeedsBuild, tt.wantNeedsBuild)
			}
			if img.Resolution != tt.wantResolution {
				t.Errorf("resolution = %s, want %s", img.Resolution, tt.wantResolution)
			}
			if tt.wantImage != "" && img.Ref != tt.wantImage {
				t.Errorf("ref = %s, want %s", img.Ref, tt.wantImage)
			}
			if tt.wantResolution == imageResolvedByDigest && img.Digest == "" {
				t.Errorf("digest-pinned classification must carry the digest")
			}
			if tt.wantNeedsBuild && img.ContextFingerprint == "" {
				t.Errorf("build classification must bind a context fingerprint — got none (unbound build plan)")
			}
			// The unresolved notes must SAY it.
			notes := unresolvedNotes(img, true)
			if tt.wantNeedsBuild && len(notes) == 0 {
				t.Errorf("awaiting-build plan must state the unresolved image")
			}
		})
	}
}

// TestComposePlan_EffectSetSurvivesImport: an imported stack plans the
// translated surfaces — accessory adds, env keys, storage — so the
// conformance corpus's fixtures survive plan, not just import.
func TestComposePlan_EffectSetSurvivesImport(t *testing.T) {
	cfg := composePlanFixture(t, `services:
  web:
    build: .
    ports: ["3000:3000"]
    environment:
      - DATABASE_URL=postgres://db/blog
    volumes:
      - uploads:/app/uploads
  db:
    image: postgres:16
    volumes: ["pgdata:/var/lib/postgresql/data"]
`)
	if _, ok := cfg.Accessories["db"]; !ok {
		t.Fatalf("db not imported as accessory: %+v", cfg.Accessories)
	}

	// First deploy: no deployed manifest to diff against.
	acc := accessoryEffects(cfg, nil)
	if e := findEffect(acc, "accessory db"); e == nil || e.Action != "add" || e.To != "postgres:16" {
		t.Errorf("accessory add missing: %+v", e)
	}
	env := envEffects(cfg, nil)
	if e := findEffect(env, "env DATABASE_URL"); e == nil || e.Action != "add" {
		t.Errorf("web env add missing (import dropped it?): %+v", e)
	}
	// Web volumes are APP storage; the db's pgdata rides the accessory
	// effect's detail instead.
	storage := storageEffects("blog", cfg, nil)
	if e := findEffect(storage, "volume uploads"); e == nil || e.Action != "add" {
		t.Errorf("web volume add missing (import dropped it?): %+v", e)
	} else if !strings.Contains(e.Detail, "/deployments/blog/volumes/uploads") {
		t.Errorf("volume detail must name the managed host path: %+v", e)
	}
	if e := findEffect(acc, "accessory db"); e == nil || !strings.Contains(e.Detail, "1 volume(s)") {
		t.Errorf("accessory detail must summarize its volumes: %+v", e)
	}
}

// TestComposePlan_RefusedShapesNeverPlan: the importer's refused shapes
// surface as import errors, never as plans — the reject-safely half of
// the acceptance.
func TestComposePlan_RefusedShapesNeverPlan(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "compose.yml"), []byte(`services:
  web:
    build: .
    ports: ["3000:3000"]
  jobs:
    build: ./jobs
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := config.LoadCompose(dir); err == nil {
		t.Fatal("distinct-builds shape must be refused at import — a plan over it would silently lose build identity")
	}
}
