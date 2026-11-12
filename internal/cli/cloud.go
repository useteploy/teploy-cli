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
	cryptossh "golang.org/x/crypto/ssh"
	"golang.org/x/term"
)

// cloudEnv carries the side effects of the `teploy cloud` commands so tests
// can replace the terminal, the SSH wait and the server setup.
type cloudEnv struct {
	in              io.Reader
	out             io.Writer
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
	createAttempts   int
	retryDelay       time.Duration
}

const (
	cloudPollInterval     = 5 * time.Second
	cloudProvisionTimeout = 10 * time.Minute
	cloudSSHWaitTimeout   = 2 * time.Minute
)

func defaultCloudEnv() *cloudEnv {
	return &cloudEnv{
		in:              os.Stdin,
		out:             os.Stdout,
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
		createAttempts:   3,
		retryDelay:       3 * time.Second,
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
			"and create or delete managed servers. A created server is added to\n" +
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
			"Create the key in the Teploy Cloud dashboard. The key is never accepted\n" +
			"as a command-line value; use --key-stdin to pipe it in.\n\n" +
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

type cloudStatusDTO struct {
	BaseURL  string `json:"base_url"`
	LoggedIn bool   `json:"logged_in"`
	OrgID    string `json:"org_id,omitempty"`
	KeyWorks bool   `json:"key_works"`
	Error    string `json:"error,omitempty"`
}

func newCloudStatusCmd(flags *Flags, env *cloudEnv) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show the Teploy Cloud URL, organization, and whether the key works",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			path, err := cloudCredentialsPath()
			if err != nil {
				return err
			}
			st := cloudStatusDTO{}
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
				enc := json.NewEncoder(env.out)
				enc.SetIndent("", "  ")
				if err := enc.Encode(st); err != nil {
					return err
				}
			} else {
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
			}
			if checkErr != nil {
				return checkErr
			}
			return nil
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
				return writeJSONTo(env.out, servers)
			}
			if len(servers) == 0 {
				fmt.Fprintln(env.out, "No servers. Create one with `teploy cloud server create`.")
				return nil
			}
			tw := tabwriter.NewWriter(env.out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tSTATUS\tIP\tPROVIDER\tREGION\tSIZE\tMONTHLY")
			for _, s := range servers {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", s.Name, s.Status, dash(s.IPAddress),
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
					return writeJSONTo(env.out, providers)
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
				return writeJSONTo(env.out, regions)
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
				return writeJSONTo(env.out, sizes)
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
		Short: "Create or delete Teploy Cloud servers",
	}
	cmd.AddCommand(newCloudServerCreateCmd(flags, env))
	cmd.AddCommand(newCloudServerDeleteCmd(env))
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
			"The local SSH public key (--ssh-key, else ~/.ssh/id_ed25519.pub, else\n" +
			"~/.ssh/id_rsa.pub) is registered with Teploy Cloud if it is not there yet.\n" +
			"After the server is active it is added to ~/.teploy/servers.yml as\n" +
			"root@<ip> under <name>, and `teploy setup` runs against it unless\n" +
			"--no-setup is given.\n\n" +
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
	cmd.Flags().StringVar(&o.sshKey, "ssh-key", "", "path to the SSH public key (.pub) to install")
	cmd.Flags().BoolVar(&o.noSetup, "no-setup", false, "skip running teploy setup after the server is active")
	cmd.Flags().BoolVarP(&o.yes, "yes", "y", false, "skip the purchase confirmation")
	return cmd
}

// localPublicKey picks the public key to install: path, else the first of
// ~/.ssh/id_ed25519.pub and ~/.ssh/id_rsa.pub. It returns the key line and
// the path it came from.
func localPublicKey(path string) (string, string, error) {
	candidates := []string{path}
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", "", fmt.Errorf("finding home directory: %w", err)
		}
		candidates = []string{
			filepath.Join(home, ".ssh", "id_ed25519.pub"),
			filepath.Join(home, ".ssh", "id_rsa.pub"),
		}
	}
	for _, p := range candidates {
		data, err := os.ReadFile(p)
		if errors.Is(err, os.ErrNotExist) && path == "" {
			continue
		}
		if err != nil {
			return "", "", fmt.Errorf("reading SSH public key: %w", err)
		}
		line := strings.TrimSpace(string(data))
		if _, _, _, _, err := cryptossh.ParseAuthorizedKey([]byte(line)); err != nil {
			return "", "", fmt.Errorf("%s is not an SSH public key: %w", p, err)
		}
		return line, p, nil
	}
	return "", "", errors.New("no SSH public key found in ~/.ssh (id_ed25519.pub, id_rsa.pub): pass --ssh-key")
}

// samePublicKey compares two authorized_keys lines by key type and data,
// ignoring the comment.
func samePublicKey(a, b string) bool {
	fa, fb := strings.Fields(a), strings.Fields(b)
	return len(fa) >= 2 && len(fb) >= 2 && fa[0] == fb[0] && fa[1] == fb[1]
}

func publicKeyName(line, path string) string {
	if f := strings.Fields(line); len(f) >= 3 {
		return strings.Join(f[2:], " ")
	}
	host, _ := os.Hostname()
	if host == "" {
		return "teploy-cli " + filepath.Base(path)
	}
	return "teploy-cli " + host
}

func runCloudServerCreate(ctx context.Context, flags *Flags, env *cloudEnv, o cloudCreateOpts) error {
	if o.provider == "" || o.region == "" || o.size == "" {
		return errors.New("--provider, --region and --size are required (see `teploy cloud regions` and `teploy cloud sizes`)")
	}
	serversPath, err := config.DefaultServersPath()
	if err != nil {
		return fmt.Errorf("determining servers path: %w", err)
	}
	if err := refuseExistingServer(serversPath, o.name); err != nil {
		return err
	}
	client, err := loadCloudClient()
	if err != nil {
		return err
	}

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

	// a. The SSH key.
	pubKey, pubPath, err := localPublicKey(o.sshKey)
	if err != nil {
		return err
	}
	keys, err := client.ListSSHKeys(ctx)
	if err != nil {
		return fmt.Errorf("listing SSH keys: %w", err)
	}
	var keyID string
	for _, k := range keys {
		if samePublicKey(k.PublicKey, pubKey) {
			keyID = k.ID
			break
		}
	}
	if keyID == "" {
		k, err := client.CreateSSHKey(ctx, publicKeyName(pubKey, pubPath), pubKey)
		if err != nil {
			return fmt.Errorf("registering SSH key %s: %w", pubPath, err)
		}
		keyID = k.ID
		fmt.Fprintf(env.out, "Registered SSH key %s with Teploy Cloud\n", pubPath)
	}

	// b. The price.
	fmt.Fprintf(env.out, "Server %q: %s, region %s, size %s, %s per month.\n",
		o.name, o.provider, o.region, size.ID, formatCents(size.PriceMonthlyCents))
	if !o.yes && !env.confirm("Buy this server? Your organization is billed monthly until it is deleted.") {
		return errors.New("aborted: no server was created")
	}

	// c. The purchase, one idempotency key across retries.
	idemKey, err := cloud.NewIdempotencyKey()
	if err != nil {
		return err
	}
	req := cloud.CreateServerRequest{Provider: o.provider, Name: o.name, Region: o.region, Size: size.ID, SSHKeyID: keyID}
	attempts := env.createAttempts
	if attempts < 1 {
		attempts = 1
	}
	var srv *cloud.Server
	for attempt := 1; ; attempt++ {
		srv, err = client.CreateServer(ctx, req, idemKey)
		if err == nil {
			break
		}
		if attempt >= attempts || !cloud.IsRetryable(err) || ctx.Err() != nil {
			return fmt.Errorf("creating server: %w", err)
		}
		fmt.Fprintf(env.out, "Create request failed (%v); retrying\n", err)
		if err := env.sleep(ctx, env.retryDelay); err != nil {
			return fmt.Errorf("creating server: %w", err)
		}
	}
	fmt.Fprintf(env.out, "Created server %s (id %s); waiting for it to become active\n", srv.Name, srv.ID)

	// d. Wait for active with an address.
	srv, err = waitForActive(ctx, env, client, srv)
	if err != nil {
		return err
	}
	fmt.Fprintf(env.out, "Server %s is active at %s\n", srv.Name, srv.IPAddress)

	// e. servers.yml, through the `teploy server add` path.
	if err := refuseExistingServer(serversPath, o.name); err != nil {
		return fmt.Errorf("%w; the server exists on Teploy Cloud at %s", err, srv.IPAddress)
	}
	if err := config.AddServer(serversPath, o.name, srv.IPAddress, "root", "", ""); err != nil {
		return fmt.Errorf("adding server: %w", err)
	}
	fmt.Fprintf(env.out, "Added server %q (root@%s) to %s\n", o.name, srv.IPAddress, serversPath)

	// f. `teploy setup`.
	if o.noSetup {
		fmt.Fprintf(env.out, "\nSkipped setup. Run `teploy setup %s --name %s` before the first deploy.\n", srv.IPAddress, o.name)
	} else {
		setupFlags := *flags
		setupFlags.User = "root"
		if setupFlags.Key == "" && o.sshKey != "" {
			if priv := strings.TrimSuffix(o.sshKey, ".pub"); priv != o.sshKey {
				if _, err := os.Stat(priv); err == nil {
					setupFlags.Key = priv
				}
			}
		}
		fmt.Fprintf(env.out, "Waiting for SSH on %s\n", srv.IPAddress)
		if err := env.waitSSH(ctx, srv.IPAddress, setupFlags.Key); err != nil {
			return fmt.Errorf("server %s is active but SSH is not reachable: %w (retry with `teploy setup %s --name %s`)",
				o.name, err, srv.IPAddress, o.name)
		}
		if err := env.setup(&setupFlags, srv.IPAddress, o.name, o.yes); err != nil {
			return fmt.Errorf("setting up %s: %w (retry with `teploy setup %s --name %s`)", o.name, err, srv.IPAddress, o.name)
		}
	}

	// g. Next step.
	fmt.Fprintf(env.out, "\nNext: put the server in teploy.yml and deploy:\n\n  server: %s\n\n  teploy deploy\n", o.name)
	return nil
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

func waitForActive(ctx context.Context, env *cloudEnv, client *cloud.Client, srv *cloud.Server) (*cloud.Server, error) {
	ctx, cancel := context.WithTimeout(ctx, env.provisionTimeout)
	defer cancel()
	last := ""
	for {
		if srv.Status == "active" && srv.IPAddress != "" {
			return srv, nil
		}
		if srv.Status == "error" {
			return nil, fmt.Errorf("server %s (id %s) failed to provision: status error (see the Teploy Cloud dashboard, then `teploy cloud server delete %s`)",
				srv.Name, srv.ID, srv.ID)
		}
		if srv.Status != last {
			fmt.Fprintf(env.out, "  status: %s\n", srv.Status)
			last = srv.Status
		}
		if err := env.sleep(ctx, env.pollInterval); err != nil {
			return nil, fmt.Errorf("server %s (id %s) is not active after %s (still %s): check `teploy cloud servers`",
				srv.Name, srv.ID, env.provisionTimeout, srv.Status)
		}
		next, err := client.GetServer(ctx, srv.ID)
		if err != nil {
			if ctx.Err() != nil {
				return nil, fmt.Errorf("server %s (id %s) is not active after %s: check `teploy cloud servers`",
					srv.Name, srv.ID, env.provisionTimeout)
			}
			var ae *cloud.APIError
			if errors.As(err, &ae) && ae.Status < 500 {
				return nil, fmt.Errorf("checking server %s: %w", srv.ID, err)
			}
			fmt.Fprintf(env.out, "  status check failed (%v); retrying\n", err)
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

func newCloudServerDeleteCmd(env *cloudEnv) *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "delete <name-or-id>",
		Short: "Delete a Teploy Cloud server",
		Long: "Destroy a Teploy Cloud server and stop its billing. Everything on the\n" +
			"server is lost. The servers.yml entry of the same name is removed only\n" +
			"when its host is the deleted server's address.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCloudServerDelete(cmdContext(cmd), env, args[0], yes)
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip the confirmation")
	return cmd
}

func runCloudServerDelete(ctx context.Context, env *cloudEnv, ref string, yes bool) error {
	client, err := loadCloudClient()
	if err != nil {
		return err
	}
	servers, err := client.ListServers(ctx)
	if err != nil {
		return fmt.Errorf("listing servers: %w", err)
	}
	var target *cloud.Server
	var byName []cloud.Server
	for i := range servers {
		if servers[i].ID == ref {
			target = &servers[i]
			break
		}
		if servers[i].Name == ref {
			byName = append(byName, servers[i])
		}
	}
	if target == nil {
		switch len(byName) {
		case 0:
			return fmt.Errorf("no Teploy Cloud server named or with id %q (see `teploy cloud servers`)", ref)
		case 1:
			target = &byName[0]
		default:
			ids := make([]string, len(byName))
			for i, s := range byName {
				ids[i] = s.ID
			}
			return fmt.Errorf("%d servers are named %q: delete by id (%s)", len(byName), ref, strings.Join(ids, ", "))
		}
	}

	if !yes && !env.confirm(fmt.Sprintf("Delete server %s (%s, id %s)? It is destroyed with everything on it.",
		target.Name, dash(target.IPAddress), target.ID)) {
		return errors.New("aborted: nothing was deleted")
	}
	if err := client.DeleteServer(ctx, target.ID); err != nil {
		return fmt.Errorf("deleting server: %w", err)
	}
	fmt.Fprintf(env.out, "Deleted server %s (id %s)\n", target.Name, target.ID)

	serversPath, err := config.DefaultServersPath()
	if err != nil {
		return fmt.Errorf("determining servers path: %w", err)
	}
	local, err := config.ListServers(serversPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("reading servers.yml: %w", err)
	}
	entry, ok := local[target.Name]
	if !ok {
		return nil
	}
	if target.IPAddress == "" || entry.Host != target.IPAddress {
		fmt.Fprintf(env.out, "Left servers.yml entry %q in place: its host %s is not the deleted server's address\n", target.Name, entry.Host)
		return nil
	}
	if err := config.RemoveServer(serversPath, target.Name); err != nil {
		return fmt.Errorf("removing server from servers.yml: %w", err)
	}
	fmt.Fprintf(env.out, "Removed server %q from servers.yml\n", target.Name)
	return nil
}
