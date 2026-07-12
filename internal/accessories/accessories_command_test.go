package accessories

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/ssh"
)

// The command: override must land as trailing args after the image —
// MinIO/ntfy style images need an explicit verb (`server /data`, `serve`).
func TestEnsureRunningAppendsCommand(t *testing.T) {
	mock := ssh.NewMockExecutor("h",
		ssh.MockCommand{Match: "docker inspect", Output: "exited"},
		ssh.MockCommand{Match: "mkdir -p", Output: ""},
		ssh.MockCommand{Match: "docker run", Output: "abc"},
	)
	m := NewManager(mock, io.Discard)
	_, err := m.EnsureRunning(context.Background(), "myapp", "minio", config.AccessoryConfig{
		Image:   "minio/minio:latest",
		Port:    9000,
		Command: "server /data --console-address :9001",
	})
	if err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	var run string
	for _, c := range mock.Calls {
		if strings.HasPrefix(c, "docker run") {
			run = c
		}
	}
	if !strings.Contains(run, "'minio/minio:latest' 'server' '/data' '--console-address' ':9001'") {
		t.Errorf("command not appended after image: %s", run)
	}
}

// Without command:, the docker run line must end at the image (no regression).
func TestEnsureRunningNoCommandUnchanged(t *testing.T) {
	mock := ssh.NewMockExecutor("h",
		ssh.MockCommand{Match: "docker inspect", Output: "exited"},
		ssh.MockCommand{Match: "mkdir -p", Output: ""},
		ssh.MockCommand{Match: "docker run", Output: "abc"},
	)
	m := NewManager(mock, io.Discard)
	_, err := m.EnsureRunning(context.Background(), "myapp", "db", config.AccessoryConfig{
		Image: "postgres:16",
	})
	if err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	var run string
	for _, c := range mock.Calls {
		if strings.HasPrefix(c, "docker run") {
			run = c
		}
	}
	if !strings.HasSuffix(strings.TrimSpace(run), "'postgres:16'") {
		t.Errorf("docker run should end at the image: %s", run)
	}
}
