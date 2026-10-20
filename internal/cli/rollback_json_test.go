package cli

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/useteploy/teploy/internal/ssh"
)

func TestRollbackResultJSON(t *testing.T) {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(rollbackResult{
		MachineInterface: MachineInterface,
		App:              "blog",
		Server:           "infra-home",
		From:             "abc1234",
		To:               "9f8e7d6",
		OK:               true,
	}); err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"machine_interface", "app", "server", "from", "to", "ok"} {
		if _, ok := doc[k]; !ok {
			t.Fatalf("result missing %q: %v", k, doc)
		}
	}
	if doc["ok"] != true || doc["from"] != "abc1234" || doc["to"] != "9f8e7d6" {
		t.Fatalf("result = %v", doc)
	}
}

// rollbackStateFor frames a state.json the way ReadRemoteFile reads it.
func rollbackStateFor(t *testing.T, current, previous string) string {
	t.Helper()
	ts := time.Now().UTC().Format(time.RFC3339Nano)
	return "present\n" + `{"schema_version":2,"deployment_type":"container","ingress_mode":"caddy","updated_at":"` + ts + `","current_hash":"` + current + `","previous_hash":"` + previous + `","operation_id":"op","generation":4}`
}

func TestRollbackLanded(t *testing.T) {
	mock := ssh.NewMockExecutor("h",
		ssh.MockCommand{Match: "if [ ! -e '/deployments/blog/state.json' ]", Output: rollbackStateFor(t, "9f8e7d6", "abc1234")},
	)
	from, to := rollbackLanded(t.Context(), mock, "blog")
	if from != "abc1234" || to != "9f8e7d6" {
		t.Fatalf("landed = %q -> %q, want abc1234 -> 9f8e7d6", from, to)
	}
}

func TestStaticReleaseRow(t *testing.T) {
	if r := staticReleaseRow("r1", "r1"); !r.Current || r.RollbackAvailable || r.Reason != "current release" {
		t.Fatalf("current static row = %+v", r)
	}
	if r := staticReleaseRow("r0", "r1"); r.Current || !r.RollbackAvailable || r.Reason != "" {
		t.Fatalf("retained static row = %+v", r)
	}
}
