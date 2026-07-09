package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"sort"

	"github.com/spf13/cobra"
	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/docker"
	"github.com/useteploy/teploy/internal/ssh"
	"github.com/useteploy/teploy/internal/state"
)

func newPlanCmd(flags *Flags) *cobra.Command {
	var version string
	cmd := &cobra.Command{
		Use:   "plan",
		Short: "Preview what a deploy would change (read-only, no changes made)",
		Long: "Compares the current on-server state against what a deploy would produce " +
			"and prints the difference. Makes no changes. Requires teploy.yml (run from the " +
			"app directory); use --host to target a specific server.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPlan(flags, version)
		},
	}
	cmd.Flags().StringVar(&version, "version", "", "target version (default: current git short hash, matching deploy)")
	return cmd
}

// planChange is a single predicted change from a deploy.
type planChange struct {
	Action string `json:"action"` // "create", "stop", "unchanged"
	Name   string `json:"name"`
	Detail string `json:"detail,omitempty"`
}

// actionOrder gives a stable print/sort order for change actions.
var actionOrder = map[string]int{"create": 0, "unchanged": 1, "stop": 2}

// knownProcessSet returns the process names a deploy manages for this app:
// "web" (always started) plus every declared process. Used to tell
// deploy-managed containers (web/workers) apart from accessories
// (teploy.role=accessory) and preview containers (teploy.process=preview-*),
// which share the teploy.app label but are NOT part of a normal deploy's
// container set — so they must not show up as "stop" / drift.
func knownProcessSet(appCfg *config.AppConfig) map[string]bool {
	s := map[string]bool{"web": true}
	for name := range appCfg.Processes {
		s[name] = true
	}
	return s
}

// isManagedAppContainer reports whether a container is one a deploy of this
// app manages, i.e. a web/worker process container — excluding accessories
// and previews.
func isManagedAppContainer(c docker.Container, known map[string]bool) bool {
	if c.Labels["teploy.role"] == "accessory" {
		return false
	}
	return known[c.Labels["teploy.process"]]
}

// planChanges computes the create/stop/unchanged diff purely from its inputs
// (no I/O), so it is directly unit-testable.
func planChanges(appCfg *config.AppConfig, version string, versionKnown bool, current *state.AppState, containers []docker.Container) (changes []planChange, sameVersion bool) {
	known := knownProcessSet(appCfg)
	running := map[string]bool{}
	for _, c := range containers {
		if c.State == "running" && isManagedAppContainer(c, known) {
			running[c.Name] = true
		}
	}

	if versionKnown {
		desired := desiredContainers(appCfg, version)
		desiredSet := map[string]bool{}
		for _, d := range desired {
			desiredSet[d.Name] = true
			if running[d.Name] {
				changes = append(changes, planChange{Action: "unchanged", Name: d.Name, Detail: "already running"})
			} else {
				changes = append(changes, planChange{Action: "create", Name: d.Name, Detail: d.Process + " container"})
			}
		}
		for name := range running {
			if !desiredSet[name] {
				changes = append(changes, planChange{Action: "stop", Name: name, Detail: "old version — stopped after new is healthy"})
			}
		}
		sameVersion = current != nil && current.CurrentHash == version
	} else {
		for _, d := range desiredContainers(appCfg, "<new>") {
			changes = append(changes, planChange{Action: "create", Name: d.Name, Detail: d.Process + " container (new version, name is indicative)"})
		}
		for name := range running {
			changes = append(changes, planChange{Action: "stop", Name: name, Detail: "old version — stopped after new is healthy"})
		}
	}

	sort.Slice(changes, func(i, j int) bool {
		if actionOrder[changes[i].Action] != actionOrder[changes[j].Action] {
			return actionOrder[changes[i].Action] < actionOrder[changes[j].Action]
		}
		return changes[i].Name < changes[j].Name
	})
	return changes, sameVersion
}

func runPlan(flags *Flags, version string) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	appCfg, executor, err := resolveApp(ctx, flags, "")
	if err != nil {
		return err
	}
	defer executor.Close()

	// Static apps deploy via rsync + symlink swap, not containers. A full
	// static diff is v2; for now report the current release and note the swap.
	if appCfg.IsStatic() {
		return planStatic(ctx, flags, appCfg, executor)
	}

	// Resolve the target version exactly as `deploy` does: --version wins, else
	// the local git short hash. When neither is available (no git repo) but a
	// pre-built image is configured, deploy falls back to a timestamp — a
	// non-deterministic version — so we fall back to an indicative diff.
	versionKnown := true
	if version == "" {
		version, err = gitShortHash()
		if err != nil {
			if appCfg.Image == "" {
				return fmt.Errorf("could not determine target version from git: %w (pass --version)", err)
			}
			versionKnown = false
		}
	}

	current, _ := state.Read(ctx, executor, appCfg.App)
	dk := docker.NewClient(executor)
	containers, err := dk.ListContainers(ctx, appCfg.App)
	if err != nil {
		return err
	}

	changes, sameVersion := planChanges(appCfg, version, versionKnown, current, containers)

	if flags.JSON {
		return json.NewEncoder(os.Stdout).Encode(map[string]interface{}{
			"app":            appCfg.App,
			"server":         executor.Host(),
			"target_version": version,
			"version_known":  versionKnown,
			"same_version":   sameVersion,
			"changes":        changes,
		})
	}

	fmt.Printf("Plan for %s on %s\n", appCfg.App, executor.Host())
	if versionKnown {
		fmt.Printf("Target version: %s\n", version)
	} else {
		fmt.Printf("Target version: (timestamp — not predictable; names below are indicative)\n")
	}
	if sameVersion {
		fmt.Printf("\nNote: target matches the currently-deployed version %s — a deploy would\n", version)
		fmt.Printf("recreate these containers in place rather than blue/green swap.\n")
	}

	var nCreate, nStop int
	fmt.Println()
	for _, ch := range changes {
		switch ch.Action {
		case "create":
			nCreate++
			fmt.Printf("  + %-40s %s\n", ch.Name, ch.Detail)
		case "stop":
			nStop++
			fmt.Printf("  - %-40s %s\n", ch.Name, ch.Detail)
		case "unchanged":
			fmt.Printf("    %-40s %s\n", ch.Name, ch.Detail)
		}
	}
	if len(changes) == 0 {
		fmt.Println("  (no changes)")
	}
	fmt.Printf("\n%d to create, %d to stop\n", nCreate, nStop)
	fmt.Println("\nThis is a preview. No changes were made. Run `teploy deploy` to apply.")
	return nil
}

// desiredContainer is one container a deploy of the given version would run.
type desiredContainer struct {
	Process string
	Name    string
}

// desiredContainers computes the set of containers a deploy would produce for
// the given app config and version: `replicas` web containers plus one
// container per non-web process. Mirrors internal/deploy/deploy.go's start
// loop (web replicas + one-each workers).
func desiredContainers(appCfg *config.AppConfig, version string) []desiredContainer {
	replicas := appCfg.Replicas
	if replicas < 1 {
		replicas = 1
	}

	var out []desiredContainer
	for i := 0; i < replicas; i++ {
		out = append(out, desiredContainer{
			Process: "web",
			Name:    docker.ReplicaContainerName(appCfg.App, "web", version, i+1, replicas),
		})
	}

	var others []string
	for name := range appCfg.Processes {
		if name != "web" {
			others = append(others, name)
		}
	}
	sort.Strings(others)
	for _, p := range others {
		out = append(out, desiredContainer{
			Process: p,
			Name:    docker.ContainerName(appCfg.App, p, version),
		})
	}
	return out
}

// planStatic reports the state of a static app. A full static diff is future
// work; for now it notes the swap a deploy performs.
func planStatic(ctx context.Context, flags *Flags, appCfg *config.AppConfig, executor ssh.Executor) error {
	_ = ctx // reserved for the future per-file static diff
	if flags.JSON {
		return json.NewEncoder(os.Stdout).Encode(map[string]interface{}{
			"app":    appCfg.App,
			"server": executor.Host(),
			"type":   "static",
			"note":   "static apps deploy by rsyncing a new release and swapping the current symlink; per-file diff is not yet computed",
		})
	}
	fmt.Printf("Plan for %s on %s (static)\n\n", appCfg.App, executor.Host())
	fmt.Println("A deploy rsyncs a new release and swaps the `current` symlink.")
	fmt.Println("Per-file diffing for static apps is not yet implemented.")
	fmt.Println("\nThis is a preview. No changes were made.")
	return nil
}
