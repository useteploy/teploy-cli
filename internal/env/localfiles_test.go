package env

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadLocalEnvFilesPlainDotenv(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.env"), []byte("FOO=1\nBAR=two\n# comment\n"), 0o600)
	os.WriteFile(filepath.Join(dir, "b.env"), []byte("BAR=three\nBAZ=4\n"), 0o600)
	got, err := LoadLocalEnvFiles(dir, []string{"a.env", "b.env"})
	if err != nil {
		t.Fatal(err)
	}
	// Later files win.
	if got["FOO"] != "1" || got["BAR"] != "three" || got["BAZ"] != "4" {
		t.Errorf("merged = %v", got)
	}
}

func TestLoadLocalEnvFilesYAMLScalars(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "vars.yaml"), []byte("KEY: value\nNUM: 7\nFLAG: true\nnested:\n  x: 1\n"), 0o600)
	got, err := LoadLocalEnvFiles(dir, []string{"vars.yaml"})
	if err != nil {
		t.Fatal(err)
	}
	if got["KEY"] != "value" || got["NUM"] != "7" || got["FLAG"] != "true" {
		t.Errorf("yaml scalars = %v", got)
	}
	if _, ok := got["nested"]; ok {
		t.Error("nested structures must not become env vars")
	}
}

func TestLoadLocalEnvFilesMissingFile(t *testing.T) {
	if _, err := LoadLocalEnvFiles(t.TempDir(), []string{"nope.env"}); err == nil {
		t.Fatal("missing file must error, not silently deploy without secrets")
	}
}

func TestIsSopsName(t *testing.T) {
	for name, want := range map[string]bool{
		"secrets.sops.yaml": true,
		"secrets.enc.env":   true,
		"plain.env":         false,
		"key.age":           false,
	} {
		if got := isSopsName(name); got != want {
			t.Errorf("isSopsName(%q) = %v, want %v", name, got, want)
		}
	}
}

// End-to-end age round-trip — skipped when age isn't installed locally.
func TestLoadLocalEnvFilesAgeRoundTrip(t *testing.T) {
	if _, err := exec.LookPath("age"); err != nil {
		t.Skip("age not installed")
	}
	if _, err := exec.LookPath("age-keygen"); err != nil {
		t.Skip("age-keygen not installed")
	}
	dir := t.TempDir()
	identity := filepath.Join(dir, "key.txt")
	keygenOut, err := exec.Command("age-keygen", "-o", identity).CombinedOutput()
	if err != nil {
		t.Fatalf("age-keygen: %v: %s", err, keygenOut)
	}
	recipient := ""
	for _, line := range strings.Split(string(mustRead(t, identity)), "\n") {
		if strings.HasPrefix(line, "# public key: ") {
			recipient = strings.TrimPrefix(line, "# public key: ")
		}
	}
	if recipient == "" {
		t.Fatal("no recipient in age-keygen output")
	}

	plain := filepath.Join(dir, "secrets.env")
	os.WriteFile(plain, []byte("TOKEN=s3cret\n"), 0o600)
	encPath := filepath.Join(dir, "secrets.env.age")
	if out, err := exec.Command("age", "-r", recipient, "-o", encPath, plain).CombinedOutput(); err != nil {
		t.Fatalf("age encrypt: %v: %s", err, out)
	}
	os.Remove(plain)

	t.Setenv("TEPLOY_AGE_IDENTITY", identity)
	got, err := LoadLocalEnvFiles(dir, []string{"secrets.env.age"})
	if err != nil {
		t.Fatal(err)
	}
	if got["TOKEN"] != "s3cret" {
		t.Errorf("TOKEN = %q", got["TOKEN"])
	}
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
