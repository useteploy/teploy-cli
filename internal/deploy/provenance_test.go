package deploy

// C04 plan/receipt equality tests: the deploy plan output (before
// execution) shows the immutable image digest, source revision and
// effective-config digest; the post-deploy receipt repeats them; a closing
// verification asserts record digest == plan digest and a mismatch is a
// loud warning plus repair debt, never a failed live deploy.

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/releasemeta"
	"github.com/useteploy/teploy/internal/ssh"
)

func c04Provenance(digest string, dirty bool) *releasemeta.Provenance {
	return &releasemeta.Provenance{
		App:                "myapp",
		Release:            "v9",
		Revision:           "462d7a7b1c2d3e4f5a6b7c8d9e0f1a2b3c4d5e6f",
		Dirty:              dirty,
		ContextPath:        ".",
		ContextFingerprint: strings.Repeat("f", 64),
		Dockerfile:         "Dockerfile",
		DockerfileSHA256:   strings.Repeat("d", 64),
		Platform:           "linux/amd64",
		ImageRef:           "myapp:v9",
		ImageDigest:        digest,
		ManifestSHA256:     strings.Repeat("c", 64),
	}
}

func TestDeploy_PlanOutputShowsDigestRevisionAndConfig(t *testing.T) {
	imageDigest := "sha256:" + strings.Repeat("a", 64)
	mock := deployRecordFixture(imageDigest,
		ssh.MockCommand{Match: "docker image inspect --format '{{.Id}}'", Output: imageDigest},
	)

	var buf bytes.Buffer
	err := NewDeployer(mock, &buf).Deploy(context.Background(), Config{
		App:            "myapp",
		Domain:         "myapp.com",
		Image:          "myapp:v9",
		Version:        "v9",
		SourceRevision: "462d7a7b1c2d3e4f5a6b7c8d9e0f1a2b3c4d5e6f",
		ManifestSHA256: strings.Repeat("c", 64),
		Provenance:     c04Provenance(imageDigest, true),
	})
	if err != nil {
		t.Fatalf("Deploy: %v\n%s", err, buf.String())
	}

	out := buf.String()
	// The plan appears BEFORE execution: its output precedes the first
	// container start, which precedes everything else the deploy mutates.
	planIdx := strings.Index(out, "Plan:")
	if planIdx < 0 {
		t.Fatalf("no plan output:\n%s", out)
	}
	runLine := strings.Index(out, "Starting container myapp-web-v9")
	if runLine >= 0 && planIdx > runLine {
		t.Fatalf("the plan must be printed before execution begins:\n%s", out)
	}

	for _, want := range []string{
		"Plan:",
		imageDigest,
		"462d7a7b1c2d3e4f5a6b7c8d9e0f1a2b3c4d5e6f",
		"building uncommitted changes",
		strings.Repeat("f", 64),
		strings.Repeat("c", 64),
		"Verified: the deployed image digest equals the plan (" + imageDigest + ")",
		"Receipt: image " + imageDigest,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("deploy output missing %q:\n%s", want, out)
		}
	}

	// The F14 receipt embeds the provenance and the effective-config digest.
	raw, ok := mock.Files["/deployments/myapp/meta/v9.json"]
	if !ok {
		t.Fatalf("release record not written\n%s", out)
	}
	var rec releasemeta.Record
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatalf("invalid record: %v", err)
	}
	if rec.ManifestSHA256 != strings.Repeat("c", 64) {
		t.Errorf("record does not carry the effective-config digest: %q", rec.ManifestSHA256)
	}
	if rec.Provenance == nil {
		t.Fatalf("record does not embed the deploy provenance")
	}
	if rec.Provenance.Revision != "462d7a7b1c2d3e4f5a6b7c8d9e0f1a2b3c4d5e6f" ||
		rec.Provenance.ContextFingerprint != strings.Repeat("f", 64) ||
		rec.Provenance.ImageDigest != imageDigest {
		t.Errorf("record's embedded provenance lost fields: %+v", rec.Provenance)
	}
}

// A clean worktree must not claim uncommitted changes.
func TestDeploy_PlanCleanWorktreeHasNoDirtyWarning(t *testing.T) {
	imageDigest := "sha256:" + strings.Repeat("a", 64)
	mock := deployRecordFixture(imageDigest,
		ssh.MockCommand{Match: "docker image inspect --format '{{.Id}}'", Output: imageDigest},
	)
	var buf bytes.Buffer
	err := NewDeployer(mock, &buf).Deploy(context.Background(), Config{
		App: "myapp", Domain: "myapp.com", Image: "myapp:v9", Version: "v9",
		SourceRevision: "462d7a7b1c2d3e4f5a6b7c8d9e0f1a2b3c4d5e6f",
		Provenance:     c04Provenance(imageDigest, false),
	})
	if err != nil {
		t.Fatalf("Deploy: %v\n%s", err, buf.String())
	}
	if strings.Contains(buf.String(), "building uncommitted changes") {
		t.Errorf("clean worktree flagged as dirty:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "Verified: the deployed image digest equals the plan") {
		t.Errorf("equality must still be verified:\n%s", buf.String())
	}
}

// The closing verification: a record digest that disagrees with the plan is
// a LOUD warning + repair debt (C01-6's marker), never a failed live
// deploy.
func TestDeploy_PlanReceiptMismatchWarnsAndRecordsRepairDebt(t *testing.T) {
	recordDigest := "sha256:" + strings.Repeat("a", 64)
	planDigest := "sha256:" + strings.Repeat("e", 64)
	mock := deployRecordFixture(recordDigest,
		ssh.MockCommand{Match: "docker image inspect --format '{{.Id}}'", Output: planDigest},
	)

	var buf bytes.Buffer
	err := NewDeployer(mock, &buf).Deploy(context.Background(), Config{
		App:        "myapp",
		Domain:     "myapp.com",
		Image:      "myapp:v9",
		Version:    "v9",
		Provenance: c04Provenance(planDigest, false),
	})
	if err != nil {
		t.Fatalf("a plan/receipt mismatch must not fail the live deploy: %v\n%s", err, buf.String())
	}

	out := buf.String()
	if !strings.Contains(out, "plan/receipt mismatch") && !strings.Contains(out, "plan/receipt MISMATCH") {
		t.Errorf("expected a loud mismatch warning:\n%s", out)
	}
	if !strings.Contains(out, planDigest) || !strings.Contains(out, recordDigest) {
		t.Errorf("the warning must name both digests:\n%s", out)
	}
	if strings.Contains(out, "Verified: the deployed image digest equals the plan") {
		t.Errorf("a mismatch must not also report equality:\n%s", out)
	}

	debtRaw, ok := mock.Files["/deployments/myapp/repair-debt.json"]
	if !ok {
		t.Fatalf("mismatch must record repair debt via the C01-6 marker\n%s", out)
	}
	debt := string(debtRaw)
	if !strings.Contains(debt, planDigest) || !strings.Contains(debt, recordDigest) {
		t.Errorf("repair-debt marker does not describe the mismatch:\n%s", debt)
	}
}

// A provenance record describing a different deploy than the Config it
// rides on is an identity lie — refused before any effect.
func TestDeploy_ProvenanceIdentityMismatchRefused(t *testing.T) {
	prov := c04Provenance("sha256:"+strings.Repeat("a", 64), false)
	prov.App = "otherapp"
	err := NewDeployer(deployRecordFixture("sha256:"+strings.Repeat("a", 64)), &strings.Builder{}).
		Deploy(context.Background(), Config{
			App: "myapp", Domain: "myapp.com", Image: "myapp:v9", Version: "v9",
			Provenance: prov,
		})
	if err == nil || !strings.Contains(err.Error(), "provenance") {
		t.Fatalf("expected a provenance identity refusal, got %v", err)
	}
}

// Without a resolvable plan digest (legacy direct construction, resolution
// unavailable) the equality check stays silent rather than crying wolf —
// the CLI paths that carry provenance always resolve first.
func TestDeploy_NoPlanDigestNoFalseAlarm(t *testing.T) {
	mock := deployRecordFixture("sha256:" + strings.Repeat("a", 64))
	var buf bytes.Buffer
	err := NewDeployer(mock, &buf).Deploy(context.Background(), Config{
		App: "myapp", Domain: "myapp.com", Image: "myapp:v9", Version: "v9",
	})
	if err != nil {
		t.Fatalf("Deploy: %v\n%s", err, buf.String())
	}
	if strings.Contains(buf.String(), "MISMATCH") || strings.Contains(buf.String(), "mismatch") {
		t.Errorf("nothing to compare must not warn:\n%s", buf.String())
	}
}
