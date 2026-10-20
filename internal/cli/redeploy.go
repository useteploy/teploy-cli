package cli

// redeploy.go — `teploy redeploy` (server-authority plan D2): a fresh
// deploy of the CURRENT release's recorded image with env re-resolved
// from .env, secrets and the recorded config, through the normal fenced
// deploy engine. This is the operation that makes `teploy secret set` /
// `teploy env set` reach a folder app's containers (restart is docker
// stop/start and re-resolves nothing), and the dash Redeploy button.
//
// Homes:
//   - Teploy home (config.git): equivalent to deploy from config — the
//     server config is deployed under the app lock (deployServerConfig).
//   - Folder home, from the folder: the folder's teploy.yml deploys the
//     recorded image at the recorded version (env files and secrets are
//     re-read the way every deploy reads them).
//   - Folder home, folderless (--app X --host S): the config is rebuilt
//     from the server's own records (seedFromServer) and accepted ONLY
//     when it provably reproduces the running release (digest match);
//     anything the server did not record is an explicit error naming the
//     remedy, never a guess.
//
// Static apps are refused: a static release is files on disk, and the
// operations that make sense there are deploy and rollback.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"

	"github.com/spf13/cobra"
	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/confighome"
	"github.com/useteploy/teploy/internal/docker"
	"github.com/useteploy/teploy/internal/releasemeta"
	"github.com/useteploy/teploy/internal/ssh"
	"github.com/useteploy/teploy/internal/state"
)

// redeployKey labels the deploy events a redeploy produces (detail
// trigger=redeploy on the ordinary deploy kind — one engine, one funnel,
// one history).
type redeployKey struct{}

func withRedeployTrigger(ctx context.Context) context.Context {
	return context.WithValue(ctx, redeployKey{}, true)
}

func eventTrigger(ctx context.Context) string {
	if _, ok := ctx.Value(redeployKey{}).(bool); ok {
		return "redeploy"
	}
	return ""
}

// redeployDTO is the `redeploy --json` result.
type redeployDTO struct {
	MachineInterface int    `json:"machine_interface"`
	App              string `json:"app"`
	Server           string `json:"server"`
	Home             string `json:"home"`
	Release          string `json:"release"`
	Image            string `json:"image"`
	ConfigCommit     string `json:"config_commit,omitempty"`
	OK               bool   `json:"ok"`
}

func newRedeployCmd(flags *Flags) *cobra.Command {
	var appName string
	cmd := &cobra.Command{
		Use:   "redeploy",
		Short: "Redeploy the current release with fresh env and secrets",
		Long: `Redeploys the app's CURRENT release: the recorded image, at the same
version, with env re-resolved from .env, ` + "`teploy secret`" + ` and the recorded
config, through the normal fenced deploy engine (health check, traffic
switch, rollback safety). This is how a secret or env edit reaches
containers — restart only stops and starts them.

For an app edited in Teploy this is the same as deploying its server
config. From an app folder, the folder's teploy.yml is used. Without a
folder, pass --app X --host S: the config is rebuilt from the server's
records and only used when it reproduces the running release exactly —
anything unrecorded (accessories, hooks, secret-looking env values) is
refused with the remedy instead of guessed at.

Static apps are refused (deploy or rollback instead). --json prints
{app, server, home, release, image, ok}; progress goes to stderr.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
			defer cancel()
			// Under --json, stdout carries only the result document; the
			// deploy paths print progress through os.Stdout/fmt defaults,
			// which are swapped to stderr for the duration.
			restore := func() {}
			if flags.JSON {
				real := os.Stdout
				os.Stdout = os.Stderr
				restore = func() { os.Stdout = real }
			}
			res, err := runRedeploy(ctx, flags, appName)
			restore()
			if err != nil {
				return err
			}
			res.MachineInterface = MachineInterface
			res.OK = true
			if flags.JSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(res)
			}
			fmt.Printf("Redeployed %s (release %s, image %s)\n", res.App, res.Release, res.Image)
			return nil
		},
	}
	cmd.Flags().StringVar(&appName, "app", "", "app name — act on the server instead of teploy.yml (requires --host)")
	return cmd
}

func runRedeploy(ctx context.Context, flags *Flags, appName string) (redeployDTO, error) {
	var folderCfg *config.AppConfig
	if appName == "" {
		appCfg, err := config.LoadApp(".")
		if err != nil {
			return redeployDTO{}, err
		}
		if appCfg.IsStatic() {
			return redeployDTO{}, fmt.Errorf("redeploy is for container apps — %s is type:static; run teploy deploy (or teploy rollback) instead", appCfg.App)
		}
		folderCfg = appCfg
		appName = appCfg.App
	}

	serverName := flags.Host
	if folderCfg != nil {
		if s := folderCfg.Server; s != "" {
			serverName = s
		} else if len(folderCfg.Servers) == 1 {
			serverName = folderCfg.Servers[0]
		}
	}
	if serverName == "" {
		return redeployDTO{}, fmt.Errorf("no server for %s — pass --host <server> or set 'server' in teploy.yml", appName)
	}
	if err := config.ValidateName(appName); err != nil {
		return redeployDTO{}, err
	}
	host, user, key, err := config.ResolveServer(serverName, flags.Host, flags.User, flags.Key)
	if err != nil {
		return redeployDTO{}, err
	}
	if folderCfg != nil {
		user = config.EffectiveUser(user, flags.User, folderCfg.User)
	}
	if !flags.JSON {
		fmt.Printf("Connecting to %s@%s...\n", user, host)
	}
	executor, err := ssh.Connect(ctx, ssh.ConnectConfig{Host: host, User: user, KeyPath: key})
	if err != nil {
		return redeployDTO{}, err
	}
	defer executor.Close()

	// A pulled copy of a Teploy config deploys the server config it links
	// to — same rule as `teploy deploy`.
	if folderCfg != nil {
		if raw, rerr := os.ReadFile("teploy.yml"); rerr == nil {
			if h, _ := parsePulledHeader(raw); h.app == appName {
				folderCfg = nil
			}
		}
	}

	// Teploy home: equivalent to deploy from config (spec §2.3).
	exists, _, err := confighome.Head(ctx, executor, appName)
	if err != nil {
		return redeployDTO{}, fmt.Errorf("checking %s's config home: %w", appName, err)
	}
	home := confighome.HomeFolder
	if exists {
		home = confighome.HomeTeploy
		if folderCfg != nil {
			// A plain folder deploy of a Teploy-home app is refused by
			// `teploy deploy`; redeploy keeps the same rule.
			return redeployDTO{}, refuseAdmission(confighome.RefuseFolderDeploy(appName, ""))
		}
		if err := deployServerConfig(withRedeployTrigger(ctx), executor, host, serverName, appName, "", "", true, false); err != nil {
			return redeployDTO{}, err
		}
		return redeployResult(ctx, executor, appName, serverName, home)
	}

	st, err := state.Read(ctx, executor, appName)
	if err != nil {
		return redeployDTO{}, fmt.Errorf("reading state for %q: %w", appName, err)
	}
	if st == nil {
		return redeployDTO{}, fmt.Errorf("no state found for app %q on %s — has it been deployed?", appName, serverName)
	}
	if st.DeploymentType == "static" {
		return redeployDTO{}, fmt.Errorf("redeploy is for container apps — %s is type:static; run teploy deploy (or teploy rollback) instead", appName)
	}
	rec, err := releasemeta.Read(ctx, executor, appName, st.CurrentHash)
	if err != nil {
		return redeployDTO{}, err
	}
	if rec == nil || rec.ImageRef == "" {
		return redeployDTO{}, fmt.Errorf("the running release of %s (%s) has no recorded image — it predates release records; redeploy it from its source, or move it with `teploy config home teploy --from-server`", appName, st.CurrentHash)
	}

	if folderCfg != nil {
		// Folder home, from the folder: the folder's config deploys the
		// recorded image at the recorded version. Env files and secrets
		// are re-resolved exactly as a deploy resolves them.
		if err := resolveDeployEnv(ctx, folderCfg, flags.StrictEnv); err != nil {
			return redeployDTO{}, err
		}
		folderCfg.Server = serverName
		if err := deployRecordedRelease(withRedeployTrigger(ctx), executor, host, serverName, folderCfg, st, rec); err != nil {
			return redeployDTO{}, err
		}
		return redeployResult(ctx, executor, appName, serverName, home)
	}

	// Folder home, folderless: rebuild from the server's records — only a
	// provably exact rebuild is used (seedFromServer), anything else is an
	// explicit error naming the remedy.
	seed, err := seedFromServer(ctx, executor, appName)
	if err != nil {
		return redeployDTO{}, err
	}
	seedCfg, err := config.LoadAppBytes(seed)
	if err != nil {
		return redeployDTO{}, refuseAdmission(fmt.Errorf("the config rebuilt from %s's server records does not load: %w", appName, err))
	}
	resolveServerConfigEnv(seedCfg)
	seedCfg.Server = serverName
	if err := deployRecordedRelease(withRedeployTrigger(ctx), executor, host, serverName, seedCfg, st, rec); err != nil {
		return redeployDTO{}, err
	}
	return redeployResult(ctx, executor, appName, serverName, home)
}

// deployRecordedRelease performs the folder-home redeploy under the app
// lock with an already-resolved config (the folder's teploy.yml, or the
// seed rebuilt from the server's records): re-check the gate and the
// current release, then deploy the recorded image through the fenced
// engine at the recorded version.
func deployRecordedRelease(ctx context.Context, executor ssh.Executor, host, serverName string, appCfg *config.AppConfig, st *state.AppState, rec *releasemeta.Record) error {
	if rec.Provenance != nil {
		appCfg.SourceRevision = rec.Provenance.Revision
	}
	if appCfg.SourceYAML == nil {
		if raw, ok, err := readRecordedSource(ctx, executor, rec); err == nil && ok {
			appCfg.SourceYAML = raw
		}
	}

	app := appCfg.App
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
	if gate.Server {
		return refuseAdmission(fmt.Errorf("%s's settings moved to Teploy while this redeploy ran — run `teploy redeploy --app %s --host %s` again to deploy the server config", app, app, serverName))
	}
	// The release facts were read before the lock; refuse if the app moved
	// in between rather than redeploying a stale choice.
	cur, err := state.Read(ctx, executor, app)
	if err != nil || cur == nil {
		return fmt.Errorf("re-reading state for %s: %w", app, err)
	}
	if cur.CurrentHash != st.CurrentHash {
		return fmt.Errorf("%s deployed release %s while this redeploy was starting — run teploy redeploy again", app, cur.CurrentHash)
	}

	if err := admitDeploymentVolumes(ctx, &state.FencedExecutor{Executor: executor, Lock: lk}, appCfg); err != nil {
		return err
	}
	image, err := resolveRedeployImage(ctx, executor, rec)
	if err != nil {
		return err
	}
	att := releasemeta.MustAttempt(app, st.CurrentHash)
	return deployBuiltImageFenced(ctx, executor, appCfg, image, st.CurrentHash, host, false, false, "", lk, &att, "")
}

// resolveRedeployImage pins the redeploy to the RECORDED image: the
// digest-pinned ref as-is; a tag whose local image ID still equals the
// recorded digest; otherwise the recorded digest itself (a moved tag must
// not turn a redeploy into an upgrade). Absent images are pulled first.
func resolveRedeployImage(ctx context.Context, executor ssh.Executor, rec *releasemeta.Record) (string, error) {
	ref := rec.ImageRef
	if isDigestPinned(ref) {
		return ref, nil
	}
	dk := docker.NewClient(executor)
	exists, err := dk.ImageExists(ctx, ref)
	if err != nil {
		return "", err
	}
	if !exists {
		if err := ensureImage(ctx, dk, ref, os.Stderr); err != nil {
			return "", err
		}
	}
	if rec.ImageDigest == "" {
		// Nothing recorded to verify against (pre-F19 record): the ref is
		// the best fact available.
		return ref, nil
	}
	id, err := dk.ResolveImageID(ctx, ref)
	if err != nil {
		return "", err
	}
	if id == rec.ImageDigest {
		return ref, nil
	}
	return rec.ImageDigest, nil
}

// readRecordedSource returns the masked folder config the release
// deployed (meta/<hash>.source.yml) when its digest still matches the
// record — carried over so a same-version redeploy does not orphan it.
func readRecordedSource(ctx context.Context, executor ssh.Executor, rec *releasemeta.Record) ([]byte, bool, error) {
	if rec.SourceSHA256 == "" {
		return nil, false, nil
	}
	p, err := releasemeta.SourcePath(rec.App, rec.Hash)
	if err != nil {
		return nil, false, err
	}
	raw, present, err := state.ReadRemoteFile(ctx, executor, p)
	if err != nil || !present {
		return nil, false, err
	}
	if digest := sha256Hex(raw); digest != rec.SourceSHA256 {
		return nil, false, nil
	}
	return raw, true, nil
}

func redeployResult(ctx context.Context, executor ssh.Executor, app, serverName, home string) (redeployDTO, error) {
	res := redeployDTO{App: app, Server: serverName, Home: home}
	st, err := state.Read(ctx, executor, app)
	if err != nil || st == nil {
		return res, nil
	}
	res.Release = st.CurrentHash
	res.Image = st.ImageRef
	if rec, err := releasemeta.Read(ctx, executor, app, st.CurrentHash); err == nil && rec != nil {
		res.ConfigCommit = rec.ConfigCommit
	}
	return res, nil
}
