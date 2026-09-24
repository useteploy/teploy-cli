package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/ssh"
)

func secretStoreMock(names string) *ssh.MockExecutor {
	return ssh.NewMockExecutor("h",
		ssh.MockCommand{Match: "if [ ! -e '/deployments/demo/secrets'", Output: "present\n"},
		ssh.MockCommand{Match: "find '/deployments/demo/secrets'", Output: names},
	)
}

// Ship wave-9: `env set` on a secret-backed key reported success but the
// decrypted secret overrides .env at deploy, so the value never reached the
// container. It must refuse with the remedy and write nothing.
func TestEnvSet_RefusesSecretBackedKey(t *testing.T) {
	mock := secretStoreMock("DB_PASSWORD.age\n")
	err := checkEnvSetShadowing(context.Background(), mock, &config.AppConfig{App: "demo"},
		map[string]string{"DB_PASSWORD": "x", "PLAIN": "y"}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "DB_PASSWORD") || !strings.Contains(err.Error(), "teploy secret set DB_PASSWORD") {
		t.Fatalf("want a refusal naming the key and the remedy, got %v", err)
	}
	if strings.Contains(err.Error(), "PLAIN") {
		t.Errorf("plain key refused too: %v", err)
	}
}

func TestEnvSet_RefusesYAMLSecretRef(t *testing.T) {
	mock := secretStoreMock("")
	cfg := &config.AppConfig{App: "demo", Env: map[string]string{"API_KEY": "secret:api#key"}}
	err := checkEnvSetShadowing(context.Background(), mock, cfg, map[string]string{"API_KEY": "x"}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "API_KEY") || !strings.Contains(err.Error(), "OpenBao") {
		t.Fatalf("want a refusal for a secret: reference, got %v", err)
	}
}

func TestEnvSet_WarnsWhenTeployYAMLShadows(t *testing.T) {
	mock := secretStoreMock("")
	cfg := &config.AppConfig{App: "demo", Env: map[string]string{"LOG_LEVEL": "info"}}
	var warn bytes.Buffer
	if err := checkEnvSetShadowing(context.Background(), mock, cfg, map[string]string{"LOG_LEVEL": "debug", "OTHER": "1"}, &warn); err != nil {
		t.Fatalf("plain yml key must warn, not refuse: %v", err)
	}
	if !strings.Contains(warn.String(), "LOG_LEVEL") || strings.Contains(warn.String(), "OTHER") {
		t.Fatalf("warning = %q", warn.String())
	}
}

func TestEnvSet_NoSecretStoreAllowsEverything(t *testing.T) {
	mock := ssh.NewMockExecutor("h", ssh.MockCommand{Match: "if [ ! -e '/deployments/demo/secrets'", Output: "absent\n"})
	var warn bytes.Buffer
	if err := checkEnvSetShadowing(context.Background(), mock, &config.AppConfig{App: "demo"}, map[string]string{"A": "1"}, &warn); err != nil || warn.Len() != 0 {
		t.Fatalf("err=%v warn=%q", err, warn.String())
	}
}
