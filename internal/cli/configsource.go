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
	"github.com/useteploy/teploy/internal/events"
	"github.com/useteploy/teploy/internal/gitsource"
	"github.com/useteploy/teploy/internal/secret"
	"github.com/useteploy/teploy/internal/ssh"
)

// `teploy config source` — the D4 git-source connection surface: per-app
// credentials stored age-encrypted OUTSIDE /deployments, the forge host
// key pin, and the public source/admission state.

func newConfigSourceCmd(flags *Flags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "source",
		Short: "Manage an app's git source credential and host key",
		Long: `A Teploy-home app with a git: block builds from a git repository —
GitHub, GitLab, Forgejo/Gitea, Cursor Origin, or any git host over SSH or
HTTPS. The server fetches it with a per-app credential that never leaves
the server:

  teploy config source key --app X --host S            generate an ed25519 deploy key
                                                       (print the public half to register)
  teploy config source key --app X --host S --type token
                                                       store an HTTPS token read from stdin
  teploy config source host-key --app X --host S       pin the forge's SSH host key
  teploy config source show --app X --host S --json    public state, no secrets
  teploy config source remove --app X --host S         forget the credential

Credentials live age-encrypted outside /deployments (/etc/teploy/apps as
root), so a compromised Caddy cannot read them. HTTPS tokens are used via
GIT_ASKPASS and SSH keys via GIT_SSH_COMMAND — never in URLs, .git/config
or a command line. Fetches use StrictHostKeyChecking=yes against the
pinned host key.`,
	}
	cmd.AddCommand(newConfigSourceKeyCmd(flags), newConfigSourceHostKeyCmd(flags),
		newConfigSourceShowCmd(flags), newConfigSourceRemoveCmd(flags))
	return cmd
}

// sourceCredentialDTO is the public half of a credential (never a secret).
type sourceCredentialDTO struct {
	Type        string   `json:"type"`
	PublicKey   string   `json:"public_key,omitempty"`
	Fingerprint string   `json:"fingerprint,omitempty"`
	Username    string   `json:"username,omitempty"`
	KnownHosts  []string `json:"known_host_fingerprints,omitempty"`
}

// sourceTriggerDTO is one ref's admission state (the dash repository card).
type sourceTriggerDTO struct {
	Ref                 string `json:"ref"`
	Watermark           string `json:"watermark,omitempty"`
	LastHead            string `json:"last_head,omitempty"`
	RefusedHead         string `json:"refused_head,omitempty"`
	LastDeployedRelease string `json:"last_deployed_release,omitempty"`
}

// sourceShowDTO is `config source show --json`.
type sourceShowDTO struct {
	MachineInterface int                  `json:"machine_interface"`
	Host             string               `json:"host"`
	App              string               `json:"app"`
	Repository       string               `json:"repository,omitempty"`
	Branch           string               `json:"branch,omitempty"`
	Poll             string               `json:"poll,omitempty"`
	Credential       *sourceCredentialDTO `json:"credential,omitempty"`
	Trigger          *sourceTriggerDTO    `json:"trigger,omitempty"`
}

// sourceKeyDTO is `config source key --json` / `host-key --json`.
type sourceKeyDTO struct {
	MachineInterface int      `json:"machine_interface"`
	Host             string   `json:"host"`
	App              string   `json:"app"`
	Type             string   `json:"type"`
	PublicKey        string   `json:"public_key,omitempty"`
	Fingerprint      string   `json:"fingerprint,omitempty"`
	Username         string   `json:"username,omitempty"`
	Created          bool     `json:"created"`
	HostKeys         []string `json:"host_key_fingerprints,omitempty"`
}

func newConfigSourceKeyCmd(flags *Flags) *cobra.Command {
	var (
		appName, keyType, username string
		rotate, noTimer, asJSON    bool
	)
	cmd := &cobra.Command{
		Use:   "key",
		Short: "Generate the app's deploy key (or store an HTTPS token from stdin)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if appName == "" {
				return fmt.Errorf("--app is required")
			}
			if keyType != "ssh" && keyType != "token" {
				return fmt.Errorf("--type must be ssh or token")
			}
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
			defer cancel()
			t, err := openConfigTarget(ctx, flags, appName)
			if err != nil {
				return err
			}
			defer t.exec.Close()
			out := cmd.OutOrStdout()

			var info gitsource.KeyInfo
			if keyType == "token" {
				token, err := readTokenStdin(cmd.InOrStdin())
				if err != nil {
					return err
				}
				if err := secret.NewManager(t.exec).EnsureAge(ctx); err != nil {
					return err
				}
				if info, err = gitsource.SetToken(ctx, t.exec, t.app, username, token); err != nil {
					return err
				}
			} else {
				if err := secret.NewManager(t.exec).EnsureAge(ctx); err != nil {
					return err
				}
				if info, err = gitsource.GenerateKey(ctx, t.exec, t.app, rotate); err != nil {
					return err
				}
			}
			events.Record(ctx, t.exec, events.Event{App: t.app, Kind: events.KindSourceConnect, Outcome: events.Succeeded,
				Detail: map[string]any{"action": "connect", "type": info.Type, "fingerprint": info.Fingerprint}})

			if asJSON {
				return json.NewEncoder(out).Encode(sourceKeyDTO{
					MachineInterface: MachineInterface, Host: t.server, App: t.app,
					Type: info.Type, PublicKey: info.PublicKey, Fingerprint: info.Fingerprint,
					Username: info.Username, Created: info.Created,
				})
			}
			switch info.Type {
			case "ssh":
				fmt.Fprintf(out, "Deploy key for %s (%s)\n  %s\n", t.app, info.Fingerprint, info.PublicKey)
				fmt.Fprintf(out, "Add it as a read-only deploy key to the repository, then pin the forge host key:\n")
				fmt.Fprintf(out, "  teploy config source host-key --app %s --host %s\n", t.app, t.server)
				if !info.Created {
					fmt.Fprintf(out, "(key already existed; --rotate replaces it)\n")
				}
			case "token":
				fmt.Fprintf(out, "Token stored for %s (username %s)\n", t.app, info.Username)
			}
			if !noTimer {
				offerTriggerTimer(ctx, t, out)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&appName, "app", "", "app the credential belongs to (required)")
	cmd.Flags().StringVar(&keyType, "type", "ssh", "credential type: ssh (deploy key) or token (HTTPS, read from stdin)")
	cmd.Flags().StringVar(&username, "username", "", "username sent with the token (default x-access-token)")
	cmd.Flags().BoolVar(&rotate, "rotate", false, "replace an existing SSH key")
	cmd.Flags().BoolVar(&noTimer, "no-timer", false, "do not try to install the server poll timer")
	cmd.Flags().BoolVar(&asJSON, "json", flags.JSON, "machine-readable output")
	return cmd
}

// readTokenStdin reads the HTTPS token from stdin (never argv).
func readTokenStdin(r io.Reader) ([]byte, error) {
	token, err := io.ReadAll(io.LimitReader(r, 8192))
	if err != nil {
		return nil, err
	}
	token = []byte(strings.TrimRight(string(token), "\r\n"))
	if len(token) == 0 {
		return nil, fmt.Errorf("no token on stdin — pass it with: teploy config source key --type token ... < token-file")
	}
	return token, nil
}

func newConfigSourceHostKeyCmd(flags *Flags) *cobra.Command {
	var appName, expected string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "host-key",
		Short: "Pin the forge's SSH host key (ssh-keyscan on the server)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if appName == "" {
				return fmt.Errorf("--app is required")
			}
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
			defer cancel()
			t, err := openConfigTarget(ctx, flags, appName)
			if err != nil {
				return err
			}
			defer t.exec.Close()
			repository, err := sourceRepository(ctx, t)
			if err != nil {
				return err
			}
			entries, err := gitsource.PinHostKey(ctx, t.exec, t.app, repository, expected)
			if err != nil {
				return err
			}
			var fps []string
			for _, e := range entries {
				fps = append(fps, e.Fingerprint)
			}
			if asJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(sourceKeyDTO{
					MachineInterface: MachineInterface, Host: t.server, App: t.app,
					Type: "host-key", Created: true, HostKeys: fps,
				})
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Pinned host keys for %s:\n", repository)
			for _, e := range entries {
				fmt.Fprintf(cmd.OutOrStdout(), "  %s\n", e.Fingerprint)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&appName, "app", "", "app whose repository host to pin (required)")
	cmd.Flags().StringVar(&expected, "expected-fingerprint", "", "refuse unless the host presents this fingerprint (SHA256:...)")
	cmd.Flags().BoolVar(&asJSON, "json", flags.JSON, "machine-readable output")
	return cmd
}

func newConfigSourceShowCmd(flags *Flags) *cobra.Command {
	var appName string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "show",
		Short: "Show the app's git source state (no secrets)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if appName == "" {
				return fmt.Errorf("--app is required")
			}
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
			defer cancel()
			t, err := openConfigTarget(ctx, flags, appName)
			if err != nil {
				return err
			}
			defer t.exec.Close()
			dto := sourceShowDTO{MachineInterface: MachineInterface, Host: t.server, App: t.app}
			if _, head, err := confighome.Head(ctx, t.exec, t.app); err == nil && head != "" {
				if raw, _, err := confighome.Read(ctx, t.exec, t.app, head); err == nil {
					if cfg, err := confighome.Load(raw, t.app); err == nil && cfg.Git != nil {
						dto.Repository = cfg.Git.Repository
						dto.Branch = cfg.Git.Branch
						if cfg.Git.Poll == "" {
							dto.Poll = "5m"
						} else {
							dto.Poll = cfg.Git.Poll
						}
					}
				}
			}
			if show, err := gitsource.Show(ctx, t.exec, t.app); err == nil {
				cred := &sourceCredentialDTO{Type: show.Type, PublicKey: show.PublicKey, Fingerprint: show.Fingerprint, Username: show.Username}
				for _, kh := range show.KnownHosts {
					cred.KnownHosts = append(cred.KnownHosts, kh.Fingerprint)
				}
				dto.Credential = cred
			}
			if dto.Repository != "" {
				if st, err := gitsource.LoadState(ctx, t.exec, t.app); err == nil && st.Repository == dto.Repository {
					ref := "refs/heads/" + dto.Branch
					if r := st.Ref(ref); r != nil {
						trig := &sourceTriggerDTO{Ref: ref, Watermark: r.Watermark, LastHead: r.LastSeen, RefusedHead: r.RefusedHead}
						if c := st.Commits[ref+"/"+short12(r.Watermark)]; c != nil {
							trig.LastDeployedRelease = c.Release
						}
						dto.Trigger = trig
					}
				}
			}
			if asJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(dto)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s on %s\n", t.app, t.server)
			if dto.Repository == "" {
				fmt.Fprintf(cmd.OutOrStdout(), "  source: none (set git: in its config)\n")
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "  source: %s (branch %s, poll %s)\n", dto.Repository, dto.Branch, dto.Poll)
			}
			if dto.Credential == nil {
				fmt.Fprintf(cmd.OutOrStdout(), "  credential: none\n")
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "  credential: %s", dto.Credential.Type)
				if dto.Credential.Fingerprint != "" {
					fmt.Fprintf(cmd.OutOrStdout(), " (%s)", dto.Credential.Fingerprint)
				}
				fmt.Fprintln(cmd.OutOrStdout())
				for _, kh := range dto.Credential.KnownHosts {
					fmt.Fprintf(cmd.OutOrStdout(), "  host key: %s\n", kh)
				}
			}
			if dto.Trigger != nil {
				fmt.Fprintf(cmd.OutOrStdout(), "  watermark: %s (last head %s)\n", short12(dto.Trigger.Watermark), short12(dto.Trigger.LastHead))
				if dto.Trigger.RefusedHead != "" {
					fmt.Fprintf(cmd.OutOrStdout(), "  refused: %s — reset with `teploy trigger reset --app %s --ref %s`\n", short12(dto.Trigger.RefusedHead), t.app, dto.Trigger.Ref)
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&appName, "app", "", "app to show (required)")
	cmd.Flags().BoolVar(&asJSON, "json", flags.JSON, "machine-readable output")
	return cmd
}

func newConfigSourceRemoveCmd(flags *Flags) *cobra.Command {
	var appName string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "remove",
		Short: "Forget the app's credential, host keys and code mirror",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if appName == "" {
				return fmt.Errorf("--app is required")
			}
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
			defer cancel()
			t, err := openConfigTarget(ctx, flags, appName)
			if err != nil {
				return err
			}
			defer t.exec.Close()
			if err := gitsource.Remove(ctx, t.exec, t.app); err != nil {
				return err
			}
			// The mirror holds only code, but it is the credential's
			// working set: forget it with the credential.
			if _, err := t.exec.Run(ctx, "rm -rf "+ssh.ShellQuote(confighome.Dir(t.app)+"/mirror.git")); err != nil {
				return err
			}
			events.Record(ctx, t.exec, events.Event{App: t.app, Kind: events.KindSourceConnect, Outcome: events.Succeeded,
				Detail: map[string]any{"action": "remove"}})
			if asJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(sourceKeyDTO{
					MachineInterface: MachineInterface, Host: t.server, App: t.app, Type: "none",
				})
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Removed %s's git credential and mirror\n", t.app)
			return nil
		},
	}
	cmd.Flags().StringVar(&appName, "app", "", "app whose credential to remove (required)")
	cmd.Flags().BoolVar(&asJSON, "json", flags.JSON, "machine-readable output")
	return cmd
}

// sourceRepository reads the app's git.repository from its Teploy config.
func sourceRepository(ctx context.Context, t *configTarget) (string, error) {
	raw, _, err := readHead(ctx, t)
	if err != nil {
		return "", err
	}
	cfg, err := config.LoadAppBytes(raw)
	if err != nil {
		return "", err
	}
	if cfg.Git == nil || cfg.Git.Repository == "" {
		return "", fmt.Errorf("%s has no git: source in its config — set one first (teploy config set --app %s --host %s git.repository=<url> git.branch=<branch>)", t.app, t.app, t.server)
	}
	return cfg.Git.Repository, nil
}
