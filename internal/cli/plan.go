package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"

	"github.com/spf13/cobra"
	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/docker"
	"github.com/useteploy/teploy/internal/releasemeta"
	"github.com/useteploy/teploy/internal/ssh"
	"github.com/useteploy/teploy/internal/state"
)

func newPlanCmd(flags *Flags) *cobra.Command {
	var (
		version     string
		image       string
		destination string
		outFile     string
	)
	cmd := &cobra.Command{
		Use:   "plan",
		Short: "Preview what a deploy would change (read-only, no changes made)",
		Long: "Compares the current on-server state against what a deploy would produce " +
			"and prints the difference: containers, routing, environment keys, storage, " +
			"resources and accessories, plus what the plan could NOT resolve (an image " +
			"still to be built is bound by its build inputs, not guessed). Makes no " +
			"changes. Requires teploy.yml (run from the app directory); use --host to " +
			"target a specific server.\n\n" +
			"With --out FILE the plan is written as a JSON record that `teploy apply` " +
			"verifies against config, version, build-input and target-state identity " +
			"before executing — anything that moved since the plan invalidates it.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPlan(flags, version, image, destination, outFile)
		},
	}
	cmd.Flags().StringVar(&version, "version", "", "target version (default: current git short hash, matching deploy)")
	cmd.Flags().StringVar(&image, "image", "", "Docker image to deploy (skips build if set) — mirror of deploy --image")
	cmd.Flags().StringVarP(&destination, "destination", "d", "", "destination overlay (e.g. staging merges teploy.staging.yml) — mirror of deploy -d")
	cmd.Flags().StringVar(&outFile, "out", "", "write the plan record to this file for `teploy apply`")
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

// buildPlanRecord computes the full plan (identity + effects) from its
// inputs. The executor is used read-only (state read, container listing,
// image digest resolution). Static apps are handled by the caller.
// Returns the record and whether the target version equals the
// currently-deployed one (display fact for the human/JSON output).
func buildPlanRecord(ctx context.Context, exec ssh.Executor, out io.Writer, appCfg *config.AppConfig, version string, versionKnown, versionExplicit bool, image, destination, serverName string) (*PlanRecord, bool, error) {
	current, err := state.Read(ctx, exec, appCfg.App)
	if err != nil {
		return nil, false, err
	}
	dk := docker.NewClient(exec)
	containers, err := dk.ListContainers(ctx, appCfg.App)
	if err != nil {
		return nil, false, err
	}

	needsBuild := image == ""
	_, manifestSHA, err := config.NormalizeAndDigest(appCfg, image)
	if err != nil {
		return nil, false, fmt.Errorf("normalizing planned manifest: %w", err)
	}

	// Image identity + build inputs resolve through the SAME provenance
	// path a deploy uses (C04), so plan and deploy cannot disagree about
	// what "the build inputs" are.
	prov := resolveDeployProvenance(ctx, exec, out, appCfg, ".", image, version, manifestSHA, needsBuild)
	img := planImageIdentity(prov, image, needsBuild)

	var view *config.AppliedManifestView
	if current != nil && len(current.AppliedManifest) > 0 {
		if view, err = config.ParseAppliedManifest(current.AppliedManifest); err != nil {
			fmt.Fprintf(out, "Warning: deployed manifest unreadable (%v) — config surfaces diff against an unknown current\n", err)
		}
	}

	changes, sameVersion := planChanges(appCfg, version, versionKnown, current, containers)
	rec := &PlanRecord{
		SchemaVersion:   PlanRecordSchemaVersion,
		App:             appCfg.App,
		Server:          exec.Host(),
		ServerName:      serverName,
		Destination:     destination,
		TargetVersion:   version,
		VersionKnown:    versionKnown,
		VersionExplicit: versionExplicit,
		ConfigDigest:    manifestSHA,
		Image:           img,
		TargetState: PlanTargetState{
			Deployed:       current != nil,
			Generation:     0,
			CurrentHash:    "",
			ManifestSHA256: "",
		},
		Effects: PlanEffects{Containers: changes},
	}
	if current != nil {
		rec.TargetState.Generation = current.Generation
		rec.TargetState.CurrentHash = current.CurrentHash
		rec.TargetState.ManifestSHA256 = current.ManifestSHA256
	}
	rec.User = exec.User()

	rec.Effects.Routing = routingEffects(appCfg, current, view)
	rec.Effects.Env = envEffects(appCfg, view)
	rec.Effects.Storage = storageEffects(appCfg.App, appCfg, view)
	rec.Effects.Resources = resourceEffects(appCfg, view)
	rec.Effects.Accessories = accessoryEffects(appCfg, view)
	rec.Unresolved = unresolvedNotes(img, versionKnown)
	rec.PlanID = computePlanID(rec)
	return rec, sameVersion, nil
}

// planImageIdentity maps a resolved provenance record onto the plan's
// image classification: known bytes (digest-pinned or resolved content
// ID) vs unresolved (mutable tag that could not be resolved, or an
// image that does not exist until the deploy builds it).
func planImageIdentity(prov *releasemeta.Provenance, image string, needsBuild bool) PlanImageIdentity {
	img := PlanImageIdentity{
		Ref:                image,
		NeedsBuild:         needsBuild,
		ContextPath:        prov.ContextPath,
		ContextFingerprint: prov.ContextFingerprint,
		Dockerfile:         prov.Dockerfile,
		DockerfileSHA256:   prov.DockerfileSHA256,
		Platform:           prov.Platform,
	}
	switch {
	case needsBuild:
		img.Resolution = imageUnresolvedAwaitingBuild
	case prov.DigestPinned && prov.ImageDigest != "":
		img.Resolution = imageResolvedByDigest
		img.Digest = prov.ImageDigest
	case prov.ImageDigest != "":
		img.Resolution = imageResolvedByImageID
		img.Digest = prov.ImageDigest
	default:
		img.Resolution = imageUnresolvedMutableTag
	}
	return img
}

// unresolvedNotes states, in operator terms, everything the plan could
// not know — the KNOWN-vs-UNRESOLVED contract's other half.
func unresolvedNotes(img PlanImageIdentity, versionKnown bool) []string {
	var notes []string
	switch img.Resolution {
	case imageUnresolvedAwaitingBuild:
		notes = append(notes, fmt.Sprintf("image unresolved — built at deploy time; the plan binds the build inputs (context %s fingerprint %s, Dockerfile %s sha %s)",
			orUnset(img.ContextPath), shortDigest(img.ContextFingerprint), orUnset(img.Dockerfile), shortDigest(img.DockerfileSHA256)))
	case imageUnresolvedMutableTag:
		notes = append(notes, fmt.Sprintf("image %s is a mutable tag whose content could not be resolved at plan time — the deploy pulls and resolves it fresh", img.Ref))
	case imageResolvedByImageID:
		notes = append(notes, fmt.Sprintf("image %s is a MUTABLE tag (currently %s) — the registry can move it before apply; the plan binds the reference, not the bytes", img.Ref, shortDigest(img.Digest)))
	}
	if !versionKnown {
		notes = append(notes, "target version is a deploy-time timestamp — container names are indicative and `teploy apply` refuses this plan (re-plan with --version or a digest-pinned image)")
	}
	return notes
}

func orUnset(s string) string {
	if s == "" {
		return "(unset)"
	}
	return s
}

func shortDigest(s string) string {
	if len(s) > 16 {
		return s[:16] + "..."
	}
	if s == "" {
		return "(unset)"
	}
	return s
}

// resolvePlanServerName resolves the logical server the plan targets,
// mirroring connectForApp's resolution so the record names the server
// apply will deploy to.
func resolvePlanServerName(appCfg *config.AppConfig) string {
	if appCfg.Server != "" {
		return appCfg.Server
	}
	if len(appCfg.Servers) > 0 {
		return appCfg.Servers[0]
	}
	return ""
}

func runPlan(flags *Flags, version, image, destination, outFile string) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	var appCfg *config.AppConfig
	var err error
	if destination != "" {
		appCfg, err = config.LoadAppWithDestination(".", destination, config.OverlayOptions{Strict: flags.StrictEnv})
	} else {
		appCfg, err = config.LoadApp(".")
	}
	if err != nil {
		return err
	}
	serverName := resolvePlanServerName(appCfg)
	executor, err := connectForApp(ctx, flags, appCfg)
	if err != nil {
		return err
	}
	defer executor.Close()

	// Static apps deploy via rsync + symlink swap, not containers. A full
	// static diff is v2; the plan records identity + the swap.
	if appCfg.IsStatic() {
		return planStatic(ctx, flags, appCfg, executor, destination, outFile)
	}

	// Resolve the target version exactly as `deploy` does — and it has to stay
	// exactly, or plan predicts container names the deploy will not create.
	// --version wins; else a prebuilt image supplies it; else the git hash.
	// A floating tag (:latest, untagged) yields a timestamp at deploy time,
	// which is non-deterministic, so the diff can only be indicative.
	versionExplicit := version != ""
	versionKnown := true
	if version == "" {
		if appCfg.Image != "" {
			version = versionFromImage(appCfg.Image)
			if version == "" {
				versionKnown = false
			}
		} else {
			version, err = gitShortHash()
			if err != nil {
				return fmt.Errorf("could not determine target version from git: %w (pass --version)", err)
			}
		}
	}
	if image == "" {
		image = appCfg.Image
	}

	// Warnings go to stderr under --json so stdout stays one parseable
	// document (the connectForApp precedent).
	warnOut := io.Writer(os.Stdout)
	if flags.JSON {
		warnOut = os.Stderr
	}
	rec, sameVersion, err := buildPlanRecord(ctx, executor, warnOut, appCfg, version, versionKnown, versionExplicit, image, destination, serverName)
	if err != nil {
		return err
	}

	if err := renderPlan(flags, rec, sameVersion); err != nil {
		return err
	}
	if outFile != "" {
		if err := savePlanFile(outFile, rec); err != nil {
			return err
		}
		if !flags.JSON {
			fmt.Printf("\nPlan %s written to %s — apply with: teploy apply %s\n", rec.PlanID, outFile, outFile)
		}
	}
	return nil
}

// planJSONEnvelope is `teploy plan --json`'s output: the PRE-plan/apply
// keys (app/server/target_version/version_known/same_version/changes)
// unchanged, with the plan-record fields riding additively (the MI
// rules: new keys are additive; removing or renaming the old ones would
// bump MachineInterface). The --out FILE record keeps its own shape —
// it is apply's input, not a display.
type planJSONEnvelope struct {
	App           string       `json:"app"`
	Server        string       `json:"server"`
	TargetVersion string       `json:"target_version"`
	VersionKnown  bool         `json:"version_known"`
	SameVersion   bool         `json:"same_version"`
	Changes       []planChange `json:"changes"`

	PlanID       string            `json:"plan_id"`
	Destination  string            `json:"destination,omitempty"`
	ConfigDigest string            `json:"config_digest"`
	Image        PlanImageIdentity `json:"image"`
	TargetState  PlanTargetState   `json:"target_state"`
	Effects      PlanEffects       `json:"effects"`
	Unresolved   []string          `json:"unresolved,omitempty"`
}

// renderPlan prints the plan: identity header, known effects by surface,
// then the unresolved list. JSON mode emits the compat envelope.
func renderPlan(flags *Flags, rec *PlanRecord, sameVersion bool) error {
	if flags.JSON {
		return json.NewEncoder(os.Stdout).Encode(planJSONEnvelope{
			App:           rec.App,
			Server:        rec.Server,
			TargetVersion: rec.TargetVersion,
			VersionKnown:  rec.VersionKnown,
			SameVersion:   sameVersion,
			Changes:       rec.Effects.Containers,
			PlanID:        rec.PlanID,
			Destination:   rec.Destination,
			ConfigDigest:  rec.ConfigDigest,
			Image:         rec.Image,
			TargetState:   rec.TargetState,
			Effects:       rec.Effects,
			Unresolved:    rec.Unresolved,
		})
	}

	fmt.Printf("Plan %s for %s on %s\n", rec.PlanID, rec.App, rec.Server)
	if rec.Destination != "" {
		fmt.Printf("Destination overlay: %s\n", rec.Destination)
	}
	fmt.Printf("Config digest: %s\n", rec.ConfigDigest)
	fmt.Printf("Image: %s\n", describePlanImage(rec.Image))
	if rec.TargetState.Deployed {
		fmt.Printf("Target state: generation %d, deployed hash %s\n", rec.TargetState.Generation, rec.TargetState.CurrentHash)
	} else {
		fmt.Printf("Target state: not deployed (first deploy)\n")
	}
	if rec.TargetVersion != "" && rec.VersionKnown {
		fmt.Printf("Target version: %s\n", rec.TargetVersion)
	} else {
		fmt.Printf("Target version: (timestamp — not predictable; names below are indicative)\n")
	}

	printEffectSection("Containers", containerChangesAsEffects(rec.Effects.Containers))
	printEffectSection("Routing", rec.Effects.Routing)
	printEffectSection("Environment", rec.Effects.Env)
	printEffectSection("Storage", rec.Effects.Storage)
	printEffectSection("Resources", rec.Effects.Resources)
	printEffectSection("Accessories", rec.Effects.Accessories)

	if len(rec.Unresolved) > 0 {
		fmt.Println("\nUnresolved (not known at plan time):")
		for _, n := range rec.Unresolved {
			fmt.Printf("  ? %s\n", n)
		}
	}
	fmt.Println("\nThis is a preview. No changes were made. Run `teploy deploy` to apply, or `teploy apply <file>` for a bound plan.")
	return nil
}

// containerChangesAsEffects re-views container changes through the
// add/remove/change printer without changing their recorded vocabulary.
func containerChangesAsEffects(changes []planChange) []planEffect {
	out := make([]planEffect, 0, len(changes))
	for _, c := range changes {
		e := planEffect{Action: c.Action, Name: c.Name, Detail: c.Detail}
		switch c.Action {
		case "create":
			e.Action = "add"
		case "stop":
			e.Action = "remove"
		}
		out = append(out, e)
	}
	return out
}

func printEffectSection(title string, effects []planEffect) {
	fmt.Printf("\n%s:\n", title)
	if len(effects) == 0 {
		fmt.Println("  (no changes)")
		return
	}
	for _, e := range effects {
		line := fmt.Sprintf("  %-6s %-34s", e.Action+":", e.Name)
		if e.From != "" || e.To != "" {
			line += fmt.Sprintf(" %s -> %s", orDash(e.From), orDash(e.To))
		}
		if e.Detail != "" {
			line += " — " + e.Detail
		}
		fmt.Println(line)
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func describePlanImage(img PlanImageIdentity) string {
	switch img.Resolution {
	case imageUnresolvedAwaitingBuild:
		return fmt.Sprintf("built at deploy time from context %s (fingerprint %s)", orUnset(img.ContextPath), shortDigest(img.ContextFingerprint))
	case imageResolvedByDigest:
		return fmt.Sprintf("%s — resolved by digest (%s)", img.Ref, shortDigest(img.Digest))
	case imageResolvedByImageID:
		return fmt.Sprintf("%s — mutable tag, content resolved at plan time (%s)", img.Ref, shortDigest(img.Digest))
	default:
		return fmt.Sprintf("%s — UNRESOLVED mutable tag (content could not be resolved)", img.Ref)
	}
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

// planStatic reports the state of a static app and writes a bound plan
// record: identity + digest bind the config; the effect set is the swap
// (a per-file diff is future work). Static deploys currently leave no
// provenance receipt, so apply executes them without a plan-id stamp —
// recorded as a C05 tail.
func planStatic(ctx context.Context, flags *Flags, appCfg *config.AppConfig, executor ssh.Executor, destination, outFile string) error {
	_ = ctx
	current, _ := state.Read(ctx, executor, appCfg.App)

	version, err := gitShortHash()
	if err != nil {
		version = ""
	}
	_, manifestSHA, err := config.NormalizeAndDigest(appCfg, "")
	if err != nil {
		return fmt.Errorf("normalizing planned manifest: %w", err)
	}
	rec := &PlanRecord{
		SchemaVersion: PlanRecordSchemaVersion,
		App:           appCfg.App,
		Server:        executor.Host(),
		ServerName:    resolvePlanServerName(appCfg),
		Destination:   destination,
		TargetVersion: version,
		VersionKnown:  version != "",
		ConfigDigest:  manifestSHA,
		Image:         PlanImageIdentity{Resolution: imageUnresolvedAwaitingBuild, NeedsBuild: true},
		Effects:       PlanEffects{},
		Unresolved: []string{
			"static app — deploy rsyncs a new release and swaps the `current` symlink; per-file diffing is not yet computed",
			"static deploys write no provenance receipt — `teploy apply` binds this plan but cannot stamp the release with its plan id",
		},
	}
	if current != nil {
		rec.TargetState = PlanTargetState{Deployed: true, Generation: current.Generation, CurrentHash: current.CurrentHash, ManifestSHA256: current.ManifestSHA256}
	}
	rec.PlanID = computePlanID(rec)

	if flags.JSON {
		// Old static keys (app/server/type/note) unchanged; record
		// identity rides additively.
		return json.NewEncoder(os.Stdout).Encode(map[string]any{
			"app":           appCfg.App,
			"server":        executor.Host(),
			"type":          "static",
			"note":          "static apps deploy by rsyncing a new release and swapping the current symlink; per-file diff is not yet computed",
			"plan_id":       rec.PlanID,
			"config_digest": rec.ConfigDigest,
			"target_state":  rec.TargetState,
			"unresolved":    rec.Unresolved,
		})
	}
	fmt.Printf("Plan %s for %s on %s (static)\n\n", rec.PlanID, appCfg.App, executor.Host())
	fmt.Println("A deploy rsyncs a new release and swaps the `current` symlink.")
	fmt.Println("Per-file diffing for static apps is not yet implemented.")
	for _, n := range rec.Unresolved {
		fmt.Printf("? %s\n", n)
	}
	if outFile != "" {
		if err := savePlanFile(outFile, rec); err != nil {
			return err
		}
		fmt.Printf("\nPlan %s written to %s — apply with: teploy apply %s\n", rec.PlanID, outFile, outFile)
	}
	fmt.Println("\nThis is a preview. No changes were made.")
	return nil
}
