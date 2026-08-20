package cli

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// chdirTemp moves into a fresh directory holding one teploy.yml and restores
// the working directory afterwards. runBuild reads its config from ".", the
// same as deploy.
func chdirTemp(t *testing.T, config string) string {
	t.Helper()
	original, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "teploy.yml"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(original); err != nil {
			t.Errorf("restoring working directory: %v", err)
		}
	})
	return dir
}

// captureStdout runs fn with os.Stdout replaced and returns what it wrote.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	original := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		out, _ := io.ReadAll(r)
		done <- string(out)
	}()
	fn()
	w.Close()
	os.Stdout = original
	return <-done
}

// The point of the command: it must refuse anything it cannot do, BEFORE it
// opens an SSH connection. Each of these would otherwise fail late, after a
// connection and possibly an rsync.
func TestBuildRefusesWhatItCannotBuild(t *testing.T) {
	t.Run("static app has no image", func(t *testing.T) {
		chdirTemp(t, "app: brochure\ntype: static\nsource: dist\ndomain: example.com\nserver: web1\n")
		err := runBuild(&Flags{}, "", "")
		if err == nil || !strings.Contains(err.Error(), "static app") {
			t.Fatalf("expected a static-app refusal, got %v", err)
		}
	})

	t.Run("no server to build on", func(t *testing.T) {
		chdirTemp(t, "app: api\ndomain: example.com\n")
		err := runBuild(&Flags{}, "", "")
		if err == nil || !strings.Contains(err.Error(), "no server specified") {
			t.Fatalf("expected a missing-server refusal, got %v", err)
		}
	})
}

// The tag is the whole output contract: `teploy build` exists so something
// else can run the image it names. A human line that a script cannot parse,
// or JSON with build chatter in front of it, breaks that.
func TestBuildReportsTheImageTag(t *testing.T) {
	t.Run("json carries the tag and nothing else", func(t *testing.T) {
		out := captureStdout(t, func() {
			if err := reportBuild(&Flags{JSON: true}, "api-build-abc1234", "abc1234", true); err != nil {
				t.Fatal(err)
			}
		})
		var got map[string]any
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("stdout was not parseable JSON (%v): %q", err, out)
		}
		if got["image"] != "api-build-abc1234" || got["version"] != "abc1234" || got["built"] != true {
			t.Fatalf("unexpected payload: %v", got)
		}
	})

	t.Run("a pinned image reports ready, not built", func(t *testing.T) {
		out := captureStdout(t, func() {
			if err := reportBuild(&Flags{}, "registry/api:v3", "v3", false); err != nil {
				t.Fatal(err)
			}
		})
		if !strings.Contains(out, "registry/api:v3") {
			t.Fatalf("the tag must be printed: %q", out)
		}
		if strings.Contains(out, "Built image") {
			t.Fatalf("nothing was built — saying so would be a lie: %q", out)
		}
	})

	t.Run("json mode reports a pull the same way", func(t *testing.T) {
		out := captureStdout(t, func() {
			if err := reportBuild(&Flags{JSON: true}, "registry/api:v3", "v3", false); err != nil {
				t.Fatal(err)
			}
		})
		var got map[string]any
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("stdout was not parseable JSON (%v): %q", err, out)
		}
		if got["built"] != false {
			t.Fatalf("a pulled image must not be reported as built: %v", got)
		}
	})
}

// `teploy build` and `teploy preview deploy` are two halves of one workflow:
// build prints a tag, preview runs it. Before --image existed they agreed only
// by both re-deriving `<app>-build-<git hash>` from the working directory,
// which breaks silently the moment they run from different checkouts.
func TestBuildAndPreviewComposeWithoutRederivingTheTag(t *testing.T) {
	root := NewRootCmd("test")

	build, _, err := root.Find([]string{"build"})
	if err != nil || build.Name() != "build" {
		t.Fatalf("teploy build is not registered: %v", err)
	}
	if build.Flags().Lookup("version") == nil {
		t.Error("build needs --version for callers with no git repo")
	}

	preview, _, err := root.Find([]string{"preview", "deploy"})
	if err != nil || preview.Name() != "deploy" {
		t.Fatalf("teploy preview deploy is not registered: %v", err)
	}
	if preview.Flags().Lookup("image") == nil {
		t.Fatal("preview deploy must accept --image, or the tag build printed cannot be passed to it")
	}
	if got := preview.Flags().Lookup("ttl").DefValue; got != "72h" {
		t.Errorf("preview ttl default changed to %q", got)
	}

	// The help is part of the contract here: it used to instruct people to run
	// `teploy deploy` first, which replaces production — the exact thing this
	// pair exists to avoid.
	if strings.Contains(preview.Long, "teploy deploy --image") {
		t.Error("preview help still tells the reader to deploy to production first")
	}
	if !strings.Contains(preview.Long, "teploy build") {
		t.Error("preview help should point at teploy build as the way to get an image")
	}
}
