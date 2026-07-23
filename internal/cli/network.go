package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/network"
	"github.com/useteploy/teploy/internal/ssh"
)

func newNetworkCmd(flags *Flags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "network",
		Short: "Manage cross-server VPN networking",
	}

	cmd.AddCommand(newNetworkSetupCmd(flags))
	cmd.AddCommand(newNetworkStatusCmd(flags))
	cmd.AddCommand(newNetworkJoinCmd(flags))
	cmd.AddCommand(newNetworkGrantCmd(flags))
	cmd.AddCommand(newNetworkGrantsCmd(flags))
	cmd.AddCommand(newNetworkRevokeCmd(flags))

	return cmd
}

func newNetworkSetupCmd(flags *Flags) *cobra.Command {
	return &cobra.Command{
		Use:   "setup",
		Short: "Install VPN on all servers and join mesh",
		Long:  "Install the configured VPN provider on each server, join the mesh, and update DNS entries for cross-server communication.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runNetworkSetup(flags)
		},
	}
}

func newNetworkStatusCmd(flags *Flags) *cobra.Command {
	return &cobra.Command{
		Use:   "status [server]",
		Short: "Show mesh connectivity between servers",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				return runNetworkStatusServer(flags, args[0])
			}
			return runNetworkStatus(flags)
		},
	}
}

func newNetworkJoinCmd(flags *Flags) *cobra.Command {
	var (
		provider string
		authKey  string
	)

	cmd := &cobra.Command{
		Use:   "join [server]",
		Short: "Join a server to the VPN mesh",
		Long:  "Install the VPN provider on a server and join the mesh network.",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runNetworkJoin(flags, args, provider, authKey)
		},
	}

	cmd.Flags().StringVar(&provider, "provider", "", "VPN provider (tailscale, headscale, netbird)")
	cmd.Flags().StringVar(&authKey, "auth-key", "", "auth/setup key for the VPN provider")

	return cmd
}

func runNetworkJoin(flags *Flags, args []string, providerFlag, authKeyFlag string) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	// Determine which servers to join.
	var servers []serverInfo
	if len(args) == 1 {
		host, user, _, err := config.ResolveServer(args[0], flags.Host, flags.User, flags.Key)
		if err != nil {
			return fmt.Errorf("resolving server %q: %w", args[0], err)
		}
		servers = []serverInfo{{name: args[0], host: host, user: user}}
	} else {
		// Try teploy.yml first, then servers.yml.
		appCfg, appErr := config.LoadApp(".")
		if appErr == nil && appCfg.Network.Provider != "" {
			resolved, err := resolveAllServers(appCfg, flags)
			if err != nil {
				return err
			}
			servers = resolved
			// Use teploy.yml provider config as defaults.
			if providerFlag == "" {
				providerFlag = appCfg.Network.Provider
			}
			if authKeyFlag == "" {
				authKeyFlag = appCfg.Network.AuthKey
				if authKeyFlag == "" {
					authKeyFlag = appCfg.Network.SetupKey
				}
			}
		} else {
			// Fall back to servers.yml.
			serversPath, err := config.DefaultServersPath()
			if err != nil {
				return err
			}
			allServers, err := config.ListServers(serversPath)
			if err != nil {
				return fmt.Errorf("listing servers: %w", err)
			}
			if len(allServers) == 0 {
				return fmt.Errorf("no servers configured — add servers with 'teploy server add' or specify a server name")
			}
			for name, srv := range allServers {
				user := srv.User
				if user == "" {
					user = "root"
				}
				servers = append(servers, serverInfo{name: name, host: srv.Host, user: user})
			}
		}
	}

	// Resolve provider.
	if providerFlag == "" {
		return fmt.Errorf("no provider specified — use --provider or configure network.provider in teploy.yml")
	}

	// Resolve auth key: CLI flag > env var > error.
	cfg, err := resolveNetworkJoinConfig(providerFlag, authKeyFlag)
	if err != nil {
		return err
	}

	provider, err := network.NewProvider(cfg)
	if err != nil {
		return err
	}

	for _, s := range servers {
		fmt.Printf("Joining %s (%s) to %s mesh...\n", s.name, s.host, providerFlag)

		exec, err := ssh.Connect(ctx, ssh.ConnectConfig{
			Host:    s.host,
			User:    s.user,
			KeyPath: flags.Key,
		})
		if err != nil {
			return fmt.Errorf("connecting to %s: %w", s.name, err)
		}

		if err := provider.Install(ctx, exec, os.Stdout); err != nil {
			exec.Close()
			return fmt.Errorf("installing on %s: %w", s.name, err)
		}

		if err := provider.Join(ctx, exec); err != nil {
			exec.Close()
			return fmt.Errorf("joining mesh on %s: %w", s.name, err)
		}

		ip, err := provider.GetIP(ctx, exec)
		exec.Close()
		if err != nil {
			return fmt.Errorf("getting VPN IP on %s: %w", s.name, err)
		}
		fmt.Printf("  %s VPN IP: %s\n", s.name, ip)
	}

	fmt.Println("Network join complete")
	return nil
}

// resolveNetworkJoinConfig builds a network.Config from provider name and auth key,
// falling back to env vars if the auth key is not provided directly.
func resolveNetworkJoinConfig(providerName, authKey string) (network.Config, error) {
	switch providerName {
	case "tailscale":
		if authKey == "" {
			authKey = os.Getenv("TEPLOY_TS_AUTHKEY")
		}
		if authKey == "" {
			return network.Config{}, fmt.Errorf("auth key required — use --auth-key or set TEPLOY_TS_AUTHKEY")
		}
		return network.Config{Provider: "tailscale", AuthKey: authKey}, nil
	case "headscale":
		if authKey == "" {
			authKey = os.Getenv("TEPLOY_HEADSCALE_AUTHKEY")
		}
		if authKey == "" {
			return network.Config{}, fmt.Errorf("auth key required — use --auth-key or set TEPLOY_HEADSCALE_AUTHKEY")
		}
		server := os.Getenv("TEPLOY_HEADSCALE_SERVER")
		if server == "" {
			return network.Config{}, fmt.Errorf("TEPLOY_HEADSCALE_SERVER not set — set this env var with your Headscale server URL")
		}
		return network.Config{Provider: "headscale", AuthKey: authKey, Server: server}, nil
	case "netbird":
		if authKey == "" {
			authKey = os.Getenv("TEPLOY_NETBIRD_SETUP_KEY")
		}
		if authKey == "" {
			return network.Config{}, fmt.Errorf("setup key required — use --auth-key or set TEPLOY_NETBIRD_SETUP_KEY")
		}
		return network.Config{Provider: "netbird", SetupKey: authKey}, nil
	default:
		return network.Config{}, fmt.Errorf("unknown network provider: %q (supported: tailscale, headscale, netbird)", providerName)
	}
}

func runNetworkSetup(flags *Flags) error {
	appCfg, err := config.LoadApp(".")
	if err != nil {
		return err
	}

	if appCfg.Network.Provider == "" {
		return fmt.Errorf("no network provider configured — add a [network] block to teploy.yml")
	}

	provider, err := network.NewProvider(network.Config{
		Provider: appCfg.Network.Provider,
		AuthKey:  appCfg.Network.AuthKey,
		Server:   appCfg.Network.Server,
		SetupKey: appCfg.Network.SetupKey,
	})
	if err != nil {
		return err
	}

	servers, err := resolveAllServers(appCfg, flags)
	if err != nil {
		return err
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	// Phase 1: Install and join on each server, collect VPN IPs.
	vpnIPs := make(map[string]string) // server name -> VPN IP
	executors := make([]ssh.Executor, 0, len(servers))
	defer func() {
		for _, exec := range executors {
			exec.Close()
		}
	}()

	for _, s := range servers {
		fmt.Printf("Setting up %s (%s)...\n", s.name, s.host)

		exec, err := ssh.Connect(ctx, ssh.ConnectConfig{
			Host:    s.host,
			User:    s.user,
			KeyPath: flags.Key,
		})
		if err != nil {
			return fmt.Errorf("connecting to %s: %w", s.name, err)
		}
		executors = append(executors, exec)

		fmt.Printf("  Installing %s...\n", appCfg.Network.Provider)
		if err := provider.Install(ctx, exec, os.Stdout); err != nil {
			return fmt.Errorf("installing on %s: %w", s.name, err)
		}

		fmt.Printf("  Joining mesh...\n")
		if err := provider.Join(ctx, exec); err != nil {
			return fmt.Errorf("joining mesh on %s: %w", s.name, err)
		}

		ip, err := provider.GetIP(ctx, exec)
		if err != nil {
			return fmt.Errorf("getting VPN IP on %s: %w", s.name, err)
		}
		vpnIPs[s.name] = ip
		fmt.Printf("  VPN IP: %s\n", ip)
	}

	// Phase 2: Update DNS on all servers.
	if len(vpnIPs) > 0 {
		fmt.Println("Updating DNS entries...")
		for i, exec := range executors {
			s := servers[i]
			fmt.Printf("  Updating /etc/hosts on %s...\n", s.name)
			if err := network.UpdateDNS(ctx, exec, vpnIPs); err != nil {
				return fmt.Errorf("updating DNS on %s: %w", s.name, err)
			}
		}
	}

	fmt.Println("Network setup complete")
	return nil
}

func runNetworkStatus(flags *Flags) error {
	// Try servers.yml first, fall back to teploy.yml.
	var servers []serverInfo

	serversPath, pathErr := config.DefaultServersPath()
	if pathErr == nil {
		allServers, err := config.ListServers(serversPath)
		if err == nil && len(allServers) > 0 {
			for name, srv := range allServers {
				user := srv.User
				if user == "" {
					user = "root"
				}
				servers = append(servers, serverInfo{name: name, host: srv.Host, user: user})
			}
		}
	}

	// Fall back to teploy.yml if no servers found.
	if len(servers) == 0 {
		appCfg, err := config.LoadApp(".")
		if err != nil {
			return fmt.Errorf("no servers found in servers.yml and no teploy.yml: %w", err)
		}
		resolved, err := resolveAllServers(appCfg, flags)
		if err != nil {
			return err
		}
		servers = resolved
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	sort.Slice(servers, func(i, j int) bool { return servers[i].name < servers[j].name })
	results := make([]networkStatusDTO, 0, len(servers))
	for _, s := range servers {
		exec, err := ssh.Connect(ctx, ssh.ConnectConfig{
			Host:    s.host,
			User:    s.user,
			KeyPath: flags.Key,
		})
		if err != nil {
			results = append(results, networkStatusDTO{Server: s.name, Host: s.host, VPNIP: "-", Status: "unavailable", Error: err.Error(), ObservedAt: time.Now().UTC()})
			continue
		}

		ip, status := detectVPNStatus(ctx, exec)
		exec.Close()
		results = append(results, networkStatusDTO{Server: s.name, Host: s.host, VPNIP: ip, Status: status, ObservedAt: time.Now().UTC()})
	}
	return writeNetworkStatus(flags, results)
}

func runNetworkStatusServer(flags *Flags, serverName string) error {
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

	ip, status := detectVPNStatus(ctx, exec)
	return writeNetworkStatus(flags, []networkStatusDTO{{Server: serverName, Host: host, VPNIP: ip, Status: status, ObservedAt: time.Now().UTC()}})
}

type networkStatusDTO struct {
	Server     string    `json:"server"`
	Host       string    `json:"host"`
	VPNIP      string    `json:"vpn_ip"`
	Status     string    `json:"status"`
	Error      string    `json:"error"`
	ObservedAt time.Time `json:"observed_at"`
}

func writeNetworkStatus(flags *Flags, results []networkStatusDTO) error {
	if results == nil {
		results = []networkStatusDTO{}
	}
	if flags.JSON {
		return json.NewEncoder(os.Stdout).Encode(results)
	}
	fmt.Printf("%-20s  %-16s  %s\n", "SERVER", "VPN IP", "STATUS")
	for _, result := range results {
		status := result.Status
		if result.Error != "" {
			status = "connection failed: " + result.Error
		}
		fmt.Printf("%-20s  %-16s  %s\n", result.Server, result.VPNIP, status)
	}
	return nil
}

// detectVPNStatus checks which VPN is installed and returns (ip, status).
func detectVPNStatus(ctx context.Context, exec ssh.Executor) (string, string) {
	// Try tailscale first.
	if _, err := exec.Run(ctx, "which tailscale"); err == nil {
		ip := "-"
		if out, ipErr := exec.Run(ctx, "tailscale ip -4"); ipErr == nil {
			ip = strings.TrimSpace(out)
		}
		status := "installed"
		if out, sErr := exec.Run(ctx, "tailscale status"); sErr == nil {
			status = splitFirstLine(strings.TrimSpace(out))
		}
		return ip, status
	}

	// Try netbird.
	if _, err := exec.Run(ctx, "which netbird"); err == nil {
		ip := "-"
		status := "installed"
		if out, sErr := exec.Run(ctx, "netbird status"); sErr == nil {
			status = splitFirstLine(strings.TrimSpace(out))
			for _, line := range strings.Split(out, "\n") {
				trimmed := strings.TrimSpace(line)
				if strings.HasPrefix(trimmed, "NetBird IP:") || strings.HasPrefix(trimmed, "IP:") {
					parts := strings.Fields(trimmed)
					if len(parts) >= 2 {
						ip = parts[len(parts)-1]
						if idx := strings.Index(ip, "/"); idx != -1 {
							ip = ip[:idx]
						}
						break
					}
				}
			}
		}
		return ip, status
	}

	return "-", "no VPN installed"
}

// serverInfo holds resolved server connection details.
type serverInfo struct {
	name string
	host string
	user string
}

// resolveAllServers resolves the server list from app config.
// Uses the servers list if available, otherwise falls back to the single server.
func resolveAllServers(appCfg *config.AppConfig, flags *Flags) ([]serverInfo, error) {
	serverNames := appCfg.Servers
	if len(serverNames) == 0 && appCfg.Server != "" {
		serverNames = []string{appCfg.Server}
	}
	if len(serverNames) == 0 {
		return nil, fmt.Errorf("no servers configured — set 'server' or 'servers' in teploy.yml")
	}

	servers := make([]serverInfo, 0, len(serverNames))
	for _, name := range serverNames {
		host, user, _, err := config.ResolveServer(name, flags.Host, flags.User, flags.Key)
		if err != nil {
			return nil, fmt.Errorf("resolving server %q: %w", name, err)
		}
		servers = append(servers, serverInfo{name: name, host: host, user: user})
	}

	return servers, nil
}

// splitFirstLine returns the first line of a multi-line string.
func splitFirstLine(s string) string {
	for i, c := range s {
		if c == '\n' {
			return s[:i]
		}
	}
	return s
}
