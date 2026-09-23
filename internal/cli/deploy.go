package cli

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/useteploy/teploy/internal/accessories"
	"github.com/useteploy/teploy/internal/audit"
	"github.com/useteploy/teploy/internal/build"
	"github.com/useteploy/teploy/internal/caddy"
	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/deploy"
	"github.com/useteploy/teploy/internal/dns"
	"github.com/useteploy/teploy/internal/docker"
	"github.com/useteploy/teploy/internal/env"
	"github.com/useteploy/teploy/internal/multideploy"
	"github.com/useteploy/teploy/internal/notify"
	"github.com/useteploy/teploy/internal/releasemeta"
	"github.com/useteploy/teploy/internal/secret"
	"github.com/useteploy/teploy/internal/ssh"
	"github.com/useteploy/teploy/internal/state"
)

func newDeployCmd(flags *Flags) *cobra.Command {
	var (
		image          string
		version        string
		skipDNSCheck   bool
		parallel       int
		destination    string
		appName        string
		domain         string
		port           int
		migrateVolumes bool
		role           string
		tagFilters     []string
	)

	cmd := &cobra.Command{
		Use:   "deploy [server]",
		Short: "Deploy the app to a server",
		Long: `Start a new container with health checking, route traffic via Caddy, and stop the old container — zero downtime.
Use -d to deploy with a destination overlay (e.g. -d staging merges teploy.staging.yml).

For ad-hoc deploys without a teploy.yml (e.g. from teploy-dash), pass --app, --image, and --domain:
  teploy deploy myserver --app myapp --image nginx:latest --domain app.example.com

If a previously-deployed container mounted volumes from a different host path
(common when migrating from Dokploy or hand-rolled docker run setups), the
deploy aborts safely rather than orphaning data. Pass --migrate-volumes to
copy data from the existing source into the teploy-expected path before
swapping traffic.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var serverName string
			if len(args) > 0 {
				serverName = args[0]
			}
			if appName != "" {
				return runAdHocDeploy(flags, serverName, appName, image, domain, port, version, skipDNSCheck, migrateVolumes)
			}
			tags, err := parseTagFilters(tagFilters)
			if err != nil {
				return refuseAdmission(err)
			}
			return runDeploy(flags, serverName, image, version, skipDNSCheck, parallel, destination, migrateVolumes, role, tags)
		},
	}

	cmd.Flags().StringVar(&image, "image", "", "Docker image to deploy (skips build if set)")
	cmd.Flags().StringVar(&version, "version", "", "version identifier (default: git short hash)")
	cmd.Flags().BoolVar(&skipDNSCheck, "skip-dns-check", false, "skip DNS validation (for proxied domains like Cloudflare)")
	cmd.Flags().IntVar(&parallel, "parallel", 0, "max concurrent deploys for multi-server (default: from teploy.yml or 1)")
	cmd.Flags().StringVarP(&destination, "destination", "d", "", "destination overlay (e.g. staging merges teploy.staging.yml)")
	cmd.Flags().StringVar(&appName, "app", "", "app name for ad-hoc deploy (bypasses teploy.yml)")
	cmd.Flags().StringVar(&domain, "domain", "", "domain for ad-hoc deploy")
	cmd.Flags().IntVar(&port, "port", 80, "container port for ad-hoc deploy")
	cmd.Flags().BoolVar(&migrateVolumes, "migrate-volumes", false, "auto-migrate data from foreign volume sources to teploy paths (cp -a)")
	cmd.Flags().StringVar(&role, "role", "", "only deploy to servers with this role (from servers.yml)")
	cmd.Flags().StringSliceVar(&tagFilters, "tag", nil, "only deploy to servers matching tag key=value (repeatable)")

	return cmd
}

// runAdHocDeploy handles deploys without a teploy.yml — used by teploy-dash
// and scripting. Requires --app and --image at minimum.
func runAdHocDeploy(flags *Flags, serverName, appName, image, domain string, port int, version string, skipDNSCheck, migrateVolumes bool) error {
	if image == "" {
		return refuseAdmission(fmt.Errorf("--image is required for ad-hoc deploy (no teploy.yml)"))
	}
	// This path builds an AppConfig directly instead of going through
	// config.LoadApp, so it never reaches AppConfig.validate() — app and
	// domain must be validated explicitly here before either one reaches
	// the network (state paths, remote shell commands, Caddyfile content).
	if err := config.ValidateName(appName); err != nil {
		return refuseAdmission(err)
	}
	if err := config.ValidateDomain(domain, false); err != nil {
		return refuseAdmission(err)
	}
	if serverName == "" {
		serverName = flags.Host
	}
	if serverName == "" {
		return refuseAdmission(fmt.Errorf("server is required — use 'teploy deploy <server> --app ...' or --host"))
	}
	if port <= 0 {
		port = 80
	}

	appCfg := &config.AppConfig{
		App:    appName,
		Image:  image,
		Domain: domain,
		Port:   port,
		Server: serverName,
	}

	return deployAppConfig(flags, appCfg, serverName, image, version, skipDNSCheck, migrateVolumes)
}

func runDeploy(flags *Flags, serverName, image, version string, skipDNSCheck bool, parallel int, destination string, migrateVolumes bool, role string, tags map[string]string) error {
	// Binds local work (env-file decryption subprocesses) to the operator's
	// Ctrl-C from the very start of the run (audit F75).
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	// 1. Load teploy.yml from current directory (with optional destination overlay).
	var appCfg *config.AppConfig
	var err error
	if destination != "" {
		appCfg, err = config.LoadAppWithDestination(".", destination, config.OverlayOptions{Strict: flags.StrictEnv})
	} else {
		appCfg, err = config.LoadApp(".")
	}
	if err != nil {
		// Zero-config first run: nothing here at all (not a malformed file —
		// those keep erroring as-is), plain deploy, and a human at the
		// terminal. Offer to create the config inline, then continue.
		if destination == "" && errors.Is(err, config.ErrNoConfig) {
			if !stdinIsTerminal() {
				return fmt.Errorf("%w (run `teploy init` to create a config)", err)
			}
			fmt.Println("No teploy config found in this directory — let's create one.")
			if _, ierr := initFlow(bufio.NewReader(os.Stdin), ".", false, false); ierr != nil {
				return ierr
			}
			appCfg, err = config.LoadApp(".")
		}
		if err != nil {
			return err
		}
	}
	if revision, revisionErr := gitRevisionIn("."); revisionErr == nil {
		appCfg.SourceRevision = revision
	}

	// Narrow the target list by --role/--tag (no-op if neither is set). Only
	// affects the multi-server list; an explicit `deploy <server>` arg wins.
	if err := filterServersByRoleTag(appCfg, role, tags); err != nil {
		return err
	}

	// Resolve env_files (SOPS/age-encrypted or plain, decrypted locally)
	// once, before dispatch — both the single- and multi-server paths then
	// pick them up from appCfg.Env. Explicit env: keys win over file values.
	//
	// ${VAR} interpolation applies ONLY to the explicit YAML env: templates,
	// expanded exactly once HERE — before file values merge in. Decrypting a
	// file whose password contains a literal $ used to hand it to
	// os.Expand at serialization time and silently alter it based on the
	// operator's environment (audit F59); file/secret values are literal.
	// --strict-env (F57/TCL-32) turns an unset ${VAR} into a listed failure
	// instead of a silent empty expansion.
	if err := expandEnvTemplates(appCfg.Env, flags.StrictEnv); err != nil {
		return err
	}
	if len(appCfg.EnvFiles) > 0 {
		fileVars, err := env.LoadLocalEnvFiles(ctx, ".", appCfg.EnvFiles)
		if err != nil {
			return err
		}
		if appCfg.Env == nil {
			appCfg.Env = map[string]string{}
		}
		for k, v := range fileVars {
			if _, explicit := appCfg.Env[k]; !explicit {
				appCfg.Env[k] = v
			}
		}
	}

	// Multi-server deploy: if teploy.yml lists multiple servers and no explicit
	// server argument was provided, deploy to all of them in parallel.
	if len(appCfg.Servers) > 1 && serverName == "" {
		return runMultiDeploy(flags, appCfg, image, version, skipDNSCheck, parallel, migrateVolumes)
	}

	return deployAppConfig(flags, appCfg, serverName, image, version, skipDNSCheck, migrateVolumes)
}

// parseTagFilters parses repeated "key=value" flag values into a map.
func parseTagFilters(pairs []string) (map[string]string, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	m := make(map[string]string, len(pairs))
	for _, p := range pairs {
		k, v, ok := strings.Cut(p, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("invalid --tag %q (want key=value)", p)
		}
		m[k] = v
	}
	return m, nil
}

// filterServersByRoleTag narrows appCfg.Servers to the servers whose
// servers.yml entry matches the given role (empty role defaults to "app") and
// every given tag. A no-op when neither role nor tags are set — so default
// behavior (deploy to all listed servers) is unchanged. Errors if the filter
// matches nothing, so a typo'd role/tag fails loudly instead of silently
// deploying nowhere.
func filterServersByRoleTag(appCfg *config.AppConfig, role string, tags map[string]string) error {
	if role == "" && len(tags) == 0 {
		return nil
	}
	serversPath, err := config.DefaultServersPath()
	if err != nil {
		return err
	}
	all, err := config.ListServers(serversPath)
	if err != nil {
		return err
	}

	kept := selectServersByRoleTag(appCfg.Servers, all, role, tags)
	if len(kept) == 0 {
		return fmt.Errorf("no servers match the --role/--tag filter")
	}
	appCfg.Servers = kept
	return nil
}

// selectServersByRoleTag is the pure matcher behind filterServersByRoleTag:
// from names, keep those whose servers.yml entry matches role (empty → "app")
// and every tag.
func selectServersByRoleTag(names []string, all map[string]config.Server, role string, tags map[string]string) []string {
	var kept []string
	for _, name := range names {
		srv, ok := all[name]
		if !ok {
			continue
		}
		if role != "" {
			r := srv.Role
			if r == "" {
				r = "app"
			}
			if r != role {
				continue
			}
		}
		matches := true
		for k, v := range tags {
			if srv.Tags[k] != v {
				matches = false
				break
			}
		}
		if matches {
			kept = append(kept, name)
		}
	}
	return kept
}

// deployAppConfig runs a single-server deploy from an already-loaded AppConfig.
// Used by both runDeploy (from teploy.yml) and runTemplateInstall (from template).
func deployAppConfig(flags *Flags, appCfg *config.AppConfig, serverName, image, version string, skipDNSCheck, migrateVolumes bool) error {
	var err error

	// 2. Resolve server (single-server deploy).
	if serverName == "" {
		// If there's exactly one server in the servers list, use it.
		if len(appCfg.Servers) == 1 {
			serverName = appCfg.Servers[0]
		} else {
			serverName = appCfg.Server
		}
	}
	if serverName == "" {
		return refuseAdmission(fmt.Errorf("no server specified — use 'teploy deploy <server>' or set 'server' in teploy.yml"))
	}

	host, user, key, err := config.ResolveServer(serverName, flags.Host, flags.User, flags.Key)
	if err != nil {
		return err
	}
	user = config.EffectiveUser(user, flags.User, appCfg.User)

	// Static deploys take an entirely different path (no docker, no image,
	// no health checks) so we branch here before the container build/run
	// machinery runs.
	if appCfg.IsStatic() {
		return runStaticDeploy(appCfg, host, user, key)
	}

	// 3. Resolve image.
	if image == "" {
		image = appCfg.Image
	}

	// 4. Resolve version.
	//
	// Order matters. An explicit --version always wins (validated first —
	// it is interpolated unquoted into remote commands, audit F18).
	// Otherwise, when a
	// prebuilt image is being deployed, the version comes from THE IMAGE, not
	// from git: the image is what actually runs, and git HEAD is merely
	// whatever the operator's working directory happened to be on. Only a
	// build-from-source deploy falls through to the git hash, which is correct
	// there because the source and the artifact are the same thing.
	if version == "" {
		if image != "" {
			version = versionFromImage(image)
			if version == "" {
				// A floating or digest-less reference carries no usable
				// version; a timestamp is at least unique per deploy.
				version = fmt.Sprintf("%d", time.Now().Unix())
			}
		} else {
			version, err = gitShortHash()
			if err != nil {
				return fmt.Errorf("could not determine version from git: %w (use --version flag)", err)
			}
		}
	} else if err := validateVersionArg(version); err != nil {
		return refuseAdmission(err)
	}

	// 5. Detect build mode (when no pre-built image). Honors the optional
	// 'context'/'dockerfile' fields so a monorepo's subdir Dockerfile is
	// found (and an explicitly-named-but-missing one errors, rather than
	// silently falling back to Nixpacks).
	needsBuild := image == ""
	var buildMode build.Mode
	if needsBuild {
		buildMode, err = build.DetectAt(appCfg.Context, appCfg.Dockerfile)
		if err != nil {
			return err
		}
		fmt.Printf("No image specified — detected %s build\n", buildMode)
	}

	// 6. Connect to server.
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	fmt.Printf("Connecting to %s@%s...\n", user, host)

	executor, err := ssh.Connect(ctx, ssh.ConnectConfig{
		Host:    host,
		User:    user,
		KeyPath: key,
	})
	if err != nil {
		return err
	}
	defer executor.Close()

	// 6a. Take the deploy lease NOW, before any artifact is generated
	// (F08): the attempt's build context, env file, and TLS cert/key are
	// written under this lease, so concurrent attempts of the same app
	// serialize at the source instead of interleaving writes onto shared
	// per-app paths. The lease is fenced (F16) and renewed in the
	// background; it is released when deployAppConfig returns.
	if err := state.EnsureAppDir(ctx, executor, appCfg.App); err != nil {
		return fmt.Errorf("creating app directory: %w", err)
	}
	lk, err := state.AcquireLockFenced(ctx, executor, appCfg.App)
	if err != nil {
		return err
	}
	defer state.ReleaseLockFenced(executor, lk, appCfg.App)
	lk.StartRenewal(executor)

	// The attempt keys every artifact this deploy generates (F08):
	// immutable per (release, attempt), so the F14 record's references
	// name exactly the bytes that were deployed.
	att := releasemeta.MustAttempt(appCfg.App, version)

	// 6b. Ensure the pre-built image is available (CI pipeline mode). An image
	// already present on the server — built or `docker load`ed out of band, and
	// possibly in no registry at all — must not be re-pulled, or the deploy
	// would fail with "pull access denied" for something already on disk.
	if !needsBuild && image != "" {
		dk := docker.NewClient(executor)
		if err := ensureImage(ctx, dk, image, os.Stdout); err != nil {
			return err
		}
	}

	// 7. Build (if no pre-built image).
	if needsBuild {
		if appCfg.BuildLocal {
			// Local build mode: build on this machine, stream to server.
			fmt.Println("Building image locally...")
			image, err = build.LocalBuild(ctx, build.LocalBuildConfig{
				App:        appCfg.App,
				Version:    version,
				Mode:       buildMode,
				Dir:        ".",
				Context:    appCfg.Context,
				Dockerfile: appCfg.Dockerfile,
				Host:       host,
				User:       user,
				KeyPath:    key,
				Platform:   appCfg.Platform,
				Exec:       executor,
			}, os.Stdout)
			if err != nil {
				return fmt.Errorf("local build: %w", err)
			}
		} else {
			// Server build mode: rsync into the ATTEMPT's build context
			// (F08) — a directory no other attempt writes — with the
			// previous attempt's build dir as rsync's --link-dest basis so
			// the fresh directory still transfers incrementally and
			// hardlink-shares unchanged files.
			remoteDir := att.BuildDir()
			if _, err := executor.Run(ctx, "mkdir -p "+remoteDir); err != nil {
				return fmt.Errorf("creating build directory: %w", err)
			}

			fmt.Println("Syncing source to server...")
			excludes, err := build.LoadIgnore(".")
			if err != nil {
				return fmt.Errorf("loading ignore rules: %w", err)
			}
			if err := build.Sync(ctx, build.SyncConfig{
				LocalDir:  ".",
				RemoteDir: remoteDir,
				Host:      host,
				User:      user,
				KeyPath:   key,
				Excludes:  excludes,
				LinkDest:  releasemeta.PreviousAttemptBuildDir(ctx, executor, appCfg.App, att.ID),
			}, os.Stdout, os.Stderr); err != nil {
				return fmt.Errorf("syncing source: %w", err)
			}

			fmt.Println("Building image on server...")
			builder := build.NewBuilder(executor, os.Stdout)
			image, err = builder.Build(ctx, build.BuildConfig{
				App:        appCfg.App,
				Version:    version,
				Mode:       buildMode,
				BuildDir:   remoteDir,
				Context:    appCfg.Context,
				Dockerfile: appCfg.Dockerfile,
				Platform:   appCfg.Platform,
			})
			if err != nil {
				return fmt.Errorf("building image: %w", err)
			}
		}
		fmt.Printf("Built image: %s\n", image)
	}

	// 8. DNS validation (first deploy only).
	// Skipped for host ingress (publishes a raw port, no domain) and for
	// external ingress (the user's own front — Cloudflare Tunnel / nginx /
	// ALB — serves the domain, so it resolves there, not to the server IP).
	if !skipDNSCheck && appCfg.Domain != "" &&
		appCfg.Ingress != config.IngressHost && appCfg.Ingress != config.IngressExternal {
		current, _ := state.Read(ctx, executor, appCfg.App)
		if current == nil {
			fmt.Println("Validating DNS...")
			if err := dns.Validate(appCfg.Domain, host, nil); err != nil {
				return err
			}
			fmt.Println("  DNS validated")
		}
	}

	return deployBuiltImageFenced(ctx, executor, appCfg, image, version, host, migrateVolumes, needsBuild, ".", lk, &att)
}

// deployBuiltImage runs the shared post-build deploy orchestration:
// ensuring accessories, decrypting secrets, resolving volumes/TLS, and
// calling the actual deploy.Deployer.Deploy — the core both
// deployAppConfig (SSH from an operator's machine) and `teploy autodeploy
// serve` (running locally on the server via an ssh.LocalExecutor — see
// runAutodeployServe in autodeploy_serve.go) share, so a fix here (e.g.
// the secrets-via-env-file fix in buildContainerEnvFiles) can never
// silently apply to only one of the two trigger paths the way the old
// generated-bash-script autodeploy implementation did.
//
// executor must already be connected/ready; image and version must already
// be resolved (built or pulled pre-built). serverDisplay is a display-only
// string for the notification payload (a hostname for the SSH path,
// "localhost" for the resident-server path).
func deployBuiltImage(ctx context.Context, executor ssh.Executor, appCfg *config.AppConfig, image, version, serverDisplay string, migrateVolumes, needsBuild bool) error {
	return deployBuiltImageFenced(ctx, executor, appCfg, image, version, serverDisplay, migrateVolumes, needsBuild, "", nil, nil)
}

// deployBuiltImageFenced is deployBuiltImage with the caller's lease and
// attempt: lk is a lock the caller already owns (the terminal path's early
// lease and the resident autodeploy path — audits F07/F08) and att keys the
// attempt-scoped artifacts (env file, TLS). lk == nil means Deployer.Deploy
// acquires the lock itself (att must still be non-nil for the env file).
// sourceRoot is the directory the source was synced/built from ("." for
// manual deploys, the fetched checkout for autodeploy) — it keys the
// plan-time provenance (C04); empty means no build provenance.
func deployBuiltImageFenced(ctx context.Context, executor ssh.Executor, appCfg *config.AppConfig, image, version, serverDisplay string, migrateVolumes, needsBuild bool, sourceRoot string, lk *state.Lock, att *releasemeta.Attempt) error {
	if att == nil {
		attVal := releasemeta.MustAttempt(appCfg.App, version)
		att = &attVal
	}
	appliedManifest, manifestSHA256, err := config.NormalizeAndDigest(appCfg, image)
	if err != nil {
		return fmt.Errorf("normalizing applied manifest: %w", err)
	}

	// 8b. Capture and persist the plan-time provenance (C04): revision,
	// worktree cleanliness, build-context fingerprint, Dockerfile
	// identity, platform, the resolved image digest, and the mutability
	// of the requested ref — resolved BEFORE execution and filed into the
	// attempt's write-once namespace, next to the build context it
	// describes. Best-effort resolution, but the receipt itself must
	// land: a missing provenance file is an unwitnessed plan (warned).
	prov := resolveDeployProvenance(ctx, executor, os.Stdout, appCfg, sourceRoot, image, version, manifestSHA256, needsBuild)
	if err := releasemeta.WriteAttemptProvenance(ctx, executor, *att, prov); err != nil {
		fmt.Printf("Warning: could not persist the deploy provenance receipt for %s@%s: %v\n", appCfg.App, version, err)
	}

	// 9. Ensure accessories are running.
	var envFile string
	if len(appCfg.Accessories) > 0 {
		fmt.Println("Ensuring accessories...")
		accMgr := accessories.NewManager(executor, os.Stdout)
		allVars := make(map[string]string)
		for _, name := range sortedAccessoryNames(appCfg.Accessories) {
			vars, err := accMgr.EnsureRunning(ctx, appCfg.App, name, appCfg.Accessories[name])
			if err != nil {
				return fmt.Errorf("accessory %s: %w", name, err)
			}
			for k, v := range vars {
				allVars[k] = v
			}
		}
		if len(allVars) > 0 {
			if err := accMgr.InjectEnvVars(ctx, appCfg.App, allVars); err != nil {
				return fmt.Errorf("injecting accessory env vars: %w", err)
			}
		}
	}

	// Check if .env file exists on server.
	envPath := fmt.Sprintf("/deployments/%s/.env", appCfg.App)
	if _, err := executor.Run(ctx, "test -f "+envPath); err == nil {
		envFile = envPath
	}

	// Decrypt secrets (set via `teploy secret set`) and inject as -e
	// container args. Previously secrets were stored encrypted but never
	// surfaced to containers — apps reading process.env.KEY would get
	// undefined for anything set via `teploy secret`, defeating the
	// "encrypted at rest" workflow's whole point.
	secretMgr := secret.NewManager(executor)
	deploySecrets, err := secretMgr.DecryptAll(ctx, appCfg.App)
	if err != nil {
		return fmt.Errorf("decrypting secrets: %w", err)
	}
	// Resolve any `vault:<name>#<key>` references in env: from OpenBao and merge
	// them in (they win over plaintext env, same as decrypted secrets).
	deploySecrets, err = mergeSecretVaultRefs(ctx, executor, appCfg, deploySecrets)
	if err != nil {
		return err
	}

	// 10. Resolve persistent volumes.
	var volumes map[string]string
	if len(appCfg.Volumes) > 0 {
		volumes = plannedVolumeMounts(appCfg.App, appCfg.Volumes)
		for _, hostPath := range managedVolumeHostPaths(appCfg.App, appCfg.Volumes) {
			if _, err := executor.Run(ctx, "mkdir -p "+hostPath); err != nil {
				return fmt.Errorf("creating volume directory %s: %w", hostPath, err)
			}
		}
	}

	// 10-secret. If the OpenBao Agent sidecar is enabled, (re)deploy it and mount
	// the shared secrets volume into the app.
	volumes, err = ensureSecretAgent(ctx, executor, appCfg, volumes)
	if err != nil {
		return err
	}

	// 10a. Detect any existing container whose volume mount source doesn't match
	// what teploy resolved above. Without this check, redeploying an app that
	// was originally launched with a Docker named volume (or any non-teploy
	// bind mount) would silently create empty bind mounts, swap traffic, and
	// orphan the data. See tyler/teploy-cli#1.
	if len(volumes) > 0 {
		dockerClient := docker.NewClient(executor)
		mismatches, err := dockerClient.DetectVolumeMismatches(ctx, appCfg.App, volumes)
		if err != nil {
			return fmt.Errorf("checking existing volume mounts: %w", err)
		}
		if len(mismatches) > 0 {
			if !migrateVolumes {
				return docker.FormatMismatchError(appCfg.App, mismatches)
			}
			if err := docker.MigrateVolumes(ctx, executor, appCfg.App, mismatches, os.Stdout); err != nil {
				return fmt.Errorf("migrating volumes: %w", err)
			}
		}
	}

	// 10b. Upload custom TLS cert (if configured) so Caddy can terminate
	// HTTPS with it instead of ACME — required behind Cloudflare proxy/Tunnel.
	// Attempt-scoped (F08): the cert bytes this deploy references are
	// immutable for the release.
	tlsCert, tlsKey, tlsInternal, err := resolveAppTLS(ctx, executor, appCfg, att)
	if err != nil {
		return err
	}
	if tlsCert != "" {
		fmt.Println("  TLS certificate uploaded")
	}

	// Container env: teploy.yml's `env:` block plus decrypted secrets,
	// uploaded to the ATTEMPT's env file (F08) rather than passed as
	// `docker run -e` args — see buildContainerEnvFiles for why.
	envFiles, err := buildContainerEnvFiles(ctx, executor, appCfg.App, att, envFile, appCfg.Env, nil, deploySecrets)
	if err != nil {
		return err
	}

	// 11. Deploy.
	deployer := deploy.NewDeployer(executor, os.Stdout)
	deployCfg := deployConfigFromApp(appCfg, image, version, envFiles, volumes, tlsCert, tlsKey, tlsInternal, appliedManifest, manifestSHA256)
	deployCfg.Provenance = prov

	// Vulnerability gate: scan the image on the server before any container
	// starts — fixable CRITICALs block the deploy.
	if appCfg.Scan {
		fmt.Println("Scanning image for vulnerabilities (trivy)...")
		if err := docker.NewClient(executor).ScanImage(ctx, image, os.Stdout); err != nil {
			return err
		}
	}

	multiNotifier := buildNotifier(appCfg)
	var deployErr error
	if lk != nil {
		deployErr = deployer.DeployFenced(ctx, deployCfg, lk)
	} else {
		deployErr = deployer.Deploy(ctx, deployCfg)
	}

	// 12. Send notification (fire-and-forget).
	if multiNotifier != nil {
		msg := fmt.Sprintf("Deployed %s version %s", appCfg.App, version)
		if deployErr != nil {
			msg = fmt.Sprintf("Deploy failed for %s: %s", appCfg.App, deployErr)
		}
		if errs := multiNotifier.Send(ctx, notify.Payload{
			App:     appCfg.App,
			Server:  serverDisplay,
			Type:    "deploy",
			Success: deployErr == nil,
			Hash:    version,
			Message: msg,
		}); len(errs) > 0 {
			for _, e := range errs {
				fmt.Fprintf(os.Stderr, "Warning: notification failed: %v\n", e)
			}
		}
	}

	// Emit a deploy audit event to observe (fire-and-forget; no-op unless
	// `audit:` is configured). Captures who shipped what version where.
	emitDeployAudit(ctx, appCfg, "deploy.run", version, serverDisplay, deployErr)

	if deployErr != nil {
		return deployErr
	}

	// 13. Prune old build images (best-effort).
	if needsBuild {
		builder := build.NewBuilder(executor, os.Stdout)
		builder.PruneImages(ctx, appCfg.App)
	}

	return nil
}

// plannedVolumeMounts resolves config volume declarations to the docker
// mount map a deploy uses: a host bind (name starting with "/") mounts a
// directory the operator owns, exactly as given — teploy never creates or
// relocates it (it may hold a clone with credentials, or anything else
// that is not app data); every other name is a teploy-managed volume at
// /deployments/<app>/volumes/<name>. Single home: the deploy path and the
// plan storage effects both resolve through it, so a plan can never
// describe a different host layout than the deploy creates.
func plannedVolumeMounts(app string, cfgVolumes map[string]string) map[string]string {
	volumes := make(map[string]string, len(cfgVolumes))
	for name, containerPath := range cfgVolumes {
		if config.IsHostBindVolume(name) {
			volumes[name] = containerPath
			continue
		}
		volumes[fmt.Sprintf("/deployments/%s/volumes/%s", app, name)] = containerPath
	}
	return volumes
}

// managedVolumeHostPaths lists the teploy-managed host paths behind
// named volumes (the ones the deploy mkdir's). Host binds are absent —
// they are the operator's directories.
func managedVolumeHostPaths(app string, cfgVolumes map[string]string) []string {
	var paths []string
	for name := range cfgVolumes {
		if !config.IsHostBindVolume(name) {
			paths = append(paths, fmt.Sprintf("/deployments/%s/volumes/%s", app, name))
		}
	}
	sort.Strings(paths)
	return paths
}

// imageTagPattern is Docker's tag grammar. Version strings are interpolated
// UNQUOTED into remote shell commands via ContainerName (docker rename, docker
// rm -f), and until now a version could only come from git output or an
// operator's own --version. Deriving it from an image reference introduces a
// less-trusted source, so it is gated here rather than trusted.
var imageTagPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)

// versionFromImage derives a deploy version from an image reference, or ""
// when the reference carries no meaningful one.
//
// Why this exists: `teploy deploy --image repo/app:462d7a7` used to label the
// deploy with the local git HEAD instead — so the container name, `teploy log`
// and the operator's screen all asserted a commit whose code was not running.
// Observed in production as "Deployed version fe82bf0" while running the image
// tagged 462d7a7. During a rollback that is worse than cosmetic: the version
// someone picks out of the log never corresponded to that image.
//
// Note this is NOT specific to the --image flag. The caller resolves
// `image = appCfg.Image` first, so a teploy.yml pinning a prebuilt image hit
// the identical problem with no flag passed.
// validateVersionArg constrains an operator-supplied --version before it
// reaches container names and remote shell commands (audit F18). Same
// grammar versionFromImage already enforces for image-derived tags.
func validateVersionArg(v string) error {
	if !imageTagPattern.MatchString(v) {
		return fmt.Errorf("invalid --version %q — letters, digits, underscore, dot and hyphen only; must start with a letter, digit or underscore (max 128 chars)", v)
	}
	return nil
}

func versionFromImage(image string) string {
	// A digest pins exact content, so it is the most truthful label available,
	// and it wins over any tag beside it: Docker resolves the digest and
	// ignores the tag, so labelling `repo:1.2.3@sha256:...` as 1.2.3 would be
	// the same class of lie this function exists to remove.
	if _, digest, ok := strings.Cut(image, "@"); ok {
		const prefix = "sha256:"
		if strings.HasPrefix(digest, prefix) && len(digest) == len(prefix)+64 {
			// Colons are invalid in a container name, so the prefix is dashed.
			return "sha256-" + digest[len(prefix):len(prefix)+12]
		}
		return ""
	}

	// The tag follows the last ':', but only when that colon comes after the
	// last '/' — otherwise a registry port (100.108.123.49:49152/tyler/app)
	// parses as the tag.
	tag := ""
	if i := strings.LastIndex(image, ":"); i > strings.LastIndex(image, "/") {
		tag = image[i+1:]
	}

	// "" and "latest" are refused deliberately. A floating tag reused as the
	// version gives every deploy the same container name AND leaves
	// CurrentHash == PreviousHash, which disables rollback entirely. The
	// caller's timestamp fallback is at least unique per deploy.
	if tag == "" || tag == "latest" || !imageTagPattern.MatchString(tag) {
		return ""
	}
	return tag
}

// runMultiDeploy handles deploying to multiple servers in parallel.
func runMultiDeploy(flags *Flags, appCfg *config.AppConfig, image, version string, skipDNSCheck bool, parallel int, migrateVolumes bool) error {
	// Resolve parallel setting.
	if parallel <= 0 {
		parallel = appCfg.Parallel
	}
	if parallel <= 0 {
		parallel = 1
	}

	// Build server targets from the servers list in teploy.yml.
	serversPath, err := config.DefaultServersPath()
	if err != nil {
		return fmt.Errorf("determining servers path: %w", err)
	}

	// Load servers config to get tags.
	allServers, _ := config.ListServers(serversPath)

	targets := make([]multideploy.ServerTarget, 0, len(appCfg.Servers))
	for _, name := range appCfg.Servers {
		host, user, key, err := config.ResolveServer(name, flags.Host, flags.User, flags.Key)
		if err != nil {
			return fmt.Errorf("resolving server %s: %w", name, err)
		}
		user = config.EffectiveUser(user, flags.User, appCfg.User)
		var tags map[string]string
		if srv, ok := allServers[name]; ok {
			tags = srv.Tags
		}
		targets = append(targets, multideploy.ServerTarget{
			Name: name,
			Host: host,
			User: user,
			Key:  key,
			Role: "app",
			Tags: tags,
		})
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	deployFn := func(ctx context.Context, target multideploy.ServerTarget, out io.Writer) error {
		return deploySingleServer(ctx, appCfg, target, out, migrateVolumes, image, version)
	}

	// Staged rollout: canary wave first, gated, then the rest of the fleet.
	maxFailures := 0
	mainTargets := targets
	var canarySucceeded []multideploy.ServerTarget
	if appCfg.Rollout != nil && len(targets) > 1 {
		maxFailures = appCfg.Rollout.MaxFailures
		canaryN, err := appCfg.Rollout.CanaryCount(len(targets))
		if err != nil {
			return err
		}
		canary, rest := targets[:canaryN], targets[canaryN:]
		fmt.Printf("Rollout: canary wave — deploying %s to %d of %d server(s) serially...\n",
			appCfg.App, len(canary), len(targets))
		canaryResults := multideploy.ParallelDeploy(ctx, canary, 1, deployFn, os.Stdout)
		fmt.Print(multideploy.FormatResults(canaryResults))
		if failed := rollbackFailedWave(ctx, appCfg, canary, canaryResults, parallel, migrateVolumes); failed > 0 {
			// A canary failure halts everything: the rest of the fleet was
			// never touched and the canary is back on the old version.
			return fmt.Errorf("rollout halted: %d of %d canary server(s) failed; rest of fleet untouched", failed, len(canary))
		}
		fmt.Printf("Rollout: canary healthy — deploying remaining %d server(s) (parallel=%d, max_failures=%d)...\n",
			len(rest), parallel, maxFailures)
		canarySucceeded = canary
		mainTargets = rest
	} else {
		fmt.Printf("Deploying %s to %d servers (parallel=%d)...\n", appCfg.App, len(targets), parallel)
	}

	var results []multideploy.Result
	if maxFailures > 0 {
		// Failure-tolerant wave: attempt every server (no fail-fast skip) so
		// the failure count reflects reality, then judge against the budget.
		results = multideploy.ParallelDeployAll(ctx, mainTargets, parallel, deployFn, os.Stdout)
	} else {
		results = multideploy.ParallelDeploy(ctx, mainTargets, parallel, deployFn, os.Stdout)
	}
	targets = mainTargets

	fmt.Print(multideploy.FormatResults(results))

	var successTargets []multideploy.ServerTarget
	var failCount int
	for i, r := range results {
		if r.Success {
			successTargets = append(successTargets, targets[i])
		} else {
			failCount++
		}
	}
	// Canary servers that passed the gate are on the new version too: they
	// belong in the LB on success, and in the convergence rollback if the
	// main wave busts the failure budget.
	successTargets = append(canarySucceeded, successTargets...)

	if failCount == 0 {
		if len(successTargets) > 0 {
			// Front-door activation is a required deployment phase (audit
			// T57): the old shape printed a warning and returned nil, so a
			// green CLI exit did not prove the deployment was reachable —
			// backends could serve new versions on new ports while the LB
			// still targeted the old ones.
			if err := updateLoadBalancer(ctx, flags, appCfg, serversPath, successTargets); err != nil {
				return fmt.Errorf("backends deployed but load-balancer activation failed — the fleet needs reconciliation: %w", err)
			}
		}
		return nil
	}

	// Within the rollout failure budget: succeeded servers KEEP the new
	// version (no fleet-wide yo-yo on a large rollout) and only they enter
	// the load balancer. The exit is still non-zero with an explicit
	// straggler list — a mixed-version fleet must be converged deliberately,
	// never left silent (the M1 version-divergence guard).
	if maxFailures > 0 && failCount <= maxFailures {
		if len(successTargets) > 0 {
			if err := updateLoadBalancer(ctx, flags, appCfg, serversPath, successTargets); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: LB update failed: %v\n", err)
			}
		}
		var stragglers []string
		for i, r := range results {
			if !r.Success {
				stragglers = append(stragglers, targets[i].Name)
				fmt.Fprintf(os.Stderr, "  straggler %s (still on the previous version): %v\n", targets[i].Name, r.Error)
			}
		}
		fmt.Fprintf(os.Stderr, "\nConverge the stragglers with:  teploy deploy --version %s\n", version)
		return fmt.Errorf("rollout completed within failure budget (%d/%d failed <= max_failures=%d) — stragglers on the old version: %s",
			failCount, len(targets), maxFailures, strings.Join(stragglers, ", "))
	}

	// Partial failure: roll back every server that succeeded so the fleet
	// ends up consistent on the old version everywhere, instead of
	// split-brain (some servers on the new version, others on old, with no
	// reconciliation). Deliberately skip the LB update above in this branch
	// — none of successTargets should end up in rotation serving a version
	// we're about to revert.
	if len(successTargets) > 0 {
		fmt.Printf("\n%d of %d servers failed — rolling back the %d server(s) that succeeded...\n",
			failCount, len(targets), len(successTargets))

	// Best-effort: attempt to roll back EVERY succeeded server even if one
	// rollback fails — otherwise a single rollback failure would fail-fast
	// and strand the remaining servers on the new version (M1). The wave
	// runs on a bounded DETACHED recovery context (audit T58): the deploy
	// context is signal-cancelled exactly when the operator interrupts,
	// and recovery work that skips itself because the cancelled context
	// disappeared is how a Ctrl-C strands half a fleet on the new version.
	rollbackCtx, rollbackCancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Minute)
	defer rollbackCancel()
	rollbackResults := multideploy.ParallelDeployAll(rollbackCtx, successTargets, parallel, func(ctx context.Context, target multideploy.ServerTarget, out io.Writer) error {
		return rollbackSingleServer(ctx, appCfg, target, out)
	}, os.Stdout)

		var rolledBack, firstDeploys, rollbackFailed []string
		for _, r := range rollbackResults {
			switch {
			case r.Success:
				rolledBack = append(rolledBack, r.Server)
			case errors.Is(r.Error, deploy.ErrNoPreviousDeploy):
				// This was the server's first-ever deploy for this app —
				// there's nothing to revert to. Not a rollback failure;
				// the (partial/broken) new version is simply the only
				// version that ever existed there.
				firstDeploys = append(firstDeploys, r.Server)
			default:
				rollbackFailed = append(rollbackFailed, r.Server)
				fmt.Fprintf(os.Stderr, "  WARNING: rollback also failed on %s: %v — needs manual attention\n", r.Server, r.Error)
			}
		}
		if len(rolledBack) > 0 {
			fmt.Printf("  Rolled back: %s\n", strings.Join(rolledBack, ", "))
		}
		if len(firstDeploys) > 0 {
			fmt.Printf("  No previous version to roll back to (first deploy): %s\n", strings.Join(firstDeploys, ", "))
		}
		if len(rollbackFailed) > 0 {
			fmt.Fprintf(os.Stderr, "  Rollback failed, still on the new version and needs manual attention: %s\n", strings.Join(rollbackFailed, ", "))
		}
	}

	return fmt.Errorf("%d of %d servers failed", failCount, len(targets))
}

// rollbackFailedWave rolls back the succeeded servers of a deploy wave when
// the wave had failures, so the wave converges back to the old version.
// Returns the wave's failure count (0 = nothing to do). Used by the canary
// gate: a failed canary must leave the canary servers on the old version.
func rollbackFailedWave(ctx context.Context, appCfg *config.AppConfig, wave []multideploy.ServerTarget, results []multideploy.Result, parallel int, migrateVolumes bool) int {
	_ = migrateVolumes // rollback re-routes to the previous version; no volume migration
	var succeeded []multideploy.ServerTarget
	failed := 0
	for i, r := range results {
		if r.Success {
			succeeded = append(succeeded, wave[i])
		} else {
			failed++
		}
	}
	if failed == 0 {
		return 0
	}
	if len(succeeded) > 0 {
		fmt.Printf("Rolling back %d canary server(s) that succeeded...\n", len(succeeded))
		// Detached bounded recovery context (audit T58, same rationale as
		// the partial-failure rollback): an interrupted canary wave must
		// still converge its succeeded servers.
		recoveryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Minute)
		defer cancel()
		rollbackResults := multideploy.ParallelDeployAll(recoveryCtx, succeeded, parallel, func(ctx context.Context, target multideploy.ServerTarget, out io.Writer) error {
			return rollbackSingleServer(ctx, appCfg, target, out)
		}, os.Stdout)
		for _, r := range rollbackResults {
			if !r.Success && !errors.Is(r.Error, deploy.ErrNoPreviousDeploy) {
				fmt.Fprintf(os.Stderr, "  WARNING: canary rollback failed on %s: %v — needs manual attention\n", r.Server, r.Error)
			}
		}
	}
	return failed
}

// buildNotifier creates a MultiNotifier from the app config.
// Supports both the legacy single-webhook format and the new multi-channel format.
// executorAcceptsNew reports whether an executor was created with the
// --accept-new host-key policy (transfer channels mirror it; see
// ssh.ExternalSSHArgs).
func executorAcceptsNew(exec ssh.Executor) bool {
	a, ok := exec.(interface{ AcceptNewHost() bool })
	return ok && a.AcceptNewHost()
}

// caddyFirewall converts the teploy.yml firewall config into the caddy layer's
// firewall value. Validation happens at config load (AppConfig.validate).
func caddyFirewall(f config.FirewallConfig) caddy.Firewall {
	return caddy.Firewall{
		AllowIPs:        f.AllowIPs,
		DenyIPs:         f.DenyIPs,
		BlockUserAgents: f.BlockUserAgents,
		MaxBodySize:     f.MaxBodySize,
	}
}

// caddyAccess converts the teploy.yml access gate into the caddy layer's value.
func caddyAccess(a config.AccessConfig) caddy.Access {
	out := caddy.Access{BasicAuthUsers: a.BasicAuth}
	if a.ForwardAuth != nil {
		out.ForwardAuthURL = a.ForwardAuth.URL
		out.ForwardAuthURI = a.ForwardAuth.URI
		out.ForwardAuthCopyHeaders = a.ForwardAuth.CopyHeaders
	}
	return out
}

// emitDeployAudit records a deploy/rollback/scale event to observe if the app
// configures `audit:`. Fire-and-forget — a failed emit only warns.
func emitDeployAudit(ctx context.Context, appCfg *config.AppConfig, action, version, server string, actionErr error) {
	if appCfg.Audit.Endpoint == "" {
		return
	}
	result := "success"
	if actionErr != nil {
		result = "failure"
	}
	if err := audit.Emit(ctx, appCfg.Audit.Endpoint, appCfg.Audit.Token, appCfg.Audit.Site, audit.Event{
		Action:   action,
		Target:   appCfg.App + "@" + version,
		Result:   result,
		Metadata: map[string]any{"server": server, "version": version},
	}); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: audit emit failed: %v\n", err)
	}
}

func buildNotifier(appCfg *config.AppConfig) *notify.MultiNotifier {
	var channels []notify.Channel

	// Legacy single-webhook format.
	if appCfg.Notifications.Webhook != "" {
		channels = append(channels, notify.Channel{
			Type:   "webhook",
			URL:    appCfg.Notifications.Webhook,
			Secret: appCfg.Notifications.SigningSecret(),
		})
	}

	// Multi-channel format.
	for _, ch := range appCfg.Notifications.Channels {
		channels = append(channels, notify.Channel{
			Type:   ch.Type,
			URL:    ch.URL,
			To:     ch.To,
			Events: ch.Events,
			Secret: appCfg.Notifications.ChannelSigningSecret(ch),
		})
	}

	return notify.NewMultiNotifier(channels)
}

func gitShortHash() (string, error) {
	out, err := exec.Command("git", "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// gitShortHashIn is gitShortHash for a specific directory instead of the
// process's cwd — for `teploy autodeploy serve` (autodeploy_serve.go),
// which resolves the version from the server-side build checkout it just
// fetched, not from wherever systemd happened to start the process.
func gitShortHashIn(dir string) (string, error) {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func gitRevisionIn(dir string) (string, error) {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// runStaticDeploy handles the type:static deploy path: build (locally) →
// rsync → symlink → Caddyfile mirror. Container build/run/health-check
// machinery is intentionally bypassed.
func runStaticDeploy(cfg *config.AppConfig, host, user, key string) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	fmt.Printf("Connecting to %s@%s...\n", user, host)
	executor, err := ssh.Connect(ctx, ssh.ConnectConfig{
		Host:    host,
		User:    user,
		KeyPath: key,
	})
	if err != nil {
		return err
	}
	defer executor.Close()

	appliedManifest, manifestSHA256, err := config.NormalizeAndDigest(cfg, "")
	if err != nil {
		return fmt.Errorf("normalizing applied manifest: %w", err)
	}
	staticCfg := deploy.StaticConfig{
		App:             cfg.App,
		Domain:          cfg.Domain,
		Source:          cfg.Source,
		Build:           cfg.Build,
		BuildRemote:     cfg.BuildRemote,
		SPA:             cfg.SPA,
		SPAFallback:     cfg.SPAFallback,
		Cache:           cfg.Cache,
		Headers:         cfg.Headers,
		KeepReleases:    cfg.KeepReleases,
		CaddyExtra:      cfg.CaddyExtra,
		ManifestSHA256:  manifestSHA256,
		AppliedManifest: appliedManifest,
		SourceRevision:  cfg.SourceRevision,
	}

	d := deploy.NewStaticDeployer(executor, os.Stdout)
	d.SSHKeyPath = key
	if err := d.Deploy(ctx, staticCfg); err != nil {
		state.AppendLog(ctx, executor, state.LogEntry{
			Timestamp: time.Now().UTC(),
			App:       cfg.App,
			Type:      "deploy",
			Success:   false,
			Message:   err.Error(),
		})
		return err
	}
	return nil
}

// appTLSContainerPaths returns the LEGACY container-side cert/key paths for
// an app's custom TLS certificate (pre-F08 layout). Kept because records
// written before attempt-scoped TLS name these paths — a rollback target's
// recorded TLSCert still points here, and those files were never moved.
func appTLSContainerPaths(app string) (cert, key string) {
	return "/etc/caddy/tls/" + app + ".crt", "/etc/caddy/tls/" + app + ".key"
}

// uploadAppTLS reads the local cert + key referenced by the app's tls config
// and uploads them to the server's attempt-scoped TLS directory (F08:
// /deployments/caddy/tls/att/<app>/<hash>.<id>/, key mode 0600), where the
// directory-mounted Caddy container reads them at
// /etc/caddy/tls/att/<app>/….
// Attempt-scoping keeps the cert/key immutable for the release that
// references it: the F14 record names these exact bytes, and a concurrent
// or later attempt cannot overwrite them. It returns the container-side
// paths to reference in the Caddy site block.
//
// A nil attempt selects the LEGACY shared path — the pre-F08 layout. That
// is the rollback CLI's fallback: it re-uploads the operator's current
// cert before the rollback target is known, and for any release recorded
// by F14 the record overrides these paths with the target's own attempt
// paths anyway; only backfilled (pre-F14) releases fall back to them.
func uploadAppTLS(ctx context.Context, exec ssh.Executor, app string, tls *config.TLSConfig, att *releasemeta.Attempt) (cert, key string, err error) {
	certBytes, err := os.ReadFile(tls.Cert)
	if err != nil {
		return "", "", fmt.Errorf("reading tls cert %s: %w", tls.Cert, err)
	}
	keyBytes, err := os.ReadFile(tls.Key)
	if err != nil {
		return "", "", fmt.Errorf("reading tls key %s: %w", tls.Key, err)
	}
	var hostCert, hostKey string
	if att != nil {
		if _, err := exec.Run(ctx, "mkdir -p "+ssh.ShellQuote(att.TLSDir())); err != nil {
			return "", "", fmt.Errorf("creating tls dir: %w", err)
		}
		hostCert, hostKey = att.TLSDir()+"/"+app+".crt", att.TLSDir()+"/"+app+".key"
		cert, key = att.TLSCertPath(), att.TLSKeyPath()
	} else {
		if _, err := exec.Run(ctx, "mkdir -p /deployments/caddy/tls"); err != nil {
			return "", "", fmt.Errorf("creating tls dir: %w", err)
		}
		hostCert, hostKey = "/deployments/caddy/tls/"+app+".crt", "/deployments/caddy/tls/"+app+".key"
		cert, key = "/etc/caddy/tls/"+app+".crt", "/etc/caddy/tls/"+app+".key"
	}
	if err := ssh.UploadAtomic(ctx, exec, bytes.NewReader(certBytes), hostCert, "0644"); err != nil {
		return "", "", fmt.Errorf("uploading tls cert: %w", err)
	}
	if err := ssh.UploadAtomic(ctx, exec, bytes.NewReader(keyBytes), hostKey, "0600"); err != nil {
		return "", "", fmt.Errorf("uploading tls key: %w", err)
	}
	return cert, key, nil
}

// resolveAppTLS uploads a custom cert/key (if configured) and reports
// whether tls.internal was requested, so every deploy/rollback call site
// can populate deploy.Config's (or RollbackConfig's) TLSCert/TLSKey/
// TLSInternal fields with one call instead of repeating the appCfg.TLS !=
// nil / .Internal branch five times. att nil = legacy shared paths (see
// uploadAppTLS).
func resolveAppTLS(ctx context.Context, exec ssh.Executor, appCfg *config.AppConfig, att *releasemeta.Attempt) (cert, key string, internal bool, err error) {
	if appCfg.TLS == nil {
		return "", "", false, nil
	}
	if appCfg.TLS.Internal {
		return "", "", true, nil
	}
	cert, key, err = uploadAppTLS(ctx, exec, appCfg.App, appCfg.TLS, att)
	return cert, key, false, err
}

// disabledHealthchecks returns the set of process names whose container
// HEALTHCHECK should be disabled (--no-healthcheck), built from the
// teploy.yml `healthcheck:` block. Returns nil when nothing is disabled
// so deploy.Config carries a nil map and skips the lookup hot path.
func disabledHealthchecks(hc map[string]config.ProcessHealth) map[string]bool {
	if len(hc) == 0 {
		return nil
	}
	out := make(map[string]bool, len(hc))
	for name, h := range hc {
		if h.Disable {
			out[name] = true
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// healthConfigFrom builds a deploy.HealthConfig from teploy.yml's health:
// block. Zero TimeoutSeconds/IntervalSeconds map to zero time.Duration,
// which HealthConfig.withDefaults() (internal/deploy/health.go) fills in
// as 30s/1s — so unset fields are zero behavior change from before these
// were configurable. Mode passes through: "" means auto (compat).
func healthConfigFrom(h config.AppHealthConfig) deploy.HealthConfig {
	return deploy.HealthConfig{
		Mode:     h.Mode,
		Path:     h.Path,
		Timeout:  time.Duration(h.TimeoutSeconds) * time.Second,
		Interval: time.Duration(h.IntervalSeconds) * time.Second,
	}
}

// isDigestPinned reports whether an image reference is content-addressed
// (`repo@sha256:...`). Such a reference names exactly one set of bytes forever,
// so a local copy of it can never be out of date. Everything else — every tag,
// and a bare repo (which Docker resolves to `:latest`) — is mutable: the
// registry can move it under us at any time, and "looks like a git sha" is a
// convention nothing enforces, so tags are not special-cased here.
func isDigestPinned(image string) bool {
	i := strings.LastIndex(image, "@")
	if i < 0 {
		return false
	}
	digest := image[i+1:]
	sep := strings.Index(digest, ":")
	return sep > 0 && sep < len(digest)-1
}

// ensureImage makes a pre-built image available on the server before it's used
// to run containers.
//
// A digest-pinned reference already on the server is used as-is. Anything else
// is pulled every deploy, even when a copy is already cached locally.
//
// DO NOT "optimise" that pull back out. Skipping it whenever the image existed
// locally is exactly what shipped five-day-old code to production: an app whose
// teploy.yml named an untagged registry image (so `:latest`) had a `:latest`
// already on the host, CI kept pushing newer ones, and teploy never pulled —
// every deploy started a container named after the new commit, passed its health
// check and reported success while serving the 28 Aug build. Nothing could see
// it: the container name is a label we write, so `docker ps` agrees with us, and
// `teploy drift` compares live state to deploy state by name, so it agrees too.
// A `docker pull` on an already-current tag is a manifest check, not a
// re-download — the cost is one round trip, not a layer transfer.
//
// The local cache still covers the out-of-band case the skip was added for
// (images built or `docker load`ed on the server that exist in no registry):
// when the pull fails and a copy is present we fall back to it, but say so, so
// "pulled fresh" is never confused with "registry unreachable, using what's
// already here".
func ensureImage(ctx context.Context, dk *docker.Client, image string, out io.Writer) error {
	exists, err := dk.ImageExists(ctx, image)
	if err != nil {
		return err
	}
	if exists && isDigestPinned(image) {
		fmt.Fprintf(out, "  Using local image %s (digest-pinned, cannot be stale)\n", image)
		return nil
	}
	fmt.Fprintf(out, "Pulling image %s...\n", image)
	if err := dk.Pull(ctx, image); err != nil {
		if exists {
			fmt.Fprintf(out, "  WARNING: pull failed (%v)\n", err)
			fmt.Fprintf(out, "  Falling back to the local copy of %s — it may be older than the registry\n", image)
			return nil
		}
		return fmt.Errorf("image not found or registry auth failed: %w", err)
	}
	fmt.Fprintln(out, "  Image pulled")
	return nil
}
