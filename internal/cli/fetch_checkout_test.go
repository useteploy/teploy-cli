package cli

// C02 commit-pinned builds: the webhook's fetch must check out the commit
// the delivery AUTHENTICATED (payload after/checkout_sha), not the branch
// tip at fetch time. If the branch moved between push and fetch, the
// delivery's commit still builds; if the commit is gone (force-pushed away,
// deleted), the deploy fails loudly naming both commits — never a silent
// fallback to the tip.

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/ssh"
)

var errUnfetchable = errors.New("exit status 128")

const (
	testBuildDir  = "/deployments/myapp/build"
	testCommit    = "0123456789abcdef0123456789abcdef01234567"
	testMovedTip  = "fedcba9876543210fedcba9876543210fedcba98"
	gitQuotedDir  = "'/deployments/myapp/build'"
	gitQuotedMain = "'main'"
)

// fetchMocks answers every git command fetchCheckout issues; individual
// tests override cat-file/rev-parse with failures where needed. Matches are
// command prefixes, so they carry the "cd <dir> && " lead-in.
func fetchMocks(extra ...ssh.MockCommand) []ssh.MockCommand {
	const lead = "cd " + gitQuotedDir + " && "
	mocks := []ssh.MockCommand{
		{Match: lead + "git fetch origin", Output: ""},
		{Match: lead + "git cat-file -e", Output: ""},
		{Match: lead + "git rev-parse", Output: testMovedTip},
		{Match: lead + "git reset --hard", Output: ""},
	}
	return append(mocks, extra...)
}

func callSequence(mock *ssh.MockExecutor, needles ...string) []int {
	indexes := make([]int, len(needles))
	for i, needle := range needles {
		indexes[i] = -1
		for j, call := range mock.Calls {
			if indexes[i] == -1 && strings.Contains(call, needle) {
				indexes[i] = j
			}
		}
	}
	return indexes
}

// An authenticated commit pins the checkout: the exact commit is fetched,
// verified present as a commit object, and the worktree resets to IT —
// never to origin/<branch>.
func TestFetchCheckout_CommitPinned(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4", fetchMocks()...)
	var buf bytes.Buffer
	if err := fetchCheckout(context.Background(), mock, testBuildDir, "main", testCommit, &buf); err != nil {
		t.Fatalf("fetchCheckout(commit): %v", err)
	}

	// Exact command forms (quoting included).
	for _, want := range []string{
		"cd " + gitQuotedDir + " && git fetch origin " + gitQuotedMain,
		"cd " + gitQuotedDir + " && git fetch origin '" + testCommit + "'",
		"cd " + gitQuotedDir + " && git cat-file -e '" + testCommit + "^{commit}'",
		"cd " + gitQuotedDir + " && git reset --hard '" + testCommit + "'",
	} {
		found := false
		for _, call := range mock.Calls {
			if call == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing exact command %q, calls: %v", want, mock.Calls)
		}
	}
	// The reset must target the COMMIT, never the moving tip.
	for _, call := range mock.Calls {
		if strings.Contains(call, "git reset --hard") && strings.Contains(call, "origin/main") {
			t.Errorf("commit-pinned checkout reset to the branch tip: %q", call)
		}
	}
	// Ordering: fetch → (sha fetch) → verify → reset.
	seq := callSequence(mock, "git fetch origin "+gitQuotedMain, "git fetch origin '"+testCommit+"'",
		"git cat-file -e", "git reset --hard")
	for i := 1; i < len(seq); i++ {
		if seq[i] <= seq[i-1] {
			t.Errorf("command ordering wrong (indexes %v, calls %v)", seq, mock.Calls)
		}
	}
	if out := buf.String(); !strings.Contains(out, "Deploying "+testCommit+" from delivery") {
		t.Errorf("output must state the pinned deploy, got: %q", out)
	}
}

// Without a commit (scheduled path), the checkout resets to the branch tip
// and says so — and never runs the pinning verification commands.
func TestFetchCheckout_Tip(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4", fetchMocks()...)
	var buf bytes.Buffer
	if err := fetchCheckout(context.Background(), mock, testBuildDir, "main", "", &buf); err != nil {
		t.Fatalf("fetchCheckout(tip): %v", err)
	}

	for _, call := range mock.Calls {
		if strings.Contains(call, "git cat-file") || strings.Contains(call, testCommit) {
			t.Errorf("tip deploy must not run commit-pinning commands: %q", call)
		}
	}
	resetFound := false
	for _, call := range mock.Calls {
		if call == "cd "+gitQuotedDir+" && git reset --hard 'origin/main'" {
			resetFound = true
		}
	}
	if !resetFound {
		t.Errorf("tip reset command missing, calls: %v", mock.Calls)
	}
	if out := buf.String(); !strings.Contains(out, "Deploying tip of main") {
		t.Errorf("output must state the tip deploy, got: %q", out)
	}
}

// The unfetchable commit (force-pushed away / deleted): FAIL LOUDLY naming
// BOTH commits — the authenticated one and where the branch is now — and
// never reset the worktree.
func TestFetchCheckout_UnfetchableCommitFailsLoudly(t *testing.T) {
	const lead = "cd " + gitQuotedDir + " && "
	// The cat-file override must come FIRST: the mock matches in
	// registration order, and the default success entry would shadow it.
	mock := ssh.NewMockExecutor("1.2.3.4",
		append([]ssh.MockCommand{{Match: lead + "git cat-file -e", Err: errUnfetchable}}, fetchMocks()...)...)
	var buf bytes.Buffer
	err := fetchCheckout(context.Background(), mock, testBuildDir, "main", testCommit, &buf)
	if err == nil {
		t.Fatal("unfetchable commit must fail the deploy, not fall back to the tip")
	}
	for _, want := range []string{testCommit, testMovedTip, "main"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must name %q: %v", want, err)
		}
	}
	for _, call := range mock.Calls {
		if strings.Contains(call, "git reset --hard") {
			t.Errorf("worktree was reset despite the unfetchable commit: %q", call)
		}
	}
}
