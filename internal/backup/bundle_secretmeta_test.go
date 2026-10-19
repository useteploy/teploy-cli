package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/ssh"
)

const oldShapeRecord = `{"schema_version":1,"app":"myapp","hash":"abc123","cmd":"npm start",` +
	`"env":{"SESSION_SALT":"salt-secret-1"},` +
	`"recreate":{"name":"myapp-web-abc123","env":["DB_PASSWORD=pw-secret-2","DATABASE_URL=postgres://u:urlpw-3@db/x","PORT=3000"]}}`

func assertNoOldSecrets(t *testing.T, where string, data []byte) {
	t.Helper()
	for _, s := range []string{"salt-secret-1", "pw-secret-2", "urlpw-3"} {
		if bytes.Contains(data, []byte(s)) {
			t.Errorf("%s contains a secret value", where)
		}
	}
}

// A DR bundle is a copy that travels: records written before redaction must
// be exported without their secret env values, other fields intact.
func TestCollectReleaseRecords_RedactsSecretEnv(t *testing.T) {
	mock := ssh.NewMockExecutor("dr-host",
		ssh.MockCommand{Match: "find '/deployments/myapp/meta'", Output: "/deployments/myapp/meta/abc123.json\n"},
	)
	mock.Files["/deployments/myapp/meta/abc123.json"] = []byte(oldShapeRecord)
	records, err := NewClient(mock, &bytes.Buffer{}).collectReleaseRecords(context.Background(), "myapp")
	if err != nil || len(records) != 1 {
		t.Fatalf("collect: %v (%d records)", err, len(records))
	}
	assertNoOldSecrets(t, "bundled record", records[0])
	var rec map[string]any
	if err := json.Unmarshal(records[0], &rec); err != nil {
		t.Fatal(err)
	}
	if rec["cmd"] != "npm start" || rec["hash"] != "abc123" {
		t.Errorf("non-env fields lost: %v", rec)
	}
	if !strings.Contains(string(records[0]), `"PORT=3000"`) {
		t.Error("non-secret value lost")
	}
}

// Restoring a bundle made before redaction must not put secret values back
// at rest on the target.
func TestInstallReleaseRecords_RedactsOldBundles(t *testing.T) {
	mock := ssh.NewMockExecutor("dr-host")
	err := NewClient(mock, &bytes.Buffer{}).installReleaseRecords(context.Background(), "/deployments/myapp", []json.RawMessage{json.RawMessage(oldShapeRecord)})
	if err != nil {
		t.Fatal(err)
	}
	written := mock.Files["/deployments/myapp/meta/abc123.json"]
	if written == nil {
		t.Fatal("record not restored")
	}
	assertNoOldSecrets(t, "restored record", written)
}
