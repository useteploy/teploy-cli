package cli

// teploy kv — operator surface for a shared Nucleus KV store
// (HashiCorp-parity plan #3: the Consul-KV analog, trusted-domain MVP).
//
// Apps talk to the shared store directly over pgwire (neutron-go KV model)
// using their DATABASE_URL; this command gives operators and deploy scripts
// the same access from the workstation, via the accessory container's own
// `nucleus shell -c` (no client tooling required on the server, auth
// inherited from the container's env).
//
// TRUST BOUNDARY (from the parity plan, locked): Nucleus KV is ONE global
// keyspace with no server-enforced namespaces. Key prefixes ("flags/...",
// "<app>/...") are hygiene, not isolation — any app with the DATABASE_URL
// can read or flush everything. Share a store only between mutually-trusted
// apps; real isolation is per-app instances.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strings"

	"github.com/spf13/cobra"
	"github.com/useteploy/teploy/internal/accessories"
	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/docker"
	"github.com/useteploy/teploy/internal/ssh"
)

// kvDestination mirrors the accessory command's -d overlay flag.
var kvDestination string

func newKvCmd(flags *Flags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "kv",
		Short: "Read/write the shared Nucleus KV store (config, flags)",
		Long: "Operate on a Nucleus accessory's KV store — shared config, feature flags,\n" +
			"counters — from the workstation. Runs `nucleus shell -c` inside the accessory\n" +
			"container, so it needs no client tooling on the server and inherits the\n" +
			"container's auth environment.\n\n" +
			"The store is one global keyspace with no server-side namespaces: key prefixes\n" +
			"(\"flags/...\", \"<app>/...\") are convention, not isolation. Share a store only\n" +
			"between mutually-trusted apps.\n\n" +
			"Examples:\n" +
			"  teploy kv set flags/beta on\n" +
			"  teploy kv get flags/beta\n" +
			"  teploy kv list 'flags/*'\n" +
			"  teploy kv incr deploys/count\n" +
			"  teploy kv del flags/beta",
	}

	cmd.PersistentFlags().StringVarP(&kvDestination, "destination", "d", "", "destination overlay (e.g. prod merges teploy.prod.yml)")

	cmd.AddCommand(newKvGetCmd(flags))
	cmd.AddCommand(newKvSetCmd(flags))
	cmd.AddCommand(newKvDelCmd(flags))
	cmd.AddCommand(newKvExistsCmd(flags))
	cmd.AddCommand(newKvIncrCmd(flags))
	cmd.AddCommand(newKvListCmd(flags))

	return cmd
}

// kvTarget carries the per-subcommand targeting flags.
type kvTarget struct {
	appName   string
	accessory string
}

func addKvTargetFlags(cmd *cobra.Command, t *kvTarget) {
	cmd.Flags().StringVar(&t.appName, "app", "", "app name — act on server state instead of teploy.yml (requires --host)")
	cmd.Flags().StringVar(&t.accessory, "accessory", "nucleus", "name of the Nucleus accessory holding the store")
}

func newKvGetCmd(flags *Flags) *cobra.Command {
	t := &kvTarget{}
	cmd := &cobra.Command{
		Use:   "get <key>",
		Short: "Print a key's value (exit 1 if unset)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			val, err := runKvQuery(flags, t, fmt.Sprintf("SELECT KV_GET(%s)", kvQuote(args[0])))
			if err != nil {
				return err
			}
			if val == "" || val == "NULL" {
				return fmt.Errorf("key %q is not set", args[0])
			}
			fmt.Println(val)
			return nil
		},
	}
	addKvTargetFlags(cmd, t)
	return cmd
}

func newKvSetCmd(flags *Flags) *cobra.Command {
	t := &kvTarget{}
	var ttl int64
	cmd := &cobra.Command{
		Use:   "set <key> <value>",
		Short: "Set a key to a value (optionally with a TTL)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			sql := fmt.Sprintf("SELECT KV_SET(%s, %s)", kvQuote(args[0]), kvQuote(args[1]))
			if ttl > 0 {
				sql = fmt.Sprintf("SELECT KV_SET(%s, %s, %d)", kvQuote(args[0]), kvQuote(args[1]), ttl)
			}
			if _, err := runKvQuery(flags, t, sql); err != nil {
				return err
			}
			fmt.Printf("%s = %s\n", args[0], args[1])
			return nil
		},
	}
	cmd.Flags().Int64Var(&ttl, "ttl", 0, "expiry in seconds (0 = no expiry)")
	addKvTargetFlags(cmd, t)
	return cmd
}

func newKvDelCmd(flags *Flags) *cobra.Command {
	t := &kvTarget{}
	cmd := &cobra.Command{
		Use:   "del <key>",
		Short: "Delete a key",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, err := runKvQuery(flags, t, fmt.Sprintf("SELECT KV_DEL(%s)", kvQuote(args[0]))); err != nil {
				return err
			}
			fmt.Printf("deleted %s\n", args[0])
			return nil
		},
	}
	addKvTargetFlags(cmd, t)
	return cmd
}

func newKvExistsCmd(flags *Flags) *cobra.Command {
	t := &kvTarget{}
	cmd := &cobra.Command{
		Use:   "exists <key>",
		Short: "Exit 0 if the key exists, 1 otherwise",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			val, err := runKvQuery(flags, t, fmt.Sprintf("SELECT KV_EXISTS(%s)", kvQuote(args[0])))
			if err != nil {
				return err
			}
			if !kvTruthy(val) {
				return fmt.Errorf("key %q does not exist", args[0])
			}
			fmt.Println("true")
			return nil
		},
	}
	addKvTargetFlags(cmd, t)
	return cmd
}

func newKvIncrCmd(flags *Flags) *cobra.Command {
	t := &kvTarget{}
	var by int64
	cmd := &cobra.Command{
		Use:   "incr <key>",
		Short: "Atomically increment a counter, print the new value",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sql := fmt.Sprintf("SELECT KV_INCR(%s)", kvQuote(args[0]))
			if by != 1 {
				sql = fmt.Sprintf("SELECT KV_INCR(%s, %d)", kvQuote(args[0]), by)
			}
			val, err := runKvQuery(flags, t, sql)
			if err != nil {
				return err
			}
			fmt.Println(val)
			return nil
		},
	}
	cmd.Flags().Int64Var(&by, "by", 1, "increment amount")
	addKvTargetFlags(cmd, t)
	return cmd
}

func newKvListCmd(flags *Flags) *cobra.Command {
	t := &kvTarget{}
	cmd := &cobra.Command{
		Use:   "list [pattern]",
		Short: "List keys matching a glob pattern (default '*')",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pattern := "*"
			if len(args) == 1 {
				pattern = args[0]
			}
			val, err := runKvQuery(flags, t, fmt.Sprintf("SELECT KV_KEYS(%s)", kvQuote(pattern)))
			if err != nil {
				return err
			}
			// KV_KEYS returns a JSON array of key names.
			var keys []string
			if err := json.Unmarshal([]byte(val), &keys); err != nil {
				return fmt.Errorf("unexpected KV_KEYS response %q: %w", val, err)
			}
			for _, k := range keys {
				fmt.Println(k)
			}
			return nil
		},
	}
	addKvTargetFlags(cmd, t)
	return cmd
}

// runKvQuery executes one KV_* statement inside the Nucleus accessory
// container via `nucleus shell -c --json` and returns the single result
// value (first column of the first row).
func runKvQuery(flags *Flags, t *kvTarget, sql string) (string, error) {
	if err := config.ValidateIdentifier("accessory", t.accessory); err != nil {
		return "", err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	appCfg, executor, err := resolveAppForKv(ctx, flags, t.appName)
	if err != nil {
		return "", err
	}
	defer executor.Close()

	dk := docker.NewClient(executor)
	container := accessories.ContainerName(appCfg.App, t.accessory)
	command := fmt.Sprintf("nucleus shell -c %s --json", shellSingleQuote(sql))
	output, err := dk.Exec(ctx, container, command)
	if err != nil {
		msg := strings.TrimSpace(output)
		if msg != "" {
			return "", fmt.Errorf("kv query failed in %s: %s", container, msg)
		}
		return "", fmt.Errorf("kv query failed in %s: %w", container, err)
	}
	return kvFirstValue(output)
}

// resolveAppForKv mirrors resolveAppForAccessory but honors the kv command's
// own -d overlay variable.
func resolveAppForKv(ctx context.Context, flags *Flags, appName string) (*config.AppConfig, ssh.Executor, error) {
	if appName != "" {
		return resolveApp(ctx, flags, appName)
	}
	var appCfg *config.AppConfig
	var err error
	if kvDestination != "" {
		appCfg, err = config.LoadAppWithDestination(".", kvDestination)
	} else {
		appCfg, err = config.LoadApp(".")
	}
	if err != nil {
		return nil, nil, err
	}
	ex, err := connectForApp(ctx, flags, appCfg)
	if err != nil {
		return nil, nil, err
	}
	return appCfg, ex, nil
}

// kvFirstValue extracts the single result value from `nucleus shell --json`
// output: a JSON array of row objects; we want the first value of the first
// row regardless of the column's name (Nucleus's column naming varies
// between friendly aliases and raw expressions).
func kvFirstValue(output string) (string, error) {
	trimmed := strings.TrimSpace(output)
	// The shell may print warnings before the JSON line; use the last line.
	lines := strings.Split(trimmed, "\n")
	last := strings.TrimSpace(lines[len(lines)-1])
	var rows []map[string]any
	if err := json.Unmarshal([]byte(last), &rows); err != nil {
		return "", fmt.Errorf("unexpected shell response %q", trimmed)
	}
	if len(rows) == 0 {
		return "", nil
	}
	for _, v := range rows[0] {
		return fmt.Sprintf("%v", v), nil
	}
	return "", nil
}

// kvQuote renders a SQL single-quoted string literal (quotes doubled).
func kvQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// kvTruthy interprets Nucleus boolean text renderings.
func kvTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "t", "1", "on", "yes":
		return true
	}
	return false
}

// shellSingleQuote quotes for the container's `sh -c` layer (docker.Exec
// already quotes the OUTER remote-shell layer; this protects the inner one).
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}
