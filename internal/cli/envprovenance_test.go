package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/releasemeta"
)

// resolveDeployEnv records where every env value came from, so the release
// record can keep plain config and drop everything secret-capable.
func TestResolveDeployEnv_RecordsEnvSources(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.env"), []byte("FILE_ONLY=from-file\nLOG_LEVEL=file-loses\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	t.Setenv("DEPLOYER_TOKEN", "from-deployer-env")

	cfg := &config.AppConfig{
		App: "myapp",
		Env: map[string]string{
			"LOG_LEVEL": "debug",
			"TOKEN":     "${DEPLOYER_TOKEN}",
			"DB_PASS":   "vault:db#password",
		},
		EnvLiteral: map[string]string{"PRICE": "$5"},
		EnvFiles:   []string{"app.env"},
	}
	if err := resolveDeployEnv(context.Background(), cfg, false); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"LOG_LEVEL": releasemeta.EnvSourceConfig,
		"TOKEN":     releasemeta.EnvSourceInterpolated,
		"DB_PASS":   releasemeta.EnvSourceVault,
		"FILE_ONLY": releasemeta.EnvSourceEnvFile,
		"PRICE":     releasemeta.EnvSourceConfigLiteral,
	}
	for k, src := range want {
		if cfg.EnvSources[k] != src {
			t.Errorf("%s source = %q, want %q", k, cfg.EnvSources[k], src)
		}
	}

	// Decrypted secrets win over plain env: age-store keys and OpenBao keys.
	prov := envProvenance(cfg, map[string]string{"AGE_ONLY": "x", "LOG_LEVEL": "y", "BAO": "z"}, map[string]bool{"AGE_ONLY": true, "LOG_LEVEL": true})
	if prov["AGE_ONLY"] != releasemeta.EnvSourceSecretStore || prov["LOG_LEVEL"] != releasemeta.EnvSourceSecretStore || prov["BAO"] != releasemeta.EnvSourceVault {
		t.Errorf("secret sources not applied: %v", prov)
	}
	if cfg.EnvSources["LOG_LEVEL"] != releasemeta.EnvSourceConfig {
		t.Error("envProvenance must not mutate the shared app config")
	}
}
