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
	return client.Setup(ctx, vault.SetupOptions{App: appCfg.App, Accessory: accessory, Image: image})
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
