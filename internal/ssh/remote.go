package ssh

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	gossh "golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
	"golang.org/x/term"
)

// Compile-time check: RemoteExecutor implements Executor.
var _ Executor = (*RemoteExecutor)(nil)

// RemoteExecutor implements Executor using a real SSH connection.
type RemoteExecutor struct {
	client *gossh.Client
	host   string
	user   string
	// acceptNewHost records the host-key policy this connection was created
	// with, so secondary channels (e.g. static-deploy rsync) can mirror it.
	acceptNewHost bool
	// commandTimeout bounds each command when the caller's context has
	// no earlier deadline (ConnectConfig.CommandTimeout, C08).
	commandTimeout time.Duration
}

// ConnectConfig holds the parameters for establishing an SSH connection.
type ConnectConfig struct {
	Host          string // IP or hostname (with optional :port)
	User          string // SSH user (default: root)
	KeyPath       string // Path to SSH private key (optional, tries defaults)
	Password      string // if set, use password auth instead of/in addition to key auth
	AcceptNewHost bool   // if true, auto-accept unknown host keys and save to known_hosts
	// CommandTimeout, when > 0, bounds EVERY command run on this
	// connection: a context with no deadline (or a later one) gets this
	// one, so a hung remote command dies at a deadline instead of
	// hanging the CLI forever (C08 bounded subprocess lifetime). A
	// caller-supplied EARLIER deadline always wins. 0 keeps the historic
	// caller-controlled behavior. Long-running work (deploys, log
	// tails) should pass explicit deadlines rather than raise this.
	CommandTimeout time.Duration
}

// Connect establishes an SSH connection and returns a RemoteExecutor.
func Connect(ctx context.Context, cfg ConnectConfig) (*RemoteExecutor, error) {
	if cfg.User == "" {
		cfg.User = "root"
	}

	host := cfg.Host
	if !strings.Contains(host, ":") {
		host = host + ":22"
	} else if _, _, splitErr := net.SplitHostPort(host); splitErr != nil {
		// A host that contains colons but doesn't parse as host:port is a
		// bare (unbracketed) IPv6 literal — "2001:db8::1" — which would
		// otherwise be passed to the dialer as-is and fail. Bracket it.
		if net.ParseIP(strings.Trim(host, "[]")) != nil {
			host = net.JoinHostPort(strings.Trim(host, "[]"), "22")
		}
	}

	signers, err := resolveSigners(cfg.KeyPath)
	if err != nil {
		return nil, fmt.Errorf("resolving SSH keys: %w", err)
	}
	if len(signers) == 0 && cfg.Password == "" {
		return nil, fmt.Errorf("no SSH keys found; provide --key, set TEPLOY_SSH_KEY, or place a key at ~/.ssh/id_ed25519")
	}

	authMethods := []gossh.AuthMethod{}
	if len(signers) > 0 {
		authMethods = append(authMethods, gossh.PublicKeys(signers...))
	}
	if cfg.Password != "" {
		authMethods = append(authMethods, gossh.Password(cfg.Password))
	}

	var hostKeyCallback gossh.HostKeyCallback
	if cfg.AcceptNewHost {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("cannot determine home directory for known_hosts (%w) — set $HOME so host-key verification can record trusted hosts", err)
		}
		knownHostsPath := filepath.Join(home, ".ssh", "known_hosts")
		hostKeyCallback = acceptNewHostKeyCallback(knownHostsPath)
	} else {
		hostKeyCallback, err = defaultHostKeyCallback()
		if err != nil {
			return nil, fmt.Errorf("loading known hosts: %w", err)
		}
	}

	clientConfig := &gossh.ClientConfig{
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

	return &RemoteExecutor{client: client, host: cfg.Host, user: cfg.User, acceptNewHost: cfg.AcceptNewHost, commandTimeout: cfg.CommandTimeout}, nil
}

// boundCtx applies the connection's CommandTimeout when the caller's
// context has no deadline (or a later one). A caller-supplied earlier
// deadline always wins; CommandTimeout 0 leaves the context untouched.
func (e *RemoteExecutor) boundCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	if e.commandTimeout <= 0 {
		return ctx, func() {}
	}
	if d, ok := ctx.Deadline(); ok && d.Before(time.Now().Add(e.commandTimeout)) {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, e.commandTimeout)
}

func (e *RemoteExecutor) Run(ctx context.Context, cmd string) (string, error) {
	ctx, cancel := e.boundCtx(ctx)
	defer cancel()
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
	ctx, cancel := e.boundCtx(ctx)
	defer cancel()
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
		_ = session.Signal(gossh.SIGTERM)
		_ = session.Close()
		// Wait for session.Run to actually return before we do — otherwise the
		// goroutine can keep writing to the caller's stdout/stderr after
		// RunStream has returned (a data race on those writers).
		<-done
		return ctx.Err()
	}
}

func (e *RemoteExecutor) RunInput(ctx context.Context, cmd string, stdin io.Reader) error {
	ctx, cancel := e.boundCtx(ctx)
	defer cancel()
	session, err := e.client.NewSession()
	if err != nil {
		return fmt.Errorf("creating SSH session: %w", err)
	}
	defer session.Close()

	session.Stdin = stdin

	done := make(chan error, 1)
	go func() {
		done <- session.Run(cmd)
	}()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		_ = session.Signal(gossh.SIGTERM)
		_ = session.Close()
		<-done
		return ctx.Err()
	}
}

// runDetailed is RemoteExecutor's native structured capture: separate
// bounded stdout/stderr buffers, the remote exit status from the SSH
// channel's exit-status request, and the context's cancellation vs
// deadline distinction. See Result for the field contract.
func (e *RemoteExecutor) runDetailed(ctx context.Context, cmd string, stdin io.Reader, limit int64) Result {
	ctx, cancel := e.boundCtx(ctx)
	defer cancel()
	if res, done := contextFailureResult(ctx); done {
		return res
	}
	session, err := e.client.NewSession()
	if err != nil {
		return Result{ExitCode: -1, Err: fmt.Errorf("creating SSH session: %w", err)}
	}
	defer session.Close()

	var stdout, stderr limitedBuffer
	stdout.limit, stderr.limit = limit, limit
	session.Stdout = &stdout
	session.Stderr = &stderr
	if stdin != nil {
		session.Stdin = stdin
	}

	done := make(chan error, 1)
	go func() {
		done <- session.Run(cmd)
	}()

	select {
	case err := <-done:
		res := Result{
			Stdout:    stdout.bytes(),
			Stderr:    stderr.bytes(),
			ExitCode:  0,
			Truncated: stdout.overflow || stderr.overflow,
		}
		if err == nil {
			return res
		}
		var exitErr *gossh.ExitError
		if errors.As(err, &exitErr) {
			res.ExitCode = exitErr.Waitmsg.ExitStatus()
			return res
		}
		// No exit status arrived: the command did not complete (channel
		// torn down, connection lost).
		res.ExitCode = -1
		res.Err = err
		return res
	case <-ctx.Done():
		_ = session.Signal(gossh.SIGTERM)
		_ = session.Close()
		<-done
		res := Result{
			Stdout:    stdout.bytes(),
			Stderr:    stderr.bytes(),
			ExitCode:  -1,
			Truncated: stdout.overflow || stderr.overflow,
		}
		res.Canceled = errors.Is(ctx.Err(), context.Canceled)
		res.TimedOut = errors.Is(ctx.Err(), context.DeadlineExceeded)
		res.Err = ctx.Err()
		return res
	}
}

// Upload streams content into a securely created sibling temporary file and
// atomically renames it over remotePath.
//
// The old implementation buffered the whole input in client memory, wrote the
// destination via shell redirection, and chmod'd afterwards — so under a
// normal 022 umask a freshly created secret file was briefly world-readable,
// an interrupted write left a half-written destination, and a pre-existing
// symlink at the destination was followed. All three are closed here:
// mktemp(1) creates the temp 0600, umask 077 keeps it that way until the
// requested mode is applied BEFORE any content lands, cat streams stdin
// without buffering, and mv -f renames on the same filesystem (replacing a
// destination symlink itself, not its target). Readers of remotePath see
// either the old file or the complete new one, never a partial write.
func (e *RemoteExecutor) Upload(ctx context.Context, content io.Reader, remotePath string, mode string) error {
	// Use path (not filepath) — remote is always Linux.
	dir := path.Dir(remotePath)

	script := fmt.Sprintf(
		`umask 077 && mkdir -p %s && tmp=$(mktemp %s) && trap 'rm -f -- "$tmp"' EXIT HUP INT TERM && chmod %s "$tmp" && cat > "$tmp" && mv -fT -- "$tmp" %s && trap - EXIT HUP INT TERM`,
		ShellQuote(dir),
		ShellQuote(dir+"/.teploy-upload.XXXXXXXX"),
		ShellQuote(mode),
		ShellQuote(remotePath),
	)
	if err := e.RunInput(ctx, script, content); err != nil {
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

func (e *RemoteExecutor) AcceptNewHost() bool {
	return e.acceptNewHost
}

func (e *RemoteExecutor) User() string {
	return e.user
}

// defaultHostKeyCallback returns a known_hosts-based callback. When
// known_hosts doesn't exist yet it falls back to trust-on-first-use (see
// acceptNewHostKeyCallback) rather than accepting every key.
func defaultHostKeyCallback() (gossh.HostKeyCallback, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		// Previously fell through to ssh.InsecureIgnoreHostKey() here — silently
		// disabling host-key verification entirely on any box where $HOME can't
		// be resolved (some containers, CI runners, restricted shells), with no
		// indication to the user that MITM protection was off. Fail closed: an
		// unresolvable home directory is rare and the caller can set $HOME or
		// pass --accept-new (which threads its own known_hosts path through
		// Connect, so it hits the same check there, not this one).
		return nil, fmt.Errorf("cannot determine home directory for known_hosts (%w) — set $HOME, or use --accept-new for trust-on-first-use", err)
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
	return func(hostname string, remote net.Addr, key gossh.PublicKey) error {
		if err := callback(hostname, remote, key); err != nil {
			return mismatchHint(hostname, key, err)
		}
		return nil
	}, nil
}

// mismatchHint enriches a knownhosts mismatch error with what the raw
// "knownhosts: key mismatch" omits: the host, the algorithm the server
// presented, and the algorithms known_hosts holds for that host. A
// known_hosts scanned for a single algorithm (ssh-keyscan -t <one>, the
// shape most guides produce) makes every connection that negotiates a
// different algorithm fail without naming either side — which reads as a
// MITM alarm rather than the algorithm-coverage gap it is (2026-09-22 live
// finding from ship's delivery provisioning). Verification is unchanged —
// every mismatch still fails closed; only the message gains context.
// Non-mismatch failures (revoked keys, database problems) pass through.
func mismatchHint(hostname string, key gossh.PublicKey, err error) error {
	var keyErr *knownhosts.KeyError
	if !errors.As(err, &keyErr) || len(keyErr.Want) == 0 {
		return err
	}
	onFile := make([]string, 0, len(keyErr.Want))
	seen := make(map[string]bool, len(keyErr.Want))
	for _, known := range keyErr.Want {
		if t := known.Key.Type(); !seen[t] {
			seen[t] = true
			onFile = append(onFile, t)
		}
	}
	return fmt.Errorf("host key mismatch for %s: server presented %s, known_hosts has no matching entry (has %s) — scan all algorithms (ssh-keyscan without -t), not just one: %w",
		hostname, key.Type(), strings.Join(onFile, ", "), err)
}

// resolveSigners finds and loads SSH private keys. For an EXPLICIT key path,
// read/parse/passphrase failures are returned, never swallowed: falling
// through to the default identities on a bad --key used to end with the
// misleading "no SSH keys found" (or, worse, authenticated as a different
// key than the operator named) instead of the actual key error (TCL-55).
func resolveSigners(keyPath string) ([]gossh.Signer, error) {
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

	var signers []gossh.Signer
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			if keyPath != "" {
				return nil, fmt.Errorf("reading requested SSH identity %s: %w", p, err)
			}
			continue
		}

		signer, err := gossh.ParsePrivateKey(data)
		if err != nil {
			var passphraseErr *gossh.PassphraseMissingError
			if errors.As(err, &passphraseErr) {
				signer, err = parseEncryptedKey(data, p)
				if err != nil {
					if keyPath != "" {
						return nil, fmt.Errorf("using requested SSH identity %s: %w", p, err)
					}
					continue
				}
			} else {
				if keyPath != "" {
					return nil, fmt.Errorf("parsing requested SSH identity %s: %w", p, err)
				}
				continue
			}
		}
		signers = append(signers, signer)
	}
	return signers, nil
}

func parseEncryptedKey(data []byte, keyPath string) (gossh.Signer, error) {
	fmt.Fprintf(os.Stderr, "Enter passphrase for %s: ", keyPath)
	passphrase, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return nil, fmt.Errorf("reading passphrase: %w", err)
	}
	return gossh.ParsePrivateKeyWithPassphrase(data, passphrase)
}

// dialWithContext bounds the SSH handshake by the context. The TCP dial is
// covered by DialContext, but the key-exchange handshake itself
// (ssh.NewClientConn) is not: a slow or malicious peer that accepts TCP and
// then stalls could previously hang the connection attempt indefinitely,
// past any caller timeout. A deadline bounds the whole handshake, and a
// context cancellation closes the underlying connection so a blocked
// handshake unblocks immediately. The deadline is cleared once the
// connection is established so the returned client is not time-limited.
func dialWithContext(ctx context.Context, network, addr string, config *gossh.ClientConfig) (*gossh.Client, error) {
	// The TCP dial itself is bounded even when the caller's context has no
	// deadline (Background): a black-holed address used to rely on the
	// OS-level connect timeout (~75s+) before the handshake deadline below
	// could even start (TCL-55). 15s matches the handshake bound.
	conn, err := (&net.Dialer{Timeout: 15 * time.Second}).DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}

	deadline := time.Now().Add(15 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := conn.SetDeadline(deadline); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("setting handshake deadline: %w", err)
	}
	stopClose := context.AfterFunc(ctx, func() { _ = conn.Close() })
	c, chans, reqs, err := gossh.NewClientConn(conn, addr, config)
	if !stopClose() {
		_ = conn.Close()
		return nil, ctx.Err()
	}
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		_ = conn.Close()
		_ = c.Close()
		return nil, fmt.Errorf("clearing handshake deadline: %w", err)
	}
	return gossh.NewClient(c, chans, reqs), nil
}

// acceptNewHostKeyCallback, the TOFU enrollment machinery, and the
// mismatch / change-vs-rename distinction live in hostkey.go (C08).

// PublicKeyBytes returns the authorized-key line for the identity the
// caller will actually authenticate with (audit A32): for an explicit key
// path the public key is DERIVED from that private key, an existing .pub
// file is verified against it (a stale .pub used to silently provision a
// different identity), and there is no fallthrough to unrelated default
// keys. For an empty keyPath the first loadable default identity is used,
// matching resolveSigners' preference order.
func PublicKeyBytes(keyPath string) ([]byte, error) {
	signers, err := resolveSigners(keyPath)
	if err != nil {
		return nil, err
	}
	if len(signers) == 0 {
		return nil, fmt.Errorf("no SSH identity found for %q", keyPath)
	}
	derived := signers[0].PublicKey()
	if keyPath != "" {
		if raw, rerr := os.ReadFile(keyPath + ".pub"); rerr == nil {
			pub, _, _, _, perr := gossh.ParseAuthorizedKey(raw)
			if perr != nil {
				return nil, fmt.Errorf("parsing %s.pub: %w", keyPath, perr)
			}
			if !bytes.Equal(pub.Marshal(), derived.Marshal()) {
				return nil, fmt.Errorf("%s.pub does not match the private key %s — refusing to provision an unrelated identity", keyPath, keyPath)
			}
		} else if !errors.Is(rerr, fs.ErrNotExist) {
			return nil, fmt.Errorf("reading %s.pub: %w", keyPath, rerr)
		}
	}
	return gossh.MarshalAuthorizedKey(derived), nil
}

// PublicKeyPath returns the path to the SSH public key file. With an
// EXPLICIT key path it returns that key's .pub only when the file exists
// and matches the private key — never an unrelated default (audit A32);
// derive one with PublicKeyBytes when the .pub is absent.
func PublicKeyPath(keyPath string) (string, error) {
	if keyPath != "" {
		pub := keyPath + ".pub"
		if _, err := os.Stat(pub); err == nil {
			// Verify the .pub against the private key before trusting it.
			if _, derr := PublicKeyBytes(keyPath); derr != nil {
				return "", derr
			}
			return pub, nil
		}
		return "", fmt.Errorf("no public key file at %s — one can be derived from the private key (see PublicKeyBytes)", pub)
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
