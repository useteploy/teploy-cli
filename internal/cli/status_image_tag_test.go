package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy/internal/ssh"
)

// Ship wave-9 lesson: `app status --json` reported an ID-created web
// container's image as the bare short ID, so a consumer matching it against
// the artifact tag never matched. image carries the tag; image_id the ID.
// Exercised through the batched fleet path: one docker ps for the host, one
// image inspect for every ID-referenced container.
func TestAppStatus_IDCreatedContainerReportsTag(t *testing.T) {
	id := "sha256:0123456789ab" + strings.Repeat("0", 52)
	mock := ssh.NewMockExecutor("h",
		ssh.MockCommand{Match: "for f in /deployments/*/state.json", Output: "demo\n"},
		ssh.MockCommand{Match: "for app in", Output: snapshotLines(
			"A demo", "S absent", "G absent", "L ", "M 0", "R 0",
		)},
		ssh.MockCommand{Match: "docker ps --all", Output: `{"ID":"c1","Names":"demo-web-v1","Image":"0123456789ab","State":"running","Status":"Up","CreatedAt":"2026-07-22 10:00:00 +0000 UTC","Labels":{"teploy.app":"demo","teploy.process":"web","teploy.version":"v1"}}`},
		ssh.MockCommand{Match: "docker image inspect --format", Output: id + ` ["demo-build-v1:latest"]`},
	)
	got := collectAppList(context.Background(), mock, time.Unix(0, 0).UTC())
	if len(got.Apps) != 1 || len(got.Apps[0].Containers) != 1 {
		t.Fatalf("apps = %#v (errors %#v)", got.Apps, got.Errors)
	}
	data, err := json.Marshal(got.Apps[0].Containers[0])
	if err != nil {
		t.Fatal(err)
	}
	gotJSON := string(data)
	for _, want := range []string{`"image":"demo-build-v1:latest"`, `"image_id":"` + id + `"`, `"image_tags":["demo-build-v1:latest"]`} {
		if !strings.Contains(gotJSON, want) {
			t.Errorf("container JSON %s missing %s", gotJSON, want)
		}
	}
	if len(mock.Calls) != 4 {
		t.Fatalf("app list took %d round trips, want 4: %q", len(mock.Calls), mock.Calls)
	}
}
