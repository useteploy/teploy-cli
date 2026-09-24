package cli

import (
	"context"
	"encoding/json"
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

// setupReconnectBudget bounds how many times the setup flow redials
// after a transport failure before giving the stage-named error back to
// the operator (C08 connection recovery). Three covers a transient
// network blip and one brief outage without turning a dead box into a
// multi-minute hang.
const setupReconnectBudget = 3

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

	// The whole setup flow runs through a reconnecting executor (C08
	// connection recovery): a transport failure mid-provisioning — SSH
	// channel death, connection reset — redials and retries the one
	// invocation that died, instead of aborting at whatever stage it
	// hit. Setup, hardening, and network join are check-then-act
	// idempotent, so every retried command is safe to re-run; commands
	// that RAN and failed are never retried here.
	dial := func(ctx context.Context) (ssh.Executor, error) {
		return ssh.Connect(ctx, cfg)
	}
	executor, err := ssh.NewReconnectingExecutor(ctx, dial, setupReconnectBudget)
	if err != nil {
		return err
	}
	defer executor.Close()

	// If password auth was used, inject the local SSH public key for future key-based auth.
	if usePassword {
		// Derived from the private identity itself (A32): the old
		// PublicKeyPath fallthrough could hand provisioning an unrelated
		// default public key when --key named a key without a .pub.
		pubKeyData, err := ssh.PublicKeyBytes(flags.Key)
		if err != nil {
			return fmt.Errorf("deriving SSH public key: %w", err)
		}
		if err := installAuthorizedKey(ctx, executor, strings.TrimSpace(string(pubKeyData))); err != nil {
			return err
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
			// Root SSH denied — run su over the existing connection.
			// The root password travels the session's stdin and nowhere
			// else: the old path embedded it in a script uploaded to
			// /tmp (an on-disk artifact) whose removal was best-effort —
			// a failed run could leave the root password sitting in
			// /tmp (C08).
			if err := installSudoViaSu(ctx, executor, user, rootPass); err != nil {
				return err
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
		vpnDial := func(ctx context.Context) (ssh.Executor, error) {
			return ssh.Connect(ctx, reconnectCfg)
		}
		executor, err = ssh.NewReconnectingExecutor(ctx, vpnDial, setupReconnectBudget)
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

// installSudoViaSu installs sudo + configures passwordless sudo for
// user by feeding su the root password over the session's stdin. No
// temp file, no password in any command string (C08). Success is the
// TEPLOY_SUDO_OK marker — su's own exit status alone does not prove the
// whole chain ran.
func installSudoViaSu(ctx context.Context, executor ssh.Executor, user, rootPass string) error {
	inner := fmt.Sprintf(
		`DEBIAN_FRONTEND=noninteractive apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq sudo >/dev/null 2>&1 && usermod -aG sudo %s && printf '%%s\n' %s > /etc/sudoers.d/%s && chmod 440 /etc/sudoers.d/%s && echo TEPLOY_SUDO_OK`,
		user, ssh.ShellQuote(user+" ALL=(ALL) NOPASSWD:ALL"), user, user)
	cmd := "su -c " + ssh.ShellQuote(inner) + " - root"
	res := ssh.RunInputDetailed(ctx, executor, cmd, strings.NewReader(rootPass+"\n"))
	combined := string(res.Stdout) + "\n" + string(res.Stderr)
	if res.Err == nil && strings.Contains(combined, "TEPLOY_SUDO_OK") {
		return nil
	}
	if strings.Contains(combined, "Authentication failure") {
		return fmt.Errorf("wrong root password")
	}
	if res.Err != nil {
		return fmt.Errorf("installing sudo via su: %w", res.Err)
	}
	return fmt.Errorf("installing sudo via su failed: %s", strings.TrimSpace(combined))
}

// installAuthorizedKey appends the public key to ~/.ssh/authorized_keys
// — GUARDED, so an interrupted setup re-run cannot stack duplicate
// entries (C08 resumability): the old bare `echo <key> >>` appended on
// every attempt.
func installAuthorizedKey(ctx context.Context, executor ssh.Executor, pubKey string) error {
	cmd := fmt.Sprintf(
		"mkdir -p ~/.ssh && grep -qF %s ~/.ssh/authorized_keys 2>/dev/null || echo %s >> ~/.ssh/authorized_keys; chmod 700 ~/.ssh && chmod 600 ~/.ssh/authorized_keys",
		ssh.ShellQuote(pubKey), ssh.ShellQuote(pubKey),
	)
	if _, err := executor.Run(ctx, cmd); err != nil {
		return fmt.Errorf("installing SSH key: %w", err)
	}
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
	if err := joinVPNMesh(ctx, exec, sudo, providerName, cfg); err != nil {
		return "", err
	}

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

// vpnCredential resolves the provider's join credential and the env var
// its CLI documents for it (tailscale up reads TS_AUTHKEY, netbird up
// reads NB_SETUP_KEY).
func vpnCredential(providerName string, cfg network.Config) (envVar, value string, err error) {
	switch providerName {
	case "tailscale", "headscale":
		return "TS_AUTHKEY", cfg.AuthKey, nil
	case "netbird":
		return "NB_SETUP_KEY", cfg.SetupKey, nil
	default:
		return "", "", fmt.Errorf("unknown network provider: %q", providerName)
	}
}

// joinVPNMesh stages the join credential in a private file and fires the
// detached provider join. The credential never enters any command
// string: the detached shell reads the file into the provider's
// documented env var, removes the file BEFORE exec'ing the provider,
// then execs (C08) — the transport lives in the network package
// (StageJoinCredential/EnvVarJoinShell) and is shared with
// `teploy network join`. Failure semantics: an upload failure aborts
// setup with the file path named (nothing was joined); a join failure
// after that surfaces through the tailnet wait below, and the key file
// is gone regardless — its only reader is the rm-ing shell itself.
func joinVPNMesh(ctx context.Context, exec ssh.Executor, sudo, providerName string, cfg network.Config) error {
	_, credential, err := vpnCredential(providerName, cfg)
	if err != nil {
		return err
	}
	if credential == "" {
		return fmt.Errorf("%s join credential is empty", providerName)
	}
	keyPath, err := network.StageJoinCredential(ctx, exec, credential)
	if err != nil {
		return err
	}
	_, _ = exec.Run(ctx, vpnJoinCommand(providerName, cfg, sudo, keyPath))
	return nil
}

// vpnJoinCommand renders the detached join: `cat` the key file into the
// provider's env var, remove the file, then exec the provider. The
// login-server URL (headscale) is not a secret and stays a plain flag.
func vpnJoinCommand(providerName string, cfg network.Config, sudo, keyPath string) string {
	envVar, _, _ := vpnCredential(providerName, cfg)
	var tail string
	switch providerName {
	case "tailscale":
		tail = "tailscale up --accept-routes"
	case "headscale":
		tail = "tailscale up --login-server=" + ssh.ShellQuote(cfg.Server) + " --accept-routes"
	case "netbird":
		tail = "netbird up"
	}
	return sudo + "nohup " + network.EnvVarJoinShell(envVar, keyPath, tail) + " >/dev/null 2>&1 &"
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

// dockerMount mirrors the fields teploy needs from `docker inspect`'s
// per-mount JSON (Type/Name/Source/Destination/RW) — used to detect and
// preserve mounts on an existing Caddy container that teploy didn't create
// itself, when recreating it.
type dockerMount struct {
	Type        string
	Name        string // set for Type "volume"; empty for "bind"
	Source      string
	Destination string
	RW          bool
}

// setupStage is one resumable provisioning step (C08): each stage is
// check-then-act idempotent, so an interrupted setup re-run skips what
// completed and continues — no duplicate or corrupt state. affects is
// the preflight's affected-resource description.
type setupStage struct {
	name    string
	affects string
	run     func(ctx context.Context, exec ssh.Executor, w io.Writer, env *setupEnv) error
}

// setupEnv carries state computed by early stages for later ones.
type setupEnv struct {
	sudo      string
	dockerCmd string
	yes       bool
}

// setupStages is the ordered provisioning plan. The ORDER is part of
// the contract: docker before the network that needs it, the network
// before the container that joins it, directories before the files
// that land in them.
func setupStages() []setupStage {
	return []setupStage{
		{
			name:    "sudo detection",
			affects: "nothing (read-only: whoami)",
			run: func(ctx context.Context, exec ssh.Executor, w io.Writer, env *setupEnv) error {
				if whoami, _ := exec.Run(ctx, "whoami"); strings.TrimSpace(whoami) != "root" {
					env.sudo = "sudo "
				}
				return nil
			},
		},
		{
			name:    "docker",
			affects: "Docker engine (installed via get.docker.com if missing), current user in the docker group",
			run: func(ctx context.Context, exec ssh.Executor, w io.Writer, env *setupEnv) error {
				// 1. Check/install Docker
				fmt.Fprintln(w, "Checking Docker...")
				if _, err := exec.Run(ctx, "docker --version"); err != nil {
					fmt.Fprintln(w, "  Installing Docker...")

					// Try curl first, fall back to wget.
					installCmd := env.sudo + "sh -c 'curl -fsSL https://get.docker.com | sh'"
					if _, curlErr := exec.Run(ctx, "which curl"); curlErr != nil {
						installCmd = env.sudo + "sh -c 'wget -qO- https://get.docker.com | sh'"
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
					exec.Run(ctx, env.sudo+"usermod -aG docker $(whoami)")
				} else {
					fmt.Fprintln(w, "  Docker already installed")
				}
				return nil
			},
		},
		{
			name:    "rsync",
			affects: "rsync package (apt, if missing) — required by type:static deploys",
			run: func(ctx context.Context, exec ssh.Executor, w io.Writer, env *setupEnv) error {
				// 2. Check/install rsync — required by type:static deploys (internal/deploy
				// static.go shells out to it directly). Not preinstalled on minimal
				// Debian/Ubuntu cloud images, so a fresh box otherwise deploys containers
				// fine but fails static deploys on the first rsync with a cryptic
				// "command not found" from the remote shell.
				fmt.Fprintln(w, "Checking rsync...")
				if _, err := exec.Run(ctx, "rsync --version"); err != nil {
					fmt.Fprintln(w, "  Installing rsync...")
					installCmd := env.sudo + "sh -c 'DEBIAN_FRONTEND=noninteractive apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq rsync'"
					if out, err := exec.Run(ctx, installCmd); err != nil {
						fmt.Fprintln(w, out)
						return fmt.Errorf("installing rsync: %w", err)
					}
					fmt.Fprintln(w, "  rsync installed")
				} else {
					fmt.Fprintln(w, "  rsync already installed")
				}
				return nil
			},
		},
		{
			name:    "firewall",
			affects: "tcp ports 80 and 443 in ufw, when ufw is active",
			run: func(ctx context.Context, exec ssh.Executor, w io.Writer, env *setupEnv) error {
				// 3. Check firewall
				fmt.Fprintln(w, "Checking firewall...")
				ufwOutput, ufwErr := exec.Run(ctx, "ufw status 2>/dev/null")
				if ufwErr == nil && strings.Contains(ufwOutput, "Status: active") {
					_, err1 := exec.Run(ctx, env.sudo+"ufw allow 80/tcp")
					_, err2 := exec.Run(ctx, env.sudo+"ufw allow 443/tcp")
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
				return nil
			},
		},
		{
			name:    "docker access probe",
			affects: "nothing (read-only: docker info — decides whether docker needs sudo)",
			run: func(ctx context.Context, exec ssh.Executor, w io.Writer, env *setupEnv) error {
				env.dockerCmd = "docker"
				if _, err := exec.Run(ctx, "docker info >/dev/null 2>&1"); err != nil {
					env.dockerCmd = env.sudo + "docker"
				}
				return nil
			},
		},
		{
			name:    "docker network",
			affects: "docker network 'teploy' (created if missing)",
			run: func(ctx context.Context, exec ssh.Executor, w io.Writer, env *setupEnv) error {
				// 4. Create Docker network
				fmt.Fprintln(w, "Creating Docker network...")
				netCmd := env.dockerCmd + " network inspect teploy >/dev/null 2>&1 || " + env.dockerCmd + " network create teploy"
				if _, err := exec.Run(ctx, netCmd); err != nil {
					return fmt.Errorf("creating docker network: %w", err)
				}
				return nil
			},
		},
		{
			name:    "directories + Caddyfile",
			affects: "/deployments and /deployments/caddy (ownership of these two control-plane dirs only), a stub Caddyfile only if none exists",
			run: func(ctx context.Context, exec ssh.Executor, w io.Writer, env *setupEnv) error {
				// 5. Create directories and upload Caddyfile
				if _, err := exec.Run(ctx, env.sudo+"mkdir -p /deployments/caddy"); err != nil {
					return fmt.Errorf("creating directories: %w", err)
				}
				// Ensure the deploy user owns the CONTROL-PLANE directories only —
				// never the whole /deployments tree. The old `chown -R
				// $(whoami):$(whoami) /deployments` reassigned every application's
				// bind-mounted data (database files owned by engine UIDs, accessory
				// state) to the interactive SSH user on every re-run, breaking engines
				// that rely on their own numeric ownership (TCL-26). Existing app data
				// ownership is an invariant, not setup's cleanup target: app
				// directories created later are owned by this user anyway, and the
				// error is propagated instead of ignored.
				if _, err := exec.Run(ctx, env.sudo+"chown $(whoami):$(whoami) /deployments /deployments/caddy"); err != nil {
					return fmt.Errorf("setting control-plane directory ownership: %w", err)
				}

				// Caddy admin API listens on 0.0.0.0 inside container so Docker port
				// forwarding can reach it. Port 2019 is only published to 127.0.0.1
				// on the host — never publicly accessible.
				// Tab-indented to match `caddy fmt` output so Caddy doesn't warn.
				// Only write the stub Caddyfile when none exists — on servers that
				// were provisioned by other tooling (e.g., Dokploy) or hand-edited,
				// the existing Caddyfile holds live production routes and must be
				// preserved.
				const stubCaddyfile = "{\n\tadmin 127.0.0.1:2019\n}\n"
				// Only write the stub Caddyfile when the file is confirmed ABSENT —
				// `test -s` also fails for an unreadable or empty-but-present file, and
				// overwriting either with a stub discards live routes (TCL-27
				// containment). An existing empty file is left for the operator to
				// inspect rather than silently clobbered.
				present, err := exec.Run(ctx, "[ -f /deployments/caddy/Caddyfile ] && echo present || echo absent")
				if err != nil {
					return fmt.Errorf("checking for an existing Caddyfile: %w", err)
				}
				if strings.TrimSpace(present) == "absent" {
					if err := exec.Upload(ctx, strings.NewReader(stubCaddyfile), "/deployments/caddy/Caddyfile", "0644"); err != nil {
						return fmt.Errorf("uploading Caddyfile: %w", err)
					}
				} else {
					fmt.Fprintln(w, "  Existing Caddyfile preserved")
					// Lock the admin API to the container loopback. Older setups bound it to
					// 0.0.0.0:2019, reachable by any container on the teploy network.
					exec.Run(ctx, "sed -i 's/admin 0.0.0.0:2019/admin 127.0.0.1:2019/' /deployments/caddy/Caddyfile")
				}
				return nil
			},
		},
		{
			name:    "caddy container",
			affects: "caddy container (started if absent/stopped; a legacy-config container is recreated after confirmation — brief outage)",
			run: func(ctx context.Context, exec ssh.Executor, w io.Writer, env *setupEnv) error {
				// 6. Start Caddy (idempotent — skip if container already exists).
				// The on-disk Caddyfile is Teploy's single source of truth: Caddy loads it
				// on every boot and `caddy reload`, so we run WITHOUT `--resume` (which
				// would boot from admin-API autosave and shadow the file). The admin API
				// binds the container loopback only and is never exposed off-box.
				fmt.Fprintln(w, "Starting Caddy...")
				caddyCheck, err := exec.Run(ctx, env.dockerCmd+" ps -a --filter name=^caddy$ --format '{{.Names}}'")
				if err != nil {
					return fmt.Errorf("checking for an existing caddy container: %w", err)
				}
				extraNetworks := []string{}
				var extraMountFlags []string
				if strings.TrimSpace(caddyCheck) != "" {
					// Three legacy conditions require recreating the Caddy container. All
					// three recreations are destructive (brief outage + re-attaching
					// non-teploy networks/mounts), so we require explicit confirmation.
					// Inventory reads must SUCCEED before any recreate decision: a failed
					// inspect used to read as an empty set, which both triggered
					// unnecessary migrations and silently dropped adopted networks/mounts
					// during a real one (audit F67).
					cmdOut, cmdErr := exec.Run(ctx, env.dockerCmd+" inspect -f '{{join .Config.Cmd \" \"}}' caddy")
					if cmdErr != nil {
						return fmt.Errorf("cannot safely inventory the existing caddy container (cmd): %w", cmdErr)
					}
					mountOut, mountErr := exec.Run(ctx, env.dockerCmd+" inspect -f '{{range .Mounts}}{{.Destination}} {{end}}' caddy")
					if mountErr != nil {
						return fmt.Errorf("cannot safely inventory the existing caddy container (mounts): %w", mountErr)
					}

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
					// (3) No /deployments bind mount at all means type:static deploys
					// are completely non-functional on this server — Deploy writes
					// releases to /deployments/<app>/current on the host, and Caddy's
					// generated site block reads from {DefaultStaticMount}/<app>/current
					// (also /deployments now — see internal/deploy/static.go), but
					// without this mount that path doesn't exist inside the container
					// at all. Confirmed live: `teploy deploy` reports success (the
					// release really does land on disk) while every request 404s.
					// Every server provisioned before this fix landed is missing this
					// mount unconditionally — not a legacy-config edge case, a gap in
					// what teploy setup has always provisioned.
					missingStaticMount := !strings.Contains(mountOut, "/deployments ")

					if legacyResume || legacyFileMount || missingStaticMount {
						// Capture every network the existing Caddy is attached to so
						// we can reattach them after recreating. Previously these were
						// silently dropped, leaving apps on other networks (e.g.
						// dokploy-network) unreachable.
						netOut, netErr := exec.Run(ctx, env.dockerCmd+" inspect -f '{{range $k,$v := .NetworkSettings.Networks}}{{$k}} {{end}}' caddy")
						if netErr != nil {
							return fmt.Errorf("cannot safely inventory the existing caddy container (networks): %w", netErr)
						}
						for _, n := range strings.Fields(netOut) {
							if n != "" && n != "teploy" {
								extraNetworks = append(extraNetworks, n)
							}
						}

						// Capture every mount teploy itself didn't put there, so those
						// are preserved too — the same principle as extraNetworks
						// above, just for volumes. Without this, recreating Caddy
						// blindly replaces its whole mount set with teploy's own fixed
						// list (caddy_data, caddy_config, /etc/caddy, /deployments),
						// silently dropping anything hand-added on a server that
						// predates teploy or was adopted from other tooling — e.g. a
						// legacy /srv/static bind mount serving live static sites,
						// which would 404 the instant the container came back up.
						// Confirmed live on a real production box before this fix:
						// exactly that mount existed and would have been lost.
						// This is what actually makes it safe to run `teploy setup`
						// against an existing, previously-hand-managed server — not
						// just a fresh one.
						var extraMounts []dockerMount
						// The detailed mount inventory must SUCCEED before the
						// destructive recreate below: a failed inspect or unparseable
						// JSON used to read as "no extra mounts", silently dropping
						// adopted volumes during the migration (TCL-27).
						mountJSON, mountJSONErr := exec.Run(ctx, env.dockerCmd+" inspect -f '{{json .Mounts}}' caddy")
						if mountJSONErr != nil {
							return fmt.Errorf("cannot safely inventory the existing caddy container (mount detail): %w", mountJSONErr)
						}
						teployDestinations := map[string]bool{
							"/data": true, "/config": true, "/etc/caddy": true, "/deployments": true,
						}
						var mounts []dockerMount
						if err := json.Unmarshal([]byte(mountJSON), &mounts); err != nil {
							return fmt.Errorf("decoding the existing caddy container's mount inventory: %w", err)
						}
						for _, m := range mounts {
							if m.Type == "volume" && (m.Name == "caddy_data" || m.Name == "caddy_config") {
								continue // teploy's own named volumes, re-added explicitly below
							}
							if teployDestinations[m.Destination] {
								continue // teploy's own bind mounts, re-added explicitly below
							}
							extraMounts = append(extraMounts, m)
						}
						for _, m := range extraMounts {
							src := m.Source
							if m.Type == "volume" && m.Name != "" {
								src = m.Name
							}
							flag := fmt.Sprintf("%s:%s", src, m.Destination)
							if !m.RW {
								flag += ":ro"
							}
							extraMountFlags = append(extraMountFlags, flag)
						}

						reason := "running with --resume (legacy admin-API mode)"
						switch {
						case !legacyResume && legacyFileMount:
							reason = "using a single-file Caddyfile mount (atomic config writes don't reach the container)"
						case !legacyResume && !legacyFileMount && missingStaticMount:
							reason = "missing the /deployments mount type:static deploys require to actually be served"
						}
						fmt.Fprintf(w, "  Caddy is %s — migrating to the directory-mounted, Caddyfile-authoritative model.\n", reason)
						fmt.Fprintln(w, "  This recreates the container (brief outage).")
						if len(extraNetworks) > 0 {
							fmt.Fprintf(w, "  Additional networks to reattach: %s\n", strings.Join(extraNetworks, ", "))
						}
						if len(extraMountFlags) > 0 {
							fmt.Fprintf(w, "  Additional mounts to preserve: %s\n", strings.Join(extraMountFlags, ", "))
						}
						if !env.yes && !confirm(w, "  Recreate Caddy container now?") {
							fmt.Fprintln(w, "  Skipping Caddy upgrade — re-run with --yes to apply.")
							return nil
						}
						if _, err := exec.Run(ctx, env.dockerCmd+" rm -f caddy"); err != nil {
							return fmt.Errorf("removing old caddy: %w", err)
						}
						caddyCheck = ""
					}
				}
				if strings.TrimSpace(caddyCheck) == "" {
					caddyRunArgs := []string{
						env.dockerCmd, "run", "-d",
						"--restart", "always",
						"--name", "caddy",
						"--network", "teploy",
						// Lets Caddy (in its own network namespace on the "teploy"
						// bridge) reach services bound only to the host's loopback —
						// specifically `teploy autodeploy serve`, a systemd-resident
						// host process (not a container, since it needs direct Docker
						// CLI access to run deploys) listening on 0.0.0.0:9876. Without
						// this, host.docker.internal doesn't resolve inside the
						// container on native Linux Docker (only Docker Desktop adds it
						// automatically) and the webhook route can never connect —
						// found live: SetupCaddyRoute's dial target was unreachable
						// from inside the container regardless of what host/IP it named.
						"--add-host", "host.docker.internal:host-gateway",
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
						// Lets Caddy serve type:static apps. Deploy (internal/deploy/
						// static.go) writes releases to /deployments/<app>/current on
						// the server's own filesystem and points each site block's
						// root at the identical path — /deployments/<app>/current —
						// inside the Caddy container. Read-only: Caddy the file
						// server never needs to write here, and this also mounts
						// every other app's /deployments/<app>/ tree (state, .env,
						// secrets) read-only into Caddy's filesystem — :ro caps what
						// a compromised Caddy process could do with that visibility
						// to read-only, even though none of it is web-exposed (each
						// site block's root stays scoped to that one app's own
						// current symlink).
						"-v", "/deployments:/deployments:ro",
					}
					// Re-add any mount that was on the previous container but isn't
					// one of teploy's own (see extraMountFlags above) — e.g. a legacy
					// /srv/static bind mount predating this fix, or anything else
					// adopted from other tooling. Without this, whatever those mounts
					// served would 404 the instant the recreated container came up.
					for _, m := range extraMountFlags {
						caddyRunArgs = append(caddyRunArgs, "-v", ssh.ShellQuote(m))
					}
					caddyRunArgs = append(caddyRunArgs,
						"caddy",
						"caddy", "run", "--config", "/etc/caddy/Caddyfile", "--adapter", "caddyfile",
					)
					caddyRun := strings.Join(caddyRunArgs, " ")
					if _, err := exec.Run(ctx, caddyRun); err != nil {
						return fmt.Errorf("starting caddy: %w", err)
					}
					for _, n := range extraNetworks {
						if _, err := exec.Run(ctx, fmt.Sprintf("%s network connect %s caddy", env.dockerCmd, n)); err != nil {
							fmt.Fprintf(w, "  Warning: failed to reattach %s: %v\n", n, err)
						} else {
							fmt.Fprintf(w, "  Reattached network %s\n", n)
						}
					}
					fmt.Fprintln(w, "  Caddy started")
				} else {
					// Presence in `docker ps -a` proves nothing about health — an exited
					// caddy also matches. Report what actually is, and start a stopped
					// one instead of declaring success by name (audit F67).
					running, rErr := exec.Run(ctx, env.dockerCmd+" inspect -f '{{.State.Running}}' caddy")
					if rErr != nil {
						return fmt.Errorf("checking the existing caddy container's state: %w", rErr)
					}
					switch strings.TrimSpace(running) {
					case "true":
						fmt.Fprintln(w, "  Caddy already running")
					case "false":
						fmt.Fprintln(w, "  Caddy container exists but is stopped — starting it")
						if _, sErr := exec.Run(ctx, env.dockerCmd+" start caddy"); sErr != nil {
							return fmt.Errorf("starting the stopped caddy container: %w", sErr)
						}
						fmt.Fprintln(w, "  Caddy started")
					default:
						return fmt.Errorf("could not determine the caddy container's state (inspect said %q)", strings.TrimSpace(running))
					}
				}
				return nil
			},
		},
	}
}

// setupServer runs the provisioning stages on a connected server.
// Separated from runSetup for testability with MockExecutor.
// yes skips interactive confirmation for destructive upgrade steps
// (Caddy recreate). Every stage is idempotent, so an interrupted run
// (connection loss, Ctrl-C) is safely resumable by re-running setup
// (C08); the preflight states what will be touched before anything is.
func setupServer(ctx context.Context, exec ssh.Executor, w io.Writer, yes bool) error {
	env := &setupEnv{yes: yes}
	stages := setupStages()

	fmt.Fprintln(w, "Preflight — this run will ensure (items already in place are skipped):")
	for _, s := range stages {
		fmt.Fprintf(w, "  - %s: %s\n", s.name, s.affects)
	}

	for _, s := range stages {
		if err := s.run(ctx, exec, w, env); err != nil {
			return fmt.Errorf("setup stage %q: %w", s.name, err)
		}
	}

	fmt.Fprintln(w, "Server provisioned successfully")
	return nil
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
