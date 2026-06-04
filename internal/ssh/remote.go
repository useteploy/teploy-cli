package ssh

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
	"golang.org/x/term"
)

// Compile-time check: RemoteExecutor implements Executor.
var _ Executor = (*RemoteExecutor)(nil)

// RemoteExecutor implements Executor using a real SSH connection.
type RemoteExecutor struct {
	client *ssh.Client
	host   string
	user   string
}

// ConnectConfig holds the parameters for establishing an SSH connection.
type ConnectConfig struct {
	Host           string // IP or hostname (with optional :port)
	User           string // SSH user (default: root)
	KeyPath        string // Path to SSH private key (optional, tries defaults)
	Password       string // if set, use password auth instead of/in addition to key auth
	AcceptNewHost  bool   // if true, auto-accept unknown host keys and save to known_hosts
}

// Connect establishes an SSH connection and returns a RemoteExecutor.
func Connect(ctx context.Context, cfg ConnectConfig) (*RemoteExecutor, error) {
	if cfg.User == "" {
		cfg.User = "root"
	}

	host := cfg.Host
	if !strings.Contains(host, ":") {
		host = host + ":22"
	}

	signers, err := resolveSigners(cfg.KeyPath)
	if err != nil {
		return nil, fmt.Errorf("resolving SSH keys: %w", err)
	}
	if len(signers) == 0 && cfg.Password == "" {
		return nil, fmt.Errorf("no SSH keys found; provide --key, set TEPLOY_SSH_KEY, or place a key at ~/.ssh/id_ed25519")
	}

	authMethods := []ssh.AuthMethod{}
	if len(signers) > 0 {
		authMethods = append(authMethods, ssh.PublicKeys(signers...))
	}
	if cfg.Password != "" {
		authMethods = append(authMethods, ssh.Password(cfg.Password))
	}

	var hostKeyCallback ssh.HostKeyCallback
	if cfg.AcceptNewHost {
		home, _ := os.UserHomeDir()
		knownHostsPath := filepath.Join(home, ".ssh", "known_hosts")
		hostKeyCallback = acceptNewHostKeyCallback(knownHostsPath)
	} else {
		hostKeyCallback, err = defaultHostKeyCallback()
		if err != nil {
			return nil, fmt.Errorf("loading known hosts: %w", err)
		}
	}

	clientConfig := &ssh.ClientConfig{
		User:            cfg.User,
		Auth:            authMethods,
		HostKeyCallback: hostKeyCallback,
	}

	client, err := dialWithContext(ctx, "tcp", host, clientConfig)
	if err != nil {
		// Detect SSH auth failures and surface an actionable hint. The raw
		// crypto/ssh message ("unable to authenticate, attempted methods [...]")
		// doesn't tell users that root SSH is commonly disabled and they need
		// --user.
		if strings.Contains(err.Error(), "unable to authenticate") {
			return nil, fmt.Errorf("authentication failed for %s@%s; try --user <name> if the server account isn't %q (root SSH is disabled on most distros), --key <path> for a specific key, or --password to use password auth", cfg.User, cfg.Host, cfg.User)
		}
		return nil, fmt.Errorf("connecting to %s: %w", cfg.Host, err)
	}

	return &RemoteExecutor{client: client, host: cfg.Host, user: cfg.User}, nil
}

func (e *RemoteExecutor) Run(ctx context.Context, cmd string) (string, error) {
	var stdout, stderr bytes.Buffer
	if err := e.RunStream(ctx, cmd, &stdout, &stderr); err != nil {
		if stderr.Len() > 0 {
			return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
		}
		return "", err
	}
	return strings.TrimSpace(stdout.String()), nil
}

func (e *RemoteExecutor) RunStream(ctx context.Context, cmd string, stdout, stderr io.Writer) error {
	session, err := e.client.NewSession()
	if err != nil {
		return fmt.Errorf("creating SSH session: %w", err)
	}
	defer session.Close()

	session.Stdout = stdout
	session.Stderr = stderr

	done := make(chan error, 1)
	go func() {
		done <- session.Run(cmd)
	}()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		_ = session.Signal(ssh.SIGTERM)
		_ = session.Close()
		return ctx.Err()
	}
}

func (e *RemoteExecutor) Upload(ctx context.Context, content io.Reader, remotePath string, mode string) error {
	data, err := io.ReadAll(content)
	if err != nil {
		return fmt.Errorf("reading upload content: %w", err)
	}

	// Use path (not filepath) — remote is always Linux.
	dir := path.Dir(remotePath)

	cmd := fmt.Sprintf("mkdir -p %s && cat > %s && chmod %s %s",
		ShellQuote(dir),
		ShellQuote(remotePath),
		ShellQuote(mode),
		ShellQuote(remotePath),
	)

	session, err := e.client.NewSession()
	if err != nil {
		return fmt.Errorf("creating SSH session for upload: %w", err)
	}
	defer session.Close()

	session.Stdin = bytes.NewReader(data)

	if err := session.Run(cmd); err != nil {
		return fmt.Errorf("uploading %s: %w", remotePath, err)
	}
	return nil
}

func (e *RemoteExecutor) Close() error {
	return e.client.Close()
}

func (e *RemoteExecutor) Host() string {
	return e.host
}

func (e *RemoteExecutor) User() string {
	return e.user
}

// defaultHostKeyCallback returns a known_hosts-based callback, falling back
// to accept-all only if ~/.ssh/known_hosts does not exist.
func defaultHostKeyCallback() (ssh.HostKeyCallback, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return ssh.InsecureIgnoreHostKey(), nil
	}

	knownHostsPath := filepath.Join(home, ".ssh", "known_hosts")
	if _, err := os.Stat(knownHostsPath); err != nil {
		// No known_hosts file — fall back to trust-on-first-use rather than
		// accept-all: record the key on first connect and detect a mismatch on
		// every connection after. InsecureIgnoreHostKey never records and never
		// detects a MITM, so a fresh box (the common CI/first-deploy case) had
		// no host-key protection at all. TOFU keeps first-connect frictionless
		// while closing the silent-MITM hole; a changed key then errors (use
		// --accept-new or clear the entry after a deliberate re-provision).
		return acceptNewHostKeyCallback(knownHostsPath), nil
	}

	callback, err := knownhosts.New(knownHostsPath)
	if err != nil {
		return nil, fmt.Errorf("parsing known_hosts: %w", err)
	}
	return callback, nil
}

// resolveSigners finds and loads SSH private keys.
func resolveSigners(keyPath string) ([]ssh.Signer, error) {
	var paths []string
	if keyPath != "" {
		paths = []string{keyPath}
	} else {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		paths = []string{
			filepath.Join(home, ".ssh", "id_ed25519"),
			filepath.Join(home, ".ssh", "id_rsa"),
		}
	}

	var signers []ssh.Signer
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}

		signer, err := ssh.ParsePrivateKey(data)
		if err != nil {
			var passphraseErr *ssh.PassphraseMissingError
			if errors.As(err, &passphraseErr) {
				signer, err = parseEncryptedKey(data, p)
				if err != nil {
					continue
				}
			} else {
				continue
			}
		}
		signers = append(signers, signer)
	}
	return signers, nil
}

func parseEncryptedKey(data []byte, keyPath string) (ssh.Signer, error) {
	fmt.Fprintf(os.Stderr, "Enter passphrase for %s: ", keyPath)
	passphrase, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return nil, fmt.Errorf("reading passphrase: %w", err)
	}
	return ssh.ParsePrivateKeyWithPassphrase(data, passphrase)
}

func dialWithContext(ctx context.Context, network, addr string, config *ssh.ClientConfig) (*ssh.Client, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}

	c, chans, reqs, err := ssh.NewClientConn(conn, addr, config)
	if err != nil {
		conn.Close()
		return nil, err
	}
	return ssh.NewClient(c, chans, reqs), nil
}

// acceptNewHostKeyCallback returns a host key callback that accepts unknown
// host keys (appending them to known_hosts) but rejects key mismatches.
func acceptNewHostKeyCallback(knownHostsPath string) ssh.HostKeyCallback {
	existing, existingErr := knownhosts.New(knownHostsPath)
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		if existingErr == nil {
			err := existing(hostname, remote, key)
			if err == nil {
				return nil // known and matches
			}
			// Check if it's a genuine key mismatch vs unknown key type.
			var keyErr *knownhosts.KeyError
			if errors.As(err, &keyErr) && len(keyErr.Want) > 0 {
				// The host exists in known_hosts. Check if any wanted key
				// has the same key type — if so, it's a real mismatch (different
				// key for the same type = MITM). If the key types differ,
				// it's just a new key type we haven't seen — accept it.
				presentedType := key.Type()
				for _, want := range keyErr.Want {
					if want.Key.Type() == presentedType {
						return err // same type, different key = real mismatch
					}
				}
				// Different key type — accept and save.
			}
			// Unknown key — fall through to accept and save.
		}
		// Append to known_hosts.
		line := knownhosts.Line([]string{knownhosts.Normalize(hostname)}, key)
		f, err := os.OpenFile(knownHostsPath, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0644)
		if err != nil {
			return nil // accept anyway, just can't save
		}
		defer f.Close()
		fmt.Fprintln(f, line)
		return nil
	}
}

// PublicKeyPath returns the path to the SSH public key file.
// Checks KeyPath+".pub" first, then default locations.
func PublicKeyPath(keyPath string) (string, error) {
	if keyPath != "" {
		pub := keyPath + ".pub"
		if _, err := os.Stat(pub); err == nil {
			return pub, nil
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	for _, name := range []string{"id_ed25519.pub", "id_rsa.pub"} {
		p := filepath.Join(home, ".ssh", name)
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("no SSH public key found")
}

// ShellQuote returns s wrapped in single quotes, safe for POSIX shells —
// single quotes suppress all expansion ($, backticks, backslash, globbing), so
// arbitrary values can be passed through a remote shell without injection or
// corruption. Exported for use by other packages that build remote commands.
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}
