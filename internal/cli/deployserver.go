package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"time"

	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/confighome"
	"github.com/useteploy/teploy/internal/dns"
	"github.com/useteploy/teploy/internal/docker"
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

	image = appCfg.Image
	if version == "" {
		version = versionFromImage(image)
		if version == "" {
			version = fmt.Sprintf("%d", time.Now().Unix())
		}
	} else if err := validateVersionArg(version); err != nil {
		return refuseAdmission(err)
	}
	fmt.Printf("Deploying %s from its Teploy config %s\n", app, short12(commit))

	if err := admitDeploymentVolumes(ctx, &state.FencedExecutor{Executor: executor, Lock: lk}, appCfg); err != nil {
		return err
	}
	att := releasemeta.MustAttempt(app, version)
	if err := ensureImage(ctx, docker.NewClient(executor), image, os.Stdout); err != nil {
		return err
	}
	if !skipDNSCheck && appCfg.Domain != "" &&
		appCfg.Ingress != config.IngressHost && appCfg.Ingress != config.IngressExternal {
		if current, _ := state.Read(ctx, executor, app); current == nil {
			fmt.Println("Validating DNS...")
			if err := dns.Validate(appCfg.Domain, host, nil); err != nil {
				return err
			}
			fmt.Println("  DNS validated")
		}
	}
	return deployBuiltImageFenced(ctx, executor, appCfg, image, version, host, migrateVolumes, false, "", lk, &att, "")
}
