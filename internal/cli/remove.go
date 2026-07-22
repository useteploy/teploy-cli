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
	"github.com/useteploy/teploy/internal/caddy"
	"github.com/useteploy/teploy/internal/docker"
	"github.com/useteploy/teploy/internal/ssh"
)

func newRemoveCmd(flags *Flags) *cobra.Command {
	var appName string
	var yes, purge bool
	var redirect string
	cmd := &cobra.Command{
		Use:     "remove",
		Aliases: []string{"destroy"},
		Short:   "Retire an app: containers, proxy route, and deploy state",
		Long: `Remove is the inverse of deploy. It stops and removes the app's containers,
removes its Caddy route (reloading the proxy), and deletes its deploy state
under /deployments/<app>.

Data is preserved by default: volumes/, accessory data, and running accessory
containers are left in place so a removal is never silently destructive.
Pass --purge to delete those too (irreversible).

Pass --redirect <url> to leave a permanent redirect behind for the app's
domains. The redirect is written as a plain, unmanaged Caddy block (no TEPLOY
markers), so later teploy operations never touch it.

Every step is idempotent — re-running after a partial failure is safe.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRemove(flags, appName, yes, purge, redirect)
		},
	}
	cmd.Flags().StringVar(&appName, "app", "", "app name — act on server state instead of teploy.yml (requires --host)")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip the confirmation prompt")
	cmd.Flags().BoolVar(&purge, "purge", false, "also delete volumes, accessory data, and accessory containers (irreversible)")
	cmd.Flags().StringVar(&redirect, "redirect", "", "leave a permanent redirect to this URL for the app's domains")
	return cmd
}

type removeOptions struct {
	Purge    bool
	Redirect string
}

type removeSummary struct {
	App               string   `json:"app"`
	Server            string   `json:"server"`
	RemovedContainers []string `json:"removed_containers"`
	KeptAccessories   []string `json:"kept_accessories,omitempty"`
	Route             string   `json:"route"` // "removed", "redirected", "none"
	PreservedData     []string `json:"preserved_data,omitempty"`
}

func runRemove(flags *Flags, appName string, yes, purge bool, redirect string) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	if redirect != "" && !strings.HasPrefix(redirect, "http://") && !strings.HasPrefix(redirect, "https://") {
		return fmt.Errorf("--redirect must be a full URL (https://...), got %q", redirect)
	}

	appCfg, executor, err := resolveApp(ctx, flags, appName)
	if err != nil {
		return err
	}
	defer executor.Close()

	domains := splitDomains(appCfg.Domain)
	if redirect != "" && len(domains) == 0 {
		return fmt.Errorf("app %q has no domains recorded in its state — cannot write a redirect", appCfg.App)
	}

	if !yes {
		fmt.Printf("Remove %s from %s:\n", appCfg.App, executor.Host())
		fmt.Println("  - stop and remove its containers")
		if redirect != "" {
			fmt.Printf("  - replace its route with a permanent redirect to %s\n", redirect)
		} else {
			fmt.Println("  - remove its Caddy route")
		}
		if purge {
			fmt.Println("  - DELETE volumes, accessory data, and accessory containers (--purge)")
		} else {
			fmt.Println("  - delete deploy state (volumes and accessories are preserved)")
		}
		if !confirm(os.Stdout, "Proceed?") {
			return fmt.Errorf("aborted")
		}
	}

	out := io.Writer(os.Stdout)
	if flags.JSON {
		out = io.Discard
	}
	summary, err := executeRemove(ctx, executor, appCfg.App, domains, removeOptions{Purge: purge, Redirect: redirect}, out)
	if err != nil {
		return err
	}

	if flags.JSON {
		return json.NewEncoder(os.Stdout).Encode(summary)
	}
	fmt.Fprintf(out, "Removed %s from %s\n", summary.App, summary.Server)
	return nil
}

// splitDomains parses the comma-separated domain list stored in app state
// ("a.com, www.a.com") into individual hosts.
func splitDomains(domain string) []string {
	var out []string
	for _, d := range strings.Split(domain, ",") {
		if d = strings.TrimSpace(d); d != "" {
			out = append(out, d)
		}
	}
	return out
}

// executeRemove tears down one app on one server: containers, route, state.
// Split from runRemove so it can be tested against a MockExecutor. Steps are
// ordered containers -> route -> state so a mid-way failure leaves state on
// disk and a re-run picks up where it stopped.
func executeRemove(ctx context.Context, exec ssh.Executor, app string, domains []string, opts removeOptions, out io.Writer) (*removeSummary, error) {
	summary := &removeSummary{App: app, Server: exec.Host(), RemovedContainers: []string{}}
	dk := docker.NewClient(exec)

	// 1. Containers. Accessories (databases, caches) hold data — leave them
	// running unless --purge.
	containers, err := dk.ListContainers(ctx, app)
	if err != nil {
		return nil, fmt.Errorf("listing containers: %w", err)
	}
	for _, c := range containers {
		if c.Labels["teploy.role"] == "accessory" && !opts.Purge {
			summary.KeptAccessories = append(summary.KeptAccessories, c.Name)
			continue
		}
		fmt.Fprintf(out, "Removing container %s...\n", c.Name)
		if c.State == "running" {
			if err := dk.Stop(ctx, c.Name, 10); err != nil {
				return nil, err
			}
		}
		if err := dk.Remove(ctx, c.Name); err != nil {
			return nil, err
		}
		summary.RemovedContainers = append(summary.RemovedContainers, c.Name)
	}

	// 2. Route. Servers running only ingress:host apps have no Caddyfile —
	// nothing to unwire there. A failure here aborts before state deletion so
	// a re-run can retry the edit.
	cdy := caddy.NewClient(exec)
	switch {
	case !cdy.HasCaddyfile(ctx):
		summary.Route = "none"
	case opts.Redirect != "":
		fmt.Fprintf(out, "Writing redirect %s -> %s...\n", strings.Join(domains, ", "), opts.Redirect)
		if err := cdy.AppendRedirect(ctx, app, domains, opts.Redirect); err != nil {
			return nil, fmt.Errorf("writing redirect: %w", err)
		}
		summary.Route = "redirected"
	default:
		fmt.Fprintln(out, "Removing Caddy route...")
		if err := cdy.RemoveRoute(ctx, app); err != nil {
			return nil, fmt.Errorf("removing route: %w", err)
		}
		summary.Route = "removed"
	}

	// 3. State. Default keeps volumes/ and accessories/ (the data); --purge
	// removes the whole tree.
	appDir := "/deployments/" + app
	if opts.Purge {
		fmt.Fprintln(out, "Purging deploy state and data...")
		if _, err := exec.Run(ctx, "rm -rf "+ssh.ShellQuote(appDir)); err != nil {
			return nil, fmt.Errorf("purging %s: %w", appDir, err)
		}
	} else {
		fmt.Fprintln(out, "Removing deploy state (preserving volumes and accessory data)...")
		exec.Run(ctx, "find "+ssh.ShellQuote(appDir)+" -mindepth 1 -maxdepth 1 ! -name volumes ! -name accessories -exec rm -rf {} + 2>/dev/null")
		exec.Run(ctx, "rmdir "+ssh.ShellQuote(appDir)+" 2>/dev/null || true")
		if left, err := exec.Run(ctx, "ls "+ssh.ShellQuote(appDir)+" 2>/dev/null"); err == nil {
			for _, d := range strings.Fields(left) {
				summary.PreservedData = append(summary.PreservedData, appDir+"/"+d)
			}
		}
	}

	for _, kept := range summary.KeptAccessories {
		fmt.Fprintf(out, "Kept accessory container %s (use --purge to remove)\n", kept)
	}
	for _, p := range summary.PreservedData {
		fmt.Fprintf(out, "Preserved %s\n", p)
	}
	return summary, nil
}
