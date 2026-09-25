package cli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/build"
	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/releasemeta"
	"github.com/useteploy/teploy/internal/ssh"
)

// buildContextRepo makes a git work tree shaped like the Teploy repos that
// leaked on 2026-09-24: a gitignored teploy.home.yml overlay holding a
// password, a gitignored locally built dist/ the Dockerfile COPYs, and a
// fake rsync on PATH that records the file list it is handed.
func buildContextRepo(t *testing.T, teployignore string) (listFile string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	dir := t.TempDir()
	files := map[string]string{
		".gitignore":      "teploy.*.yml\n!teploy.yml\ndist/\n",
		"teploy.yml":      "app: dash\n",
		"Dockerfile":      "FROM node\nCOPY dist/ /app/dist/\nCOPY main.js /app/\n",
		"main.js":         "console.log(1)",
		"teploy.home.yml": "env:\n  TEPLOY_DASH_PASSWORD: hunter2\n",
		"dist/bundle.js":  "built",
	}
	if teployignore != "" {
		files[".teployignore"] = teployignore
	}
	for rel, content := range files {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}, {"-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-q", "-m", "init"}} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	t.Chdir(dir)

	bin := t.TempDir()
	listFile = filepath.Join(bin, "list")
	fake := "#!/bin/sh\ncat > '" + listFile + "'\n"
	if err := os.WriteFile(filepath.Join(bin, "rsync"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return listFile
}

// TestSyncAttemptBuildContext_L14 drives the shared deploy/build upload
// path: the gitignored overlay never reaches rsync, the allowlisted dist/
// does, and the attempt dir is created owner-only before anything lands.
func TestSyncAttemptBuildContext_L14(t *testing.T) {
	listFile := buildContextRepo(t, "!/dist/\n")
	mock := ssh.NewMockExecutor("192.0.2.1",
		ssh.MockCommand{Match: "mkdir -p /deployments/dash/meta/att/", Output: ""},
		ssh.MockCommand{Match: "ls -1t", Output: ""},
	)
	att := releasemeta.MustAttempt("dash", "v1")
	appCfg := &config.AppConfig{App: "dash"}

	var out bytes.Buffer
	remoteDir, err := syncAttemptBuildContext(context.Background(), mock, appCfg, att, build.ModeDockerfile, "192.0.2.1", "root", "", &out, &out)
	if err != nil {
		t.Fatalf("syncAttemptBuildContext: %v\n%s", err, out.String())
	}
	if remoteDir != att.BuildDir() {
		t.Fatalf("remote dir = %s, want %s", remoteDir, att.BuildDir())
	}
	if len(mock.Calls) == 0 || mock.Calls[0] != att.MkdirCmd("build") {
		t.Fatalf("first remote call must be the private attempt mkdir, got %v", mock.Calls)
	}
	raw, err := os.ReadFile(listFile)
	if err != nil {
		t.Fatal(err)
	}
	sent := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
	joined := "|" + strings.Join(sent, "|") + "|"
	for _, want := range []string{"Dockerfile", "main.js", "dist/bundle.js", ".gitignore"} {
		if !strings.Contains(joined, "|"+want+"|") {
			t.Errorf("%s must be uploaded; sent %v", want, sent)
		}
	}
	for _, never := range []string{"teploy.home.yml", "teploy.yml", ".teployignore"} {
		if strings.Contains(joined, "|"+never+"|") {
			t.Errorf("%s must never be uploaded; sent %v", never, sent)
		}
	}
	if !strings.Contains(out.String(), ".gitignore honored") {
		t.Errorf("the sync line must say .gitignore is honored: %s", out.String())
	}
}

// Without the allowlist line the Dockerfile's gitignored COPY source is
// refused before a single remote command runs, naming the fix.
func TestSyncAttemptBuildContext_PreflightNamesTheAllowlistFix(t *testing.T) {
	buildContextRepo(t, "")
	mock := ssh.NewMockExecutor("192.0.2.1")
	att := releasemeta.MustAttempt("dash", "v1")

	var out bytes.Buffer
	_, err := syncAttemptBuildContext(context.Background(), mock, &config.AppConfig{App: "dash"}, att, build.ModeDockerfile, "192.0.2.1", "root", "", &out, &out)
	if err == nil || !strings.Contains(err.Error(), "`!/dist/`") {
		t.Fatalf("want the allowlist fix in the error, got %v", err)
	}
	if len(mock.Calls) != 0 {
		t.Fatalf("preflight must fail before any remote effect, got %v", mock.Calls)
	}
}
