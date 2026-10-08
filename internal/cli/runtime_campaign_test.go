package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/ssh"
	"io"
	"os"
	"testing"
)

func TestCampaignNetbirdSetupUsesTargetProvider(t *testing.T) {
	m := ssh.NewMockExecutor("dummy", ssh.MockCommand{Match: "whoami", Output: "root"}, ssh.MockCommand{Match: "which netbird", Output: "/usr/bin/netbird"}, ssh.MockCommand{Match: "netbird status", Output: "Management: Connected\nSignal: Connected\nNetBird IP: 100.80.1.2/16"})
	got, err := setupNetwork(context.Background(), m, &bytes.Buffer{}, "netbird", "dummy-key")
	if err != nil || got != "100.80.1.2" {
		t.Fatalf("%s %v", got, err)
	}
}
func TestCampaignInvalidJSONValidationFails(t *testing.T) {
	original := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	defer func() { os.Stdout = original }()
	err = outputResult(&Flags{JSON: true}, &validationResult{Valid: false, Errors: []string{"dummy invalid"}})
	w.Close()
	b, _ := io.ReadAll(r)
	r.Close()
	if err == nil {
		t.Fatal("invalid config exited successfully")
	}
	var result validationResult
	if json.Unmarshal(b, &result) != nil || result.Valid {
		t.Fatalf("invalid JSON result %s", b)
	}
}

func TestCampaignLiteralVaultValueDoesNotResolve(t *testing.T) {
	m := ssh.NewMockExecutor("host")
	cfg := &config.AppConfig{App: "demo", Env: map[string]string{"PASSWORD": "vault:literal#password"}, EnvLiteral: map[string]string{"PASSWORD": "vault:literal#password"}}
	got, err := mergeSecretVaultRefs(context.Background(), m, cfg, nil)
	if err != nil || len(got) != 0 || len(m.Calls) != 0 {
		t.Fatalf("literal reference resolved: %v %v calls=%v", got, err, m.Calls)
	}
}
