package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/useteploy/teploy/internal/accessories"
	"github.com/useteploy/teploy/internal/build"
	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/deploy"
	"github.com/useteploy/teploy/internal/docker"
	"github.com/useteploy/teploy/internal/secret"
	"github.com/useteploy/teploy/internal/ssh"
)

// singleServerDeployer wraps the deploy logic for a single server.
// Used by both the deploy command and multi-server scale/parallel deploy.
type singleServerDeployer struct {
	exec           ssh.Executor
	out            io.Writer
	keyPath        string
	migrateVolumes bool // auto-migrate foreign volume sources instead of aborting
}

func newSingleServerDeployer(exec ssh.Executor, out io.Writer, keyPath string, migrateVolumes bool) *singleServerDeployer {
	return &singleServerDeployer{exec: exec, out: out, keyPath: keyPath, migrateVolumes: migrateVolumes}
}

// deployApp performs the full deploy flow on a single server using an existing SSH connection.
// tags are per-host env vars from servers.yml (may be nil).
func (s *singleServerDeployer) deployApp(ctx context.Context, appCfg *config.AppConfig, tags map[string]string) error {
	image := appCfg.Image

	// Resolve version.
	version, err := gitShortHash()
	if err != nil {
		return fmt.Errorf("could not determine version from git: %w (use --version flag)", err)
	}

	// Build if needed.
	needsBuild := image == ""
	if needsBuild {
		buildMode := build.Detect(".")
		fmt.Fprintf(s.out, "Detected %s build\n", buildMode)

		if appCfg.BuildLocal {
			fmt.Fprintln(s.out, "Building image locally...")
			image, err = build.LocalBuild(ctx, build.LocalBuildConfig{
				App:      appCfg.App,
				Version:  version,
				Mode:     buildMode,
				Dir:      ".",
				Host:     s.exec.Host(),
				User:     s.exec.User(),
				KeyPath:  s.keyPath,
				Platform: appCfg.Platform,
				Exec:     s.exec,
			}, s.out)
			if err != nil {
				return fmt.Errorf("local build: %w", err)
			}
		} else {
			remoteDir := fmt.Sprintf("/deployments/%s/build", appCfg.App)
			if _, err := s.exec.Run(ctx, "mkdir -p "+remoteDir); err != nil {
				return fmt.Errorf("creating build directory: %w", err)
			}

			fmt.Fprintln(s.out, "Syncing source to server...")
			excludes := build.LoadIgnore(".")
			if err := build.Sync(ctx, build.SyncConfig{
				LocalDir:  ".",
				RemoteDir: remoteDir,
				Host:      s.exec.Host(),
				User:      s.exec.User(),
				KeyPath:   s.keyPath,
				Excludes:  excludes,
			}, s.out, s.out); err != nil {
				return fmt.Errorf("syncing source: %w", err)
			}

			fmt.Fprintln(s.out, "Building image on server...")
			builder := build.NewBuilder(s.exec, s.out)
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
		fmt.Fprintf(s.out, "Built image: %s\n", image)
	} else {
		// Pull pre-built image.
		fmt.Fprintf(s.out, "Pulling image %s...\n", image)
		dk := docker.NewClient(s.exec)
		if err := dk.Pull(ctx, image); err != nil {
			return fmt.Errorf("image not found or registry auth failed: %w", err)
		}
		fmt.Fprintln(s.out, "  Image pulled")
	}

	// Ensure accessories are running. This was previously missing entirely
	// from the multi-server path (see the secrets fix above for the same
	// pattern) — an app with `accessories:` in teploy.yml deployed to
	// multiple servers never got its postgres/redis/etc. containers
	// started on any but the (differently-coded) single-server path.
	if len(appCfg.Accessories) > 0 {
		fmt.Fprintln(s.out, "Ensuring accessories...")
		accMgr := accessories.NewManager(s.exec, s.out)
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

	// Check for env file.
	var envFile string
	envPath := fmt.Sprintf("/deployments/%s/.env", appCfg.App)
	if _, err := s.exec.Run(ctx, "test -f "+envPath); err == nil {
		envFile = envPath
	}

	// Resolve volumes.
	var volumes map[string]string
	if len(appCfg.Volumes) > 0 {
		volumes = make(map[string]string, len(appCfg.Volumes))
		for name, containerPath := range appCfg.Volumes {
			hostPath := fmt.Sprintf("/deployments/%s/volumes/%s", appCfg.App, name)
			volumes[hostPath] = containerPath
			if _, err := s.exec.Run(ctx, fmt.Sprintf("mkdir -p %s", hostPath)); err != nil {
				return fmt.Errorf("creating volume directory %s: %w", hostPath, err)
			}
		}
	}

	// Detect any existing container whose volume mount source doesn't match
	// what teploy resolved above — same guard as the single-server path
	// (deployAppConfig). Without it, redeploying an app first launched with a
	// Docker named volume / foreign bind mount would silently create empty
	// mounts and orphan the data. This runs on EVERY server of a multi-server
	// deploy (previously it only ran on the single-server path — see the old
	// TODO(autodeploy/multideploy)).
	if len(volumes) > 0 {
		dockerClient := docker.NewClient(s.exec)
		mismatches, err := dockerClient.DetectVolumeMismatches(ctx, appCfg.App, volumes)
		if err != nil {
			return fmt.Errorf("checking existing volume mounts: %w", err)
		}
		if len(mismatches) > 0 {
			if !s.migrateVolumes {
				return docker.FormatMismatchError(appCfg.App, mismatches)
			}
			if err := docker.MigrateVolumes(ctx, s.exec, appCfg.App, mismatches, s.out); err != nil {
				return fmt.Errorf("migrating volumes: %w", err)
			}
		}
	}

	// Upload custom TLS cert (if configured) before deploy.
	tlsCert, tlsKey, tlsInternal, err := resolveAppTLS(ctx, s.exec, appCfg)
	if err != nil {
		return err
	}
	if tlsCert != "" {
		fmt.Fprintln(s.out, "  TLS certificate uploaded")
	}

	// Decrypt secrets (set via `teploy secret set`). This was previously
	// missing entirely from the multi-server path — deploy.go (single
	// server) had it, singledeploy.go (multi-server, also used by scale/
	// parallel deploy) never called secret.NewManager at all, so any app
	// deployed to multiple servers silently got `undefined` for every
	// `teploy secret`-managed value.
	secretMgr := secret.NewManager(s.exec)
	deploySecrets, err := secretMgr.DecryptAll(ctx, appCfg.App)
	if err != nil {
		return fmt.Errorf("decrypting secrets: %w", err)
	}

	// Container env: teploy.yml's `env:` block, per-host tags (from
	// servers.yml), and decrypted secrets, uploaded to a fresh env file
	// rather than passed as `docker run -e` args — see
	// buildContainerEnvFiles for why.
	envFiles, err := buildContainerEnvFiles(ctx, s.exec, appCfg.App, envFile, appCfg.Env, tags, deploySecrets)
	if err != nil {
		return err
	}

	// Deploy.
	deployer := deploy.NewDeployer(s.exec, s.out)
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
	}

	return deployer.Deploy(ctx, deployCfg)
}
