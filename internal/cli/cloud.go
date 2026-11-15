package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/useteploy/teploy/internal/cloud"
	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/ssh"
	"golang.org/x/term"
)

// cloudEnv carries the side effects of the `teploy cloud` commands so tests
// can replace the terminal, the SSH wait and the server setup.
type cloudEnv struct {
	in  io.Reader
	out io.Writer
	// errOut carries progress lines under --json, so stdout holds only the
	// machine document.
	errOut          io.Writer
	stdinIsTerminal func() bool
	// readHidden prompts and reads one line without echo.
	readHidden func(prompt string) (string, error)
	confirm    func(prompt string) bool
	// waitSSH blocks until host accepts SSH connections as root.
	waitSSH func(ctx context.Context, host, keyPath string) error
	// setup runs the `teploy setup` code path against host, naming the
	// servers.yml entry name.
	setup            func(flags *Flags, host, name string, yes bool) error
	sleep            func(ctx context.Context, d time.Duration) error
	pollInterval     time.Duration
	provisionTimeout time.Duration
	// createTimeout bounds the create request (0: cloud.CreateRequestTimeout).
	createTimeout time.Duration
	// confirmChecks is how many times the server list is read, pollInterval
	// apart, to find out whether a create whose outcome is unknown bought a
	// server.
	confirmChecks int
}

const (
	cloudPollInterval     = 5 * time.Second
	cloudProvisionTimeout = 10 * time.Minute
	cloudSSHWaitTimeout   = 2 * time.Minute
	cloudConfirmChecks    = 3
)

// progress is where human progress lines go: stdout, or stderr under --json.
func (env *cloudEnv) progress(flags *Flags) io.Writer {
	if flags.JSON {
		return env.errOut
	}
	return env.out
}

func defaultCloudEnv() *cloudEnv {
	return &cloudEnv{
		in:              os.Stdin,
		out:             os.Stdout,
		errOut:          os.Stderr,
		stdinIsTerminal: func() bool { return term.IsTerminal(int(os.Stdin.Fd())) },
		readHidden: func(prompt string) (string, error) {
			fmt.Fprint(os.Stderr, prompt)
			b, err := term.ReadPassword(int(os.Stdin.Fd()))
			fmt.Fprintln(os.Stderr)
			return string(b), err
		},
		confirm:          func(prompt string) bool { return confirm(os.Stdout, prompt) },
		waitSSH:          waitForSSH,
		setup:            cloudRunSetup,
		sleep:            sleepCtx,
		pollInterval:     cloudPollInterval,
		provisionTimeout: cloudProvisionTimeout,
		confirmChecks:    cloudConfirmChecks,
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func newCloudCmd(flags *Flags) *cobra.Command {
	return newCloudCmdWithEnv(flags, defaultCloudEnv())
}

func newCloudCmdWithEnv(flags *Flags, env *cloudEnv) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cloud",
		Short: "Buy and manage servers on Teploy Cloud",
		Long: "Connect to Teploy Cloud (" + cloud.DefaultBaseURL + "), list what can be bought,\n" +
			"and create, attach or delete managed servers. A created server is added to\n" +
			"~/.teploy/servers.yml and set up for `teploy deploy`.\n\n" +
			"Credentials are stored in ~/.teploy/cloud.yml (mode 0600). Set\n" +
			cloud.BaseURLEnv + " to use another Teploy Cloud instance.",
	}
	cmd.AddCommand(newCloudLoginCmd(flags, env))
	cmd.AddCommand(newCloudLogoutCmd(env))
	cmd.AddCommand(newCloudStatusCmd(flags, env))
	cmd.AddCommand(newCloudServersCmd(flags, env))
	cmd.AddCommand(newCloudRegionsCmd(flags, env))
	cmd.AddCommand(newCloudSizesCmd(flags, env))
	cmd.AddCommand(newCloudServerCmd(flags, env))
	return cmd
}

func cloudCredentialsPath() (string, error) {
	serversPath, err := config.DefaultServersPath()
	if err != nil {
		return "", fmt.Errorf("determining config directory: %w", err)
	}
	return filepath.Join(filepath.Dir(serversPath), cloud.FileName), nil
}

func loadCloudClient() (*cloud.Client, error) {
	path, err := cloudCredentialsPath()
	if err != nil {
		return nil, err
	}
	creds, err := cloud.LoadCredentials(path)
	if err != nil {
		return nil, err
	}
	base, err := cloud.ResolveBaseURL(creds.BaseURL)
	if err != nil {
		return nil, err
	}
	return cloud.New(base, creds.APIKey, creds.OrgID)
}

func newCloudLoginCmd(flags *Flags, env *cloudEnv) *cobra.Command {
	var (
		org      string
		keyStdin bool
	)
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Store a Teploy Cloud API key and organization id",
		Long: "Ask for an API key (input hidden) and an organization id, check them\n" +
			"against Teploy Cloud, and store them in ~/.teploy/cloud.yml (mode 0600).\n" +
			"The key is never accepted as a command-line value; use --key-stdin to\n" +
			"pipe it in.\n\n" +
			"Create the key in the Teploy Cloud dashboard under Settings > API keys,\n" +
			"while the organization is selected (the key works only there). Give it\n" +
			"read:servers (login, status, the lists and server attach) and\n" +
			"write:servers (server create and delete), or deploy:* for both;\n" +
			"creating and deleting servers also needs an owner or admin role in the\n" +
			"organization. The organization id is under Settings > General >\n" +
			"Organization ID.\n\n" +
			"Examples:\n" +
			"  teploy cloud login --org org_123\n" +
			"  printf '%s' \"$TEPLOY_CLOUD_KEY\" | teploy cloud login --key-stdin --org org_123",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if flags.Key != "" {
				return errors.New("--key is the SSH private key path and is not used by login: the API key is prompted for (hidden) or read with --key-stdin")
			}
			return runCloudLogin(cmd.Context(), env, org, keyStdin)
		},
	}
	cmd.Flags().StringVar(&org, "org", "", "organization id (prompted when omitted)")
	cmd.Flags().BoolVar(&keyStdin, "key-stdin", false, "read the API key from stdin")
	return cmd
}

func runCloudLogin(ctx context.Context, env *cloudEnv, org string, keyStdin bool) error {
	if ctx == nil {
		ctx = context.Background()
	}
	reader := bufio.NewReader(env.in)
	var key string
	switch {
	case keyStdin:
		line, err := reader.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("reading API key from stdin: %w", err)
		}
		key = strings.TrimSpace(line)
	case env.stdinIsTerminal():
		k, err := env.readHidden("API key: ")
		if err != nil {
			return fmt.Errorf("reading API key: %w", err)
		}
		key = strings.TrimSpace(k)
	default:
		return errors.New("stdin is not a terminal: pass --key-stdin and pipe the API key in")
	}
	if key == "" {
		return errors.New("no API key given")
	}

	org = strings.TrimSpace(org)
	if org == "" {
		if keyStdin || !env.stdinIsTerminal() {
			return errors.New("--org is required with --key-stdin")
		}
		fmt.Fprint(env.out, "Organization id: ")
		line, err := reader.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("reading organization id: %w", err)
		}
		org = strings.TrimSpace(line)
		if org == "" {
			return errors.New("no organization id given")
		}
	}

	base, err := cloud.ResolveBaseURL("")
	if err != nil {
		return err
	}
	client, err := cloud.New(base, key, org)
	if err != nil {
		return err
	}
	if _, err := client.ListServers(ctx); err != nil {
		return fmt.Errorf("checking the API key: %w", err)
	}

	path, err := cloudCredentialsPath()
	if err != nil {
		return err
	}
	creds := &cloud.Credentials{OrgID: org, APIKey: key}
	if base != cloud.DefaultBaseURL {
		creds.BaseURL = base
	}
	if err := cloud.SaveCredentials(path, creds); err != nil {
		return err
	}
	fmt.Fprintf(env.out, "Logged in to %s (organization %s). Credentials stored in %s\n", base, org, path)
	return nil
}

func newCloudLogoutCmd(env *cloudEnv) *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Remove the stored Teploy Cloud credentials",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := cloudCredentialsPath()
			if err != nil {
				return err
			}
			removed, err := cloud.RemoveCredentials(path)
			if err != nil {
				return err
			}
			if removed {
				fmt.Fprintf(env.out, "Removed %s\n", path)
			} else {
				fmt.Fprintln(env.out, "Not logged in to Teploy Cloud")
			}
			return nil
		},
	}
}

func newCloudStatusCmd(flags *Flags, env *cloudEnv) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show the Teploy Cloud URL, organization, and whether the key works",
		Long: "Show the Teploy Cloud URL, the stored organization, and whether the stored\n" +
			"API key works. It exits 1 when the key does not work; with --json it\n" +
			"exits 0 and the document reports key_works and error instead.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			path, err := cloudCredentialsPath()
			if err != nil {
				return err
			}
			st := cloudStatusDTO{MachineInterface: MachineInterface}
			creds, err := cloud.LoadCredentials(path)
			var checkErr error
			switch {
			case errors.Is(err, cloud.ErrNotLoggedIn):
				if st.BaseURL, err = cloud.ResolveBaseURL(""); err != nil {
					return err
				}
			case err != nil:
				return err
			default:
				st.LoggedIn = true
				st.OrgID = creds.OrgID
				if st.BaseURL, err = cloud.ResolveBaseURL(creds.BaseURL); err != nil {
					return err
				}
				client, err := cloud.New(st.BaseURL, creds.APIKey, creds.OrgID)
				if err != nil {
					return err
				}
				if _, checkErr = client.ListServers(ctx); checkErr == nil {
					st.KeyWorks = true
				} else {
					st.Error = checkErr.Error()
				}
			}

			if flags.JSON {
				// The document is the whole answer: a key that does not
				// work is reported in it, not as a second (error) document.
				return writeJSONTo(env.out, st)
			}
			fmt.Fprintf(env.out, "URL:          %s\n", st.BaseURL)
			if !st.LoggedIn {
				fmt.Fprintln(env.out, "Logged in:    no (run `teploy cloud login`)")
				return nil
			}
			fmt.Fprintf(env.out, "Organization: %s\n", st.OrgID)
			if st.KeyWorks {
				fmt.Fprintln(env.out, "API key:      works")
			} else {
				fmt.Fprintln(env.out, "API key:      does not work")
			}
			return checkErr
		},
	}
}

func formatCents(c int64) string {
	return fmt.Sprintf("$%d.%02d", c/100, c%100)
}

func writeJSONTo(out io.Writer, v any) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func newCloudServersCmd(flags *Flags, env *cloudEnv) *cobra.Command {
	return &cobra.Command{
		Use:   "servers",
		Short: "List the organization's Teploy Cloud servers",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := loadCloudClient()
			if err != nil {
				return err
			}
			servers, err := client.ListServers(cmdContext(cmd))
			if err != nil {
				return fmt.Errorf("listing servers: %w", err)
			}
			if flags.JSON {
				return writeJSONTo(env.out, cloudServersEnvelope(servers))
			}
			if len(servers) == 0 {
				fmt.Fprintln(env.out, "No servers. Create one with `teploy cloud server create`.")
				return nil
			}
			tw := tabwriter.NewWriter(env.out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tSTATUS\tIP\tPROVIDER\tREGION\tSIZE\tMONTHLY")
			for _, s := range servers {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", s.Name, s.Status, dash(realAddress(s.IPAddress)),
					s.Provider, s.ProviderRegion, s.ProviderSize, formatCents(s.PriceMonthlyCents))
			}
			return tw.Flush()
		},
	}
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func cmdContext(cmd *cobra.Command) context.Context {
	if ctx := cmd.Context(); ctx != nil {
		return ctx
	}
	return context.Background()
}

func newCloudRegionsCmd(flags *Flags, env *cloudEnv) *cobra.Command {
	var provider string
	cmd := &cobra.Command{
		Use:   "regions",
		Short: "List the regions a provider offers",
		Long: "List the regions a Teploy Cloud provider offers. Without --provider,\n" +
			"list the providers instead.\n\n" +
			"Example:\n  teploy cloud regions --provider vultr",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := loadCloudClient()
			if err != nil {
				return err
			}
			ctx := cmdContext(cmd)
			if provider == "" {
				providers, err := client.ListProviders(ctx)
				if err != nil {
					return fmt.Errorf("listing providers: %w", err)
				}
				if flags.JSON {
					return writeJSONTo(env.out, cloudProvidersEnvelope(providers))
				}
				tw := tabwriter.NewWriter(env.out, 0, 0, 2, ' ', 0)
				fmt.Fprintln(tw, "PROVIDER\tNAME\tAVAILABLE")
				for _, p := range providers {
					fmt.Fprintf(tw, "%s\t%s\t%s\n", p.Type, p.Name, yesNo(p.ManagedAvailable))
				}
				if err := tw.Flush(); err != nil {
					return err
				}
				fmt.Fprintln(env.out, "\nPass --provider to list a provider's regions.")
				return nil
			}
			regions, err := client.ListRegions(ctx, provider)
			if err != nil {
				return fmt.Errorf("listing regions: %w", err)
			}
			if flags.JSON {
				return writeJSONTo(env.out, cloudRegionsEnvelope(provider, regions))
			}
			tw := tabwriter.NewWriter(env.out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "REGION\tNAME\tCOUNTRY\tAVAILABLE")
			for _, r := range regions {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.ID, r.Name, r.Country, yesNo(r.Available))
			}
			return tw.Flush()
		},
	}
	cmd.Flags().StringVar(&provider, "provider", "", "provider (e.g. vultr)")
	return cmd
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func newCloudSizesCmd(flags *Flags, env *cloudEnv) *cobra.Command {
	var provider, region string
	cmd := &cobra.Command{
		Use:   "sizes",
		Short: "List the server sizes a provider offers, with monthly prices",
		Long: "List the server sizes a Teploy Cloud provider offers and what each costs\n" +
			"per month. --region limits the list to sizes sold in that region.\n\n" +
			"Example:\n  teploy cloud sizes --provider vultr --region ewr",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if provider == "" {
				return errors.New("--provider is required (list providers with `teploy cloud regions`)")
			}
			client, err := loadCloudClient()
			if err != nil {
				return err
			}
			sizes, err := client.ListSizes(cmdContext(cmd), provider, region)
			if err != nil {
				return fmt.Errorf("listing sizes: %w", err)
			}
			if flags.JSON {
				return writeJSONTo(env.out, cloudSizesEnvelope(provider, region, sizes))
			}
			tw := tabwriter.NewWriter(env.out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "SIZE\tVCPUS\tMEMORY\tDISK\tMONTHLY")
			for _, s := range sizes {
				fmt.Fprintf(tw, "%s\t%d\t%d MB\t%d GB\t%s\n", s.ID, s.VCPUs, s.Memory, s.Disk, formatCents(s.PriceMonthlyCents))
			}
			return tw.Flush()
		},
	}
	cmd.Flags().StringVar(&provider, "provider", "", "provider (e.g. vultr)")
	cmd.Flags().StringVar(&region, "region", "", "only sizes sold in this region")
	return cmd
}

func newCloudServerCmd(flags *Flags, env *cloudEnv) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "server",
		Short: "Create, attach or delete Teploy Cloud servers",
	}
	cmd.AddCommand(newCloudServerCreateCmd(flags, env))
	cmd.AddCommand(newCloudServerAttachCmd(flags, env))
	cmd.AddCommand(newCloudServerDeleteCmd(flags, env))
	return cmd
}

type cloudCreateOpts struct {
	name, provider, region, size, sshKey string
	noSetup, yes                         bool
}

func newCloudServerCreateCmd(flags *Flags, env *cloudEnv) *cobra.Command {
	var o cloudCreateOpts
	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Buy a managed server, add it to servers.yml, and set it up",
		Long: "Buy a server on Teploy Cloud. This bills your organization monthly.\n\n" +
			"The server gets the public half of the SSH key the CLI then connects\n" +
			"with: --key, else the private key next to --ssh-key <key>.pub, else\n" +
			"~/.ssh/id_ed25519, else ~/.ssh/id_rsa. After you confirm the price, that\n" +
			"public key is registered with Teploy Cloud if it is not there yet, and\n" +
			"the server is bought with one request that is never repeated. When the\n" +
			"server is active it is added to ~/.teploy/servers.yml as root@<ip>\n" +
			"under <name>, and `teploy setup` runs against it unless --no-setup.\n\n" +
			"If the command stops after the purchase, it prints the\n" +
			"`teploy cloud server attach` command that finishes the job; do not run\n" +
			"create again, which would buy a second server.\n\n" +
			"With --json, --yes and --no-setup are required, progress goes to stderr\n" +
			"and stdout carries one result document, also when the command fails\n" +
			"after the purchase.\n\n" +
			"Examples:\n" +
			"  teploy cloud sizes --provider vultr --region ewr\n" +
			"  teploy cloud server create web1 --provider vultr --region ewr --size vc2-1c-1gb",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			o.name = args[0]
			ctx, cancel := signal.NotifyContext(cmdContext(cmd), os.Interrupt)
			defer cancel()
			return runCloudServerCreate(ctx, flags, env, o)
		},
	}
	cmd.Flags().StringVar(&o.provider, "provider", "", "provider (e.g. vultr)")
	cmd.Flags().StringVar(&o.region, "region", "", "region id (list them with: teploy cloud regions --provider P)")
	cmd.Flags().StringVar(&o.size, "size", "", "size id (list them with: teploy cloud sizes --provider P --region R)")
	cmd.Flags().StringVar(&o.sshKey, "ssh-key", "", "SSH public key (.pub) to install; its private key must sit next to it (default: derived from --key or ~/.ssh/id_ed25519)")
	cmd.Flags().BoolVar(&o.noSetup, "no-setup", false, "skip running teploy setup after the server is active")
	cmd.Flags().BoolVarP(&o.yes, "yes", "y", false, "skip the purchase confirmation")
	return cmd
}

// cloudIdentity is the SSH identity a new server is set up with: the
// private key the CLI connects with and the public half installed on it.
type cloudIdentity struct {
	privPath string
	pubLine  string
}

// resolveCloudIdentity picks the private key the CLI will connect with
// (--key, else the private key next to --ssh-key, else the first loadable
// of ~/.ssh/id_ed25519 and ~/.ssh/id_rsa, the order ssh.Connect uses) and
// derives the public key to install from it, so the key on the server is
// always the key the CLI logs in with. A --ssh-key that is not the public
// half of that private key is refused before anything is bought.
func resolveCloudIdentity(keyFlag, sshKeyFlag string) (*cloudIdentity, error) {
	priv := keyFlag
	if priv == "" && sshKeyFlag != "" {
		p := strings.TrimSuffix(sshKeyFlag, ".pub")
		if p == sshKeyFlag {
			return nil, fmt.Errorf("--ssh-key %s: expected the public key file (<key>.pub) next to its private key, or pass --key <private key>", sshKeyFlag)
		}
		if _, err := os.Stat(p); err != nil {
			return nil, fmt.Errorf("--ssh-key %s: the private key %s is needed to connect to the new server and cannot be read (%v); pass --key <private key>", sshKeyFlag, p, err)
		}
		priv = p
	}
	candidates := []string{priv}
	if priv == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("finding home directory: %w", err)
		}
		candidates = []string{filepath.Join(home, ".ssh", "id_ed25519"), filepath.Join(home, ".ssh", "id_rsa")}
	}
	var lastErr error
	for _, p := range candidates {
		if priv == "" {
			if _, err := os.Stat(p); err != nil {
				continue
			}
		}
		pub, err := ssh.PublicKeyBytes(p)
		if err != nil {
			if priv != "" {
				return nil, fmt.Errorf("SSH key %s: %w", p, err)
			}
			lastErr = err
			continue
		}
		line := strings.TrimSpace(string(pub))
		if sshKeyFlag != "" {
			data, err := os.ReadFile(sshKeyFlag)
			if err != nil {
				return nil, fmt.Errorf("reading --ssh-key: %w", err)
			}
			if !samePublicKey(strings.TrimSpace(string(data)), line) {
				return nil, fmt.Errorf("--ssh-key %s is not the public half of %s, the key the CLI connects with: the new server would be unreachable", sshKeyFlag, p)
			}
		}
		return &cloudIdentity{privPath: p, pubLine: line}, nil
	}
	if lastErr != nil {
		return nil, fmt.Errorf("no usable SSH key in ~/.ssh (%v): pass --key <private key>", lastErr)
	}
	return nil, errors.New("no SSH private key found in ~/.ssh (id_ed25519, id_rsa): pass --key <private key>")
}

// samePublicKey compares two authorized_keys lines by key type and data,
// ignoring the comment.
func samePublicKey(a, b string) bool {
	fa, fb := strings.Fields(a), strings.Fields(b)
	return len(fa) >= 2 && len(fb) >= 2 && fa[0] == fb[0] && fa[1] == fb[1]
}

// publicKeyName names a key registered with Teploy Cloud: the comment of
// the private key's .pub file when it has one, else teploy-cli <host>.
func publicKeyName(privPath string) string {
	if data, err := os.ReadFile(privPath + ".pub"); err == nil {
		if f := strings.Fields(string(data)); len(f) >= 3 {
			return strings.Join(f[2:], " ")
		}
	}
	host, _ := os.Hostname()
	if host == "" {
		return "teploy-cli " + filepath.Base(privPath)
	}
	return "teploy-cli " + host
}

// errCloudPurchaseUnknown marks a create whose outcome the CLI could not
// establish. Under --json it is reported as uncertain-outcome.
var errCloudPurchaseUnknown = errors.New("teploy cloud: purchase outcome unknown")

type cloudPurchaseUnknownError struct{ msg string }

func (e *cloudPurchaseUnknownError) Error() string { return e.msg }
func (e *cloudPurchaseUnknownError) Is(target error) bool {
	return target == errCloudPurchaseUnknown
}

// commandLine renders argv for a user to copy, quoting only what needs it.
func commandLine(argv []string) string {
	out := make([]string, len(argv))
	for i, a := range argv {
		if a == "" || strings.ContainsAny(a, " \t\n'\"$`\\|&;<>()*?[]#~") {
			a = ssh.ShellQuote(a)
		}
		out[i] = a
	}
	return strings.Join(out, " ")
}

func runCloudServerCreate(ctx context.Context, flags *Flags, env *cloudEnv, o cloudCreateOpts) error {
	if o.provider == "" || o.region == "" || o.size == "" {
		return errors.New("--provider, --region and --size are required (see `teploy cloud regions` and `teploy cloud sizes`)")
	}
	if flags.JSON && (!o.yes || !o.noSetup) {
		return errors.New("--json needs --yes (a purchase is never confirmed by a prompt in machine mode) and --no-setup (setup prints its own progress on stdout); run `teploy setup <ip> --name <name>` afterwards")
	}
	progress := env.progress(flags)
	serversPath, err := config.DefaultServersPath()
	if err != nil {
		return fmt.Errorf("determining servers path: %w", err)
	}
	if err := refuseExistingServer(serversPath, o.name); err != nil {
		return err
	}
	ident, err := resolveCloudIdentity(flags.Key, o.sshKey)
	if err != nil {
		return err
	}
	client, err := loadCloudClient()
	if err != nil {
		return err
	}
	client.SetCreateTimeout(env.createTimeout)

	sizes, err := client.ListSizes(ctx, o.provider, o.region)
	if err != nil {
		return fmt.Errorf("looking up size %q: %w", o.size, err)
	}
	var size *cloud.Size
	for i := range sizes {
		if sizes[i].ID == o.size || (sizes[i].Slug != "" && sizes[i].Slug == o.size) {
			size = &sizes[i]
			break
		}
	}
	if size == nil {
		return fmt.Errorf("size %q is not sold by %s in %s (see `teploy cloud sizes --provider %s --region %s`)",
			o.size, o.provider, o.region, o.provider, o.region)
	}
	keys, err := client.ListSSHKeys(ctx)
	if err != nil {
		return fmt.Errorf("listing SSH keys: %w", err)
	}
	var keyID string
	for _, k := range keys {
		if samePublicKey(k.PublicKey, ident.pubLine) {
			keyID = k.ID
			break
		}
	}

	// The price, and the only consent to spend money.
	fmt.Fprintf(progress, "Server %q: %s, region %s, size %s, %s per month. SSH key: %s.\n",
		o.name, o.provider, o.region, size.ID, formatCents(size.PriceMonthlyCents), ident.privPath)
	if !o.yes && !env.confirm("Buy this server? Your organization is billed monthly until it is deleted.") {
		return errors.New("aborted: no server was created")
	}

	// The SSH key is registered only after the user agreed to buy.
	if keyID == "" {
		k, err := client.CreateSSHKey(ctx, publicKeyName(ident.privPath), ident.pubLine)
		if err != nil {
			return fmt.Errorf("registering SSH key %s.pub: %w (nothing was bought)", ident.privPath, err)
		}
		keyID = k.ID
		fmt.Fprintf(progress, "Registered SSH key %s.pub with Teploy Cloud\n", ident.privPath)
	}

	// The servers that exist before the purchase, so a create whose answer
	// is lost can be told apart from a server that was already there.
	before, err := client.ListServers(ctx)
	if err != nil {
		return fmt.Errorf("listing servers before the purchase: %w (nothing was bought)", err)
	}
	// One key per invocation. Teploy Cloud may deduplicate on it; the CLI
	// does not rely on that and never sends the create twice.
	idemKey, err := cloud.NewIdempotencyKey()
	if err != nil {
		return err
	}
	req := cloud.CreateServerRequest{Provider: o.provider, Name: o.name, Region: o.region, Size: size.ID, SSHKeyID: keyID}
	res := &cloudServerResultDTO{MachineInterface: MachineInterface, Setup: cloudSetupNotRun}
	srv, err := client.CreateServer(ctx, req, idemKey)
	if err == nil && srv.ID == "" {
		// A success that does not say which server: look for it.
		err = errors.New("Teploy Cloud answered without a server id")
	}
	switch {
	case err == nil:
		fmt.Fprintf(progress, "Created server %s (id %s); waiting for it to become active\n", srv.Name, srv.ID)
	case cloud.OutcomeKnown(err):
		return fmt.Errorf("creating server: %w (nothing was bought)", err)
	case ctx.Err() != nil:
		return &cloudPurchaseUnknownError{msg: fmt.Sprintf(
			"interrupted while buying server %q: it may or may not have been bought. Check with `teploy cloud servers` before trying again; if %q is listed, finish it with `teploy cloud server attach <id>`",
			o.name, o.name)}
	default:
		fmt.Fprintf(progress, "The create request did not complete (%v).\nChecking whether Teploy Cloud bought %q anyway; the request is not sent again.\n", err, o.name)
		srv, err = findNewCloudServer(ctx, env, client, before, req, err)
		if err != nil {
			return err
		}
		res.Adopted = true
		fmt.Fprintf(progress, "Found server %s (id %s), created by that request; continuing with it\n", srv.Name, srv.ID)
	}
	resumeKey := flags.Key
	if resumeKey == "" && o.sshKey != "" {
		resumeKey = ident.privPath
	}
	return finishCloudServer(ctx, flags, env, client, srv, o.name, ident.privPath, resumeKey, o.noSetup, o.yes, res)
}

// findNewCloudServer looks for the server a create with an unknown outcome
// may have bought: a server whose id was not in before, with the requested
// name and, where the row says them, the requested provider, region and
// size (Teploy Cloud stores them as sent, so a same-named server somebody
// else created meanwhile with another shape is not taken for ours). It
// reads the list up to env.confirmChecks times, pollInterval apart (a
// purchase still in progress is listed from its reservation on). It never
// posts.
func findNewCloudServer(ctx context.Context, env *cloudEnv, client *cloud.Client, before []cloud.Server, want cloud.CreateServerRequest, createErr error) (*cloud.Server, error) {
	name := want.Name
	differs := func(got, asked string) bool { return got != "" && got != asked }
	known := make(map[string]bool, len(before))
	for _, s := range before {
		known[s.ID] = true
	}
	checks := env.confirmChecks
	if checks < 1 {
		checks = 1
	}
	var listErr error
	for i := 0; i < checks; i++ {
		if i > 0 {
			if err := env.sleep(ctx, env.pollInterval); err != nil {
				break
			}
		}
		servers, err := client.ListServers(ctx)
		if err != nil {
			listErr = err
			continue
		}
		listErr = nil
		var fresh []cloud.Server
		for _, s := range servers {
			if known[s.ID] || s.Name != name {
				continue
			}
			if differs(s.Provider, want.Provider) || differs(s.ProviderRegion, want.Region) || differs(s.ProviderSize, want.Size) {
				continue
			}
			fresh = append(fresh, s)
		}
		switch len(fresh) {
		case 0:
			continue
		case 1:
			return &fresh[0], nil
		default:
			ids := make([]string, len(fresh))
			for j, s := range fresh {
				ids[j] = s.ID
			}
			return nil, &cloudPurchaseUnknownError{msg: fmt.Sprintf(
				"the create request failed (%v) and %d new servers named %q are now listed (%s); none was adopted. Keep one with `teploy cloud server attach <id>` and delete the others with `teploy cloud server delete <id>`",
				createErr, len(fresh), name, strings.Join(ids, ", "))}
		}
	}
	if listErr != nil {
		return nil, &cloudPurchaseUnknownError{msg: fmt.Sprintf(
			"could not confirm whether server %q was bought: the create request failed (%v) and listing servers failed (%v). Check with `teploy cloud servers` or the Teploy Cloud dashboard before trying again; if %q is listed, finish it with `teploy cloud server attach <id>`",
			name, createErr, listErr, name)}
	}
	return nil, &cloudPurchaseUnknownError{msg: fmt.Sprintf(
		"server %q does not appear to have been bought: the create request failed (%v) and no new server of that name is listed in the organization. Teploy Cloud may still be working on the request, so run `teploy cloud servers` in a few minutes before trying again; if %q shows up, finish it with `teploy cloud server attach <id>` instead of creating another",
		name, createErr, name)}
}

// finishCloudServer takes a bought server to the end: wait until it is
// active with a real address, add it to servers.yml under name, and run
// `teploy setup` unless noSetup. keyPath is the private key setup connects
// with; resumeKey is the --key to name in the resume command ("" for the
// default identity). Every failure names the command that resumes from it.
func finishCloudServer(ctx context.Context, flags *Flags, env *cloudEnv, client *cloud.Client, srv *cloud.Server,
	name, keyPath, resumeKey string, noSetup, yes bool, res *cloudServerResultDTO) error {
	progress := env.progress(flags)
	attach := func(name string) []string {
		argv := []string{"teploy", "cloud", "server", "attach", srv.ID}
		if name != srv.Name {
			argv = append(argv, "--name", name)
		}
		if resumeKey != "" {
			argv = append(argv, "--key", resumeKey)
		}
		if noSetup {
			argv = append(argv, "--no-setup")
		}
		return argv
	}
	resume := attach(name)
	res.Server = cloudServerRow(*srv)
	fail := func(err error, next []string) error {
		res.OK = false
		res.Error = err.Error()
		res.Resume = next
		if flags.JSON {
			_ = writeJSONTo(env.out, res)
		}
		return err
	}

	srv, err := waitForActive(ctx, env, client, srv, progress)
	if err != nil {
		if errors.Is(err, errCloudServerTerminal) {
			return fail(err, nil)
		}
		return fail(fmt.Errorf("%w\nThe server is bought; do not create it again. When it is ready, finish with:\n  %s", err, commandLine(resume)), resume)
	}
	res.Server = cloudServerRow(*srv)
	ip := realAddress(srv.IPAddress)
	fmt.Fprintf(progress, "Server %s is active at %s\n", srv.Name, ip)

	serversPath, err := config.DefaultServersPath()
	if err != nil {
		return fail(fmt.Errorf("determining servers path: %w", err), resume)
	}
	added, err := addCloudServerEntry(serversPath, name, ip)
	if err != nil {
		other := attach("<other-name>")
		return fail(fmt.Errorf("%w; the server is bought (id %s, %s). Finish it under another name with:\n  %s", err, srv.ID, ip, commandLine(other)), other)
	}
	res.ServersYML = name
	if added {
		fmt.Fprintf(progress, "Added server %q (root@%s) to %s\n", name, ip, serversPath)
	} else {
		fmt.Fprintf(progress, "Server %q (root@%s) is already in %s\n", name, ip, serversPath)
	}

	if noSetup {
		res.Setup = cloudSetupSkipped
		fmt.Fprintf(progress, "\nSkipped setup. Run `teploy setup %s --name %s` before the first deploy.\n", ip, name)
	} else {
		setupArgv := []string{"teploy", "setup", ip, "--name", name}
		if resumeKey != "" {
			setupArgv = append(setupArgv, "--key", resumeKey)
		}
		setupFlags := *flags
		setupFlags.User = "root"
		setupFlags.Key = keyPath
		fmt.Fprintf(progress, "Waiting for SSH on %s\n", ip)
		if err := env.waitSSH(ctx, ip, keyPath); err != nil {
			return fail(fmt.Errorf("server %s is active but SSH is not reachable: %w (retry with `%s`)", name, err, commandLine(setupArgv)), setupArgv)
		}
		if err := env.setup(&setupFlags, ip, name, yes); err != nil {
			return fail(fmt.Errorf("setting up %s: %w (retry with `%s`)", name, err, commandLine(setupArgv)), setupArgv)
		}
		res.Setup = cloudSetupDone
	}

	res.OK = true
	if flags.JSON {
		return writeJSONTo(env.out, res)
	}
	fmt.Fprintf(env.out, "\nNext: put the server in teploy.yml and deploy:\n\n  server: %s\n\n  teploy deploy\n", name)
	return nil
}

// addCloudServerEntry adds name -> root@ip to servers.yml. An entry that
// already points at ip is left as it is (added false), so attach can be
// run again; an entry pointing elsewhere is refused.
func addCloudServerEntry(serversPath, name, ip string) (bool, error) {
	servers, err := config.ListServers(serversPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("reading servers.yml: %w", err)
	}
	if existing, ok := servers[name]; ok {
		if existing.Host == ip {
			return false, nil
		}
		return false, fmt.Errorf("server %q already exists in servers.yml (host %s)", name, existing.Host)
	}
	if err := config.AddServer(serversPath, name, ip, "root", "", ""); err != nil {
		return false, fmt.Errorf("adding server to servers.yml: %w", err)
	}
	return true, nil
}

func refuseExistingServer(serversPath, name string) error {
	servers, err := config.ListServers(serversPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("reading servers.yml: %w", err)
	}
	if existing, ok := servers[name]; ok {
		return fmt.Errorf("server %q already exists in servers.yml (host %s): pick another name or remove it with `teploy server remove %s`",
			name, existing.Host, name)
	}
	return nil
}

// errCloudServerTerminal marks a server that will never become usable
// (provisioning failed, or it no longer exists): attach cannot help.
var errCloudServerTerminal = errors.New("teploy cloud: server cannot become active")

// waitForActive polls until the server is active AND has a real address:
// Teploy Cloud reports 0.0.0.0 until the provider assigns one, and that
// placeholder is never treated as the server's address. Bounded by
// env.provisionTimeout.
func waitForActive(ctx context.Context, env *cloudEnv, client *cloud.Client, srv *cloud.Server, out io.Writer) (*cloud.Server, error) {
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, env.provisionTimeout)
	defer cancel()
	stopped := func(srv *cloud.Server) error {
		if parent.Err() != nil {
			return fmt.Errorf("interrupted while waiting for server %s (id %s) to become active (status %s)", srv.Name, srv.ID, srv.Status)
		}
		return fmt.Errorf("server %s (id %s) is not active with an address after %s (status %s)",
			srv.Name, srv.ID, env.provisionTimeout, srv.Status)
	}
	last := ""
	for {
		addr := realAddress(srv.IPAddress)
		if srv.Status == "active" && addr != "" {
			return srv, nil
		}
		if srv.Status == "error" {
			return nil, fmt.Errorf("%w: server %s (id %s) failed to provision: status error. Teploy Cloud removes a server that failed while it was being created and refunds its charge; if `teploy cloud servers` still lists it in a few minutes, see the Teploy Cloud dashboard and remove it with `teploy cloud server delete %s`",
				errCloudServerTerminal, srv.Name, srv.ID, srv.ID)
		}
		shown := srv.Status
		if srv.Status == "active" {
			shown = "active, waiting for an IP address"
		}
		if shown != last {
			fmt.Fprintf(out, "  status: %s\n", shown)
			last = shown
		}
		if err := env.sleep(ctx, env.pollInterval); err != nil {
			return nil, stopped(srv)
		}
		next, err := client.GetServer(ctx, srv.ID)
		if err != nil {
			if ctx.Err() != nil {
				return nil, stopped(srv)
			}
			var ae *cloud.APIError
			if errors.As(err, &ae) && ae.Status == 404 {
				return nil, fmt.Errorf("%w: server %s (id %s) no longer exists on Teploy Cloud (the purchase was undone; see the Teploy Cloud dashboard)",
					errCloudServerTerminal, srv.Name, srv.ID)
			}
			if errors.As(err, &ae) && ae.Status < 500 && ae.Status != 429 {
				return nil, fmt.Errorf("checking server %s: %w", srv.ID, err)
			}
			fmt.Fprintf(out, "  status check failed (%v); retrying\n", err)
			continue
		}
		srv = next
	}
}

// waitForSSH retries an SSH connection as root until it runs a command or
// cloudSSHWaitTimeout passes. A new server's host key is accepted, as
// `teploy setup` does.
func waitForSSH(ctx context.Context, host, keyPath string) error {
	ctx, cancel := context.WithTimeout(ctx, cloudSSHWaitTimeout)
	defer cancel()
	var lastErr error
	for {
		attemptCtx, attemptCancel := context.WithTimeout(ctx, 15*time.Second)
		exec, err := ssh.Connect(attemptCtx, ssh.ConnectConfig{Host: host, User: "root", KeyPath: keyPath, AcceptNewHost: true})
		if err == nil {
			_, err = exec.Run(attemptCtx, "true")
			exec.Close()
		}
		attemptCancel()
		if err == nil {
			return nil
		}
		lastErr = err
		if sleepCtx(ctx, cloudPollInterval) != nil {
			return fmt.Errorf("no SSH after %s: %w", cloudSSHWaitTimeout, lastErr)
		}
	}
}

// cloudRunSetup is the `teploy setup <host> --name <name>` code path.
func cloudRunSetup(flags *Flags, host, name string, yes bool) error {
	return runSetup(flags, host, name, false, "", "", false, yes)
}

// resolveCloudServer finds ref among servers by id, else by a unique name.
func resolveCloudServer(servers []cloud.Server, ref string) (*cloud.Server, error) {
	var byName []cloud.Server
	for i := range servers {
		if servers[i].ID == ref {
			return &servers[i], nil
		}
		if servers[i].Name == ref {
			byName = append(byName, servers[i])
		}
	}
	switch len(byName) {
	case 0:
		return nil, fmt.Errorf("no Teploy Cloud server named or with id %q (see `teploy cloud servers`)", ref)
	case 1:
		return &byName[0], nil
	default:
		ids := make([]string, len(byName))
		for i, s := range byName {
			ids[i] = s.ID
		}
		return nil, fmt.Errorf("%d servers are named %q: use the id (%s)", len(byName), ref, strings.Join(ids, ", "))
	}
}

func newCloudServerAttachCmd(flags *Flags, env *cloudEnv) *cobra.Command {
	var (
		name         string
		noSetup, yes bool
	)
	cmd := &cobra.Command{
		Use:   "attach <name-or-id>",
		Short: "Finish a server that is already bought: wait, add to servers.yml, set up",
		Long: "Take a Teploy Cloud server that is already bought to where `server create`\n" +
			"would have left it: wait until it is active with an address, add it to\n" +
			"~/.teploy/servers.yml as root@<ip>, and run `teploy setup` unless\n" +
			"--no-setup. Nothing is bought. Use it when `server create` stopped after\n" +
			"the purchase; it prints this command with the server's id.\n\n" +
			"The servers.yml name is the server's Teploy Cloud name unless --name is\n" +
			"given. An entry of that name that already points at the server is kept.\n" +
			"Setup connects with --key, else the default SSH identity.\n\n" +
			"With --json, --no-setup is required and stdout carries one result\n" +
			"document.\n\n" +
			"Example:\n  teploy cloud server attach 3f2c9a7e-...",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signal.NotifyContext(cmdContext(cmd), os.Interrupt)
			defer cancel()
			return runCloudServerAttach(ctx, flags, env, args[0], name, noSetup, yes)
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "servers.yml name (default: the server's Teploy Cloud name)")
	cmd.Flags().BoolVar(&noSetup, "no-setup", false, "skip running teploy setup after the server is active")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "answer yes to setup's confirmations")
	return cmd
}

func runCloudServerAttach(ctx context.Context, flags *Flags, env *cloudEnv, ref, name string, noSetup, yes bool) error {
	if flags.JSON && !noSetup {
		return errors.New("--json needs --no-setup (setup prints its own progress on stdout); run `teploy setup <ip> --name <name>` afterwards")
	}
	client, err := loadCloudClient()
	if err != nil {
		return err
	}
	servers, err := client.ListServers(ctx)
	if err != nil {
		return fmt.Errorf("listing servers: %w", err)
	}
	target, err := resolveCloudServer(servers, ref)
	if err != nil {
		return err
	}
	if name == "" {
		name = target.Name
	}
	fmt.Fprintf(env.progress(flags), "Attaching server %s (id %s)\n", target.Name, target.ID)
	res := &cloudServerResultDTO{MachineInterface: MachineInterface, Setup: cloudSetupNotRun}
	return finishCloudServer(ctx, flags, env, client, target, name, flags.Key, flags.Key, noSetup, yes, res)
}

func newCloudServerDeleteCmd(flags *Flags, env *cloudEnv) *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "delete <name-or-id>",
		Short: "Delete a Teploy Cloud server",
		Long: "Destroy a Teploy Cloud server and stop its billing. Everything on the\n" +
			"server is lost. The servers.yml entry of the same name is removed only\n" +
			"when its host is the deleted server's address.\n\n" +
			"With --json, --yes is required and stdout carries one result document.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCloudServerDelete(cmdContext(cmd), flags, env, args[0], yes)
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip the confirmation")
	return cmd
}

func runCloudServerDelete(ctx context.Context, flags *Flags, env *cloudEnv, ref string, yes bool) error {
	if flags.JSON && !yes {
		return errors.New("--json needs --yes: a server is never deleted on a prompt in machine mode")
	}
	progress := env.progress(flags)
	client, err := loadCloudClient()
	if err != nil {
		return err
	}
	servers, err := client.ListServers(ctx)
	if err != nil {
		return fmt.Errorf("listing servers: %w", err)
	}
	target, err := resolveCloudServer(servers, ref)
	if err != nil {
		return err
	}
	addr := realAddress(target.IPAddress)

	if !yes && !env.confirm(fmt.Sprintf("Delete server %s (%s, id %s)? It is destroyed with everything on it.",
		target.Name, dash(addr), target.ID)) {
		return errors.New("aborted: nothing was deleted")
	}
	if err := client.DeleteServer(ctx, target.ID); err != nil {
		return fmt.Errorf("deleting server: %w", err)
	}
	fmt.Fprintf(progress, "Deleted server %s (id %s)\n", target.Name, target.ID)
	res := cloudServerDeleteDTO{MachineInterface: MachineInterface, OK: true, Server: cloudServerRow(*target)}
	done := func() error {
		if flags.JSON {
			return writeJSONTo(env.out, res)
		}
		return nil
	}

	serversPath, err := config.DefaultServersPath()
	if err != nil {
		return fmt.Errorf("determining servers path: %w", err)
	}
	local, err := config.ListServers(serversPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return done()
		}
		return fmt.Errorf("reading servers.yml: %w", err)
	}
	entry, ok := local[target.Name]
	if !ok {
		return done()
	}
	if addr == "" || entry.Host != addr {
		fmt.Fprintf(progress, "Left servers.yml entry %q in place: its host %s is not the deleted server's address\n", target.Name, entry.Host)
		return done()
	}
	if err := config.RemoveServer(serversPath, target.Name); err != nil {
		return fmt.Errorf("removing server from servers.yml: %w", err)
	}
	res.ServersYMLRemoved = true
	fmt.Fprintf(progress, "Removed server %q from servers.yml\n", target.Name)
	return done()
}
