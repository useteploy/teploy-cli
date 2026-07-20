package cli

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/spf13/cobra"
	"github.com/useteploy/teploy/internal/config"
	"golang.org/x/term"
	"gopkg.in/yaml.v3"
)

func newInitCmd() *cobra.Command {
	var (
		force   bool
		useTOML bool
	)

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Generate teploy config for this project",
		Long:  "Interactive config generation. Detects docker-compose.yml, Dockerfile, and other project files to pre-fill answers.\nUse --toml to generate teploy.toml instead of teploy.yml.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			_, err := initFlow(bufio.NewReader(os.Stdin), ".", useTOML, force)
			return err
		},
	}

	cmd.Flags().BoolVar(&force, "force", false, "overwrite existing config")
	cmd.Flags().BoolVar(&useTOML, "toml", false, "generate teploy.toml instead of teploy.yml")
	return cmd
}

// initFlow prompts for the minimal config and writes it to dir. Shared by
// `teploy init` and the zero-config first run inside `teploy deploy`.
// Returns the path of the file written.
func initFlow(reader *bufio.Reader, dir string, useTOML, force bool) (string, error) {
	filename := "teploy.yml"
	if useTOML {
		filename = "teploy.toml"
	}
	outPath := filepath.Join(dir, filename)

	// Check for existing config.
	if _, err := os.Stat(outPath); err == nil && !force {
		return "", fmt.Errorf("%s already exists (use --force to overwrite)", filename)
	}

	cfg := &config.AppConfig{}

	// Try compose auto-detection.
	composeCfg, _ := config.LoadCompose(dir)
	if composeCfg != nil {
		fmt.Printf("Found docker-compose file. Generate %s from it? (y/n): ", filename)
		answer, _ := reader.ReadString('\n')
		if strings.TrimSpace(strings.ToLower(answer)) == "y" {
			cfg = composeCfg
		}
	}

	// App name.
	defaultApp := cfg.App
	if defaultApp == "" {
		defaultApp = detectAppName(dir)
	}
	cfg.App = prompt(reader, "App name", defaultApp)

	// Domain. No fabricated default — an example.com placeholder that then
	// deploys is worse than a question. Empty switches to ingress:host
	// (publish a raw port), which validate() requires a port for.
	defaultDomain := cfg.Domain
	if strings.HasSuffix(defaultDomain, ".example.com") {
		defaultDomain = ""
	}
	cfg.Domain = prompt(reader, "Domain (empty = publish a raw port, no HTTPS routing)", defaultDomain)
	if cfg.Domain == "" {
		cfg.Ingress = config.IngressHost
		defaultPort := cfg.Port
		if defaultPort <= 0 {
			defaultPort = 3000
		}
		for {
			answer := prompt(reader, "App port to publish on the server", strconv.Itoa(defaultPort))
			p, err := strconv.Atoi(strings.TrimSpace(answer))
			if err == nil && p > 0 && p < 65536 {
				cfg.Port = p
				break
			}
			fmt.Println("Port must be a number between 1 and 65535.")
		}
	}

	// Server. Offer known servers.yml entries so first-run users pick a
	// name instead of retyping an IP.
	defaultServer := cfg.Server
	if names := knownServerNames(); len(names) > 0 {
		fmt.Printf("Known servers: %s\n", strings.Join(names, ", "))
		if defaultServer == "" && len(names) == 1 {
			defaultServer = names[0]
		}
	}
	cfg.Server = prompt(reader, "Server (name from servers.yml or IP)", defaultServer)

	// Write config.
	var out []byte
	var err error
	if useTOML {
		var buf strings.Builder
		enc := toml.NewEncoder(&buf)
		if err := enc.Encode(cfg); err != nil {
			return "", fmt.Errorf("marshaling config: %w", err)
		}
		out = []byte(buf.String())
	} else {
		out, err = yaml.Marshal(cfg)
		if err != nil {
			return "", fmt.Errorf("marshaling config: %w", err)
		}
	}

	if err := os.WriteFile(outPath, out, 0644); err != nil {
		return "", fmt.Errorf("writing %s: %w", filename, err)
	}

	fmt.Printf("\nCreated %s\n", filename)
	return outPath, nil
}

// stdinIsTerminal reports whether stdin is an interactive terminal — the
// gate for offering prompts instead of failing (CI/pipes keep hard errors).
func stdinIsTerminal() bool {
	return term.IsTerminal(int(os.Stdin.Fd()))
}

// knownServerNames returns the sorted server names from ~/.teploy/servers.yml,
// or nil if the file is missing/unreadable (first-run friendly, never fatal).
func knownServerNames() []string {
	path, err := config.DefaultServersPath()
	if err != nil {
		return nil
	}
	servers, err := config.ListServers(path)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func prompt(reader *bufio.Reader, label, defaultValue string) string {
	if defaultValue != "" {
		fmt.Printf("%s [%s]: ", label, defaultValue)
	} else {
		fmt.Printf("%s: ", label)
	}
	answer, _ := reader.ReadString('\n')
	answer = strings.TrimSpace(answer)
	if answer == "" {
		return defaultValue
	}
	return answer
}

func detectAppName(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "myapp"
	}
	name := filepath.Base(abs)
	name = strings.ToLower(strings.ReplaceAll(name, "_", "-"))
	name = strings.ReplaceAll(name, " ", "-")
	if name == "" || name == "." {
		return "myapp"
	}
	return name
}
