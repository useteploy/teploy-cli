package cli

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"

	"github.com/spf13/cobra"
	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/ssh"
)

func newRegistryCmd(flags *Flags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "registry",
		Short: "Manage Docker registry credentials on server",
	}

	cmd.AddCommand(newRegistryLoginCmd(flags))
	cmd.AddCommand(newRegistryListCmd(flags))
	cmd.AddCommand(newRegistryRemoveCmd(flags))

	return cmd
}

func newRegistryLoginCmd(flags *Flags) *cobra.Command {
	var (
		token    bool
		server   string
		username string
		password string
	)

	cmd := &cobra.Command{
		Use:   "login <registry>",
		Short: "Store registry credentials on server",
		Long:  "Run `docker login` on the server to store registry credentials for pulling private images.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRegistryLogin(flags, args[0], server, username, password, token)
		},
	}

	cmd.Flags().BoolVar(&token, "token", false, "read token from stdin (for CI)")
	cmd.Flags().StringVar(&server, "server", "", "server name or IP")
	cmd.Flags().StringVar(&username, "username", "", "registry username")
	cmd.Flags().StringVar(&password, "password", "", "registry password")

	return cmd
}

func runRegistryLogin(flags *Flags, registry, serverName, username, password string, useToken bool) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	executor, err := connectForRegistry(ctx, flags, serverName)
	if err != nil {
		return err
	}
	defer executor.Close()

	if useToken {
		// Read token from stdin.
		scanner := bufio.NewScanner(os.Stdin)
		if scanner.Scan() {
			password = scanner.Text()
		}
		if username == "" {
			username = "token"
		}
	} else if username == "" || password == "" {
		reader := bufio.NewReader(os.Stdin)
		if username == "" {
			fmt.Print("Username: ")
			username, _ = reader.ReadString('\n')
			username = strings.TrimSpace(username)
		}
		if password == "" {
			fmt.Print("Password: ")
			password, _ = reader.ReadString('\n')
			password = strings.TrimSpace(password)
		}
	}

	// Single-quote every value so the remote shell can't expand or execute it.
	// `echo %q` used double quotes, under which $/backticks in the password (or
	// registry/username) still expand — a shell-injection running as the SSH
	// user. printf '%s' emits the password literally to docker --password-stdin.
	cmd := fmt.Sprintf("printf '%%s' %s | docker login %s -u %s --password-stdin",
		ssh.ShellQuote(password), ssh.ShellQuote(registry), ssh.ShellQuote(username))
	if _, err := executor.Run(ctx, cmd); err != nil {
		return fmt.Errorf("docker login failed: %w", err)
	}

	fmt.Printf("Logged in to %s on server\n", registry)
	return nil
}

func newRegistryListCmd(flags *Flags) *cobra.Command {
	var server string

	cmd := &cobra.Command{
		Use:   "list",
		Short: "Show registries with stored credentials",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRegistryList(flags, server)
		},
	}

	cmd.Flags().StringVar(&server, "server", "", "server name or IP")
	return cmd
}

func runRegistryList(flags *Flags, serverName string) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	executor, err := connectForRegistry(ctx, flags, serverName)
	if err != nil {
		return err
	}
	defer executor.Close()

	output, err := executor.Run(ctx, "cat ~/.docker/config.json 2>/dev/null || echo '{}'")
	if err != nil {
		return err
	}

	// Simple parse — just show registry names.
	fmt.Println("Configured registries:")
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.Contains(line, ":") && !strings.Contains(line, "{") && !strings.Contains(line, "}") {
			// Extract registry name from JSON key.
			parts := strings.SplitN(line, ":", 2)
			registry := strings.Trim(parts[0], `" `)
			if registry != "" && registry != "auths" && registry != "credsStore" {
				fmt.Printf("  %s\n", registry)
			}
		}
	}
	return nil
}

func newRegistryRemoveCmd(flags *Flags) *cobra.Command {
	var server string

	cmd := &cobra.Command{
		Use:   "remove <registry>",
		Short: "Remove registry credentials from server",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRegistryRemove(flags, args[0], server)
		},
	}

	cmd.Flags().StringVar(&server, "server", "", "server name or IP")
	return cmd
}

func runRegistryRemove(flags *Flags, registry, serverName string) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	executor, err := connectForRegistry(ctx, flags, serverName)
	if err != nil {
		return err
	}
	defer executor.Close()

	if _, err := executor.Run(ctx, "docker logout "+registry); err != nil {
		return fmt.Errorf("docker logout failed: %w", err)
	}

	fmt.Printf("Removed credentials for %s\n", registry)
	return nil
}

// connectForRegistry establishes SSH connection using server flag, app config, or flags.
func connectForRegistry(ctx context.Context, flags *Flags, serverName string) (ssh.Executor, error) {
	if serverName == "" {
		// Try loading app config for server info.
		appCfg, err := config.LoadApp(".")
		if err == nil {
			serverName = appCfg.Server
		}
	}

	if serverName == "" && flags.Host == "" {
		return nil, fmt.Errorf("no server specified (use --server or --host)")
	}

	host, user, key, err := config.ResolveServer(serverName, flags.Host, flags.User, flags.Key)
	if err != nil {
		return nil, err
	}

	fmt.Printf("Connecting to %s@%s...\n", user, host)
	return ssh.Connect(ctx, ssh.ConnectConfig{
		Host:    host,
		User:    user,
		KeyPath: key,
	})
}
