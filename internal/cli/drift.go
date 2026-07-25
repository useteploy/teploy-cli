package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/docker"
	"github.com/useteploy/teploy/internal/ssh"
	"github.com/useteploy/teploy/internal/state"
)

func newDriftCmd(flags *Flags) *cobra.Command {
	var exitCode bool
	var appName string
	cmd := &cobra.Command{
		Use:   "drift",
		Short: "Detect when live server state has diverged from what was deployed (read-only)",
		Long: "Compares the containers actually running on the server against what " +
			"teploy.yml declares for the currently-deployed version, and reports any " +
			"divergence — a container manually stopped, an old-version container left " +
			"running, a replica-count mismatch. Makes no changes and never reconciles.\n\n" +
			"Run from an app directory to compare against teploy.yml. With --app " +
			"(and --host) there is no teploy.yml to read, so drift is derived from " +
			"server state instead: containers of the deployed version that stopped, " +
			"and containers of other versions still running. That covers manual stops " +
			"and stale versions, but cannot see replica-count drift — only the " +
			"manifest knows how many replicas were declared.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDrift(flags, appName, exitCode)
		},
	}
	cmd.Flags().BoolVar(&exitCode, "exit-code", false, "exit 2 when drift is detected (for CI/monitoring)")
	cmd.Flags().StringVar(&appName, "app", "", "app name — derive drift from server state instead of teploy.yml (requires --host)")
	return cmd
}

// driftItem is a single divergence between declared and live state.
type driftItem struct {
	Kind   string `json:"kind"` // "missing" (declared, not running) or "unexpected" (running, not declared)
	Name   string `json:"name"`
	Detail string `json:"detail,omitempty"`
}

// driftItems computes the divergence purely from its inputs (no I/O), so it is
// directly unit-testable. `current` must be non-nil (caller handles the
// not-deployed case).
func driftItems(appCfg *config.AppConfig, current *state.AppState, containers []docker.Container) []driftItem {
	// Declared = what teploy.yml says should run at the CURRENTLY-deployed
	// version (not a new version — that's `teploy plan`).
	declared := desiredContainers(appCfg, current.CurrentHash)
	declaredSet := map[string]bool{}
	for _, d := range declared {
		declaredSet[d.Name] = true
	}

	// Live = running deploy-managed containers only. Accessories
	// (teploy.role=accessory) and previews (teploy.process=preview-*) share the
	// teploy.app label but are not part of the app-process set — including them
	// would produce false "unexpected" drift.
	known := knownProcessSet(appCfg)
	running := map[string]bool{}
	for _, c := range containers {
		if c.State == "running" && isManagedAppContainer(c, known) {
			running[c.Name] = true
		}
	}

	var items []driftItem
	for _, d := range declared {
		if !running[d.Name] {
			items = append(items, driftItem{Kind: "missing", Name: d.Name, Detail: "declared but not running"})
		}
	}
	for name := range running {
		if !declaredSet[name] {
			items = append(items, driftItem{Kind: "unexpected", Name: name, Detail: "running but not declared (old version or manual container)"})
		}
	}

	sort.Slice(items, func(i, j int) bool {
		if items[i].Kind != items[j].Kind {
			return items[i].Kind < items[j].Kind // "missing" before "unexpected"
		}
		return items[i].Name < items[j].Name
	})
	return items
}

// driftItemsFromState computes drift without a manifest, using only what the
// server itself records: the deployed version in state, and the version label
// on each managed container. It deliberately reports nothing about replica
// counts — with no teploy.yml there is no declared count to compare against,
// and inventing one would produce false drift on every multi-replica app.
func driftItemsFromState(currentHash string, containers []docker.Container) []driftItem {
	var items []driftItem
	for _, c := range containers {
		// Accessories and preview containers share the app label but are not
		// part of the deployed app-process set.
		if c.Labels["teploy.role"] == "accessory" {
			continue
		}
		if strings.HasPrefix(c.Labels["teploy.process"], "preview-") {
			continue
		}
		version := c.Labels["teploy.version"]
		if version == "" {
			continue // unlabeled (pre-label deploy) — not classifiable
		}
		switch {
		case version == currentHash && c.State != "running":
			items = append(items, driftItem{
				Kind: "missing", Name: c.Name,
				Detail: "deployed version but not running (stopped outside teploy?)",
			})
		case version != currentHash && c.State == "running":
			items = append(items, driftItem{
				Kind: "unexpected", Name: c.Name,
				Detail: "running an old version (" + version + "), not the deployed " + currentHash,
			})
		}
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Kind != items[j].Kind {
			return items[i].Kind < items[j].Kind
		}
		return items[i].Name < items[j].Name
	})
	return items
}

func runDrift(flags *Flags, appName string, exitCode bool) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	appCfg, executor, err := resolveApp(ctx, flags, appName)
	if err != nil {
		return err
	}

	driftFound, err := reportDrift(ctx, flags, appCfg, executor, appName != "")
	executor.Close()
	if err != nil {
		return err
	}
	// --exit-code makes drift observable to CI/monitoring without treating it
	// as a command failure. Executor is already closed above so os.Exit is safe.
	if exitCode && driftFound {
		os.Exit(2)
	}
	return nil
}

func reportDrift(ctx context.Context, flags *Flags, appCfg *config.AppConfig, executor ssh.Executor, fromState bool) (bool, error) {
	if appCfg.IsStatic() && !fromState {
		if flags.JSON {
			return false, json.NewEncoder(os.Stdout).Encode(map[string]interface{}{
				"app": appCfg.App, "server": executor.Host(), "type": "static",
				"note": "drift detection for static apps is not yet implemented",
			})
		}
		fmt.Printf("Drift check for %s on %s (static): not yet implemented.\n", appCfg.App, executor.Host())
		return false, nil
	}

	current, _ := state.Read(ctx, executor, appCfg.App)
	if current == nil || current.CurrentHash == "" {
		if flags.JSON {
			return false, json.NewEncoder(os.Stdout).Encode(map[string]interface{}{
				"app": appCfg.App, "server": executor.Host(), "deployed": false,
			})
		}
		fmt.Printf("%s is not deployed on %s — nothing to compare.\n", appCfg.App, executor.Host())
		return false, nil
	}

	dk := docker.NewClient(executor)
	containers, err := dk.ListContainers(ctx, appCfg.App)
	if err != nil {
		return false, err
	}

	// Without a manifest, fall back to state-derived detection (see
	// driftItemsFromState) — narrower, but honest about what it can see.
	mode := "manifest"
	var items []driftItem
	if fromState {
		mode = "state"
		items = driftItemsFromState(current.CurrentHash, containers)
	} else {
		items = driftItems(appCfg, current, containers)
	}
	driftFound := len(items) > 0

	if flags.JSON {
		out := map[string]interface{}{
			"app":              appCfg.App,
			"server":           executor.Host(),
			"deployed_version": current.CurrentHash,
			"drift":            driftFound,
			"items":            items,
			"mode":             mode,
		}
		if fromState {
			out["limitation"] = "replica-count drift is not detectable without teploy.yml"
		}
		return driftFound, json.NewEncoder(os.Stdout).Encode(out)
	}

	fmt.Printf("Drift check for %s on %s (deployed version %s)\n\n", appCfg.App, executor.Host(), current.CurrentHash)
	if !driftFound {
		if fromState {
			fmt.Println("In sync — every running container is the deployed version.")
		} else {
			fmt.Println("In sync — live containers match teploy.yml.")
		}
		return false, nil
	}
	for _, it := range items {
		switch it.Kind {
		case "missing":
			fmt.Printf("  - %-40s %s\n", it.Name, it.Detail)
		case "unexpected":
			fmt.Printf("  + %-40s %s\n", it.Name, it.Detail)
		}
	}
	fmt.Printf("\nDRIFT DETECTED: %d difference(s). Run `teploy deploy` to reconcile.\n", len(items))
	fmt.Println("(This is report-only — no changes were made.)")
	return true, nil
}
