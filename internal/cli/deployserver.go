package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"github.com/useteploy/teploy/internal/build"
	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/confighome"
	"github.com/useteploy/teploy/internal/docker"
	"github.com/useteploy/teploy/internal/gitsource"
	"github.com/useteploy/teploy/internal/releasemeta"
	"github.com/useteploy/teploy/internal/ssh"
	"github.com/useteploy/teploy/internal/state"
)

// folderHasConfig reports whether the working directory holds an app
// config of its own (a pulled copy of a Teploy config does not count: it
// is not a folder home).
func folderHasConfig(destination string) bool {
	if destination != "" {
		return true
	}
	if raw, err := os.ReadFile("teploy.yml"); err == nil {
		if h, _ := parsePulledHeader(raw); h.app != "" {
			return false
		}
	}
	_, err := config.LoadApp(".")
	return !errors.Is(err, config.ErrNoConfig)
}

// deployServerConfigIfHome deploys appName from its server config when it
// has one (served=true); otherwise it returns served=false and the caller
// continues with the ad-hoc deploy.
func deployServerConfigIfHome(flags *Flags, serverName, appName, image, version string, skipDNSCheck, migrateVolumes, overrides bool) (served bool, err error) {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	host, user, key, err := config.ResolveServer(serverName, flags.Host, flags.User, flags.Key)
	if err != nil {
		return false, err
	}
	fmt.Printf("Connecting to %s@%s...\n", user, host)
	executor, err := ssh.Connect(ctx, ssh.ConnectConfig{Host: host, User: user, KeyPath: key})
	if err != nil {
		return false, err
	}
	defer executor.Close()
	exists, _, err := confighome.Head(ctx, executor, appName)
	if err != nil || !exists {
		return false, err
	}
	if overrides {
		return true, refuseAdmission(fmt.Errorf("%s's settings are edited in Teploy; --domain/--port do not override them — change them with `teploy config set --app %s --host %s domain=<domain> port=<port>`", appName, appName, serverName))
	}
	return true, deployServerConfig(ctx, executor, host, serverName, appName, image, version, skipDNSCheck, migrateVolumes)
}

// deployServerConfig deploys a Teploy-home app (spec §2.3): under the app
// lock it reads config.git@teploy, optionally commits image: <image> first
// (compare-and-swap, so `deploy --app X --image T` from CI keeps working),
// validates the config and runs the normal deploy, recording the commit.
func deployServerConfig(ctx context.Context, executor ssh.Executor, host, serverName, app, image, version string, skipDNSCheck, migrateVolumes bool) error {
	if err := docker.NewClient(executor).EnsureManagedDirectory(ctx, app, "", ""); err != nil {
		return fmt.Errorf("creating app directory: %w", err)
	}
	lk, err := state.AcquireLockFenced(ctx, executor, app)
	if err != nil {
		return err
	}
	defer state.ReleaseLockFenced(executor, lk, app)
	lk.StartRenewal(executor)
	return deployServerConfigLocked(ctx, executor, host, serverName, app, image, version, skipDNSCheck, migrateVolumes, lk, "")
}

// deployServerConfigLocked is deployServerConfig under a lock the caller
// already holds (the poll admitter) and an optional pre-admitted git
// commit: gitCommit != "" deploys exactly that commit; "" resolves the
// branch head the way a manual deploy does.
func deployServerConfigLocked(ctx context.Context, executor ssh.Executor, host, serverName, app, image, version string, skipDNSCheck, migrateVolumes bool, lk *state.Lock, gitCommit string) error {
	gate, err := confighome.CheckGate(ctx, executor, app, lk, os.Stdout)
	if err != nil {
		return refuseAdmission(err)
	}
	if !gate.Server {
		return refuseAdmission(fmt.Errorf("%s's settings were moved back to its folder; deploy it from there", app))
	}
	if gate.Head == "" {
		return refuseAdmission(fmt.Errorf("%s has no config yet — save one with `teploy config set --app %s --host %s --file teploy.yml --expect %s`", app, app, serverName, confighome.ZeroCommit))
	}
	raw, commit, err := confighome.Read(ctx, executor, app, gate.Head)
	if err != nil {
		return err
	}
	if image != "" {
		if cur, ok := confighome.GetPath(raw, "image"); !ok || cur != image {
			quoted := strconv.Quote(image)
			next, err := confighome.EditPath(raw, "image", &quoted)
			if err != nil {
				return err
			}
			t := &configTarget{app: app, server: serverName, exec: executor}
			if commit, err = commitConfig(ctx, t, raw, next, commit, "deploy --image "+image); err != nil {
				return err
			}
			raw = next
			fmt.Printf("Saved image: %s to %s's config (%s)\n", image, app, short12(commit))
		}
	}
	appCfg, err := confighome.Load(raw, app)
	if err != nil {
		return refuseAdmission(err)
	}
	if appCfg.IsStatic() {
		return refuseAdmission(fmt.Errorf("static sites in Teploy home need a code source, which this teploy version does not support yet — keep %s in folder home (`teploy config home folder`)", app))
	}
	resolveServerConfigEnv(appCfg)
	appCfg.Server = serverName
	appCfg.ConfigHome = confighome.HomeTeploy
	appCfg.ConfigCommit = commit

	// Git source: build the admitted (or, for a manual deploy, the
	// current) branch head from the server mirror.
	sourceCommit := ""
	if appCfg.Git != nil {
		if gitCommit != "" {
			sourceCommit = gitCommit
		} else {
			if refused, rerr := gitsource.RefusedHead(ctx, executor, app, appCfg.Git.Repository, appCfg.Git.Branch); rerr == nil && refused != "" {
				return refuseAdmission(fmt.Errorf("%s's branch %s was force-pushed to %s and is refused until the watermark is reset — run `teploy trigger reset --app %s --ref refs/heads/%s`", app, appCfg.Git.Branch, short12(refused), app, appCfg.Git.Branch))
			}
			if sourceCommit, err = gitsource.ResolveHead(ctx, executor, app, appCfg.Git); err != nil {
				return refuseAdmission(err)
			}
		}
		appCfg.SourceRevision = sourceCommit
	} else if appCfg.Image != "" {
		image = appCfg.Image
	}

	if version == "" {
		if appCfg.Git != nil {
			version = short12(sourceCommit)
		} else {
			version = versionFromImage(image)
			if version == "" {
				version = fmt.Sprintf("%d", time.Now().Unix())
			}
		}
	} else if err := validateVersionArg(version); err != nil {
		return refuseAdmission(err)
	}
	fmt.Printf("Deploying %s from its Teploy config %s\n", app, short12(commit))
	if appCfg.Git != nil {
		fmt.Printf("Building %s@%s from %s (branch %s)\n", app, short12(sourceCommit), appCfg.Git.Repository, appCfg.Git.Branch)
	}

	if err := admitDeploymentVolumes(ctx, &state.FencedExecutor{Executor: executor, Lock: lk}, appCfg); err != nil {
		return err
	}
	att := releasemeta.MustAttempt(app, version)
	if appCfg.Git != nil {
		built, err := buildFromGitSource(ctx, executor, appCfg, att, sourceCommit, os.Stdout)
		if err != nil {
			return err
		}
		image = built
	} else if err := ensureImage(ctx, docker.NewClient(executor), image, os.Stdout); err != nil {
		return err
	}
	if !skipDNSCheck && appCfg.Domain != "" &&
		appCfg.Ingress != config.IngressHost && appCfg.Ingress != config.IngressExternal {
		if current, _ := state.Read(ctx, executor, app); current == nil {
			fmt.Println("Validating DNS...")
			if err := validateDomainDNS(ctx, executor, appCfg.Domain, host); err != nil {
				return err
			}
			fmt.Println("  DNS validated")
		}
	}
	err = deployBuiltImageFenced(ctx, executor, appCfg, image, version, host, migrateVolumes, appCfg.Git != nil, "", lk, &att, "")
	if err != nil {
		return err
	}
	if appCfg.Git != nil && gitCommit == "" {
		// A manual deploy is a valid way past a retry cap; record it so
		// the next poll does not redo the same head.
		if rerr := gitsource.RecordManualDeploy(ctx, executor, app, appCfg.Git.Repository, appCfg.Git.Branch, sourceCommit, version); rerr != nil {
			fmt.Printf("Warning: could not advance %s's git watermark: %v\n", app, rerr)
		}
	}
	return nil
}

// buildFromGitSource exports the commit's tree from the per-app mirror
// into the attempt's build directory and builds it server-side (context:
// and dockerfile: are relative to the repository root). build_secrets are
// decrypted to 0600 files outside the build context, mounted with
// `docker build --secret`, and removed immediately after.
func buildFromGitSource(ctx context.Context, executor ssh.Executor, appCfg *config.AppConfig, att releasemeta.Attempt, commit string, out io.Writer) (string, error) {
	remoteDir := att.BuildDir()
	if _, err := executor.Run(ctx, att.MkdirCmd("build")); err != nil {
		return "", fmt.Errorf("creating build directory: %w", err)
	}
	contextDir := appCfg.Context
	if contextDir == "" {
		contextDir = "."
	}
	mode, hasGitmodules, err := gitsource.ExportArchive(ctx, executor, appCfg.App, remoteDir, commit, contextDir, appCfg.Dockerfile)
	if err != nil {
		return "", refuseAdmission(fmt.Errorf("exporting %s@%s from the mirror: %w", appCfg.App, short12(commit), err))
	}
	if hasGitmodules {
		return "", refuseAdmission(fmt.Errorf("%s's repository declares submodules, which this teploy version does not build — flatten the repository or keep the app in folder home", appCfg.App))
	}
	var buildMode build.Mode
	switch mode {
	case "dockerfile":
		buildMode = build.ModeDockerfile
	case "nixpacks":
		if len(appCfg.BuildSecrets) > 0 {
			return "", refuseAdmission(fmt.Errorf("build_secrets need a Dockerfile build (RUN --mount=type=secret) — %s's repository has none", appCfg.App))
		}
		buildMode = build.ModeNixpacks
	default:
		return "", fmt.Errorf("unknown build mode %q", mode)
	}
	secrets, cleanup, err := prepareGitBuildSecrets(ctx, executor, appCfg)
	if err != nil {
		return "", err
	}
	defer cleanup()
	image, err := build.NewBuilder(executor, out).WithSecrets(secrets).Build(ctx, build.BuildConfig{
		App:        appCfg.App,
		Version:    short12(commit),
		Mode:       buildMode,
		BuildDir:   remoteDir,
		Context:    appCfg.Context,
		Dockerfile: appCfg.Dockerfile,
		Platform:   appCfg.Platform,
	})
	if err != nil {
		return "", fmt.Errorf("building image: %w", err)
	}
	return image, nil
}

// prepareGitBuildSecrets decrypts each named age-store secret into a
// server-side private directory for the duration of one build.
func prepareGitBuildSecrets(ctx context.Context, executor ssh.Executor, appCfg *config.AppConfig) ([]build.BuildSecret, func(), error) {
	if len(appCfg.BuildSecrets) == 0 {
		return nil, func() {}, nil
	}
	dirOut, err := executor.Run(ctx, `base=/tmp; [ -d /dev/shm ] && base=/dev/shm; mktemp -d "$base/teploy-build-secrets.XXXXXXXX"`)
	if err != nil {
		return nil, func() {}, fmt.Errorf("creating the build-secret directory: %w", err)
	}
	dir := strings.TrimSpace(dirOut)
	if dir == "" || strings.Contains(dir, "\n") {
		return nil, func() {}, fmt.Errorf("unusable build-secret directory")
	}
	var out []build.BuildSecret
	for _, key := range appCfg.BuildSecrets {
		dest := dir + "/" + key
		if err := gitsource.SealBuildSecret(ctx, executor, appCfg.App, key, dest); err != nil {
			executor.Run(context.Background(), "rm -rf "+ssh.ShellQuote(dir))
			return nil, func() {}, refuseAdmission(fmt.Errorf("preparing build secret %s: %w", key, err))
		}
		out = append(out, build.BuildSecret{ID: key, Path: dest})
	}
	return out, func() {
		executor.Run(context.Background(), "rm -rf "+ssh.ShellQuote(dir))
	}, nil
}

// createServerConfigApp creates a NEW app in Teploy home from ad-hoc deploy
// flags and deploys it (dash Deploy, spec P2): under the app lock it checks
// that nothing exists yet, commits a minimal teploy.yml (app, image,
// domain, port), marks the app v3, then deploys from that config. An
// existing Teploy-home app deploys as usual (--image commits image: first);
// an existing folder app is refused — convert it explicitly.
func createServerConfigApp(flags *Flags, serverName, app, image, domain string, port int, version string, skipDNSCheck, migrateVolumes bool) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	host, user, key, err := config.ResolveServer(serverName, flags.Host, flags.User, flags.Key)
	if err != nil {
		return err
	}
	fmt.Printf("Connecting to %s@%s...\n", user, host)
	executor, err := ssh.Connect(ctx, ssh.ConnectConfig{Host: host, User: user, KeyPath: key})
	if err != nil {
		return err
	}
	defer executor.Close()

	fields := [][2]string{{"app", app}, {"image", strconv.Quote(image)}}
	if domain != "" {
		fields = append(fields, [2]string{"domain", strconv.Quote(domain)})
	}
	if port != 80 {
		fields = append(fields, [2]string{"port", strconv.Itoa(port)})
	}
	var raw []byte
	for _, f := range fields {
		v := f[1]
		if raw, err = confighome.EditPath(raw, f[0], &v); err != nil {
			return err
		}
	}
	if _, err := confighome.Load(raw, app); err != nil {
		return refuseAdmission(err)
	}

	t := &configTarget{app: app, server: serverName, exec: executor, unprivileged: flags.ConfigUnprivileged}
	existing, err := func() (bool, error) {
		lk, release, err := lockConfigTarget(ctx, t)
		if err != nil {
			return false, err
		}
		defer release()
		gate, err := confighome.CheckGate(ctx, executor, app, lk, os.Stdout)
		if err != nil {
			return false, refuseAdmission(err)
		}
		if gate.Server {
			return true, nil
		}
		if st, err := state.Read(ctx, executor, app); err != nil {
			return false, err
		} else if st != nil {
			return false, refuseAdmission(fmt.Errorf("%s is already deployed on %s with its settings in a folder — deploy it as before, or move it with `teploy config home teploy --from-server`", app, serverName))
		}
		return false, commitHomeTeploy(ctx, t, lk, raw, "create (deploy --home teploy)", os.Stdout)
	}()
	if err != nil {
		return err
	}
	if existing {
		fmt.Printf("%s is already edited in Teploy; deploying it with image %s\n", app, image)
	}
	return deployServerConfig(ctx, executor, host, serverName, app, image, version, skipDNSCheck, migrateVolumes)
}
