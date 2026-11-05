package cli

// The two disk remedies `server audit` offers: `releases prune --keep N` and
// `server prune --build-cache --dangling`. Neither touches a volume.

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
	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/docker"
	"github.com/useteploy/teploy/internal/events"
	"github.com/useteploy/teploy/internal/ssh"
	"github.com/useteploy/teploy/internal/state"
)

type releasesPruneResult struct {
	MachineInterface int      `json:"machine_interface"`
	App              string   `json:"app"`
	Server           string   `json:"server"`
	Keep             int      `json:"keep"`
	Pruned           []string `json:"pruned"`
	Protected        []string `json:"protected"`
}

func newReleasesPruneCmd(flags *Flags) *cobra.Command {
	var app string
	var keep int
	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Remove old versions' containers and images beyond --keep",
		Long: `Keeps the --keep most recent versions of a container app and removes the
containers and images of the rest. Pinned, current and previous versions and any
version with a running container are never removed, whatever --keep says. Volumes
and accessories are not touched. Takes the app's lock; recorded in its event
history. Nothing beyond --keep is success.

--keep 0 removes every version the protections above do not name — unlike
keep_versions: 0 in teploy.yml, which keeps everything.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !cmd.Flags().Changed("keep") {
				return fmt.Errorf("--keep is required: how many recent versions to retain")
			}
			if keep < 0 {
				return fmt.Errorf("--keep must be >= 0")
			}
			return runReleasesPrune(flags, app, keep, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&app, "app", "", "app name — act on server state instead of teploy.yml (requires --host)")
	cmd.Flags().IntVar(&keep, "keep", 0, "how many recent versions to retain")
	return cmd
}

func runReleasesPrune(flags *Flags, app string, keep int, out io.Writer) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	appCfg, exec, err := resolveApp(ctx, flags, app)
	if err != nil {
		return err
	}
	defer exec.Close()
	res, err := executeReleasesPrune(ctx, exec, appCfg.App, keep)
	if err != nil {
		return err
	}
	if flags.JSON {
		return json.NewEncoder(out).Encode(res)
	}
	if len(res.Pruned) == 0 {
		fmt.Fprintf(out, "Nothing to prune for %s\n", res.App)
	} else {
		fmt.Fprintf(out, "Pruned %d version(s) of %s: %s\n", len(res.Pruned), res.App, strings.Join(res.Pruned, ", "))
	}
	return nil
}

func executeReleasesPrune(ctx context.Context, exec ssh.Executor, app string, keep int) (*releasesPruneResult, error) {
	res := &releasesPruneResult{MachineInterface: MachineInterface, App: app, Server: exec.Host(), Keep: keep, Pruned: []string{}, Protected: []string{}}
	exists, err := appDirExists(ctx, exec, app)
	if err != nil {
		return nil, err
	}
	if !exists {
		return res, nil // already gone
	}
	lk, err := state.AcquireLockFenced(ctx, exec, app)
	if err != nil {
		return nil, fmt.Errorf("acquiring deploy lock: %w", err)
	}
	defer state.ReleaseLockFenced(exec, lk, app)

	// Every read is under the lock, and each failure stops the prune: acting
	// without knowing current, previous or the pins could delete them.
	st, err := state.Read(ctx, exec, app)
	if err != nil {
		return nil, fmt.Errorf("reading state for %s: %w", app, err)
	}
	if st == nil {
		return res, nil
	}
	if st.DeploymentType == "static" {
		return nil, fmt.Errorf("%s is a static app: its releases are pruned on deploy by keep_releases", app)
	}
	pins, err := state.ReadPins(ctx, exec, app)
	if err != nil {
		return nil, fmt.Errorf("refusing to prune: %w", err)
	}
	dk := docker.NewClient(exec)
	containers, err := dk.ListContainers(ctx, app)
	if err != nil {
		return nil, fmt.Errorf("refusing to prune: %w", err)
	}
	for _, v := range retentionProtected(st, pins, containers) {
		if v != "" {
			res.Protected = append(res.Protected, v)
		}
	}
	if len(docker.PrunableVersions(containers, keep, res.Protected...)) == 0 {
		return res, nil
	}
	fenced := docker.NewClient(&state.FencedExecutor{Executor: exec, Lock: lk})
	pruned, pruneErr := fenced.PruneVersions(ctx, app, keep, res.Protected...)
	res.Pruned = append(res.Pruned, pruned...)
	events.Record(ctx, exec, events.Event{App: app, Kind: events.KindReleasesPrune, Outcome: events.Outcome(pruneErr), Release: st.CurrentHash,
		Detail: events.ErrDetail(map[string]any{"keep": keep, "pruned": res.Pruned, "protected": res.Protected, "trigger": "command"}, pruneErr)})
	if pruneErr != nil {
		return nil, fmt.Errorf("prune incomplete: %w", pruneErr)
	}
	return res, nil
}

type serverPruneResult struct {
	MachineInterface int    `json:"machine_interface"`
	Server           string `json:"server"`
	BuildCache       bool   `json:"build_cache"`
	Dangling         bool   `json:"dangling"`
	Output           string `json:"output"`
}

func newServerPruneCmd(flags *Flags) *cobra.Command {
	var buildCache, dangling bool
	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Remove unused build cache and dangling images",
		Long: `--build-cache removes docker's unused build cache: the next build rebuilds it.
--dangling removes untagged images no container uses. Nothing else: no tagged
image, no container, no volume. Recorded in the server's event history.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !buildCache && !dangling {
				return fmt.Errorf("say what to prune: --build-cache and/or --dangling")
			}
			if flags.Host == "" {
				return fmt.Errorf("--host is required")
			}
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
			defer cancel()
			host, user, key, err := config.ResolveServer(flags.Host, flags.Host, flags.User, flags.Key)
			if err != nil {
				return err
			}
			exec, err := ssh.Connect(ctx, ssh.ConnectConfig{Host: host, User: user, KeyPath: key})
			if err != nil {
				return err
			}
			defer exec.Close()
			res, err := executeServerPrune(ctx, exec, flags.Host, buildCache, dangling)
			if err != nil {
				return err
			}
			if flags.JSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(res)
			}
			fmt.Fprintln(cmd.OutOrStdout(), res.Output)
			return nil
		},
	}
	cmd.Flags().BoolVar(&buildCache, "build-cache", false, "remove unused build cache")
	cmd.Flags().BoolVar(&dangling, "dangling", false, "remove dangling images")
	return cmd
}

func executeServerPrune(ctx context.Context, exec ssh.Executor, server string, buildCache, dangling bool) (*serverPruneResult, error) {
	res := &serverPruneResult{MachineInterface: MachineInterface, Server: server, BuildCache: buildCache, Dangling: dangling}
	var failures []error
	var lines []string
	run := func(cmd string) {
		out, err := exec.Run(ctx, cmd)
		if err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", cmd, err))
			return
		}
		for _, line := range doctorLines(out) {
			if strings.HasPrefix(line, "Total") {
				lines = append(lines, line)
			}
		}
	}
	if buildCache {
		run("docker builder prune -a -f")
	}
	if dangling {
		run("docker image prune -f")
	}
	res.Output = strings.Join(lines, "; ")
	err := errors.Join(failures...)
	events.Record(ctx, exec, events.Event{App: "_fleet", Kind: events.KindServerPrune, Outcome: events.Outcome(err),
		Detail: events.ErrDetail(map[string]any{"build_cache": buildCache, "dangling": dangling, "output": res.Output}, err)})
	if err != nil {
		return nil, err
	}
	return res, nil
}
