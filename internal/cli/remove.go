package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/useteploy/teploy/internal/events"
	"io"
	"os"
	"os/signal"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"github.com/useteploy/teploy/internal/build"
	"github.com/useteploy/teploy/internal/caddy"
	"github.com/useteploy/teploy/internal/confighome"
	"github.com/useteploy/teploy/internal/docker"
	"github.com/useteploy/teploy/internal/gitsource"
	"github.com/useteploy/teploy/internal/ssh"
	"github.com/useteploy/teploy/internal/state"
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
removes its Caddy route (reloading the proxy), deletes the server-built images
those containers ran (<app>-build-<version>; pulled images are left alone),
and deletes its deploy state under /deployments/<app>.

The removal itself is recorded in the app's event history (who removed it,
and whether --purge was used). With --purge, that record is all that stays,
so the server history can still answer what happened to the app.

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
	RemovedImages     []string `json:"removed_images,omitempty"`
	KeptAccessories   []string `json:"kept_accessories,omitempty"`
	RemovedAutomation []string `json:"removed_automation,omitempty"`
	Route             string   `json:"route"` // "removed", "redirected", "none"
	PreservedData     []string `json:"preserved_data,omitempty"`
	KeptConfig        bool     `json:"kept_config,omitempty"`
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
	recordAppEvent(ctx, executor, appCfg.App, events.KindRemove, err, map[string]any{"purge": purge, "redirect": redirect})
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
	appDir := "/deployments/" + app

	// 0. Serialize with deploys, heal and the listener under the app's
	// fenced lock, and retire everything that could act on the app later
	// (webhook listener, scheduled redeploy, heal and audit timers) before
	// any container goes. Without this a listener delivery or a heal pass
	// could re-create containers mid-removal, and a scheduled-redeploy cron
	// kept firing against a deleted script forever.
	var lk *state.Lock
	if _, err := exec.Run(ctx, "[ -d "+ssh.ShellQuote(appDir)+" ]"); err == nil {
		lk, err = state.AcquireLockFenced(ctx, exec, app)
		if err != nil {
			return nil, fmt.Errorf("acquiring deploy lock: %w", err)
		}
		defer func() {
			state.ReleaseLockFenced(exec, lk, app)
			exec.Run(ctx, "rmdir "+ssh.ShellQuote(appDir)+" 2>/dev/null || true")
		}()
	}
	removed, err := removeAppAutomation(ctx, exec, app, opts.Purge)
	if err != nil {
		return nil, err
	}
	summary.RemovedAutomation = removed
	for _, r := range removed {
		fmt.Fprintf(out, "Removed %s\n", r)
	}

	// 1. Containers. Accessories (databases, caches) hold data — leave them
	// running unless --purge.
	containers, err := dk.ListContainers(ctx, app)
	if err != nil {
		return nil, fmt.Errorf("listing containers: %w", err)
	}
	// Containers created by image ID report the ID; resolve it to the tags
	// so their build images are found too.
	containers = dk.ResolveImageTags(ctx, containers)
	var buildImages []string
	for _, c := range containers {
		if c.Labels["teploy.role"] == "accessory" && !opts.Purge {
			summary.KeptAccessories = append(summary.KeptAccessories, c.Name)
			continue
		}
		fmt.Fprintf(out, "Removing container %s...\n", c.Name)
		// A crash-looping process reports "restarting", and `docker rm` refuses
		// it just as it refuses "running" — stop both.
		if c.State == "running" || c.State == "restarting" {
			if err := dk.Stop(ctx, c.Name, 10); err != nil {
				return nil, err
			}
		}
		if err := dk.Remove(ctx, c.Name); err != nil {
			return nil, err
		}
		summary.RemovedContainers = append(summary.RemovedContainers, c.Name)
		tags := c.ImageTags
		if len(tags) == 0 {
			tags = []string{c.Image}
		}
		for _, tag := range tags {
			if strings.HasPrefix(tag, build.ImageTag(app, "")) && !slices.Contains(buildImages, tag) {
				buildImages = append(buildImages, tag)
			}
		}
	}
	summary.RemovedImages = removeBuildImages(ctx, exec, buildImages)
	for _, img := range summary.RemovedImages {
		fmt.Fprintf(out, "Removed image %s\n", img)
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
	// removes the whole tree. The held lock (.lock) goes last, on release.
	if err := lk.Check(ctx, exec); err != nil {
		return nil, err
	}
	serverHome, _, err := confighome.Head(ctx, exec, app)
	if err != nil {
		return nil, err
	}
	if opts.Purge {
		fmt.Fprintln(out, "Purging deploy state and data...")
		if _, err := exec.Run(ctx, "rm -rf "+ssh.ShellQuote(appDir)); err != nil {
			// Accessory data is owned by whatever uid the engine ran as (Nucleus:
			// 10001), which a non-root deploy user cannot delete. Finish the job
			// as root inside a throwaway container, the same way the directories
			// were handed over on start — no host sudo assumed.
			if _, cErr := exec.Run(ctx, "docker run --rm --user 0 -v "+ssh.ShellQuote(appDir+":/teploy-data")+
				" alpine:3 sh -c 'rm -rf /teploy-data/* /teploy-data/.[!.]*'"); cErr != nil {
				return nil, fmt.Errorf("purging %s: %w", appDir, err)
			}
			if _, rmErr := exec.Run(ctx, "rmdir "+ssh.ShellQuote(appDir)); rmErr != nil {
				return nil, fmt.Errorf("purging %s: %w", appDir, rmErr)
			}
		}
	} else {
		fmt.Fprintln(out, "Removing deploy state (preserving volumes, accessory data, and credentials)...")
		// The keep-set preserves everything a retained DATABASE still needs
		// to be usable again: volumes/ (the data), accessories/ (engine
		// state), secrets/ (the encrypted secret store + age material),
		// and .env. The old two-directory allowlist deleted the credential
		// material while keeping the data — a remove-then-redeploy could
		// never reconnect to the retained database (TCL-56). Hidden files
		// (.maintenance-block etc.) are matched by find itself; the
		// exclusion names must be bare, not dotted paths, to match .env.
		keepArgs := ""
		keeps := []string{"volumes", "accessories", "secrets", ".env", ".lock"}
		if serverHome {
			// The tombstone below replaces the state and continues its
			// generation, so the fences stay monotonic across a re-create.
			keeps = append(keeps, "state.json", ".generation")
		}
		for _, keep := range keeps {
			keepArgs += " ! -name " + ssh.ShellQuote(keep)
		}
		delCmd := fmt.Sprintf("find %s -mindepth 1 -maxdepth 1%s -exec rm -rf {} +",
			ssh.ShellQuote(appDir), keepArgs)
		if _, err := exec.Run(ctx, delCmd); err != nil {
			return nil, fmt.Errorf("removing deploy state under %s: %w", appDir, err)
		}
		if left, err := exec.Run(ctx, "ls "+ssh.ShellQuote(appDir)+" 2>/dev/null"); err == nil {
			for _, d := range strings.Fields(left) {
				if serverHome && d == "state.json" {
					continue
				}
				summary.PreservedData = append(summary.PreservedData, appDir+"/"+d)
			}
		}
	}

	// 4. Config home (spec §3.1). A Teploy-home app keeps its config and
	// history and becomes a v3 tombstone (not deployed, still marked, so an
	// older CLI cannot re-create it from a stale folder); --purge deletes
	// its server data and the app returns to folder home.
	switch {
	case serverHome && opts.Purge:
		fmt.Fprintln(out, "Deleting its Teploy config and history...")
		if _, err := exec.Run(ctx, "rm -rf -- "+ssh.ShellQuote(confighome.Dir(app))); err != nil {
			return nil, fmt.Errorf("deleting %s: %w", confighome.Dir(app), err)
		}
		// The git credential lives outside /deployments (D4); forget it
		// with everything else. Best-effort: a missing helper or age is
		// not a reason to keep a half-removed app.
		if err := gitsource.Remove(ctx, exec, app); err != nil {
			fmt.Fprintf(out, "Warning: could not remove %s's git credential: %v\n", app, err)
		}
	case serverHome && lk != nil:
		if err := state.WriteTombstone(ctx, exec, app, lk); err != nil {
			return nil, fmt.Errorf("recording %s as not deployed: %w", app, err)
		}
		summary.KeptConfig = true
		fmt.Fprintf(out, "Kept its Teploy config (redeploy with `teploy deploy --app %s`; --purge deletes it)\n", app)
	}

	for _, kept := range summary.KeptAccessories {
		fmt.Fprintf(out, "Kept accessory container %s (use --purge to remove)\n", kept)
	}
	for _, p := range summary.PreservedData {
		fmt.Fprintf(out, "Preserved %s\n", p)
	}
	return summary, nil
}

// removeAppAutomation disables and deletes the app's systemd units (webhook
// listener, heal and audit timers) and its scheduled-redeploy cron entry in
// one server round trip. Backup schedules are removed only with purge: the
// default remove keeps the data they back up. Returns what it removed;
// absent units and an empty crontab are not errors.
func removeAppAutomation(ctx context.Context, exec ssh.Executor, app string, purge bool) ([]string, error) {
	sudo := sudoPrefixFor(ctx, exec)
	units := []string{
		"teploy-webhook-" + app + ".service",
		healTimerName(app), healServiceName(app),
		secretAuditTimerName(app), secretAuditServiceName(app),
	}
	patterns := []string{"/deployments/" + app + "/scheduled-redeploy.sh"}
	if purge {
		patterns = append(patterns, "[teploy-backup:"+app+"]", "[teploy-accessory-backup:"+app+":")
	}
	var script strings.Builder
	script.WriteString("reload=0; ")
	for _, u := range units {
		path := ssh.ShellQuote("/etc/systemd/system/" + u)
		fmt.Fprintf(&script, "if [ -e %s ]; then %ssystemctl disable --now %s >/dev/null 2>&1; %srm -f -- %s || exit 1; reload=1; echo unit:%s; fi; ",
			path, sudo, ssh.ShellQuote(u), sudo, path, u)
	}
	fmt.Fprintf(&script, "if [ $reload = 1 ]; then %ssystemctl daemon-reload || exit 1; fi; ", sudo)
	grepArgs := ""
	for _, p := range patterns {
		grepArgs += " -e " + ssh.ShellQuote(p)
	}
	// Same status-checked crontab read as autodeploy.Unschedule (audit
	// T29): only "no crontab" means empty; any other read failure aborts
	// before the install so unrelated jobs can never be dropped.
	fmt.Fprintf(&script, `if command -v crontab >/dev/null 2>&1; then raw=$(crontab -l 2>&1); rc=$?; `+
		`if [ "$rc" -ne 0 ]; then case "$raw" in *"no crontab"*) raw="";; *) echo "reading crontab failed: $raw" >&2; exit 1;; esac; fi; `+
		`n=$(printf '%%s
' "$raw" | grep -cF%s); `+
		`if [ "$n" -gt 0 ]; then printf '%%s
' "$raw" | grep -vF%s | crontab - || exit 1; echo cron:$n; fi; fi`,
		grepArgs, grepArgs)
	out, err := exec.Run(ctx, script.String())
	if err != nil {
		return nil, fmt.Errorf("removing %s's listener, timers and cron entries: %w", app, err)
	}
	var removed []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		switch {
		case strings.HasPrefix(line, "unit:"):
			removed = append(removed, "systemd unit "+strings.TrimPrefix(line, "unit:"))
		case strings.HasPrefix(line, "cron:"):
			removed = append(removed, strings.TrimPrefix(line, "cron:")+" cron entr(ies)")
		}
	}
	return removed, nil
}

// removeBuildImages deletes the server-built images (<app>-build-<version>)
// of the containers remove just took away. Nothing can deploy or roll back
// to them once the app is gone, and the deploy-time 72h prune never reached
// them while a kept container still used them. The names come from the
// app's own containers, never a name pattern, so another app's images are
// never matched. Best-effort and never forced: an image some other
// container still uses stays, and a failure does not fail the remove.
func removeBuildImages(ctx context.Context, exec ssh.Executor, images []string) []string {
	if len(images) == 0 {
		return nil
	}
	quoted := make([]string, len(images))
	for i, img := range images {
		quoted[i] = ssh.ShellQuote(img)
	}
	out, _ := exec.Run(ctx, "docker rmi "+strings.Join(quoted, " ")+" 2>/dev/null || true")
	var removed []string
	for _, line := range strings.Split(out, "\n") {
		tag, ok := strings.CutPrefix(strings.TrimSpace(line), "Untagged: ")
		if !ok {
			continue
		}
		for _, img := range images {
			if tag == img || tag == img+":latest" {
				removed = append(removed, img)
			}
		}
	}
	return removed
}
