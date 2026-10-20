package secret

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/ssh"
)

func TestLocateIdentityStates(t *testing.T) {
	const probe = "if [ -r '/etc/teploy/age-key' ]"
	cases := []struct {
		out  string
		want string
	}{
		{"new", identityNew},
		{"legacy", identityLegacy},
		{"moved", identityMoved},
		{"absent", identityAbsent},
	}
	for _, tc := range cases {
		mock := ssh.NewMockExecutor("h", ssh.MockCommand{Match: probe, Output: tc.out})
		got, err := locateIdentity(context.Background(), mock)
		if err != nil || got != tc.want {
			t.Errorf("probe %q: got %q (%v), want %q", tc.out, got, err, tc.want)
		}
	}
	mock := ssh.NewMockExecutor("h", ssh.MockCommand{Match: probe, Output: "garbage"})
	if _, err := locateIdentity(context.Background(), mock); err == nil {
		t.Error("unrecognized framing must be an error, never an absence signal")
	}
}

func TestResolveIdentity(t *testing.T) {
	const probe = "if [ -r '/etc/teploy/age-key' ]"
	for _, tc := range []struct{ out, want string }{
		{"new", IdentityPath},
		{"legacy", LegacyIdentityPath},
		{"moved", ""},
		{"absent", ""},
	} {
		mock := ssh.NewMockExecutor("h", ssh.MockCommand{Match: probe, Output: tc.out})
		got, err := ResolveIdentity(context.Background(), mock)
		if err != nil || got != tc.want {
			t.Errorf("probe %q: got %q (%v), want %q", tc.out, got, err, tc.want)
		}
	}
}

// TestHasLegacyIdentity pins the setup probe: only a file still holding
// a private key counts; the public-key marker a migration leaves behind
// must read as "nothing to do".
func TestHasLegacyIdentity(t *testing.T) {
	const probe = "if [ -r '/deployments/.age-key' ]"
	for _, tc := range []struct {
		out  string
		want bool
	}{
		{"yes", true},
		{"no", false},
	} {
		mock := ssh.NewMockExecutor("h", ssh.MockCommand{Match: probe, Output: tc.out})
		got, err := HasLegacyIdentity(context.Background(), mock)
		if err != nil || got != tc.want {
			t.Errorf("probe %q: got %v (%v), want %v", tc.out, got, err, tc.want)
		}
	}
}

// shellSandbox runs one of the generated scripts under a real /bin/sh
// with the fixed identity paths redirected into dir, so shell syntax and
// control flow are exercised exactly as a server would run them.
type shellSandbox struct {
	dir     string
	newDir  string
	legacy  string
	deploys string
}

func newShellSandbox(t *testing.T) *shellSandbox {
	t.Helper()
	for _, bin := range []string{"age", "age-keygen"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skip(bin + " not installed")
		}
	}
	s := &shellSandbox{dir: t.TempDir()}
	s.newDir = filepath.Join(s.dir, "etc", "teploy")
	s.deploys = filepath.Join(s.dir, "deployments")
	s.legacy = filepath.Join(s.deploys, ".age-key")
	if err := os.MkdirAll(s.deploys, 0o755); err != nil {
		t.Fatal(err)
	}
	return s
}

func (s *shellSandbox) rewrite(script string) string {
	r := strings.ReplaceAll(script, deploymentsDir, s.deploys)
	r = strings.ReplaceAll(r, IdentityDir, s.newDir)
	// The scripts target Linux servers (GNU mv -fT); the sandbox may run
	// on macOS, where plain -f has the same create/replace semantics for
	// a file onto a file.
	return strings.ReplaceAll(r, "mv -fT -- ", "mv -f ")
}

func (s *shellSandbox) run(t *testing.T, script string) string {
	t.Helper()
	out, err := exec.Command("/bin/sh", "-c", s.rewrite(script)).CombinedOutput()
	if err != nil {
		t.Fatalf("script failed: %v: %s", err, out)
	}
	return strings.TrimSpace(string(out))
}

func (s *shellSandbox) tryRun(script string) (string, error) {
	out, err := exec.Command("/bin/sh", "-c", s.rewrite(script)).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func (s *shellSandbox) keygen(t *testing.T, path string) {
	t.Helper()
	if out, err := exec.Command("age-keygen", "-o", path).CombinedOutput(); err != nil {
		t.Fatalf("age-keygen %s: %v: %s", path, err, out)
	}
}

func TestGenerateIdentityScriptPrefersNewLocation(t *testing.T) {
	s := newShellSandbox(t)
	got := s.run(t, generateIdentityScript)
	want := filepath.Join(s.newDir, "age-key")
	if got != want {
		t.Fatalf("generated at %q, want %q", got, want)
	}
	if fi, err := os.Stat(want); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("key mode: %v (%v)", fi, err)
	}
	if fi, err := os.Stat(s.newDir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode: %v (%v)", fi, err)
	}
	if loc := s.run(t, locateIdentityScript); loc != identityNew {
		t.Fatalf("locate after generate = %q", loc)
	}
}

func TestGenerateIdentityScriptFallsBackToLegacy(t *testing.T) {
	s := newShellSandbox(t)
	// /etc is not writable for the sandbox: point the new dir at a file's
	// path so mkdir fails and the legacy branch runs.
	script := strings.ReplaceAll(s.rewrite(generateIdentityScript), s.newDir, s.legacy+".blocked-file")
	if err := os.WriteFile(s.legacy+".blocked-file", []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("/bin/sh", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("generate fallback: %v: %s", err, out)
	}
	if strings.TrimSpace(string(out)) != s.legacy {
		t.Fatalf("generated at %q, want legacy %q", out, s.legacy)
	}
	if _, err := os.Stat(s.legacy); err != nil {
		t.Fatal(err)
	}
}

func TestMigrateIdentityScriptMovesAndMarks(t *testing.T) {
	s := newShellSandbox(t)
	s.keygen(t, s.legacy)
	oldKey, _ := os.ReadFile(s.legacy)

	if out := s.run(t, fmt.Sprintf(migrateScript, "", "")); out != "migrated" {
		t.Fatalf("migrate said %q", out)
	}
	newKey, err := os.ReadFile(filepath.Join(s.newDir, "age-key"))
	if err != nil {
		t.Fatal(err)
	}
	if string(newKey) != string(oldKey) {
		t.Fatal("the migrated key differs from the legacy key")
	}
	if fi, err := os.Stat(filepath.Join(s.newDir, "age-key")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("key mode: %v (%v)", fi, err)
	}
	// The migrate script leaves the legacy file untouched; writeMarker
	// replaces it. Simulate the full MigrateIdentity flow's second half.
	if out, err := exec.Command("/bin/sh", "-c", fmt.Sprintf(s.rewrite(writeMarkerScript), "")).CombinedOutput(); err != nil {
		t.Fatalf("marker: %v: %s", err, out)
	}
	marker, _ := os.ReadFile(s.legacy)
	if strings.Contains(string(marker), "AGE-SECRET-KEY-") {
		t.Fatal("the marker must not hold the private key")
	}
	if !strings.Contains(string(marker), strings.TrimSpace(recipientOf(t, string(newKey)))) {
		t.Fatalf("the marker must carry the public key for older CLIs:\n%s", marker)
	}
	if loc := s.run(t, locateIdentityScript); loc != identityNew {
		t.Fatalf("locate after migrate = %q", loc)
	}
	// With the new key unreadable (chmod 000 + non-root semantics cannot
	// be simulated as root), at least the framing stays valid.
	_ = os.Chmod(filepath.Join(s.newDir, "age-key"), 0o600)
}

func recipientOf(t *testing.T, key string) string {
	t.Helper()
	out, err := exec.Command("age-keygen", "-y").Output()
	if err == nil && strings.TrimSpace(string(out)) != "" {
		return strings.TrimSpace(string(out))
	}
	cmd := exec.Command("age-keygen", "-y", "-")
	cmd.Stdin = strings.NewReader(key)
	out, err = cmd.CombinedOutput()
	if err != nil {
		t.Skipf("age-keygen -y failed: %v: %s", err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestMigrateIdentityScriptRefusesNonIdentity(t *testing.T) {
	s := newShellSandbox(t)
	if err := os.WriteFile(s.legacy, []byte("not a key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := s.tryRun(fmt.Sprintf(migrateScript, "", ""))
	if err != nil {
		t.Fatalf("migrate: %v: %s", err, out)
	}
	if out != "not-an-identity" {
		t.Fatalf("migrate said %q, want not-an-identity", out)
	}
	if _, err := os.Stat(filepath.Join(s.newDir, "age-key")); !os.IsNotExist(err) {
		t.Fatal("nothing may be migrated from a non-identity file")
	}
}

func TestInstallBundledKeyScript(t *testing.T) {
	s := newShellSandbox(t)
	src := filepath.Join(s.dir, "bundled-key")
	s.keygen(t, src)
	script := fmt.Sprintf(s.rewrite(installBundledKeyScript), quote(src))
	out, err := exec.Command("/bin/sh", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("install: %v: %s", err, out)
	}
	if strings.TrimSpace(string(out)) != filepath.Join(s.newDir, "age-key") {
		t.Fatalf("installed at %q", out)
	}
	// A second install must fail (ln refuses to replace), never swap keys.
	other := filepath.Join(s.dir, "other-key")
	s.keygen(t, other)
	script2 := fmt.Sprintf(s.rewrite(installBundledKeyScript), quote(other))
	if out, err := exec.Command("/bin/sh", "-c", script2).CombinedOutput(); err == nil {
		t.Fatalf("a key already exists; install must fail, got %q", out)
	}
	cur, _ := os.ReadFile(filepath.Join(s.newDir, "age-key"))
	want, _ := os.ReadFile(src)
	if string(cur) != string(want) {
		t.Fatal("an existing key was replaced")
	}
}

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
