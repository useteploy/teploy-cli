package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/notify"
	"github.com/useteploy/teploy/internal/preview"
)

func newPreviewCmd(flags *Flags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "preview",
		Short: "Manage preview environments",
	}

	cmd.AddCommand(newPreviewDeployCmd(flags))
	cmd.AddCommand(newPreviewListCmd(flags))
	cmd.AddCommand(newPreviewDestroyCmd(flags))
	cmd.AddCommand(newPreviewPruneCmd(flags))

	return cmd
}

// previewDeployOpts are the parsed `preview deploy` flags. The exposure
// fields are tri-state: a flag not given leaves its field unset (nil) so an
// update inherits the preview's recorded mode instead of resetting it.
type previewDeployOpts struct {
	ttl        string
	image      string
	baseDomain string
	httpOnly   *bool
	allowIPs   []string
}

func newPreviewDeployCmd(flags *Flags) *cobra.Command {
	return newPreviewDeployCmdWith(func(branch string, opts previewDeployOpts) error {
		return runPreviewDeploy(flags, branch, opts)
	})
}

// newPreviewDeployCmdWith builds the command around run (the test seam:
// flag parsing and validation are the real ones).
func newPreviewDeployCmdWith(run func(branch string, opts previewDeployOpts) error) *cobra.Command {
	var opts previewDeployOpts
	var httpOnly bool
	var allowIPs []string

	cmd := &cobra.Command{
		Use:   "deploy <branch>",
		Short: "Deploy a preview environment for a branch",
		Long: `Deploy a preview environment on a temporary <branch>.<domain> subdomain.

Runs an image that already exists on the server; it does not build one and
does not check out a branch. Use "teploy build" for that — it builds without
touching production, which "teploy deploy" cannot do.

Example:
  git checkout feat/new-landing
  teploy build --json          # prints the image tag
  teploy preview deploy feat-new-landing --ttl 24h --image <tag>

Tailnet-only preview (plain HTTP, reachable only from Tailscale addresses):
  teploy preview deploy feat-new-landing --image <tag> \
    --base-domain 100-64-1-2.sslip.io --http-only --allow-ip 100.64.0.0/10

--base-domain, --http-only and --allow-ip are recorded with the preview;
a later deploy of the same branch keeps them unless it passes them again
(--http-only=false turns HTTP-only off, --allow-ip "" clears the list).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := finishPreviewDeployOpts(cmd, &opts, httpOnly, allowIPs); err != nil {
				return err
			}
			return run(args[0], opts)
		},
	}

	cmd.Flags().StringVar(&opts.ttl, "ttl", "72h", "time-to-live before auto-expiry")
	cmd.Flags().StringVar(&opts.image, "image", "", "image to run (default: teploy.yml's image, else <app>-build-<git hash>)")
	cmd.Flags().StringVar(&opts.baseDomain, "base-domain", "", "hostname base instead of the app domain (e.g. 100-64-1-2.sslip.io)")
	cmd.Flags().BoolVar(&httpOnly, "http-only", false, "serve the preview over plain HTTP (no certificate)")
	cmd.Flags().StringSliceVar(&allowIPs, "allow-ip", nil, "only this IP/CIDR may reach the preview (repeatable)")

	return cmd
}

// finishPreviewDeployOpts turns the raw exposure flags into opts, keeping
// "not given" distinct from "given as false/empty", and validates them
// before anything connects.
func finishPreviewDeployOpts(cmd *cobra.Command, opts *previewDeployOpts, httpOnly bool, allowIPs []string) error {
	opts.baseDomain = strings.ToLower(strings.TrimSpace(opts.baseDomain))
	if cmd.Flags().Changed("base-domain") {
		if err := preview.ValidateBaseDomain(opts.baseDomain); err != nil {
			return err
		}
	}
	opts.httpOnly = nil
	if cmd.Flags().Changed("http-only") {
		v := httpOnly
		opts.httpOnly = &v
	}
	opts.allowIPs = nil
	if cmd.Flags().Changed("allow-ip") {
		opts.allowIPs = []string{}
		for _, ip := range allowIPs {
			if ip = strings.TrimSpace(ip); ip != "" {
				opts.allowIPs = append(opts.allowIPs, ip)
			}
		}
		if err := preview.ValidateAllowIPs(opts.allowIPs); err != nil {
			return err
		}
	}
	return nil
}

func runPreviewDeploy(flags *Flags, branch string, opts previewDeployOpts) error {
	ttlStr, image := opts.ttl, opts.image
	appCfg, err := config.LoadApp(".")
	if err != nil {
		return err
	}

	// Preview environments need Teploy-managed Caddy to provision a
	// branch.app.example.com subdomain route on demand. With external
	// ingress (CF Tunnel, etc.), preview hostnames have to be added at
	// the external layer — out of scope for Teploy. Reject explicitly so
	// the failure mode is "useful error" not "silently broken preview".
	if !appCfg.UsesCaddy() {
		return fmt.Errorf("'teploy preview' requires Teploy-managed Caddy; this app uses ingress: %s — add the preview hostname route at your external ingress and deploy the branch with -d instead", appCfg.Ingress)
	}

	ttl, err := time.ParseDuration(ttlStr)
	if err != nil {
		return fmt.Errorf("invalid TTL: %w", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	executor, err := connectForApp(ctx, flags, appCfg)
	if err != nil {
		return err
	}
	defer executor.Close()

	version, err := gitShortHash()
	if err != nil {
		return fmt.Errorf("could not determine version from git: %w", err)
	}

	// Which image the preview runs. An explicit --image is what `teploy build`
	// prints, passed straight through: without it, build and preview agree only
	// because both happen to re-derive the same `<app>-build-<git hash>` from
	// the same working directory, which silently produces "image not found" the
	// moment they are run from different checkouts or at different commits.
	if image == "" {
		image = appCfg.Image
	}
	if image == "" {
		image = appCfg.App + "-build-" + version
	}

	// Repo identity recorded in the preview record (provenance + legacy
	// disambiguation). Not part of the preview ID — see gitRepoIdentity.
	repo := gitRepoIdentity(".")

	mgr := preview.NewManager(executor, os.Stdout)

	// Prune expired previews for this app before deploying a new one
	// (the same shared prune core `teploy preview prune` runs across all
	// apps — Manager.Prune). Teploy deliberately has no server-side
	// agent/daemon (see CLAUDE.md), so nothing else enforces preview TTLs
	// on its own — ExpiresAt was being written but never checked by
	// anything, letting expired containers and Caddy routes leak
	// indefinitely for an app nobody deploys new previews for. The
	// standalone `preview prune` (PruneAll) is the cron-able enforcement
	// point; this piggyback keeps an actively-used app clean between runs.
	// Best-effort: a prune failure shouldn't block the actual deploy the
	// operator asked for.
	if pruned, err := mgr.Prune(ctx, appCfg.App); err != nil {
		fmt.Printf("Warning: pruning expired previews: %v\n", err)
	} else if pruned > 0 {
		fmt.Printf("Pruned %d expired preview(s)\n", pruned)
	}

	err = mgr.Deploy(ctx, preview.DeployConfig{
		App:     appCfg.App,
		Domain:  appCfg.Domain,
		Branch:  branch,
		Image:   image,
		Version: version,
		TTL:     ttl,
		Repo:    repo,

		BaseDomain: opts.baseDomain,
		HTTPOnly:   opts.httpOnly,
		AllowIPs:   opts.allowIPs,
	})

	if n := buildNotifier(appCfg); n != nil {
		msg := fmt.Sprintf("Preview %s deployed for %s", branch, appCfg.App)
		if err != nil {
			msg = fmt.Sprintf("Preview %s failed for %s: %s", branch, appCfg.App, err)
		}
		n.Send(ctx, notify.Payload{
			App:     appCfg.App,
			Server:  executor.Host(),
			Type:    "preview",
			Success: err == nil,
			Hash:    version,
			Message: msg,
		})
	}

	return err
}

func newPreviewListCmd(flags *Flags) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List active previews",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPreviewList(flags)
		},
	}
}

func runPreviewList(flags *Flags) error {
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

	mgr := preview.NewManager(executor, os.Stdout)
	previews, err := mgr.List(ctx, appCfg.App)
	if err != nil {
		return err
	}
	if flags.JSON {
		return json.NewEncoder(os.Stdout).Encode(previewListRows(previews))
	}

	if len(previews) == 0 {
		fmt.Println("No active previews")
		return nil
	}

	for _, p := range previews {
		expired := ""
		if time.Now().UTC().After(p.ExpiresAt) {
			expired = " (expired)"
		}
		fmt.Printf("  %s → %s%s\n", p.Branch, p.URL(), expired)
		fmt.Printf("    Container: %s  Port: %d  Expires: %s\n",
			p.Container, p.Port, p.ExpiresAt.Format(time.RFC3339))
	}
	return nil
}

// previewListRow is one `preview list --json` row: the preview record
// plus its url, carrying the scheme the route actually serves (http:// for
// an HTTP-only preview).
type previewListRow struct {
	preview.State
	URL string `json:"url"`
}

// previewListRows is the `preview list --json` encoder input: never null.
func previewListRows(previews []preview.State) []previewListRow {
	rows := make([]previewListRow, 0, len(previews))
	for _, p := range previews {
		rows = append(rows, previewListRow{State: p, URL: p.URL()})
	}
	return rows
}

func newPreviewDestroyCmd(flags *Flags) *cobra.Command {
	return &cobra.Command{
		Use:   "destroy <branch>",
		Short: "Tear down a preview environment",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPreviewDestroy(flags, args[0])
		},
	}
}

func runPreviewDestroy(flags *Flags, branch string) error {
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

	mgr := preview.NewManager(executor, os.Stdout)
	return mgr.Destroy(ctx, appCfg.App, branch)
}

func newPreviewPruneCmd(flags *Flags) *cobra.Command {
	return &cobra.Command{
		Use:   "prune",
		Short: "Remove expired previews",
		Long: "Remove expired previews across ALL apps on the target server,\n" +
			"enumerating every app's preview records (both the current\n" +
			"canonical and the legacy slug-keyed era).\n\n" +
			"`teploy preview deploy` prunes that app's expired previews\n" +
			"automatically before deploying, but teploy has no server-side\n" +
			"agent/daemon — nothing enforces TTLs on a schedule by itself.\n" +
			"This command is the standalone enforcement point: run it by hand\n" +
			"or from cron to tear down expired previews even for apps nobody\n" +
			"is actively deploying. It connects to the server named by the\n" +
			"teploy.yml in the current directory and never touches anything\n" +
			"outside /deployments/<app>/previews and the artifacts those\n" +
			"records name.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPreviewPrune(flags)
		},
	}
}

func runPreviewPrune(flags *Flags) error {
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

	mgr := preview.NewManager(executor, os.Stdout)
	// PruneAll — the same shared prune core the deploy piggyback uses
	// (Manager.Prune), driven across every app on this server.
	n, err := mgr.PruneAll(ctx)
	if err != nil {
		return err
	}
	if n == 0 {
		fmt.Println("No expired previews to prune")
	} else {
		fmt.Printf("Pruned %d expired preview(s)\n", n)
	}
	return nil
}

// gitRepoIdentity returns the trivially normalized origin remote URL of the
// checkout at dir, or "" when it cannot be resolved (no git repo, no
// origin remote). The value is recorded in preview records as repo
// provenance and is compared when adopting legacy records; it is
// deliberately NOT hashed into the canonical preview ID — remote URLs
// change on repo renames and protocol switches, and keying identity on
// them would silently orphan every existing preview.
func gitRepoIdentity(dir string) string {
	out, err := exec.Command("git", "-C", dir, "config", "--get", "remote.origin.url").Output()
	if err != nil {
		return ""
	}
	return normalizeRepoURL(string(out))
}

// normalizeRepoURL applies teploy's trivial repo-URL normalization: strip
// surrounding whitespace and a trailing ".git", strip the scheme and any
// user:token@ credentials, and rewrite the scp-like form to host/path —
// so https://git@github.com/o/r.git, git@github.com:o/r.git and
// ssh://git@github.com/o/r all record as github.com/o/r. This collapses
// the common spellings of one remote; anything else is recorded verbatim.
func normalizeRepoURL(raw string) string {
	s := strings.TrimSuffix(strings.TrimSpace(raw), ".git")
	if s == "" {
		return ""
	}
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
		if slash := strings.IndexByte(s, '/'); slash >= 0 {
			authority, path := s[:slash], s[slash:]
			if at := strings.LastIndexByte(authority, '@'); at >= 0 {
				authority = authority[at+1:]
			}
			s = authority + path
		} else if at := strings.LastIndexByte(s, '@'); at >= 0 {
			s = s[at+1:]
		}
		return s
	}
	// scp-like form: [user@]host:path — the first colon before any slash
	// separates host from path.
	if i := strings.IndexByte(s, ':'); i > 0 && !strings.Contains(s[:i], "/") {
		host := s[:i]
		if at := strings.LastIndexByte(host, '@'); at >= 0 {
			host = host[at+1:]
		}
		return host + "/" + s[i+1:]
	}
	return s
}
