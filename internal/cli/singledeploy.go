package cli

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/useteploy/teploy/internal/build"
	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/deploy"
	"github.com/useteploy/teploy/internal/docker"
	"github.com/useteploy/teploy/internal/ssh"
)

// singleServerDeployer wraps the deploy logic for a single server.
// Used by both the deploy command and multi-server scale/parallel deploy.
type singleServerDeployer struct {
	exec    ssh.Executor
	out     io.Writer
	keyPath string
}

func newSingleServerDeployer(exec ssh.Executor, out io.Writer, keyPath string) *singleServerDeployer {
	return &singleServerDeployer{exec: exec, out: out, keyPath: keyPath}
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

	// Upload custom TLS cert (if configured) before deploy.
	var tlsCert, tlsKey string
	if appCfg.TLS != nil {
		tlsCert, tlsKey, err = uploadAppTLS(ctx, s.exec, appCfg.App, appCfg.TLS)
		if err != nil {
			return err
		}
		fmt.Fprintln(s.out, "  TLS certificate uploaded")
	}

	// Container env: the teploy.yml `env:` block (with ${VAR} expanded from the
	// local environment), then per-host tags (from servers.yml) layered on top.
	containerEnv := make(map[string]string, len(appCfg.Env)+len(tags))
	for k, v := range appCfg.Env {
		containerEnv[k] = os.Expand(v, os.Getenv)
	}
	for k, v := range tags {
		containerEnv[k] = v
	}

	// Deploy.
	deployer := deploy.NewDeployer(s.exec, s.out)
	deployCfg := deploy.Config{
		App:           appCfg.App,
		Domain:        appCfg.Domain,
		Image:         image,
		Version:       version,
		EnvFile:       envFile,
		Env:           containerEnv,
		Volumes:       volumes,
		Processes:     appCfg.Processes,
		NoHealthcheck: disabledHealthchecks(appCfg.Healthcheck),
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
	}

	return deployer.Deploy(ctx, deployCfg)
}
