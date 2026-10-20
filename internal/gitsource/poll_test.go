package gitsource

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/ssh"
)

// gitFixture is a local bare repository served over file:// — the same
// helper commands production uses, against a repository the test owns.
type gitFixture struct {
	t    *testing.T
	bare string
	work string
}

func newGitFixture(t *testing.T, branch string) *gitFixture {
	t.Helper()
	skipWithoutTools(t)
	f := &gitFixture{t: t, bare: filepath.Join(t.TempDir(), "src.git"), work: t.TempDir()}
	if out, err := exec.Command("git", "init", "-q", "--bare", "-b", branch, f.bare).CombinedOutput(); err != nil {
		// Older git without -b for bare repos: init then set HEAD.
		if err2 := exec.Command("git", "init", "-q", "--bare", f.bare).Run(); err2 != nil {
			t.Fatalf("git init --bare: %v\n%s", err, out)
		}
		exec.Command("git", "--git-dir", f.bare, "symbolic-ref", "HEAD", "refs/heads/"+branch).Run()
	}
	f.git(f.work, "init", "-q", "-b", branch)
	f.commit("Dockerfile", "FROM traefik/whoami")
	f.git(f.work, "push", "-q", "file://"+f.bare, branch)
	return f
}

func skipWithoutTools(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"git", "python3"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available", tool)
		}
	}
}

func (f *gitFixture) git(dir string, args ...string) string {
	f.t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.email=t@t", "-c", "user.name=t"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		f.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func (f *gitFixture) commit(file, content string) string {
	f.t.Helper()
	path := filepath.Join(f.work, file)
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
	f.git(f.work, "add", "-A")
	f.git(f.work, "commit", "-q", "-m", "add "+file)
	return strings.TrimSpace(f.git(f.work, "rev-parse", "HEAD"))
}

func (f *gitFixture) head() string {
	f.t.Helper()
	return strings.TrimSpace(f.git(f.work, "rev-parse", "HEAD"))
}

func (f *gitFixture) push(branch string) {
	f.t.Helper()
	f.git(f.work, "push", "-q", "file://"+f.bare, branch)
}

func (f *gitFixture) forcePush(branch string) {
	f.t.Helper()
	f.git(f.work, "push", "-q", "-f", "file://"+f.bare, branch)
}

// pollEnv wires the helper's test roots and returns a LocalExecutor.
func pollEnv(t *testing.T) ssh.Executor {
	t.Helper()
	skipWithoutTools(t)
	root := filepath.Join(t.TempDir(), ".teploy")
	t.Setenv("TEPLOY_CONFIG_ROOT_TEST", root)
	t.Setenv("TEPLOY_GIT_CRED_ROOT_TEST", filepath.Join(t.TempDir(), "creds"))
	t.Setenv("TEPLOY_DEPLOYMENTS_TEST", filepath.Join(t.TempDir(), "deployments"))
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	return ssh.NewLocalExecutor()
}

type pollHarness struct {
	exec    ssh.Executor
	app     string
	cfg     *config.AppConfig
	deploys []string
	fail    error
	events  []string
}

func newPollHarness(t *testing.T, f *gitFixture, paths []string, poll string) *pollHarness {
	exec := pollEnv(t)
	return &pollHarness{
		exec: exec,
		app:  "w4app",
		cfg: &config.AppConfig{App: "w4app", Git: &config.GitSource{
			Repository: "file://" + f.bare, Branch: "main", Paths: paths, Poll: poll,
		}},
	}
}

func (h *pollHarness) poll(ctx context.Context, now bool) (PollResult, error) {
	return Poll(ctx, PollDeps{
		Exec: h.exec, App: h.app, AppCf: h.cfg, Now: now,
		RecordEvent: func(kind, outcome string, detail map[string]any) {
			h.events = append(h.events, fmt.Sprintf("%s/%s:%v", kind, outcome, detail["reason"]))
		},
		Deploy: func(ctx context.Context, commit, version string) error {
			h.deploys = append(h.deploys, commit)
			return h.fail
		},
	})
}

func TestPollAdmitsDedupsAndSupersedes(t *testing.T) {
	f := newGitFixture(t, "main")
	h := newPollHarness(t, f, nil, "")
	ctx := context.Background()

	c1 := f.head()
	res, err := h.poll(ctx, true)
	if err != nil || !res.Admitted || res.Reason != ReasonDeployed || res.Head != c1 {
		t.Fatalf("first poll = %+v err=%v, want admitted %s", res, err, c1)
	}
	if len(h.deploys) != 1 || h.deploys[0] != c1 {
		t.Fatalf("deploys = %v", h.deploys)
	}

	res, err = h.poll(ctx, true)
	if err != nil || res.Admitted || res.Reason != ReasonUpToDate {
		t.Fatalf("second poll = %+v err=%v, want up-to-date", res, err)
	}
	if len(h.deploys) != 1 {
		t.Fatalf("duplicate deploy: %v", h.deploys)
	}

	c2 := f.commit("app.txt", "v2")
	f.push("main")
	// A stale hint (the previous head) must not deploy the wrong commit.
	res, err = h.pollWithHint(ctx, c1)
	if err != nil || !res.Admitted || res.Head != c2 {
		t.Fatalf("poll after push = %+v err=%v, want admitted %s", res, err, c2)
	}

	// The watermark is per (repository, ref): state carries it.
	st, err := LoadState(ctx, h.exec, h.app)
	if err != nil || st.Ref("refs/heads/main").Watermark != c2 {
		t.Fatalf("watermark = %+v err=%v", st.Ref("refs/heads/main"), err)
	}
}

func (h *pollHarness) pollWithHint(ctx context.Context, hint string) (PollResult, error) {
	deploysBefore := len(h.deploys)
	res, err := Poll(ctx, PollDeps{
		Exec: h.exec, App: h.app, AppCf: h.cfg, Now: true, Hint: hint,
		RecordEvent: func(kind, outcome string, detail map[string]any) {
			h.events = append(h.events, fmt.Sprintf("%s/%s:%v", kind, outcome, detail["reason"]))
		},
		Deploy: func(ctx context.Context, commit, version string) error {
			h.deploys = append(h.deploys, commit)
			return h.fail
		},
	})
	if len(h.deploys) != deploysBefore+1 {
		h.deploys = append(h.deploys, "(none)")
	}
	return res, err
}

func TestPollIntervalAndOff(t *testing.T) {
	f := newGitFixture(t, "main")
	h := newPollHarness(t, f, nil, "10m")
	ctx := context.Background()

	// No state yet: the first poll bootstraps immediately.
	if res, err := h.poll(ctx, false); err != nil || !res.Admitted {
		t.Fatalf("first poll without state = %+v err=%v", res, err)
	}
	if res, err := h.poll(ctx, false); err != nil || res.Reason != ReasonNotDue {
		t.Fatalf("second poll = %+v err=%v, want not-due", res, err)
	}

	off := newPollHarness(t, f, nil, "off")
	if res, err := off.poll(ctx, false); err != nil || res.Reason != ReasonPollOff {
		t.Fatalf("poll: off without --now = %+v err=%v", res, err)
	}
	if res, err := off.poll(ctx, true); err != nil || !res.Admitted {
		t.Fatalf("poll: off with --now (webhook wake) = %+v err=%v", res, err)
	}
}

func TestPollForcePushRefusalAndReset(t *testing.T) {
	f := newGitFixture(t, "main")
	h := newPollHarness(t, f, nil, "")
	ctx := context.Background()

	if _, err := h.poll(ctx, true); err != nil {
		t.Fatal(err)
	}
	f.commit("app.txt", "v2")
	f.push("main")
	if _, err := h.poll(ctx, true); err != nil {
		t.Fatal(err)
	}

	// Rewrite history: reset to c1 and commit a different change.
	f.git(f.work, "reset", "-q", "--hard", "HEAD~1")
	divergent := f.commit("other.txt", "divergent")
	f.forcePush("main")

	res, err := h.poll(ctx, true)
	if err != nil {
		t.Fatalf("force-push poll err=%v", err)
	}
	if res.Admitted || res.Reason != ReasonForcePush || res.Head != divergent {
		t.Fatalf("force-push poll = %+v, want refused %s", res, divergent)
	}
	refuses := countEvents(h, EventRefuse)
	if refuses == 0 {
		t.Fatalf("no refusal event: %v", h.events)
	}
	// Still refused on later polls, without a new event or deploy.
	res, err = h.poll(ctx, true)
	if err != nil || res.Reason != ReasonForcePush || countEvents(h, EventRefuse) != refuses {
		t.Fatalf("repeat force-push poll = %+v err=%v events=%v", res, err, h.events)
	}
	if len(h.deploys) != 2 {
		t.Fatalf("force-push must not deploy: %v", h.deploys)
	}

	// Reset re-admits the current head.
	if err := ResetRef(ctx, h.exec, h.app, "file://"+f.bare, "refs/heads/main"); err != nil {
		t.Fatal(err)
	}
	res, err = h.poll(ctx, true)
	if err != nil || !res.Admitted || res.Head != divergent {
		t.Fatalf("poll after reset = %+v err=%v", res, err)
	}

	// A manual deploy honors the same refusal.
	f.git(f.work, "reset", "-q", "--hard", "HEAD~1")
	again := f.commit("third.txt", "divergent-2")
	f.forcePush("main")
	if _, err := h.poll(ctx, true); err != nil {
		t.Fatal(err)
	}
	if refused, err := RefusedHead(ctx, h.exec, h.app, "file://"+f.bare, "main"); err != nil || refused != again {
		t.Fatalf("refused head = %q err=%v, want %q", refused, err, again)
	}
}

func TestPollPathFilter(t *testing.T) {
	f := newGitFixture(t, "main")
	h := newPollHarness(t, f, []string{"web/**"}, "")
	ctx := context.Background()

	if _, err := h.poll(ctx, true); err != nil {
		t.Fatal(err)
	}
	f.commit("docs/readme.md", "docs only")
	f.push("main")
	res, err := h.poll(ctx, true)
	if err != nil || res.Admitted || res.Reason != ReasonNoChanges {
		t.Fatalf("docs-only poll = %+v err=%v, want no-relevant-changes", res, err)
	}
	if len(h.deploys) != 1 {
		t.Fatalf("docs-only change deployed: %v", h.deploys)
	}
	// The skipped head advanced the watermark: still quiet next poll.
	res, err = h.poll(ctx, true)
	if err != nil || res.Reason != ReasonUpToDate {
		t.Fatalf("poll after skip = %+v err=%v", res, err)
	}
	web := f.commit("web/index.html", "<html>")
	f.push("main")
	res, err = h.poll(ctx, true)
	if err != nil || !res.Admitted || res.Head != web {
		t.Fatalf("web poll = %+v err=%v", res, err)
	}
}

func TestPollRetryBudget(t *testing.T) {
	f := newGitFixture(t, "main")
	h := newPollHarness(t, f, nil, "")
	h.fail = errors.New("build exploded")
	ctx := context.Background()

	for i := 1; i <= MaxDeployAttempts; i++ {
		res, err := h.poll(ctx, true)
		if err == nil || res.Admitted {
			t.Fatalf("failing poll %d = %+v err=%v", i, res, err)
		}
		if len(h.deploys) != i {
			t.Fatalf("attempt %d: deploys=%v", i, h.deploys)
		}
	}
	if countEvents(h, EventRefuse+"/refused:"+ReasonRetryLimit) != 1 {
		t.Fatalf("retry-limit events = %v", h.events)
	}
	// Past the budget: no more attempts.
	if res, err := h.poll(ctx, true); err != nil || res.Reason != ReasonRetryLimit || len(h.deploys) != MaxDeployAttempts {
		t.Fatalf("post-budget poll = %+v err=%v deploys=%v", res, err, h.deploys)
	}
	// A new commit restarts the budget.
	h.fail = nil
	next := f.commit("fix.txt", "fixed")
	f.push("main")
	if res, err := h.poll(ctx, true); err != nil || !res.Admitted || res.Head != next {
		t.Fatalf("poll after fix = %+v err=%v", res, err)
	}

	// ...and `trigger retry` re-arms a failed head.
	h.fail = errors.New("again")
	failure := f.commit("flaky.txt", "flaky")
	f.push("main")
	for i := 0; i < MaxDeployAttempts; i++ {
		h.poll(ctx, true)
	}
	if res, _ := h.poll(ctx, true); res.Reason != ReasonRetryLimit {
		t.Fatalf("expected retry-limit, got %+v", res)
	}
	if _, err := RetryClear(ctx, h.exec, h.app, "file://"+f.bare, "main"); err != nil {
		t.Fatal(err)
	}
	h.fail = nil
	if res, err := h.poll(ctx, true); err != nil || !res.Admitted || res.Head != failure {
		t.Fatalf("poll after retry = %+v err=%v", res, err)
	}
}

func TestPollRepositoryChangeResetsWatermarks(t *testing.T) {
	f := newGitFixture(t, "main")
	h := newPollHarness(t, f, nil, "")
	ctx := context.Background()
	if _, err := h.poll(ctx, true); err != nil {
		t.Fatal(err)
	}

	// A different repository with unrelated history: no ancestry, yet it
	// must deploy (old watermarks do not apply).
	other := newGitFixture(t, "main")
	h.cfg.Git.Repository = "file://" + other.bare
	res, err := h.poll(ctx, true)
	if err != nil || !res.Admitted {
		t.Fatalf("poll after repository change = %+v err=%v", res, err)
	}
}

func TestPollBranchMissing(t *testing.T) {
	f := newGitFixture(t, "main")
	h := newPollHarness(t, f, nil, "")
	ctx := context.Background()
	if _, err := h.poll(ctx, true); err != nil {
		t.Fatal(err)
	}
	h.cfg.Git.Branch = "gone"
	res, err := h.poll(ctx, true)
	if err != nil || res.Reason != ReasonBranchGone {
		t.Fatalf("poll for missing branch = %+v err=%v", res, err)
	}
}

func TestManualDeployRecordsWatermark(t *testing.T) {
	f := newGitFixture(t, "main")
	h := newPollHarness(t, f, nil, "")
	ctx := context.Background()
	head, err := ResolveHead(ctx, h.exec, h.app, h.cfg.Git)
	if err != nil {
		t.Fatal(err)
	}
	if head != f.head() {
		t.Fatalf("ResolveHead = %s, want %s", head, f.head())
	}
	if err := RecordManualDeploy(ctx, h.exec, h.app, h.cfg.Git.Repository, "main", head, "abc123"); err != nil {
		t.Fatal(err)
	}
	// The next poll treats the manually deployed head as done.
	if res, err := h.poll(ctx, true); err != nil || res.Admitted || res.Reason != ReasonUpToDate {
		t.Fatalf("poll after manual deploy = %+v err=%v", res, err)
	}
}

func TestPathRelevant(t *testing.T) {
	cases := []struct {
		changed, filter []string
		want            bool
	}{
		{[]string{"a.txt"}, nil, true},
		{[]string{"web/a.txt"}, []string{"web"}, true},
		{[]string{"web/a.txt"}, []string{"web/"}, true},
		{[]string{"docs/a.txt"}, []string{"web"}, false},
		{[]string{"web/a.txt"}, []string{"web/**"}, true},
		{[]string{"webx/a.txt"}, []string{"web"}, false},
		{[]string{"src/main.go"}, []string{"*.go"}, true},
		{[]string{"src/main.go"}, []string{"*.rs"}, false},
		{[]string{}, []string{"web"}, false},
	}
	for _, c := range cases {
		if got := PathRelevant(c.changed, c.filter); got != c.want {
			t.Errorf("PathRelevant(%v, %v) = %v, want %v", c.changed, c.filter, got, c.want)
		}
	}
}

func countEvents(h *pollHarness, prefix string) int {
	n := 0
	for _, e := range h.events {
		if strings.HasPrefix(e, prefix) {
			n++
		}
	}
	return n
}
