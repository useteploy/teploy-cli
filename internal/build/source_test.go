package build

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// gitRepo initializes a hermetic git repository at dir (no global or
// system config, so a developer's core.excludesFile cannot change what the
// test sees) and commits the given tracked files.
func gitRepo(t *testing.T, dir string, tracked map[string]string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	writeTree(t, dir, tracked)
	run("add", "-A")
	run("-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-q", "-m", "init")
}

func resolve(t *testing.T, dir string) *Source {
	t.Helper()
	src, err := ResolveSource(dir)
	if err != nil {
		t.Fatalf("ResolveSource: %v", err)
	}
	return src
}

func assertEntries(t *testing.T, src *Source, want, notWant []string) {
	t.Helper()
	have := map[string]bool{}
	for _, e := range src.Entries {
		have[e] = true
	}
	for _, w := range want {
		if !have[w] {
			t.Errorf("%s must be uploaded; entries = %v", w, src.Entries)
		}
	}
	for _, n := range notWant {
		if have[n] {
			t.Errorf("%s must NOT be uploaded; entries = %v", n, src.Entries)
		}
	}
}

// TestResolveSource_GitignoredOverlayNeverUploaded is the L14 regression:
// the live leak was a gitignored teploy.home.yml (admin password in
// plaintext) riding the source sync into a world-readable build dir.
// Gitignored files stay home; teploy config, overlays, env files and
// secrets stores stay home even when git would show them.
func TestResolveSource_GitignoredOverlayNeverUploaded(t *testing.T) {
	dir := t.TempDir()
	gitRepo(t, dir, map[string]string{
		".gitignore":      "teploy.*.yml\n!teploy.yml\n/dist/\n*.log\n",
		"teploy.yml":      "app: dash\n",
		"Dockerfile":      "FROM alpine\nCOPY . .\n",
		"main.go":         "package main",
		"cmd/app/run.go":  "package app",
		"secrets.go":      "package main // source named like a secret is still source",
		".env.example":    "KEY=",
		"sub/teploy.yml":  "app: nested",
		"web/src/app.tsx": "export {}",
	})
	writeTree(t, dir, map[string]string{
		"teploy.home.yml":     "env:\n  TEPLOY_DASH_PASSWORD: hunter2\n", // gitignored overlay
		"teploy.staging.yml":  "env: {}\n",                               // gitignored overlay
		"teploy.prod.yaml":    "env: {}\n",                               // NOT gitignored: protected anyway
		".env":                "SECRET=1",
		".env.production":     "SECRET=2",
		"secrets.env":         "TOKEN=3",
		"app.secrets.env":     "TOKEN=4",
		"dist/bundle.js":      "built",
		"debug.log":           "noise",
		"notes.md":            "untracked, not ignored: uploaded",
		"node_modules/x/i.js": "dep",
	})

	src := resolve(t, dir)
	if !src.GitAware {
		t.Fatal("a git work tree must resolve git-aware")
	}
	assertEntries(t, src,
		[]string{"Dockerfile", "main.go", "cmd/app/run.go", "secrets.go", ".gitignore", "notes.md", "sub/teploy.yml", "web/src/app.tsx"},
		[]string{
			"teploy.home.yml", "teploy.staging.yml", "teploy.prod.yaml", "teploy.yml",
			".env", ".env.production", ".env.example", "secrets.env", "app.secrets.env",
			"dist/bundle.js", "debug.log", "node_modules/x/i.js",
		})
}

// TestResolveSource_AllowlistReincludesBuildArtifact pins the explicit
// door for a locally built artifact the Dockerfile COPYs (Ship's dist/ and
// web/dist/): `!` lines re-include gitignored paths, .teployignore
// excludes still beat them, and protected files can never be re-included.
func TestResolveSource_AllowlistReincludesBuildArtifact(t *testing.T) {
	dir := t.TempDir()
	gitRepo(t, dir, map[string]string{
		".gitignore":    "dist/\nweb/dist/\nteploy.*.yml\n.env\n",
		".teployignore": "!/dist/\n!web/dist/\n/web/dist/**/*.map\n!teploy.home.yml\n!.env\n/src\n",
		"Dockerfile":    "FROM node\nCOPY dist/ dist/\nCOPY web/dist/ web/dist/\n",
		"src/index.ts":  "export {}",
	})
	writeTree(t, dir, map[string]string{
		"dist/index.js":            "built",
		"dist/nested/chunk.js":     "built",
		"web/dist/app.js":          "built",
		"web/dist/app.js.map":      "map",
		"web/dist/assets/x.js.map": "map",
		"teploy.home.yml":          "password: hunter2",
		".env":                     "SECRET=1",
		"other/dist/leak.txt":      "matches the unanchored !web/dist? no — only dist/",
	})

	src := resolve(t, dir)
	assertEntries(t, src,
		[]string{"Dockerfile", "dist/index.js", "dist/nested/chunk.js", "web/dist/app.js"},
		[]string{
			"web/dist/app.js.map", "web/dist/assets/x.js.map", // exclude beats allowlist
			"teploy.home.yml", ".env", ".teployignore", // protected beats allowlist
			"src/index.ts", // .teployignore exclude of a tracked path
		})
	if err := src.CheckDockerfile("", ""); err != nil {
		t.Fatalf("allowlisted COPY sources must pass the preflight: %v", err)
	}
}

// TestResolveSource_OutsideGit: no work tree, no .gitignore to consult —
// everything but protected/excluded goes, empty directories included, and
// GitAware reports it so the caller can say so.
func TestResolveSource_OutsideGit(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"Dockerfile":      "FROM alpine",
		"app.py":          "print(1)",
		"teploy.yml":      "app: x",
		"teploy.home.yml": "password: hunter2",
		".env.local":      "SECRET=1",
		".git/config":     "not a real repo",
	})
	if err := os.MkdirAll(filepath.Join(dir, "empty/dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A bare .git directory without git's own layout is not a work tree.
	src := resolve(t, dir)
	if src.GitAware {
		t.Skip("temp dir resolved as a git work tree (unexpected environment)")
	}
	assertEntries(t, src,
		[]string{"Dockerfile", "app.py", "empty", "empty/dir"},
		[]string{"teploy.yml", "teploy.home.yml", ".env.local", ".git", ".git/config"})
}

// TestResolveSource_NestedRepository: a nested repository (or an
// initialized submodule) is listed by its OWN .gitignore, as rsync used to
// send it whole.
func TestResolveSource_NestedRepository(t *testing.T) {
	dir := t.TempDir()
	gitRepo(t, dir, map[string]string{".gitignore": "*.log\n", "main.go": "package main"})
	nested := filepath.Join(dir, "vendor/lib")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRepo(t, nested, map[string]string{".gitignore": "out/\n", "lib.go": "package lib"})
	writeTree(t, nested, map[string]string{"out/gen.bin": "built", "teploy.home.yml": "x"})

	src := resolve(t, dir)
	assertEntries(t, src,
		[]string{"main.go", "vendor/lib/lib.go", "vendor/lib/.gitignore"},
		[]string{"vendor/lib/out/gen.bin", "vendor/lib/teploy.home.yml", "vendor/lib/.git"})
}

func TestResolveSource_DeletedTrackedFileSkipped(t *testing.T) {
	dir := t.TempDir()
	gitRepo(t, dir, map[string]string{"a.go": "package a", "b.go": "package a"})
	if err := os.Remove(filepath.Join(dir, "b.go")); err != nil {
		t.Fatal(err)
	}
	assertEntries(t, resolve(t, dir), []string{"a.go"}, []string{"b.go"})
}

// The Dockerfile preflight names the cause and the fix before anything is
// uploaded, instead of a remote "COPY failed: not found".
func TestCheckDockerfile(t *testing.T) {
	dir := t.TempDir()
	gitRepo(t, dir, map[string]string{
		".gitignore": "dist/\n",
		"Dockerfile": strings.Join([]string{
			"FROM node AS build",
			"# COPY ignored-in-a-comment/ x/",
			"COPY --from=build /out /out",
			"COPY --chown=1000:1000 \\",
			"     package.json \\",
			"     dist/ /app/",
			"ADD https://example.com/x.tgz /tmp/",
			"COPY *.json /app/",
			`COPY ["package.json", "/app/"]`,
		}, "\n"),
		"package.json": "{}",
	})
	writeTree(t, dir, map[string]string{"dist/index.js": "built"})

	err := resolve(t, dir).CheckDockerfile("", "")
	if err == nil || !strings.Contains(err.Error(), "dist is gitignored") || !strings.Contains(err.Error(), "`!/dist/`") {
		t.Fatalf("a gitignored COPY source must fail with the allowlist fix, got %v", err)
	}

	// Protected COPY sources are refused with the protected message: an
	// allowlist line cannot help, and the image must not need them.
	dir2 := t.TempDir()
	gitRepo(t, dir2, map[string]string{"Dockerfile": "FROM alpine\nCOPY .env /app/.env\n", ".gitignore": ".env\n"})
	writeTree(t, dir2, map[string]string{".env": "SECRET=1"})
	if err := resolve(t, dir2).CheckDockerfile("", ""); err == nil || !strings.Contains(err.Error(), "protected") {
		t.Fatalf("a protected COPY source must be refused as protected, got %v", err)
	}

	// Context subdirectory: sources resolve under it.
	dir3 := t.TempDir()
	gitRepo(t, dir3, map[string]string{".gitignore": "app/build/\n", "app/Dockerfile": "FROM x\nCOPY build/ /b/\nCOPY missing/ /m/\n"})
	writeTree(t, dir3, map[string]string{"app/build/a": "built"})
	if err := resolve(t, dir3).CheckDockerfile("app", ""); err == nil || !strings.Contains(err.Error(), "app/build") {
		t.Fatalf("context-relative COPY source must be judged under the context, got %v", err)
	}
}

// Contains must not confuse a sibling sharing a prefix ("dist-old") with
// content under the directory ("dist/").
func TestSourceContains(t *testing.T) {
	s := &Source{Entries: []string{"dist-old/a", "dist.txt", "web/app.js"}}
	if s.Contains("dist") {
		t.Fatal("dist has no entries beneath it")
	}
	s.Entries = []string{"dist-old/a", "dist.txt", "dist/x", "web/app.js"}
	if !s.Contains("dist") || !s.Contains("dist/") || !s.Contains("web") || !s.Contains("dist.txt") {
		t.Fatal("Contains missed an uploaded entry")
	}
}

func TestRuleMatching(t *testing.T) {
	cases := []struct {
		pattern string
		path    string
		isDir   bool
		want    bool
	}{
		{"node_modules", "web/node_modules/x/y.js", false, true},
		{".env.*", "config/.env.production", false, true},
		{".env.*", "config/env.production", false, false},
		{"/teploy.yml", "teploy.yml", false, true},
		{"/teploy.yml", "sub/teploy.yml", false, false},
		{"teploy.*.yml", "teploy.home.yml", false, true},
		{"teploy.*.yml", "deep/teploy.home.yml", false, true},
		{"teploy.*.yml", "teploy.yml", false, false},
		{"/src", "src/index.ts", false, true},
		{"/src", "web/src/index.ts", false, false},
		{"/web/dist/**/*.map", "web/dist/a.js.map", false, true},
		{"/web/dist/**/*.map", "web/dist/assets/a.js.map", false, true},
		{"/web/dist/**/*.map", "web/dist/a.js", false, false},
		{"dist/", "dist", true, true},
		{"dist/", "dist", false, false},
		{"dist/", "dist/a.js", false, true},
		{"foo/bar", "x/foo/bar", false, true},
		{"foo/bar", "x/foo/barn", false, false},
		{"*.secrets.env", "ship.secrets.env", false, true},
		{"secrets.yml", "secrets.yml.go", false, false},
		{"file[0-9].txt", "file7.txt", false, true},
		{"file[!0-9].txt", "file7.txt", false, false},
		{"a?c", "abc", false, true},
		{"a?c", "a/c", false, false},
	}
	for _, c := range cases {
		r, err := compileRule(c.pattern)
		if err != nil {
			t.Fatalf("compile %q: %v", c.pattern, err)
		}
		if got := matchesAny([]rule{r}, c.path, c.isDir); got != c.want {
			t.Errorf("pattern %q vs %q (dir=%v) = %v, want %v", c.pattern, c.path, c.isDir, got, c.want)
		}
	}
}

// A gitignored file must not move the fingerprint (it is not build input
// any more); an allowlisted one must.
func TestFingerprint_TracksTheSelection(t *testing.T) {
	dir := t.TempDir()
	gitRepo(t, dir, map[string]string{".gitignore": "cache/\ndist/\n", ".teployignore": "!/dist/\n", "main.go": "package main"})
	writeTree(t, dir, map[string]string{"cache/a": "1", "dist/a": "1"})
	before, err := contextFingerprint(dir)
	if err != nil {
		t.Fatal(err)
	}
	writeTree(t, dir, map[string]string{"cache/a": "2", "teploy.home.yml": "changed"})
	if after, _ := contextFingerprint(dir); after != before {
		t.Fatal("a gitignored or protected change moved the fingerprint")
	}
	writeTree(t, dir, map[string]string{"dist/a": "2"})
	if after, _ := contextFingerprint(dir); after == before {
		t.Fatal("an allowlisted artifact change must move the fingerprint")
	}
}

// Sync hands rsync exactly the resolved list over --files-from (NUL
// separated) and never the directory wholesale. A fake rsync on PATH
// records the argv and the list it was fed.
func TestSync_SendsOnlyTheResolvedList(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fake")
	}
	bin := t.TempDir()
	record := filepath.Join(bin, "record")
	fake := "#!/bin/sh\nprintf '%s\\n' \"$@\" > '" + record + ".args'\ncat > '" + record + ".list'\n"
	if err := os.WriteFile(filepath.Join(bin, "rsync"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	src := &Source{Root: "/work/app", Entries: []string{"Dockerfile", "dist/a b.js", "main.go"}}
	err := Sync(context.Background(), SyncConfig{
		Source: src, RemoteDir: "/deployments/app/meta/att/v1.0123456789abcdef/build",
		Host: "192.0.2.1", User: "deploy", LinkDest: "/deployments/app/meta/att/v0.fedcba9876543210/build",
	}, os.Stdout, os.Stderr)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	args, _ := os.ReadFile(record + ".args")
	list, _ := os.ReadFile(record + ".list")
	argv := strings.Split(strings.TrimSpace(string(args)), "\n")
	joined := strings.Join(argv, " ")
	for _, want := range []string{"--from0", "--files-from=-", "--link-dest=/deployments/app/meta/att/v0.fedcba9876543210/build", "/work/app/"} {
		if !strings.Contains(joined, want) {
			t.Errorf("rsync argv missing %q: %v", want, argv)
		}
	}
	if strings.Contains(joined, "--delete") || strings.Contains(joined, "--exclude") {
		t.Errorf("the list is the whole selection — no --delete/--exclude: %v", argv)
	}
	if got, want := string(list), "Dockerfile\x00dist/a b.js\x00main.go\x00"; got != want {
		t.Errorf("files-from list = %q, want %q", got, want)
	}
}
