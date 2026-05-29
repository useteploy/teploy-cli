package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/useteploy/teploy/internal/accessories"
	"github.com/useteploy/teploy/internal/build"
	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/deploy"
	"github.com/useteploy/teploy/internal/dns"
	"github.com/useteploy/teploy/internal/docker"
	"github.com/useteploy/teploy/internal/multideploy"
	"github.com/useteploy/teploy/internal/notify"
	"github.com/useteploy/teploy/internal/secret"
	"github.com/useteploy/teploy/internal/ssh"
	"github.com/useteploy/teploy/internal/state"
)

func newDeployCmd(flags *Flags) *cobra.Command {
	var (
		image           string
		version         string
		skipDNSCheck    bool
		parallel        int
		destination     string
		appName         string
		domain          string
		port            int
		migrateVolumes  bool
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
			return runDeploy(flags, serverName, image, version, skipDNSCheck, parallel, destination, migrateVolumes)
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

	return cmd
}

// runAdHocDeploy handles deploys without a teploy.yml — used by teploy-dash
// and scripting. Requires --app and --image at minimum.
func runAdHocDeploy(flags *Flags, serverName, appName, image, domain string, port int, version string, skipDNSCheck, migrateVolumes bool) error {
	if image == "" {
		return fmt.Errorf("--image is required for ad-hoc deploy (no teploy.yml)")
	}
	if domain == "" {
		return fmt.Errorf("--domain is required for ad-hoc deploy")
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

func runDeploy(flags *Flags, serverName, image, version string, skipDNSCheck bool, parallel int, destination string, migrateVolumes bool) error {
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

	// Multi-server deploy: if teploy.yml lists multiple servers and no explicit
	// server argument was provided, deploy to all of them in parallel.
	if len(appCfg.Servers) > 1 && serverName == "" {
		// TODO(autodeploy/multideploy): plumb migrateVolumes through the
		// multi-server path. The volume-mismatch check currently runs only on
		// single-server deploys (deployAppConfig). Same fix applies in
		// internal/cli/singledeploy.go's deployApp.
		return runMultiDeploy(flags, appCfg, image, version, skipDNSCheck, parallel)
	}

	return deployAppConfig(flags, appCfg, serverName, image, version, skipDNSCheck, migrateVolumes)
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

	// 5. Detect build mode (when no pre-built image).
	needsBuild := image == ""
	var buildMode build.Mode
	if needsBuild {
		buildMode = build.Detect(".")
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

	// 6b. Pull pre-built image (CI pipeline mode).
	if !needsBuild && image != "" {
		fmt.Printf("Pulling image %s...\n", image)
		dk := docker.NewClient(executor)
		if err := dk.Pull(ctx, image); err != nil {
			return fmt.Errorf("image not found or registry auth failed: %w", err)
		}
		fmt.Println("  Image pulled")
	}

	// 7. Build (if no pre-built image).
	if needsBuild {
		if appCfg.BuildLocal {
			// Local build mode: build on this machine, stream to server.
			fmt.Println("Building image locally...")
			image, err = build.LocalBuild(ctx, build.LocalBuildConfig{
				App:      appCfg.App,
				Version:  version,
				Mode:     buildMode,
				Dir:      ".",
				Host:     host,
				User:     user,
				KeyPath:  key,
				Platform: appCfg.Platform,
				Exec:     executor,
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
				App:      appCfg.App,
				Version:  version,
				Mode:     buildMode,
				BuildDir: remoteDir,
				Platform: appCfg.Platform,
			})
			if err != nil {
				return fmt.Errorf("building image: %w", err)
			}
		}
		fmt.Printf("Built image: %s\n", image)
	}

	// 8. DNS validation (first deploy only).
	if !skipDNSCheck {
		current, _ := state.Read(ctx, executor, appCfg.App)
		if current == nil {
			fmt.Println("Validating DNS...")
			if err := dns.Validate(appCfg.Domain, host, nil); err != nil {
				return err
			}
			fmt.Println("  DNS validated")
		}
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

	// 11. Deploy.
	deployer := deploy.NewDeployer(executor, os.Stdout)
	deployCfg := deploy.Config{
		App:           appCfg.App,
		Domain:        appCfg.Domain,
		Image:         image,
		Version:       version,
		EnvFile:       envFile,
		Env:           deploySecrets,
		Volumes:       volumes,
		Processes:     appCfg.Processes,
		Ingress:       appCfg.Ingress,
		ContainerPort: appCfg.Port,
		StopTimeout:   appCfg.StopTimeout,
		Replicas:      appCfg.Replicas,
		PreDeploy:     appCfg.Hooks.PreDeploy,
		PostDeploy:    appCfg.Hooks.PostDeploy,
		AssetPath:     appCfg.Assets.Path,
		AssetKeepDays: appCfg.Assets.KeepDays,
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
			Server:  host,
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
func runMultiDeploy(flags *Flags, appCfg *config.AppConfig, image, version string, skipDNSCheck bool, parallel int) error {
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

	fmt.Printf("Deploying %s to %d servers (parallel=%d)...\n", appCfg.App, len(targets), parallel)

	results := multideploy.ParallelDeploy(ctx, targets, parallel, func(ctx context.Context, target multideploy.ServerTarget, out io.Writer) error {
		return deploySingleServer(ctx, appCfg, target, out)
	}, os.Stdout)

	fmt.Print(multideploy.FormatResults(results))

	// Update LB if any servers succeeded.
	var successTargets []multideploy.ServerTarget
	var failCount int
	for i, r := range results {
		if r.Success {
			successTargets = append(successTargets, targets[i])
		} else {
			failCount++
		}
	}

	if len(successTargets) > 0 {
		if err := updateLoadBalancer(ctx, flags, appCfg, serversPath, successTargets); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: LB update failed: %v\n", err)
		}
	}

	if failCount > 0 {
		return fmt.Errorf("%d of %d servers failed", failCount, len(targets))
	}

	return nil
}

// buildNotifier creates a MultiNotifier from the app config.
// Supports both the legacy single-webhook format and the new multi-channel format.
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
