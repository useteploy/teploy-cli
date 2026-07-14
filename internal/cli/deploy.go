package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
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
				return err
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
		return fmt.Errorf("--image is required for ad-hoc deploy (no teploy.yml)")
	}
	// This path builds an AppConfig directly instead of going through
	// config.LoadApp, so it never reaches AppConfig.validate() — app and
	// domain must be validated explicitly here before either one reaches
	// the network (state paths, remote shell commands, Caddyfile content).
	if err := config.ValidateName(appName); err != nil {
		return err
	}
	if err := config.ValidateDomain(domain, false); err != nil {
		return err
	}
	if serverName == "" {
		serverName = flags.Host
	}
	if serverName == "" {
		return fmt.Errorf("server is required — use 'teploy deploy <server> --app ...' or --host")
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
	// 1. Load teploy.yml from current directory (with optional destination overlay).
	var appCfg *config.AppConfig
	var err error
	if destination != "" {
		appCfg, err = config.LoadAppWithDestination(".", destination)
	} else {
		appCfg, err = config.LoadApp(".")
	}
	if err != nil {
		return err
	}

	// Narrow the target list by --role/--tag (no-op if neither is set). Only
	// affects the multi-server list; an explicit `deploy <server>` arg wins.
	if err := filterServersByRoleTag(appCfg, role, tags); err != nil {
		return err
	}

	// Resolve env_files (SOPS/age-encrypted or plain, decrypted locally)
	// once, before dispatch — both the single- and multi-server paths then
	// pick them up from appCfg.Env. Explicit env: keys win over file values.
	if len(appCfg.EnvFiles) > 0 {
		fileVars, err := env.LoadLocalEnvFiles(".", appCfg.EnvFiles)
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
		return fmt.Errorf("no server specified — use 'teploy deploy <server>' or set 'server' in teploy.yml")
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
	if version == "" {
		version, err = gitShortHash()
		if err != nil {
			if image != "" {
				// Pre-built image with no git repo — use a timestamp.
				version = fmt.Sprintf("%d", time.Now().Unix())
			} else {
				return fmt.Errorf("could not determine version from git: %w (use --version flag)", err)
			}
		}
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
			// Server build mode: rsync + build on server.
			remoteDir := fmt.Sprintf("/deployments/%s/build", appCfg.App)
			if _, err := executor.Run(ctx, "mkdir -p "+remoteDir); err != nil {
				return fmt.Errorf("creating build directory: %w", err)
			}

			fmt.Println("Syncing source to server...")
			excludes := build.LoadIgnore(".")
			if err := build.Sync(ctx, build.SyncConfig{
				LocalDir:  ".",
				RemoteDir: remoteDir,
				Host:      host,
				User:      user,
				KeyPath:   key,
				Excludes:  excludes,
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

	return deployBuiltImage(ctx, executor, appCfg, image, version, host, migrateVolumes, needsBuild)
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
	if err := mergeVaultRefs(ctx, executor, appCfg, deploySecrets); err != nil {
		return err
	}

	// 10. Resolve persistent volumes.
	var volumes map[string]string
	if len(appCfg.Volumes) > 0 {
		volumes = make(map[string]string, len(appCfg.Volumes))
		for name, containerPath := range appCfg.Volumes {
			hostPath := fmt.Sprintf("/deployments/%s/volumes/%s", appCfg.App, name)
			volumes[hostPath] = containerPath
			if _, err := executor.Run(ctx, fmt.Sprintf("mkdir -p %s", hostPath)); err != nil {
				return fmt.Errorf("creating volume directory %s: %w", hostPath, err)
			}
		}
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
	tlsCert, tlsKey, tlsInternal, err := resolveAppTLS(ctx, executor, appCfg)
	if err != nil {
		return err
	}
	if tlsCert != "" {
		fmt.Println("  TLS certificate uploaded")
	}

	// Container env: teploy.yml's `env:` block plus decrypted secrets,
	// uploaded to a fresh env file rather than passed as `docker run -e`
	// args — see buildContainerEnvFiles for why.
	envFiles, err := buildContainerEnvFiles(ctx, executor, appCfg.App, envFile, appCfg.Env, nil, deploySecrets)
	if err != nil {
		return err
	}

	// 11. Deploy.
	deployer := deploy.NewDeployer(executor, os.Stdout)
	deployCfg := deploy.Config{
		App:           appCfg.App,
		Domain:        appCfg.Domain,
		Image:         image,
		Version:       version,
		EnvFiles:      envFiles,
		Volumes:       volumes,
		Processes:     appCfg.Processes,
		NoHealthcheck: disabledHealthchecks(appCfg.Healthcheck),
		Health:        healthConfigFrom(appCfg.Health),
		KeepVersions:  appCfg.KeepVersions,
		Ingress:       appCfg.Ingress,
		Bind:          appCfg.Bind,
		ContainerPort: appCfg.Port,
		StopTimeout:   appCfg.StopTimeout,
		Replicas:      appCfg.Replicas,
		PreDeploy:     appCfg.Hooks.PreDeploy,
		PostDeploy:    appCfg.Hooks.PostDeploy,
		AssetPath:     appCfg.Assets.Path,
		AssetKeepDays: appCfg.Assets.KeepDays,
		TLSCert:       tlsCert,
		TLSKey:        tlsKey,
		TLSInternal:   tlsInternal,
		CaddyExtra:    appCfg.CaddyExtra,
		Firewall:      caddyFirewall(appCfg.Firewall),
		Access:        caddyAccess(appCfg.Access),
	}

	// Vulnerability gate: scan the image on the server before any container
	// starts — fixable CRITICALs block the deploy.
	if appCfg.Scan {
		fmt.Println("Scanning image for vulnerabilities (trivy)...")
		if err := docker.NewClient(executor).ScanImage(ctx, image, os.Stdout); err != nil {
			return err
		}
	}

	multiNotifier := buildNotifier(appCfg)
	deployErr := deployer.Deploy(ctx, deployCfg)

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
		return deploySingleServer(ctx, appCfg, target, out, migrateVolumes)
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
			if err := updateLoadBalancer(ctx, flags, appCfg, serversPath, successTargets); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: LB update failed: %v\n", err)
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
		// and strand the remaining servers on the new version (M1).
		rollbackResults := multideploy.ParallelDeployAll(ctx, successTargets, parallel, func(ctx context.Context, target multideploy.ServerTarget, out io.Writer) error {
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
		rollbackResults := multideploy.ParallelDeployAll(ctx, succeeded, parallel, func(ctx context.Context, target multideploy.ServerTarget, out io.Writer) error {
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
			Type: "webhook",
			URL:  appCfg.Notifications.Webhook,
		})
	}

	// Multi-channel format.
	for _, ch := range appCfg.Notifications.Channels {
		channels = append(channels, notify.Channel{
			Type:   ch.Type,
			URL:    ch.URL,
			To:     ch.To,
			Events: ch.Events,
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

	staticCfg := deploy.StaticConfig{
		App:          cfg.App,
		Domain:       cfg.Domain,
		Source:       cfg.Source,
		Build:        cfg.Build,
		BuildRemote:  cfg.BuildRemote,
		SPA:          cfg.SPA,
		SPAFallback:  cfg.SPAFallback,
		Cache:        cfg.Cache,
		Headers:      cfg.Headers,
		KeepReleases: cfg.KeepReleases,
		CaddyExtra:   cfg.CaddyExtra,
	}

	d := deploy.NewStaticDeployer(executor, os.Stdout)
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

// appTLSContainerPaths returns the container-side cert/key paths for an app's
// custom TLS certificate. These live under /etc/caddy/tls, which the Caddy
// container sees via the /deployments/caddy directory mount.
func appTLSContainerPaths(app string) (cert, key string) {
	return "/etc/caddy/tls/" + app + ".crt", "/etc/caddy/tls/" + app + ".key"
}

// uploadAppTLS reads the local cert + key referenced by the app's tls config
// and uploads them to the server's /deployments/caddy/tls directory (key mode
// 0600), where the directory-mounted Caddy container reads them. It returns
// the container-side paths to reference in the Caddy site block.
func uploadAppTLS(ctx context.Context, exec ssh.Executor, app string, tls *config.TLSConfig) (cert, key string, err error) {
	certBytes, err := os.ReadFile(tls.Cert)
	if err != nil {
		return "", "", fmt.Errorf("reading tls cert %s: %w", tls.Cert, err)
	}
	keyBytes, err := os.ReadFile(tls.Key)
	if err != nil {
		return "", "", fmt.Errorf("reading tls key %s: %w", tls.Key, err)
	}
	if _, err := exec.Run(ctx, "mkdir -p /deployments/caddy/tls"); err != nil {
		return "", "", fmt.Errorf("creating tls dir: %w", err)
	}
	hostCert := "/deployments/caddy/tls/" + app + ".crt"
	hostKey := "/deployments/caddy/tls/" + app + ".key"
	if err := exec.Upload(ctx, bytes.NewReader(certBytes), hostCert, "0644"); err != nil {
		return "", "", fmt.Errorf("uploading tls cert: %w", err)
	}
	if err := exec.Upload(ctx, bytes.NewReader(keyBytes), hostKey, "0600"); err != nil {
		return "", "", fmt.Errorf("uploading tls key: %w", err)
	}
	cert, key = appTLSContainerPaths(app)
	return cert, key, nil
}

// resolveAppTLS uploads a custom cert/key (if configured) and reports
// whether tls.internal was requested, so every deploy/rollback call site
// can populate deploy.Config's (or RollbackConfig's) TLSCert/TLSKey/
// TLSInternal fields with one call instead of repeating the appCfg.TLS !=
// nil / .Internal branch five times.
func resolveAppTLS(ctx context.Context, exec ssh.Executor, appCfg *config.AppConfig) (cert, key string, internal bool, err error) {
	if appCfg.TLS == nil {
		return "", "", false, nil
	}
	if appCfg.TLS.Internal {
		return "", "", true, nil
	}
	cert, key, err = uploadAppTLS(ctx, exec, appCfg.App, appCfg.TLS)
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
// were configurable.
func healthConfigFrom(h config.AppHealthConfig) deploy.HealthConfig {
	return deploy.HealthConfig{
		Path:     h.Path,
		Timeout:  time.Duration(h.TimeoutSeconds) * time.Second,
		Interval: time.Duration(h.IntervalSeconds) * time.Second,
	}
}

// ensureImage makes a pre-built image available on the server before it's used
// to run containers. If the image is already in the server's local cache
// (built or `docker load`ed out of band — including images that exist in no
// registry) the pull is skipped; otherwise it's pulled from its registry,
// preserving the original pull-on-miss behavior for real registry images.
func ensureImage(ctx context.Context, dk *docker.Client, image string, out io.Writer) error {
	exists, err := dk.ImageExists(ctx, image)
	if err != nil {
		return err
	}
	if exists {
		fmt.Fprintf(out, "  Using local image %s\n", image)
		return nil
	}
	fmt.Fprintf(out, "Pulling image %s...\n", image)
	if err := dk.Pull(ctx, image); err != nil {
		return fmt.Errorf("image not found or registry auth failed: %w", err)
	}
	fmt.Fprintln(out, "  Image pulled")
	return nil
}
