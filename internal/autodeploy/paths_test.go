package autodeploy

import "testing"

func TestMatchOne(t *testing.T) {
	cases := []struct {
		pattern, file string
		want          bool
	}{
		{"apps/web/**", "apps/web/src/index.ts", true},
		{"apps/web/**", "apps/web", true},
		{"apps/web/**", "apps/webhooks/x.go", false}, // prefix must be a path boundary
		{"apps/web/**", "apps/api/main.go", false},
		{"**", "anything/at/all", true},
		{"*.md", "README.md", true},
		{"*.md", "docs/README.md", false}, // * does not cross a segment
		{"apps/*/Dockerfile", "apps/web/Dockerfile", true},
		{"apps/*/Dockerfile", "apps/web/sub/Dockerfile", false},
		{"go.mod", "go.mod", true},
		{"go.mod", "api/go.mod", false},
		{"", "anything", false},
	}
	for _, c := range cases {
		if got := matchOne(c.pattern, c.file); got != c.want {
			t.Errorf("matchOne(%q, %q) = %v, want %v", c.pattern, c.file, got, c.want)
		}
	}
}

func TestPathMatches(t *testing.T) {
	patterns := []string{"apps/web/**", "packages/common/**"}
	if !PathMatches(patterns, []string{"README.md", "apps/web/src/a.ts"}) {
		t.Error("expected match on apps/web change")
	}
	if !PathMatches(patterns, []string{"packages/common/util.ts"}) {
		t.Error("expected match on shared package change")
	}
	if PathMatches(patterns, []string{"apps/api/main.go", "docs/x.md"}) {
		t.Error("expected no match for unrelated changes")
	}
	if PathMatches(patterns, nil) {
		t.Error("no files should not match")
	}
}

func TestChangedFilesGitHub(t *testing.T) {
	body := []byte(`{
		"commits": [
			{"added": ["apps/web/a.ts"], "modified": ["README.md"], "removed": []},
			{"added": [], "modified": ["apps/web/a.ts"], "removed": ["apps/web/old.ts"]}
		]
	}`)
	files, known := ChangedFiles(body)
	if !known {
		t.Fatal("expected known=true")
	}
	// Deduped union across commits.
	got := map[string]bool{}
	for _, f := range files {
		got[f] = true
	}
	for _, want := range []string{"apps/web/a.ts", "README.md", "apps/web/old.ts"} {
		if !got[want] {
			t.Errorf("missing %q in %v", want, files)
		}
	}
	if len(files) != 3 {
		t.Errorf("expected 3 unique files, got %d: %v", len(files), files)
	}
}

func TestChangedFilesGitLabTruncation(t *testing.T) {
	// GitLab reports more commits than it enumerated → we can't trust the
	// file list, so known must be false (fail open).
	body := []byte(`{
		"total_commits_count": 50,
		"commits": [{"added": ["a.txt"], "modified": [], "removed": []}]
	}`)
	if _, known := ChangedFiles(body); known {
		t.Fatal("expected known=false on truncated GitLab payload")
	}
}

func TestChangedFilesGitHubCap(t *testing.T) {
	// 20 commits = GitHub's cap → assume truncation, fail open.
	commit := `{"added":["x"],"modified":[],"removed":[]}`
	body := []byte(`{"commits":[`)
	for i := 0; i < 20; i++ {
		if i > 0 {
			body = append(body, ',')
		}
		body = append(body, commit...)
	}
	body = append(body, []byte(`]}`)...)
	if _, known := ChangedFiles(body); known {
		t.Fatal("expected known=false at the 20-commit cap")
	}
}

func TestChangedFilesEmptyOrJunk(t *testing.T) {
	if _, known := ChangedFiles([]byte(`{"commits": []}`)); known {
		t.Fatal("no commits → known=false (tag push / ping)")
	}
	if _, known := ChangedFiles([]byte(`not json`)); known {
		t.Fatal("unparseable → known=false")
	}
}

// TestPushCommit (C02): the commit a push payload authenticates as the
// branch's new head — GitLab's checkout_sha preferred, else after
// (GitHub/Gitea/Forgejo). Non-push shapes, deletions, all-zero deletion
// markers, and malformed hashes return "" so the caller deploys the tip and
// says so, never pins to garbage.
func TestPushCommit(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef01234567"
	const sha2 = "fedcba9876543210fedcba9876543210fedcba98"
	const zeros = "0000000000000000000000000000000000000000"
	cases := []struct {
		name string
		body string
		want string
	}{
		{"github after", `{"ref":"refs/heads/main","after":"` + sha + `"}`, sha},
		{"gitlab checkout_sha preferred", `{"ref":"refs/heads/main","checkout_sha":"` + sha + `","after":"` + sha2 + `"}`, sha},
		{"gitlab empty checkout_sha falls to after", `{"ref":"refs/heads/main","checkout_sha":"","after":"` + sha2 + `"}`, sha2},
		{"ping", `{}`, ""},
		{"tag push", `{"ref":"refs/tags/v1.0.0","after":"` + sha + `"}`, ""},
		{"branch deletion marker", `{"ref":"refs/heads/main","deleted":true,"after":"` + sha + `"}`, ""},
		{"all-zero after (deletion)", `{"ref":"refs/heads/main","after":"` + zeros + `"}`, ""},
		{"malformed hash ignored", `{"ref":"refs/heads/main","after":"deadbeef"}`, ""},
		{"non-hex ignored", `{"ref":"refs/heads/main","after":"zzz456789abcdef0123456789abcdef01234567"}`, ""},
		{"unparseable body", `not json`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := PushCommit([]byte(tc.body)); got != tc.want {
				t.Errorf("PushCommit(%s) = %q, want %q", tc.name, got, tc.want)
			}
		})
	}
	// sha256-object-id repos send 64-hex hashes — those must pin too.
	const sha256 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if got := PushCommit([]byte(`{"ref":"refs/heads/main","after":"` + sha256 + `"}`)); got != sha256 {
		t.Errorf("PushCommit(64-hex) = %q, want %q", got, sha256)
	}
}
