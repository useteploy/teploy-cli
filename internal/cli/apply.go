package cli

// `teploy apply <plan-file>` (C05): execute a reviewed plan through the
// EXISTING deploy engine — there is no side engine. Before anything
// runs, the plan's recorded binding (effective-config digest, target
// version, build inputs, target server/app identity, deployed-state
// generation) is re-derived and compared; any drift refuses the apply
// naming WHAT moved, with the remedy (re-plan). The plan id is stamped
// into the deploy's provenance receipt, tying the release back to the
// plan that was verified.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"

	"github.com/spf13/cobra"
	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/state"
)

func newApplyCmd(flags *Flags) *cobra.Command {
	var (
		migrateVolumes bool
		skipDNSCheck   bool
	)
	cmd := &cobra.Command{
		Use:   "apply <plan-file>",
		Short: "Execute a reviewed plan (refuses if anything drifted since it was made)",
		Long: "Executes a plan written by `teploy plan --out` through the normal deploy " +
			"engine. Before executing, the plan's binding is re-verified: the effective " +
			"config digest, the target version, the build inputs (for build plans), the " +
			"target server and app, and the deployed-state generation must all still " +
			"match what the plan recorded. Anything that moved — a config edit, a deploy " +
			"or rollback that ran in between, source changes without a version change — " +
			"invalidates the plan and the apply refuses with the remedy (re-plan).\n\n" +
			"Run from the same app directory the plan was made in (the plan records the " +
			"destination overlay it was computed with; do not pass a different one).",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runApply(flags, args[0], skipDNSCheck, migrateVolumes)
		},
	}
	cmd.Flags().BoolVar(&migrateVolumes, "migrate-volumes", false, "auto-migrate data from foreign volume sources to teploy paths (cp -a) — mirror of deploy --migrate-volumes")
	cmd.Flags().BoolVar(&skipDNSCheck, "skip-dns-check", false, "skip DNS validation (for proxied domains like Cloudflare) — mirror of deploy --skip-dns-check")
	return cmd
}

func runApply(flags *Flags, planPath string, skipDNSCheck, migrateVolumes bool) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	rec, err := loadPlanFile(planPath)
	if err != nil {
		return refuseApply(err)
	}
	// A plan whose target version is a deploy-time timestamp can never be
	// bound — the thing apply would deploy is not the thing reviewed.
	if !rec.VersionKnown {
		return refuseApply(fmt.Errorf("plan %s targets an unpredictable version (floating image tag) — re-plan with --version or a digest-pinned image", rec.PlanID))
	}

	// Load the config exactly as the plan did (same loader, same overlay),
	// then resolve env the same way `deploy` does — the shared
	// resolveDeployEnv, not a fork.
	appCfg, image, curDigest, curVersion, err := applyResolveCurrent(ctx, flags, rec)
	if err != nil {
		return err
	}

	// Connect to the plan's target.
	serverName := rec.ServerName
	if serverName == "" {
		serverName = resolvePlanServerName(appCfg)
	}
	executor, err := connectForApp(ctx, flags, appCfg)
	if err != nil {
		return err
	}
	defer executor.Close()

	current, err := state.Read(ctx, executor, appCfg.App)
	if err != nil {
		return err
	}

	// Build-input re-verification for build plans: same C04 resolution
	// path the plan used, compared on the fingerprint + Dockerfile
	// identity. This catches edits the version cannot see (a dirty-tree
	// change between plan and apply).
	var curFingerprint, curDockerfileSHA string
	if rec.Image.NeedsBuild {
		prov := resolveDeployProvenance(ctx, executor, io.Discard, appCfg, ".", image, rec.TargetVersion, curDigest, true)
		curFingerprint = prov.ContextFingerprint
		curDockerfileSHA = prov.DockerfileSHA256
	}

	if err := verifyPlanBinding(rec, planCurrentFacts{
		App:                appCfg.App,
		Server:             executor.Host(),
		Version:            curVersion,
		ConfigDigest:       curDigest,
		ContextFingerprint: curFingerprint,
		DockerfileSHA256:   curDockerfileSHA,
		State:              current,
	}); err != nil {
		return refuseApply(err)
	}

	if !flags.JSON {
		fmt.Printf("Plan %s verified (config digest %s, target %s generation %d) — executing through the deploy engine\n",
			rec.PlanID, shortDigest(rec.ConfigDigest), rec.Server, rec.TargetState.Generation)
	}

	// ONE execution path: the same deployAppConfig a direct `teploy
	// deploy` runs, with the plan's recorded image/version/overlay. The
	// plan id rides along and is stamped into the provenance receipt.
	err = deployAppConfig(flags, appCfg, serverName, image, rec.TargetVersion, skipDNSCheck, migrateVolumes, rec.PlanID)
	if err != nil {
		return err
	}
	if !flags.JSON {
		fmt.Printf("Plan %s applied — the release receipt carries this plan id (provenance.plan_id)\n", rec.PlanID)
	}
	return nil
}

// applyResolveCurrent loads the current world exactly as the plan did
// and re-derives the pre-connect binding facts: the app config (loader +
// recorded overlay + env resolution, identical to deploy — one
// resolution semantics), the effective-config digest over the plan's
// image reference (recomputable before anything executes), and the
// re-derived target version (explicit versions bind as-is; derived ones
// must re-derive, or the world the plan reviewed is gone). Everything
// here is local — no server contact — so the config-drift refusal fires
// before a connection is even opened.
func applyResolveCurrent(ctx context.Context, flags *Flags, rec *PlanRecord) (appCfg *config.AppConfig, image, curDigest, curVersion string, err error) {
	if rec.Destination != "" {
		appCfg, err = config.LoadAppWithDestination(".", rec.Destination, config.OverlayOptions{Strict: flags.StrictEnv})
	} else {
		appCfg, err = config.LoadApp(".")
	}
	if err != nil {
		return nil, "", "", "", err
	}
	if revision, revisionErr := gitRevisionIn("."); revisionErr == nil {
		appCfg.SourceRevision = revision
	}
	if err := resolveDeployEnv(ctx, appCfg, flags.StrictEnv); err != nil {
		return nil, "", "", "", err
	}

	image = rec.Image.Ref
	_, curDigest, err = config.NormalizeAndDigest(appCfg, image)
	if err != nil {
		return nil, "", "", "", fmt.Errorf("normalizing current config: %w", err)
	}

	curVersion = rec.TargetVersion
	if !rec.VersionExplicit {
		if image != "" {
			curVersion = versionFromImage(image)
			if curVersion == "" {
				curVersion = "<unresolvable>"
			}
		} else {
			if curVersion, err = gitShortHash(); err != nil {
				return nil, "", "", "", fmt.Errorf("could not re-derive the target version from git: %w", err)
			}
		}
	}
	return appCfg, image, curDigest, curVersion, nil
}

// refuseApply marks an apply refusal. Drift refusals classify as the
// error-envelope conflict code (the request is coherent; the world
// moved); the rest stay admission-shaped.
func refuseApply(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, errPlanDrift) {
		return err
	}
	return refuseAdmission(err)
}
