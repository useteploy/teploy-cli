package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/ssh"
)

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out.String())
	}
	return strings.TrimSpace(out.String())
}

// provenanceRepo builds a committed git worktree with a Dockerfile and a
// source file, returning the repo root.
func provenanceRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitRun(t, dir, "init", "-q")
	gitRun(t, dir, "-c", "user.email=ops@example.com", "-c", "user.name=ops",
		"commit", "--allow-empty", "-qm", "root")
	for rel, content := range map[string]string{
		"Dockerfile": "FROM alpine\n",
		"main.go":    "package main\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "-c", "user.email=ops@example.com", "-c", "user.name=ops", "commit", "-qm", "app")
	return dir
}

func provenanceMock(resolvedID string) *ssh.MockExecutor {
	return ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "docker image inspect --format '{{.Id}}'", Output: resolvedID},
	)
}

func TestResolveDeployProvenance_BuildPath(t *testing.T) {
	dir := provenanceRepo(t)
	rev, err := gitRevisionIn(dir)
	if err != nil {
		t.Fatal(err)
	}

	appCfg := &config.AppConfig{App: "myapp", Platform: "linux/arm64"}
	appCfg.SourceRevision = rev

	prov := resolveDeployProvenance(context.Background(), provenanceMock("sha256:"+strings.Repeat("b", 64)), io.Discard,
		appCfg, dir, "myapp-build-v1", "v1", strings.Repeat("c", 64), true)

	if prov.App != "myapp" || prov.Release != "v1" {
		t.Errorf("identity fields wrong: %+v", prov)
	}
	if prov.Revision != rev {
		t.Errorf("revision not captured: %q want %q", prov.Revision, rev)
	}
	if prov.Dirty {
		t.Errorf("a clean committed worktree must not be flagged dirty")
	}
	if prov.ImageRef != "myapp-build-v1" || prov.ImageDigest != "sha256:"+strings.Repeat("b", 64) {
		t.Errorf("image identity not captured: %+v", prov)
	}
	if prov.DigestPinned {
		t.Errorf("a built tag is not digest-pinned")
	}
	if prov.ContextPath != "." || prov.ContextFingerprint == "" {
		t.Errorf("build context identity not captured: %+v", prov)
	}
	if prov.Dockerfile != "Dockerfile" || prov.DockerfileSHA256 != fileSHA256(t, filepath.Join(dir, "Dockerfile")) {
		t.Errorf("dockerfile identity not captured: %+v", prov)
	}
	if prov.Platform != "linux/arm64" {
		t.Errorf("configured platform not captured: %q", prov.Platform)
	}
	if prov.ManifestSHA256 != strings.Repeat("c", 64) {
		t.Errorf("effective-config digest not captured: %q", prov.ManifestSHA256)
	}
}

// The dirty-worktree flag must flow into provenance: a deploy building
// uncommitted changes says so in the record that outlives it.
func TestResolveDeployProvenance_DirtyWorktreeFlagged(t *testing.T) {
	dir := provenanceRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main // WIP\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rev, err := gitRevisionIn(dir)
	if err != nil {
		t.Fatal(err)
	}
	appCfg := &config.AppConfig{App: "myapp"}
	appCfg.SourceRevision = rev

	prov := resolveDeployProvenance(context.Background(), provenanceMock(""), io.Discard,
		appCfg, dir, "myapp-build-v1", "v1", "", true)
	if !prov.Dirty {
		t.Fatal("a worktree with uncommitted changes must be flagged dirty in provenance")
	}
}

func TestResolveDeployProvenance_PrebuiltImage(t *testing.T) {
	pinned := "registry.example.com/myapp@sha256:" + strings.Repeat("7", 64)
	appCfg := &config.AppConfig{App: "myapp"}

	prov := resolveDeployProvenance(context.Background(), provenanceMock("sha256:"+strings.Repeat("b", 64)), io.Discard,
		appCfg, ".", pinned, "sha256-777777777777", strings.Repeat("c", 64), false)

	if !prov.DigestPinned {
		t.Error("a digest-pinned reference must be recorded as pinned")
	}
	// Like-for-like with the deployed record: a pinned ref is identified by
	// ITS manifest digest, not docker's local image ID for those bytes.
	if prov.ImageDigest != "sha256:"+strings.Repeat("7", 64) {
		t.Errorf("pinned ref digest not captured: %q", prov.ImageDigest)
	}
	if prov.ImageRef != pinned {
		t.Errorf("requested ref not captured: %q", prov.ImageRef)
	}
	if prov.ContextPath != "" || prov.ContextFingerprint != "" || prov.Dockerfile != "" || prov.DockerfileSHA256 != "" || prov.Platform != "" {
		t.Errorf("prebuilt deploy must not invent build provenance: %+v", prov)
	}
}

func TestResolveDeployProvenance_MutableTagResolvesContentID(t *testing.T) {
	appCfg := &config.AppConfig{App: "myapp"}
	prov := resolveDeployProvenance(context.Background(), provenanceMock("sha256:"+strings.Repeat("b", 64)), io.Discard,
		appCfg, ".", "myapp:latest", "1750000000", "", false)
	if prov.DigestPinned {
		t.Error("a tag reference is mutable, not pinned")
	}
	if prov.ImageDigest != "sha256:"+strings.Repeat("b", 64) {
		t.Errorf("the resolved immutable ID must be captured for a mutable tag: %q", prov.ImageDigest)
	}
}

// C04 retry stability: a response-loss retry of the SAME request reuses the
// same provenance — resolution is a pure function of the source tree and
// config, so the second attempt cannot re-resolve to a different source.
func TestResolveDeployProvenance_RetryStability(t *testing.T) {
	dir := provenanceRepo(t)
	rev, err := gitRevisionIn(dir)
	if err != nil {
		t.Fatal(err)
	}
	appCfg := &config.AppConfig{App: "myapp", Platform: "linux/arm64"}
	appCfg.SourceRevision = rev

	first := resolveDeployProvenance(context.Background(), provenanceMock("sha256:"+strings.Repeat("b", 64)), io.Discard,
		appCfg, dir, "myapp-build-v1", "v1", strings.Repeat("c", 64), true)
	second := resolveDeployProvenance(context.Background(), provenanceMock("sha256:"+strings.Repeat("b", 64)), io.Discard,
		appCfg, dir, "myapp-build-v1", "v1", strings.Repeat("c", 64), true)

	if !reflect.DeepEqual(first, second) {
		t.Fatalf("the same request resolved differently across attempts:\nfirst:  %+v\nsecond: %+v", first, second)
	}
}

func fileSHA256(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return sha256Hex(data)
}
