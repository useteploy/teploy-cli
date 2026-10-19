package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/confighome"
	"github.com/useteploy/teploy/internal/docker"
	"github.com/useteploy/teploy/internal/events"
	"github.com/useteploy/teploy/internal/releasemeta"
	"github.com/useteploy/teploy/internal/ssh"
	"github.com/useteploy/teploy/internal/state"
)

func newConfigCmd(flags *Flags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Edit an app's settings where they live (its folder, or Teploy on the server)",
		Long: `Every app's settings have one home:

  folder   teploy.yml in the operator's directory (the default; deploys run
           from that folder)
  teploy   a config repository on the app's server, edited by any client —
           this CLI on any machine, teploy-dash, CI — with history and
           compare-and-swap writes so nobody overwrites anybody

Move an app with 'teploy config home teploy' (from its folder) or back with
'teploy config home folder'. A Teploy-home app deploys with
'teploy deploy --app <app> --host <server>' and refuses folder deploys.

A Teploy-home teploy.yml holds no secrets and is never expanded from anyone's
environment: $VAR references are refused (write $$ for a literal $), and
secret values go in 'teploy secret set'.`,
	}
	cmd.AddCommand(newConfigGetCmd(flags), newConfigSetCmd(flags), newConfigEditCmd(flags),
		newConfigDiffCmd(flags), newConfigHistoryCmd(flags), newConfigRestoreCmd(flags),
		newConfigPullCmd(flags), newConfigPushCmd(flags), newConfigHomeCmd(flags),
		newConfigPurgeHistoryCmd(flags))
	return cmd
}

// configTarget is the app a config command acts on and its server.
type configTarget struct {
	app    string
	server string
	exec   ssh.Executor
}

// pulledHeaderRE matches the first line `teploy config pull` writes, which
// links a folder copy to the commit it came from.
var pulledHeaderRE = regexp.MustCompile(`^# teploy config: ([a-z0-9][a-z0-9-]*)@([0-9a-f]{40}) on (\S+)`)

type pulledHeader struct{ app, commit, server string }

func parsePulledHeader(data []byte) (pulledHeader, []byte) {
	line, rest, _ := bytes.Cut(data, []byte("\n"))
	m := pulledHeaderRE.FindSubmatch(line)
	if m == nil {
		return pulledHeader{}, data
	}
	return pulledHeader{app: string(m[1]), commit: string(m[2]), server: string(m[3])}, rest
}

func pulledHeaderLine(app, commit, server string) string {
	return fmt.Sprintf("# teploy config: %s@%s on %s — edit, then `teploy config push`\n", app, commit, server)
}

// openConfigTarget resolves the app (from --app, or ./teploy.yml) and its
// server (--host, the folder config's server:, or a pulled copy's header)
// and connects without printing, so `config get > file` stays clean.
func openConfigTarget(ctx context.Context, flags *Flags, appName string) (*configTarget, error) {
	server := flags.Host
	if raw, err := os.ReadFile("teploy.yml"); err == nil {
		h, body := parsePulledHeader(raw)
		if h.app != "" && (appName == "" || appName == h.app) {
			if appName == "" {
				appName = h.app
			}
			if server == "" {
				server = h.server
			}
		} else if cfg, lerr := config.LoadAppBytes(body); lerr == nil && (appName == "" || appName == cfg.App) {
			if appName == "" {
				appName = cfg.App
			}
			if server == "" {
				server = cfg.Server
				if server == "" && len(cfg.Servers) > 0 {
					server = cfg.Servers[0]
				}
			}
		}
	}
	if appName == "" {
		return nil, fmt.Errorf("no app — pass --app <name> --host <server>, or run in the app's folder")
	}
	if err := config.ValidateName(appName); err != nil {
		return nil, err
	}
	if server == "" {
		return nil, fmt.Errorf("no server for %s — pass --host <server>", appName)
	}
	host, user, key, err := config.ResolveServer(server, flags.Host, flags.User, flags.Key)
	if err != nil {
		return nil, err
	}
	ex, err := ssh.Connect(ctx, ssh.ConnectConfig{Host: host, User: user, KeyPath: key})
	if err != nil {
		return nil, err
	}
	return &configTarget{app: appName, server: server, exec: ex}, nil
}

func configAuthor() confighome.Author {
	a := events.ProcessActor()
	return confighome.Author{Name: events.ActorString(a), Email: a.Client + "@teploy"}
}

// configConflictError carries the full current commit so a client can
// merge against it without another read.
type configConflictError struct {
	app     string
	current string
}

var errConfigConflict = errors.New("config conflict")

func (e *configConflictError) Error() string {
	return fmt.Sprintf("%s's config changed since you read it (now at %s) — re-read it and retry, or use `teploy config edit` to merge", e.app, e.current)
}
func (e *configConflictError) Is(target error) bool { return target == errConfigConflict }

func wrapConfigWriteErr(app string, err error) error {
	var ce *confighome.ConflictError
	if errors.As(err, &ce) {
		return &configConflictError{app: app, current: ce.Current}
	}
	return err
}

// commitConfig validates next and writes it if the head is still expect,
// recording a config.set event (keys changed, privileged when the diff
// reaches the host). prev is the content at expect.
func commitConfig(ctx context.Context, t *configTarget, prev, next []byte, expect, message string) (string, error) {
	nextCfg, err := confighome.Load(next, t.app)
	if err != nil {
		return "", refuseAdmission(err)
	}
	var prevCfg *config.AppConfig
	if len(prev) > 0 {
		prevCfg, _ = config.LoadAppBytes(prev)
	}
	privileged := confighome.PrivilegedChange(prevCfg, nextCfg)
	commit, err := confighome.Write(ctx, t.exec, t.app, next, expect, configAuthor(), message)
	if err != nil {
		return "", wrapConfigWriteErr(t.app, err)
	}
	detail := map[string]any{"keys": confighome.ChangedKeys(prev, next)}
	if privileged {
		detail["privileged"] = true
	}
	if expect != "" && expect != confighome.ZeroCommit {
		detail["previous_commit"] = expect
	}
	events.Record(ctx, t.exec, events.Event{App: t.app, Kind: events.KindConfigSet, Outcome: events.Succeeded, ConfigCommit: commit, Detail: detail})
	return commit, nil
}

// readHead returns the config at the branch head, or a folder-home error.
func readHead(ctx context.Context, t *configTarget) ([]byte, string, error) {
	exists, head, err := confighome.Head(ctx, t.exec, t.app)
	if err != nil {
		return nil, "", err
	}
	if !exists {
		return nil, "", fmt.Errorf("%s's settings are edited in its folder, not in Teploy (move them with `teploy config home teploy`)", t.app)
	}
	if head == "" {
		return nil, "", nil
	}
	return confighome.Read(ctx, t.exec, t.app, head)
}

// appliedConfigCommit is the config commit of the running release ("" when
// it was not deployed from Teploy config, or nothing runs).
func appliedConfigCommit(ctx context.Context, exec ssh.Executor, app string) string {
	st, err := state.Read(ctx, exec, app)
	if err != nil || st == nil || st.CurrentHash == "" {
		return ""
	}
	rec, err := releasemeta.Read(ctx, exec, app, st.CurrentHash)
	if err != nil || rec == nil {
		return ""
	}
	return rec.ConfigCommit
}

// configDTO is `config get --json` / `config home --json`.
type configDTO struct {
	MachineInterface int    `json:"machine_interface"`
	Host             string `json:"host"`
	App              string `json:"app"`
	Home             string `json:"home"`
	Commit           string `json:"commit,omitempty"`
	AppliedCommit    string `json:"applied_commit,omitempty"`
	Content          string `json:"content,omitempty"`
}

func newConfigGetCmd(flags *Flags) *cobra.Command {
	var appName, at string
	cmd := &cobra.Command{
		Use:     "get",
		Aliases: []string{"show"},
		Short:   "Print the app's Teploy config (teploy.yml) from its server",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
			defer cancel()
			t, err := openConfigTarget(ctx, flags, appName)
			if err != nil {
				return err
			}
			defer t.exec.Close()
			exists, head, err := confighome.Head(ctx, t.exec, t.app)
			if err != nil {
				return err
			}
			dto := configDTO{MachineInterface: MachineInterface, Host: t.exec.Host(), App: t.app, Home: confighome.HomeFolder}
			if exists {
				dto.Home, dto.Commit = confighome.HomeTeploy, head
				dto.AppliedCommit = appliedConfigCommit(ctx, t.exec, t.app)
				rev := at
				if rev == "" {
					rev = head
				}
				if rev != "" {
					content, commit, err := confighome.Read(ctx, t.exec, t.app, rev)
					if err != nil {
						return err
					}
					dto.Content, dto.Commit = string(content), commit
				}
			}
			if flags.JSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(dto)
			}
			if !exists {
				return fmt.Errorf("%s's settings are edited in its folder, not in Teploy", t.app)
			}
			fmt.Fprint(cmd.OutOrStdout(), dto.Content)
			return nil
		},
	}
	cmd.Flags().StringVar(&appName, "app", "", "app name (default: the app in ./teploy.yml)")
	cmd.Flags().StringVar(&at, "at", "", "print the config at this commit instead of the head")
	return cmd
}

func newConfigSetCmd(flags *Flags) *cobra.Command {
	var appName, expect, file string
	var unset []string
	cmd := &cobra.Command{
		Use:   "set [key=value ...]",
		Short: "Change keys of the app's Teploy config, or replace it with --file",
		Long: `Each key=value sets a dotted key (value is YAML: image=nginx:1.27,
port=8080, env.LOG_LEVEL=debug); --unset removes one. The write is a
compare-and-swap: with --expect <commit> it fails if the config moved since
that commit, otherwise key edits apply to the current head.

--file <path|-> replaces the whole teploy.yml and requires --expect (the
commit you started from; use 0000000000000000000000000000000000000000 for an
app with no config yet).

A conflict exits 1 with the current commit in the error (code "conflict"
under --json).`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if file == "" && len(args) == 0 && len(unset) == 0 {
				return fmt.Errorf("nothing to set — pass key=value, --unset key, or --file")
			}
			if file != "" && (len(args) > 0 || len(unset) > 0) {
				return fmt.Errorf("--file replaces the whole config; it cannot be combined with key edits")
			}
			if file != "" && expect == "" {
				return fmt.Errorf("--file needs --expect <commit> (the commit you edited), so a concurrent change is never overwritten")
			}
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
			defer cancel()
			t, err := openConfigTarget(ctx, flags, appName)
			if err != nil {
				return err
			}
			defer t.exec.Close()

			var commit string
			if file != "" {
				next, err := readConfigFile(file, cmd.InOrStdin())
				if err != nil {
					return err
				}
				var prev []byte
				if expect != confighome.ZeroCommit {
					prev, _, _ = confighome.Read(ctx, t.exec, t.app, expect)
				}
				commit, err = commitConfig(ctx, t, prev, next, expect, "config set (whole file)")
				if err != nil {
					return err
				}
			} else {
				commit, err = setConfigKeys(ctx, t, args, unset, expect)
				if err != nil {
					return err
				}
			}
			if flags.JSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(configDTO{MachineInterface: MachineInterface, Host: t.exec.Host(), App: t.app, Home: confighome.HomeTeploy, Commit: commit})
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Saved %s config %s (deploy it with `teploy deploy --app %s --host %s`)\n", t.app, commit[:12], t.app, t.server)
			return nil
		},
	}
	cmd.Flags().StringVar(&appName, "app", "", "app name (default: the app in ./teploy.yml)")
	cmd.Flags().StringVar(&expect, "expect", "", "fail unless the config is still at this commit")
	cmd.Flags().StringVar(&file, "file", "", "replace teploy.yml with this file (- for stdin)")
	cmd.Flags().StringSliceVar(&unset, "unset", nil, "remove this dotted key (repeatable)")
	return cmd
}

func readConfigFile(path string, stdin io.Reader) ([]byte, error) {
	var data []byte
	var err error
	if path == "-" {
		data, err = io.ReadAll(io.LimitReader(stdin, confighome.MaxConfigBytes+1))
	} else {
		data, err = os.ReadFile(path)
	}
	if err != nil {
		return nil, err
	}
	_, body := parsePulledHeader(data)
	return body, nil
}

// setConfigKeys applies key edits to the head. Without --expect a
// conflict re-reads and re-applies (key edits rebase cleanly); with it the
// conflict is returned.
func setConfigKeys(ctx context.Context, t *configTarget, sets, unset []string, expect string) (string, error) {
	for attempt := 0; ; attempt++ {
		prev, head, err := readHead(ctx, t)
		if err != nil {
			return "", err
		}
		if expect != "" && !strings.HasPrefix(head, expect) {
			return "", &configConflictError{app: t.app, current: head}
		}
		next := prev
		for _, kv := range sets {
			k, v, ok := strings.Cut(kv, "=")
			if !ok || k == "" {
				return "", fmt.Errorf("%q is not key=value", kv)
			}
			if next, err = confighome.EditPath(next, k, &v); err != nil {
				return "", err
			}
		}
		for _, k := range unset {
			if next, err = confighome.EditPath(next, k, nil); err != nil {
				return "", err
			}
		}
		commit, err := commitConfig(ctx, t, prev, next, head, "config set "+strings.Join(append(append([]string{}, keysOf(sets)...), unsetLabels(unset)...), " "))
		if errors.Is(err, errConfigConflict) && expect == "" && attempt < 3 {
			continue
		}
		return commit, err
	}
}

func keysOf(kvs []string) []string {
	out := make([]string, 0, len(kvs))
	for _, kv := range kvs {
		k, _, _ := strings.Cut(kv, "=")
		out = append(out, k)
	}
	return out
}

func unsetLabels(keys []string) []string {
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, "-"+k)
	}
	return out
}

func newConfigEditCmd(flags *Flags) *cobra.Command {
	var appName string
	cmd := &cobra.Command{
		Use:   "edit",
		Short: "Edit the app's Teploy config in $EDITOR (merges concurrent changes)",
		Long: `Opens the config in $VISUAL / $EDITOR (default vi). On save it is validated
and written as a compare-and-swap against the commit you opened. If someone
else saved in the meantime, the two edits are merged key by key; you are
asked only about keys you both changed.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !stdinIsTerminal() {
				return fmt.Errorf("config edit needs a terminal — use `teploy config set` or `teploy config set --file - --expect <commit>`")
			}
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
			defer cancel()
			t, err := openConfigTarget(ctx, flags, appName)
			if err != nil {
				return err
			}
			defer t.exec.Close()
			return runConfigEdit(ctx, t, bufio.NewReader(os.Stdin), cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&appName, "app", "", "app name (default: the app in ./teploy.yml)")
	return cmd
}

func runConfigEdit(ctx context.Context, t *configTarget, in *bufio.Reader, out io.Writer) error {
	base, baseCommit, err := readHead(ctx, t)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp("", "teploy-"+t.app+"-*.yml")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(base); err != nil {
		return err
	}
	tmp.Close()

	for {
		if err := runEditor(tmp.Name()); err != nil {
			return err
		}
		mine, err := os.ReadFile(tmp.Name())
		if err != nil {
			return err
		}
		if bytes.Equal(mine, base) {
			fmt.Fprintln(out, "No changes")
			return nil
		}
		if _, err := confighome.Load(mine, t.app); err != nil {
			fmt.Fprintln(out, err)
			if !confirm(out, "Edit again?") {
				return fmt.Errorf("config not saved")
			}
			continue
		}
		commit, err := commitConfig(ctx, t, base, mine, baseCommit, "config edit")
		if errors.Is(err, errConfigConflict) {
			merged, current, err := mergeConcurrentEdit(ctx, t, base, mine, in, out)
			if err != nil {
				return err
			}
			theirs, _, _ := confighome.Read(ctx, t.exec, t.app, current)
			fmt.Fprint(out, confighome.Diff("current", "merged", theirs, merged))
			if !confirm(out, "Save the merged config?") {
				return fmt.Errorf("config not saved")
			}
			commit, err = commitConfig(ctx, t, theirs, merged, current, "config edit (merged)")
			if err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		fmt.Fprintf(out, "Saved %s config %s (deploy it with `teploy deploy --app %s --host %s`)\n", t.app, commit[:12], t.app, t.server)
		return nil
	}
}

// mergeConcurrentEdit three-way merges the operator's edit with the head
// that moved under it, asking only about keys both changed.
func mergeConcurrentEdit(ctx context.Context, t *configTarget, base, mine []byte, in *bufio.Reader, out io.Writer) ([]byte, string, error) {
	theirs, current, err := readHead(ctx, t)
	if err != nil {
		return nil, "", err
	}
	fmt.Fprintf(out, "Someone saved %s's config while you were editing (now %s); merging.\n", t.app, current[:12])
	merged, conflicts, err := confighome.Merge3(base, mine, theirs)
	if err != nil {
		return nil, "", err
	}
	for _, path := range conflicts {
		mv, mok := confighome.GetPath(mine, path)
		tv, tok := confighome.GetPath(theirs, path)
		fmt.Fprintf(out, "\n%s changed on both sides:\n  yours:   %s\n  current: %s\n", path, renderValue(mv, mok), renderValue(tv, tok))
		fmt.Fprint(out, "Keep [y]ours or [c]urrent? ")
		line, _ := in.ReadString('\n')
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(line)), "c") {
			if merged, err = confighome.SetPath(merged, path, tv, !tok); err != nil {
				return nil, "", err
			}
		}
	}
	if _, err := confighome.Load(merged, t.app); err != nil {
		return nil, "", fmt.Errorf("the merged config is invalid: %w", err)
	}
	return merged, current, nil
}

func renderValue(v any, ok bool) string {
	if !ok {
		return "(removed)"
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

func runEditor(path string) error {
	editor := os.Getenv("VISUAL")
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}
	if editor == "" {
		editor = "vi"
	}
	c := exec.Command("sh", "-c", editor+` "$1"`, "sh", path)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := c.Run(); err != nil {
		return fmt.Errorf("editor %q: %w", editor, err)
	}
	return nil
}

func newConfigDiffCmd(flags *Flags) *cobra.Command {
	var appName, file string
	var applied bool
	cmd := &cobra.Command{
		Use:   "diff",
		Short: "Diff the app's Teploy config against a local file, or against what is deployed",
		Long: `Without flags, compares ./teploy.yml (or --file) with the config on the server.
With --applied, compares the config of the running release with the head —
what the next deploy would change.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
			defer cancel()
			t, err := openConfigTarget(ctx, flags, appName)
			if err != nil {
				return err
			}
			defer t.exec.Close()
			head, commit, err := readHead(ctx, t)
			if err != nil {
				return err
			}
			var d string
			if applied {
				ac := appliedConfigCommit(ctx, t.exec, t.app)
				if ac == "" {
					fmt.Fprintf(cmd.OutOrStdout(), "The running release of %s was not deployed from its Teploy config; the next deploy applies config %s.\n", t.app, short12(commit))
					return nil
				}
				old, _, err := confighome.Read(ctx, t.exec, t.app, ac)
				if err != nil {
					return err
				}
				d = confighome.Diff("deployed "+short12(ac), "config "+short12(commit), old, head)
			} else {
				if file == "" {
					file = "teploy.yml"
				}
				local, err := readConfigFile(file, cmd.InOrStdin())
				if err != nil {
					return err
				}
				d = confighome.Diff("config "+short12(commit), file, head, local)
			}
			if d == "" {
				fmt.Fprintln(cmd.OutOrStdout(), "No differences")
				return nil
			}
			fmt.Fprint(cmd.OutOrStdout(), d)
			return nil
		},
	}
	cmd.Flags().StringVar(&appName, "app", "", "app name (default: the app in ./teploy.yml)")
	cmd.Flags().StringVar(&file, "file", "", "local file to compare (default ./teploy.yml)")
	cmd.Flags().BoolVar(&applied, "applied", false, "compare the deployed config with the head")
	return cmd
}

func short12(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

type configHistoryDTO struct {
	MachineInterface int                 `json:"machine_interface"`
	Host             string              `json:"host"`
	App              string              `json:"app"`
	AppliedCommit    string              `json:"applied_commit,omitempty"`
	Commits          []confighome.Commit `json:"commits"`
}

func newConfigHistoryCmd(flags *Flags) *cobra.Command {
	var appName string
	var n int
	cmd := &cobra.Command{
		Use:   "history",
		Short: "List the app's config commits, newest first",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
			defer cancel()
			t, err := openConfigTarget(ctx, flags, appName)
			if err != nil {
				return err
			}
			defer t.exec.Close()
			commits, err := confighome.History(ctx, t.exec, t.app, n)
			if errors.Is(err, confighome.ErrAbsent) {
				return fmt.Errorf("%s's settings are edited in its folder, not in Teploy", t.app)
			}
			if err != nil {
				return err
			}
			if commits == nil {
				commits = []confighome.Commit{}
			}
			applied := appliedConfigCommit(ctx, t.exec, t.app)
			if flags.JSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(configHistoryDTO{MachineInterface: MachineInterface, Host: t.exec.Host(), App: t.app, AppliedCommit: applied, Commits: commits})
			}
			for _, c := range commits {
				mark := " "
				if c.OID == applied {
					mark = "*"
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s %s  %s  %-28s %s\n", mark, c.OID[:12], c.Date.Local().Format(time.DateTime), c.Author, c.Message)
			}
			if applied != "" {
				fmt.Fprintln(cmd.OutOrStdout(), "(* = deployed)")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&appName, "app", "", "app name (default: the app in ./teploy.yml)")
	cmd.Flags().IntVarP(&n, "number", "n", 50, "how many commits")
	return cmd
}

func newConfigRestoreCmd(flags *Flags) *cobra.Command {
	var appName string
	cmd := &cobra.Command{
		Use:   "restore <commit>",
		Short: "Make an earlier config commit the current config (as a new commit)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
			defer cancel()
			t, err := openConfigTarget(ctx, flags, appName)
			if err != nil {
				return err
			}
			defer t.exec.Close()
			cur, head, err := readHead(ctx, t)
			if err != nil {
				return err
			}
			old, oid, err := confighome.Read(ctx, t.exec, t.app, args[0])
			if err != nil {
				return err
			}
			commit, err := commitConfig(ctx, t, cur, old, head, "restore "+short12(oid))
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Restored %s config from %s as %s (deploy it to apply)\n", t.app, short12(oid), short12(commit))
			return nil
		},
	}
	cmd.Flags().StringVar(&appName, "app", "", "app name (default: the app in ./teploy.yml)")
	return cmd
}

func newConfigPullCmd(flags *Flags) *cobra.Command {
	var appName, file string
	var force bool
	cmd := &cobra.Command{
		Use:   "pull",
		Short: "Write the app's Teploy config to a local file to edit (then `config push`)",
		Long: `Writes the config to ./teploy.yml (or --file) with a first-line comment naming
the app, commit and server. 'teploy config push' sends it back as a
compare-and-swap against that commit. The copy is not a folder home: the app
still deploys from its server config.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if file == "" {
				file = "teploy.yml"
			}
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
			defer cancel()
			t, err := openConfigTarget(ctx, flags, appName)
			if err != nil {
				return err
			}
			defer t.exec.Close()
			content, commit, err := readHead(ctx, t)
			if err != nil {
				return err
			}
			if commit == "" {
				return fmt.Errorf("%s has no config commit yet", t.app)
			}
			if existing, err := os.ReadFile(file); err == nil && !force {
				if h, _ := parsePulledHeader(existing); h.app != t.app {
					return fmt.Errorf("%s exists and is not a pulled copy of %s's config — pass --force to overwrite, or --file to write elsewhere", file, t.app)
				}
			}
			data := append([]byte(pulledHeaderLine(t.app, commit, t.server)), content...)
			if err := os.WriteFile(file, data, 0o600); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Wrote %s (%s config %s)\n", file, t.app, short12(commit))
			return nil
		},
	}
	cmd.Flags().StringVar(&appName, "app", "", "app name (default: the app in ./teploy.yml)")
	cmd.Flags().StringVar(&file, "file", "", "write here instead of ./teploy.yml")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite a file that is not a pulled copy")
	return cmd
}

func newConfigPushCmd(flags *Flags) *cobra.Command {
	var appName, file, expect string
	cmd := &cobra.Command{
		Use:   "push",
		Short: "Send a pulled teploy.yml back to the server (compare-and-swap)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if file == "" {
				file = "teploy.yml"
			}
			raw, err := os.ReadFile(file)
			if err != nil {
				return err
			}
			h, body := parsePulledHeader(raw)
			if expect == "" {
				expect = h.commit
			}
			if expect == "" {
				return fmt.Errorf("%s is not a pulled copy (no `# teploy config:` first line) — run `teploy config pull` first, pass --expect <commit>, or move a folder app with `teploy config home teploy`", file)
			}
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
			defer cancel()
			t, err := openConfigTarget(ctx, flags, appName)
			if err != nil {
				return err
			}
			defer t.exec.Close()
			prev, _, _ := confighome.Read(ctx, t.exec, t.app, expect)
			if bytes.Equal(prev, body) {
				fmt.Fprintln(cmd.OutOrStdout(), "No changes")
				return nil
			}
			commit, err := commitConfig(ctx, t, prev, body, expect, "config push")
			if err != nil {
				if errors.Is(err, errConfigConflict) {
					return fmt.Errorf("%w\n(your file is unchanged; merge interactively with `teploy config edit`, or re-pull)", err)
				}
				return err
			}
			if err := os.WriteFile(file, append([]byte(pulledHeaderLine(t.app, commit, t.server)), body...), 0o600); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Saved %s config %s (deploy it with `teploy deploy --app %s --host %s`)\n", t.app, short12(commit), t.app, t.server)
			return nil
		},
	}
	cmd.Flags().StringVar(&appName, "app", "", "app name (default: from the file)")
	cmd.Flags().StringVar(&file, "file", "", "file to push (default ./teploy.yml)")
	cmd.Flags().StringVar(&expect, "expect", "", "commit the file was edited from (default: the file's header)")
	return cmd
}

func newConfigHomeCmd(flags *Flags) *cobra.Command {
	var appName, file string
	cmd := &cobra.Command{
		Use:   "home [folder|teploy]",
		Short: "Show or change where the app's settings are edited",
		Long: `Without an argument, prints the home.

  teploy config home teploy    run in the app's folder: validates teploy.yml
                               for Teploy (no $VAR, no env_files, no secret
                               values; server:/servers: are dropped — the app
                               lives on that server), stores it on the server
                               and marks the app, so folder deploys and older
                               teploy CLIs refuse it from now on.
  teploy config home folder    archives the server config (kept as
                               config.git.archived-<time>) and makes the app a
                               folder app again; writes ./teploy.yml from the
                               config when there is none.

Changing the home does not redeploy.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
			defer cancel()
			if len(args) == 0 {
				t, err := openConfigTarget(ctx, flags, appName)
				if err != nil {
					return err
				}
				defer t.exec.Close()
				exists, head, err := confighome.Head(ctx, t.exec, t.app)
				if err != nil {
					return err
				}
				dto := configDTO{MachineInterface: MachineInterface, Host: t.exec.Host(), App: t.app, Home: confighome.HomeFolder}
				if exists {
					dto.Home, dto.Commit = confighome.HomeTeploy, head
					dto.AppliedCommit = appliedConfigCommit(ctx, t.exec, t.app)
				}
				if flags.JSON {
					return json.NewEncoder(cmd.OutOrStdout()).Encode(dto)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s: settings edited in %s\n", t.app, dto.Home)
				return nil
			}
			switch args[0] {
			case confighome.HomeTeploy:
				return runConfigHomeTeploy(ctx, flags, appName, file, cmd.OutOrStdout())
			case confighome.HomeFolder:
				return runConfigHomeFolder(ctx, flags, appName, file, cmd.OutOrStdout())
			case confighome.HomeRepo:
				return fmt.Errorf("repo home is not available yet in this teploy version")
			}
			return fmt.Errorf("unknown home %q (folder or teploy)", args[0])
		},
	}
	cmd.Flags().StringVar(&appName, "app", "", "app name (default: the app in ./teploy.yml)")
	cmd.Flags().StringVar(&file, "file", "", "folder config to move (home teploy) or to write (home folder); default ./teploy.yml")
	return cmd
}

// lockConfigTarget takes the app's fenced lock for a home change.
func lockConfigTarget(ctx context.Context, t *configTarget) (*state.Lock, func(), error) {
	if err := docker.NewClient(t.exec).EnsureManagedDirectory(ctx, t.app, "", ""); err != nil {
		return nil, nil, fmt.Errorf("creating app directory: %w", err)
	}
	lk, err := state.AcquireLockFenced(ctx, t.exec, t.app)
	if err != nil {
		return nil, nil, err
	}
	lk.StartRenewal(t.exec)
	return lk, func() { state.ReleaseLockFenced(t.exec, lk, t.app) }, nil
}

func runConfigHomeTeploy(ctx context.Context, flags *Flags, appName, file string, out io.Writer) error {
	if file == "" {
		file = "teploy.yml"
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return fmt.Errorf("reading %s: %w (run this in the app's folder, or pass --file)", file, err)
	}
	if h, _ := parsePulledHeader(raw); h.app != "" {
		return fmt.Errorf("%s is a pulled copy of %s's Teploy config — it is already in Teploy", file, h.app)
	}
	folderCfg, err := config.LoadAppBytes(raw)
	if err != nil {
		return err
	}
	if appName != "" && appName != folderCfg.App {
		return fmt.Errorf("%s is the config of %q, not %q", file, folderCfg.App, appName)
	}
	if flags.Host == "" && folderCfg.Server == "" && len(folderCfg.Servers) != 1 {
		if len(folderCfg.Servers) > 1 {
			return fmt.Errorf("%s deploys to %d servers; Teploy home is single-server — pass --host for the one server that will hold it", file, len(folderCfg.Servers))
		}
		return fmt.Errorf("no server in %s — pass --host", file)
	}
	content := raw
	for _, k := range []string{"server", "servers"} {
		if content, err = confighome.EditPath(content, k, nil); err != nil {
			return err
		}
	}
	if _, err := confighome.Load(content, folderCfg.App); err != nil {
		return refuseAdmission(fmt.Errorf("%w\nFix these in %s and run `teploy config home teploy` again", err, file))
	}
	warnDollarEscapes(out, content, "a folder deploy turned $$ into an empty string; in Teploy it is a literal $")

	t, err := openConfigTarget(ctx, flags, folderCfg.App)
	if err != nil {
		return err
	}
	defer t.exec.Close()
	lk, release, err := lockConfigTarget(ctx, t)
	if err != nil {
		return err
	}
	defer release()
	gate, err := confighome.CheckGate(ctx, t.exec, t.app, lk, out)
	if err != nil {
		return err
	}
	if gate.Server {
		return fmt.Errorf("%s's settings are already edited in Teploy (config %s)", t.app, short12(gate.Head))
	}
	commit, err := commitConfig(ctx, t, nil, content, confighome.ZeroCommit, "config home teploy (from "+file+")")
	if err != nil {
		return err
	}
	// The repository exists first, then the state marker: a crash between
	// leaves config.git + v2, which the next CLI repairs (CheckGate).
	if err := state.SetSchemaVersion(ctx, t.exec, t.app, state.SchemaVersionV3, lk); err != nil {
		return fmt.Errorf("marking %s as edited in Teploy: %w", t.app, err)
	}
	events.Record(ctx, t.exec, events.Event{App: t.app, Kind: events.KindConfigHome, Outcome: events.Succeeded, ConfigCommit: commit, Detail: map[string]any{"from": confighome.HomeFolder, "to": confighome.HomeTeploy}})
	fmt.Fprintf(out, "%s's settings are now edited in Teploy (config %s on %s).\n", t.app, short12(commit), t.server)
	fmt.Fprintf(out, "  deploy:  teploy deploy --app %s --host %s\n", t.app, t.server)
	fmt.Fprintf(out, "  edit:    teploy config edit --app %s --host %s\n", t.app, t.server)
	fmt.Fprintf(out, "%s here is no longer used to deploy; `teploy config pull --force` replaces it with a linked copy.\n", file)
	return nil
}

func runConfigHomeFolder(ctx context.Context, flags *Flags, appName, file string, out io.Writer) error {
	if file == "" {
		file = "teploy.yml"
	}
	t, err := openConfigTarget(ctx, flags, appName)
	if err != nil {
		return err
	}
	defer t.exec.Close()
	lk, release, err := lockConfigTarget(ctx, t)
	if err != nil {
		return err
	}
	defer release()
	exists, head, err := confighome.Head(ctx, t.exec, t.app)
	if err != nil {
		return err
	}
	if exists && head != "" {
		content, _, err := confighome.Read(ctx, t.exec, t.app, head)
		if err != nil {
			return err
		}
		existing, readErr := os.ReadFile(file)
		h, _ := parsePulledHeader(existing)
		if errors.Is(readErr, os.ErrNotExist) || h.app == t.app {
			// A folder config names its server.
			folder := append([]byte("server: "+t.server+"\n"), content...)
			warnDollarEscapes(out, content, "a folder deploy expands $$ to an empty string")
			if err := os.WriteFile(file, folder, 0o600); err != nil {
				return err
			}
			fmt.Fprintf(out, "Wrote %s from config %s\n", file, short12(head))
		} else {
			fmt.Fprintf(out, "Kept the existing %s (the Teploy config was %s; see it with `teploy config get --at %s` before this, or in the archive)\n", file, short12(head), short12(head))
		}
	}
	if exists {
		dst, err := confighome.Archive(ctx, t.exec, t.app)
		if err != nil && !errors.Is(err, confighome.ErrAbsent) {
			return err
		}
		if dst != "" {
			fmt.Fprintf(out, "Archived the server config to %s\n", dst)
		}
	}
	if err := state.SetSchemaVersion(ctx, t.exec, t.app, state.SchemaVersionV2, lk); err != nil {
		return fmt.Errorf("marking %s as a folder app: %w", t.app, err)
	}
	if !exists {
		fmt.Fprintf(out, "%s's settings are edited in its folder\n", t.app)
		return nil
	}
	events.Record(ctx, t.exec, events.Event{App: t.app, Kind: events.KindConfigHome, Outcome: events.Succeeded, ConfigCommit: head, Detail: map[string]any{"from": confighome.HomeTeploy, "to": confighome.HomeFolder}})
	fmt.Fprintf(out, "%s's settings are now edited in its folder; deploy with `teploy deploy` from there.\n", t.app)
	return nil
}

func newConfigPurgeHistoryCmd(flags *Flags) *cobra.Command {
	var appName, expect string
	cmd := &cobra.Command{
		Use:   "purge-history",
		Short: "Drop every earlier config commit (for a value committed by accident)",
		Long: `Replaces the app's config history with one commit holding the current
config and deletes the old objects from the server. Remove the value with
'teploy config set --unset' first, then purge. --expect must name the
current head. Irreversible.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if expect == "" {
				return fmt.Errorf("--expect <current commit> is required (see `teploy config history`)")
			}
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
			defer cancel()
			t, err := openConfigTarget(ctx, flags, appName)
			if err != nil {
				return err
			}
			defer t.exec.Close()
			commit, err := confighome.PurgeHistory(ctx, t.exec, t.app, expect, configAuthor())
			if err != nil {
				return wrapConfigWriteErr(t.app, err)
			}
			events.Record(ctx, t.exec, events.Event{App: t.app, Kind: events.KindConfigSet, Outcome: events.Succeeded, ConfigCommit: commit, Detail: map[string]any{"keys": []string{}, "previous_commit": expect}})
			fmt.Fprintf(cmd.OutOrStdout(), "Purged %s's config history; the config is now %s\n", t.app, short12(commit))
			return nil
		},
	}
	cmd.Flags().StringVar(&appName, "app", "", "app name (default: the app in ./teploy.yml)")
	cmd.Flags().StringVar(&expect, "expect", "", "the current head commit")
	return cmd
}

// warnDollarEscapes flags env: values containing $$, whose meaning differs
// between the homes.
func warnDollarEscapes(out io.Writer, content []byte, why string) {
	cfg, err := config.LoadAppBytes(content)
	if err != nil {
		return
	}
	for _, k := range sortedKeys(cfg.Env) {
		if strings.Contains(cfg.Env[k], "$$") {
			fmt.Fprintf(out, "Warning: env.%s contains $$ — %s; check the value\n", k, why)
		}
	}
}
