package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"

	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/confighome"
	"github.com/useteploy/teploy/internal/releasemeta"
	"github.com/useteploy/teploy/internal/ssh"
	"github.com/useteploy/teploy/internal/state"
)

// seedFromServer rebuilds a deployed folder app's teploy.yml from its
// server records, and returns it only when it reproduces the running
// release's effective config exactly (spec §2.1).
func seedFromServer(ctx context.Context, exec ssh.Executor, app string) ([]byte, error) {
	st, err := state.Read(ctx, exec, app)
	if err != nil {
		return nil, err
	}
	if st == nil || st.CurrentHash == "" {
		return nil, fmt.Errorf("%s is not deployed on %s — nothing to rebuild a config from", app, exec.Host())
	}
	rec, err := releasemeta.Read(ctx, exec, app, st.CurrentHash)
	if err != nil {
		return nil, err
	}
	if rec == nil || rec.ManifestSHA256 == "" || len(st.AppliedManifest) == 0 {
		return nil, fmt.Errorf("the running release of %s (%s) predates applied-config records — redeploy it from its folder once, or move it from there with `teploy config home teploy`", app, st.CurrentHash)
	}
	seed, problems, err := confighome.Seed(rec, st.AppliedManifest)
	if err != nil {
		return nil, err
	}
	if len(problems) > 0 {
		return nil, fmt.Errorf("%s's config cannot be rebuilt from the server:\n  - %s\nmove it from its folder instead (`teploy config home teploy` there)", app, strings.Join(problems, "\n  - "))
	}
	ok, err := confighome.SeedMatches(seed, rec.ImageRef, rec.ManifestSHA256)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("the config rebuilt from %s's server records does not reproduce the running release exactly — move it from its folder instead (`teploy config home teploy` there)", app)
	}
	return seed, nil
}

// runInitFromServer bootstraps a folder for an app already on --host.
func runInitFromServer(flags *Flags, app string, force bool, out io.Writer) error {
	if flags.Host == "" {
		return fmt.Errorf("--from-server needs --host <server>")
	}
	if _, err := os.Stat("teploy.yml"); err == nil && !force {
		return fmt.Errorf("teploy.yml already exists (use --force to overwrite)")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	t, err := openConfigTarget(ctx, flags, app)
	if err != nil {
		return err
	}
	defer t.exec.Close()
	content, commit, err := readHead(ctx, t)
	if err == nil {
		if commit == "" {
			return fmt.Errorf("%s has no config commit yet", app)
		}
		if err := os.WriteFile("teploy.yml", append([]byte(pulledHeaderLine(app, commit, t.server)), content...), 0o600); err != nil {
			return err
		}
		fmt.Fprintf(out, "Wrote teploy.yml: a linked copy of %s's Teploy config %s (edit, then `teploy config push`)\n", app, short12(commit))
		return nil
	}
	seed, err := seedFromServer(ctx, t.exec, app)
	if err != nil {
		return err
	}
	cfg, err := config.LoadAppBytes(seed)
	if err != nil {
		return err
	}
	for _, k := range sortedKeys(cfg.Env) {
		if strings.Contains(cfg.Env[k], "$$") {
			return fmt.Errorf("env.%s contains a literal $, which a folder config cannot express (folder deploys expand $NAME) — move the app with `teploy config home teploy --from-server --app %s` instead", k, app)
		}
	}
	if err := os.WriteFile("teploy.yml", append([]byte("server: "+t.server+"\n"), seed...), 0o600); err != nil {
		return err
	}
	fmt.Fprintf(out, "Wrote teploy.yml for %s, rebuilt from %s and verified against the running release's config digest\n", app, t.server)
	return nil
}
