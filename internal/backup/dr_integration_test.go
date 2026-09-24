//go:build integration

// REAL-fixture verification of the C07 recovery path (colima SSH+Docker
// host, real postgres:16-alpine engine, real gzip/tar, no mocks anywhere
// in the path under test). Same fixture contract as the C01/C03 lanes:
//
//	TEPLOY_FAULT_HOST=127.0.0.1:50075 \
//	TEPLOY_FAULT_USER=tyler \
//	TEPLOY_FAULT_KEY=~/.colima/_lima/_config/user \
//	go test -tags integration -run TestDRIntegration -v ./internal/backup
//
// Disposable fixture only — the test creates and removes
// /deployments/drprobe, /var/tmp/teploy-dr/drprobe, /tmp/teploy-dr-fixture and
// drprobe-* containers.
package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/ssh"
)

func drFixtureEnv(t *testing.T) ssh.Executor {
	t.Helper()
	host := os.Getenv("TEPLOY_FAULT_HOST")
	user := os.Getenv("TEPLOY_FAULT_USER")
	key := os.Getenv("TEPLOY_FAULT_KEY")
	if host == "" || user == "" || key == "" {
		t.Skip("DR fixture needs TEPLOY_FAULT_HOST, TEPLOY_FAULT_USER, TEPLOY_FAULT_KEY (see internal/backup/dr_integration_test.go)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	exec, err := ssh.Connect(ctx, ssh.ConnectConfig{Host: host, User: user, KeyPath: key})
	if err != nil {
		t.Fatalf("connecting fixture: %v", err)
	}
	if out, err := exec.Run(ctx, "docker version --format '{{.Server.Version}}'"); err != nil {
		t.Skipf("fixture host has no reachable docker daemon (%v)", err)
	} else {
		t.Logf("fixture docker server %s", strings.TrimSpace(out))
	}
	t.Cleanup(func() { exec.Close() })
	return exec
}

const drFixtureApp = "drprobe"

func drFixtureCleanup(t *testing.T, exec ssh.Executor) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	for _, c := range []string{drFixtureApp + "-db", drFixtureApp + "-db-drcheck", drFixtureApp + "-dr-appcheck"} {
		exec.Run(ctx, "docker rm -f "+ssh.ShellQuote(c)+" >/dev/null 2>&1 || true")
	}
	// Engine data dirs are chowned to the image's uid with mode 700 (the
	// same reality reconcileDataOwnership exists for), so removal needs
	// sudo — a plain rm -rf silently leaves the data behind and the next
	// run starts from a stale database.
	for _, d := range []string{
		"/deployments/" + drFixtureApp,
		DRStagingRoot + "/" + drFixtureApp,
		"/tmp/teploy-dr-fixture",
	} {
		if out, err := exec.Run(ctx, "sudo rm -rf "+ssh.ShellQuote(d)); err != nil {
			t.Logf("cleanup of %s failed (continuing): %v %s", d, err, out)
		}
	}
}

// drFixtureState is a valid v2 state for the probe app: image alpine:3 with
// the release record carrying "sleep 600" so the app check can boot it.
const drFixtureState = `{"schema_version":2,"deployment_type":"container","ingress_mode":"host","updated_at":"2026-09-23T09:00:00Z","image_ref":"alpine:3","operation_id":"op1","generation":2,"current_port":3000,"current_hash":"h2"}`
const drFixtureRelease = `{"schema_version":1,"app":"drprobe","hash":"h2","created_at":"2026-09-22T10:00:00Z","image_ref":"alpine:3","cmd":"sleep 600"}`

// drFixtureConfig is the restore-time teploy.yml equivalent.
func drFixtureConfig() config.AppConfig {
	return config.AppConfig{
		App:     drFixtureApp,
		Volumes: map[string]string{"data": "/app/data"},
		Accessories: map[string]config.AccessoryConfig{
			"db": {
				Image:   "postgres:16-alpine",
				Env:     map[string]string{"POSTGRES_DB": "appdb", "POSTGRES_USER": "appuser", "POSTGRES_PASSWORD": "fixture-pw"},
				Volumes: map[string]string{"pgdata": "/var/lib/postgresql/data"},
			},
		},
	}
}

// TestDRIntegration_BundleRoundTrip drives the full C07 acceptance against
// the real fixture: create a bundle from a live postgres accessory + app
// volume, restore it ISOLATED (proving /deployments is untouched), check
// the receipt's RPO/RTO and checks, then cut over and prove the engine data
// landed (row count), the staged volume replaced the live one (post-bundle
// file gone, preserved in a recovery dir), and state.json was reinstalled.
func TestDRIntegration_BundleRoundTrip(t *testing.T) {
	exec := drFixtureEnv(t)
	drFixtureCleanup(t, exec)
	t.Cleanup(func() { drFixtureCleanup(t, exec) })

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()

	// --- Stand up the "original host" state.
	must := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	run := func(cmd string) string {
		t.Helper()
		out, err := exec.Run(ctx, cmd)
		if err != nil {
			t.Fatalf("fixture command failed: %s\nerror: %v\noutput: %s", cmd, err, out)
		}
		return out
	}

	run("docker network create teploy >/dev/null 2>&1 || true")
	run("mkdir -p /deployments/" + drFixtureApp + "/volumes/data /deployments/" + drFixtureApp + "/meta /deployments/" + drFixtureApp + "/accessories/db/pgdata")
	run("printf 'original-marker' > /deployments/" + drFixtureApp + "/volumes/data/marker.txt")
	if err := exec.Upload(ctx, strings.NewReader(drFixtureState), "/deployments/"+drFixtureApp+"/state.json", "0600"); err != nil {
		t.Fatalf("seeding state.json: %v", err)
	}
	if err := exec.Upload(ctx, strings.NewReader(drFixtureRelease), "/deployments/"+drFixtureApp+"/meta/h2.json", "0600"); err != nil {
		t.Fatalf("seeding release record: %v", err)
	}

	// The live accessory: a real postgres with real data.
	run(fmt.Sprintf(
		"docker run -d --name %s --restart no --label teploy.app=%s --label teploy.role=accessory "+
			"-e POSTGRES_DB=appdb -e POSTGRES_USER=appuser -e POSTGRES_PASSWORD=fixture-pw "+
			"-v /deployments/%s/accessories/db/pgdata:/var/lib/postgresql/data postgres:16-alpine",
		drFixtureApp+"-db", drFixtureApp, drFixtureApp))
	run(fmt.Sprintf("for i in $(seq 1 30); do docker exec %s pg_isready -U appuser >/dev/null 2>&1 && break; sleep 2; done", drFixtureApp+"-db"))
	run(fmt.Sprintf("docker exec %s psql -U appuser -d appdb -c 'CREATE TABLE probe(id int); INSERT INTO probe VALUES (1),(2),(3);'", drFixtureApp+"-db"))

	client := NewClient(exec, os.Stdout)
	store := DirBundleStore{Root: "/tmp/teploy-dr-fixture"}

	// --- Create the bundle from the live app.
	manifest, err := client.CreateBundle(ctx, BundleOptions{
		App:     drFixtureApp,
		Config:  drFixtureConfig(),
		Version: "integration-test",
	}, store)
	must("CreateBundle", err)
	t.Logf("bundle %s created with %d snapshots", manifest.ID, len(manifest.Snapshots))
	for _, s := range manifest.Snapshots {
		t.Logf("  snapshot %-6s engine=%-8s consistency=%s", s.Name, s.Engine, s.Consistency)
	}
	if len(manifest.Snapshots) != 2 {
		t.Fatalf("expected 2 snapshots (db + data), got %+v", manifest.Snapshots)
	}
	if manifest.AppRun.Image != "alpine:3" || manifest.AppRun.Cmd != "sleep 600" {
		t.Errorf("app run spec not derived: %+v", manifest.AppRun)
	}

	// Post-bundle drift: this file exists ONLY in the live volume, so the
	// cutover's replacement (and its recovery copy) is provable.
	run("printf 'post-bundle' > /deployments/" + drFixtureApp + "/volumes/data/post-bundle.txt")
	beforeTree := run(fmt.Sprintf("find /deployments/%s -type f | sort | xargs md5sum 2>/dev/null", drFixtureApp))

	// --- Isolated restore + validation.
	receipt, err := client.RestoreBundleIsolated(ctx, BundleRestoreOptions{
		App: drFixtureApp, ID: manifest.ID, Config: drFixtureConfig(),
	}, store)
	must("RestoreBundleIsolated", err)
	for _, ck := range receipt.Checks {
		t.Logf("check %-28s %-7s %s %s", ck.Name, ck.Status, ck.Metric, ck.Detail)
	}
	if !receipt.OK {
		t.Fatalf("isolated restore did not validate: %+v", receipt.Checks)
	}
	if receipt.RPOSeconds < 0 || receipt.RTOSeconds <= 0 {
		t.Errorf("RPO/RTO must be measured (RPO may be 0 for an immediate restore): %d/%d", receipt.RPOSeconds, receipt.RTOSeconds)
	}
	var dataOK, appOK bool
	for _, ck := range receipt.Checks {
		if ck.Kind == "data" && ck.Status == "pass" {
			dataOK = true
		}
		if ck.Kind == "app" && ck.Status == "pass" {
			appOK = true
		}
	}
	if !dataOK || !appOK {
		t.Fatalf("fresh-host restore must pass application AND data checks: %+v", receipt.Checks)
	}

	// Isolation proof: the live tree is byte-identical after the restore.
	afterTree := run(fmt.Sprintf("find /deployments/%s -type f | sort | xargs md5sum 2>/dev/null", drFixtureApp))
	if beforeTree != afterTree {
		t.Errorf("isolated restore modified /deployments/%s:\nbefore:\n%s\nafter:\n%s", drFixtureApp, beforeTree, afterTree)
	}

	// --- Explicit cutover.
	cutover, err := client.CutoverBundle(ctx, BundleRestoreOptions{
		App: drFixtureApp, ID: manifest.ID, Config: drFixtureConfig(),
	})
	must("CutoverBundle", err)
	t.Logf("cutover promoted %d path(s), recovery dirs %v", len(cutover.Promoted), cutover.RecoveryDirs)

	// Engine data landed in the recreated accessory: the same 3 rows.
	got := run(fmt.Sprintf("for i in $(seq 1 30); do docker exec %s pg_isready -U appuser >/dev/null 2>&1 && break; sleep 2; done; docker exec %s psql -tA -U appuser -d appdb -c 'SELECT COUNT(*) FROM probe'",
		drFixtureApp+"-db", drFixtureApp+"-db"))
	if strings.TrimSpace(got) != "3" {
		t.Errorf("restored engine data wrong: probe count = %q, want 3", strings.TrimSpace(got))
	}

	// Volume replacement: post-bundle drift is gone from the live volume
	// and preserved in a recovery dir.
	liveMarker := run("cat /deployments/" + drFixtureApp + "/volumes/data/marker.txt")
	if strings.TrimSpace(liveMarker) != "original-marker" {
		t.Errorf("live marker after cutover = %q", liveMarker)
	}
	postBundleGone := run("if [ ! -f /deployments/" + drFixtureApp + "/volumes/data/post-bundle.txt ]; then printf 'gone'; else printf 'present'; fi")
	if strings.TrimSpace(postBundleGone) != "gone" {
		t.Errorf("cutover did not replace the live volume (post-bundle.txt still present)")
	}
	recRecovered := run(fmt.Sprintf("grep -rl 'post-bundle' /deployments/%s/volumes/data.restore-old.* 2>/dev/null | head -1", drFixtureApp))
	if strings.TrimSpace(recRecovered) == "" {
		t.Errorf("the pre-cutover volume copy (with post-bundle.txt) was not preserved in a recovery dir")
	} else {
		t.Logf("pre-cutover original preserved at %s", strings.TrimSpace(recRecovered))
	}

	// State reinstalled from the bundle (parse, don't string-match: the
	// staged manifest legitimately re-indents the embedded raw JSON).
	var stateCheck struct {
		Generation uint64 `json:"generation"`
		ImageRef   string `json:"image_ref"`
	}
	if err := json.Unmarshal([]byte(run("cat /deployments/"+drFixtureApp+"/state.json")), &stateCheck); err != nil {
		t.Fatalf("restored state.json unparseable: %v", err)
	}
	if stateCheck.Generation != 2 || stateCheck.ImageRef != "alpine:3" {
		t.Errorf("restored state.json wrong: %+v", stateCheck)
	}
}

// TestDRIntegration_CorruptBundleRefused: a truncated artifact member
// fails gzip -t on the real host and the restore refuses before any
// engine boots.
func TestDRIntegration_CorruptBundleRefused(t *testing.T) {
	exec := drFixtureEnv(t)
	drFixtureCleanup(t, exec)
	t.Cleanup(func() { drFixtureCleanup(t, exec) })

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	run := func(cmd string) string {
		t.Helper()
		out, err := exec.Run(ctx, cmd)
		if err != nil {
			t.Fatalf("fixture command failed: %s\nerror: %v\noutput: %s", cmd, err, out)
		}
		return out
	}
	run("mkdir -p /deployments/" + drFixtureApp + "/volumes/data /tmp/teploy-dr-fixture/" + drFixtureApp + "/dr")
	run("printf 'original' > /deployments/" + drFixtureApp + "/volumes/data/marker.txt")
	if err := exec.Upload(ctx, strings.NewReader(drFixtureState), "/deployments/"+drFixtureApp+"/state.json", "0600"); err != nil {
		t.Fatalf("seeding state.json: %v", err)
	}

	client := NewClient(exec, os.Stdout)
	store := DirBundleStore{Root: "/tmp/teploy-dr-fixture"}
	cfg := drFixtureConfig()
	delete(cfg.Accessories, "db") // volume-only: the corrupt member is the volume tar
	manifest, err := client.CreateBundle(ctx, BundleOptions{App: drFixtureApp, Config: cfg}, store)
	if err != nil {
		t.Fatalf("CreateBundle: %v", err)
	}

	// Corrupt the volume member: truncate mid-gzip.
	run(fmt.Sprintf("head -c 20 %s > /tmp/teploy-dr-fixture/.corrupt && mv /tmp/teploy-dr-fixture/.corrupt %s",
		ssh.ShellQuote("/tmp/teploy-dr-fixture/"+drFixtureApp+"/dr/"+manifest.ID+"/volumes/data.tar.gz"),
		ssh.ShellQuote("/tmp/teploy-dr-fixture/"+drFixtureApp+"/dr/"+manifest.ID+"/volumes/data.tar.gz")))

	_, err = client.RestoreBundleIsolated(ctx, BundleRestoreOptions{App: drFixtureApp, ID: manifest.ID, Config: drFixtureConfig()}, store)
	if err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("expected corrupt-bundle refusal, got %v", err)
	}
	if booted := run("if docker ps -a --format '{{.Names}}' | grep -q drprobe; then printf 'booted'; else printf 'none'; fi"); strings.TrimSpace(booted) != "none" {
		t.Errorf("no engine must boot from a corrupt bundle")
	}
}

// TestDRIntegration_CutoverCopyFailurePreservesOriginals: an injected
// failure of the promotion copy (staged tree made unreadable AFTER
// validation) rolls the live volume back to its originals on the real
// host — the C07 acceptance, live.
func TestDRIntegration_CutoverCopyFailurePreservesOriginals(t *testing.T) {
	exec := drFixtureEnv(t)
	drFixtureCleanup(t, exec)
	t.Cleanup(func() { drFixtureCleanup(t, exec) })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	run := func(cmd string) string {
		t.Helper()
		out, err := exec.Run(ctx, cmd)
		if err != nil {
			t.Fatalf("fixture command failed: %s\nerror: %v\noutput: %s", cmd, err, out)
		}
		return out
	}
	cfg := drFixtureConfig()
	delete(cfg.Accessories, "db") // volume-only app: the promotion is the mutation under test

	run("mkdir -p /deployments/" + drFixtureApp + "/volumes/data")
	run("printf 'live-original' > /deployments/" + drFixtureApp + "/volumes/data/marker.txt")
	if err := exec.Upload(ctx, strings.NewReader(drFixtureState), "/deployments/"+drFixtureApp+"/state.json", "0600"); err != nil {
		t.Fatalf("seeding state.json: %v", err)
	}
	if err := exec.Upload(ctx, strings.NewReader(drFixtureRelease), "/deployments/"+drFixtureApp+"/meta/h2.json", "0600"); err != nil {
		t.Fatalf("seeding release record: %v", err)
	}

	client := NewClient(exec, os.Stdout)
	store := DirBundleStore{Root: "/tmp/teploy-dr-fixture"}
	manifest, err := client.CreateBundle(ctx, BundleOptions{App: drFixtureApp, Config: cfg}, store)
	if err != nil {
		t.Fatalf("CreateBundle: %v", err)
	}
	receipt, err := client.RestoreBundleIsolated(ctx, BundleRestoreOptions{App: drFixtureApp, ID: manifest.ID, Config: cfg}, store)
	if err != nil || !receipt.OK {
		t.Fatalf("isolated restore must validate first: %v %+v", err, receipt.Checks)
	}

	// Inject the copy failure: the staged tree becomes unreadable, so
	// promoteStaged's `cp -a` fails AFTER the originals were moved aside —
	// exactly the mid-restore partial-copy window.
	run("chmod 000 " + ssh.ShellQuote(DRStagingPath(drFixtureApp, manifest.ID)+"/volumes/data"))
	defer run("chmod -R 700 " + ssh.ShellQuote(DRStagingPath(drFixtureApp, manifest.ID)) + " >/dev/null 2>&1 || true")

	_, err = client.CutoverBundle(ctx, BundleRestoreOptions{App: drFixtureApp, ID: manifest.ID, Config: cfg})
	if err == nil || !strings.Contains(err.Error(), "promoting data at cutover") {
		t.Fatalf("expected cutover promotion failure, got %v", err)
	}
	if !strings.Contains(err.Error(), "previous contents restored") && !strings.Contains(err.Error(), "kept") {
		t.Errorf("error must name the preserved originals: %v", err)
	}

	// The live volume is EXACTLY the original: marker intact, no partial copy.
	got := run("cat /deployments/" + drFixtureApp + "/volumes/data/marker.txt")
	if strings.TrimSpace(got) != "live-original" {
		t.Errorf("originals not preserved after failed promotion: marker=%q", got)
	}
	entries := run("ls /deployments/" + drFixtureApp + "/volumes/data")
	if strings.TrimSpace(entries) != "marker.txt" {
		t.Errorf("live volume not rolled back cleanly: %q", entries)
	}
	// No leftover restore-old directory from the failed promotion.
	leftover := run("ls -d /deployments/" + drFixtureApp + "/volumes/data.restore-old.* 2>/dev/null | wc -l")
	if strings.TrimSpace(leftover) != "0" {
		t.Errorf("failed promotion left recovery dirs behind: %s", leftover)
	}
}

// TestDRIntegration_MissingKeysFailBeforeMutation: against the real host, a
// references-mode bundle with a key the target lacks aborts before staging
// or any docker activity (only read-only commands observed).
func TestDRIntegration_MissingKeysFailBeforeMutation(t *testing.T) {
	exec := drFixtureEnv(t)
	drFixtureCleanup(t, exec)
	t.Cleanup(func() { drFixtureCleanup(t, exec) })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	manifest := drTestManifest("API_KEY")
	manifest.App = drFixtureApp
	manifest.ID = "20260923-101500-0123456789abcdef"
	manifestJSON, _ := json.Marshal(manifest)

	run := func(cmd string) string {
		t.Helper()
		out, err := exec.Run(ctx, cmd)
		if err != nil {
			t.Fatalf("fixture command failed: %s\nerror: %v", cmd, err)
		}
		return out
	}
	run("mkdir -p /tmp/teploy-dr-fixture/" + drFixtureApp + "/dr/" + manifest.ID)
	if err := exec.Upload(ctx, bytes.NewReader(manifestJSON), "/tmp/teploy-dr-fixture/"+drFixtureApp+"/dr/"+manifest.ID+"/manifest.json", "0600"); err != nil {
		t.Fatalf("seeding manifest: %v", err)
	}

	client := NewClient(exec, os.Stdout)
	_, err := client.RestoreBundleIsolated(ctx, BundleRestoreOptions{
		App: drFixtureApp, ID: manifest.ID, Config: drFixtureConfig(),
	}, DirBundleStore{Root: "/tmp/teploy-dr-fixture"})
	if err == nil || !strings.Contains(err.Error(), "API_KEY") {
		t.Fatalf("expected missing-keys failure naming API_KEY, got %v", err)
	}
	if staged := run("if [ -e " + DRStagingRoot + "/" + drFixtureApp + " ]; then printf 'staged'; else printf 'clean'; fi"); strings.TrimSpace(staged) != "clean" {
		t.Errorf("preflight failure must not leave staging behind: %s", staged)
	}
}
