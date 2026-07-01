package autodeploy

import (
	"context"
	"strings"
	"testing"
)

// These tests previously covered shell-escaping in the generated bash
// webhook listener (generateListener/generateScript, removed by the
// autodeploy rebuild — see autodeploy.go). HMAC verification and secret
// handling moved into real Go code (webhook.go, tested in
// webhook_test.go), which structurally can't have the shell-escaping bugs
// those tests were guarding against: hmac.Equal never touches a shell at
// all. What remains here is the branch-name validation this rewrite added
// (the previous generateScript embedded BRANCH="%s" into bash unescaped —
// a latent injection vector nothing tested before).

func TestValidateBranch_RejectsShellMetacharacters(t *testing.T) {
	dangerous := []string{
		"",
		"feature$(whoami)",
		"feature`id`",
		"feature;rm -rf /",
		"feature|cat /etc/passwd",
		"feature\ninjected",
		"has spaces",
	}
	for _, branch := range dangerous {
		if err := ValidateBranch(branch); err == nil {
			t.Errorf("ValidateBranch(%q) = nil, want error", branch)
		}
	}
}

func TestValidateBranch_AcceptsRealBranchNames(t *testing.T) {
	valid := []string{"main", "master", "feature/my-branch", "release-1.2.3", "fix_bug_123"}
	for _, branch := range valid {
		if err := ValidateBranch(branch); err != nil {
			t.Errorf("ValidateBranch(%q) = %v, want nil", branch, err)
		}
	}
}

// TestSetup_RejectsInvalidBranch confirms Setup itself enforces
// ValidateBranch, not just the CLI layer, since Config can be constructed
// directly by any caller.
func TestSetup_RejectsInvalidBranch(t *testing.T) {
	err := (&Manager{}).Setup(context.Background(), Config{
		App:              "myapp",
		Branch:           "feature; rm -rf /",
		Secret:           "s3cret",
		TeployBinaryPath: "/deployments/.bin/teploy",
	})
	if err == nil {
		t.Fatal("expected error for a branch name with shell metacharacters")
	}
	if !strings.Contains(err.Error(), "branch") {
		t.Errorf("error should mention the branch, got: %v", err)
	}
}
