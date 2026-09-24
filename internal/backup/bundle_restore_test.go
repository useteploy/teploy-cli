package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy/internal/ssh"
)

const drTestBundleID = "20260923-101500-0123456789abcdef"

// drTestManifest builds a manifest for myapp with one postgres accessory
// snapshot and one app-volume snapshot, references-only secrets.
func drTestManifest(keys ...string) *BundleManifest {
	return &BundleManifest{
		SchemaVersion: BundleSchemaVersion,
		Kind:          BundleKindDR,
		ID:            drTestBundleID,
		App:           "myapp",
		Server:        "dr-host",
		CreatedAt:     time.Date(2026, 9, 23, 9, 0, 0, 0, time.UTC),
		State:         json.RawMessage(drTestState),
		Secrets:       SecretsRecord{Mode: "references", Keys: keys},
		Snapshots: []SnapshotRecord{
			{Name: "db", Role: "accessory", Engine: "postgres", Method: "pg_dump",
				Consistency: consistencyEngineDump, Detection: "image-pattern",
				Artifact: "accessories/db.sql.gz", Image: "postgres:16",
				EngineParams: map[string]string{"db": "appdb", "user": "appuser"}},
			{Name: "data", Role: "app-volume", Method: "tar",
				Consistency: consistencyCrash, Artifact: "volumes/data.tar.gz"},
		},
	}
}

func drRestoreMock(manifest *BundleManifest, extra ...ssh.MockCommand) *ssh.MockExecutor {
	staging := DRStagingPath("myapp", manifest.ID)
	manifestJSON, _ := json.Marshal(manifest)
	// extras are PREPENDED so injected failures outrank the base successes.
	mock := ssh.NewMockExecutor("dr-host", append(extra, []ssh.MockCommand{
		{Match: "rm -rf '" + staging, Output: ""},
		{Match: "umask 077; mkdir -p '" + staging, Output: ""},
		{Match: "mkdir -p '" + staging, Output: ""},
		{Match: "cp -p", Output: ""},
		{Match: "gzip -t", Output: ""},
		{Match: "tar -xzf", Output: ""},
		{Match: "docker rm -f", Output: ""},
		{Match: "docker run -d", Output: "cid123\n"},
		{Match: "for i in $(seq", Output: ""},
		{Match: "gunzip -c", Output: ""},
		{Match: "docker exec 'myapp-db-drcheck' psql -tA", Output: "3\n"},
	}...)...)
	// The store's manifest, readable through the framed auto-answer.
	mock.Files["/var/drstore/myapp/dr/"+manifest.ID+"/manifest.json"] = manifestJSON
	return mock
}

// TestRestoreBundleIsolated_MissingSecretKeysFailBeforeMutation — the C07
// acceptance: in references mode, keys missing on the target abort BEFORE
// any staging, download, or docker activity. Only read-only commands may
// have run.
func TestRestoreBundleIsolated_MissingSecretKeysFailBeforeMutation(t *testing.T) {
	manifest := drTestManifest("API_KEY", "DATABASE_URL")
	mock := drRestoreMock(manifest)
	// Target has NO secrets: the framed auto-answer reports the secrets dir
	// absent (nothing seeded).

	client := NewClient(mock, &bytes.Buffer{})
	_, err := client.RestoreBundleIsolated(context.Background(), BundleRestoreOptions{
		App: "myapp", ID: manifest.ID, Config: drTestConfig(),
	}, DirBundleStore{Root: "/var/drstore"})
	if err == nil || !strings.Contains(err.Error(), "API_KEY, DATABASE_URL") {
		t.Fatalf("expected missing-keys error naming the keys, got %v", err)
	}
	if !strings.Contains(err.Error(), "nothing has been touched") {
		t.Errorf("error must state that nothing was touched: %v", err)
	}
	for _, call := range mock.Calls {
		if strings.Contains(call, "docker ") || strings.Contains(call, "mkdir -p") || strings.HasPrefix(call, "rm -rf") || strings.Contains(call, "cp -p") {
			t.Errorf("mutating command ran before preflight passed: %s", call)
		}
	}
}

// TestRestoreBundleIsolated_WrongAppRefused: a bundle for another app never
// reaches staging.
func TestRestoreBundleIsolated_WrongAppRefused(t *testing.T) {
	manifest := drTestManifest()
	manifest.App = "otherapp"
	mock := drRestoreMock(manifest)
	_, err := NewClient(mock, &bytes.Buffer{}).RestoreBundleIsolated(context.Background(),
		BundleRestoreOptions{App: "myapp", ID: manifest.ID, Config: drTestConfig()},
		DirBundleStore{Root: "/var/drstore"})
	if err == nil || !strings.Contains(err.Error(), "wrong app") {
		t.Fatalf("expected wrong-app refusal, got %v", err)
	}
}

// TestRestoreBundleIsolated_CorruptBundleFailsBeforeEngines: a gzip member
// that fails integrity aborts before any engine boot; staging is the only
// thing that existed.
func TestRestoreBundleIsolated_CorruptBundleFailsBeforeEngines(t *testing.T) {
	manifest := drTestManifest()
	mock := drRestoreMock(manifest,
		ssh.MockCommand{Match: "gzip -t", Err: errors.New("exit status 1: gzip: invalid compressed data")},
	)
	_, err := NewClient(mock, &bytes.Buffer{}).RestoreBundleIsolated(context.Background(),
		BundleRestoreOptions{App: "myapp", ID: manifest.ID, Config: drTestConfig()},
		DirBundleStore{Root: "/var/drstore"})
	if err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("expected corrupt-bundle refusal, got %v", err)
	}
	for _, call := range mock.Calls {
		if strings.Contains(call, "docker run") {
			t.Errorf("engine booted despite corrupt bundle: %s", call)
		}
	}
}

// TestRestoreBundleIsolated_ReceiptRPORTO: the happy path produces a receipt
// with passing data + app checks, RPO measured from bundle creation, RTO
// from the restore clock, scratch containers torn down, and NOTHING under
// /deployments touched.
func TestRestoreBundleIsolated_ReceiptRPORTO(t *testing.T) {
	manifest := drTestManifest()
	mock := drRestoreMock(manifest)
	start := time.Date(2026, 9, 23, 11, 0, 0, 0, time.UTC)
	validated := start.Add(90 * time.Second)
	calls := 0
	now := func() time.Time {
		calls++
		if calls == 1 {
			return start
		}
		return validated
	}

	client := NewClient(mock, &bytes.Buffer{})
	receipt, err := client.RestoreBundleIsolated(context.Background(), BundleRestoreOptions{
		App: "myapp", ID: manifest.ID, Config: drTestConfig(), Now: now,
	}, DirBundleStore{Root: "/var/drstore"})
	if err != nil {
		t.Fatalf("RestoreBundleIsolated: %v", err)
	}
	if !receipt.OK {
		t.Fatalf("expected OK receipt, checks: %+v", receipt.Checks)
	}
	// RPO: restore start (11:00) - bundle creation (09:00) = 7200s.
	if receipt.RPOSeconds != 7200 {
		t.Errorf("RPO = %d, want 7200", receipt.RPOSeconds)
	}
	if receipt.RTOSeconds != 90 {
		t.Errorf("RTO = %d, want 90", receipt.RTOSeconds)
	}
	if receipt.StagingPath != DRStagingPath("myapp", manifest.ID) {
		t.Errorf("staging path: %s", receipt.StagingPath)
	}
	var dataCheck, appCheck bool
	for _, ck := range receipt.Checks {
		if ck.Kind == "data" && ck.Status == "pass" && ck.Metric == "tables=3" {
			dataCheck = true
		}
		if ck.Kind == "app" && ck.Status == "pass" {
			appCheck = true
		}
	}
	if !dataCheck || !appCheck {
		t.Errorf("expected passing data+app checks: %+v", receipt.Checks)
	}
	// Scratch teardown happened.
	var sawTeardown bool
	for _, call := range mock.Calls {
		if strings.Contains(call, "docker rm -f 'myapp-db-drcheck'") {
			sawTeardown = true
		}
		if strings.Contains(call, "/deployments/myapp") {
			t.Errorf("isolated restore must not touch /deployments/myapp: %s", call)
		}
	}
	if !sawTeardown {
		t.Errorf("scratch container was not torn down")
	}
}

// TestRestoreBundleIsolated_EncryptedWithoutKeyFailsBeforeEngines: an
// encrypted bundle without the age key must prove the TARGET key decrypts
// it — failure aborts before any engine work.
func TestRestoreBundleIsolated_EncryptedWithoutKeyFailsBeforeEngines(t *testing.T) {
	manifest := drTestManifest()
	manifest.Secrets = SecretsRecord{
		Mode:     "encrypted",
		Keys:     []string{"API_KEY"},
		Included: []string{"secrets/API_KEY.age"},
	}
	mock := drRestoreMock(manifest,
		ssh.MockCommand{Match: "age -d -i '/deployments/.age-key'", Err: errors.New("exit status 1: age: no identity matched")},
	)
	_, err := NewClient(mock, &bytes.Buffer{}).RestoreBundleIsolated(context.Background(),
		BundleRestoreOptions{App: "myapp", ID: manifest.ID, Config: drTestConfig()},
		DirBundleStore{Root: "/var/drstore"})
	if err == nil || !strings.Contains(err.Error(), "age key") {
		t.Fatalf("expected decrypt-proof failure naming the age key, got %v", err)
	}
	for _, call := range mock.Calls {
		if strings.Contains(call, "docker run") {
			t.Errorf("engine booted despite undecryptable bundle: %s", call)
		}
	}
}

// ---- Cutover ----

func drCutoverMock(receipt *RestoreReceipt, manifest *BundleManifest, extra ...ssh.MockCommand) *ssh.MockExecutor {
	staging := DRStagingPath("myapp", manifest.ID)
	receiptJSON, _ := json.Marshal(receipt)
	manifestJSON, _ := json.Marshal(manifest)
	// extras are PREPENDED so injected failures outrank the base successes.
	mock := ssh.NewMockExecutor("dr-host", append(extra, []ssh.MockCommand{
		{Match: "mkdir /deployments/myapp/.lock", Output: ""},
		{Match: "docker ps --filter label=teploy.app='myapp'", Output: "myapp-web\nmyapp-db\n"},
		{Match: "docker stop", Output: ""},
		{Match: "docker start", Output: ""},
		{Match: "if [ -z \"$(ls -A", Output: "empty\n"},
		{Match: "docker inspect -f '{{.State.Status}}' 'myapp-db'", Output: "exited\n"},
		{Match: "docker rm -f", Output: ""},
		{Match: "mkdir -p /deployments/myapp/accessories/db", Output: ""},
		{Match: "mkdir -p /deployments/myapp/accessories/db/pgdata", Output: ""},
		{Match: "docker image inspect", Output: "\n"},
		{Match: "docker run --detach", Output: ""},
		{Match: "for i in $(seq", Output: ""},
		{Match: "gunzip -c", Output: ""},
		{Match: "mkdir -p '/deployments/myapp/volumes/data'", Output: ""},
		{Match: "mktemp -d '/deployments", Output: "/deployments/myapp/recovery123\n"},
		{Match: "find ", Output: ""},
		{Match: "cp -a ", Output: ""},
		{Match: "mv -f ", Output: ""},
		{Match: "cp -p", Output: ""},
		{Match: "rm -rf", Output: ""},
		{Match: "mkdir -p '/deployments/myapp/meta' '/deployments/myapp/secrets'", Output: ""},
		{Match: "mkdir -p /deployments/myapp/meta", Output: ""},
	}...)...)
	mock.Files[staging+"/receipt.json"] = receiptJSON
	mock.Files[staging+"/manifest.json"] = manifestJSON
	return mock
}

func drOKReceipt() *RestoreReceipt {
	return &RestoreReceipt{
		SchemaVersion: RestoreReceiptSchemaVersion,
		BundleID:      drTestBundleID,
		App:           "myapp",
		OK:            true,
		StagingPath:   DRStagingPath("myapp", drTestBundleID),
	}
}

// TestCutover_RequiresValidatedStaging: no staged restore, or one that
// FAILED validation, must never promote.
func TestCutover_RequiresValidatedStaging(t *testing.T) {
	manifest := drTestManifest()

	noReceipt := ssh.NewMockExecutor("dr-host")
	_, err := NewClient(noReceipt, &bytes.Buffer{}).CutoverBundle(context.Background(),
		BundleRestoreOptions{App: "myapp", ID: manifest.ID, Config: drTestConfig()})
	if !errors.Is(err, ErrValidationRequired) {
		t.Fatalf("expected ErrValidationRequired, got %v", err)
	}

	bad := drOKReceipt()
	bad.OK = false
	mock := drCutoverMock(bad, manifest)
	_, err = NewClient(mock, &bytes.Buffer{}).CutoverBundle(context.Background(),
		BundleRestoreOptions{App: "myapp", ID: manifest.ID, Config: drTestConfig()})
	if err == nil || !strings.Contains(err.Error(), "FAILED validation") {
		t.Fatalf("expected validation refusal, got %v", err)
	}
	for _, call := range mock.Calls {
		if strings.Contains(call, "docker stop") {
			t.Errorf("refused cutover still stopped containers: %s", call)
		}
	}
}

// TestCutover_InjectedCopyFailurePreservesOriginals — the C07 acceptance:
// a promotion copy that fails mid-flight rolls the live directory back to
// its originals (move-back executed), and the error names what was kept.
func TestCutover_InjectedCopyFailurePreservesOriginals(t *testing.T) {
	manifest := drTestManifest()
	mock := drCutoverMock(drOKReceipt(), manifest,
		ssh.MockCommand{Match: "cp -a '", Err: errors.New("exit status 1: cp: cannot create file: No space left on device")},
	)
	receipt, err := NewClient(mock, &bytes.Buffer{}).CutoverBundle(context.Background(),
		BundleRestoreOptions{App: "myapp", ID: manifest.ID, Config: drTestConfig()})
	if err == nil {
		t.Fatal("expected cutover failure")
	}
	if !strings.Contains(err.Error(), "previous contents restored") && !strings.Contains(err.Error(), "kept") {
		t.Errorf("error must name the preserved originals: %v", err)
	}
	if receipt == nil {
		t.Fatal("a cutover receipt (even failed) should be returned")
	}
	var sawRollback bool
	for _, call := range mock.Calls {
		if strings.Contains(call, "find '/deployments/myapp/volumes/data' -mindepth 1 -delete && find '/deployments/myapp/recovery123'") {
			sawRollback = true
		}
	}
	if !sawRollback {
		t.Errorf("rollback (clear partial copy + move originals back) never ran")
	}
	// Stopped containers were restarted by the failure path.
	var sawRestart bool
	for _, call := range mock.Calls {
		if strings.Contains(call, "docker start 'myapp-web'") {
			sawRestart = true
		}
	}
	if !sawRestart {
		t.Errorf("stopped containers were not restarted after the failed cutover")
	}
}

// TestCutover_InjectedStartFailurePreservesOriginals: an accessory that
// cannot start aborts the cutover BEFORE any data promotion (no cp -a, no
// state install), with the stopped containers restarted.
func TestCutover_InjectedStartFailurePreservesOriginals(t *testing.T) {
	manifest := drTestManifest()
	mock := drCutoverMock(drOKReceipt(), manifest,
		ssh.MockCommand{Match: "docker run --detach", Err: errors.New("exit status 125: docker: Error response from daemon: no space on device")},
	)
	_, err := NewClient(mock, &bytes.Buffer{}).CutoverBundle(context.Background(),
		BundleRestoreOptions{App: "myapp", ID: manifest.ID, Config: drTestConfig()})
	if err == nil || !strings.Contains(err.Error(), "starting accessory db") {
		t.Fatalf("expected accessory start failure, got %v", err)
	}
	for _, call := range mock.Calls {
		if strings.HasPrefix(call, "cp -a ") {
			t.Errorf("data promotion ran despite accessory start failure: %s", call)
		}
		if strings.Contains(call, "UPLOAD:/deployments/myapp/state.json") {
			t.Errorf("state was installed despite accessory start failure: %s", call)
		}
	}
	var sawRestart bool
	for _, call := range mock.Calls {
		if strings.Contains(call, "docker start 'myapp-web'") || strings.Contains(call, "docker start 'myapp-db'") {
			sawRestart = true
		}
	}
	if !sawRestart {
		t.Errorf("stopped containers were not restarted after the aborted cutover")
	}
}

// TestCutover_HappyPath: engine dump restored into the recreated accessory,
// volume promoted two-phase, state.json + release records installed, and
// the pre-existing engine data dir preserved as a recovery copy.
func TestCutover_HappyPath(t *testing.T) {
	manifest := drTestManifest()
	manifest.ReleaseRecords = []json.RawMessage{json.RawMessage(`{"schema_version":1,"app":"myapp","hash":"rel-77","created_at":"2026-09-20T10:00:00Z"}`)}
	// Non-empty engine dir forces the pre-cutover move-aside.
	mock := drCutoverMock(drOKReceipt(), manifest,
		ssh.MockCommand{Match: "if [ -z \"$(ls -A", Output: "nonempty\n"},
		ssh.MockCommand{Match: "if [ -e '/deployments/myapp/accessories/db/pgdata'", Output: "present\n"},
		ssh.MockCommand{Match: "mktemp -d '/deployments/myapp/accessories/db.pre-cutover.XXXXXX'", Output: "/deployments/myapp/accessories/db.pre-cutover.Aa1\n"},
	)
	receipt, err := NewClient(mock, &bytes.Buffer{}).CutoverBundle(context.Background(),
		BundleRestoreOptions{App: "myapp", ID: manifest.ID, Config: drTestConfig()})
	if err != nil {
		t.Fatalf("CutoverBundle: %v", err)
	}
	joined := strings.Join(mock.Calls, "\n")
	if !strings.Contains(joined, "docker exec -i 'myapp-db' psql -v ON_ERROR_STOP=1 -U 'appuser' 'appdb'") {
		t.Errorf("engine dump was not restored into the live accessory")
	}
	if !strings.Contains(joined, "mv -f '/deployments/myapp/accessories/db/pgdata' '/deployments/myapp/accessories/db.pre-cutover.Aa1/pgdata'") {
		t.Errorf("pre-cutover engine data was not preserved")
	}
	if len(receipt.Promoted) != 1 || receipt.Promoted[0] != "/deployments/myapp/volumes/data" {
		t.Errorf("promoted paths: %v", receipt.Promoted)
	}
	stateBytes, ok := mock.Files["/deployments/myapp/state.json"]
	if !ok || !strings.Contains(string(stateBytes), `"generation":4`) {
		t.Errorf("restored state.json not installed: %q", string(stateBytes))
	}
	if _, ok := mock.Files["/deployments/myapp/meta/rel-77.json"]; !ok {
		t.Errorf("release record not installed")
	}
	if len(receipt.RecoveryDirs) == 0 {
		t.Errorf("recovery dirs not recorded: %+v", receipt)
	}
}

// TestCutover_NonEmptyAccessoryDirWithoutVolumeData: an accessory dir that
// is nonempty (env/credential files) but has NO on-disk volume directory —
// a volume added to teploy.yml after the bundle, or a re-run after a
// partial failure — has nothing to preserve: the cutover proceeds, moves
// nothing, and records no empty recovery dir.
func TestCutover_NonEmptyAccessoryDirWithoutVolumeData(t *testing.T) {
	manifest := drTestManifest()
	mock := drCutoverMock(drOKReceipt(), manifest,
		ssh.MockCommand{Match: "if [ -z \"$(ls -A", Output: "nonempty\n"},
		ssh.MockCommand{Match: "if [ -e '/deployments/myapp/accessories/db/pgdata'", Output: "absent\n"},
	)
	receipt, err := NewClient(mock, &bytes.Buffer{}).CutoverBundle(context.Background(),
		BundleRestoreOptions{App: "myapp", ID: manifest.ID, Config: drTestConfig()})
	if err != nil {
		t.Fatalf("CutoverBundle: %v", err)
	}
	for _, call := range mock.Calls {
		if strings.HasPrefix(call, "mv -f ") || strings.Contains(call, "docker run --rm --user 0") {
			t.Errorf("nothing existed to move aside, yet a move ran: %s", call)
		}
		if strings.Contains(call, "pre-cutover.XXXXXX") {
			t.Errorf("an empty recovery dir was created: %s", call)
		}
	}
	for _, d := range receipt.RecoveryDirs {
		if strings.Contains(d, "pre-cutover") {
			t.Errorf("empty pre-cutover recovery dir recorded: %+v", receipt.RecoveryDirs)
		}
	}
}

// TestCutover_MissingConfiguredAccessoryRefused: a bundle accessory absent
// from the restore-time teploy.yml aborts before anything is stopped.
func TestCutover_MissingConfiguredAccessoryRefused(t *testing.T) {
	manifest := drTestManifest()
	cfg := drTestConfig()
	delete(cfg.Accessories, "db")
	mock := drCutoverMock(drOKReceipt(), manifest)
	_, err := NewClient(mock, &bytes.Buffer{}).CutoverBundle(context.Background(),
		BundleRestoreOptions{App: "myapp", ID: manifest.ID, Config: cfg})
	if err == nil || !strings.Contains(err.Error(), "does not define it") {
		t.Fatalf("expected config cross-check refusal, got %v", err)
	}
	for _, call := range mock.Calls {
		if strings.Contains(call, "docker stop") {
			t.Errorf("cross-check failure still stopped containers: %s", call)
		}
	}
}

// TestCutover_MissingSecretKeysRefusedBeforeMutation: the preflight runs
// again at cutover — keys that vanished since restore abort before stops.
func TestCutover_MissingSecretKeysRefusedBeforeMutation(t *testing.T) {
	manifest := drTestManifest("API_KEY")
	mock := drCutoverMock(drOKReceipt(), manifest)
	_, err := NewClient(mock, &bytes.Buffer{}).CutoverBundle(context.Background(),
		BundleRestoreOptions{App: "myapp", ID: manifest.ID, Config: drTestConfig()})
	if err == nil || !strings.Contains(err.Error(), "API_KEY") {
		t.Fatalf("expected missing-keys refusal, got %v", err)
	}
	for _, call := range mock.Calls {
		if strings.Contains(call, "docker stop") {
			t.Errorf("preflight failure still stopped containers: %s", call)
		}
	}
}
