package cli

// JIT scoped mesh access (network grant/grants/revoke): time-boxed pre-auth
// keys that auto-revoke. See internal/network/jit.go for the control-plane
// clients and the scope model (tags + your mesh ACL decide reach; teploy
// mints and revokes keys, never edits ACL policy).

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/network"
)

// resolveGrantClient picks the provider (flag > teploy.yml network block)
// and builds the control-plane client.
func resolveGrantClient(providerFlag string) (network.GrantClient, string, error) {
	provider := providerFlag
	server := ""
	if appCfg, err := config.LoadApp("."); err == nil {
		if provider == "" {
			provider = appCfg.Network.Provider
		}
		server = appCfg.Network.Server
	}
	if provider == "" {
		return nil, "", fmt.Errorf("no network provider configured — set network.provider in teploy.yml or pass --provider")
	}
	client, err := network.NewGrantClient(provider, serverURLFor(provider, server))
	return client, provider, err
}

// serverURLFor: only headscale consumes the config server URL; tailscale
// always talks to the public API.
func serverURLFor(provider, server string) string {
	if provider == "headscale" {
		return server
	}
	return ""
}

func newNetworkGrantCmd(flags *Flags) *cobra.Command {
	var (
		provider string
		ttl      time.Duration
		tags     []string
		reusable bool
	)
	cmd := &cobra.Command{
		Use:   "grant",
		Short: "Mint a time-boxed mesh access key (auto-revokes)",
		Long: "Create a pre-auth key that expires on its own — \"give the contractor two\n" +
			"hours of access\". The node it enrolls is ephemeral (drops off the mesh when it\n" +
			"disconnects) and carries the tags you set; what those tags can reach is decided\n" +
			"by your tailnet/headscale ACL policy, which teploy never edits.\n\n" +
			"Credentials: TAILSCALE_API_KEY (+ optional TAILSCALE_TAILNET), or\n" +
			"HEADSCALE_API_KEY + HEADSCALE_USER (+ network.server / HEADSCALE_URL).\n\n" +
			"Examples:\n" +
			"  teploy network grant --ttl 2h --tag tag:contractor\n" +
			"  teploy network grant --ttl 30m --reusable   # team onboarding window",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
			defer cancel()
			client, prov, err := resolveGrantClient(provider)
			if err != nil {
				return err
			}
			if ttl <= 0 || ttl > 90*24*time.Hour {
				return fmt.Errorf("--ttl must be positive and at most 90 days (got %s)", ttl)
			}
			grant, err := client.CreateGrant(ctx, ttl, tags, reusable)
			if err != nil {
				return err
			}
			fmt.Printf("Grant created (%s), expires %s\n", prov, grant.Expires.Local().Format(time.RFC1123))
			if len(grant.Tags) > 0 {
				fmt.Printf("Tags: %s (reach is governed by your ACL policy)\n", strings.Join(grant.Tags, ", "))
			}
			fmt.Printf("\n  %s\n\n", grant.Key)
			fmt.Println("Hand this to the grantee; they join with:")
			switch prov {
			case "headscale":
				fmt.Println("  tailscale up --login-server <headscale-url> --auth-key <key>")
			default:
				fmt.Println("  tailscale up --auth-key <key>")
			}
			fmt.Println("The key expires automatically; revoke early with: teploy network revoke", grant.ID)
			return nil
		},
	}
	cmd.Flags().StringVar(&provider, "provider", "", "mesh provider (tailscale, headscale); default from teploy.yml")
	cmd.Flags().DurationVar(&ttl, "ttl", time.Hour, "grant lifetime (e.g. 30m, 2h, 24h)")
	cmd.Flags().StringArrayVar(&tags, "tag", nil, "ACL tag(s) for the enrolled node (e.g. tag:contractor); repeatable")
	cmd.Flags().BoolVar(&reusable, "reusable", false, "allow multiple devices to enroll with this key")
	return cmd
}

func newNetworkGrantsCmd(flags *Flags) *cobra.Command {
	var provider string
	cmd := &cobra.Command{
		Use:   "grants",
		Short: "List active mesh access keys",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
			defer cancel()
			client, providerName, err := resolveGrantClient(provider)
			if err != nil {
				return err
			}
			grants, err := client.ListGrants(ctx)
			if err != nil {
				return err
			}
			if flags.JSON {
				type grantDTO struct {
					ID        string    `json:"id"`
					Provider  string    `json:"provider"`
					ExpiresAt time.Time `json:"expires_at"`
					Tags      []string  `json:"tags"`
					Ephemeral bool      `json:"ephemeral"`
					Used      bool      `json:"used"`
					Status    string    `json:"status"`
				}
				now := time.Now()
				result := make([]grantDTO, 0, len(grants))
				for _, grant := range grants {
					status := "active"
					if !grant.Expires.IsZero() && grant.Expires.Before(now) {
						status = "expired"
					}
					tags := grant.Tags
					if tags == nil {
						tags = []string{}
					}
					result = append(result, grantDTO{ID: grant.ID, Provider: providerName, ExpiresAt: grant.Expires, Tags: tags, Ephemeral: grant.Ephemeral, Used: grant.Used, Status: status})
				}
				return json.NewEncoder(os.Stdout).Encode(result)
			}
			if len(grants) == 0 {
				fmt.Println("No pre-auth keys.")
				return nil
			}
			now := time.Now()
			for _, g := range grants {
				state := "active"
				if !g.Expires.IsZero() && g.Expires.Before(now) {
					state = "expired"
				}
				line := fmt.Sprintf("%-12s %-8s expires %s", g.ID, state, g.Expires.Local().Format(time.RFC1123))
				if len(g.Tags) > 0 {
					line += "  [" + strings.Join(g.Tags, ",") + "]"
				}
				if g.Used {
					line += "  (used)"
				}
				fmt.Println(line)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&provider, "provider", "", "mesh provider (tailscale, headscale); default from teploy.yml")
	return cmd
}

func newNetworkRevokeCmd(flags *Flags) *cobra.Command {
	var provider string
	cmd := &cobra.Command{
		Use:   "revoke <key-id>",
		Short: "Revoke a mesh access key before it expires",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
			defer cancel()
			client, _, err := resolveGrantClient(provider)
			if err != nil {
				return err
			}
			if err := client.RevokeGrant(ctx, args[0]); err != nil {
				return err
			}
			fmt.Printf("Revoked %s. Ephemeral nodes it enrolled drop off when they disconnect;\n", args[0])
			fmt.Println("to cut an active session immediately, also remove the device in your mesh admin.")
			return nil
		},
	}
	cmd.Flags().StringVar(&provider, "provider", "", "mesh provider (tailscale, headscale); default from teploy.yml")
	return cmd
}
