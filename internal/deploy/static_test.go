package deploy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/caddy"
	"github.com/useteploy/teploy/internal/ssh"
)

// staticTestSource creates a tempdir with deterministic content so the hash
// is stable across runs. Returns the path; caller cleans up via t.TempDir.
func staticTestSource(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"),
		[]byte("<h1>hello</h1>"), 0644); err != nil {
		t.Fatal(err)
	}
	assets := filepath.Join(dir, "assets")
	if err := os.MkdirAll(assets, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(assets, "app.js"),
		[]byte("console.log('hi')"), 0644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestStaticDeploy_FreshFirstDeploy(t *testing.T) {
	src := staticTestSource(t)
	manifest := json.RawMessage(`{"app":"myapp","deployment_type":"static"}`)
	manifestSHA := fmt.Sprintf("%x", sha256.Sum256(manifest))

	mock := ssh.NewMockExecutor("1.2.3.4",
		// EnsureAppDir
		ssh.MockCommand{Match: "mkdir -p /deployments/myapp", Output: ""},
		// AcquireLock — start clean (no lock).
		ssh.MockCommand{Match: "mkdir /deployments/myapp/.lock", Output: ""},
		// state.Read — no prior state.
		ssh.MockCommand{Match: "cat /deployments/myapp/state", Output: ""},
		// mkdir releases dir
		ssh.MockCommand{Match: "mkdir -p /deployments/myapp/releases", Output: ""},
		// release exists check — say "no", forcing rsync path
		ssh.MockCommand{Match: "test -d /deployments/myapp/releases/", Output: ""},
		// cleanup any leftover tmp
		ssh.MockCommand{Match: "rm -rf /deployments/myapp/releases/", Output: ""},
		// mkdir tmp release
		ssh.MockCommand{Match: "mkdir -p /deployments/myapp/releases/", Output: ""},
		// rename tmp to final
		ssh.MockCommand{Match: "mv /deployments/myapp/releases/", Output: ""},
		// symlink swap
		ssh.MockCommand{Match: "ln -sfn releases/", Output: ""},
		// caddy admin API: ensure server (success — already exists)
		ssh.MockCommand{Match: "curl -sf http://localhost:2019/config/apps/http/servers/srv0", Output: `{"listen":[":80",":443"]}`},
		// deleteRouteByID for stale teploy-myapp / teploy-lb-myapp (404 = no-op)
		ssh.MockCommand{Match: "curl -sf -X DELETE", Err: fmt.Errorf("not found")},
		// Caddyfile mirror operations — for lb- removal and main upsert
		ssh.MockCommand{Match: "cat /deployments/caddy/Caddyfile", Output: "{\n\tadmin 0.0.0.0:2019\n}\n"},
		ssh.MockCommand{Match: "mv /tmp/teploy_caddyfile.tmp", Output: ""},
		ssh.MockCommand{Match: "mkdir /deployments/caddy/.lock", Output: ""},
		ssh.MockCommand{Match: "rmdir /deployments/caddy/.lock", Output: ""},
		// caddy reload
		ssh.MockCommand{Match: "docker exec caddy caddy reload", Output: ""},
		// state.Write — ensure app dir is already present
		ssh.MockCommand{Match: "cat /tmp/teploy_state", Output: ""},
		// AppendLog
		ssh.MockCommand{Match: "cat /tmp/teploy_log_entry", Output: ""},
		// pruneReleases
		ssh.MockCommand{Match: "ls -1t /deployments/myapp/releases", Output: ""},
		// ReleaseLock
		ssh.MockCommand{Match: "rm -rf /deployments/myapp/.lock", Output: ""},
		// catch-all for any remaining state-package writes
		ssh.MockCommand{Match: "UPLOAD:", Output: ""},
		ssh.MockCommand{Match: "rm -f", Output: ""},
		ssh.MockCommand{Match: "mkdir -p", Output: ""},
		ssh.MockCommand{Match: "mv", Output: ""},
		ssh.MockCommand{Match: "cat", Output: ""},
	)

	d := NewStaticDeployer(mock, &bytes.Buffer{})
	// Note: rsync uses os/exec to a real `rsync` binary against the mock host;
	// we can't run that in a unit test. The mock executor's Run captures the
	// docker/curl/state interactions, but the rsync subprocess will fail.
	// For now, exercise everything except rsync by pre-staging release dir
	// existence: respond "yes" so rsync is skipped.
	mock = ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "mkdir -p /deployments/myapp", Output: ""},
		ssh.MockCommand{Match: "mkdir /deployments/myapp/.lock", Output: ""},
		ssh.MockCommand{Match: "cat /deployments/myapp/state", Output: ""},
		ssh.MockCommand{Match: "mkdir -p /deployments/myapp/releases", Output: ""},
		// Pre-existing release dir → skip rsync
		ssh.MockCommand{Match: "test -d /deployments/myapp/releases/", Output: "yes"},
		ssh.MockCommand{Match: "ln -sfn releases/", Output: ""},
		ssh.MockCommand{Match: "curl -sf http://localhost:2019/config/apps/http/servers/srv0", Output: `{"listen":[":80",":443"]}`},
		ssh.MockCommand{Match: "curl -sf -X DELETE", Err: fmt.Errorf("not found")},
		ssh.MockCommand{Match: "cat /deployments/caddy/Caddyfile", Output: "{\n\tadmin 0.0.0.0:2019\n}\n"},
		ssh.MockCommand{Match: "mv /tmp/teploy_caddyfile.tmp", Output: ""},
		ssh.MockCommand{Match: "mkdir /deployments/caddy/.lock", Output: ""},
		ssh.MockCommand{Match: "rmdir /deployments/caddy/.lock", Output: ""},
		ssh.MockCommand{Match: "docker exec caddy caddy reload", Output: ""},
		ssh.MockCommand{Match: "ls -1t /deployments/myapp/releases", Output: ""},
		ssh.MockCommand{Match: "rm -rf /deployments/myapp/.lock", Output: ""},
		ssh.MockCommand{Match: "UPLOAD:", Output: ""},
		ssh.MockCommand{Match: "rm -f", Output: ""},
		ssh.MockCommand{Match: "mkdir -p", Output: ""},
		ssh.MockCommand{Match: "mv", Output: ""},
		ssh.MockCommand{Match: "cat", Output: ""},
	)
	d = NewStaticDeployer(mock, &bytes.Buffer{})

	cfg := StaticConfig{
		App:             "myapp",
		Domain:          "myapp.com, www.myapp.com",
		Source:          src,
		SPA:             false,
		Headers:         map[string]string{"Strict-Transport-Security": "max-age=31536000"},
		ManifestSHA256:  manifestSHA,
		AppliedManifest: manifest,
		SourceRevision:  strings.Repeat("c", 40),
	}

	if err := d.Deploy(context.Background(), cfg); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	// Verify a Caddyfile mirror upload happened with a static block.
	mirror, ok := mock.Files["/tmp/teploy_caddyfile.tmp"]
	if !ok {
		t.Fatal("caddyfile mirror not uploaded")
	}
	got := string(mirror)
	for _, want := range []string{
		"# TEPLOY BEGIN myapp",
		"# TEPLOY END myapp",
		"myapp.com, www.myapp.com {",
		"root * /deployments/myapp/current",
		"file_server",
		"Strict-Transport-Security",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("mirror missing %q\nfull:\n%s", want, got)
		}
	}
	applied := writtenState(t, mock, "myapp")
	if applied.DeploymentType != "static" || applied.IngressMode != "caddy" || applied.Domain != cfg.Domain || applied.ManifestSHA256 != manifestSHA || !bytes.Equal(applied.AppliedManifest, manifest) || applied.SourceRevision != strings.Repeat("c", 40) {
		t.Errorf("unexpected static applied state: %+v", applied)
	}
}

func TestStaticDeploy_StateCommitFailureRestoresPreviousRelease(t *testing.T) {
	src := staticTestSource(t)
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "mkdir -p /deployments/myapp", Output: ""},
		ssh.MockCommand{Match: "mkdir /deployments/myapp/.lock", Output: ""},
		ssh.MockCommand{Match: "cat /deployments/myapp/state", Output: "current_hash=old123\nprevious_hash=older456\n"},
		ssh.MockCommand{Match: "mkdir -p /deployments/myapp/releases", Output: ""},
		ssh.MockCommand{Match: "test -d /deployments/myapp/releases/", Output: "yes"},
		ssh.MockCommand{Match: "ln -sfn releases/", Output: ""},
		ssh.MockCommand{Match: "cat /deployments/caddy/Caddyfile", Output: "{\n\tadmin 0.0.0.0:2019\n}\n"},
		ssh.MockCommand{Match: "mv /tmp/teploy_caddyfile.tmp", Output: ""},
		ssh.MockCommand{Match: "mkdir /deployments/caddy/.lock", Output: ""},
		ssh.MockCommand{Match: "docker exec caddy caddy reload", Output: ""},
		ssh.MockCommand{Match: "rmdir /deployments/caddy/.lock", Output: ""},
		ssh.MockCommand{Match: "UPLOAD:/deployments/myapp/state.json.tmp-", Err: fmt.Errorf("disk full")},
		ssh.MockCommand{Match: "rm -rf /deployments/myapp/.lock", Output: ""},
	)
	var out bytes.Buffer

	err := NewStaticDeployer(mock, &out).Deploy(context.Background(), StaticConfig{
		App: "myapp", Domain: "myapp.com", Source: src,
	})
	if err == nil || !strings.Contains(err.Error(), "release old123 was restored") {
		t.Fatalf("expected restored-release state commit error, got %v", err)
	}
	if strings.Contains(out.String(), "Deployed myapp") {
		t.Fatalf("failed deploy reported success: %s", out.String())
	}
	var lastSwap string
	for _, call := range mock.Calls {
		if strings.HasPrefix(call, "ln -sfn releases/") {
			lastSwap = call
		}
		if strings.HasPrefix(call, "ls -1t /deployments/myapp/releases") {
			t.Fatalf("releases were pruned after failed state commit: %s", call)
		}
	}
	if !strings.Contains(lastSwap, "releases/old123") {
		t.Fatalf("last symlink swap did not restore old123: %s", lastSwap)
	}
}

func TestStaticRollback_StateCommitFailureRestoresOriginalRelease(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "mkdir /deployments/myapp/.lock", Output: ""},
		ssh.MockCommand{Match: "cat /deployments/myapp/state", Output: "current_hash=v2\nprevious_hash=v1\n"},
		ssh.MockCommand{Match: "test -d /deployments/myapp/releases/v1", Output: "yes"},
		ssh.MockCommand{Match: "ln -sfn releases/", Output: ""},
		ssh.MockCommand{Match: "cat /deployments/caddy/Caddyfile", Output: "{\n\tadmin 0.0.0.0:2019\n}\n"},
		ssh.MockCommand{Match: "mv /tmp/teploy_caddyfile.tmp", Output: ""},
		ssh.MockCommand{Match: "mkdir /deployments/caddy/.lock", Output: ""},
		ssh.MockCommand{Match: "docker exec caddy caddy reload", Output: ""},
		ssh.MockCommand{Match: "rmdir /deployments/caddy/.lock", Output: ""},
		ssh.MockCommand{Match: "UPLOAD:/deployments/myapp/state.json.tmp-", Err: fmt.Errorf("disk full")},
		ssh.MockCommand{Match: "rm -rf /deployments/myapp/.lock", Output: ""},
	)
	var out bytes.Buffer

	err := NewStaticDeployer(mock, &out).Rollback(context.Background(), StaticRollbackConfig{
		App: "myapp", Domain: "myapp.com",
	})
	if err == nil || !strings.Contains(err.Error(), "release v2 was restored") {
		t.Fatalf("expected restored-release rollback error, got %v", err)
	}
	if strings.Contains(out.String(), "Rolled back") {
		t.Fatalf("failed rollback reported success: %s", out.String())
	}
	var lastSwap string
	for _, call := range mock.Calls {
		if strings.HasPrefix(call, "ln -sfn releases/") {
			lastSwap = call
		}
	}
	if !strings.Contains(lastSwap, "releases/v2") {
		t.Fatalf("last symlink swap did not restore v2: %s", lastSwap)
	}
}

func TestStaticBlock_Plain(t *testing.T) {
	got := caddy.StaticBlock(caddy.StaticBlockOpts{
		Hosts: []string{"example.com", "www.example.com"},
		Root:  "/srv/static/example/current",
	})
	for _, want := range []string{
		"example.com, www.example.com {",
		"encode gzip",
		"root * /srv/static/example/current",
		"file_server",
		"precompressed gzip",
		"X-Content-Type-Options",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "try_files") {
		t.Error("plain block should not have try_files (SPA was off)")
	}
}

func TestStaticBlock_SPA(t *testing.T) {
	got := caddy.StaticBlock(caddy.StaticBlockOpts{
		Hosts:       []string{"app.example.com"},
		Root:        "/srv/static/app/current",
		SPA:         true,
		SPAFallback: "/index.html",
	})
	if !strings.Contains(got, "try_files {path} {path}/ {path}/index.html /index.html") {
		t.Errorf("SPA fallback missing or malformed:\n%s", got)
	}
}

func TestStaticBlock_CacheAndCaddyExtra(t *testing.T) {
	got := caddy.StaticBlock(caddy.StaticBlockOpts{
		Hosts: []string{"example.com"},
		Root:  "/srv/static/example/current",
		Cache: map[string]string{
			"/_astro/*": "public, max-age=31536000, immutable",
		},
		CaddyExtra: "rate_limit 60r/m",
	})
	if !strings.Contains(got, "/_astro/*") {
		t.Errorf("cache matcher missing:\n%s", got)
	}
	if !strings.Contains(got, "public, max-age=31536000, immutable") {
		t.Errorf("cache value missing:\n%s", got)
	}
	if !strings.Contains(got, "rate_limit 60r/m") {
		t.Errorf("caddy_extra not appended:\n%s", got)
	}
}

func TestHashDir_Stable(t *testing.T) {
	src := staticTestSource(t)
	h1, err := hashDir(src)
	if err != nil {
		t.Fatal(err)
	}
	h2, err := hashDir(src)
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Errorf("hash not stable for same content: %s vs %s", h1, h2)
	}
	// Mutate one file → hash must change.
	if err := os.WriteFile(filepath.Join(src, "index.html"), []byte("<h1>changed</h1>"), 0644); err != nil {
		t.Fatal(err)
	}
	h3, err := hashDir(src)
	if err != nil {
		t.Fatal(err)
	}
	if h1 == h3 {
		t.Errorf("hash didn't change after content change")
	}
}
