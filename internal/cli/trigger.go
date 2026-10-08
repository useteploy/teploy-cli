package cli

import (
	"context"
	"errors"
	"fmt"
	"github.com/useteploy/teploy/internal/caddy"
	"io"
	"reflect"
	"strings"
	"time"

	"encoding/json"
	"github.com/spf13/cobra"
	"github.com/useteploy/teploy/internal/build"
	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/deploy"
	"github.com/useteploy/teploy/internal/docker"
	"github.com/useteploy/teploy/internal/preview"
	"github.com/useteploy/teploy/internal/releasemeta"
	"github.com/useteploy/teploy/internal/ssh"
	"github.com/useteploy/teploy/internal/state"
	"github.com/useteploy/teploy/internal/trigger"
)

type triggerContextKey struct{}
type triggerOutputKey struct{}
type triggerTransportKey struct{}

func deployOutput(ctx context.Context) io.Writer {
	if out, ok := ctx.Value(triggerOutputKey{}).(io.Writer); ok {
		return out
	}
	return nil
}

// TriggerBindingDigest uses the SAME private PlanRecord2 algorithm with the
// admitted target substituted for mutable connection aliases. It never uses
// the public, redacted ManifestSHA256. Existing PlanRecord2 stays unchanged.
func TriggerBindingDigest(cfg *config.AppConfig, image, target string) (string, error) {
	bound := *cfg
	bound.Server = target
	bound.Servers = nil
	bound.User = ""
	return config.ExecutionBindingDigest(&bound, image)
}

// triggerCommand is the actual Cobra stdin/final-result boundary. Diagnostic
// writers are passed explicitly; production effect wrappers do not write to
// stdout. The final result is emitted even when decode/admission/effects fail.
func triggerCommand(cmd *cobra.Command, action string, run func(trigger.Request) (trigger.Result, error)) error {
	r, err := trigger.Decode(cmd.InOrStdin())
	result := trigger.Initial(r)
	if err == nil && r.Action != action {
		err = fmt.Errorf("request action disagrees with command")
	}
	if err != nil {
		result.ErrorCode = "invalid_request"
	} else {
		result, err = run(r)
	}
	if err != nil && result.ErrorCode == "" {
		result.ErrorCode = "admission_failed"
	}
	if encodeErr := json.NewEncoder(cmd.OutOrStdout()).Encode(result); encodeErr != nil {
		return errors.Join(err, encodeErr)
	}
	return err
}

func observeDeploy(ctx context.Context, exec ssh.Executor, app string, r trigger.Request) (*trigger.Result, error) {
	actual, err := state.Read(ctx, exec, app)
	if err != nil {
		return nil, err
	}
	if actual == nil || actual.TriggerOperationKey != r.OperationKey {
		return nil, nil
	}
	if actual.ExecutionBindingDigest != r.ExecutionBindingDigest || actual.SourceRevision != r.Commit || actual.ImageDigest == "" {
		return nil, fmt.Errorf("deployed authority binding/image mismatch")
	}

	observed := time.Now().UTC()
	gen := actual.Generation
	result := trigger.Result{SchemaVersion: 1, OperationKey: r.OperationKey, Publication: "committed", Reconciliation: "required", Generation: &gen, ReleaseHash: actual.CurrentHash, ImageDigest: actual.ImageDigest, AuthorityObservedAt: &observed}
	release, err := releasemeta.Read(ctx, exec, app, actual.CurrentHash)
	if err != nil {
		return &result, err
	}
	if release == nil || release.TriggerOperationKey != r.OperationKey || release.ExecutionBindingDigest != r.ExecutionBindingDigest || release.Hash != actual.CurrentHash || release.Generation != actual.Generation || release.ImageDigest != actual.ImageDigest {
		return &result, fmt.Errorf("immutable release completion unproven")
	}
	inventory, err := docker.NewClient(exec).ListContainers(ctx, app)
	if err != nil {
		return &result, err
	}
	web := false
	for _, container := range inventory {
		if container.Labels["teploy.version"] != actual.CurrentHash || container.Labels["teploy.process"] != "web" {
			continue
		}
		if container.State != "running" || container.ID == "" {
			return &result, fmt.Errorf("serving workload unproven")
		}
		digest, err := docker.NewClient(exec).ContainerImageDigest(ctx, container.ID)
		if err != nil || digest != actual.ImageDigest {
			return &result, errors.Join(fmt.Errorf("actual workload image mismatch"), err)
		}
		web = true
	}
	if !web {
		return &result, fmt.Errorf("serving web workload absent")
	}
	result.Reconciliation = "complete"

	// A committed state with failed generation-sidecar repair is not complete.
	sidecar, err := state.ReadCommittedGeneration(ctx, exec, app)
	if err != nil {
		result.Reconciliation = "required"
		return &result, err
	}
	if sidecar != gen {
		result.Reconciliation = "required"
		return &result, fmt.Errorf("generation repair required")
	}
	return &result, nil
}

// runTriggerDeployment keeps admission, preparations and publication under
// one target lease. The injected command tests exercise this wrapper through
// triggerCommand rather than bypassing the production adapter.
func runTriggerDeployment(ctx context.Context, exec ssh.Executor, lock *state.Lock, cfg *config.AppConfig, image, version string, r trigger.Request, out io.Writer) (trigger.Result, error) {
	result := trigger.Initial(r)
	if err := state.RequireLease(lock); err != nil {
		return result, err
	}
	fenced := &state.FencedExecutor{Executor: exec, Lock: lock}
	target, err := trigger.EnrolledTarget(ctx, fenced, cfg.App)
	if err != nil {
		return result, err
	}
	binding, err := TriggerBindingDigest(cfg, image, target)
	if err != nil {
		return result, err
	}
	if r.TargetID != target || r.ExecutionBindingDigest != binding || r.Commit != cfg.SourceRevision {
		result.ErrorCode = "identity_mismatch"
		return result, fmt.Errorf("target/source/private execution binding mismatch")
	}
	adapter := trigger.Adapter{Store: trigger.RemoteStore{Exec: fenced, App: cfg.App}, Evidence: func(ctx context.Context, r trigger.Request) (json.RawMessage, error) {
		e, err := readDeploymentEvidence(ctx, fenced, cfg.App)
		if e.Authority != nil && e.Authority.Generation != r.ExpectedGeneration {
			return nil, fmt.Errorf("application generation changed")
		}
		if e.Authority == nil && r.ExpectedGeneration != 0 {
			return nil, fmt.Errorf("application authority absent")
		}
		if err != nil {
			return nil, err
		}
		return json.Marshal(e)
	}, Observe: func(ctx context.Context, receipt trigger.Receipt) (*trigger.Result, error) {
		return observeDeploymentReceipt(ctx, fenced, cfg.App, receipt)
	}}
	needsBuild := image == ""
	return adapter.Run(ctx, r, func(ctx context.Context) error {
		if err := admitDeploymentVolumes(ctx, fenced, cfg); err != nil {
			return err
		}
		ctx = context.WithValue(ctx, triggerContextKey{}, r)
		ctx = context.WithValue(ctx, triggerOutputKey{}, out)
		if image == "" {
			mode, err := build.DetectAt(cfg.Context, cfg.Dockerfile)
			if err != nil {
				return err
			}
			_, _, key, err := config.ResolveServer(cfg.Server, "", "", "")
			host, user := exec.Host(), exec.User()
			if override, ok := ctx.Value(triggerTransportKey{}).(string); ok && override != "" {
				key = override
			}
			if err != nil {
				return err
			}
			att := releasemeta.MustAttempt(cfg.App, version)
			if cfg.BuildLocal {
				image, err = build.LocalBuild(ctx, build.LocalBuildConfig{App: cfg.App, Version: version, Mode: mode, Dir: ".", Context: cfg.Context, Dockerfile: cfg.Dockerfile, Host: host, User: user, KeyPath: key, Platform: cfg.Platform, Exec: fenced}, out)
			} else {
				dir, syncErr := syncAttemptBuildContext(ctx, fenced, cfg, att, mode, host, user, key, out, out)
				if syncErr != nil {
					return syncErr
				}
				image, err = build.NewBuilder(fenced, out).Build(ctx, build.BuildConfig{App: cfg.App, Version: version, Mode: mode, BuildDir: dir, Context: cfg.Context, Dockerfile: cfg.Dockerfile, Platform: cfg.Platform})
			}
			if err != nil {
				return err
			}
		}
		if err := ensureImage(ctx, docker.NewClient(fenced), image, out); err != nil {
			return err
		}
		att := releasemeta.MustAttempt(cfg.App, version)
		return deployBuiltImageFenced(ctx, exec, cfg, image, version, exec.Host(), false, needsBuild, ".", lock, &att, "")
	})
}

func runTriggerCLI(cmd *cobra.Command, flags *Flags, action, branch, image, version string, opts previewDeployOpts, r trigger.Request) (trigger.Result, error) {
	result := trigger.Initial(r)
	ctx := cmd.Context()
	cfg, err := config.LoadApp(".")
	if err != nil {
		return result, err
	}
	if err = resolveDeployEnv(ctx, cfg, flags.StrictEnv); err != nil {
		return result, err
	}
	if action != "preview_destroy" {
		if err := verifyCheckoutRequest(ctx, r); err != nil {
			return result, err
		}
		revision, revisionErr := gitRevisionIn(".")
		if revisionErr != nil || revision != r.Commit {
			return result, fmt.Errorf("checkout does not match full authenticated commit")
		}
		cfg.SourceRevision = revision
	}
	if image == "" {
		image = cfg.Image
	}
	if version == "" {
		version = r.Commit + "-" + r.OperationKey[:16]
	}
	exec, err := connectForApp(ctx, flags, cfg)
	if err != nil {
		return result, err
	}
	defer exec.Close()
	if err = docker.NewClient(exec).EnsureManagedDirectory(ctx, cfg.App, "", ""); err != nil {
		return result, err
	}
	lock, err := state.AcquireLockFenced(ctx, exec, cfg.App)
	if err != nil {
		return result, err
	}
	defer state.ReleaseLockFenced(exec, lock, cfg.App)
	lock.StartRenewal(exec)
	if action == "deploy" {
		if cfg.IsStatic() {
			result.ErrorCode = "unsupported_static_trigger"
			return result, fmt.Errorf("static trigger adapter is not yet implemented")
		}
		ctx = context.WithValue(ctx, triggerTransportKey{}, flags.Key)
		return runTriggerDeployment(ctx, exec, lock, cfg, image, version, r, cmd.ErrOrStderr())
	}
	fenced := &state.FencedExecutor{Executor: exec, Lock: lock}
	target, err := trigger.EnrolledTarget(ctx, fenced, cfg.App)
	if err != nil {
		return result, err
	}
	binding, err := TriggerBindingDigest(cfg, image, target)
	if err != nil {
		return result, err
	}
	if r.TargetID != target || (action != "preview_destroy" && r.ExecutionBindingDigest != binding) {
		return result, fmt.Errorf("target/private binding mismatch")
	}
	if strings.TrimPrefix(r.Ref, "refs/heads/") != branch {
		return result, fmt.Errorf("preview branch does not match exact request ref")
	}
	mgr := preview.NewManager(exec, cmd.ErrOrStderr()).WithLease(lock)
	adapter := trigger.Adapter{Store: trigger.RemoteStore{Exec: fenced, App: cfg.App}, Evidence: func(ctx context.Context, request trigger.Request) (json.RawMessage, error) {
		if request.Action != "preview_destroy" {
			return nil, nil
		}
		s, err := mgr.Observe(ctx, cfg.App, branch)
		if err != nil {
			return nil, err
		}
		if s == nil || s.OwnershipID != request.PreviewIdentity || s.Generation != request.ExpectedGeneration || s.ExecutionBindingDigest != request.ExecutionBindingDigest || s.SourceRevision != request.Commit || s.Repo != request.RepositoryID {
			return nil, fmt.Errorf("retained preview authority mismatch")
		}
		return json.Marshal(s)
	}, Observe: func(ctx context.Context, receipt trigger.Receipt) (*trigger.Result, error) {
		request := receipt.Request
		s, err := mgr.Observe(ctx, cfg.App, strings.TrimPrefix(request.Ref, "refs/heads/"))
		if err != nil {
			return nil, err
		}
		if request.Action == "preview_destroy" {
			var admitted preview.State
			if err := json.Unmarshal(receipt.Evidence, &admitted); err != nil {
				return nil, err
			}
			absent, err := mgr.ProveDestroyed(ctx, cfg.App, admitted)
			if err != nil || !absent {
				return nil, err
			}
			now := time.Now().UTC()
			gen := admitted.Generation
			return &trigger.Result{SchemaVersion: 1, OperationKey: request.OperationKey, Publication: "committed", Reconciliation: "complete", Generation: &gen, AuthorityObservedAt: &now}, nil
		}
		if s == nil {
			return nil, nil
		}
		if s.OperationKey != request.OperationKey {
			return nil, nil
		}
		if s.ExecutionBindingDigest != request.ExecutionBindingDigest || s.SourceRevision != request.Commit || s.OwnershipID != request.PreviewIdentity || s.Generation <= request.ExpectedGeneration || s.ImageDigest == "" {
			return nil, fmt.Errorf("preview completion identity unproven")
		}
		if err := mgr.ProveServing(ctx, cfg.App, *s); err != nil {
			return nil, err
		}
		now := time.Now().UTC()
		gen := s.Generation
		return &trigger.Result{SchemaVersion: 1, OperationKey: request.OperationKey, Publication: "committed", Reconciliation: "complete", Generation: &gen, ImageDigest: s.ImageDigest, AuthorityObservedAt: &now}, nil
	}}
	return adapter.Run(ctx, r, func(ctx context.Context) error {
		if action == "preview_destroy" {
			return mgr.CompareDestroyHeld(ctx, cfg.App, branch, r.PreviewIdentity, r.ExpectedGeneration, r.ExpectedPreviewUpdatedAt)
		}
		ttl, err := time.ParseDuration(opts.ttl)
		if err != nil {
			return err
		}
		if err := ensureImage(ctx, docker.NewClient(fenced), image, cmd.ErrOrStderr()); err != nil {
			return err
		}
		return mgr.DeployHeld(ctx, preview.DeployConfig{App: cfg.App, Domain: cfg.Domain, Branch: branch, Image: image, Version: version, TTL: ttl, Repo: r.RepositoryID, OwnershipID: r.PreviewIdentity, ExpectedGeneration: &r.ExpectedGeneration, OperationKey: r.OperationKey, ExecutionBindingDigest: binding, SourceRevision: r.Commit, BaseDomain: opts.baseDomain, HTTPOnly: opts.httpOnly, AllowIPs: opts.allowIPs})
	})
}

// Assert the deployment metadata always reaches the authoritative engine.
func attachTrigger(ctx context.Context, cfg *deploy.Config) {
	if r, ok := ctx.Value(triggerContextKey{}).(trigger.Request); ok {
		cfg.TriggerOperationKey = r.OperationKey
		cfg.ExecutionBindingDigest = r.ExecutionBindingDigest
		cfg.SourceRevision = r.Commit
	}
}

// deploymentEvidence captures authority plus exact immutable workload inventory
// BEFORE any preparation. It supports honest ordinary failed-health results.
type deploymentEvidence struct {
	Authority    *state.AppState    `json:"authority"`
	Containers   []docker.Container `json:"containers"`
	Route        string             `json:"route"`
	RoutePresent bool               `json:"route_present"`
}

func readDeploymentEvidence(ctx context.Context, exec ssh.Executor, app string) (deploymentEvidence, error) {
	var e deploymentEvidence
	var err error
	e.Authority, err = state.Read(ctx, exec, app)
	if err != nil {
		return e, err
	}
	e.Containers, err = docker.NewClient(exec).ListContainers(ctx, app)
	if err != nil {
		return e, err
	}
	e.Route, e.RoutePresent, err = caddy.NewClient(exec).ReadManagedBlock(ctx, app)
	return e, err
}
func observeDeploymentReceipt(ctx context.Context, exec ssh.Executor, app string, receipt trigger.Receipt) (*trigger.Result, error) {
	result, err := observeDeploy(ctx, exec, app, receipt.Request)
	if result != nil || err != nil {
		return result, err
	}
	var before deploymentEvidence
	if err = json.Unmarshal(receipt.Evidence, &before); err != nil {
		return nil, err
	}
	actual, err := readDeploymentEvidence(ctx, exec, app)
	if err != nil {
		return nil, err
	}
	// Comparison includes immutable IDs and running states. A candidate, route
	// switch, changed generation or unknown inventory cannot read as precommit.
	if !reflect.DeepEqual(before, actual) {
		return nil, nil
	}
	now := time.Now().UTC()
	return &trigger.Result{SchemaVersion: 1, OperationKey: receipt.Request.OperationKey, Publication: "not_committed", Reconciliation: "complete", AuthorityObservedAt: &now}, nil
}
