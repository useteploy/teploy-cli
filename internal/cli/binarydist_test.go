package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

func TestInstallServerBinaryValidatesBeforePublication(t *testing.T) {
	for _, tt := range []struct {
		name, binary string
		capability   []string
	}{
		{"wrong-version", "#!/bin/sh\necho teploy 8.0.0\n", nil},
		{"cannot-run", "#!/bin/sh\nexit 2\n", nil},
		{"missing-capability", "#!/bin/sh\nif [ \"$1\" = version ]; then echo teploy 9.0.0; else exit 1; fi\n", []string{"autodeploy redeploy"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dst := filepath.Join(t.TempDir(), "teploy")
			if err := os.WriteFile(dst, []byte("incumbent"), 0700); err != nil {
				t.Fatal(err)
			}
			err := installServerBinary(context.Background(), ssh.NewLocalExecutor(), dst, []byte(tt.binary), "9.0.0", tt.capability...)
			if err == nil {
				t.Fatal("rejected candidate accepted")
			}
			got, err := os.ReadFile(dst)
			if err != nil || string(got) != "incumbent" {
				t.Fatalf("incumbent changed: %q %v", got, err)
			}
		})
	}
}

func TestInstallServerBinaryConcurrentPublication(t *testing.T) {
	if _, err := exec.LookPath("flock"); err != nil {
		t.Skip("flock unavailable (Linux server publication contract)")
	}
	dst := filepath.Join(t.TempDir(), "teploy")
	failures := make(chan error, 4)
	for i := 0; i < 4; i++ {
		go func(i int) {
			failures <- installServerBinary(context.Background(), ssh.NewLocalExecutor(), dst, []byte(fmt.Sprintf("#!/bin/sh\n# candidate %d\necho teploy 9.0.0\n", i)), "9.0.0")
		}(i)
	}
	for i := 0; i < 4; i++ {
		if err := <-failures; err != nil {
			t.Fatal(err)
		}
	}
	output, err := exec.Command(dst, "version").Output()
	if err != nil || strings.TrimSpace(string(output)) != "teploy 9.0.0" {
		t.Fatalf("published candidate: %q %v", output, err)
	}
}

func TestInstallServerBinaryUsesSudoForProtectedDestination(t *testing.T) {
	executor := ssh.NewMockExecutor("test",
		ssh.MockCommand{Match: "umask 077; mktemp -d", Output: "/tmp/teploy-install.test"},
		ssh.MockCommand{Match: "'/tmp/teploy-install.test/teploy' version", Output: "teploy 9.0.0"},
		ssh.MockCommand{Match: "id -u", Output: "1000"},
		ssh.MockCommand{Match: "sudo -n flock -w 30", Output: ""},
		ssh.MockCommand{Match: "rm -rf --", Output: ""},
	)
	if err := installServerBinary(context.Background(), executor, "/usr/local/bin/teploy", []byte("binary"), "9.0.0"); err != nil {
		t.Fatal(err)
	}
	var published bool
	for _, command := range executor.Calls {
		if strings.HasPrefix(command, "sudo -n flock -w 30 ") {
			published = true
		}
	}
	if !published {
		t.Fatal("protected binary publication bypassed sudo")
	}
	for uploaded := range executor.Files {
		if !strings.HasPrefix(uploaded, "/tmp/teploy-install.test/") {
			t.Fatalf("privileged direct upload %s", uploaded)
		}
	}
}

func TestInstallServerBinaryNonRootIntegration(t *testing.T) {
	if os.Getenv("TEPLOY_INSTALL_INTEGRATION") != "1" {
		t.Skip("requires a disposable Linux host with passwordless sudo")
	}
	if os.Geteuid() == 0 {
		t.Fatal("integration must exercise a non-root installer")
	}
	output, err := exec.Command("sudo", "-n", "mktemp", "-d", "/usr/local/bin/teploy-campaign.XXXXXXXX").Output()
	if err != nil {
		t.Fatal(err)
	}
	dir := strings.TrimSpace(string(output))
	if filepath.Dir(dir) != "/usr/local/bin" || !strings.HasPrefix(filepath.Base(dir), "teploy-campaign.") {
		t.Fatal("invalid disposable destination")
	}
	defer exec.Command("sudo", "-n", "rm", "-rf", "--", dir).Run()
	if err := exec.Command("sudo", "-n", "chmod", "0755", dir).Run(); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(dir, "teploy")
	good := []byte("#!/bin/sh\necho teploy 9.0.0\n")
	if err := installServerBinary(context.Background(), ssh.NewLocalExecutor(), destination, good, "9.0.0"); err != nil {
		t.Fatal(err)
	}
	if err := installServerBinary(context.Background(), ssh.NewLocalExecutor(), destination, []byte("#!/bin/sh\nexit 3\n"), "9.0.0"); err == nil {
		t.Fatal("broken candidate accepted")
	}
	actual, err := exec.Command(destination, "version").Output()
	if err != nil || strings.TrimSpace(string(actual)) != "teploy 9.0.0" {
		t.Fatalf("incumbent damaged: %q %v", actual, err)
	}
}
