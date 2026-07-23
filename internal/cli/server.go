package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"github.com/useteploy/teploy/internal/config"
)

func newServerCmd(flags *Flags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "server",
		Short: "Manage servers",
		Long:  "Add, remove, and list servers in ~/.teploy/servers.yml.",
	}

	cmd.AddCommand(newServerAddCmd(flags))
	cmd.AddCommand(newServerRemoveCmd(flags))
	cmd.AddCommand(newServerListCmd(flags))
	cmd.AddCommand(newServerStatusCmd(flags))

	return cmd
}

func newServerAddCmd(flags *Flags) *cobra.Command {
	var (
		role string
		user string
		key  string
	)

	cmd := &cobra.Command{
		Use:   "add <name> <host>",
		Short: "Add a server to servers.yml",
		Long:  "Add or update a server entry in ~/.teploy/servers.yml with an optional role (app or lb).",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			host := args[1]

			if role != "" && role != "app" && role != "lb" {
				return fmt.Errorf("role must be 'app' or 'lb' (got %q)", role)
			}

			if user == "" {
				user = "root"
			}

			path, err := config.DefaultServersPath()
			if err != nil {
				return fmt.Errorf("determining servers path: %w", err)
			}

			if err := config.AddServer(path, name, host, user, role, ""); err != nil {
				return fmt.Errorf("adding server: %w", err)
			}

			roleLabel := role
			if roleLabel == "" {
				roleLabel = "app"
			}
			fmt.Printf("Added server %q (%s@%s, role=%s)\n", name, user, host, roleLabel)
			return nil
		},
	}

	cmd.Flags().StringVar(&role, "role", "", "server role: app or lb (default: app)")
	cmd.Flags().StringVar(&user, "user", "", "SSH user (default: root)")
	cmd.Flags().StringVar(&key, "key", "", "path to SSH private key")

	return cmd
}

func newServerRemoveCmd(flags *Flags) *cobra.Command {
	return &cobra.Command{
		Use:   "remove <name>",
		Short: "Remove a server from servers.yml",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]

			path, err := config.DefaultServersPath()
			if err != nil {
				return fmt.Errorf("determining servers path: %w", err)
			}

			if err := config.RemoveServer(path, name); err != nil {
				return fmt.Errorf("removing server: %w", err)
			}

			fmt.Printf("Removed server %q\n", name)
			return nil
		},
	}
}

func newServerListCmd(flags *Flags) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List all configured servers",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runServerList(flags, cmd.OutOrStdout())
		},
	}
}

func runServerList(flags *Flags, out io.Writer) error {
	path, err := config.DefaultServersPath()
	if err != nil {
		return fmt.Errorf("determining servers path: %w", err)
	}

	servers, err := config.ListServers(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("listing servers: %w", err)
		}
		servers = map[string]config.Server{}
	}
	return writeServerList(out, servers, flags.JSON)
}

func writeServerList(out io.Writer, servers map[string]config.Server, jsonOutput bool) error {
	if jsonOutput {
		if servers == nil {
			servers = map[string]config.Server{}
		}
		return json.NewEncoder(out).Encode(servers)
	}
	if len(servers) == 0 {
		fmt.Fprintln(out, "No servers configured. Use 'teploy server add' to add one.")
		return nil
	}

	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	sort.Strings(names)

	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tHOST\tUSER\tROLE")
	for _, name := range names {
		srv := servers[name]
		role := srv.Role
		if role == "" {
			role = "app"
		}
		user := srv.User
		if user == "" {
			user = "root"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", name, srv.Host, user, role)
	}
	return w.Flush()
}
