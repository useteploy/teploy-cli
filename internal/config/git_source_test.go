package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestValidateGitRepository(t *testing.T) {
	ok := []string{
		"git@github.com:useteploy/teploy.git",
		"ssh://git@github.com:22/useteploy/teploy.git",
		"ssh://git@forge.internal:2222/org/repo.git",
		"https://github.com/useteploy/teploy.git",
		"https://user@github.com/useteploy/teploy.git",
		"file:///srv/git/repo.git",
	}
	for _, repo := range ok {
		if err := ValidateGitRepository(repo); err != nil {
			t.Errorf("ValidateGitRepository(%q) = %v, want ok", repo, err)
		}
	}
	bad := map[string]string{
		"":                           "empty",
		"http://github.com/org/repo": "unencrypted",
		"https://user:pass@host/r":   "password",
		"git://host/r":               "git protocol",
		"-oProxyCommand=x":           "dash",
		"/local/path":                "bare path",
		"ext::sh -c id":              "ext",
		"git@github.com:useteploy/x git push --force": "space",
	}
	for repo, why := range bad {
		if err := ValidateGitRepository(repo); err == nil {
			t.Errorf("ValidateGitRepository(%q) accepted (%s)", repo, why)
		}
	}
	if !GitRepositoryIsLocal("file:///x") || GitRepositoryIsLocal("ssh://x/y") {
		t.Error("GitRepositoryIsLocal misclassifies")
	}
}

func TestValidateGitBranch(t *testing.T) {
	for _, b := range []string{"main", "develop", "release-1.2", "v1_x"} {
		if err := ValidateGitBranch(b); err != nil {
			t.Errorf("ValidateGitBranch(%q) = %v", b, err)
		}
	}
	for _, b := range []string{"", "-x", ".x", "x.", "a..b", "a//b", "a/b", "x.lock", "a b", "a~", "x^", "@{x"} {
		if err := ValidateGitBranch(b); err == nil {
			t.Errorf("ValidateGitBranch(%q) accepted", b)
		}
	}
}

func TestParseGitPoll(t *testing.T) {
	for _, v := range []string{"1m", "5m", "30m", "2h", "24h"} {
		if _, err := ParseGitPoll(v); err != nil {
			t.Errorf("ParseGitPoll(%q) = %v", v, err)
		}
	}
	for _, v := range []string{"0s", "30s", "48h", "5x", "off"} {
		if _, err := ParseGitPoll(v); err == nil {
			t.Errorf("ParseGitPoll(%q) accepted", v)
		}
	}
	if d, _ := ParseGitPoll("90s"); d != 90*time.Second {
		t.Errorf("ParseGitPoll(90s) = %v", d)
	}
}

func TestLoadAppRefusesGitInFolder(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) {
		t.Helper()
		if err := osWriteFile(dir+"/teploy.yml", body); err != nil {
			t.Fatal(err)
		}
	}
	write("app: demo\ndomain: demo.invalid\ngit:\n  repository: git@github.com:org/repo\n  branch: main\n")
	_, err := LoadApp(dir)
	if err == nil || !strings.Contains(err.Error(), "Teploy-home") {
		t.Fatalf("folder LoadApp with git: = %v, want Teploy-home refusal", err)
	}
}

func TestLoadAppBytesGitValidation(t *testing.T) {
	load := func(body string) error {
		t.Helper()
		_, err := LoadAppBytes([]byte(body))
		return err
	}
	if err := load("app: demo\ndomain: demo.invalid\ngit:\n  repository: git@github.com:org/repo\n  branch: main\n"); err != nil {
		t.Fatalf("valid git: rejected: %v", err)
	}
	if err := load("app: demo\ndomain: demo.invalid\nimage: nginx:1\ngit:\n  repository: git@github.com:org/repo\n  branch: main\n"); err == nil || !strings.Contains(err.Error(), "image") {
		t.Fatalf("image+git = %v, want refusal", err)
	}
	if err := load("app: demo\ndomain: demo.invalid\ngit:\n  branch: main\n"); err == nil || !strings.Contains(err.Error(), "repository") {
		t.Fatalf("missing repository = %v", err)
	}
	if err := load("app: demo\ndomain: demo.invalid\ngit:\n  repository: git@github.com:org/repo\n  branch: main\n  submodules: true\n"); err == nil || !strings.Contains(err.Error(), "submodules") {
		t.Fatalf("submodules: true = %v, want refusal", err)
	}
	if err := load("app: demo\ndomain: demo.invalid\ngit:\n  repository: git@github.com:org/repo\n  branch: main\n  poll: 30s\n"); err == nil || !strings.Contains(err.Error(), "poll") {
		t.Fatalf("poll: 30s = %v, want refusal", err)
	}
	if err := load("app: demo\ndomain: demo.invalid\nbuild_secrets: [TOKEN]\n"); err == nil || !strings.Contains(err.Error(), "build_secrets") {
		t.Fatalf("build_secrets without git = %v, want refusal", err)
	}
	if err := load("app: demo\ndomain: demo.invalid\ngit:\n  repository: git@github.com:org/repo\n  branch: main\n  paths: [\"../escape\"]\n"); err == nil || !strings.Contains(err.Error(), "paths") {
		t.Fatalf("escaping path = %v, want refusal", err)
	}
}

func osWriteFile(path, body string) error {
	return os.WriteFile(path, []byte(body), 0o600)
}
