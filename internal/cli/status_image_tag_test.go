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
func TestAppStatus_IDCreatedContainerReportsTag(t *testing.T) {
	id := "sha256:0123456789ab" + strings.Repeat("0", 52)
	mock := ssh.NewMockExecutor("h",
		ssh.MockCommand{Match: "docker ps --all --filter label=teploy.app='demo'",
			Output: `{"ID":"c1","Names":"demo-web-v1","Image":"0123456789ab","State":"running","Status":"Up","Labels":{"teploy.app":"demo","teploy.process":"web","teploy.version":"v1"}}`},
		ssh.MockCommand{Match: "docker image inspect --format", Output: id + ` ["demo-build-v1:latest"]`},
	)
	result := collectAppStatus(context.Background(), mock, "demo", time.Unix(0, 0).UTC())
	if len(result.Containers) != 1 {
		t.Fatalf("containers = %+v (errors %+v)", result.Containers, result.Errors)
	}
	data, err := json.Marshal(result.Containers[0])
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	for _, want := range []string{`"image":"demo-build-v1:latest"`, `"image_id":"` + id + `"`, `"image_tags":["demo-build-v1:latest"]`} {
		if !strings.Contains(got, want) {
			t.Errorf("container JSON %s missing %s", got, want)
		}
	}
}
