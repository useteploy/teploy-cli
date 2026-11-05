package cli

// `teploy route remove`: retire a managed Caddy block that nothing serves. Only
// a route proven dead goes (removalRefusal), only that app's own marked region
// is edited, and the commit is a compare-and-swap on the exact block that was
// judged, so a deploy that rewrites it in between is not undone.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"

	"github.com/spf13/cobra"
	"github.com/useteploy/teploy/internal/caddy"
	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/docker"
	"github.com/useteploy/teploy/internal/domains"
	"github.com/useteploy/teploy/internal/events"
	"github.com/useteploy/teploy/internal/ssh"
	"github.com/useteploy/teploy/internal/state"
)

var errRouteRefused = errors.New("route remove refused")

type routeRemoveResult struct {
	MachineInterface int      `json:"machine_interface"`
	App              string   `json:"app"`
	Server           string   `json:"server"`
	Route            string   `json:"route"` // removed | absent; refused/failed appear only in event detail
	Hosts            []string `json:"hosts,omitempty"`
	Upstreams        []string `json:"upstreams,omitempty"`
}

func newRouteCmd(flags *Flags) *cobra.Command {
	cmd := &cobra.Command{Use: "route", Short: "Manage Caddy routes"}
	var app string
	remove := &cobra.Command{
		Use:   "remove",
		Short: "Remove a managed route that nothing serves",
		Long: `Removes one app's managed Caddy block, reloads Caddy, and records it in the
app's event history. Hand-written blocks and other apps' blocks are never
touched. An absent block is success.

Refuses a route belonging to a live app, and any route not proven dead: an
upstream that resolves or cannot be checked, files that still exist, app state
or containers that cannot be read, or a block carrying the autodeploy webhook.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRouteRemove(flags, app, cmd.OutOrStdout())
		},
	}
	remove.Flags().StringVar(&app, "app", "", "the app whose managed block to remove")
	_ = remove.MarkFlagRequired("app")
	cmd.AddCommand(remove)
	return cmd
}

func runRouteRemove(flags *Flags, app string, out io.Writer) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	serverName, exec, err := connectAppServer(ctx, flags, app)
	if err != nil {
		return err
	}
	defer exec.Close()
	res, err := executeRouteRemove(ctx, exec, app)
	if err != nil {
		return err
	}
	res.Server = serverName
	if flags.JSON {
		return json.NewEncoder(out).Encode(res)
	}
	if res.Route == "absent" {
		fmt.Fprintf(out, "%s has no managed route on %s\n", app, serverName)
	} else {
		fmt.Fprintf(out, "Removed %s's route on %s (%s)\n", app, serverName, strings.Join(res.Hosts, ", "))
	}
	return nil
}

// routeLockApp is the app whose lock serializes an edit of this route. A
// preview's route has no directory of its own, so its parent's lock guards it.
func routeLockApp(ctx context.Context, exec ssh.Executor, app string) (string, error) {
	exists, err := appDirExists(ctx, exec, app)
	if err != nil || exists {
		return app, err
	}
	parent, _, ok := strings.Cut(app, "-preview-")
	if !ok || config.ValidateName(parent) != nil {
		return "", nil
	}
	if exists, err = appDirExists(ctx, exec, parent); err != nil || !exists {
		return "", err
	}
	return parent, nil
}

// appDirExists separates "no directory" from "could not ask".
func appDirExists(ctx context.Context, exec ssh.Executor, app string) (bool, error) {
	out, err := exec.Run(ctx, "if [ -d "+ssh.ShellQuote("/deployments/"+app)+" ]; then echo yes; else echo no; fi")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == "yes", nil
}

func executeRouteRemove(ctx context.Context, exec ssh.Executor, app string) (*routeRemoveResult, error) {
	res := &routeRemoveResult{MachineInterface: MachineInterface, App: app, Server: exec.Host(), Route: "absent"}
	record := func(outcome string, err error, reason string) {
		events.Record(ctx, exec, events.Event{App: app, Kind: events.KindRouteRemove, Outcome: outcome,
			Detail: events.ErrDetail(map[string]any{"route": res.Route, "hosts": res.Hosts, "upstreams": res.Upstreams, "reason": reason}, err)})
	}

	lockApp, err := routeLockApp(ctx, exec, app)
	if err != nil {
		return nil, err
	}
	var lk *state.Lock
	if lockApp != "" {
		if lk, err = state.AcquireLockFenced(ctx, exec, lockApp); err != nil {
			return nil, fmt.Errorf("acquiring deploy lock: %w", err)
		}
		defer state.ReleaseLockFenced(exec, lk, lockApp)
	}

	cd := caddy.NewClient(exec)
	content, present, err := cd.ReadCaddyfile(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading the Caddyfile: %w", err)
	}
	var region *caddy.ManagedRegion
	if present {
		regions, err := caddy.ManagedRegions(content)
		if err != nil {
			return nil, fmt.Errorf("reading the Caddyfile: %w", err)
		}
		for i := range regions {
			if regions[i].App == app {
				region = &regions[i]
			}
		}
	}
	if region == nil {
		record(events.Succeeded, nil, "")
		return res, nil
	}
	res.Hosts = siteHosts(region.Sites)
	for _, site := range region.Sites {
		res.Upstreams = append(res.Upstreams, site.Upstreams...)
	}

	facts := routeFacts{app: app, region: region.Text, sites: region.Sites, resolves: map[string]string{}, roots: map[string]string{}}
	st, stErr := state.Read(ctx, exec, app)
	facts.st, facts.stateKnown = st, stErr == nil
	containers, listErr := docker.NewClient(exec).ListAllContainers(ctx)
	facts.containers, facts.known = containers, listErr == nil
	facts.resolves = resolveRegionDials(ctx, exec, region.Sites, containers)
	for _, root := range caddy.RegionRoots(region.Text) {
		facts.roots[root], _ = readRootExists(ctx, exec, root)
	}
	if reason := removalRefusal(facts); reason != "" {
		err := fmt.Errorf("%w: %s: %s", errRouteRefused, app, reason)
		res.Route = "refused"
		record(events.Refused, nil, reason)
		return nil, err
	}

	generation, _ := caddy.RegionGeneration(region.Text)
	guard := ""
	if lk != nil {
		guard = lk.GuardPrefix()
	}
	err = cd.WithCommitGuard(guard).WithRouteCAS(app, []string{caddy.ManagedRegionHash(region.Text)}, generation).RemoveRoute(ctx, app)
	if err != nil {
		res.Route = "failed"
		record(events.Failed, err, "")
		return nil, fmt.Errorf("removing %s's route: %w", app, err)
	}
	res.Route = "removed"
	record(events.Succeeded, nil, "")
	return res, nil
}

// resolveRegionDials asks the running Caddy about a region's upstreams. No
// answer leaves a name unknown, which refuses the removal.
func resolveRegionDials(ctx context.Context, exec ssh.Executor, sites []caddy.RouteSite, containers []docker.Container) map[string]string {
	running := false
	for _, c := range containers {
		if c.Name == "caddy" && c.State == "running" {
			running = true
		}
	}
	var names []string
	for _, site := range sites {
		for _, dial := range site.Upstreams {
			if name, _ := domains.UpstreamHost(dial); name != "" {
				names = append(names, name)
			}
		}
	}
	if !running || len(names) == 0 {
		return map[string]string{}
	}
	answers, err := domains.ResolveNames(ctx, exec, names)
	if err != nil {
		return map[string]string{}
	}
	return answers
}
