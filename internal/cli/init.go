package cli

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/spf13/cobra"
	"github.com/useteploy/teploy/internal/config"
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
			return runInit(force, useTOML)
		},
	}

	cmd.Flags().BoolVar(&force, "force", false, "overwrite existing config")
	cmd.Flags().BoolVar(&useTOML, "toml", false, "generate teploy.toml instead of teploy.yml")
	return cmd
}

func runInit(force, useTOML bool) error {
	dir := "."
	filename := "teploy.yml"
	if useTOML {
		filename = "teploy.toml"
	}
	outPath := filepath.Join(dir, filename)

	// Check for existing config.
	if _, err := os.Stat(outPath); err == nil && !force {
		return fmt.Errorf("%s already exists (use --force to overwrite)", filename)
	}

	reader := bufio.NewReader(os.Stdin)
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

	// Domain.
	defaultDomain := cfg.Domain
	if defaultDomain == "" || strings.HasSuffix(defaultDomain, ".example.com") {
		defaultDomain = cfg.App + ".example.com"
	}
	cfg.Domain = prompt(reader, "Domain", defaultDomain)

	// Server.
	defaultServer := cfg.Server
	cfg.Server = prompt(reader, "Server (name from servers.yml or IP)", defaultServer)

	// Write config.
	var out []byte
	var err error
	if useTOML {
		var buf strings.Builder
		enc := toml.NewEncoder(&buf)
		if err := enc.Encode(cfg); err != nil {
			return fmt.Errorf("marshaling config: %w", err)
		}
		out = []byte(buf.String())
	} else {
		out, err = yaml.Marshal(cfg)
		if err != nil {
			return fmt.Errorf("marshaling config: %w", err)
		}
	}

	if err := os.WriteFile(outPath, out, 0644); err != nil {
		return fmt.Errorf("writing %s: %w", filename, err)
	}

	fmt.Printf("\nCreated %s\n", filename)
	return nil
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
