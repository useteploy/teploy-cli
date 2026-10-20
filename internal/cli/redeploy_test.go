package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/releasemeta"
	"github.com/useteploy/teploy/internal/ssh"
)

func TestEventTrigger(t *testing.T) {
	if got := eventTrigger(context.Background()); got != "" {
		t.Fatalf("plain ctx trigger = %q", got)
	}
	if got := eventTrigger(withRedeployTrigger(context.Background())); got != "redeploy" {
		t.Fatalf("redeploy ctx trigger = %q", got)
	}
}

func TestRedeployRefusesStaticFolder(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "teploy.yml"), []byte("app: site\ntype: static\ndomain: site.example.com\nserver: prod\nsource: ./dist\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cwd, _ := os.Getwd()
	defer os.Chdir(cwd)
	os.Chdir(dir)
	_, err := runRedeploy(context.Background(), &Flags{}, "")
	if err == nil || !strings.Contains(err.Error(), "type:static") {
		t.Fatalf("static refusal: %v", err)
	}
}

func TestRedeployNeedsServer(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "teploy.yml"), []byte("app: blog\nimage: nginx:1\ndomain: blog.example.com\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cwd, _ := os.Getwd()
	defer os.Chdir(cwd)
	os.Chdir(dir)
	_, err := runRedeploy(context.Background(), &Flags{Host: ""}, "")
	if err == nil || !strings.Contains(err.Error(), "no server") {
		t.Fatalf("expected no-server refusal, got %v", err)
	}
}

// TestResolveRedeployImage pins the "recorded image" rule: a tag that
// still resolves to the recorded digest is used verbatim; a tag that has
// moved falls back to the recorded digest; a digest-pinned ref is used
// as-is without any inspection.
func TestResolveRedeployImage(t *testing.T) {
	ctx := context.Background()
	t.Run("tag unchanged", func(t *testing.T) {
		mock := ssh.NewMockExecutor("h",
			ssh.MockCommand{Match: "err=$(mktemp); if docker image inspect 'nginx:1'", Output: "exists"},
			ssh.MockCommand{Match: "docker image inspect --format '{{.Id}}' 'nginx:1'", Output: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		)
		got, err := resolveRedeployImage(ctx, mock, &releasemeta.Record{ImageRef: "nginx:1", ImageDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"})
		if err != nil || got != "nginx:1" {
			t.Fatalf("got %q (%v), want the tag", got, err)
		}
	})
	t.Run("tag moved", func(t *testing.T) {
		mock := ssh.NewMockExecutor("h",
			ssh.MockCommand{Match: "err=$(mktemp); if docker image inspect 'nginx:1'", Output: "exists"},
			ssh.MockCommand{Match: "docker image inspect --format '{{.Id}}' 'nginx:1'", Output: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
		)
		got, err := resolveRedeployImage(ctx, mock, &releasemeta.Record{ImageRef: "nginx:1", ImageDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"})
		if err != nil || got != "sha256:"+strings.Repeat("a", 64) {
			t.Fatalf("got %q (%v), want the recorded digest", got, err)
		}
	})
	t.Run("digest pinned", func(t *testing.T) {
		mock := ssh.NewMockExecutor("h")
		got, err := resolveRedeployImage(ctx, mock, &releasemeta.Record{ImageRef: "nginx:1@sha256:" + strings.Repeat("a", 64), ImageDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"})
		if err != nil || got != "nginx:1@sha256:"+strings.Repeat("a", 64) {
			t.Fatalf("got %q (%v), want the pinned ref", got, err)
		}
		if len(mock.Calls) != 0 {
			t.Fatalf("a pinned ref must not be inspected: %v", mock.Calls)
		}
	})
	t.Run("absent image is pulled then verified", func(t *testing.T) {
		mock := ssh.NewMockExecutor("h",
			ssh.MockCommand{Match: "err=$(mktemp); if docker image inspect 'nginx:1'", Output: "missing"},
			ssh.MockCommand{Match: "docker pull nginx:1", Output: "nginx:1 pulled"},
			ssh.MockCommand{Match: "docker image inspect --format '{{.Id}}' 'nginx:1'", Err: errImageMissing, Output: ""},
		)
		if _, err := resolveRedeployImage(ctx, mock, &releasemeta.Record{ImageRef: "nginx:1", ImageDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}); err == nil {
			t.Fatal("a pull that still cannot resolve the id must error, not guess")
		}
	})
	t.Run("no recorded digest falls back to the ref", func(t *testing.T) {
		mock := ssh.NewMockExecutor("h",
			ssh.MockCommand{Match: "err=$(mktemp); if docker image inspect 'nginx:1'", Output: "exists"},
			ssh.MockCommand{Match: "docker image inspect --format '{{.Id}}' 'nginx:1'", Output: "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"},
		)
		got, err := resolveRedeployImage(ctx, mock, &releasemeta.Record{ImageRef: "nginx:1"})
		if err != nil || got != "nginx:1" {
			t.Fatalf("got %q (%v), want the ref", got, err)
		}
	})
}

// errImageMissing models ImageExists false: the inspect probe the client
// runs to check presence fails with "no such image".
var errImageMissing = &imageMissingError{}

type imageMissingError struct{}

func (e *imageMissingError) Error() string { return "exit status 1: Error: No such image" }
