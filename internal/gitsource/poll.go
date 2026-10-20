package gitsource

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/ssh"
)

// Reason vocabulary for `trigger poll --json` (contracts corpus).
const (
	ReasonDeployed   = "deployed"
	ReasonUpToDate   = "up-to-date"
	ReasonNotDue     = "not-due"
	ReasonPollOff    = "poll-off"
	ReasonNoChanges  = "no-relevant-changes"
	ReasonForcePush  = "force-push"
	ReasonRetryLimit = "retry-limit"
	ReasonBranchGone = "branch-missing"
	ReasonFailed     = "failed"
)

// PollResult is the `trigger poll --json` object (one per app; `--all`
// emits one JSON line per app in this same shape).
type PollResult struct {
	MachineInterface int    `json:"machine_interface"`
	App              string `json:"app"`
	Host             string `json:"host,omitempty"`
	Ref              string `json:"ref"`
	Head             string `json:"head,omitempty"`
	Admitted         bool   `json:"admitted"`
	Reason           string `json:"reason,omitempty"`
	Release          string `json:"deployed_release,omitempty"`
}

// PollEventKind selects admit or refuse in Deps.RecordEvent.
const (
	EventAdmit  = "trigger.admit"
	EventRefuse = "trigger.refuse"
)

// PollDeps carries everything Poll needs beyond the repository: the
// loaded app config (Git non-nil), the deploy callback (run under the
// app lock the CALLER holds — poll never locks) and the event recorder.
type PollDeps struct {
	Exec  ssh.Executor
	App   string
	AppCf *config.AppConfig
	// Now is --now: ignore the interval and poll: off (a webhook wake).
	Now bool
	// Hint is an advisory head (a webhook payload); the remote head
	// always wins — the hint only wakes the poll.
	Hint string
	// RecordEvent appends to the app's server history (nil = tests).
	RecordEvent func(kind, outcome string, detail map[string]any)
	// Deploy builds and deploys the commit under the caller's lock and
	// returns when the release is committed.
	Deploy func(ctx context.Context, commit, version string) error
	// Clock overrides time.Now (tests); nil uses the real one.
	Clock func() time.Time
}

func (d PollDeps) now() time.Time {
	if d.Clock != nil {
		return d.Clock()
	}
	return time.Now()
}

func (d PollDeps) event(kind, outcome string, detail map[string]any) {
	if d.RecordEvent != nil {
		d.RecordEvent(kind, outcome, detail)
	}
}

// Poll is the one admitter (D4): ls-remote the branch with the app
// credential, dedup by (app, ref, commit), enforce the per-ref watermark
// with an ancestry check on the mirror, refuse force-pushes until
// `trigger reset`, filter by changed paths, cap retries of a failed
// commit, and always deploy the newest admitted head. All decisions and
// their records happen under the app lock the caller holds.
func Poll(ctx context.Context, d PollDeps) (PollResult, error) {
	g := d.AppCf.Git
	if g == nil {
		return PollResult{}, fmt.Errorf("%s has no git: source", d.App)
	}
	ref := "refs/heads/" + g.Branch
	res := PollResult{App: d.App, Ref: ref}

	interval, polling := PollInterval(g)
	if !polling && !d.Now {
		res.Reason = ReasonPollOff
		return res, nil
	}

	st, err := LoadState(ctx, d.Exec, d.App)
	if err != nil {
		return res, err
	}
	if st.SchemaVersion == 0 {
		st.SchemaVersion = 1
	}
	if st.Repository != "" && st.Repository != g.Repository {
		// The repository changed: old watermarks prove nothing about the
		// new history — start fresh (the mirror refetches over it).
		st.Refs, st.Commits = nil, nil
	}
	st.Repository = g.Repository

	if !d.Now {
		if elapsed := d.now().Unix() - st.LastPoll; st.LastPoll > 0 && elapsed >= 0 && time.Duration(elapsed)*time.Second < interval {
			res.Reason = ReasonNotDue
			return res, nil
		}
	}
	st.LastPoll = d.now().Unix()

	r := st.Ref(ref)
	head, err := LsRemote(ctx, d.Exec, d.App, g.Repository, g.Branch)
	if err != nil {
		if errors.Is(err, ErrAbsent) {
			had := r.LastSeen != ""
			r.LastSeen = ""
			if had {
				d.event(EventRefuse, "refused", map[string]any{"ref": ref, "reason": ReasonBranchGone})
			}
			if err := SaveState(ctx, d.Exec, d.App, st); err != nil {
				return res, err
			}
			res.Reason = ReasonBranchGone
			return res, nil
		}
		return res, fmt.Errorf("checking %s's branch %s: %w", d.App, g.Branch, err)
	}
	res.Head = head

	if reason, done := deduped(st, r, ref, head); done {
		if err := SaveState(ctx, d.Exec, d.App, st); err != nil {
			return res, err
		}
		res.Reason = reason
		return res, nil
	}

	// The remote head is the truth (a hint only woke us); fetching may
	// even land on something newer — supersedes always win.
	fetched, err := FetchMirror(ctx, d.Exec, d.App, g.Repository, g.Branch, head)
	if err != nil {
		return res, fmt.Errorf("fetching %s's mirror: %w", d.App, err)
	}
	if fetched != head {
		head, res.Head = fetched, fetched
		if reason, done := deduped(st, r, ref, head); done {
			if err := SaveState(ctx, d.Exec, d.App, st); err != nil {
				return res, err
			}
			res.Reason = reason
			return res, nil
		}
	}
	r.LastSeen = head

	if r.Watermark != "" {
		ok, err := IsAncestor(ctx, d.Exec, d.App, g.Branch, r.Watermark, head)
		if err != nil {
			var refused *ErrRefused
			if errors.As(err, &refused) {
				return d.refuse(ctx, st, r, ref, head, ReasonForcePush, res, err)
			}
			return res, err
		}
		if !ok {
			return d.refuse(ctx, st, r, ref, head, ReasonForcePush, res, nil)
		}
	}

	if r.Watermark != "" {
		changed, err := DiffNames(ctx, d.Exec, d.App, g.Branch, r.Watermark, head)
		if err != nil {
			return res, fmt.Errorf("computing changed paths for %s: %w", d.App, err)
		}
		if !PathRelevant(changed, DefaultPaths(d.AppCf)) {
			c := st.Commit(ref, head)
			c.Status = RecordSkipped
			r.Watermark = head
			if err := SaveState(ctx, d.Exec, d.App, st); err != nil {
				return res, err
			}
			res.Reason = ReasonNoChanges
			return res, nil
		}
	}

	c := st.Commit(ref, head)
	why := "new-commit"
	if c.Attempts > 0 {
		why = "retry"
	}
	d.event(EventAdmit, "started", map[string]any{"ref": ref, "commit": head, "reason": why})

	version := short(head)
	deployErr := d.Deploy(ctx, head, version)
	c.Attempts++
	c.LastAttempt = d.now().Unix()
	if deployErr != nil {
		c.Status = RecordFailed
		c.LastError = truncate(deployErr.Error(), 300)
		if c.Attempts >= MaxDeployAttempts {
			// One refusal per commit: after this, only a new commit,
			// `trigger retry` or a redeploy restarts it.
			d.event(EventRefuse, "refused", map[string]any{"ref": ref, "commit": head, "reason": ReasonRetryLimit, "error": c.LastError})
		}
		if err := SaveState(ctx, d.Exec, d.App, st); err != nil {
			return res, err
		}
		res.Reason = ReasonFailed
		return res, deployErr
	}
	c.Status = RecordDeployed
	c.Release = version
	c.LastError = ""
	r.Watermark = head
	r.RefusedHead = ""
	if err := SaveState(ctx, d.Exec, d.App, st); err != nil {
		return res, err
	}
	res.Admitted = true
	res.Reason = ReasonDeployed
	res.Release = version
	return res, nil
}

// refuse records one force-push refusal per (ref, head) and refuses
// until `teploy trigger reset --app <app> --ref <ref>`.
func (d PollDeps) refuse(ctx context.Context, st *PollState, r *RefState, ref, head, reason string, res PollResult, cause error) (PollResult, error) {
	if r.RefusedHead != head {
		d.event(EventRefuse, "refused", map[string]any{"ref": ref, "commit": head, "reason": reason})
	}
	r.RefusedHead = head
	r.RefusedAt = d.now().Unix()
	st.Commit(ref, head).Status = RecordRefused
	if err := SaveState(ctx, d.Exec, d.App, st); err != nil {
		return res, err
	}
	res.Reason = reason
	if cause != nil {
		return res, fmt.Errorf("%s: %w", d.App, cause)
	}
	return res, nil
}

// deduped reports the cheap no-work outcomes decidable before/after the
// fetch: same head as the watermark or an already-final ledger record.
// Read-only: never creates ledger entries.
func deduped(st *PollState, r *RefState, ref, head string) (string, bool) {
	if head == "" {
		return ReasonBranchGone, true
	}
	if head == r.Watermark && r.Watermark != "" {
		return ReasonUpToDate, true
	}
	if r.RefusedHead == head {
		return ReasonForcePush, true
	}
	switch c := st.Commits[ledgerKey(ref, head)]; {
	case c == nil:
	case c.Status == RecordDeployed || c.Status == RecordSkipped:
		return ReasonUpToDate, true
	case c.Status == RecordFailed:
		if c.Attempts >= MaxDeployAttempts {
			return ReasonRetryLimit, true
		}
	case c.Status == RecordRefused:
		return ReasonForcePush, true
	}
	return "", false
}

// ResolveHead resolves the branch head for a manual deploy (deploy from
// config with git: set): ls-remote with the credential, then fetch the
// mirror so the build can archive the commit.
func ResolveHead(ctx context.Context, exec ssh.Executor, app string, g *config.GitSource) (string, error) {
	head, err := LsRemote(ctx, exec, app, g.Repository, g.Branch)
	if errors.Is(err, ErrAbsent) {
		return "", fmt.Errorf("branch %s does not exist on %s", g.Branch, g.Repository)
	}
	if err != nil {
		return "", fmt.Errorf("checking %s's branch %s: %w", app, g.Branch, err)
	}
	return FetchMirror(ctx, exec, app, g.Repository, g.Branch, head)
}

// RefusedHead returns the ref's refused head ("" when none) so a manual
// deploy can honor the same force-push refusal the poller records.
func RefusedHead(ctx context.Context, exec ssh.Executor, app, repository, branch string) (string, error) {
	st, err := LoadState(ctx, exec, app)
	if err != nil {
		return "", err
	}
	if st.Repository != "" && st.Repository != repository {
		return "", nil
	}
	return st.Ref("refs/heads/" + branch).RefusedHead, nil
}

// RecordManualDeploy advances the watermark after a successful manual
// deploy (deploy from config / redeploy): the release record already
// carries the commit; this keeps the next poll from redoing it.
func RecordManualDeploy(ctx context.Context, exec ssh.Executor, app, repository, branch, commit, release string) error {
	st, err := LoadState(ctx, exec, app)
	if err != nil {
		return err
	}
	if st.SchemaVersion == 0 {
		st.SchemaVersion = 1
	}
	if st.Repository != "" && st.Repository != repository {
		st.Refs, st.Commits = nil, nil
	}
	st.Repository = repository
	ref := "refs/heads/" + branch
	r := st.Ref(ref)
	r.Watermark = commit
	r.LastSeen = commit
	r.RefusedHead = ""
	c := st.Commit(ref, commit)
	c.Status = RecordDeployed
	c.Release = release
	c.LastError = ""
	return SaveState(ctx, exec, app, st)
}

// ResetRef clears a ref's watermark, refusal and ledger (`trigger
// reset`): the next poll admits the current head with no ancestry bar.
func ResetRef(ctx context.Context, exec ssh.Executor, app, repository, ref string) error {
	if !strings.HasPrefix(ref, "refs/heads/") || ref == "refs/heads/" || strings.ContainsAny(strings.TrimPrefix(ref, "refs/heads/"), "/ ") {
		return fmt.Errorf("ref must be refs/heads/<branch>")
	}
	st, err := LoadState(ctx, exec, app)
	if err != nil {
		return err
	}
	if st.SchemaVersion == 0 {
		st.SchemaVersion = 1
	}
	if st.Repository != "" && st.Repository != repository {
		st.Refs, st.Commits = nil, nil
	}
	st.Repository = repository
	delete(st.Refs, ref)
	prefix := ref + "/"
	for k := range st.Commits {
		if strings.HasPrefix(k, prefix) {
			delete(st.Commits, k)
		}
	}
	return SaveState(ctx, exec, app, st)
}

// RetryClear resets the failed commit's attempt budget so the next poll
// tries it again (`trigger retry`). Returns the head it cleared.
func RetryClear(ctx context.Context, exec ssh.Executor, app, repository, branch string) (string, error) {
	st, err := LoadState(ctx, exec, app)
	if err != nil {
		return "", err
	}
	if st.Repository != "" && st.Repository != repository {
		return "", fmt.Errorf("%s now builds from a different repository", app)
	}
	ref := "refs/heads/" + branch
	r := st.Ref(ref)
	head := r.LastSeen
	if head == "" {
		head = r.Watermark
	}
	if head == "" {
		return "", fmt.Errorf("nothing to retry — %s has no recorded head for %s", app, ref)
	}
	c := st.Commit(ref, head)
	if c.Status != RecordFailed {
		return head, nil
	}
	c.Attempts = 0
	if err := SaveState(ctx, exec, app, st); err != nil {
		return "", err
	}
	return head, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
