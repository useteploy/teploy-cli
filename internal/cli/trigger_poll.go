package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"

	"github.com/spf13/cobra"
	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/confighome"
	"github.com/useteploy/teploy/internal/docker"
	"github.com/useteploy/teploy/internal/events"
	"github.com/useteploy/teploy/internal/gitsource"
	"github.com/useteploy/teploy/internal/ssh"
	"github.com/useteploy/teploy/internal/state"
)

// `teploy trigger poll` — the ONE admitter for git-source apps (D4 as
// amended by the P3 plan): webhooks and the server timer only wake it.
// Dedup by (app, ref, commit); a per-ref watermark with an ancestry check
// on the mirror; one recorded refusal per force-push (refused until
// `trigger reset`); changed paths filtered by git.paths (defaulting to
// context:); a failed commit retried at most twice; the newest admitted
// head always wins.

const (
	triggerPollService = "teploy-trigger-poll.service"
	triggerPollTimer   = "teploy-trigger-poll.timer"
	serverTeployBinDir = "/deployments/.bin"
)

func newTriggerPollCmd(flags *Flags) *cobra.Command {
	var (
		appName, hint    string
		now, all, asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "poll [--app X --host S [--now] [--hint <sha>]] | --all",
		Short: "Admit and deploy new heads of git-source apps (the one admitter)",
		Long: `Checks each app's branch with its stored credential and deploys the
newest head that passes admission: not a duplicate of the watermark, a
descendant of it (a force-push is refused, once, until ` + "`teploy trigger reset`" + `),
matching git.paths when set, and not past its retry budget (a failed
commit is retried at most twice; then only a new commit, ` + "`teploy trigger retry`" + `
or a redeploy restarts it).

--app polls one app now (a webhook wake passes --now --hint <sha>; the
remote head always wins over the hint). --all polls every git-source app
on the server whose interval is due — the systemd timer
(` + "`teploy trigger install-timer`" + `) runs it every minute. --json prints one
result object per app.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
			defer cancel()
			out := cmd.OutOrStdout()
			restore := quietDeployNarration(asJSON)
			defer restore()
			if all {
				return runTriggerPollAll(ctx, flags, asJSON, out)
			}
			if appName == "" {
				return fmt.Errorf("--app is required (or --all)")
			}
			t, err := openConfigTarget(ctx, flags, appName)
			if err != nil {
				return err
			}
			defer t.exec.Close()
			res, err := pollGitApp(ctx, t, now, hint)
			if asJSON {
				res.MachineInterface = MachineInterface
				res.Host = t.server
				if encodeErr := encodeJSONLine(out, res); encodeErr != nil && err == nil {
					return encodeErr
				}
				return err
			}
			if err != nil {
				return err
			}
			printPollResult(out, res)
			return nil
		},
	}
	cmd.Flags().StringVar(&appName, "app", "", "app to poll")
	cmd.Flags().StringVar(&hint, "hint", "", "advisory head from a webhook (the remote head wins)")
	cmd.Flags().BoolVar(&now, "now", false, "poll immediately: ignore the interval and poll: off")
	cmd.Flags().BoolVar(&all, "all", false, "poll every due git-source app on the server (timer mode)")
	cmd.Flags().BoolVar(&asJSON, "json", flags.JSON, "machine-readable output (one JSON object per app)")
	return cmd
}

// quietDeployNarration moves the deploy engine's human narration to
// stderr while JSON goes to the captured stdout writer. Returns the
// restore func (always call).
func quietDeployNarration(asJSON bool) func() {
	if !asJSON {
		return func() {}
	}
	orig := os.Stdout
	os.Stdout = os.Stderr
	return func() { os.Stdout = orig }
}

// pollGitApp runs one app's poll under its fenced lock. A busy lock is a
// result ("locked"), not an error: the next minute's timer catches up.
func pollGitApp(ctx context.Context, t *configTarget, now bool, hint string) (gitsource.PollResult, error) {
	exec := t.exec
	if err := docker.NewClient(exec).EnsureManagedDirectory(ctx, t.app, "", ""); err != nil {
		return gitsource.PollResult{}, fmt.Errorf("creating app directory: %w", err)
	}
	lk, err := state.AcquireLockFenced(ctx, exec, t.app)
	if err != nil {
		if strings.Contains(err.Error(), "already locked") {
			return gitsource.PollResult{App: t.app, Reason: "locked"}, nil
		}
		return gitsource.PollResult{}, err
	}
	defer state.ReleaseLockFenced(exec, lk, t.app)
	lk.StartRenewal(exec)

	gate, err := confighome.CheckGate(ctx, exec, t.app, lk, os.Stdout)
	if err != nil {
		return gitsource.PollResult{}, err
	}
	if !gate.Server {
		return gitsource.PollResult{}, fmt.Errorf("%s's settings are not edited in Teploy — git sources are a Teploy-home feature", t.app)
	}
	if gate.Head == "" {
		return gitsource.PollResult{}, fmt.Errorf("%s has no config yet", t.app)
	}
	raw, commit, err := confighome.Read(ctx, exec, t.app, gate.Head)
	if err != nil {
		return gitsource.PollResult{}, err
	}
	appCfg, err := confighome.Load(raw, t.app)
	if err != nil {
		return gitsource.PollResult{}, err
	}
	if appCfg.Git == nil {
		return gitsource.PollResult{MachineInterface: MachineInterface, App: t.app, Reason: "no-git-source"}, nil
	}
	return gitsource.Poll(ctx, gitsource.PollDeps{
		Exec:  exec,
		App:   t.app,
		AppCf: appCfg,
		Now:   now,
		Hint:  hint,
		RecordEvent: func(kind, outcome string, detail map[string]any) {
			events.Record(ctx, exec, events.Event{App: t.app, Kind: kind, Outcome: outcome, ConfigCommit: commit, Detail: detail})
		},
		Deploy: func(ctx context.Context, sourceCommit, version string) error {
			return deployServerConfigLocked(ctx, exec, exec.Host(), t.server, t.app, "", version, true, false, lk, sourceCommit)
		},
	})
}

// gitApp is one enumerated git-source app on a server.
type gitApp struct {
	Name string
	Cfg  *config.AppConfig
}

// listGitApps enumerates the server's Teploy-home apps that build from
// git (used by --all and doctor).
func listGitApps(ctx context.Context, exec ssh.Executor) ([]gitApp, error) {
	out, err := exec.Run(ctx, "ls -1 "+ssh.ShellQuote(confighome.AppsDir())+" 2>/dev/null || true")
	if err != nil {
		return nil, fmt.Errorf("listing apps: %w", err)
	}
	var apps []gitApp
	for _, name := range strings.Fields(out) {
		if name == "" || strings.ContainsAny(name, "/.") {
			continue
		}
		if err := config.ValidateName(name); err != nil {
			continue
		}
		exists, head, err := confighome.Head(ctx, exec, name)
		if err != nil || !exists || head == "" {
			continue
		}
		raw, _, err := confighome.Read(ctx, exec, name, head)
		if err != nil {
			continue
		}
		cfg, err := confighome.Load(raw, name)
		if err != nil || cfg.Git == nil {
			continue
		}
		apps = append(apps, gitApp{Name: name, Cfg: cfg})
	}
	return apps, nil
}

func runTriggerPollAll(ctx context.Context, flags *Flags, asJSON bool, out io.Writer) error {
	var exec ssh.Executor
	if flags.Host != "" {
		host, user, key, err := config.ResolveServer(flags.Host, flags.Host, flags.User, flags.Key)
		if err != nil {
			return err
		}
		ex, err := ssh.Connect(ctx, ssh.ConnectConfig{Host: host, User: user, KeyPath: key})
		if err != nil {
			return err
		}
		defer ex.Close()
		exec = ex
	} else {
		// Timer mode: running on the server itself.
		exec = ssh.NewLocalExecutor()
	}
	apps, err := listGitApps(ctx, exec)
	if err != nil {
		return err
	}
	for _, app := range apps {
		t := &configTarget{app: app.Name, server: flags.Host, exec: exec}
		res, err := pollGitApp(ctx, t, false, "")
		if err != nil {
			fmt.Fprintf(os.Stderr, "trigger poll %s: %v\n", app.Name, err)
			continue
		}
		if asJSON {
			res.MachineInterface = MachineInterface
			res.Host = flags.Host
			if err := encodeJSONLine(out, res); err != nil {
				return err
			}
		} else if res.Reason != "not-due" && res.Reason != "" {
			printPollResult(out, res)
		}
	}
	return nil
}

func printPollResult(out io.Writer, res gitsource.PollResult) {
	switch res.Reason {
	case gitsource.ReasonDeployed:
		fmt.Fprintf(out, "%s: deployed %s (release %s)\n", res.App, short12(res.Head), res.Release)
	case gitsource.ReasonUpToDate:
		fmt.Fprintf(out, "%s: up to date at %s\n", res.App, short12(res.Head))
	case "":
		fmt.Fprintf(out, "%s: done\n", res.App)
	default:
		fmt.Fprintf(out, "%s: %s (%s)\n", res.App, res.Reason, short12(res.Head))
	}
}

func encodeJSONLine(w io.Writer, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, string(data))
	return err
}

// gitSourceOf reads the app's git: block from its Teploy config.
func gitSourceOf(ctx context.Context, t *configTarget) (*config.GitSource, error) {
	raw, _, err := readHead(ctx, t)
	if err != nil {
		return nil, err
	}
	cfg, err := config.LoadAppBytes(raw)
	if err != nil {
		return nil, err
	}
	if cfg.Git == nil {
		return nil, fmt.Errorf("%s has no git: source in its config", t.app)
	}
	return cfg.Git, nil
}

func newTriggerResetCmd(flags *Flags) *cobra.Command {
	var appName, ref string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "reset",
		Short: "Clear a ref's watermark (re-admit after a force-push)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if appName == "" || ref == "" {
				return fmt.Errorf("--app and --ref refs/heads/<branch> are required")
			}
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
			defer cancel()
			out := cmd.OutOrStdout()
			t, err := openConfigTarget(ctx, flags, appName)
			if err != nil {
				return err
			}
			defer t.exec.Close()
			g, err := gitSourceOf(ctx, t)
			if err != nil {
				return err
			}
			if ref != "refs/heads/"+g.Branch {
				return fmt.Errorf("%s builds %s's branch %s — reset --ref refs/heads/%s", t.app, g.Repository, g.Branch, g.Branch)
			}
			if err := gitsource.ResetRef(ctx, t.exec, t.app, g.Repository, ref); err != nil {
				return err
			}
			events.Record(ctx, t.exec, events.Event{App: t.app, Kind: events.KindWatermarkReset, Outcome: events.Succeeded, Detail: map[string]any{"ref": ref}})
			if asJSON {
				return encodeJSONLine(out, map[string]any{"machine_interface": MachineInterface, "app": t.app, "ref": ref, "reset": true})
			}
			fmt.Fprintf(out, "Cleared %s's %s watermark — the next poll admits the current head\n", t.app, ref)
			return nil
		},
	}
	cmd.Flags().StringVar(&appName, "app", "", "app (required)")
	cmd.Flags().StringVar(&ref, "ref", "", "ref to reset, e.g. refs/heads/main (required)")
	cmd.Flags().BoolVar(&asJSON, "json", flags.JSON, "machine-readable output")
	return cmd
}

func newTriggerRetryCmd(flags *Flags) *cobra.Command {
	var appName string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "retry",
		Short: "Reset a failed commit's retry budget and poll it now",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if appName == "" {
				return fmt.Errorf("--app is required")
			}
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
			defer cancel()
			out := cmd.OutOrStdout()
			restore := quietDeployNarration(asJSON)
			defer restore()
			t, err := openConfigTarget(ctx, flags, appName)
			if err != nil {
				return err
			}
			defer t.exec.Close()
			g, err := gitSourceOf(ctx, t)
			if err != nil {
				return err
			}
			if _, err := gitsource.RetryClear(ctx, t.exec, t.app, g.Repository, g.Branch); err != nil {
				return err
			}
			res, err := pollGitApp(ctx, t, true, "")
			if asJSON {
				res.MachineInterface = MachineInterface
				res.Host = t.server
				if encodeErr := encodeJSONLine(out, res); encodeErr != nil && err == nil {
					return encodeErr
				}
				return err
			}
			if err != nil {
				return err
			}
			printPollResult(out, res)
			return nil
		},
	}
	cmd.Flags().StringVar(&appName, "app", "", "app (required)")
	cmd.Flags().BoolVar(&asJSON, "json", flags.JSON, "machine-readable output")
	return cmd
}

func newTriggerInstallTimerCmd(flags *Flags) *cobra.Command {
	var binaryPath string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "install-timer --host <server>",
		Short: "Install the systemd timer running `trigger poll --all` every minute",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if flags.Host == "" {
				return fmt.Errorf("--host <server> is required")
			}
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
			defer cancel()
			host, user, key, err := config.ResolveServer(flags.Host, flags.Host, flags.User, flags.Key)
			if err != nil {
				return err
			}
			exec, err := ssh.Connect(ctx, ssh.ConnectConfig{Host: host, User: user, KeyPath: key})
			if err != nil {
				return err
			}
			defer exec.Close()
			bin, err := ensureTriggerBinary(ctx, exec, binaryPath)
			if err != nil {
				return err
			}
			if err := installTriggerUnits(ctx, exec, bin); err != nil {
				return err
			}
			if asJSON {
				return encodeJSONLine(cmd.OutOrStdout(), map[string]any{"machine_interface": MachineInterface, "host": flags.Host, "timer": triggerPollTimer, "binary": bin})
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Installed %s (running %s trigger poll --all every minute)\n", triggerPollTimer, bin)
			return nil
		},
	}
	cmd.Flags().StringVar(&binaryPath, "binary", "", "upload this local teploy binary for the timer instead of the latest release")
	cmd.Flags().BoolVar(&asJSON, "json", flags.JSON, "machine-readable output")
	return cmd
}

func newTriggerUninstallTimerCmd(flags *Flags) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "uninstall-timer --host <server>",
		Short: "Remove the trigger poll timer and service",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if flags.Host == "" {
				return fmt.Errorf("--host <server> is required")
			}
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
			defer cancel()
			host, user, key, err := config.ResolveServer(flags.Host, flags.Host, flags.User, flags.Key)
			if err != nil {
				return err
			}
			exec, err := ssh.Connect(ctx, ssh.ConnectConfig{Host: host, User: user, KeyPath: key})
			if err != nil {
				return err
			}
			defer exec.Close()
			sudo := sudoPrefixFor(ctx, exec)
			for _, u := range []string{triggerPollTimer, triggerPollService} {
				exec.Run(ctx, sudo+"systemctl disable --now "+ssh.ShellQuote(u)+" 2>/dev/null")
				if _, err := exec.Run(ctx, sudo+"rm -f /etc/systemd/system/"+u); err != nil {
					return err
				}
			}
			if _, err := exec.Run(ctx, sudo+"systemctl daemon-reload"); err != nil {
				return err
			}
			if asJSON {
				return encodeJSONLine(cmd.OutOrStdout(), map[string]any{"machine_interface": MachineInterface, "host": flags.Host, "removed": true})
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Removed %s and %s\n", triggerPollTimer, triggerPollService)
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", flags.JSON, "machine-readable output")
	return cmd
}

// ensureTriggerBinary makes sure a teploy binary that supports
// `trigger poll` exists on the server and returns its path: the released
// /deployments/.bin/teploy when it qualifies, a dev upload with --binary
// (installed beside it as teploy-dev), or a fresh release download.
func ensureTriggerBinary(ctx context.Context, exec ssh.Executor, localBinary string) (string, error) {
	// The publish step flocks <destination>.install.lock, so the
	// directory must exist first (a server that never ran autodeploy or
	// heal has no /deployments/.bin yet).
	if _, err := exec.Run(ctx, "mkdir -p "+ssh.ShellQuote(serverTeployBinDir)); err != nil {
		return "", fmt.Errorf("creating %s: %w", serverTeployBinDir, err)
	}
	if localBinary != "" {
		data, err := os.ReadFile(localBinary)
		if err != nil {
			return "", fmt.Errorf("reading %s: %w", localBinary, err)
		}
		dst := serverTeployBinDir + "/teploy-dev"
		if err := installServerBinary(ctx, exec, dst, data, teployVersionForServer(), "trigger poll"); err != nil {
			return "", err
		}
		return dst, nil
	}
	const released = serverTeployBinDir + "/teploy"
	if _, err := exec.Run(ctx, ssh.ShellQuote(released)+" trigger poll --help >/dev/null 2>&1"); err == nil {
		return released, nil
	}
	if _, err := deployTeployBinaryToServer(ctx, exec, released, "trigger poll"); err != nil {
		return "", fmt.Errorf("installing the server teploy binary: %w (install one manually at %s and re-run)", err, released)
	}
	return released, nil
}

// installTriggerUnits writes the service + timer and enables it.
func installTriggerUnits(ctx context.Context, exec ssh.Executor, bin string) error {
	sudo := sudoPrefixFor(ctx, exec)
	userLine := ""
	if out, err := exec.Run(ctx, "id -u"); err == nil && strings.TrimSpace(out) != "0" {
		if who, err := exec.Run(ctx, "id -un"); err == nil && strings.TrimSpace(who) != "" {
			userLine = "User=" + strings.TrimSpace(who) + "\n"
		}
	}
	service := "[Unit]\nDescription=Teploy git-source poll (admits and deploys new heads)\n\n[Service]\nType=oneshot\n" +
		userLine + "ExecStart=" + bin + " trigger poll --all\n"
	timer := "[Unit]\nDescription=Run the Teploy git-source poll every minute\n\n[Timer]\nOnBootSec=90\nOnUnitActiveSec=60\nAccuracySec=5s\nPersistent=true\n\n[Install]\nWantedBy=timers.target\n"
	if err := installSystemdUnit(ctx, exec, sudo, triggerPollService, service); err != nil {
		return err
	}
	if err := installSystemdUnit(ctx, exec, sudo, triggerPollTimer, timer); err != nil {
		return err
	}
	if _, err := exec.Run(ctx, sudo+"systemctl daemon-reload"); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w", err)
	}
	if _, err := exec.Run(ctx, sudo+"systemctl enable --now "+ssh.ShellQuote(triggerPollTimer)); err != nil {
		return fmt.Errorf("enabling %s: %w", triggerPollTimer, err)
	}
	return nil
}

// offerTriggerTimer best-effort installs the poll timer after a
// credential is created (the first thing a git-source app needs).
func offerTriggerTimer(ctx context.Context, t *configTarget, out io.Writer) {
	if _, err := t.exec.Run(ctx, "systemctl is-enabled "+ssh.ShellQuote(triggerPollTimer)+" >/dev/null 2>&1"); err == nil {
		return
	}
	bin, err := ensureTriggerBinary(ctx, t.exec, "")
	if err != nil {
		fmt.Fprintf(out, "Note: the server poll timer is not installed yet (%v)\n  install it with: teploy trigger install-timer --host %s\n", err, t.server)
		return
	}
	if err := installTriggerUnits(ctx, t.exec, bin); err != nil {
		fmt.Fprintf(out, "Note: could not install the server poll timer (%v)\n  install it with: teploy trigger install-timer --host %s\n", err, t.server)
		return
	}
	fmt.Fprintf(out, "Installed the server poll timer (%s)\n", triggerPollTimer)
}
