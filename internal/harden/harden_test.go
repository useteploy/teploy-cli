package harden

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/ssh"
)

func TestConfigureUFW_AlreadyInstalled(t *testing.T) {
	mock := ssh.NewMockExecutor("server1",
		ssh.MockCommand{Match: "which ufw", Output: "/usr/sbin/ufw"},
		ssh.MockCommand{Match: "ufw default deny incoming && ufw default allow outgoing", Output: ""},
		ssh.MockCommand{Match: "ufw allow 22/tcp && ufw allow 80/tcp && ufw allow 443/tcp", Output: ""},
		ssh.MockCommand{Match: `sh -c 'echo "y" | ufw enable'`, Output: "Firewall is active"},
		ssh.MockCommand{Match: "ufw status", Output: "Status: active"},
	)

	var buf bytes.Buffer
	if err := ConfigureUFW(context.Background(), mock, &buf, ""); err != nil {
		t.Fatalf("ConfigureUFW: %v", err)
	}

	for _, call := range mock.Calls {
		if strings.Contains(call, "apt-get") {
			t.Error("should not install ufw when already present")
		}
	}

	if !strings.Contains(buf.String(), "UFW active") {
		t.Error("should report UFW status")
	}
}

func TestConfigureUFW_FreshInstall(t *testing.T) {
	mock := ssh.NewMockExecutor("server1",
		ssh.MockCommand{Match: "which ufw", Err: fmt.Errorf("not found")},
		ssh.MockCommand{Match: "DEBIAN_FRONTEND=noninteractive apt-get update", Output: ""},
		ssh.MockCommand{Match: "ufw default deny incoming", Output: ""},
		ssh.MockCommand{Match: "ufw allow 22/tcp", Output: ""},
		ssh.MockCommand{Match: `sh -c 'echo "y" | ufw enable'`, Output: "Firewall is active"},
		ssh.MockCommand{Match: "ufw status", Output: "Status: active"},
	)

	var buf bytes.Buffer
	if err := ConfigureUFW(context.Background(), mock, &buf, ""); err != nil {
		t.Fatalf("ConfigureUFW: %v", err)
	}

	if !strings.Contains(buf.String(), "Installing UFW") {
		t.Error("should report installing UFW")
	}
}

func TestConfigureUFW_WithSudo(t *testing.T) {
	mock := ssh.NewMockExecutor("server1",
		ssh.MockCommand{Match: "which ufw", Err: fmt.Errorf("not found")},
		ssh.MockCommand{Match: "sudo DEBIAN_FRONTEND=noninteractive apt-get update", Output: ""},
		ssh.MockCommand{Match: "sudo ufw default deny incoming", Output: ""},
		ssh.MockCommand{Match: "sudo ufw allow 22/tcp", Output: ""},
		ssh.MockCommand{Match: "sudo sh -c", Output: "Firewall is active"},
		ssh.MockCommand{Match: "sudo ufw status", Output: "Status: active"},
	)

	var buf bytes.Buffer
	if err := ConfigureUFW(context.Background(), mock, &buf, "sudo "); err != nil {
		t.Fatalf("ConfigureUFW: %v", err)
	}

	var foundSudo bool
	for _, call := range mock.Calls {
		if strings.HasPrefix(call, "sudo DEBIAN_FRONTEND=noninteractive apt-get") {
			foundSudo = true
		}
	}
	if !foundSudo {
		t.Error("expected sudo prefix on apt-get")
	}
}

func TestInstallFail2ban_AlreadyInstalled(t *testing.T) {
	mock := ssh.NewMockExecutor("server1",
		ssh.MockCommand{Match: "which fail2ban-server", Output: "/usr/bin/fail2ban-server"},
		ssh.MockCommand{Match: "systemctl enable --now fail2ban", Output: ""},
		ssh.MockCommand{Match: "echo $SSH_CONNECTION", Output: "203.0.113.7 54321 10.0.0.5 22"},
		ssh.MockCommand{Match: "UPLOAD:", Output: ""},
		ssh.MockCommand{Match: "mv /tmp/teploy-fail2ban-sshd.conf", Output: ""},
		ssh.MockCommand{Match: "systemctl restart fail2ban", Output: ""},
	)

	var buf bytes.Buffer
	if err := InstallFail2ban(context.Background(), mock, &buf, ""); err != nil {
		t.Fatalf("InstallFail2ban: %v", err)
	}

	for _, call := range mock.Calls {
		if strings.Contains(call, "apt-get") {
			t.Error("should not install fail2ban when already present")
		}
	}

	content, ok := mock.Files["/tmp/teploy-fail2ban-sshd.conf"]
	if !ok {
		t.Fatal("jail config not uploaded")
	}
	if !strings.Contains(string(content), "[sshd]") {
		t.Errorf("jail config should contain [sshd], got: %s", string(content))
	}
	if !strings.Contains(string(content), "mode = aggressive") {
		t.Errorf("jail config should contain aggressive mode, got: %s", string(content))
	}
	// ignoreip must protect loopback, the tailnet CGNAT range, and the IP
	// this session connected from — otherwise setup can ban the operator.
	for _, want := range []string{"ignoreip = ", "127.0.0.1/8", "100.64.0.0/10", "203.0.113.7"} {
		if !strings.Contains(string(content), want) {
			t.Errorf("jail config should contain %q, got: %s", want, string(content))
		}
	}
}

func TestInstallFail2ban_FreshInstall(t *testing.T) {
	mock := ssh.NewMockExecutor("server1",
		ssh.MockCommand{Match: "which fail2ban-server", Err: fmt.Errorf("not found")},
		ssh.MockCommand{Match: "DEBIAN_FRONTEND=noninteractive apt-get update", Output: ""},
		ssh.MockCommand{Match: "systemctl enable --now fail2ban", Output: ""},
		ssh.MockCommand{Match: "echo $SSH_CONNECTION", Output: "203.0.113.7 54321 10.0.0.5 22"},
		ssh.MockCommand{Match: "UPLOAD:", Output: ""},
		ssh.MockCommand{Match: "mv /tmp/teploy-fail2ban-sshd.conf", Output: ""},
		ssh.MockCommand{Match: "systemctl restart fail2ban", Output: ""},
	)

	var buf bytes.Buffer
	if err := InstallFail2ban(context.Background(), mock, &buf, ""); err != nil {
		t.Fatalf("InstallFail2ban: %v", err)
	}

	if !strings.Contains(buf.String(), "Installing fail2ban") {
		t.Error("should report installing fail2ban")
	}
}

func TestHardenSSH_NoAuthorizedKeys(t *testing.T) {
	mock := ssh.NewMockExecutor("server1",
		ssh.MockCommand{Match: "cat ~/.ssh/authorized_keys", Err: fmt.Errorf("no such file")},
		ssh.MockCommand{Match: "cat /root/.ssh/authorized_keys", Err: fmt.Errorf("no such file")},
	)

	var buf bytes.Buffer
	if err := HardenSSH(context.Background(), mock, &buf, ""); err != nil {
		t.Fatalf("HardenSSH: %v", err)
	}

	if !strings.Contains(buf.String(), "no authorized_keys found") {
		t.Error("should warn about missing authorized_keys")
	}

	for _, call := range mock.Calls {
		if strings.Contains(call, "sed") {
			t.Error("should not modify sshd_config when no keys found")
		}
	}
}

func TestHardenSSH_EmptyAuthorizedKeys(t *testing.T) {
	mock := ssh.NewMockExecutor("server1",
		ssh.MockCommand{Match: "cat ~/.ssh/authorized_keys", Output: "   \n\n"},
		ssh.MockCommand{Match: "cat /root/.ssh/authorized_keys", Output: "   \n\n"},
	)

	var buf bytes.Buffer
	if err := HardenSSH(context.Background(), mock, &buf, ""); err != nil {
		t.Fatalf("HardenSSH: %v", err)
	}

	if !strings.Contains(buf.String(), "no authorized_keys found") {
		t.Error("should warn about empty authorized_keys")
	}
}

func TestHardenSSH_KeysExist(t *testing.T) {
	mock := ssh.NewMockExecutor("server1",
		ssh.MockCommand{Match: "cat ~/.ssh/authorized_keys", Output: "ssh-ed25519 AAAAC3... user@host"},
		ssh.MockCommand{Match: "sed -i", Output: ""},
		ssh.MockCommand{Match: "sed -i", Output: ""},
		ssh.MockCommand{Match: "sed -i", Output: ""},
		ssh.MockCommand{Match: "systemctl restart sshd", Output: ""},
	)

	var buf bytes.Buffer
	if err := HardenSSH(context.Background(), mock, &buf, ""); err != nil {
		t.Fatalf("HardenSSH: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "PermitRootLogin prohibit-password") {
		t.Error("should report PermitRootLogin change")
	}
	if !strings.Contains(output, "PasswordAuthentication no") {
		t.Error("should report PasswordAuthentication change")
	}
	if !strings.Contains(output, "PubkeyAuthentication yes") {
		t.Error("should report PubkeyAuthentication change")
	}

	var sedCount int
	for _, call := range mock.Calls {
		if strings.Contains(call, "sed -i") {
			sedCount++
		}
	}
	if sedCount != 3 {
		t.Errorf("expected 3 sed calls, got %d", sedCount)
	}
}

func TestHarden_RunsAllSteps_AsRoot(t *testing.T) {
	mock := ssh.NewMockExecutor("server1",
		// detectSudo
		ssh.MockCommand{Match: "whoami", Output: "root"},
		// UFW
		ssh.MockCommand{Match: "which ufw", Output: "/usr/sbin/ufw"},
		ssh.MockCommand{Match: "ufw default deny incoming", Output: ""},
		ssh.MockCommand{Match: "ufw allow 22/tcp", Output: ""},
		ssh.MockCommand{Match: `sh -c 'echo "y" | ufw enable'`, Output: ""},
		ssh.MockCommand{Match: "ufw status", Output: "Status: active"},
		// fail2ban
		ssh.MockCommand{Match: "which fail2ban-server", Output: "/usr/bin/fail2ban-server"},
		ssh.MockCommand{Match: "systemctl enable --now fail2ban", Output: ""},
		ssh.MockCommand{Match: "echo $SSH_CONNECTION", Output: "203.0.113.7 54321 10.0.0.5 22"},
		ssh.MockCommand{Match: "UPLOAD:", Output: ""},
		ssh.MockCommand{Match: "mv /tmp/teploy-fail2ban-sshd.conf", Output: ""},
		ssh.MockCommand{Match: "systemctl restart fail2ban", Output: ""},
		// SSH
		ssh.MockCommand{Match: "cat ~/.ssh/authorized_keys", Output: "ssh-ed25519 AAAAC3... user@host"},
		ssh.MockCommand{Match: "sed -i", Output: ""},
		ssh.MockCommand{Match: "sed -i", Output: ""},
		ssh.MockCommand{Match: "sed -i", Output: ""},
		ssh.MockCommand{Match: "systemctl restart sshd", Output: ""},
		// audit
		ssh.MockCommand{Match: "which auditctl", Output: "/usr/sbin/auditctl"},
		ssh.MockCommand{Match: "mv /tmp/teploy-audit.rules", Output: ""},
		ssh.MockCommand{Match: "augenrules --load", Output: ""},
		ssh.MockCommand{Match: "systemctl enable --now auditd", Output: ""},
		ssh.MockCommand{Match: "visudo -c", Output: "parsed OK"},
		ssh.MockCommand{Match: "mv /tmp/teploy-audit-sudoers", Output: ""},
	)

	var buf bytes.Buffer
	if err := Harden(context.Background(), mock, &buf); err != nil {
		t.Fatalf("Harden: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "Configuring UFW") {
		t.Error("should include UFW step")
	}
	if !strings.Contains(output, "Configuring fail2ban") {
		t.Error("should include fail2ban step")
	}
	if !strings.Contains(output, "Hardening SSH") {
		t.Error("should include SSH step")
	}
}

func TestHarden_RunsAllSteps_WithSudo(t *testing.T) {
	mock := ssh.NewMockExecutor("server1",
		// detectSudo
		ssh.MockCommand{Match: "whoami", Output: "tyler"},
		// UFW
		ssh.MockCommand{Match: "which ufw", Output: "/usr/sbin/ufw"},
		ssh.MockCommand{Match: "sudo ufw default deny incoming", Output: ""},
		ssh.MockCommand{Match: "sudo ufw allow 22/tcp", Output: ""},
		ssh.MockCommand{Match: "sudo sh -c", Output: ""},
		ssh.MockCommand{Match: "sudo ufw status", Output: "Status: active"},
		// fail2ban
		ssh.MockCommand{Match: "which fail2ban-server", Output: "/usr/bin/fail2ban-server"},
		ssh.MockCommand{Match: "sudo systemctl enable --now fail2ban", Output: ""},
		ssh.MockCommand{Match: "echo $SSH_CONNECTION", Output: "203.0.113.7 54321 10.0.0.5 22"},
		ssh.MockCommand{Match: "UPLOAD:", Output: ""},
		ssh.MockCommand{Match: "sudo mv /tmp/teploy-fail2ban-sshd.conf", Output: ""},
		ssh.MockCommand{Match: "sudo systemctl restart fail2ban", Output: ""},
		// SSH
		ssh.MockCommand{Match: "cat ~/.ssh/authorized_keys", Output: "ssh-ed25519 AAAAC3... user@host"},
		ssh.MockCommand{Match: "sudo sed -i", Output: ""},
		ssh.MockCommand{Match: "sudo sed -i", Output: ""},
		ssh.MockCommand{Match: "sudo sed -i", Output: ""},
		ssh.MockCommand{Match: "sudo systemctl restart sshd", Output: ""},
		// audit
		ssh.MockCommand{Match: "which auditctl", Output: "/usr/sbin/auditctl"},
		ssh.MockCommand{Match: "sudo mv /tmp/teploy-audit.rules", Output: ""},
		ssh.MockCommand{Match: "sudo augenrules --load", Output: ""},
		ssh.MockCommand{Match: "sudo systemctl enable --now auditd", Output: ""},
		ssh.MockCommand{Match: "visudo -c", Output: "parsed OK"},
		ssh.MockCommand{Match: "sudo mv /tmp/teploy-audit-sudoers", Output: ""},
	)

	var buf bytes.Buffer
	if err := Harden(context.Background(), mock, &buf); err != nil {
		t.Fatalf("Harden: %v", err)
	}

	// Verify sudo was used on privileged commands.
	var foundSudo bool
	for _, call := range mock.Calls {
		if strings.HasPrefix(call, "sudo ") {
			foundSudo = true
			break
		}
	}
	if !foundSudo {
		t.Error("expected sudo prefix for non-root user")
	}
}
