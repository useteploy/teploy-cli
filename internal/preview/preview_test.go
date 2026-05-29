package preview

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/useteploy/teploy/internal/ssh"
)

func TestSanitizeBranch(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"main", "main"},
		{"feature/login", "feature-login"},
		{"Feature_Page", "feature-page"},
		{"fix/bug#123", "fix-bug123"},
		{"---leading---", "leading"},
		{"a/b/c/d", "a-b-c-d"},
		{"UPPERCASE", "uppercase"},
	}

	for _, tt := range tests {
		got := SanitizeBranch(tt.input)
		if got != tt.want {
			t.Errorf("SanitizeBranch(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestDeploy(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "mkdir -p /deployments/myapp/previews", Output: ""},
		ssh.MockCommand{Match: "cat /deployments/myapp/previews/feature-login.json", Output: "", Err: nil},
		ssh.MockCommand{Match: "ss -tln", Output: ""},
		ssh.MockCommand{Match: "docker run", Output: "abc123"},
		ssh.MockCommand{Match: "curl -sf http://localhost:2019/config/apps/http/servers/srv0", Output: `{"listen":[":80",":443"]}`},
		ssh.MockCommand{Match: "curl -sf -X PATCH", Err: fmt.Errorf("not found")},
		ssh.MockCommand{Match: "curl -sf -X POST http://localhost:2019/config/apps/http/servers/srv0/routes", Output: ""},
		ssh.MockCommand{Match: "rm -f /tmp/teploy_caddy_config.json", Output: ""},
		ssh.MockCommand{Match: "cat /deployments/caddy/Caddyfile", Output: "{\n\tadmin 0.0.0.0:2019\n}\n"},
		ssh.MockCommand{Match: "mv /tmp/teploy_caddyfile.tmp", Output: ""},
		ssh.MockCommand{Match: "mkdir /deployments/caddy/.lock", Output: ""},
		ssh.MockCommand{Match: "docker exec caddy caddy reload", Output: ""},
		ssh.MockCommand{Match: "rmdir /deployments/caddy/.lock", Output: ""},
		ssh.MockCommand{Match: "UPLOAD:", Output: ""},
	)

	var buf bytes.Buffer
	mgr := NewManager(mock, &buf)
	err := mgr.Deploy(context.Background(), DeployConfig{
		App:     "myapp",
		Domain:  "myapp.com",
		Branch:  "feature/login",
		Image:   "myapp:latest",
		Version: "abc123",
	})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	output := buf.String()
	if !bytes.Contains([]byte(output), []byte("preview-feature-login.myapp.com")) {
		t.Errorf("expected preview domain in output, got: %s", output)
	}
}

func TestList_Empty(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "ls", Output: "", Err: nil},
	)

	var buf bytes.Buffer
	mgr := NewManager(mock, &buf)
	previews, err := mgr.List(context.Background(), "myapp")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(previews) != 0 {
		t.Errorf("expected 0 previews, got %d", len(previews))
	}
}

func TestPreviewDomain(t *testing.T) {
	tests := []struct {
		branch, domain, want string
	}{
		{"feature/login", "myapp.com", "preview-feature-login.myapp.com"},
		{"main", "example.com", "preview-main.example.com"},
	}
	for _, tt := range tests {
		got := previewDomain(tt.branch, tt.domain)
		if got != tt.want {
			t.Errorf("previewDomain(%q, %q) = %q, want %q", tt.branch, tt.domain, got, tt.want)
		}
	}
}
