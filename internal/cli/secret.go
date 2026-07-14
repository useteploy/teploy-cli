package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"

	"github.com/spf13/cobra"
	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/openbao"
	"github.com/useteploy/teploy/internal/secret"
)

// Secret providers. "local" is the built-in age-encrypted store; "openbao" is
// the managed OpenBao provider (mirrors network's provider model).
const (
	providerLocal   = "local"
	providerOpenbao = "openbao"
)

func newSecretCmd(flags *Flags) *cobra.Command {
	var provider string
	cmd := &cobra.Command{
		Use:   "secret",
		Short: "Manage secrets (local age store or a managed provider like OpenBao)",
		Long: `Manage application secrets. By default secrets are stored in the built-in
age-encrypted store (--provider local). Set --provider openbao (or
secret.provider: openbao in teploy.yml) to use a managed OpenBao secrets
manager — provisioned, auto-unsealed, with dynamic credentials and a
tamper-evident audit trail.`,
	}
	cmd.PersistentFlags().StringVar(&provider, "provider", "", "secrets provider: local (default) or openbao")

	cmd.AddCommand(newSecretSetCmd(flags, &provider))
	cmd.AddCommand(newSecretGetCmd(flags, &provider))
	cmd.AddCommand(newSecretListCmd(flags, &provider))
	cmd.AddCommand(newSecretRotateCmd(flags))
	// Managed-provider (OpenBao) operations live under `secret` too.
	addOpenbaoSecretCommands(cmd, flags, &provider)
	return cmd
}

// resolveProvider picks the effective provider: the --provider flag wins, then
// teploy.yml secret.provider, else local.
func resolveProvider(providerFlag string, appCfg *config.AppConfig) string {
	if providerFlag != "" {
		return providerFlag
	}
	if appCfg.Secret.Provider != "" {
		return appCfg.Secret.Provider
	}
	return providerLocal
}

func newSecretSetCmd(flags *Flags, provider *string) *cobra.Command {
	return &cobra.Command{
		Use:   "set KEY=value [KEY=value...]",
		Short: "Set one or more secrets",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pairs := make(map[string]string)
			for _, arg := range args {
				idx := strings.IndexByte(arg, '=')
				if idx < 0 {
					return fmt.Errorf("invalid format: %q (expected KEY=value)", arg)
				}
				pairs[arg[:idx]] = arg[idx+1:]
			}
			return runSecretSet(flags, *provider, pairs)
		},
	}
}

func runSecretSet(flags *Flags, providerFlag string, pairs map[string]string) error {
	appCfg, err := config.LoadApp(".")
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	executor, err := connectForApp(ctx, flags, appCfg)
	if err != nil {
		return err
	}
	defer executor.Close()

	if resolveProvider(providerFlag, appCfg) == providerOpenbao {
		client := openbao.NewClient(executor, os.Stdout)
		for k, v := range pairs {
			// Flat KEY=value maps to secret/<app>/<KEY> with a single "value"
			// field — identical UX to the local store.
			if err := client.Put(ctx, appCfg.App, appCfg.Secret.Accessory, k, []string{"value=" + v}); err != nil {
				return err
			}
			fmt.Printf("  Set secret %s (openbao)\n", k)
		}
		return nil
	}

	mgr := secret.NewManager(executor)
	for k, v := range pairs {
		if err := mgr.Set(ctx, appCfg.App, k, v); err != nil {
			return err
		}
		fmt.Printf("  Set secret %s\n", k)
	}
	return nil
}

func newSecretGetCmd(flags *Flags, provider *string) *cobra.Command {
	return &cobra.Command{
		Use:   "get KEY",
		Short: "Decrypt and display a secret",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSecretGet(flags, *provider, args[0])
		},
	}
}

func runSecretGet(flags *Flags, providerFlag, key string) error {
	appCfg, err := config.LoadApp(".")
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	executor, err := connectForApp(ctx, flags, appCfg)
	if err != nil {
		return err
	}
	defer executor.Close()

	if resolveProvider(providerFlag, appCfg) == providerOpenbao {
		data, err := openbao.NewClient(executor, os.Stdout).Get(ctx, appCfg.App, appCfg.Secret.Accessory, key)
		if err != nil {
			return err
		}
		// Print the sole "value" field flat when that's all there is; otherwise
		// print all fields (multi-field secrets created via `secret put`).
		if v, ok := data["value"]; ok && len(data) == 1 {
			fmt.Println(v)
			return nil
		}
		for k, v := range data {
			fmt.Printf("%s=%v\n", k, v)
		}
		return nil
	}

	val, err := secret.NewManager(executor).Get(ctx, appCfg.App, key)
	if err != nil {
		return err
	}
	fmt.Println(val)
	return nil
}

func newSecretListCmd(flags *Flags, provider *string) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List secret keys (values masked)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSecretList(flags, *provider)
		},
	}
}

func runSecretList(flags *Flags, providerFlag string) error {
	appCfg, err := config.LoadApp(".")
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	executor, err := connectForApp(ctx, flags, appCfg)
	if err != nil {
		return err
	}
	defer executor.Close()

	var keys []string
	if resolveProvider(providerFlag, appCfg) == providerOpenbao {
		keys, err = openbao.NewClient(executor, os.Stdout).List(ctx, appCfg.App, appCfg.Secret.Accessory)
	} else {
		keys, err = secret.NewManager(executor).List(ctx, appCfg.App)
	}
	if err != nil {
		return err
	}
	if len(keys) == 0 {
		fmt.Println("No secrets set")
		return nil
	}
	for _, k := range keys {
		fmt.Printf("%s=***\n", k)
	}
	return nil
}

func newSecretRotateCmd(flags *Flags) *cobra.Command {
	return &cobra.Command{
		Use:   "rotate KEY",
		Short: "Generate a new random value for a local secret",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSecretRotate(flags, args[0])
		},
	}
}

func runSecretRotate(flags *Flags, key string) error {
	appCfg, err := config.LoadApp(".")
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	executor, err := connectForApp(ctx, flags, appCfg)
	if err != nil {
		return err
	}
	defer executor.Close()

	mgr := secret.NewManager(executor)
	newVal, err := mgr.Rotate(ctx, appCfg.App, key)
	if err != nil {
		return err
	}
	fmt.Printf("  Rotated %s\n", key)
	fmt.Printf("  New value: %s\n", newVal)
	fmt.Println("  (Restart containers to apply)")
	return nil
}
