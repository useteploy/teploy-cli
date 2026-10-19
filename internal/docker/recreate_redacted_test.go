package docker

import (
	"context"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/ssh"
)

// A persisted (redacted) recreate spec lacks secret values by design;
// recreating from it must fail closed before any docker command runs.
func TestRecreate_RefusesRedactedSpec(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4")
	spec := &RecreateSpec{
		Name:        "myapp-web-v1",
		ImageID:     "sha256:abc",
		Env:         []string{"PORT=3000"},
		EnvRedacted: []RedactedEnv{{Key: "DB_PASSWORD", Source: "secret_store"}},
	}
	err := NewClient(mock).Recreate(context.Background(), spec, nil)
	if err == nil || !strings.Contains(err.Error(), "DB_PASSWORD") || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("expected a fail-closed refusal naming the key, got %v", err)
	}
	for _, c := range mock.Calls {
		if strings.Contains(c, "docker") {
			t.Errorf("no docker command may run for a redacted spec, ran: %s", c)
		}
	}
}

func TestImageEnv(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4", ssh.MockCommand{
		Match:  "docker image inspect -f '{{json .Config.Env}}' 'sha256:abc'",
		Output: `["PATH=/usr/bin","NODE_ENV=production"]`,
	})
	env, err := NewClient(mock).ImageEnv(context.Background(), "sha256:abc")
	if err != nil || env["PATH"] != "/usr/bin" || env["NODE_ENV"] != "production" {
		t.Fatalf("ImageEnv = %v, %v", env, err)
	}
}
