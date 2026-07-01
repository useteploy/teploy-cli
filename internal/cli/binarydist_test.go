package cli

import (
	"context"
	"testing"

	"github.com/useteploy/teploy/internal/ssh"
)

func TestServerPlatform(t *testing.T) {
	cases := []struct {
		unameS, unameM   string
		wantOS, wantArch string
	}{
		{"Linux", "x86_64", "linux", "amd64"},
		{"Linux", "aarch64", "linux", "arm64"},
		{"Linux", "arm64", "linux", "arm64"},
		{"Darwin", "arm64", "darwin", "arm64"},
		{"Darwin", "x86_64", "darwin", "amd64"},
	}
	for _, c := range cases {
		mock := ssh.NewMockExecutor("1.2.3.4",
			ssh.MockCommand{Match: "uname -s", Output: c.unameS},
			ssh.MockCommand{Match: "uname -m", Output: c.unameM},
		)
		goos, goarch, err := serverPlatform(context.Background(), mock)
		if err != nil {
			t.Errorf("serverPlatform(%s, %s): unexpected error: %v", c.unameS, c.unameM, err)
			continue
		}
		if goos != c.wantOS || goarch != c.wantArch {
			t.Errorf("serverPlatform(%s, %s) = (%s, %s), want (%s, %s)",
				c.unameS, c.unameM, goos, goarch, c.wantOS, c.wantArch)
		}
	}
}

func TestServerPlatform_UnsupportedOS(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "uname -s", Output: "Windows_NT"},
	)
	if _, _, err := serverPlatform(context.Background(), mock); err == nil {
		t.Error("expected an error for an unsupported OS")
	}
}

func TestServerPlatform_UnsupportedArch(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "uname -s", Output: "Linux"},
		ssh.MockCommand{Match: "uname -m", Output: "i686"},
	)
	if _, _, err := serverPlatform(context.Background(), mock); err == nil {
		t.Error("expected an error for an unsupported architecture")
	}
}
