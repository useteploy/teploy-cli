package build

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestCampaignGitDiscoveryFailsClosed(t *testing.T) {
	repo := t.TempDir()
	cmd := exec.Command("git", "init", "-q", repo)
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(t.TempDir(), "config")
	os.WriteFile(cfg, []byte("[malformed"), 0600)
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	if git, err := isGitWorkTree(repo); err == nil || git {
		t.Fatalf("git=%v error=%v", git, err)
	}
	if git, err := isGitWorkTree(t.TempDir()); err == nil || git {
		t.Fatalf("unknown status accepted: git=%v error=%v", git, err)
	}
}
func TestCampaignNonGitSourceAccepted(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	if git, err := isGitWorkTree(t.TempDir()); err != nil || git {
		t.Fatalf("git=%v error=%v", git, err)
	}
}
