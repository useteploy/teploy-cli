package cli

import "testing"

// Repo identity is provenance in preview records and a legacy-adoption
// check, never part of the preview ID (see gitRepoIdentity). The
// normalization is deliberately trivial: collapse the common spellings of
// one remote; record anything else verbatim.
func TestNormalizeRepoURL(t *testing.T) {
	tests := []struct{ raw, want string }{
		{"https://github.com/useteploy/teploy-cli.git", "github.com/useteploy/teploy-cli"},
		{"https://github.com/useteploy/teploy-cli", "github.com/useteploy/teploy-cli"},
		{"git@github.com:useteploy/teploy-cli.git", "github.com/useteploy/teploy-cli"},
		{"ssh://git@github.com/useteploy/teploy-cli.git", "github.com/useteploy/teploy-cli"},
		{"https://user:token@github.com/o/r.git", "github.com/o/r"},
		{"git@gitlab.com:o/r.git", "gitlab.com/o/r"},
		{"git@100.108.123.49:tyler/teploy.git", "100.108.123.49/tyler/teploy"},
		{"", ""},
		{"   ", ""},
		{"/local/path/repo.git", "/local/path/repo"},
	}
	for _, tt := range tests {
		if got := normalizeRepoURL(tt.raw); got != tt.want {
			t.Errorf("normalizeRepoURL(%q) = %q, want %q", tt.raw, got, tt.want)
		}
	}
}

// gitRepoIdentity resolves from a real checkout and returns "" when no
// origin remote exists.
func TestGitRepoIdentity(t *testing.T) {
	dir := t.TempDir()
	if got := gitRepoIdentity(dir); got != "" {
		t.Errorf("empty dir must yield empty identity, got %q", got)
	}
	if got := gitRepoIdentity(t.TempDir() + "/nonexistent"); got != "" {
		t.Errorf("missing dir must yield empty identity, got %q", got)
	}
}
