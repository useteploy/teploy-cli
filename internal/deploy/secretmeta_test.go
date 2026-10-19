package deploy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/releasemeta"
	"github.com/useteploy/teploy/internal/ssh"
)

// The release record never carries a secret value: secrets from the age
// store, the persisted .env, env_files and unknown sources are named only,
// plain config and image defaults keep their values. The attempt env file
// (a plaintext copy of resolved env) is removed once the release is live.
func TestDeploy_ReleaseRecordHoldsNoSecretValues(t *testing.T) {
	imageDigest := "sha256:" + strings.Repeat("a", 64)
	inspect := fmt.Sprintf(strings.Replace(f14Inspect, `"Env": ["PORT=3000", "API_KEY=x"]`,
		`"Env": ["PORT=3000", "PATH=/usr/bin", "LOG_LEVEL=debug", "AGE_KEY=age-secret-1", "DOTENV_KEY=dotenv-secret-2", "FILE_KEY=envfile-secret-3", "MYSTERY=mystery-secret-4", "DATABASE_URL=postgres://u:urlpass-5@db/x"]`, 1), strings.Repeat("9", 64))
	const attEnv = "/deployments/myapp/meta/att/v9.abcd1234/env"
	mock := deployRecordFixture(imageDigest,
		ssh.MockCommand{Match: "docker inspect 'myapp-web-v9'", Output: inspect},
		ssh.MockCommand{Match: "ls -1 '/deployments/myapp'/secrets", Output: "AGE_KEY\n"},
		ssh.MockCommand{Match: "test -f '/deployments/myapp'/.env", Output: "DOTENV_KEY\n"},
		ssh.MockCommand{Match: "docker image inspect -f '{{json .Config.Env}}'", Output: `["PATH=/usr/bin"]`},
		ssh.MockCommand{Match: "rm -f -- ", Output: ""},
	)
	mock.Files[attEnv] = []byte("AGE_KEY=age-secret-1\n")

	var buf bytes.Buffer
	err := NewDeployer(mock, &buf).Deploy(context.Background(), Config{
		App: "myapp", Domain: "myapp.com", Image: "myapp:v9", Version: "v9", ContainerPort: 3000,
		EnvFiles: []string{"/deployments/myapp/.env", attEnv},
		EnvProvenance: map[string]string{
			"LOG_LEVEL":    releasemeta.EnvSourceConfig,
			"FILE_KEY":     releasemeta.EnvSourceEnvFile,
			"AGE_KEY":      releasemeta.EnvSourceSecretStore,
			"DATABASE_URL": releasemeta.EnvSourceConfig,
		},
	})
	if err != nil {
		t.Fatalf("Deploy: %v\n%s", err, buf.String())
	}
	raw := mock.Files["/deployments/myapp/meta/v9.json"]
	if raw == nil {
		t.Fatalf("record not written\n%s", buf.String())
	}
	for _, secret := range []string{"age-secret-1", "dotenv-secret-2", "envfile-secret-3", "mystery-secret-4", "urlpass-5"} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Errorf("release record contains a secret value")
		}
		if strings.Contains(buf.String(), secret) {
			t.Errorf("deploy output contains a secret value")
		}
	}
	var rec releasemeta.Record
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(rec.Recreate.Env, ","); got != "PORT=3000,PATH=/usr/bin,LOG_LEVEL=debug" {
		t.Errorf("kept recreate env = %s", got)
	}
	want := map[string]string{
		"AGE_KEY": releasemeta.EnvSourceSecretStore, "DOTENV_KEY": releasemeta.EnvSourcePersistedEnv,
		"FILE_KEY": releasemeta.EnvSourceEnvFile, "MYSTERY": releasemeta.EnvSourceUnclassified,
		"DATABASE_URL": releasemeta.EnvSourceCredentialURL,
	}
	got := map[string]string{}
	for _, r := range rec.Recreate.EnvRedacted {
		got[r.Key] = r.Source
	}
	for k, src := range want {
		if got[k] != src {
			t.Errorf("redacted %s source = %q, want %q", k, got[k], src)
		}
	}
	if _, still := mock.Files[attEnv]; still {
		t.Error("attempt env file must be removed once the release is live")
	}
	for _, c := range mock.Calls {
		if strings.HasPrefix(c, "rm -f -- ") && !strings.Contains(c, "/meta/att/") {
			t.Errorf("only attempt env files may be removed, ran: %s", c)
		}
	}
}
