package network

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/useteploy/teploy/internal/ssh"
)

// Provider defines the VPN mesh provider interface.
// All interaction with servers goes through ssh.Executor.
type Provider interface {
	// Install installs the VPN client on the server. No-op if already installed.
	// If w is non-nil, install output is streamed to it for long-running installs.
	Install(ctx context.Context, exec ssh.Executor, w io.Writer) error

	// Join connects the server to the VPN mesh. No-op if already connected.
	Join(ctx context.Context, exec ssh.Executor) error

	// Status returns the VPN status output from the server.
	Status(ctx context.Context, exec ssh.Executor) (string, error)

	// GetIP returns the server's stable private VPN IP address.
	GetIP(ctx context.Context, exec ssh.Executor) (string, error)
}

// Config holds network configuration.
type Config struct {
	Provider string
	AuthKey  string
	Server   string
	SetupKey string
	Sudo     string // "sudo " or "" — set by caller based on user context
}

// NewProvider creates a Provider from the given config.
func NewProvider(cfg Config) (Provider, error) {
	switch cfg.Provider {
	case "tailscale":
		return &TailscaleProvider{AuthKey: cfg.AuthKey, Sudo: cfg.Sudo}, nil
	case "headscale":
		return &HeadscaleProvider{Server: cfg.Server, AuthKey: cfg.AuthKey, Sudo: cfg.Sudo}, nil
	case "netbird":
		return &NetbirdProvider{SetupKey: cfg.SetupKey, Sudo: cfg.Sudo}, nil
	default:
		return nil, fmt.Errorf("unknown network provider: %q (supported: tailscale, headscale, netbird)", cfg.Provider)
	}
}

// --- Tailscale ---

// TailscaleProvider manages Tailscale VPN on servers.
type TailscaleProvider struct {
	AuthKey string
	Sudo    string
}

func (t *TailscaleProvider) Install(ctx context.Context, exec ssh.Executor, w io.Writer) error {
	if _, err := exec.Run(ctx, "which tailscale"); err == nil {
		return nil // already installed
	}
	installCmd := t.Sudo + "sh -c 'curl -fsSL https://tailscale.com/install.sh | sh'"
	out, err := exec.Run(ctx, installCmd)
	if err != nil {
		if w != nil {
			fmt.Fprintln(w, out)
		}
		return fmt.Errorf("installing tailscale: %w", err)
	}
	return nil
}

func (t *TailscaleProvider) Join(ctx context.Context, exec ssh.Executor) error {
	out, err := exec.Run(ctx, t.Sudo+"tailscale status --json 2>/dev/null")
	if err == nil && strings.Contains(out, `"BackendState":"Running"`) {
		return nil // already connected
	}
	// Run tailscale up in the background. Tailscale modifies iptables which can kill
	// the SSH connection that's running the command. Using nohup + background avoids this.
	cmd := t.Sudo + "nohup tailscale up --authkey=" + ssh.ShellQuote(t.AuthKey) + " --accept-routes >/dev/null 2>&1 & sleep 3 && " + t.Sudo + "tailscale status --json 2>/dev/null | grep -q Running"
	if _, err := exec.Run(ctx, cmd); err != nil {
		// SSH disconnect during tailscale up is expected — the iptables change can kill the connection.
		// Check if tailscale actually joined by trying status again.
		out, statusErr := exec.Run(ctx, t.Sudo+"tailscale status --json 2>/dev/null")
		if statusErr == nil && strings.Contains(out, `"BackendState":"Running"`) {
			return nil // joined successfully despite SSH hiccup
		}
		return fmt.Errorf("joining tailscale mesh: %w", err)
	}
	return nil
}

func (t *TailscaleProvider) Status(ctx context.Context, exec ssh.Executor) (string, error) {
	out, err := exec.Run(ctx, t.Sudo+"tailscale status")
	if err != nil {
		return "", fmt.Errorf("getting tailscale status: %w", err)
	}
	return out, nil
}

func (t *TailscaleProvider) GetIP(ctx context.Context, exec ssh.Executor) (string, error) {
	out, err := exec.Run(ctx, t.Sudo+"tailscale ip -4")
	if err != nil {
		return "", fmt.Errorf("getting tailscale IP: %w", err)
	}
	return strings.TrimSpace(out), nil
}

// --- Headscale ---

// HeadscaleProvider manages Headscale (self-hosted Tailscale) VPN on servers.
// Headscale uses the standard tailscale client pointed at a custom coordination server.
type HeadscaleProvider struct {
	Server  string
	AuthKey string
	Sudo    string
}

func (h *HeadscaleProvider) Install(ctx context.Context, exec ssh.Executor, w io.Writer) error {
	if _, err := exec.Run(ctx, "which tailscale"); err == nil {
		return nil
	}
	installCmd := h.Sudo + "sh -c 'curl -fsSL https://tailscale.com/install.sh | sh'"
	out, err := exec.Run(ctx, installCmd)
	if err != nil {
		if w != nil {
			fmt.Fprintln(w, out)
		}
		return fmt.Errorf("installing tailscale client for headscale: %w", err)
	}
	return nil
}

func (h *HeadscaleProvider) Join(ctx context.Context, exec ssh.Executor) error {
	out, err := exec.Run(ctx, h.Sudo+"tailscale status --json 2>/dev/null")
	if err == nil && strings.Contains(out, `"BackendState":"Running"`) {
		return nil
	}
	cmd := h.Sudo + "nohup tailscale up --login-server=" + ssh.ShellQuote(h.Server) + " --authkey=" + ssh.ShellQuote(h.AuthKey) + " --accept-routes >/dev/null 2>&1 & sleep 3 && " + h.Sudo + "tailscale status --json 2>/dev/null | grep -q Running"
	if _, err := exec.Run(ctx, cmd); err != nil {
		out, statusErr := exec.Run(ctx, h.Sudo+"tailscale status --json 2>/dev/null")
		if statusErr == nil && strings.Contains(out, `"BackendState":"Running"`) {
			return nil
		}
		return fmt.Errorf("joining headscale mesh: %w", err)
	}
	return nil
}

func (h *HeadscaleProvider) Status(ctx context.Context, exec ssh.Executor) (string, error) {
	out, err := exec.Run(ctx, h.Sudo+"tailscale status")
	if err != nil {
		return "", fmt.Errorf("getting headscale status: %w", err)
	}
	return out, nil
}

func (h *HeadscaleProvider) GetIP(ctx context.Context, exec ssh.Executor) (string, error) {
	out, err := exec.Run(ctx, h.Sudo+"tailscale ip -4")
	if err != nil {
		return "", fmt.Errorf("getting headscale IP: %w", err)
	}
	return strings.TrimSpace(out), nil
}

// --- Netbird ---

// NetbirdProvider manages Netbird VPN on servers.
type NetbirdProvider struct {
	SetupKey string
	Sudo     string
}

func (n *NetbirdProvider) Install(ctx context.Context, exec ssh.Executor, w io.Writer) error {
	if _, err := exec.Run(ctx, "which netbird"); err == nil {
		return nil
	}
	installCmd := n.Sudo + "sh -c 'curl -fsSL https://pkgs.netbird.io/install.sh | sh'"
	out, err := exec.Run(ctx, installCmd)
	if err != nil {
		if w != nil {
			fmt.Fprintln(w, out)
		}
		return fmt.Errorf("installing netbird: %w", err)
	}
	return nil
}

func (n *NetbirdProvider) Join(ctx context.Context, exec ssh.Executor) error {
	out, err := exec.Run(ctx, n.Sudo+"netbird status 2>/dev/null")
	if err == nil && strings.Contains(out, "Connected") {
		return nil
	}
	cmd := n.Sudo + "netbird up --setup-key " + ssh.ShellQuote(n.SetupKey)
	if _, err := exec.Run(ctx, cmd); err != nil {
		return fmt.Errorf("joining netbird mesh: %w", err)
	}
	return nil
}

func (n *NetbirdProvider) Status(ctx context.Context, exec ssh.Executor) (string, error) {
	out, err := exec.Run(ctx, n.Sudo+"netbird status")
	if err != nil {
		return "", fmt.Errorf("getting netbird status: %w", err)
	}
	return out, nil
}

func (n *NetbirdProvider) GetIP(ctx context.Context, exec ssh.Executor) (string, error) {
	out, err := exec.Run(ctx, n.Sudo+"netbird status")
	if err != nil {
		return "", fmt.Errorf("getting netbird status for IP: %w", err)
	}
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "NetBird IP:") || strings.HasPrefix(trimmed, "IP:") {
			parts := strings.Fields(trimmed)
			if len(parts) >= 2 {
				ip := parts[len(parts)-1]
				if idx := strings.Index(ip, "/"); idx != -1 {
					ip = ip[:idx]
				}
				return ip, nil
			}
		}
	}
	return "", fmt.Errorf("could not determine netbird IP from status output")
}
