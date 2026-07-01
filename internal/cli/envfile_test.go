package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/ssh"
)

// TestBuildContainerEnvFiles_SecretsNeverBecomeArgs is the regression test
// for the fix in this file: decrypted secrets used to be merged into a map
// passed as `docker run -e KEY=value` arguments (visible in `ps aux` /
// `/proc/<pid>/cmdline` on the host for the life of the invocation). They
// must now only ever reach the server via Upload (SFTP-style, never a
// shell command string).
func TestBuildContainerEnvFiles_SecretsNeverBecomeArgs(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4")

	secrets := map[string]string{"API_KEY": "super-secret-value"}
	envFiles, err := buildContainerEnvFiles(context.Background(), mock, "myapp", "", nil, nil, secrets)
	if err != nil {
		t.Fatalf("buildContainerEnvFiles: %v", err)
	}

	// The secret must never appear in any exec.Run call — only in an
	// uploaded file.
	for _, call := range mock.Calls {
		if strings.Contains(call, "super-secret-value") {
			t.Fatalf("secret value leaked into a shell command: %q", call)
		}
	}

	if len(envFiles) != 1 {
		t.Fatalf("expected 1 env file, got %d: %v", len(envFiles), envFiles)
	}
	data, ok := mock.Files[envFiles[0]]
	if !ok {
		t.Fatalf("expected secret env file to be uploaded to %s", envFiles[0])
	}
	if !strings.Contains(string(data), "API_KEY=super-secret-value") {
		t.Errorf("uploaded env file missing secret, got: %s", data)
	}
}

func TestBuildContainerEnvFiles_PersistedFileComesFirst(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4")

	envFiles, err := buildContainerEnvFiles(context.Background(), mock, "myapp",
		"/deployments/myapp/.env",
		map[string]string{"NODE_ENV": "production"},
		nil, nil,
	)
	if err != nil {
		t.Fatalf("buildContainerEnvFiles: %v", err)
	}
	if len(envFiles) != 2 {
		t.Fatalf("expected 2 env files, got %d: %v", len(envFiles), envFiles)
	}
	if envFiles[0] != "/deployments/myapp/.env" {
		t.Errorf("expected persisted env file first, got %s", envFiles[0])
	}
}

func TestBuildContainerEnvFiles_NoValuesNoUpload(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4")

	envFiles, err := buildContainerEnvFiles(context.Background(), mock, "myapp", "", nil, nil, nil)
	if err != nil {
		t.Fatalf("buildContainerEnvFiles: %v", err)
	}
	if len(envFiles) != 0 {
		t.Errorf("expected no env files when there's nothing to write, got %v", envFiles)
	}
	if len(mock.Files) != 0 {
		t.Errorf("expected no upload when there's nothing to write, got %v", mock.Files)
	}
}

// TestBuildContainerEnvFiles_SecretWinsOverPlaintextDefault preserves the
// precedence the old -e-args implementation had: a `teploy secret` always
// overrides a plaintext default from teploy.yml's env: block for the same
// key. Docker's own precedence rules make -e always beat --env-file
// regardless of flag order, so this can only be preserved by merging both
// into the SAME file (secrets applied last) rather than splitting them
// across -e and --env-file.
func TestBuildContainerEnvFiles_SecretWinsOverPlaintextDefault(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4")

	appEnv := map[string]string{"API_KEY": "plaintext-default"}
	secrets := map[string]string{"API_KEY": "real-secret"}
	envFiles, err := buildContainerEnvFiles(context.Background(), mock, "myapp", "", appEnv, nil, secrets)
	if err != nil {
		t.Fatalf("buildContainerEnvFiles: %v", err)
	}
	data := string(mock.Files[envFiles[len(envFiles)-1]])
	if !strings.Contains(data, "API_KEY=real-secret") {
		t.Errorf("expected secret to win over plaintext default, got: %s", data)
	}
	if strings.Contains(data, "plaintext-default") {
		t.Errorf("plaintext default should have been overridden, got: %s", data)
	}
}
