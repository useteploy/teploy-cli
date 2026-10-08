package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/useteploy/teploy/internal/ssh"
	"strings"
	"testing"
)

func TestBundleManifestRejectsUnsafeMembersBeforeStaging(t *testing.T) {
	mutations := []func(*BundleManifest){
		func(m *BundleManifest) { m.App = "../demo" },
		func(m *BundleManifest) { m.Snapshots[0].Name = "../escape" },
		func(m *BundleManifest) { m.Snapshots[0].Artifact = "../../escape.sql.gz" },
		func(m *BundleManifest) { m.Secrets.Included = []string{"env/credentials/../../escape"} },
		func(m *BundleManifest) {
			m.ReleaseRecords = []json.RawMessage{json.RawMessage(`{"hash":"../../escape"}`)}
		},
	}
	for i, mutation := range mutations {
		manifest := drTestManifest()
		mutation(manifest)
		data, _ := json.Marshal(manifest)
		if _, err := ParseBundleManifest(data); err == nil {
			t.Fatalf("unsafe mutation %d accepted", i)
		}
	}
}

func TestS3BundleListRecognizesPrefixRows(t *testing.T) {
	mock := ssh.NewMockExecutor("host", ssh.MockCommand{Match: "aws", Output: "                           PRE 20260923-101500-0123456789abcdef/\n2026-09-23 10:15:00 12 manifest.json\n"})
	ids, err := (S3BundleStore{S3: S3Config{Bucket: "dummy"}}).ListIDs(context.Background(), mock, "myapp")
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != drTestBundleID {
		t.Fatalf("prefix missing: %v", ids)
	}
}

func TestDRCutoverPublishesFreshTargetGeneration(t *testing.T) {
	manifest := drTestManifest()
	mock := drCutoverMock(drOKReceipt(), manifest)
	mock.Files["/deployments/myapp/state.json"] = []byte(`{"schema_version":2,"generation":9,"current_hash":"v9"}`)
	mock.Files["/deployments/myapp/.generation"] = []byte("12\n")
	receipt, err := NewClient(mock, &bytes.Buffer{}).CutoverBundle(context.Background(), BundleRestoreOptions{App: "myapp", ID: manifest.ID, Config: drTestConfig()})
	if err != nil {
		t.Fatal(err)
	}
	if !receipt.OK {
		t.Fatal("completed receipt not marked successful")
	}
	var got struct {
		Generation uint64 `json:"generation"`
	}
	json.Unmarshal(mock.Files["/deployments/myapp/state.json"], &got)
	if got.Generation != 13 {
		t.Fatalf("historical generation reinstated: %d", got.Generation)
	}
	if strings.TrimSpace(string(mock.Files["/deployments/myapp/.generation"])) != "13" {
		t.Fatal("sidecar disagrees")
	}
}

func TestDRCutoverPreservesLiveAgeKeyAndRefusesBeforeStop(t *testing.T) {
	manifest := drTestManifest()
	manifest.Secrets = SecretsRecord{Mode: "encrypted", AgeKeyIncluded: true, Included: []string{"secrets/API_KEY.age"}}
	mock := drCutoverMock(drOKReceipt(), manifest, ssh.MockCommand{Match: "age -d -i '/deployments/.age-key'", Err: fmt.Errorf("wrong target key")})
	mock.Files["/deployments/.age-key"] = []byte("dummy-key-B")
	_, err := NewClient(mock, &bytes.Buffer{}).CutoverBundle(context.Background(), BundleRestoreOptions{App: "myapp", ID: manifest.ID, Config: drTestConfig()})
	if err == nil {
		t.Fatal("undecryptable live key accepted")
	}
	for _, call := range mock.Calls {
		if strings.Contains(call, "docker stop") {
			t.Fatalf("stopped before key proof: %s", call)
		}
	}
	if string(mock.Files["/deployments/.age-key"]) != "dummy-key-B" {
		t.Fatal("global key overwritten")
	}
}

func TestBundleStopsWorkersAndReportsRestartFailure(t *testing.T) {
	mock := drBaseMock(ssh.MockCommand{Match: "docker ps --filter label=teploy.app='myapp' --filter label=teploy.process ", Output: "myapp-web\nmyapp-worker\n"}, ssh.MockCommand{Match: "docker stop", Output: ""}, ssh.MockCommand{Match: "docker start 'myapp-worker'", Err: fmt.Errorf("restart refused")}, ssh.MockCommand{Match: "docker start", Output: ""})
	_, err := NewClient(mock, &bytes.Buffer{}).CreateBundle(context.Background(), BundleOptions{App: "myapp", Config: drTestConfig(), StopApp: true}, DirBundleStore{Root: "/var/drstore"})
	if err == nil || !strings.Contains(err.Error(), "myapp-worker") {
		t.Fatalf("restart error hidden: %v", err)
	}
	found := false
	for _, call := range mock.Calls {
		if call == "docker stop 'myapp-worker'" {
			found = true
		}
	}
	if !found {
		t.Fatal("worker kept writing during snapshot")
	}
}

func TestCutoverRefusesAlteredValidatedManifest(t *testing.T) {
	manifest := drTestManifest()
	mock := drCutoverMock(drOKReceipt(), manifest)
	manifest.Snapshots[0].Image = "postgres:17"
	changed, _ := json.Marshal(manifest)
	mock.Files[DRStagingPath("myapp", manifest.ID)+"/"+strings.Repeat("b", 32)+"/manifest.json"] = changed
	_, err := NewClient(mock, &bytes.Buffer{}).CutoverBundle(context.Background(), BundleRestoreOptions{App: "myapp", ID: manifest.ID, Config: drTestConfig()})
	if err == nil || !strings.Contains(err.Error(), "does not bind") {
		t.Fatalf("changed staged manifest accepted: %v", err)
	}
	for _, call := range mock.Calls {
		if strings.Contains(call, "docker stop") {
			t.Fatal("effect before receipt binding refusal")
		}
	}
}

func TestBundleStopErrorStillAttemptsRecoveryForUncertainTarget(t *testing.T) {
	mock := drBaseMock(ssh.MockCommand{Match: "docker ps --filter label=teploy.app='myapp' --filter label=teploy.process ", Output: "myapp-web\nmyapp-worker\n"}, ssh.MockCommand{Match: "docker stop 'myapp-worker'", Err: fmt.Errorf("transport died after stop")}, ssh.MockCommand{Match: "docker stop", Output: ""}, ssh.MockCommand{Match: "docker start", Output: ""})
	_, err := NewClient(mock, &bytes.Buffer{}).CreateBundle(context.Background(), BundleOptions{App: "myapp", Config: drTestConfig(), StopApp: true}, DirBundleStore{Root: "/var/drstore"})
	if err == nil {
		t.Fatal("stop error hidden")
	}
	found := false
	for _, call := range mock.Calls {
		if call == "docker start 'myapp-worker'" {
			found = true
		}
	}
	if !found {
		t.Fatal("uncertain stop outcome left without recovery")
	}
}
