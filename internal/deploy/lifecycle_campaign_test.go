package deploy

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/useteploy/teploy/internal/deploy/recovery"
	"github.com/useteploy/teploy/internal/releasemeta"
	"github.com/useteploy/teploy/internal/ssh"
	"github.com/useteploy/teploy/internal/state"
	"strings"
	"testing"
)

func TestCampaignLifecycleStartsAccessoryBeforeCurrentRelease(t *testing.T) {
	inventory := `{"ID":"web","Names":"demo-web-v1","State":"exited","Labels":"teploy.app=demo,teploy.process=web,teploy.version=v1"}
{"ID":"old","Names":"demo-web-v0","State":"exited","Labels":"teploy.app=demo,teploy.process=web,teploy.version=v0"}
{"ID":"db","Names":"demo-db","State":"exited","Labels":"teploy.app=demo,teploy.role=accessory,teploy.accessory=db"}`
	m := ssh.NewMockExecutor("dummy", ssh.MockCommand{Match: "mkdir /deployments/demo/.lock"}, ssh.MockCommand{Match: "docker start"}, ssh.MockCommand{Match: "docker ps --all", Output: inventory})
	seedRound3Lifecycle(m, "demo")
	s := state.NewAppliedState(nil, "container", "external", "")
	s.CurrentHash = "v1"
	b, _ := json.Marshal(s)
	_ = b
	if err := NewLifecycle(m, &bytes.Buffer{}).Start(context.Background(), "demo"); err != nil {
		t.Fatal(err)
	}
	var starts []string
	for _, c := range m.Calls {
		if strings.HasPrefix(c, "docker start ") {
			starts = append(starts, c)
		}
	}
	if len(starts) != 2 || starts[0] != "docker start 'db'" || starts[1] != "docker start 'web'" {
		t.Fatalf("start ownership/order %v", starts)
	}
}
func TestCampaignObserveManagedWorkerAndAccessory(t *testing.T) {
	inv := `{"ID":"web","Names":"demo-web-v1","State":"running","Labels":"teploy.app=demo,teploy.process=web,teploy.version=v1"}
{"ID":"worker","Names":"demo-worker-v1","State":"running","Labels":"teploy.app=demo,teploy.process=worker,teploy.version=v1"}
{"ID":"db","Names":"demo-db","State":"running","Labels":"teploy.app=demo,teploy.role=accessory,teploy.accessory=db"}`
	m := ssh.NewMockExecutor("dummy", ssh.MockCommand{Match: "docker ps --all", Output: inv})
	rec := &releasemeta.Record{SchemaVersion: 1, App: "demo", Hash: "v1", Processes: map[string]string{"web": "", "worker": "work"}}
	b, _ := json.Marshal(rec)
	m.Files["/deployments/demo/meta/v1.json"] = b
	o := Observe(context.Background(), m, "demo", "v2", "v1")
	if o.ForeignCandidates == recovery.Present {
		t.Fatalf("managed processes marked foreign: %+v", o)
	}
}
