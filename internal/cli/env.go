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
	"github.com/useteploy/teploy/internal/env"
)

func newEnvCmd(flags *Flags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "env",
		Short: "Manage environment variables",
	}

	cmd.AddCommand(newEnvSetCmd(flags))
	cmd.AddCommand(newEnvGetCmd(flags))
	cmd.AddCommand(newEnvListCmd(flags))
	cmd.AddCommand(newEnvUnsetCmd(flags))

	return cmd
}

func newEnvSetCmd(flags *Flags) *cobra.Command {
	var appName string
	var valueStdin bool
	cmd := &cobra.Command{
		Use:   "set KEY=value [KEY=value...] | KEY --stdin",
		Short: "Set one or more environment variables",
		Long: "Set one or more environment variables.\n\n" +
			"With --stdin, pass the KEY alone as the argument and pipe the value in;\n" +
			"stdin is taken verbatim (no trailing-newline trim). This keeps secret\n" +
			"values out of the process list, where argv is visible to every local\n" +
			"process. Only one KEY can be set per --stdin invocation.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pairs, err := parseEnvSetArgs(cmd.InOrStdin(), args, valueStdin)
			if err != nil {
				return err
			}
			return runEnvSet(flags, appName, pairs)
		},
	}
	cmd.Flags().StringVar(&appName, "app", "", "app name — act on server state instead of teploy.yml (requires --host)")
	cmd.Flags().BoolVar(&valueStdin, "stdin", false, "read the value from stdin instead of argv (pass the KEY alone as the argument; stdin is verbatim — use for secret values)")
	return cmd
}

// parseEnvSetArgs builds the KEY->value pairs for `env set`. With valueStdin
// set, the single argument must be a bare KEY and its value is read verbatim
// from r (audit UPSTREAM-1: secret values must be able to avoid argv).
func parseEnvSetArgs(r io.Reader, args []string, valueStdin bool) (map[string]string, error) {
	if valueStdin {
		if len(args) != 1 {
			return nil, fmt.Errorf("--stdin takes exactly one KEY argument, got %d", len(args))
		}
		if strings.Contains(args[0], "=") {
			return nil, fmt.Errorf("--stdin takes a bare KEY (got %q); pass the value on stdin", args[0])
		}
		val, err := readSecretValue(r)
		if err != nil {
			return nil, err
		}
		return map[string]string{args[0]: val}, nil
	}
	pairs := make(map[string]string)
	for _, arg := range args {
		idx := strings.IndexByte(arg, '=')
		if idx < 0 {
			return nil, fmt.Errorf("invalid format: %q (expected KEY=value)", arg)
		}
		pairs[arg[:idx]] = arg[idx+1:]
	}
	return pairs, nil
}

func runEnvSet(flags *Flags, appName string, pairs map[string]string) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	appCfg, executor, err := resolveApp(ctx, flags, appName)
	if err != nil {
		return err
	}
	defer executor.Close()

	mgr := env.NewManager(executor)
	if err := mgr.Set(ctx, appCfg.App, pairs); err != nil {
		return err
	}

	for k := range pairs {
		fmt.Printf("  Set %s\n", k)
	}
	return nil
}

func newEnvGetCmd(flags *Flags) *cobra.Command {
	var appName string
	cmd := &cobra.Command{
		Use:   "get KEY",
		Short: "Get the value of an environment variable",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runEnvGet(flags, appName, args[0])
		},
	}
	cmd.Flags().StringVar(&appName, "app", "", "app name — act on server state instead of teploy.yml (requires --host)")
	return cmd
}

func runEnvGet(flags *Flags, appName, key string) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	appCfg, executor, err := resolveApp(ctx, flags, appName)
	if err != nil {
		return err
	}
	defer executor.Close()

	mgr := env.NewManager(executor)
	val, err := mgr.Get(ctx, appCfg.App, key)
	if err != nil {
		return err
	}

	fmt.Println(val)
	return nil
}

func newEnvListCmd(flags *Flags) *cobra.Command {
	var reveal bool

	var appName string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List all environment variables",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runEnvList(flags, appName, reveal, cmd.OutOrStdout())
		},
	}

	cmd.Flags().BoolVar(&reveal, "reveal", false, "show values instead of masking them")
	cmd.Flags().StringVar(&appName, "app", "", "app name — act on server state instead of teploy.yml (requires --host)")

	return cmd
}

func runEnvList(flags *Flags, appName string, reveal bool, out io.Writer) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	appCfg, executor, err := resolveApp(ctx, flags, appName)
	if err != nil {
		return err
	}
	defer executor.Close()

	mgr := env.NewManager(executor)
	entries, err := mgr.List(ctx, appCfg.App)
	if err != nil {
		return err
	}

	return writeEnvList(out, entries, reveal, flags.JSON)
}

func writeEnvList(out io.Writer, entries []env.Entry, reveal, jsonOutput bool) error {
	if jsonOutput {
		result := make([]env.Entry, len(entries))
		copy(result, entries)
		if !reveal {
			for i := range result {
				result[i].Value = "***"
			}
		}
		return json.NewEncoder(out).Encode(result)
	}

	if len(entries) == 0 {
		fmt.Fprintln(out, "No environment variables set")
		return nil
	}

	for _, e := range entries {
		if reveal {
			fmt.Fprintf(out, "%s=%s\n", e.Key, e.Value)
		} else {
			fmt.Fprintf(out, "%s=***\n", e.Key)
		}
	}
	return nil
}

func newEnvUnsetCmd(flags *Flags) *cobra.Command {
	var appName string
	cmd := &cobra.Command{
		Use:   "unset KEY",
		Short: "Remove an environment variable",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runEnvUnset(flags, appName, args[0])
		},
	}
	cmd.Flags().StringVar(&appName, "app", "", "app name — act on server state instead of teploy.yml (requires --host)")
	return cmd
}

func runEnvUnset(flags *Flags, appName, key string) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	appCfg, executor, err := resolveApp(ctx, flags, appName)
	if err != nil {
		return err
	}
	defer executor.Close()

	mgr := env.NewManager(executor)
	if err := mgr.Unset(ctx, appCfg.App, key); err != nil {
		return err
	}

	fmt.Printf("  Unset %s\n", key)
	return nil
}
