package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"

	"github.com/spf13/cobra"
	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/ssh"
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
	cmd.AddCommand(newVaultDBCmd(flags))
	cmd.AddCommand(newVaultAuditShipCmd(flags))
	return cmd
}

func newVaultAuditShipCmd(flags *Flags) *cobra.Command {
	var accessory string
	cmd := &cobra.Command{
		Use:   "audit-ship",
		Short: "Forward OpenBao secret-access audit events into the observe tamper-evident trail",
		Long: `Reads new OpenBao audit-log entries and forwards each secret/database access
into the teploy-observe tamper-evident audit trail (using the app's audit:
{ endpoint, token } config). Idempotent — tracks how many entries were already
shipped. Run on a schedule (cron / systemd timer) to stream access continuously.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runVaultKV(flags, accessory, func(ctx context.Context, c *vault.Client, app string) error {
				appCfg, _ := config.LoadApp(".")
				n, err := c.ShipAudit(ctx, app, resolveVaultAccessory(accessory),
					appCfg.Audit.Endpoint, appCfg.Audit.Token, appCfg.Audit.Site)
				if err != nil {
					return err
				}
				fmt.Printf("Shipped %d audit event(s) to observe\n", n)
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&accessory, "accessory", "openbao", "OpenBao accessory name")
	return cmd
}

func newVaultDBCmd(flags *Flags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "db",
		Short: "Dynamic database credentials (short-lived, auto-revoked)",
	}
	cmd.AddCommand(newVaultDBSetupCmd(flags))
	cmd.AddCommand(newVaultDBCredsCmd(flags))
	return cmd
}

func newVaultDBSetupCmd(flags *Flags) *cobra.Command {
	var accessory, dbAccessory, dbName, adminUser, ttl, maxTTL string
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Wire OpenBao to issue short-lived credentials for a database accessory",
		Long: `Configures OpenBao's database secrets engine against a Postgres accessory so
apps get short-lived, auto-revoked credentials instead of a static password.
The admin password is read from the accessory's stored credentials unless
--admin-pass is given. Extends the app's AppRole policy to read the dynamic
creds. Idempotent.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			adminPass, _ := cmd.Flags().GetString("admin-pass")
			return runVaultKV(flags, accessory, func(ctx context.Context, c *vault.Client, app string) error {
				if adminPass == "" {
					return fmt.Errorf("--admin-pass is required (the DB accessory's superuser password)")
				}
				if err := c.EnableDatabaseSecrets(ctx, vault.DBSetupOptions{
					App: app, Accessory: resolveVaultAccessory(accessory),
					DBAccessory: dbAccessory, DBName: dbName, AdminUser: adminUser,
					AdminPass: adminPass, TTL: ttl, MaxTTL: maxTTL,
				}); err != nil {
					return err
				}
				fmt.Printf("Dynamic DB credentials enabled for %s (db accessory: %s, TTL: %s)\n", app, dbAccessory, ttl)
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&accessory, "accessory", "openbao", "OpenBao accessory name")
	cmd.Flags().StringVar(&dbAccessory, "db-accessory", "postgres", "the database accessory to issue creds for")
	cmd.Flags().StringVar(&dbName, "db-name", "", "logical database name (default: the db accessory name)")
	cmd.Flags().StringVar(&adminUser, "admin-user", "postgres", "DB superuser OpenBao uses to create roles")
	cmd.Flags().String("admin-pass", "", "DB superuser password")
	cmd.Flags().StringVar(&ttl, "ttl", "1h", "default credential lease TTL")
	cmd.Flags().StringVar(&maxTTL, "max-ttl", "24h", "maximum credential lease TTL")
	return cmd
}

func newVaultDBCredsCmd(flags *Flags) *cobra.Command {
	var accessory string
	cmd := &cobra.Command{
		Use:   "creds",
		Short: "Request a fresh set of dynamic database credentials",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runVaultKV(flags, accessory, func(ctx context.Context, c *vault.Client, app string) error {
				creds, err := c.DBCreds(ctx, app, resolveVaultAccessory(accessory))
				if err != nil {
					return err
				}
				fmt.Printf("username=%v\npassword=%v\n", creds["username"], creds["password"])
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&accessory, "accessory", "openbao", "OpenBao accessory name")
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

// mergeVaultRefs resolves any `vault:<name>#<key>` references in the app's env:
// block from OpenBao and merges them into dst (the deploy secrets map). A no-op
// when the app has no vault references, so it's safe to call on every deploy.
// Uses the given executor (works for both the single- and multi-server paths).
func mergeVaultRefs(ctx context.Context, exec ssh.Executor, appCfg *config.AppConfig, dst map[string]string) error {
	if len(vault.CollectRefs(appCfg.Env)) == 0 {
		return nil
	}
	resolved, err := vault.NewClient(exec, os.Stderr).ResolveEnvRefs(ctx, appCfg.App, appCfg.Vault.Accessory, appCfg.Env)
	if err != nil {
		return fmt.Errorf("resolving vault references: %w", err)
	}
	for k, v := range resolved {
		dst[k] = v
	}
	return nil
}

// ensureVaultAgent, when the app enables the OpenBao Agent sidecar
// (vault.agent: true), (re)deploys the agent and mounts the shared secrets
// volume into the app so it can read /vault/secrets/db.env (auto-rotating
// dynamic credentials). Returns the (possibly newly-allocated) volumes map. A
// no-op when the agent is disabled.
func ensureVaultAgent(ctx context.Context, exec ssh.Executor, appCfg *config.AppConfig, volumes map[string]string) (map[string]string, error) {
	if !appCfg.Vault.Agent {
		return volumes, nil
	}
	client := vault.NewClient(exec, os.Stderr)
	if err := client.DeployAgent(ctx, appCfg.App, appCfg.Vault.Accessory, []vault.AgentTemplate{vault.DBEnvTemplate(appCfg.App)}); err != nil {
		return nil, fmt.Errorf("deploying vault agent: %w", err)
	}
	if volumes == nil {
		volumes = make(map[string]string, 1)
	}
	// Named volume shared with the agent; docker accepts a volume name as a
	// mount source. Mounted read-only into the app — it only consumes secrets.
	volumes[vault.SecretsVolume(appCfg.App)] = vault.AgentMountPath + ":ro"
	return volumes, nil
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
