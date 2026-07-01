package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"

	"github.com/spf13/cobra"
	"github.com/useteploy/teploy/internal/build"
	"github.com/useteploy/teploy/internal/caddy"
	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/dns"
	"github.com/useteploy/teploy/internal/ssh"
)

type validationResult struct {
	Valid  bool     `json:"valid"`
	Errors []string `json:"errors,omitempty"`
	Warns  []string `json:"warnings,omitempty"`
}

func newValidateCmd(flags *Flags) *cobra.Command {
	return &cobra.Command{
		Use:   "validate [server-name]",
		Short: "Check config and server readiness",
		Long:  "Validate teploy.yml, check server connectivity, Docker, build prerequisites, and DNS.\nIf a server name is given, report detailed server security and service status.",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				return runValidateServer(flags, args[0])
			}
			return runValidate(flags)
		},
	}
}

func runValidate(flags *Flags) error {
	result := &validationResult{Valid: true}

	// 1. Load config.
	appCfg, err := config.LoadApp(".")
	if err != nil {
		result.Valid = false
		result.Errors = append(result.Errors, fmt.Sprintf("config: %v", err))
		return outputResult(flags, result)
	}

	// 2. Check build prerequisites.
	if appCfg.Image == "" {
		mode := build.Detect(".")
		switch mode {
		case build.ModeDockerfile:
			// Dockerfile exists — good.
		case build.ModeNixpacks:
			result.Warns = append(result.Warns, "No Dockerfile found — Nixpacks will be used (requires Nixpacks on server or locally)")
		}
	}

	// 2b. Flag non-public domains (bare IPs, LAN addresses). dns.Validate
	// below can't catch this — net.LookupHost short-circuits a literal IP
	// straight back out, so a domain that's already an IP always "passes"
	// DNS validation even though Caddy will serve it over plain HTTP (or
	// self-signed HTTPS with tls.internal), never automatic HTTPS. Report
	// it here explicitly so that's a documented, expected outcome instead
	// of a surprise discovered mid-deploy.
	result.Warns = append(result.Warns, nonPublicDomainWarnings(appCfg)...)

	// 3. Check server connectivity.
	if appCfg.Server == "" && flags.Host == "" {
		result.Valid = false
		result.Errors = append(result.Errors, "no server configured (set 'server' in teploy.yml or use --host)")
	} else {
		host, user, key, err := config.ResolveServer(appCfg.Server, flags.Host, flags.User, flags.Key)
		if err != nil {
			result.Valid = false
			result.Errors = append(result.Errors, fmt.Sprintf("server resolution: %v", err))
		} else {
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
			defer cancel()

			executor, err := ssh.Connect(ctx, ssh.ConnectConfig{
				Host:    host,
				User:    user,
				KeyPath: key,
			})
			if err != nil {
				result.Valid = false
				result.Errors = append(result.Errors, fmt.Sprintf("SSH connection to %s@%s: %v", user, host, err))
			} else {
				defer executor.Close()

				// Check Docker.
				if _, err := executor.Run(ctx, "docker --version"); err != nil {
					result.Valid = false
					result.Errors = append(result.Errors, "Docker is not installed or not running on server")
				}

				// Check DNS.
				if appCfg.Domain != "" {
					if err := dns.Validate(appCfg.Domain, host, nil); err != nil {
						result.Warns = append(result.Warns, fmt.Sprintf("DNS: %v", err))
					}
				}
			}
		}
	}

	return outputResult(flags, result)
}

// nonPublicDomainWarnings reports each host in appCfg.Domain that
// caddy.IsPubliclyRoutable can't vouch for (bare IPs, LAN addresses),
// worded according to how TLS is actually going to be handled for it —
// plain HTTP by default, self-signed HTTPS with tls.internal, or a custom
// certificate. Extracted from runValidate for direct unit testing.
func nonPublicDomainWarnings(appCfg *config.AppConfig) []string {
	var warns []string
	for _, host := range strings.Split(appCfg.Domain, ",") {
		host = strings.TrimSpace(host)
		if host == "" || caddy.IsPubliclyRoutable(host) {
			continue
		}
		switch {
		case appCfg.TLS != nil && appCfg.TLS.Internal:
			warns = append(warns, fmt.Sprintf(
				"domain %q is not a public hostname — Caddy will serve it over self-signed HTTPS (tls.internal), not automatic HTTPS", host))
		case appCfg.TLS != nil:
			warns = append(warns, fmt.Sprintf(
				"domain %q is not a public hostname — served via the configured custom certificate, not automatic HTTPS", host))
		default:
			warns = append(warns, fmt.Sprintf(
				"domain %q is not a public hostname — will serve over plain HTTP (no TLS); add 'tls: {internal: true}' for self-signed HTTPS instead", host))
		}
	}
	return warns
}

// runValidateServer connects to a named server and reports detailed status.
func runValidateServer(flags *Flags, serverName string) error {
	host, user, key, err := config.ResolveServer(serverName, flags.Host, flags.User, flags.Key)
	if err != nil {
		return fmt.Errorf("resolving server %q: %w", serverName, err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	exec, err := ssh.Connect(ctx, ssh.ConnectConfig{
		Host:    host,
		User:    user,
		KeyPath: key,
	})
	if err != nil {
		return fmt.Errorf("connecting to %s: %w", serverName, err)
	}
	defer exec.Close()

	validateServerStatus(ctx, exec, os.Stdout)
	return nil
}

// validateServerStatus checks all services and security settings on a connected server.
// Separated from runValidateServer for testability with MockExecutor.
func validateServerStatus(ctx context.Context, exec ssh.Executor, w io.Writer) {
	// Docker
	if out, err := exec.Run(ctx, "docker --version"); err == nil {
		ver := strings.TrimPrefix(strings.TrimSpace(out), "Docker version ")
		if idx := strings.Index(ver, ","); idx != -1 {
			ver = ver[:idx]
		}
		fmt.Fprintf(w, "%-16sInstalled (%s)\n", "Docker", ver)
	} else {
		fmt.Fprintf(w, "%-16sNot installed\n", "Docker")
	}

	// Caddy
	if out, err := exec.Run(ctx, "docker ps --filter name=^caddy$ --format '{{.Status}}'"); err == nil && strings.TrimSpace(out) != "" {
		fmt.Fprintf(w, "%-16sRunning\n", "Caddy")
	} else {
		fmt.Fprintf(w, "%-16sNot running\n", "Caddy")
	}

	// UFW
	if out, err := exec.Run(ctx, "ufw status"); err == nil {
		if strings.Contains(out, "Status: active") {
			if strings.Contains(out, "deny") {
				fmt.Fprintf(w, "%-16sActive, default deny\n", "UFW")
			} else {
				fmt.Fprintf(w, "%-16sActive\n", "UFW")
			}
		} else {
			fmt.Fprintf(w, "%-16sInactive\n", "UFW")
		}
	} else {
		fmt.Fprintf(w, "%-16sNot installed\n", "UFW")
	}

	// SSH Key Auth
	if out, err := exec.Run(ctx, "grep -E '^PubkeyAuthentication' /etc/ssh/sshd_config"); err == nil {
		if strings.Contains(out, "yes") {
			fmt.Fprintf(w, "%-16sEnabled\n", "SSH Key Auth")
		} else {
			fmt.Fprintf(w, "%-16sDisabled\n", "SSH Key Auth")
		}
	} else {
		fmt.Fprintf(w, "%-16sUnknown\n", "SSH Key Auth")
	}

	// Password Auth
	if out, err := exec.Run(ctx, "grep -E '^PasswordAuthentication' /etc/ssh/sshd_config"); err == nil {
		if strings.Contains(out, "no") {
			fmt.Fprintf(w, "%-16sDisabled\n", "Password Auth")
		} else {
			fmt.Fprintf(w, "%-16sEnabled\n", "Password Auth")
		}
	} else {
		fmt.Fprintf(w, "%-16sUnknown\n", "Password Auth")
	}

	// Fail2ban
	if out, err := exec.Run(ctx, "systemctl is-active fail2ban && fail2ban-client status sshd"); err == nil {
		if strings.Contains(out, "active") {
			fmt.Fprintf(w, "%-16sActive, SSH protected\n", "Fail2ban")
		} else {
			fmt.Fprintf(w, "%-16s%s\n", "Fail2ban", splitFirstLine(strings.TrimSpace(out)))
		}
	} else {
		fmt.Fprintf(w, "%-16sNot installed\n", "Fail2ban")
	}

	// VPN — auto-detect
	vpnPrinted := false
	if out, err := exec.Run(ctx, "tailscale status"); err == nil {
		_ = out
		if ip, ipErr := exec.Run(ctx, "tailscale ip -4"); ipErr == nil {
			fmt.Fprintf(w, "%-16sConnected (%s)\n", "Tailscale", strings.TrimSpace(ip))
		} else {
			fmt.Fprintf(w, "%-16sInstalled\n", "Tailscale")
		}
		vpnPrinted = true
	}
	if !vpnPrinted {
		if out, err := exec.Run(ctx, "netbird status"); err == nil {
			if strings.Contains(out, "Connected") {
				for _, line := range strings.Split(out, "\n") {
					trimmed := strings.TrimSpace(line)
					if strings.HasPrefix(trimmed, "NetBird IP:") || strings.HasPrefix(trimmed, "IP:") {
						parts := strings.Fields(trimmed)
						if len(parts) >= 2 {
							ip := parts[len(parts)-1]
							if idx := strings.Index(ip, "/"); idx != -1 {
								ip = ip[:idx]
							}
							fmt.Fprintf(w, "%-16sConnected (%s)\n", "Netbird", ip)
							vpnPrinted = true
							break
						}
					}
				}
				if !vpnPrinted {
					fmt.Fprintf(w, "%-16sConnected\n", "Netbird")
					vpnPrinted = true
				}
			} else {
				fmt.Fprintf(w, "%-16s%s\n", "Netbird", splitFirstLine(strings.TrimSpace(out)))
				vpnPrinted = true
			}
		}
	}
	if !vpnPrinted {
		fmt.Fprintf(w, "%-16sNot installed\n", "VPN")
	}

	// Disk
	if out, err := exec.Run(ctx, "df -h / --output=size,used,avail,pcent | tail -1"); err == nil {
		fields := strings.Fields(strings.TrimSpace(out))
		if len(fields) >= 4 {
			fmt.Fprintf(w, "%-16s%s (%s used)\n", "Disk", fields[0], fields[3])
		}
	}

	// Memory
	if out, err := exec.Run(ctx, "free -h | grep Mem | awk '{print $2, $3}'"); err == nil {
		fields := strings.Fields(strings.TrimSpace(out))
		if len(fields) >= 2 {
			fmt.Fprintf(w, "%-16s%s total, %s used\n", "Memory", fields[0], fields[1])
		}
	}
}

func outputResult(flags *Flags, result *validationResult) error {
	if flags.JSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(result)
	}

	if result.Valid {
		fmt.Println("Config is valid")
	} else {
		fmt.Println("Validation failed")
	}

	for _, e := range result.Errors {
		fmt.Printf("  ERROR: %s\n", e)
	}
	for _, w := range result.Warns {
		fmt.Printf("  WARN: %s\n", w)
	}

	if !result.Valid {
		return fmt.Errorf("validation failed with %d error(s)", len(result.Errors))
	}
	return nil
}
