package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"

	"github.com/spf13/cobra"
	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/vault"
)

func newVaultCmd(flags *Flags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "vault",
		Short: "Manage the OpenBao secrets manager accessory",
		Long: `Provision and operate OpenBao (a Vault-compatible secrets manager) as a
first-class Teploy accessory: deployed on the private network, auto-unsealed,
and wired for per-app least-privilege secret access.`,
	}
	cmd.AddCommand(newVaultSetupCmd(flags))
	cmd.AddCommand(newVaultStatusCmd(flags))
	cmd.AddCommand(newVaultPutCmd(flags))
	cmd.AddCommand(newVaultGetCmd(flags))
	cmd.AddCommand(newVaultListCmd(flags))
	return cmd
}

func newVaultSetupCmd(flags *Flags) *cobra.Command {
	var accessory, image string
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Provision + initialize OpenBao for this app (idempotent)",
		Long: `Deploys OpenBao on the private teploy network, initializes it, and
auto-unseals it — no manual unseal ceremony. The seal key, root token, and
recovery keys are stored in the app's encrypted secret store. Safe to re-run.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runVaultSetup(flags, accessory, image)
		},
	}
	cmd.Flags().StringVar(&accessory, "accessory", "openbao", "accessory/container name for OpenBao")
	cmd.Flags().StringVar(&image, "image", "", "OpenBao image (default openbao/openbao:latest)")
	return cmd
}

func newVaultStatusCmd(flags *Flags) *cobra.Command {
	var accessory string
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show OpenBao seal + init status",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runVaultStatus(flags, accessory)
		},
	}
	cmd.Flags().StringVar(&accessory, "accessory", "openbao", "accessory/container name for OpenBao")
	return cmd
}

func runVaultSetup(flags *Flags, accessory, image string) error {
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

	client := vault.NewClient(executor, os.Stdout)
	if err := client.Setup(ctx, vault.SetupOptions{App: appCfg.App, Accessory: accessory, Image: image}); err != nil {
		return err
	}
	// Provision the per-app least-privilege AppRole so deploys can fetch secrets
	// with a scoped identity (not the root token).
	if _, err := client.EnsureAppRole(ctx, appCfg.App, resolveVaultAccessory(accessory)); err != nil {
		return fmt.Errorf("provisioning app AppRole: %w", err)
	}
	fmt.Println("Provisioned least-privilege AppRole for", appCfg.App)
	return nil
}

// resolveVaultAccessory mirrors the client's default so command wiring can pass
// a concrete accessory name.
func resolveVaultAccessory(accessory string) string {
	if accessory == "" {
		return "openbao"
	}
	return accessory
}

func newVaultPutCmd(flags *Flags) *cobra.Command {
	var accessory string
	cmd := &cobra.Command{
		Use:   "put <name> <key=value>...",
		Short: "Write secret values under secret/<app>/<name>",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runVaultKV(flags, accessory, func(ctx context.Context, c *vault.Client, app string) error {
				return c.Put(ctx, app, resolveVaultAccessory(accessory), args[0], args[1:])
			})
		},
	}
	cmd.Flags().StringVar(&accessory, "accessory", "openbao", "accessory/container name for OpenBao")
	return cmd
}

func newVaultGetCmd(flags *Flags) *cobra.Command {
	var accessory string
	cmd := &cobra.Command{
		Use:   "get <name>",
		Short: "Read secret values at secret/<app>/<name>",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runVaultKV(flags, accessory, func(ctx context.Context, c *vault.Client, app string) error {
				data, err := c.Get(ctx, app, resolveVaultAccessory(accessory), args[0])
				if err != nil {
					return err
				}
				for k, v := range data {
					fmt.Printf("%s=%v\n", k, v)
				}
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&accessory, "accessory", "openbao", "accessory/container name for OpenBao")
	return cmd
}

func newVaultListCmd(flags *Flags) *cobra.Command {
	var accessory string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List secret names under secret/<app>/",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runVaultKV(flags, accessory, func(ctx context.Context, c *vault.Client, app string) error {
				names, err := c.List(ctx, app, resolveVaultAccessory(accessory))
				if err != nil {
					return err
				}
				if len(names) == 0 {
					fmt.Println("No secrets stored")
					return nil
				}
				for _, n := range names {
					fmt.Println(n)
				}
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&accessory, "accessory", "openbao", "accessory/container name for OpenBao")
	return cmd
}

// runVaultKV loads the app, connects, and runs fn with a vault client.
func runVaultKV(flags *Flags, accessory string, fn func(ctx context.Context, c *vault.Client, app string) error) error {
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

	return fn(ctx, vault.NewClient(executor, os.Stdout), appCfg.App)
}

func runVaultStatus(flags *Flags, accessory string) error {
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

	st, err := vault.NewClient(executor, os.Stdout).Status(ctx, appCfg.App, accessory)
	if err != nil {
		return err
	}
	sealed := "unsealed"
	if st.Sealed {
		sealed = "SEALED"
	}
	fmt.Printf("OpenBao %s  seal=%s  initialized=%t  %s\n", st.Version, st.SealType, st.Initialized, sealed)
	return nil
}
