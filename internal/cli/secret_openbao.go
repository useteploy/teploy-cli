package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"

	"github.com/spf13/cobra"
	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/ssh"
	"github.com/useteploy/teploy/internal/openbao"
)

// addOpenbaoSecretCommands attaches the managed-provider (OpenBao) operations
// to the `teploy secret` command. The universal set/get/list live on the parent
// (provider-aware); these are the OpenBao-specific lifecycle + advanced ops
// (provision, status, multi-field put, dynamic DB creds, audit shipping).
func addOpenbaoSecretCommands(cmd *cobra.Command, flags *Flags, provider *string) {
	cmd.AddCommand(newVaultSetupCmd(flags))
	cmd.AddCommand(newVaultStatusCmd(flags))
	cmd.AddCommand(newVaultPutCmd(flags))
	cmd.AddCommand(newVaultDBCmd(flags))
	cmd.AddCommand(newSecretAuditCmd(flags))
}


func newVaultDBCmd(flags *Flags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "db",
		Short: "Dynamic database credentials (short-lived, auto-revoked)",
	}
	cmd.AddCommand(newVaultDBSetupCmd(flags))
	cmd.AddCommand(newVaultDBCredsCmd(flags))
	cmd.AddCommand(newVaultDBStaticRoleCmd(flags))
	cmd.AddCommand(newVaultDBStaticCredsCmd(flags))
	return cmd
}

func newVaultDBStaticRoleCmd(flags *Flags) *cobra.Command {
	var accessory, dbAccessory, dbName, adminUser, username, rotation string
	cmd := &cobra.Command{
		Use:   "static-role",
		Short: "Manage an existing DB user's password with scheduled rotation",
		Long: `Registers a static role: OpenBao takes over an EXISTING database user's
password and rotates it every --rotation-period (vs dynamic creds, which mint
a new user per request). Use for a fixed app account whose password should
rotate automatically. Idempotent.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			adminPass, _ := cmd.Flags().GetString("admin-pass")
			return runOpenbaoKV(flags, accessory, func(ctx context.Context, c *openbao.Client, app string) error {
				if adminPass == "" {
					return fmt.Errorf("--admin-pass is required (the DB accessory's superuser password)")
				}
				if err := c.EnableStaticRole(ctx, openbao.StaticRoleOptions{
					App: app, Accessory: resolveVaultAccessory(accessory),
					DBAccessory: dbAccessory, DBName: dbName, AdminUser: adminUser,
					AdminPass: adminPass, Username: username, RotationPeriod: rotation,
				}); err != nil {
					return err
				}
				fmt.Printf("Static role enabled for %s: %s rotates every %s\n", app, username, rotation)
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&accessory, "accessory", "openbao", "OpenBao accessory name")
	cmd.Flags().StringVar(&dbAccessory, "db-accessory", "postgres", "the database accessory")
	cmd.Flags().StringVar(&dbName, "db-name", "", "logical database name (default: the db accessory name)")
	cmd.Flags().StringVar(&adminUser, "admin-user", "postgres", "DB superuser OpenBao uses to rotate")
	cmd.Flags().String("admin-pass", "", "DB superuser password")
	cmd.Flags().StringVar(&username, "username", "", "the existing DB user to manage (required)")
	cmd.Flags().StringVar(&rotation, "rotation-period", "24h", "how often to rotate the password")
	return cmd
}

func newVaultDBStaticCredsCmd(flags *Flags) *cobra.Command {
	var accessory, username string
	cmd := &cobra.Command{
		Use:   "static-creds",
		Short: "Read the current rotating credentials for a static role",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runOpenbaoKV(flags, accessory, func(ctx context.Context, c *openbao.Client, app string) error {
				if username == "" {
					return fmt.Errorf("--username is required")
				}
				creds, err := c.StaticCreds(ctx, app, resolveVaultAccessory(accessory), username)
				if err != nil {
					return err
				}
				fmt.Printf("username=%v\npassword=%v\nttl=%v\n", creds["username"], creds["password"], creds["ttl"])
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&accessory, "accessory", "openbao", "OpenBao accessory name")
	cmd.Flags().StringVar(&username, "username", "", "the DB user (required)")
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
			return runOpenbaoKV(flags, accessory, func(ctx context.Context, c *openbao.Client, app string) error {
				if adminPass == "" {
					return fmt.Errorf("--admin-pass is required (the DB accessory's superuser password)")
				}
				if err := c.EnableDatabaseSecrets(ctx, openbao.DBSetupOptions{
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
			return runOpenbaoKV(flags, accessory, func(ctx context.Context, c *openbao.Client, app string) error {
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

	client := openbao.NewClient(executor, os.Stdout)
	if err := client.Setup(ctx, openbao.SetupOptions{App: appCfg.App, Accessory: accessory, Image: image}); err != nil {
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
			return runOpenbaoKV(flags, accessory, func(ctx context.Context, c *openbao.Client, app string) error {
				return c.Put(ctx, app, resolveVaultAccessory(accessory), args[0], args[1:])
			})
		},
	}
	cmd.Flags().StringVar(&accessory, "accessory", "openbao", "accessory/container name for OpenBao")
	return cmd
}



// mergeSecretVaultRefs resolves any `vault:<name>#<key>` references in the app's env:
// block from OpenBao and merges them into dst (the deploy secrets map). A no-op
// when the app has no vault references, so it's safe to call on every deploy.
// Uses the given executor (works for both the single- and multi-server paths).
func mergeSecretVaultRefs(ctx context.Context, exec ssh.Executor, appCfg *config.AppConfig, dst map[string]string) error {
	if len(openbao.CollectRefs(appCfg.Env)) == 0 {
		return nil
	}
	resolved, err := openbao.NewClient(exec, os.Stderr).ResolveEnvRefs(ctx, appCfg.App, appCfg.Secret.Accessory, appCfg.Env)
	if err != nil {
		return fmt.Errorf("resolving vault references: %w", err)
	}
	for k, v := range resolved {
		dst[k] = v
	}
	return nil
}

// ensureSecretAgent, when the app enables the OpenBao Agent sidecar
// (secret.provider openbao + agent), , (re)deploys the agent and mounts the shared secrets
// volume into the app so it can read /vault/secrets/db.env (auto-rotating
// dynamic credentials). Returns the (possibly newly-allocated) volumes map. A
// no-op when the agent is disabled.
func ensureSecretAgent(ctx context.Context, exec ssh.Executor, appCfg *config.AppConfig, volumes map[string]string) (map[string]string, error) {
	if !appCfg.Secret.Agent {
		return volumes, nil
	}
	client := openbao.NewClient(exec, os.Stderr)
	if err := client.DeployAgent(ctx, appCfg.App, appCfg.Secret.Accessory, []openbao.AgentTemplate{openbao.DBEnvTemplate(appCfg.App)}); err != nil {
		return nil, fmt.Errorf("deploying vault agent: %w", err)
	}
	if volumes == nil {
		volumes = make(map[string]string, 1)
	}
	// Named volume shared with the agent; docker accepts a volume name as a
	// mount source. Mounted read-only into the app — it only consumes secrets.
	volumes[openbao.SecretsVolume(appCfg.App)] = openbao.AgentMountPath + ":ro"
	return volumes, nil
}

// runOpenbaoKV loads the app, connects, and runs fn with a vault client.
func runOpenbaoKV(flags *Flags, accessory string, fn func(ctx context.Context, c *openbao.Client, app string) error) error {
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

	return fn(ctx, openbao.NewClient(executor, os.Stdout), appCfg.App)
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

	st, err := openbao.NewClient(executor, os.Stdout).Status(ctx, appCfg.App, accessory)
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
