package deploy

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/releasemeta"
	"github.com/useteploy/teploy/internal/ssh"
	"github.com/useteploy/teploy/internal/state"
)

func seedRound3Lifecycle(mock *ssh.MockExecutor, app string) {
	cfg := &config.AppConfig{App: app, Image: "image:v1", Accessories: map[string]config.AccessoryConfig{"db": {Image: "postgres:16"}}}
	manifest, digest, _ := config.NormalizeAndDigest(cfg, cfg.Image)
	applied := state.NewAppliedState(nil, "container", "external", "")
	applied.CurrentHash = "v1"
	applied.AppliedManifest = manifest
	applied.ManifestSHA256 = digest
	data, _ := json.Marshal(applied)
	mock.Files["/deployments/"+app+"/state.json"] = data
	record := &releasemeta.Record{SchemaVersion: 1, App: app, Hash: "v1", Processes: map[string]string{"web": "serve", "worker": "work"}}
	data, _ = json.Marshal(record)
	mock.Files["/deployments/"+app+"/meta/v1.json"] = data
}
func TestRound3LifecycleSharedWorldExcludesPreviewOldAndUnknown(t *testing.T) {
	inventory := `{"ID":"web","Names":"demo-web-v1","State":"running","Labels":{"teploy.app":"demo","teploy.process":"web","teploy.version":"v1"}}
{"ID":"worker","Names":"demo-worker-v1","State":"running","Labels":{"teploy.app":"demo","teploy.process":"worker","teploy.version":"v1"}}
{"ID":"db","Names":"demo-db","State":"running","Labels":{"teploy.app":"demo","teploy.role":"accessory","teploy.accessory":"db"}}
{"ID":"old","Names":"demo-web-v0","State":"running","Labels":{"teploy.app":"demo","teploy.process":"web","teploy.version":"v0"}}
{"ID":"preview","Names":"demo-web-v1-preview","State":"running","Labels":{"teploy.app":"demo","teploy.process":"web","teploy.version":"v1","teploy.preview":"feature"}}
{"ID":"unknown","Names":"demo-mystery-v1","State":"running","Labels":{"teploy.app":"demo","teploy.process":"mystery","teploy.version":"v1"}}
{"ID":"removed-db","Names":"demo-removed","State":"exited","Labels":{"teploy.app":"demo","teploy.role":"accessory","teploy.accessory":"removed"}}`
	for _, operation := range []string{"stop", "start", "restart"} {
		mock := ssh.NewMockExecutor("host", ssh.MockCommand{Match: "mkdir /deployments/demo/.lock"}, ssh.MockCommand{Match: "docker stop --time 5", Output: ""}, ssh.MockCommand{Match: "docker start", Output: ""}, ssh.MockCommand{Match: "docker ps --all", Output: inventory})
		seedRound3Lifecycle(mock, "demo")
		lifecycle := NewLifecycle(mock, &bytes.Buffer{})
		var err error
		switch operation {
		case "stop":
			err = lifecycle.Stop(context.Background(), "demo", 5)
		case "start":
			err = lifecycle.Start(context.Background(), "demo")
		case "restart":
			err = lifecycle.Restart(context.Background(), "demo", 5)
		}
		if err != nil {
			t.Fatalf("%s: %v", operation, err)
		}
		effects := 0
		for _, command := range mock.Calls {
			if strings.HasPrefix(command, "docker start ") || strings.HasPrefix(command, "docker stop ") {
				effects++
				for _, excluded := range []string{"'old'", "'preview'", "'unknown'", "'removed-db'"} {
					if strings.Contains(command, excluded) {
						t.Fatalf("%s touched %s", operation, command)
					}
				}
			}
		}
		want := 3
		if operation == "restart" {
			want = 6
		}
		if effects != want {
			t.Fatalf("%s effects %d want %d", operation, effects, want)
		}
	}
}
