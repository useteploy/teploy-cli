package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/dns"
	"github.com/useteploy/teploy/internal/domains"
	"github.com/useteploy/teploy/internal/ssh"
)

// Bounds of a sweep. One slow or dead server costs only its own budget.
const (
	domainsServerConcurrency = 4
	domainsConnectTimeout    = 15 * time.Second
	domainsServerTimeout     = 90 * time.Second
	domainsSweepTimeout      = 3 * time.Minute
)

func newDomainsCmd(flags *Flags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "domains",
		Short: "Check what serves your domains",
	}
	var server, app string
	var window time.Duration
	check := &cobra.Command{
		Use:   "check",
		Short: "Report DNS, certificate and route health per domain",
		Long: `For each (server, app, host) report:

  dns    what the public resolvers 1.1.1.1 and 8.8.8.8 say, not the local one:
         ok | nxdomain | wrong_target | proxied (a hint) | unknown | skipped
  tls    Caddy's certificate failures per identifier, from its logs. Logs that
         are short or unreadable give "unknown", never 0
  route  who serves the name: managed, manual, stale_managed (the app is no
         longer caddy-routed), orphan_managed (no such app), or none, and
         whether its upstreams resolve in Caddy's network

With no --server every server in servers.yml is checked. Read-only. A server
that cannot be reached is reported as such, not as healthy. With --json the
command exits 0 whenever the envelope was produced; read servers[].state.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDomainsCheck(cmd.Context(), flags, cmd.OutOrStdout(), server, app, window)
		},
	}
	check.Flags().StringVar(&server, "server", "", "check one server (servers.yml name or host)")
	check.Flags().StringVar(&app, "app", "", "report one app")
	check.Flags().DurationVar(&window, "log-window", domains.DefaultLogWindow, "how far back to read Caddy's logs")
	cmd.AddCommand(check)
	return cmd
}

type domainsTarget struct {
	name string
	host string
	user string
	key  string
}

func domainsTargets(flags *Flags, server string) ([]domainsTarget, error) {
	if server != "" || flags.Host != "" {
		name := server
		if name == "" {
			name = flags.Host
		}
		host, user, key, err := config.ResolveServer(name, flags.Host, flags.User, flags.Key)
		if err != nil {
			return nil, fmt.Errorf("resolving server %q: %w", name, err)
		}
		return []domainsTarget{{name: name, host: host, user: user, key: key}}, nil
	}
	path, err := config.DefaultServersPath()
	if err != nil {
		return nil, err
	}
	servers, err := config.ListServers(path)
	if err != nil {
		return nil, err
	}
	if len(servers) == 0 {
		return nil, fmt.Errorf("no servers configured: add one with `teploy server add` or pass --server")
	}
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	sort.Strings(names)
	targets := make([]domainsTarget, 0, len(names))
	for _, name := range names {
		host, user, key, err := config.ResolveServer(name, "", flags.User, flags.Key)
		if err != nil {
			targets = append(targets, domainsTarget{name: name})
			continue
		}
		targets = append(targets, domainsTarget{name: name, host: host, user: user, key: key})
	}
	return targets, nil
}

func runDomainsCheck(parent context.Context, flags *Flags, out io.Writer, server, app string, window time.Duration) error {
	if app != "" {
		if err := config.ValidateName(app); err != nil {
			return err
		}
	}
	if window <= 0 {
		return fmt.Errorf("--log-window must be positive")
	}
	if parent == nil {
		parent = context.Background()
	}
	targets, err := domainsTargets(flags, server)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(parent, os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, domainsSweepTimeout)
	defer cancel()

	connect := func(ctx context.Context, t domainsTarget) (ssh.Executor, error) {
		if t.host == "" {
			return nil, fmt.Errorf("server %s has no resolvable host", t.name)
		}
		dialCtx, dialCancel := context.WithTimeout(ctx, domainsConnectTimeout)
		defer dialCancel()
		return ssh.Connect(dialCtx, ssh.ConnectConfig{Host: t.host, User: t.user, KeyPath: t.key})
	}
	env := sweepDomains(ctx, targets, connect, domains.Options{LogWindow: window, App: app}, domains.NewDNSCache(), time.Now().UTC())
	if flags.JSON {
		return json.NewEncoder(out).Encode(env)
	}
	return printDomains(out, env)
}

// sweepDomains checks every target under bounded concurrency. A server that
// fails still gets its envelope entry: its absence must not read as healthy.
func sweepDomains(ctx context.Context, targets []domainsTarget, connect func(context.Context, domainsTarget) (ssh.Executor, error), opts domains.Options, cache *domains.DNSCache, now time.Time) domains.Envelope {
	if opts.LogWindow <= 0 {
		opts.LogWindow = domains.DefaultLogWindow
	}
	env := domains.Envelope{
		MachineInterface:     MachineInterface,
		CheckedAt:            now,
		LogWindowSeconds:     int64(opts.LogWindow / time.Second),
		Resolvers:            append([]string{}, dns.PublicResolvers...),
		CloudflareRangesAsOf: dns.CloudflareRangesAsOf(),
		Servers:              make([]domains.Server, len(targets)),
		Domains:              []domains.Domain{},
	}
	rows := make([][]domains.Domain, len(targets))
	sem := make(chan struct{}, domainsServerConcurrency)
	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Add(1)
		go func(i int, t domainsTarget) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				env.Servers[i] = unreachableServer(t, now, ctx.Err())
				return
			}
			serverCtx, cancel := context.WithTimeout(ctx, domainsServerTimeout)
			defer cancel()
			exec, err := connect(serverCtx, t)
			if err != nil {
				env.Servers[i] = unreachableServer(t, now, err)
				return
			}
			defer exec.Close()
			env.Servers[i], rows[i] = domains.CheckServer(serverCtx, exec, t.name, opts, cache)
			env.Servers[i].Host = t.host
		}(i, t)
	}
	wg.Wait()
	for _, r := range rows {
		env.Domains = append(env.Domains, r...)
	}
	return env
}

func unreachableServer(t domainsTarget, now time.Time, err error) domains.Server {
	return domains.Server{Server: t.name, Host: t.host, State: domains.ServerUnreachable, CheckedAt: now,
		Error: strings.Join(strings.Fields(err.Error()), " "), Addresses: []string{}, Issues: []domains.Issue{},
		Caddy: domains.CaddyStatus{Container: "unknown", Caddyfile: "unreadable", Logs: domains.LogsUnavailable}}
}

func printDomains(out io.Writer, env domains.Envelope) error {
	if len(env.Domains) > 0 {
		fmt.Fprintf(out, "%-14s  %-22s  %-34s  %-13s  %-14s  %s\n", "SERVER", "APP", "HOST", "DNS", "CERT", "ROUTE")
	}
	for _, d := range env.Domains {
		cert := d.TLS.Status
		if d.TLS.Status == domains.TLSFailing && d.TLS.Failures != nil {
			cert = fmt.Sprintf("failing x%d", *d.TLS.Failures)
		}
		route := d.Route.Kind
		if d.Route.UpstreamResolves == domains.ResolvesNo || d.Route.UpstreamResolves == domains.ResolvesPartial {
			route += " (upstream " + d.Route.UpstreamResolves + ")"
		}
		dnsCol := d.DNS.Status
		if d.Skip != "" {
			dnsCol += " [" + d.Skip + "]"
		}
		fmt.Fprintf(out, "%-14s  %-22s  %-34s  %-13s  %-14s  %s\n", d.Server, d.App, d.Host, dnsCol, cert, route)
	}
	if len(env.Domains) == 0 {
		fmt.Fprintln(out, "No domains on the checked servers")
	}
	failed := 0
	for _, s := range env.Servers {
		switch s.State {
		case domains.ServerUnreachable:
			failed++
			fmt.Fprintf(out, "\n%s: could not be checked: %s\n", s.Server, s.Error)
		case domains.ServerPartial:
			for _, i := range s.Issues {
				fmt.Fprintf(out, "\n%s: %s: %s\n", s.Server, i.Scope, i.Message)
			}
		}
		if s.State != domains.ServerUnreachable && s.Caddy.Logs != domains.LogsOK && s.Caddy.Logs != domains.LogsNoCaddy {
			fmt.Fprintf(out, "\n%s: Caddy logs are %s; certificate failures read unknown\n", s.Server, s.Caddy.Logs)
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d server(s) could not be checked", failed)
	}
	return nil
}
