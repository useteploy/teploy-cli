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
	TotalCommitsCount int `json:"total_commits_count"`
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
