package openbao

import (
	"context"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/ssh"
)

func TestAppPath(t *testing.T) {
	if got := appPath("myapp", "db"); got != "secret/myapp/db" {
		t.Errorf("appPath = %q", got)
	}
	if got := appPath("myapp", ""); got != "secret/myapp" {
		t.Errorf("appPath empty name = %q", got)
	}
}

// TestBaoTransport_SecretsNeverInArgv pins the C08 secret transport for
// every bao invocation: the auth token and the secret VALUES must travel
// the session's stdin, never the docker exec command string (where the
// server's process list would show them). Pins Put, the token-only bao
// shape, and the policy-write path.
func TestBaoTransport_SecretsNeverInArgv(t *testing.T) {
	const (
		rootToken = "hvs.root-token"
		apiKey    = "sk-live-123"
	)
	mock := ssh.NewMockExecutor("h",
		ssh.MockCommand{Match: "if [ ! -e ", Output: "present"},
		ssh.MockCommand{Match: "age -d", Output: rootToken},
		ssh.MockCommand{Match: "docker exec -i 'myapp-openbao'", Output: "{}"},
	)
	c := NewClient(mock, &strings.Builder{})
	// The framed existence check reads the recorded file state; the age
	// registration answers the decrypt that follows it.
	mock.Files["/deployments/myapp/secrets/VAULT_ROOT_TOKEN.age"] = []byte("ciphertext")

	ctx := context.Background()
	if err := c.Put(ctx, "myapp", "", "db", []string{"API_KEY=" + apiKey, "HOST=db"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := c.Get(ctx, "myapp", "", "db"); err != nil {
		t.Fatalf("Get: %v", err)
	}

	for _, call := range mock.Calls {
		if strings.HasPrefix(call, "UPLOAD:") {
			continue
		}
		if strings.Contains(call, rootToken) {
			t.Errorf("root token in command argv: %s", call)
		}
		if strings.Contains(call, apiKey) {
			t.Errorf("secret value in command argv: %s", call)
		}
		// The only token the argv may name is the stdin-fed variable —
		// never a literal assignment.
		if strings.Contains(call, "BAO_TOKEN=") && !strings.Contains(call, `BAO_TOKEN="$teploy_tok"`) {
			t.Errorf("token env must be fed from stdin, not embedded: %s", call)
		}
	}

	// The stdin payloads carry what the argv must not: Put's payload is
	// token line + JSON body; Get's is token line + newline.
	putInput := ""
	for _, in := range mock.Inputs {
		if strings.Contains(in, "API_KEY") {
			putInput = in
		}
	}
	if putInput == "" {
		t.Fatalf("Put's stdin payload not recorded: %v", mock.Inputs)
	}
	lines := strings.SplitN(putInput, "\n", 2)
	if lines[0] != rootToken {
		t.Errorf("first stdin line must be the token, got %q", lines[0])
	}
	if !strings.Contains(lines[1], `"API_KEY":"`+apiKey+`"`) {
		t.Errorf("JSON payload must carry the secret value: %q", lines[1])
	}
}

// TestWriteAppPolicyTransport pins the policy write: root token on stdin,
// policy HCL on stdin, nothing secret in the command string.
func TestWriteAppPolicyTransport(t *testing.T) {
	mock := ssh.NewMockExecutor("h",
		ssh.MockCommand{Match: "docker exec -i 'myapp-openbao'", Output: ""},
	)
	c := NewClient(mock, &strings.Builder{})
	if err := c.writeAppPolicy(context.Background(), "myapp-openbao", "hvs.tok", "myapp", false); err != nil {
		t.Fatalf("writeAppPolicy: %v", err)
	}
	for _, call := range mock.Calls {
		if strings.Contains(call, "hvs.tok") {
			t.Errorf("root token in policy-write argv: %s", call)
		}
		if strings.Contains(call, "base64") {
			t.Errorf("policy transport no longer needs base64 argv quoting: %s", call)
		}
	}
	found := false
	for _, in := range mock.Inputs {
		if strings.HasPrefix(in, "hvs.tok\n") && strings.Contains(in, "capabilities") {
			found = true
		}
	}
	if !found {
		t.Errorf("policy stdin must be token line + HCL, got %v", mock.Inputs)
	}
}

// TestEnableDatabaseSecretsTransport pins the db engine config writes:
// the admin password rides the JSON stdin payload, never the command.
func TestEnableDatabaseSecretsTransport(t *testing.T) {
	const adminPass = "pg-super-secret"
	mock := ssh.NewMockExecutor("h",
		ssh.MockCommand{Match: "if [ ! -e ", Output: "present"},
		ssh.MockCommand{Match: "age -d", Output: "hvs.root"},
		ssh.MockCommand{Match: "docker exec -i 'myapp-openbao'", Output: "{}"},
	)
	c := NewClient(mock, &strings.Builder{})
	mock.Files["/deployments/myapp/secrets/VAULT_ROOT_TOKEN.age"] = []byte("ciphertext")

	err := c.EnableDatabaseSecrets(context.Background(), DBSetupOptions{
		App:         "myapp",
		DBAccessory: "postgres",
		AdminPass:   adminPass,
	})
	if err != nil {
		t.Fatalf("EnableDatabaseSecrets: %v", err)
	}
	for _, call := range mock.Calls {
		if strings.HasPrefix(call, "UPLOAD:") {
			continue
		}
		if strings.Contains(call, adminPass) {
			t.Errorf("admin password in command argv: %s", call)
		}
	}
	inConfig := false
	for _, in := range mock.Inputs {
		if strings.Contains(in, `"password":"`+adminPass+`"`) {
			inConfig = true
		}
	}
	if !inConfig {
		t.Errorf("admin password must ride the JSON stdin payload, inputs: %v", mock.Inputs)
	}
}
