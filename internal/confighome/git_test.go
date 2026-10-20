package confighome

import (
	"strings"
	"testing"
)

func TestLoadGitSource(t *testing.T) {
	git := "git:\n  repository: git@github.com:org/repo\n  branch: main\n"
	if _, err := Load([]byte("app: demo\ndomain: demo.invalid\n"+git), "demo"); err != nil {
		t.Fatalf("git: without image rejected: %v", err)
	}
	if _, err := Load([]byte("app: demo\ndomain: demo.invalid\nimage: nginx:1\n"+git), "demo"); err == nil || !strings.Contains(err.Error(), "image") {
		t.Fatalf("image+git = %v, want refusal", err)
	}
	if _, err := Load([]byte("app: demo\ndomain: demo.invalid\ntype: static\nsource: ./site\n"+git), "demo"); err == nil || !strings.Contains(err.Error(), "git") {
		t.Fatalf("static+git = %v, want refusal", err)
	}
	if _, err := Load([]byte("app: demo\ndomain: demo.invalid\n"+git+"build_secrets: [NPM_TOKEN]\n"), "demo"); err != nil {
		t.Fatalf("git + build_secrets rejected: %v", err)
	}
}
