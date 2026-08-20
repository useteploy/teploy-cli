package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"

	"github.com/spf13/cobra"
	"github.com/useteploy/teploy/internal/build"
	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/docker"
	"github.com/useteploy/teploy/internal/ssh"
)

// `teploy build` — make the app's image exist on the server, and stop there.
//
// Why this exists: `teploy preview deploy` runs an image that must ALREADY be
// on the server at the current git hash — it falls back to `<app>-build-<hash>`
// and never builds one (internal/preview/preview.go). Until now the only thing
// that produced that image was `teploy deploy`, which also replaces the running
// production containers. So "build this branch and show it to me on a preview
// URL" was impossible without shipping the branch to production first.
//
// That is a hole for anyone scripting previews, and a hard blocker for an
// automated fixer (teploy-ship) whose whole premise is that a machine-authored
// change reaches a URL a human can look at WITHOUT reaching production.
//
// It deliberately does nothing else: no container is started or stopped, no
// Caddy route is touched, no state file is written, no DNS is checked, and no
// old images are pruned. The single postcondition is "the image named on the
// last line exists on the server".
func newBuildCmd(flags *Flags) *cobra.Command {
	var version, destination string

	cmd := &cobra.Command{
		Use:   "build",
		Short: "Build the app image without deploying it",
		Long: `Build the image described by teploy.yml and leave it on the server.

Nothing is deployed: no container is replaced, no route changes, no state is
written. Use it to prepare an image for ` + "`teploy preview deploy`" + `, or to
check that a branch builds at all.

When teploy.yml pins a pre-built ` + "`image:`" + `, there is nothing to build —
the image is pulled if the server does not already have it, so the
postcondition ("that image is on the server") holds either way.

Example:
  git checkout fix/login-500
  teploy build
  teploy preview deploy fix/login-500 --ttl 24h`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBuild(flags, version, destination)
		},
	}

	cmd.Flags().StringVar(&version, "version", "", "version identifier (default: git short hash)")
	cmd.Flags().StringVarP(&destination, "destination", "d", "", "destination overlay (e.g. staging merges teploy.staging.yml)")

	return cmd
}

func runBuild(flags *Flags, version, destination string) error {
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

	// A static app has no image at all — saying so is more useful than
	// building nothing and reporting success.
	if appCfg.IsStatic() {
		return fmt.Errorf("%s is a static app — there is no image to build", appCfg.App)
	}

	serverName := appCfg.Server
	if serverName == "" && len(appCfg.Servers) > 0 {
		serverName = appCfg.Servers[0]
	}
	if serverName == "" {
		return fmt.Errorf("no server specified — set 'server' in teploy.yml")
	}
	host, user, key, err := config.ResolveServer(serverName, flags.Host, flags.User, flags.Key)
	if err != nil {
		return err
	}
	user = config.EffectiveUser(user, flags.User, appCfg.User)

	image := appCfg.Image
	needsBuild := image == ""

	// The version is what names the built tag, and `teploy preview deploy`
	// re-derives the same name from the same git hash — so a build and the
	// preview that consumes it agree only because both read this repo. Keep
	// --version available for the case where they cannot.
	if version == "" {
		version, err = gitShortHash()
		if err != nil {
			return fmt.Errorf("could not determine version from git: %w (use --version)", err)
		}
	}

	var buildMode build.Mode
	if needsBuild {
		buildMode, err = build.DetectAt(appCfg.Context, appCfg.Dockerfile)
		if err != nil {
			return err
		}
		if !flags.JSON {
			fmt.Printf("No image specified — detected %s build\n", buildMode)
		}
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	if !flags.JSON {
		fmt.Printf("Connecting to %s@%s...\n", user, host)
	}
	executor, err := ssh.Connect(ctx, ssh.ConnectConfig{Host: host, User: user, KeyPath: key})
	if err != nil {
		return err
	}
	defer executor.Close()

	out := io.Writer(os.Stdout)
	if flags.JSON {
		// Progress must not land on stdout in --json mode: a consumer reading
		// stdout as one payload would get build chatter followed by JSON.
		out = os.Stderr
	}

	if !needsBuild {
		// Pinned image: nothing to build, but the postcondition is the same.
		if err := ensureImage(ctx, docker.NewClient(executor), image, out); err != nil {
			return err
		}
		return reportBuild(flags, image, version, false)
	}

	if appCfg.BuildLocal {
		fmt.Fprintln(out, "Building image locally...")
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
		}, out)
		if err != nil {
			return fmt.Errorf("local build: %w", err)
		}
		return reportBuild(flags, image, version, true)
	}

	remoteDir := fmt.Sprintf("/deployments/%s/build", appCfg.App)
	if _, err := executor.Run(ctx, "mkdir -p "+remoteDir); err != nil {
		return fmt.Errorf("creating build directory: %w", err)
	}
	fmt.Fprintln(out, "Syncing source to server...")
	if err := build.Sync(ctx, build.SyncConfig{
		LocalDir:  ".",
		RemoteDir: remoteDir,
		Host:      host,
		User:      user,
		KeyPath:   key,
		Excludes:  build.LoadIgnore("."),
	}, out, os.Stderr); err != nil {
		return fmt.Errorf("syncing source: %w", err)
	}

	fmt.Fprintln(out, "Building image on server...")
	image, err = build.NewBuilder(executor, out).Build(ctx, build.BuildConfig{
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
	return reportBuild(flags, image, version, true)
}

// reportBuild writes the one thing a caller needs: the image tag.
//
// The human line is last so `teploy build | tail -1` is usable; scripts should
// prefer --json, which emits nothing else on stdout.
func reportBuild(flags *Flags, image, version string, built bool) error {
	if flags.JSON {
		return json.NewEncoder(os.Stdout).Encode(map[string]any{
			"image":   image,
			"version": version,
			"built":   built,
		})
	}
	if built {
		fmt.Printf("Built image: %s\n", image)
	} else {
		fmt.Printf("Image ready: %s\n", image)
	}
	return nil
}
