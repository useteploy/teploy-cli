package cli

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/confighome"
	"github.com/useteploy/teploy/internal/ssh"
)

// localConfigHome runs the real server-side config store (git + python
// helper) against a temp root, the confighome package's own test pattern.
func localConfigHome(t *testing.T) ssh.Executor {
	t.Helper()
	for _, bin := range []string{"git", "python3"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skip(bin + " not available")
		}
	}
	t.Setenv("TEPLOY_CONFIG_ROOT_TEST", filepath.Join(t.TempDir(), ".teploy"))
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	return ssh.NewLocalExecutor()
}

// TestCheckEnvConfigGuard (token env-config-guard-v1): a Teploy-home
// config's declared env:/env_literal: keys are refused with the config
// remedy; undeclared keys pass; a folder-home app (no config.git) is
// untouched; a broken stored config is an error, not a silent pass.
func TestCheckEnvConfigGuard(t *testing.T) {
	ctx := context.Background()
	ex := localConfigHome(t)

	if _, err := confighome.Write(ctx, ex, "blog", []byte("app: blog\nimage: nginx:1\ndomain: blog.example.com\nenv:\n  LOG_LEVEL: debug\nenv_literal:\n  FEATURE_FLAG: 'on'\n"), "", confighome.Author{Name: "t", Email: "t@teploy"}, "create"); err != nil {
		t.Fatal(err)
	}

	err := checkEnvConfigGuard(ctx, ex, "blog", []string{"API_KEY"})
	if err != nil {
		t.Fatalf("undeclared key must pass: %v", err)
	}
	err = checkEnvConfigGuard(ctx, ex, "blog", []string{"LOG_LEVEL"})
	if err == nil || !strings.Contains(err.Error(), "teploy config set --app blog env.LOG_LEVEL") {
		t.Fatalf("declared key must be refused with the config remedy: %v", err)
	}
	err = checkEnvConfigGuard(ctx, ex, "blog", []string{"FEATURE_FLAG"})
	if err == nil || !strings.Contains(err.Error(), "env_literal") && !strings.Contains(err.Error(), "FEATURE_FLAG") {
		t.Fatalf("env_literal key must be refused: %v", err)
	}
	if err == nil || strings.Contains(err.Error(), "value") && strings.Contains(err.Error(), "'on'") {
		t.Fatalf("the refusal must not leak values: %v", err)
	}

	// Folder home: no config.git, guard is a no-op.
	if err := checkEnvConfigGuard(ctx, ex, "legacy", []string{"ANYTHING"}); err != nil {
		t.Fatalf("folder home must not be guarded: %v", err)
	}
}

// TestSecretSetStdinArgs: `secret set KEY --stdin` rides the shared
// parser, so the single-bare-key and NUL rules hold for secrets too.
func TestSecretSetStdinArgs(t *testing.T) {
	pairs, err := parseEnvSetArgs(strings.NewReader("hunter2\n"), []string{"DB_PASS"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if pairs["DB_PASS"] != "hunter2\n" {
		t.Fatalf("stdin must be verbatim, got %q", pairs["DB_PASS"])
	}
	if _, err := parseEnvSetArgs(strings.NewReader("x"), []string{"A", "B"}, true); err == nil {
		t.Fatal("--stdin takes exactly one KEY")
	}
	if _, err := parseEnvSetArgs(strings.NewReader("x"), []string{"K=V"}, true); err == nil {
		t.Fatal("--stdin takes a bare KEY")
	}
}

// TestServerSecretConfig: the folderless secret commands read the
// provider from the app's Teploy config without reconstructing one.
func TestServerSecretConfig(t *testing.T) {
	ctx := context.Background()
	ex := localConfigHome(t)

	if _, err := confighome.Write(ctx, ex, "vaultapp", []byte("app: vaultapp\nimage: nginx:1\ndomain: vault.example.com\nsecret:\n  provider: openbao\n  accessory: bao\n"), "", confighome.Author{Name: "t", Email: "t@teploy"}, "create"); err != nil {
		t.Fatal(err)
	}
	sc, ok, err := serverSecretConfig(ctx, ex, "vaultapp")
	if err != nil || !ok {
		t.Fatalf("serverSecretConfig: ok=%v err=%v", ok, err)
	}
	if sc.Provider != "openbao" || sc.Accessory != "bao" {
		t.Fatalf("provider fact = %+v", sc)
	}

	sc, ok, err = serverSecretConfig(ctx, ex, "plainapp")
	if err != nil || ok {
		t.Fatalf("folder home: ok=%v err=%v", ok, err)
	}
	if sc.Provider != "" {
		t.Fatalf("folder home must carry no provider, got %+v", sc)
	}
}
