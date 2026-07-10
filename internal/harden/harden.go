package harden

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/useteploy/teploy/internal/ssh"
)

// detectSudo returns "sudo " if the current user is not root, or "" if root.
func detectSudo(ctx context.Context, exec ssh.Executor) string {
	if whoami, _ := exec.Run(ctx, "whoami"); strings.TrimSpace(whoami) != "root" {
		return "sudo "
	}
	return ""
}

// ConfigureUFW installs and configures UFW with sensible defaults.
// Idempotent — safe to run multiple times.
func ConfigureUFW(ctx context.Context, exec ssh.Executor, w io.Writer, sudo string) error {
	fmt.Fprintln(w, "Configuring UFW...")

	if _, err := exec.Run(ctx, "which ufw"); err != nil {
		fmt.Fprintln(w, "  Installing UFW...")
		if _, err := exec.Run(ctx, sudo+"DEBIAN_FRONTEND=noninteractive apt-get update -qq && "+sudo+"DEBIAN_FRONTEND=noninteractive apt-get install -y -qq ufw"); err != nil {
			return fmt.Errorf("installing ufw: %w", err)
		}
	}

	if _, err := exec.Run(ctx, sudo+"ufw default deny incoming && "+sudo+"ufw default allow outgoing"); err != nil {
		return fmt.Errorf("setting ufw defaults: %w", err)
	}

	if _, err := exec.Run(ctx, sudo+"ufw allow 22/tcp && "+sudo+"ufw allow 80/tcp && "+sudo+"ufw allow 443/tcp"); err != nil {
		return fmt.Errorf("allowing essential ports: %w", err)
	}

	if _, err := exec.Run(ctx, sudo+`sh -c 'echo "y" | ufw enable'`); err != nil {
		return fmt.Errorf("enabling ufw: %w", err)
	}

	out, _ := exec.Run(ctx, sudo+"ufw status")
	fmt.Fprintf(w, "  UFW active: %s\n", splitFirstLine(out))
	return nil
}

// InstallFail2ban installs and configures fail2ban with an SSH jail.
// Idempotent — safe to run multiple times.
func InstallFail2ban(ctx context.Context, exec ssh.Executor, w io.Writer, sudo string) error {
	fmt.Fprintln(w, "Configuring fail2ban...")

	if _, err := exec.Run(ctx, "which fail2ban-server"); err != nil {
		fmt.Fprintln(w, "  Installing fail2ban...")
		if _, err := exec.Run(ctx, sudo+"DEBIAN_FRONTEND=noninteractive apt-get update -qq && "+sudo+"DEBIAN_FRONTEND=noninteractive apt-get install -y -qq fail2ban"); err != nil {
			return fmt.Errorf("installing fail2ban: %w", err)
		}
	}

	if _, err := exec.Run(ctx, sudo+"systemctl enable --now fail2ban"); err != nil {
		return fmt.Errorf("enabling fail2ban: %w", err)
	}

	// Never ban the operator's own management traffic. Aggressive mode with a
	// low maxretry will otherwise ban a trusted IP after a couple of failed
	// auth attempts, locking the operator out of SSH for the full bantime
	// (observed in the wild: a Tailscale IP banned for 24h after two failed
	// pubkey attempts). ignoreip always covers loopback and the Tailscale
	// CGNAT range (100.64.0.0/10, the mesh Teploy provisions over), plus the
	// address this very session is connecting from — so `teploy setup` can
	// never lock out the machine that ran it.
	ignore := []string{"127.0.0.1/8", "::1", "100.64.0.0/10"}
	if conn, _ := exec.Run(ctx, "echo $SSH_CONNECTION"); strings.TrimSpace(conn) != "" {
		// SSH_CONNECTION = "<client-ip> <client-port> <server-ip> <server-port>".
		if fields := strings.Fields(conn); len(fields) >= 1 {
			if ip := fields[0]; ip != "" && !contains(ignore, ip) {
				ignore = append(ignore, ip)
			}
		}
	}

	jailCfg := fmt.Sprintf("[sshd]\nenabled = true\nmode = aggressive\nignoreip = %s\n", strings.Join(ignore, " "))
	if err := exec.Upload(ctx, strings.NewReader(jailCfg), "/tmp/teploy-fail2ban-sshd.conf", "0644"); err != nil {
		return fmt.Errorf("writing fail2ban jail config: %w", err)
	}
	if _, err := exec.Run(ctx, sudo+"mv /tmp/teploy-fail2ban-sshd.conf /etc/fail2ban/jail.d/sshd.conf"); err != nil {
		return fmt.Errorf("installing fail2ban jail config: %w", err)
	}

	if _, err := exec.Run(ctx, sudo+"systemctl restart fail2ban"); err != nil {
		return fmt.Errorf("restarting fail2ban: %w", err)
	}

	fmt.Fprintln(w, "  fail2ban active with SSH jail")
	return nil
}

// HardenSSH disables password authentication and enforces key-based auth.
// Skips if no authorized_keys are found (to avoid lockout).
// Idempotent — safe to run multiple times.
func HardenSSH(ctx context.Context, exec ssh.Executor, w io.Writer, sudo string) error {
	fmt.Fprintln(w, "Hardening SSH...")

	// Verify at least one authorized key exists before disabling password auth.
	// Check both current user and root authorized_keys.
	out, err := exec.Run(ctx, "cat ~/.ssh/authorized_keys 2>/dev/null")
	if err != nil || strings.TrimSpace(out) == "" {
		out, err = exec.Run(ctx, sudo+"cat /root/.ssh/authorized_keys 2>/dev/null")
	}
	if err != nil || strings.TrimSpace(out) == "" {
		fmt.Fprintln(w, "  Warning: no authorized_keys found, skipping SSH hardening to avoid lockout")
		return nil
	}

	if _, err := exec.Run(ctx, sudo+`sed -i 's/^#\?PermitRootLogin.*/PermitRootLogin prohibit-password/' /etc/ssh/sshd_config`); err != nil {
		return fmt.Errorf("setting PermitRootLogin: %w", err)
	}

	if _, err := exec.Run(ctx, sudo+`sed -i 's/^#\?PasswordAuthentication.*/PasswordAuthentication no/' /etc/ssh/sshd_config`); err != nil {
		return fmt.Errorf("setting PasswordAuthentication: %w", err)
	}

	if _, err := exec.Run(ctx, sudo+`sed -i 's/^#\?PubkeyAuthentication.*/PubkeyAuthentication yes/' /etc/ssh/sshd_config`); err != nil {
		return fmt.Errorf("setting PubkeyAuthentication: %w", err)
	}

	if _, err := exec.Run(ctx, sudo+"systemctl restart sshd"); err != nil {
		return fmt.Errorf("restarting sshd: %w", err)
	}

	fmt.Fprintln(w, "  PermitRootLogin prohibit-password")
	fmt.Fprintln(w, "  PasswordAuthentication no")
	fmt.Fprintln(w, "  PubkeyAuthentication yes")
	return nil
}

// EnableAutoUpdates installs and configures unattended-upgrades for automatic security patches.
// Idempotent — safe to run multiple times.
func EnableAutoUpdates(ctx context.Context, exec ssh.Executor, w io.Writer, sudo string) error {
	fmt.Fprintln(w, "Configuring auto-updates...")

	if _, err := exec.Run(ctx, "which unattended-upgrades"); err != nil {
		if _, err := exec.Run(ctx, sudo+"DEBIAN_FRONTEND=noninteractive apt-get update -qq && "+sudo+"DEBIAN_FRONTEND=noninteractive apt-get install -y -qq unattended-upgrades"); err != nil {
			return fmt.Errorf("installing unattended-upgrades: %w", err)
		}
	}

	// Enable automatic security updates only (no full dist-upgrades, no auto-reboot).
	cfg := `APT::Periodic::Update-Package-Lists "1";
APT::Periodic::Unattended-Upgrade "1";
`
	if err := exec.Upload(ctx, strings.NewReader(cfg), "/tmp/teploy-auto-upgrades.conf", "0644"); err != nil {
		return fmt.Errorf("writing auto-upgrades config: %w", err)
	}
	if _, err := exec.Run(ctx, sudo+"mv /tmp/teploy-auto-upgrades.conf /etc/apt/apt.conf.d/20auto-upgrades"); err != nil {
		return fmt.Errorf("installing auto-upgrades config: %w", err)
	}

	if _, err := exec.Run(ctx, sudo+"systemctl enable --now unattended-upgrades"); err != nil {
		return fmt.Errorf("enabling unattended-upgrades: %w", err)
	}

	fmt.Fprintln(w, "  Auto security updates enabled")
	return nil
}

// Harden runs all hardening steps in order: UFW, fail2ban, SSH.
// Auto-updates is intentionally excluded — it should run last (after all
// other apt installs complete) to avoid locking conflicts.
// Call EnableAutoUpdates separately at the end of setup.
// Detects sudo once and passes it to all sub-functions.
// Idempotent — safe to run multiple times.
func Harden(ctx context.Context, exec ssh.Executor, w io.Writer) error {
	sudo := detectSudo(ctx, exec)

	if err := ConfigureUFW(ctx, exec, w, sudo); err != nil {
		return err
	}
	if err := InstallFail2ban(ctx, exec, w, sudo); err != nil {
		return err
	}
	if err := HardenSSH(ctx, exec, w, sudo); err != nil {
		return err
	}
	return nil
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func splitFirstLine(s string) string {
	for i, c := range s {
		if c == '\n' {
			return s[:i]
		}
	}
	return s
}
