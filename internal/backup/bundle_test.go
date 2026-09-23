package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/ssh"
)

// drTestState is a minimal valid v2 state.json for myapp.
const drTestState = `{"schema_version":2,"deployment_type":"container","ingress_mode":"caddy","domain":"myapp.example.com","updated_at":"2026-09-20T10:00:00Z","image_ref":"nginx:1.27","generation":4,"current_port":3000,"current_hash":"abc123"}`

func drBaseMock(extra ...ssh.MockCommand) *ssh.MockExecutor {
	mock := ssh.NewMockExecutor("dr-host", append([]ssh.MockCommand{
		{Match: "umask 077; mktemp -d /tmp/teploy-dr.XXXXXXXX", Output: "/tmp/drws1\n"},
		{Match: "mkdir -p '/tmp/drws1", Output: ""},
		{Match: "mkdir -p '/var/drstore/myapp/dr/", Output: ""},
		{Match: "mkdir -p /deployments/myapp/accessories/db", Output: ""},
		{Match: "find '/deployments/myapp/meta'", Output: ""},
		{Match: "tar -czf", Output: ""},
		{Match: "docker exec 'myapp-db' pg_dump", Output: ""},
		{Match: "docker inspect -f '{{.Config.Image}}' 'myapp-db'", Output: "postgres:16\n"},
		{Match: "docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' 'myapp-db'", Output: "POSTGRES_DB=appdb\nPOSTGRES_USER=appuser\n"},
		{Match: "cp -p", Output: ""},
		{Match: "mv -f", Output: ""},
		{Match: "rm -rf", Output: ""},
		{Match: "if [ -e '", Output: "present\n"},
		{Match: "find '/deployments/myapp/secrets'", Output: "API_KEY.age\n"},
	}, extra...)...)
	// state.json seeds the framed-read auto-answer.
	mock.Files["/deployments/myapp/state.json"] = []byte(drTestState)
	return mock
}

func drTestConfig() config.AppConfig {
	return config.AppConfig{
		App:     "myapp",
		Volumes: map[string]string{"data": "/app/data"},
		Accessories: map[string]config.AccessoryConfig{
			"db": {
				Image:   "postgres:16",
				Env:     map[string]string{"POSTGRES_DB": "appdb", "POSTGRES_USER": "appuser"},
				Volumes: map[string]string{"pgdata": "/var/lib/postgresql/data"},
			},
		},
	}
}

// TestCreateBundle_ManifestAndConsistency pins the bundle contract: schema
// version, DR kind, embedded state, engine-consistent accessory snapshots,
// crash-consistent volume snapshots with honest notes, references-only
// secrets, and the manifest uploaded LAST (completeness marker).
func TestCreateBundle_ManifestAndConsistency(t *testing.T) {
	mock := drBaseMock()
	client := NewClient(mock, &bytes.Buffer{})
	store := DirBundleStore{Root: "/var/drstore"}

	m, err := client.CreateBundle(context.Background(), BundleOptions{
		App:    "myapp",
		Config: drTestConfig(),
		Now:    func() time.Time { return time.Date(2026, 9, 23, 10, 15, 0, 0, time.UTC) },
	}, store)
	if err != nil {
		t.Fatalf("CreateBundle: %v", err)
	}
	if m.SchemaVersion != BundleSchemaVersion || m.Kind != BundleKindDR {
		t.Errorf("schema/kind: %d/%q", m.SchemaVersion, m.Kind)
	}
	if err := ValidateDate(m.ID); err != nil {
		t.Errorf("bundle id not in backup-id grammar: %v", err)
	}
	if !strings.Contains(string(m.State), `"generation":4`) {
		t.Errorf("state not embedded verbatim: %s", m.State)
	}
	if m.Routing.Domain != "myapp.example.com" || m.Routing.IngressMode != "caddy" {
		t.Errorf("routing not recorded: %+v", m.Routing)
	}
	if m.Secrets.Mode != "references" || len(m.Secrets.Included) != 0 || m.Secrets.AgeKeyIncluded {
		t.Errorf("secrets must be references-only by default: %+v", m.Secrets)
	}

	byName := map[string]SnapshotRecord{}
	for _, s := range m.Snapshots {
		byName[s.Name] = s
	}
	dbSnap, ok := byName["db"]
	if !ok {
		t.Fatalf("no db snapshot: %+v", m.Snapshots)
	}
	if dbSnap.Engine != "postgres" || dbSnap.Method != "pg_dump" || dbSnap.Consistency != consistencyEngineDump || dbSnap.Detection != "image-pattern" {
		t.Errorf("db snapshot: %+v", dbSnap)
	}
	if dbSnap.EngineParams["db"] != "appdb" || dbSnap.EngineParams["user"] != "appuser" {
		t.Errorf("engine params not recorded: %+v", dbSnap.EngineParams)
	}
	if dbSnap.Image != "postgres:16" {
		t.Errorf("snapshot image not recorded: %q", dbSnap.Image)
	}
	volSnap, ok := byName["data"]
	if !ok {
		t.Fatalf("no data volume snapshot: %+v", m.Snapshots)
	}
	if volSnap.Consistency != consistencyCrash {
		t.Errorf("live volume copy must be labeled crash-consistent: %+v", volSnap)
	}
	if !strings.Contains(volSnap.Notes, "crash recovery") {
		t.Errorf("volume snapshot must carry the honest crash-consistency note: %q", volSnap.Notes)
	}

	// Upload ordering: manifest.json LAST, after every data member.
	lastData := -1
	manifestAt := -1
	for i, call := range mock.Calls {
		if strings.HasPrefix(call, "cp -p '/tmp/drws1/") && strings.Contains(call, " /var/drstore/myapp/dr/") && !strings.Contains(call, "manifest.json") {
			if i > lastData {
				lastData = i
			}
		}
		if strings.HasPrefix(call, "cp -p '/tmp/drws1/manifest.json'") {
			manifestAt = i
		}
	}
	if manifestAt < 0 || manifestAt < lastData {
		t.Errorf("manifest must upload after all data members (manifest at %d, last data at %d)", manifestAt, lastData)
	}
}

// TestCreateBundle_NoStateRefuses: an app with no on-server state has
// nothing to recover — the error must say so and name the data-only
// command, not mint a bundle that lies about scope.
func TestCreateBundle_NoStateRefuses(t *testing.T) {
	mock := ssh.NewMockExecutor("dr-host",
		ssh.MockCommand{Match: "umask 077; mktemp -d /tmp/teploy-dr.XXXXXXXX", Output: "/tmp/drws1\n"},
	)
	client := NewClient(mock, &bytes.Buffer{})
	_, err := client.CreateBundle(context.Background(), BundleOptions{
		App: "myapp", Config: drTestConfig(),
	}, DirBundleStore{Root: "/var/drstore"})
	if err == nil || !strings.Contains(err.Error(), "data-only") {
		t.Fatalf("expected no-state refusal naming data-only backup, got %v", err)
	}
}

// TestCreateBundle_VolumePatternHeuristic: a volume whose container path
// looks like a postgres data dir is recorded with the engine hint and an
// explicit "a raw tar is NOT engine-consistent" caveat — the heuristic is
// surfaced, never trusted.
func TestCreateBundle_VolumePatternHeuristic(t *testing.T) {
	cfg := drTestConfig()
	cfg.Volumes["pgdata"] = "/var/lib/postgresql/data"
	mock := drBaseMock()
	client := NewClient(mock, &bytes.Buffer{})
	m, err := client.CreateBundle(context.Background(), BundleOptions{App: "myapp", Config: cfg}, DirBundleStore{Root: "/var/drstore"})
	if err != nil {
		t.Fatalf("CreateBundle: %v", err)
	}
	for _, s := range m.Snapshots {
		if s.Name == "pgdata" {
			if s.Engine != "postgres" || s.Detection != "volume-pattern" {
				t.Errorf("pattern snapshot: %+v", s)
			}
			if s.Consistency != consistencyCrash {
				t.Errorf("pattern-matched volume must STILL be crash-consistent: %+v", s)
			}
			if !strings.Contains(s.Notes, "NOT a postgres-consistent backup") {
				t.Errorf("note must refuse engine-consistency claim: %q", s.Notes)
			}
			return
		}
	}
	t.Fatalf("pgdata snapshot missing: %+v", m.Snapshots)
}

// TestCreateBundle_StopAppQuiescesVolumes: --stop-app stops the app's web
// containers before the volume tar and restarts them after; the volume
// snapshot is then labeled quiesced with detection=teploy-stop.
func TestCreateBundle_StopAppQuiescesVolumes(t *testing.T) {
	mock := drBaseMock(
		ssh.MockCommand{Match: "docker ps --filter label=teploy.app='myapp' --filter label=teploy.process=web", Output: "myapp-web\n"},
		ssh.MockCommand{Match: "docker stop", Output: ""},
		ssh.MockCommand{Match: "docker start", Output: ""},
	)
	client := NewClient(mock, &bytes.Buffer{})
	m, err := client.CreateBundle(context.Background(), BundleOptions{App: "myapp", Config: drTestConfig(), StopApp: true}, DirBundleStore{Root: "/var/drstore"})
	if err != nil {
		t.Fatalf("CreateBundle: %v", err)
	}
	var vol SnapshotRecord
	for _, s := range m.Snapshots {
		if s.Name == "data" {
			vol = s
		}
	}
	if vol.Consistency != consistencyQuiesced || vol.Detection != "teploy-stop" {
		t.Errorf("stopped-app volume must be quiesced/teploy-stop: %+v", vol)
	}
	var stopAt, tarAt, startAt int = -1, -1, -1
	for i, call := range mock.Calls {
		if strings.Contains(call, "docker stop 'myapp-web'") && stopAt < 0 {
			stopAt = i
		}
		if strings.Contains(call, "tar -czf") && strings.Contains(call, "volumes/data") && tarAt < 0 {
			tarAt = i
		}
		if strings.Contains(call, "docker start 'myapp-web'") && startAt < 0 {
			startAt = i
		}
	}
	if stopAt < 0 || tarAt < 0 || startAt < 0 || !(stopAt < tarAt && tarAt < startAt) {
		t.Errorf("expected stop -> tar -> start ordering (stop=%d tar=%d start=%d)", stopAt, tarAt, startAt)
	}
}

// TestCreateBundle_IncludeSecretsOptsIn: encrypted material travels ONLY
// with the explicit flag — default runs must not copy ciphertexts, .env,
// or credentials.
func TestCreateBundle_IncludeSecretsOptsIn(t *testing.T) {
	base := func() *ssh.MockExecutor {
		mock := drBaseMock()
		mock.Files["/deployments/myapp/secrets"] = []byte("")
		mock.Files["/deployments/myapp/secrets/API_KEY.age"] = []byte("ciphertext")
		mock.Files["/deployments/myapp/.env"] = []byte("FOO=bar\n")
		mock.Files["/deployments/myapp/accessories/db/credentials"] = []byte("POSTGRES_PASSWORD=p\n")
		return mock
	}

	defaultMock := base()
	if _, err := NewClient(defaultMock, &bytes.Buffer{}).CreateBundle(context.Background(),
		BundleOptions{App: "myapp", Config: drTestConfig()}, DirBundleStore{Root: "/var/drstore"}); err != nil {
		t.Fatalf("CreateBundle default: %v", err)
	}
	for _, call := range defaultMock.Calls {
		if strings.Contains(call, "secrets/API_KEY.age") || strings.Contains(call, ".env' ") && strings.Contains(call, "/tmp/drws1/env") {
			t.Errorf("default run must not bundle secret material: %s", call)
		}
	}

	optMock := base()
	m, err := NewClient(optMock, &bytes.Buffer{}).CreateBundle(context.Background(),
		BundleOptions{App: "myapp", Config: drTestConfig(), IncludeSecrets: true}, DirBundleStore{Root: "/var/drstore"})
	if err != nil {
		t.Fatalf("CreateBundle include-secrets: %v", err)
	}
	if m.Secrets.Mode != "encrypted" || len(m.Secrets.Included) != 3 {
		t.Errorf("encrypted mode must list its members: %+v", m.Secrets)
	}
	included := strings.Join(m.Secrets.Included, ",")
	for _, want := range []string{"secrets/API_KEY.age", "env/.env", "env/credentials/db"} {
		if !strings.Contains(included, want) {
			t.Errorf("member %s not included: %v", want, m.Secrets.Included)
		}
	}
	if m.Secrets.AgeKeyIncluded {
		t.Errorf("age key must not travel without --include-age-key")
	}
}

// TestCreateBundle_RecoveryPlanDocumentsManualSteps: accessory upgrades,
// credential rotation, storage-path keying, TLS material and the
// deploy-after-cutover step appear in the carried recovery plan.
func TestCreateBundle_RecoveryPlanDocumentsManualSteps(t *testing.T) {
	cfg := drTestConfig()
	cfg.TLS = &config.TLSConfig{Cert: "/etc/ssl/myapp.crt", Key: "/etc/ssl/myapp.key"}
	mock := drBaseMock()
	mock.Files["/deployments/myapp/secrets"] = []byte("")
	mock.Files["/deployments/myapp/secrets/API_KEY.age"] = []byte("ct")
	m, err := NewClient(mock, &bytes.Buffer{}).CreateBundle(context.Background(),
		BundleOptions{App: "myapp", Config: cfg}, DirBundleStore{Root: "/var/drstore"})
	if err != nil {
		t.Fatalf("CreateBundle: %v", err)
	}
	actions := map[string]RecoveryStep{}
	for _, s := range m.Recovery.Steps {
		actions[s.Action] = s
	}
	for _, want := range []string{"restore-isolated", "cutover", "accessory-upgrade", "credential-rotation", "storage-path", "tls-material", "deploy"} {
		if _, ok := actions[want]; !ok {
			t.Errorf("recovery plan missing %q: %+v", want, m.Recovery.Steps)
		}
	}
	if !strings.Contains(actions["accessory-upgrade"].Summary, "teploy accessory upgrade") {
		t.Errorf("accessory-upgrade step should name the command: %s", actions["accessory-upgrade"].Summary)
	}
	if !strings.Contains(actions["credential-rotation"].Summary, "API_KEY") {
		t.Errorf("credential-rotation step should list the referenced keys: %s", actions["credential-rotation"].Summary)
	}
}

// TestParseBundleManifest_RefusesForeignSchemaAndKind: version/kind
// mismatches die at parse time, before any restore logic runs.
func TestParseBundleManifest_RefusesForeignSchemaAndKind(t *testing.T) {
	good, _ := json.Marshal(&BundleManifest{SchemaVersion: BundleSchemaVersion, Kind: BundleKindDR, ID: "20260923-101500-0123456789abcdef", App: "myapp"})
	if _, err := ParseBundleManifest(good); err != nil {
		t.Errorf("valid manifest rejected: %v", err)
	}
	future, _ := json.Marshal(&BundleManifest{SchemaVersion: BundleSchemaVersion + 1, Kind: BundleKindDR, ID: "20260923-101500-0123456789abcdef", App: "myapp"})
	if _, err := ParseBundleManifest(future); err == nil || !strings.Contains(err.Error(), "schema version") {
		t.Errorf("future schema must be refused, got %v", err)
	}
	foreign, _ := json.Marshal(&BundleManifest{SchemaVersion: BundleSchemaVersion, Kind: "data-only", ID: "20260923-101500-0123456789abcdef", App: "myapp"})
	if _, err := ParseBundleManifest(foreign); err == nil || !strings.Contains(err.Error(), "not a disaster-recovery bundle") {
		t.Errorf("foreign kind must be refused, got %v", err)
	}
	if _, err := ParseBundleManifest([]byte("not json")); err == nil {
		t.Errorf("garbage must be refused")
	}
}

// TestDirStore_ListIDs requires manifests: a prefix without a manifest
// (partial upload) never lists as a bundle — the find in ListIDs prints
// only directories that CONTAIN manifest.json.
func TestDirStore_ListIDs(t *testing.T) {
	root := "/var/drstore"
	mock := ssh.NewMockExecutor("h",
		ssh.MockCommand{
			Match:  "find '/var/drstore/myapp/dr'",
			Output: root + "/myapp/dr/20260923-101500-0123456789abcdef\n",
		})
	ids, err := DirBundleStore{Root: root}.ListIDs(context.Background(), mock, "myapp")
	if err != nil {
		t.Fatalf("ListIDs: %v", err)
	}
	if len(ids) != 1 || ids[0] != "20260923-101500-0123456789abcdef" {
		t.Errorf("ids: %v", ids)
	}
}
