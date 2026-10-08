package build

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestSiteConsumerIgnoredDistOverrideAndMaps(t *testing.T) {
	policy, err := os.ReadFile("testdata/site-teployignore")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	gitRepo(t, root, map[string]string{".gitignore": "/dist/\n", ".teployignore": string(policy), "Dockerfile": "FROM caddy:2\nCOPY dist/ /srv/\n"})
	writeTree(t, root, map[string]string{"dist/index.html": "<html>site</html>", "dist/assets/site.js": "console.log('site')", "dist/site.js.map": "map", "dist/assets/deep/site.js.map": "map", "dist/.env": "protected secret", "dist/ignored/nested/asset.css": "body{}"})
	src := resolve(t, root)
	assertEntries(t, src, []string{"dist/index.html", "dist/assets/site.js", "dist/ignored/nested/asset.css"}, []string{"dist/site.js.map", "dist/assets/deep/site.js.map", "dist/.env", ".teployignore"})
	if !src.Contains("dist/assets") || !src.GitAware {
		t.Fatal("parent directory traversal missing")
	}
	if _, err := exec.LookPath("rsync"); err != nil {
		t.Skip("rsync unavailable")
	}
	destination := t.TempDir()
	transfer := exec.Command("rsync", "-az", "--from0", "--files-from=-", root+"/", destination+"/")
	transfer.Stdin = bytes.NewReader(src.FileList())
	if out, err := transfer.CombinedOutput(); err != nil {
		t.Fatalf("consumer sync: %v %s", err, out)
	}
	for _, name := range []string{"dist/index.html", "dist/assets/site.js", "dist/ignored/nested/asset.css"} {
		if _, err := os.Stat(filepath.Join(destination, name)); err != nil {
			t.Fatalf("missing transferred parent/tree: %s %v", name, err)
		}
	}
	for _, name := range []string{"dist/site.js.map", "dist/assets/deep/site.js.map", "dist/.env"} {
		if _, err := os.Stat(filepath.Join(destination, name)); !os.IsNotExist(err) {
			t.Fatalf("excluded child transferred: %s", name)
		}
	}
}

func TestAllowlistedIgnoredParentsAndTraversalRefusal(t *testing.T) {
	root := t.TempDir()
	gitRepo(t, root, map[string]string{".gitignore": "/nested/\n", ".teployignore": "!/nested/parent/dist/\n/nested/parent/dist/**/*.map\n"})
	writeTree(t, root, map[string]string{"nested/parent/dist/assets/app.js": "script", "nested/parent/dist/assets/app.js.map": "map"})
	assertEntries(t, resolve(t, root), []string{"nested/parent/dist/assets/app.js"}, []string{"nested/parent/dist/assets/app.js.map"})
	for _, pattern := range []string{"!/../outside/\n", "!/nested/../../outside/\n"} {
		if err := os.WriteFile(filepath.Join(root, ".teployignore"), []byte(pattern), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := ResolveSource(root); err == nil {
			t.Fatal("parent traversal accepted")
		}
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	writeTree(t, outside, map[string]string{"dist/secret": "not source"})
	if err := os.WriteFile(filepath.Join(root, ".teployignore"), []byte("!/linked/dist/\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveSource(root); err == nil {
		t.Fatal("symlinked ancestor traversed")
	}
}
