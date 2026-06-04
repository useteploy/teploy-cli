package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	osexec "os/exec"
	"os/signal"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/harden"
	"github.com/useteploy/teploy/internal/network"
	"github.com/useteploy/teploy/internal/ssh"
	"golang.org/x/term"
)

func newSetupCmd(flags *Flags) *cobra.Command {
	var (
		name       string
		noHarden   bool
		networkPro string
		authKey    string
		password   bool
		yes        bool
	)

	cmd := &cobra.Command{
		Use:   "setup <host>",
		Short: "Provision a server for teploy",
		Long: `Install Docker, configure firewall, start Caddy, harden security, and prepare a server for deployments.

Examples:
  teploy setup 192.168.1.10 --name web1
  teploy setup 192.168.1.10 --name web1 --user tyler --password
  teploy setup 192.168.1.10 --name web1 --password --network tailscale --auth-key tskey-auth-...
  teploy setup 192.168.1.10 --name web1 --no-harden`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSetup(flags, args[0], name, noHarden, networkPro, authKey, password, yes)
		},
	}

	cmd.Flags().StringVar(&name, "name", "", "server name for servers.yml (default: host address)")
	cmd.Flags().BoolVar(&password, "password", false, "authenticate with password (prompts for input, installs SSH key)")
	cmd.Flags().BoolVar(&noHarden, "no-harden", false, "skip security hardening")
	cmd.Flags().StringVar(&networkPro, "network", "", "VPN provider (tailscale, headscale, netbird)")
	cmd.Flags().StringVar(&authKey, "auth-key", "", "auth/setup key for VPN provider (falls back to env var)")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip confirmation prompts (required for non-interactive upgrades that recreate Caddy)")

	return cmd
}

func runSetup(flags *Flags, host string, name string, noHarden bool, networkProvider string, authKey string, usePassword bool, yes bool) error {
	user := flags.User
	if user == "" {
		user = "root"
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	cfg := ssh.ConnectConfig{
		Host:          host,
		User:          user,
		KeyPath:       flags.Key,
		AcceptNewHost: true,
	}

	if usePassword {
		fmt.Printf("Password for %s@%s: ", user, host)
		passBytes, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
		if err != nil {
			return fmt.Errorf("reading password: %w", err)
		}
		cfg.Password = string(passBytes)
	}

	fmt.Printf("Connecting to %s...\n", host)

	executor, err := ssh.Connect(ctx, cfg)
	if err != nil {
		return err
	}
	defer executor.Close()

	// If password auth was used, inject the local SSH public key for future key-based auth.
	if usePassword {
		pubKeyPath, err := ssh.PublicKeyPath(flags.Key)
		if err != nil {
			return fmt.Errorf("finding SSH public key: %w", err)
		}
		pubKeyData, err := os.ReadFile(pubKeyPath)
		if err != nil {
			return fmt.Errorf("reading SSH public key: %w", err)
		}
		pubKey := strings.TrimSpace(string(pubKeyData))
		installCmd := fmt.Sprintf(
			"mkdir -p ~/.ssh && echo %s >> ~/.ssh/authorized_keys && chmod 700 ~/.ssh && chmod 600 ~/.ssh/authorized_keys",
			shellQuote(pubKey),
		)
		if _, err := executor.Run(ctx, installCmd); err != nil {
			return fmt.Errorf("installing SSH key: %w", err)
		}
		fmt.Println("SSH key installed")
	}

	// Check if we have root or passwordless sudo access — required for non-interactive setup.
	whoami, _ := executor.Run(ctx, "whoami")
	isRoot := strings.TrimSpace(whoami) == "root"
	hasSudo := false
	if !isRoot {
		// Only count sudo as available if it works without a password prompt.
		_, err := executor.Run(ctx, "sudo -n true 2>/dev/null")
		hasSudo = err == nil
	}

	if !isRoot && !hasSudo {
		fmt.Println("No sudo detected — root password needed to install it.")
		fmt.Print("Root password: ")
		rootPassBytes, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
		if err != nil {
			return fmt.Errorf("reading root password: %w", err)
		}
		rootPass := string(rootPassBytes)

		// Try connecting as root via SSH first (fastest path).
		rootCfg := ssh.ConnectConfig{
			Host:          host,
			User:          "root",
			KeyPath:       flags.Key,
			Password:      rootPass,
			AcceptNewHost: true,
		}
		fmt.Println("  Installing sudo...")
		rootExec, rootErr := ssh.Connect(ctx, rootCfg)
		if rootErr == nil {
			// Root SSH works — install sudo directly.
			if _, err := rootExec.Run(ctx, "DEBIAN_FRONTEND=noninteractive apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq sudo && usermod -aG sudo "+user); err != nil {
				rootExec.Close()
				return fmt.Errorf("installing sudo: %w", err)
			}
			nopasswdCmd := fmt.Sprintf("echo '%s ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/%s && chmod 440 /etc/sudoers.d/%s", user, user, user)
			if _, err := rootExec.Run(ctx, nopasswdCmd); err != nil {
				rootExec.Close()
				return fmt.Errorf("configuring sudoers: %w", err)
			}
			rootExec.Close()
		} else {
			// Root SSH denied — use expect-style su via the existing tyler connection.
			// Write a helper script that uses su with the password from a file.
			script := fmt.Sprintf(`#!/bin/bash
exec 2>&1
echo '%s' | su -c 'DEBIAN_FRONTEND=noninteractive apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq sudo >/dev/null 2>&1 && usermod -aG sudo %s && echo "%s ALL=(ALL) NOPASSWD:ALL" > /etc/sudoers.d/%s && chmod 440 /etc/sudoers.d/%s && echo TEPLOY_SUDO_OK' - root 2>&1
`, strings.ReplaceAll(rootPass, "'", "'\"'\"'"), user, user, user, user)
			if err := executor.Upload(ctx, strings.NewReader(script), "/tmp/teploy_install_sudo.sh", "0700"); err != nil {
				return fmt.Errorf("uploading sudo installer: %w", err)
			}
			out, err := executor.Run(ctx, "/tmp/teploy_install_sudo.sh")
			executor.Run(ctx, "rm -f /tmp/teploy_install_sudo.sh")
			if err != nil || !strings.Contains(out, "TEPLOY_SUDO_OK") {
				if strings.Contains(out, "Authentication failure") {
					return fmt.Errorf("wrong root password")
				}
				return fmt.Errorf("installing sudo via su failed: %s", out)
			}
		}
		fmt.Printf("  sudo installed, %s added to sudo group\n", user)
	}

	if err := setupServer(ctx, executor, os.Stdout, yes); err != nil {
		return err
	}

	// Hardening (on by default, skip with --no-harden).
	if !noHarden {
		if err := harden.Harden(ctx, executor, os.Stdout); err != nil {
			return err
		}
	}

	// VPN network integration (opt-in via --network).
	var vpnIP string
	if networkProvider != "" {
		vpnIP, err = setupNetwork(ctx, executor, os.Stdout, networkProvider, authKey)
		if err != nil {
			return err
		}
	}

	// If VPN was set up, reconnect via VPN IP — LAN may be blocked by Tailscale iptables.
	serverHost := host
	if vpnIP != "" {
		serverHost = vpnIP
		fmt.Printf("\nVPN connected — reconnecting via %s\n", vpnIP)
		executor.Close()
		reconnectCfg := ssh.ConnectConfig{
			Host:          vpnIP,
			User:          user,
			KeyPath:       flags.Key,
			AcceptNewHost: true,
		}
		executor, err = ssh.Connect(ctx, reconnectCfg)
		if err != nil {
			return fmt.Errorf("reconnecting via VPN IP %s: %w", vpnIP, err)
		}
		defer executor.Close()
	}

	// Enable auto security updates last — after all apt installs are done.
	if !noHarden {
		sudo := ""
		if w, _ := executor.Run(ctx, "whoami"); strings.TrimSpace(w) != "root" {
			sudo = "sudo "
		}
		if err := harden.EnableAutoUpdates(ctx, executor, os.Stdout, sudo); err != nil {
			return err
		}
	}

	if name == "" {
		name = host
	}
	serversPath, err := config.DefaultServersPath()
	if err != nil {
		return err
	}
	if err := config.AddServer(serversPath, name, serverHost, user, "", vpnIP); err != nil {
		return err
	}

	fmt.Printf("\nServer %q (%s) ready for deploys\n", name, serverHost)
	return nil
}

// setupNetwork installs the VPN provider, joins the mesh, and returns the VPN IP.
func setupNetwork(ctx context.Context, exec ssh.Executor, w io.Writer, providerName string, authKeyFlag string) (string, error) {
	cfg, err := resolveNetworkConfig(providerName, authKeyFlag)
	if err != nil {
		return "", err
	}

	// Detect sudo for network commands.
	sudo := ""
	if whoami, _ := exec.Run(ctx, "whoami"); strings.TrimSpace(whoami) != "root" {
		sudo = "sudo "
		cfg.Sudo = sudo
	}

	provider, err := network.NewProvider(cfg)
	if err != nil {
		return "", err
	}

	fmt.Fprintf(w, "Installing %s...\n", providerName)
	if err := provider.Install(ctx, exec, w); err != nil {
		return "", fmt.Errorf("installing %s: %w", providerName, err)
	}

	// Get the server's hostname before joining — we'll use it to find the node locally.
	hostname, _ := exec.Run(ctx, "hostname")
	hostname = strings.TrimSpace(hostname)

	// Reset Tailscale state if present — cloned VMs inherit the previous machine's identity
	// which causes IP conflicts. Stop tailscaled, wipe state, restart.
	if providerName == "tailscale" || providerName == "headscale" {
		exec.Run(ctx, sudo+"systemctl stop tailscaled 2>/dev/null; "+sudo+"rm -rf /var/lib/tailscale; "+sudo+"systemctl start tailscaled 2>/dev/null; true")
	}

	// Preserve LAN access: detect the SSH connection's subnet and whitelist it
	// in iptables before Tailscale modifies the firewall rules.
	// Without this, tailscale up blocks all LAN traffic including our SSH session.
	sshClientIP, _ := exec.Run(ctx, "echo $SSH_CLIENT | awk '{print $1}'")
	sshClientIP = strings.TrimSpace(sshClientIP)
	if sshClientIP != "" && !strings.Contains(sshClientIP, ":") { // IPv4 only
		// Extract /24 subnet from the client IP
		parts := strings.Split(sshClientIP, ".")
		if len(parts) == 4 {
			subnet := parts[0] + "." + parts[1] + "." + parts[2] + ".0/24"
			exec.Run(ctx, sudo+"iptables -C ts-input -s "+subnet+" -j ACCEPT 2>/dev/null || "+sudo+"iptables -I ts-input 1 -s "+subnet+" -j ACCEPT 2>/dev/null; true")
		}
	}

	// Fire VPN join in the background and don't wait for it.
	// Tailscale/Headscale modifies iptables which can kill the SSH connection,
	// so we detach the command and poll from the local machine instead.
	fmt.Fprintf(w, "Joining %s mesh...\n", providerName)
	var joinCmd string
	// Single-quote user-provided mesh credentials (auth keys, login server) so a
	// value with a shell metacharacter can't break out of the join command.
	switch providerName {
	case "tailscale":
		joinCmd = fmt.Sprintf(sudo+"nohup tailscale up --authkey=%s --accept-routes >/dev/null 2>&1 &", shellQuote(cfg.AuthKey))
	case "headscale":
		joinCmd = fmt.Sprintf(sudo+"nohup tailscale up --login-server=%s --authkey=%s --accept-routes >/dev/null 2>&1 &", shellQuote(cfg.Server), shellQuote(cfg.AuthKey))
	case "netbird":
		joinCmd = fmt.Sprintf(sudo+"nohup netbird up --setup-key %s >/dev/null 2>&1 &", shellQuote(cfg.SetupKey))
	}
	exec.Run(ctx, joinCmd) // ignore error — connection may die

	// Poll locally for the node to appear on our tailnet.
	fmt.Fprintf(w, "  Waiting for node to appear on tailnet...\n")
	tsBinary := findTailscaleBinary()
	var vpnIP string
	for i := 0; i < 30; i++ { // 30 attempts, 2 seconds each = 60 second timeout
		time.Sleep(2 * time.Second)
		out, err := runLocal(tsBinary, "status")
		if err != nil {
			continue
		}
		for _, line := range strings.Split(out, "\n") {
			fields := strings.Fields(line)
			if len(fields) >= 2 && strings.EqualFold(fields[1], hostname) {
				vpnIP = fields[0]
				break
			}
		}
		if vpnIP != "" {
			break
		}
	}

	if vpnIP == "" {
		return "", fmt.Errorf("timed out waiting for %s to join tailnet (expected hostname: %s)", providerName, hostname)
	}

	fmt.Fprintf(w, "  VPN IP: %s\n", vpnIP)
	return vpnIP, nil
}

// runLocal executes a command on the local machine and returns its output.
func runLocal(name string, args ...string) (string, error) {
	cmd := osexec.Command(name, args...)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// findTailscaleBinary returns the path to the tailscale CLI binary.
// Checks PATH first, then common macOS/Linux locations.
func findTailscaleBinary() string {
	if path, err := osexec.LookPath("tailscale"); err == nil {
		return path
	}
	// macOS app bundle
	if _, err := os.Stat("/Applications/Tailscale.app/Contents/MacOS/Tailscale"); err == nil {
		return "/Applications/Tailscale.app/Contents/MacOS/Tailscale"
	}
	// Linux common paths
	for _, p := range []string{"/usr/bin/tailscale", "/usr/local/bin/tailscale"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return "tailscale" // fallback, hope it's in PATH
}

// resolveNetworkConfig resolves auth keys from --auth-key flag, falling back to environment variables.
func resolveNetworkConfig(providerName string, authKeyFlag string) (network.Config, error) {
	switch providerName {
	case "tailscale":
		authKey := authKeyFlag
		if authKey == "" {
			authKey = os.Getenv("TEPLOY_TS_AUTHKEY")
		}
		if authKey == "" {
			return network.Config{}, fmt.Errorf("auth key required — use --auth-key or set TEPLOY_TS_AUTHKEY")
		}
		return network.Config{Provider: "tailscale", AuthKey: authKey}, nil
	case "headscale":
		authKey := authKeyFlag
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
		setupKey := authKeyFlag
		if setupKey == "" {
			setupKey = os.Getenv("TEPLOY_NETBIRD_SETUP_KEY")
		}
		if setupKey == "" {
			return network.Config{}, fmt.Errorf("setup key required — use --auth-key or set TEPLOY_NETBIRD_SETUP_KEY")
		}
		return network.Config{Provider: "netbird", SetupKey: setupKey}, nil
	default:
		return network.Config{}, fmt.Errorf("unknown network provider: %q (supported: tailscale, headscale, netbird)", providerName)
	}
}

// setupServer runs the provisioning steps on a connected server.
// Separated from runSetup for testability with MockExecutor.
// yes skips interactive confirmation for destructive upgrade steps (Caddy recreate).
func setupServer(ctx context.Context, exec ssh.Executor, w io.Writer, yes bool) error {
	// Detect whether we need sudo (non-root users).
	sudo := ""
	if whoami, _ := exec.Run(ctx, "whoami"); strings.TrimSpace(whoami) != "root" {
		sudo = "sudo "
	}

	// 1. Check/install Docker
	fmt.Fprintln(w, "Checking Docker...")
	if _, err := exec.Run(ctx, "docker --version"); err != nil {
		fmt.Fprintln(w, "  Installing Docker...")

		// Try curl first, fall back to wget.
		installCmd := sudo + "sh -c 'curl -fsSL https://get.docker.com | sh'"
		if _, curlErr := exec.Run(ctx, "which curl"); curlErr != nil {
			installCmd = sudo + "sh -c 'wget -qO- https://get.docker.com | sh'"
		}

		out, err := exec.Run(ctx, installCmd)
		if err != nil {
			// Show output on failure for debugging.
			fmt.Fprintln(w, out)
			return fmt.Errorf("installing docker: %w", err)
		}

		// Verify Docker actually installed and print version.
		ver, err := exec.Run(ctx, "docker --version")
		if err != nil {
			return fmt.Errorf("docker install appeared to succeed but docker is not available")
		}
		fmt.Fprintf(w, "  Docker installed (%s)\n", strings.TrimPrefix(strings.TrimSpace(ver), "Docker version "))

		// Add current user to docker group so sudo isn't needed for docker commands.
		exec.Run(ctx, sudo+"usermod -aG docker $(whoami)")
	} else {
		fmt.Fprintln(w, "  Docker already installed")
	}

	// 2. Check firewall
	fmt.Fprintln(w, "Checking firewall...")
	ufwOutput, ufwErr := exec.Run(ctx, "ufw status 2>/dev/null")
	if ufwErr == nil && strings.Contains(ufwOutput, "Status: active") {
		_, err1 := exec.Run(ctx, sudo+"ufw allow 80/tcp")
		_, err2 := exec.Run(ctx, sudo+"ufw allow 443/tcp")
		if err1 != nil || err2 != nil {
			fmt.Fprintln(w, "  Warning: could not configure ufw. Ensure ports 80 and 443 are open.")
		} else {
			fmt.Fprintln(w, "  Opened ports 80 and 443 (ufw)")
		}
	} else if _, err := exec.Run(ctx, "systemctl is-active firewalld 2>/dev/null"); err == nil {
		fmt.Fprintln(w, "  Warning: firewalld detected. Ensure ports 80 and 443 are open.")
	} else {
		fmt.Fprintln(w, "  No active firewall detected")
	}

	// Determine docker prefix: use sudo if user can't access docker directly.
	dockerCmd := "docker"
	if _, err := exec.Run(ctx, "docker info >/dev/null 2>&1"); err != nil {
		dockerCmd = sudo + "docker"
	}

	// 3. Create Docker network
	fmt.Fprintln(w, "Creating Docker network...")
	netCmd := dockerCmd + " network inspect teploy >/dev/null 2>&1 || " + dockerCmd + " network create teploy"
	if _, err := exec.Run(ctx, netCmd); err != nil {
		return fmt.Errorf("creating docker network: %w", err)
	}

	// 4. Create directories and upload Caddyfile
	if _, err := exec.Run(ctx, sudo+"mkdir -p /deployments/caddy"); err != nil {
		return fmt.Errorf("creating directories: %w", err)
	}
	// Ensure deploy user owns the directory.
	exec.Run(ctx, sudo+"chown -R $(whoami):$(whoami) /deployments")

	// Caddy admin API listens on 0.0.0.0 inside container so Docker port
	// forwarding can reach it. Port 2019 is only published to 127.0.0.1
	// on the host — never publicly accessible.
	// Tab-indented to match `caddy fmt` output so Caddy doesn't warn.
	// Only write the stub Caddyfile when none exists — on servers that
	// were provisioned by other tooling (e.g., Dokploy) or hand-edited,
	// the existing Caddyfile holds live production routes and must be
	// preserved.
	const stubCaddyfile = "{\n\tadmin 127.0.0.1:2019\n}\n"
	if _, err := exec.Run(ctx, "test -s /deployments/caddy/Caddyfile"); err != nil {
		if err := exec.Upload(ctx, strings.NewReader(stubCaddyfile), "/deployments/caddy/Caddyfile", "0644"); err != nil {
			return fmt.Errorf("uploading Caddyfile: %w", err)
		}
	} else {
		fmt.Fprintln(w, "  Existing Caddyfile preserved")
		// Lock the admin API to the container loopback. Older setups bound it to
		// 0.0.0.0:2019, reachable by any container on the teploy network.
		exec.Run(ctx, "sed -i 's/admin 0.0.0.0:2019/admin 127.0.0.1:2019/' /deployments/caddy/Caddyfile")
	}

	// 5. Start Caddy (idempotent — skip if container already exists).
	// The on-disk Caddyfile is Teploy's single source of truth: Caddy loads it
	// on every boot and `caddy reload`, so we run WITHOUT `--resume` (which
	// would boot from admin-API autosave and shadow the file). The admin API
	// binds the container loopback only and is never exposed off-box.
	fmt.Fprintln(w, "Starting Caddy...")
	caddyCheck, _ := exec.Run(ctx, dockerCmd+" ps -a --filter name=^caddy$ --format '{{.Names}}'")
	extraNetworks := []string{}
	if strings.TrimSpace(caddyCheck) != "" {
		// Two legacy conditions require recreating the Caddy container. Both
		// recreations are destructive (brief outage + re-attaching non-teploy
		// networks), so we require explicit confirmation.
		cmdOut, _ := exec.Run(ctx, dockerCmd+" inspect -f '{{join .Config.Cmd \" \"}}' caddy")
		mountOut, _ := exec.Run(ctx, dockerCmd+" inspect -f '{{range .Mounts}}{{.Destination}} {{end}}' caddy")

		// (1) --resume boots from admin-API autosave, shadowing the Caddyfile.
		legacyResume := strings.Contains(cmdOut, "--resume")
		// (2) A single-file Caddyfile bind mount (Destination
		// /etc/caddy/Caddyfile) is pinned by Docker to the file's original
		// inode. Teploy writes the Caddyfile atomically (write tmp + mv),
		// which swaps the inode — so the running container never sees route
		// updates and `caddy reload` reloads stale config. The directory
		// mount (/etc/caddy) re-resolves the file by path on each reload and
		// avoids this. Detect the legacy file mount and migrate.
		legacyFileMount := strings.Contains(mountOut, "/etc/caddy/Caddyfile")

		if legacyResume || legacyFileMount {
			// Capture every network the existing Caddy is attached to so
			// we can reattach them after recreating. Previously these were
			// silently dropped, leaving apps on other networks (e.g.
			// dokploy-network) unreachable.
			netOut, _ := exec.Run(ctx, dockerCmd+" inspect -f '{{range $k,$v := .NetworkSettings.Networks}}{{$k}} {{end}}' caddy")
			for _, n := range strings.Fields(netOut) {
				if n != "" && n != "teploy" {
					extraNetworks = append(extraNetworks, n)
				}
			}

			reason := "running with --resume (legacy admin-API mode)"
			if !legacyResume && legacyFileMount {
				reason = "using a single-file Caddyfile mount (atomic config writes don't reach the container)"
			}
			fmt.Fprintf(w, "  Caddy is %s — migrating to the directory-mounted, Caddyfile-authoritative model.\n", reason)
			fmt.Fprintln(w, "  This recreates the container (brief outage).")
			if len(extraNetworks) > 0 {
				fmt.Fprintf(w, "  Additional networks to reattach: %s\n", strings.Join(extraNetworks, ", "))
			}
			if !yes && !confirm(w, "  Recreate Caddy container now?") {
				fmt.Fprintln(w, "  Skipping Caddy upgrade — re-run with --yes to apply.")
				return nil
			}
			if _, err := exec.Run(ctx, dockerCmd+" rm -f caddy"); err != nil {
				return fmt.Errorf("removing old caddy: %w", err)
			}
			caddyCheck = ""
		}
	}
	if strings.TrimSpace(caddyCheck) == "" {
		caddyRun := strings.Join([]string{
			dockerCmd, "run", "-d",
			"--restart", "always",
			"--name", "caddy",
			"--network", "teploy",
			"-p", "80:80",
			"-p", "443:443",
			"-v", "caddy_data:/data",
			"-v", "caddy_config:/config",
			// Mount the directory, NOT the single Caddyfile. A single-file
			// bind mount pins Docker to the file's inode, so teploy's atomic
			// (tmp + mv) Caddyfile writes never reach the container. Mounting
			// the directory lets `caddy reload` re-read the current file by
			// path. It also exposes /deployments/caddy/tls/* as /etc/caddy/tls
			// for apps that terminate TLS with a custom cert (e.g. a
			// Cloudflare Origin Certificate).
			"-v", "/deployments/caddy:/etc/caddy",
			"caddy",
			"caddy", "run", "--config", "/etc/caddy/Caddyfile", "--adapter", "caddyfile",
		}, " ")
		if _, err := exec.Run(ctx, caddyRun); err != nil {
			return fmt.Errorf("starting caddy: %w", err)
		}
		for _, n := range extraNetworks {
			if _, err := exec.Run(ctx, fmt.Sprintf("%s network connect %s caddy", dockerCmd, n)); err != nil {
				fmt.Fprintf(w, "  Warning: failed to reattach %s: %v\n", n, err)
			} else {
				fmt.Fprintf(w, "  Reattached network %s\n", n)
			}
		}
		fmt.Fprintln(w, "  Caddy started")
	} else {
		fmt.Fprintln(w, "  Caddy already running")
	}

	fmt.Fprintln(w, "Server provisioned successfully")
	return nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

// confirm prompts the user for a yes/no answer on stdin. Returns false
// when stdin isn't a TTY so non-interactive runs fail safe (callers
// should require --yes to proceed without interactive confirmation).
func confirm(w io.Writer, prompt string) bool {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Fprintln(w, "  (non-interactive — pass --yes to confirm)")
		return false
	}
	fmt.Fprintf(w, "%s [y/N]: ", prompt)
	var answer string
	if _, err := fmt.Fscanln(os.Stdin, &answer); err != nil {
		return false
	}
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "y" || answer == "yes"
}
