package autodeploy

import (
	"encoding/json"
	"path"
	"strings"
)

// Monorepo path filtering for the webhook listener. A push payload lists the
// files each commit touched; PathMatches decides whether any of them fall
// under the app's configured autodeploy.paths. The guiding rule is
// **never wrongly skip a deploy**: whenever we can't be sure of the full
// changed-file set (unknown provider, truncated payload, parse failure), we
// report "unknown" and the caller deploys anyway (fail-open).

// pushPayload is the common shape across GitHub, Gitea/Forgejo, and GitLab
// push events: a list of commits, each carrying added/modified/removed file
// lists. GitLab additionally sends total_commits_count, which lets us detect
// truncation (its commits array, like GitHub's, is capped).
type pushPayload struct {
	Commits []struct {
		Added    []string `json:"added"`
		Modified []string `json:"modified"`
		Removed  []string `json:"removed"`
	} `json:"commits"`
	TotalCommitsCount int    `json:"total_commits_count"`
	Ref               string `json:"ref"`
	Deleted           *bool  `json:"deleted"`
	After             string `json:"after"`
	CheckoutSHA       string `json:"checkout_sha"`
}

// PushEvent classifies an authenticated webhook body against the branch this
// listener watches (audit F40): a push to the watched branch returns ok=true
// (even when the changed-file set is unknown — fail-open on files, never on
// event identity); anything else — ping, tag push, a DIFFERENT branch, a
// branch deletion — returns ok=false so the caller acknowledges without
// deploying. An event for another branch used to trigger a deploy of the
// watched branch's current state with that event's (unrelated) changed-file
// list.
func PushEvent(body []byte, branch string) (ok bool) {
	var p pushPayload
	if err := json.Unmarshal(body, &p); err != nil {
		// Unparseable + no usable push markers: treat as non-push (ping or
		// unknown event) rather than deploying on it.
		return false
	}
	if p.Deleted != nil && *p.Deleted {
		return false
	}
	if p.Ref == "" {
		return false // ping / non-push event
	}
	if branch == "" {
		return true // caller did not pin a branch: any push event qualifies
	}
	return p.Ref == "refs/heads/"+branch || strings.TrimPrefix(p.Ref, "refs/heads/") == branch
}

// PushCommit returns the commit the authenticated push event names as the
// new head of the pushed branch (C02 commit pinning): GitLab's checkout_sha
// when present, else after (GitHub, Gitea/Forgejo). The value is what the
// deploy must be pinned to — NOT the branch tip at fetch time.
//
// Empty when the payload carries no usable commit: a non-push shape (the
// caller has already filtered with PushEvent), a branch deletion, the
// all-zero deletion marker, or a malformed hash. An empty result means
// "deploy the tip and say so", never "pin to garbage".
func PushCommit(body []byte) string {
	var p pushPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return ""
	}
	if p.Deleted != nil && *p.Deleted {
		return ""
	}
	// No ref = ping/non-push; a tag ref's "after" is the tag object, not a
	// branch head — neither pins a branch deploy.
	if p.Ref == "" || strings.HasPrefix(p.Ref, "refs/tags/") {
		return ""
	}
	for _, c := range []string{p.CheckoutSHA, p.After} {
		if isCommitHash(c) {
			return c
		}
	}
	return ""
}

// isCommitHash reports whether s is a well-formed git object id as providers
// send them in push payloads: 40 lowercase hex (SHA-1 repos) or 64 lowercase
// hex (SHA-256 repos), and not the all-zero deletion marker.
func isCommitHash(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	allZero := true
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			if r != '0' {
				allZero = false
			}
		case r >= 'a' && r <= 'f':
			allZero = false
		default:
			return false
		}
	}
	return !allZero
}

// githubCommitCap is the number of commits GitHub includes in a push event
// payload; more than this and the commits array is truncated, so the file
// list is incomplete and we must fail open.
const githubCommitCap = 20

// ChangedFiles extracts the set of files touched by a push. known is false
// when the payload carries no reliable file list (no commits, a truncated
// commit array, or unparseable body) — the caller must then deploy rather
// than risk skipping a real change.
func ChangedFiles(body []byte) (files []string, known bool) {
	var p pushPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, false
	}
	if len(p.Commits) == 0 {
		// A branch push always carries commits; its absence means a tag
		// push, a ping, or a shape we don't understand — deploy to be safe.
		return nil, false
	}
	// Truncation guards: GitLab tells us directly; GitHub caps silently.
	if p.TotalCommitsCount > len(p.Commits) {
		return nil, false
	}
	if len(p.Commits) >= githubCommitCap {
		return nil, false
	}

	set := map[string]struct{}{}
	for _, c := range p.Commits {
		for _, f := range c.Added {
			set[f] = struct{}{}
		}
		for _, f := range c.Modified {
			set[f] = struct{}{}
		}
		for _, f := range c.Removed {
			set[f] = struct{}{}
		}
	}
	files = make([]string, 0, len(set))
	for f := range set {
		files = append(files, f)
	}
	return files, true
}

// PathMatches reports whether any changed file matches any pattern.
func PathMatches(patterns, files []string) bool {
	for _, f := range files {
		for _, pat := range patterns {
			if matchOne(pat, f) {
				return true
			}
		}
	}
	return false
}

// matchOne matches one pattern against one file path. Supported forms:
//   - "**"          → everything
//   - "dir/**"      → everything under dir/ (recursive)
//   - path.Match    → "*"/"?"/char-classes within a single segment, exact
func matchOne(pattern, file string) bool {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return false
	}
	if pattern == "**" {
		return true
	}
	if prefix, ok := strings.CutSuffix(pattern, "/**"); ok {
		return file == prefix || strings.HasPrefix(file, prefix+"/")
	}
	ok, err := path.Match(pattern, file)
	return err == nil && ok
}
