package docker

import (
	"context"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/ssh"
)

func TestIsImageID(t *testing.T) {
	full := strings.Repeat("ab", 32)
	for ref, want := range map[string]bool{
		"0123456789ab":      true,
		full:                true,
		"sha256:" + full:    true,
		"myapp:v1":          false,
		"0123456789":        false,
		"0123456789AB":      false,
		"sha256:0123456789": false,
	} {
		if got := IsImageID(ref); got != want {
			t.Errorf("IsImageID(%q) = %v, want %v", ref, got, want)
		}
	}
}

// Ship wave-9 lesson: containers created by image ID report the ID, never
// the artifact tag. Resolution reports the tag and keeps the ID.
func TestResolveImageTags(t *testing.T) {
	idA := "sha256:0123456789ab" + strings.Repeat("0", 52)
	idB := "sha256:fedcba987654" + strings.Repeat("0", 52)
	mock := ssh.NewMockExecutor("h",
		ssh.MockCommand{Match: "docker image inspect --format", Output: idA + ` ["app-build-v1:latest","registry/app:v1"]` + "\n" + idB + " []\n"},
	)
	in := []Container{
		{Name: "app-web-v1", Image: "0123456789ab"},
		{Name: "app-web-v0", Image: "fedcba987654"},   // untagged: keeps the ID
		{Name: "app-web-gone", Image: "aaaaaaaaaaaa"}, // image removed: keeps the ID
		{Name: "app-postgres", Image: "postgres:16"},  // created by name: untouched
	}
	out := NewClient(mock).ResolveImageTags(context.Background(), in)

	if out[0].Image != "app-build-v1:latest" || out[0].ImageID != idA || len(out[0].ImageTags) != 2 {
		t.Errorf("ID-created container not resolved: %+v", out[0])
	}
	if out[1].Image != "fedcba987654" || out[1].ImageID != idB || out[1].ImageTags != nil {
		t.Errorf("untagged image: %+v", out[1])
	}
	if out[2].Image != "aaaaaaaaaaaa" || out[2].ImageID != "" {
		t.Errorf("missing image must keep the ID: %+v", out[2])
	}
	if out[3].Image != "postgres:16" || out[3].ImageID != "" {
		t.Errorf("name-created container changed: %+v", out[3])
	}
	if in[0].Image != "0123456789ab" {
		t.Error("input slice mutated")
	}
	if len(mock.Calls) != 1 || !strings.Contains(mock.Calls[0], "'0123456789ab'") {
		t.Errorf("want one batched inspect, got %v", mock.Calls)
	}
}

// No ID-form image -> no docker call at all.
func TestResolveImageTags_NoIDsNoCall(t *testing.T) {
	mock := ssh.NewMockExecutor("h")
	out := NewClient(mock).ResolveImageTags(context.Background(), []Container{{Image: "nginx:1.27"}})
	if len(mock.Calls) != 0 || out[0].Image != "nginx:1.27" {
		t.Fatalf("calls=%v out=%+v", mock.Calls, out)
	}
}

// A failed inspect is best-effort: IDs stay, nothing errors.
func TestResolveImageTags_InspectFailureKeepsIDs(t *testing.T) {
	mock := ssh.NewMockExecutor("h", ssh.MockCommand{Match: "docker image inspect", Output: "garbage line\n"})
	out := NewClient(mock).ResolveImageTags(context.Background(), []Container{{Image: "0123456789ab"}})
	if out[0].Image != "0123456789ab" || out[0].ImageID != "" {
		t.Fatalf("out=%+v", out[0])
	}
}
