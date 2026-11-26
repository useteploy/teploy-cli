package cli

// sandbox.go — `teploy sandbox`: sandboxes on a server the user already
// manages with teploy (early access, self-hosted; hosted sandboxes on
// Teploy Cloud are not this).
//
// The engine is the teploy-sandbox daemon, the one teploy-ship runs its
// agent sandboxes on, spoken to the way teploy-ship speaks to it: its HTTP
// API with a bearer token (docs/wire-contract.md in that repository). What
// is added here is only the road. The daemon is bound to the server's
// loopback and is reached through the SSH connection the CLI already
// opens: a forwarded channel to 127.0.0.1:7439 (ssh.Forwarder). There is
// no second way to talk to it, and nothing it serves is on a public
// address.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/sandbox"
	"github.com/useteploy/teploy/internal/ssh"
	"golang.org/x/term"
)

// The sandbox error classes a machine consumer tells apart. Each is wrapped
// by a typed error that carries what happened.
var (
	// errSandboxEngineUnavailable covers what a user fixes on the server:
	// the engine is not installed, its service is not running, or the
	// driver asked for is not there.
	errSandboxEngineUnavailable = errors.New("the sandbox engine is not available on this server")
	// errSandboxOutcomeUnknown marks a create or fork whose answer was
	// lost, or an exec whose stream died: the effect may or may not have
	// happened.
	errSandboxOutcomeUnknown = errors.New("the sandbox request's outcome is unknown")
	// errSandboxUnpinned marks a CLI build that has no pinned hash for the
	// engine release `host` would install. It is of the engine-unavailable
	// class too (a capability this binary lacks, error-envelope code
	// `unsupported`), and never a state of the server.
	errSandboxUnpinned = errors.New("this CLI build has no pinned hash for the sandbox engine")
	// errSandboxAborted is a confirmation that was not given.
	errSandboxAborted = errors.New("aborted: nothing was changed")
	// errSandboxWrongState is a request about a sandbox that is in the
	// wrong state for it (an exec in a stopped one).
	errSandboxWrongState = errors.New("the sandbox is in the wrong state for this request")
)

// sandboxWrongStateError says the sandbox's state and the way out, and
// stays of the engine's 409 class for a machine consumer.
type sandboxWrongStateError struct{ err error }

func (e *sandboxWrongStateError) Error() string { return e.err.Error() }
func (e *sandboxWrongStateError) Unwrap() error { return e.err }
func (e *sandboxWrongStateError) Is(target error) bool {
	return target == errSandboxWrongState
}

type sandboxUnavailableError struct{ err error }

func (e *sandboxUnavailableError) Error() string { return e.err.Error() }
func (e *sandboxUnavailableError) Unwrap() error { return e.err }
func (e *sandboxUnavailableError) Is(target error) bool {
	return target == errSandboxEngineUnavailable
}

type sandboxUnknownOutcomeError struct{ err error }

func (e *sandboxUnknownOutcomeError) Error() string { return e.err.Error() }
func (e *sandboxUnknownOutcomeError) Unwrap() error { return e.err }
func (e *sandboxUnknownOutcomeError) Is(target error) bool {
	return target == errSandboxOutcomeUnknown
}

// sandboxEnv carries the side effects of the `teploy sandbox` commands so
// tests can replace the SSH connection, the terminal, the network and the
// clock's waits.
type sandboxEnv struct {
	out, errOut     io.Writer
	stdinIsTerminal func() bool
	confirm         func(prompt string) bool
	// connect opens the SSH connection the commands use.
	connect func(ctx context.Context, host, user, keyPath string) (ssh.Forwarder, error)
	sleep   func(ctx context.Context, d time.Duration) error
	// fetch downloads a release asset (a seam: tests have no network).
	fetch func(ctx context.Context, url string) ([]byte, error)
	// newRunLabel mints the label the CLI puts on a run it makes (a seam:
	// a generated fixture needs a fixed one).
	newRunLabel  func() (string, error)
	pollInterval time.Duration
	startTimeout time.Duration
	now          func() time.Time
}

func defaultSandboxEnv() *sandboxEnv {
	return &sandboxEnv{
		out:             os.Stdout,
		errOut:          os.Stderr,
		stdinIsTerminal: func() bool { return term.IsTerminal(int(os.Stdin.Fd())) },
		confirm:         func(prompt string) bool { return confirm(os.Stdout, prompt) },
		connect: func(ctx context.Context, host, user, keyPath string) (ssh.Forwarder, error) {
			return ssh.Connect(ctx, ssh.ConnectConfig{Host: host, User: user, KeyPath: keyPath})
		},
		sleep:        sleepCtx,
		fetch:        downloadToBytes,
		newRunLabel:  newSandboxRunLabel,
		pollInterval: time.Second,
		startTimeout: 30 * time.Second,
		now:          time.Now,
	}
}

func newSandboxCmd(flags *Flags) *cobra.Command {
	return newSandboxCmdWithEnv(flags, defaultSandboxEnv())
}

func newSandboxCmdWithEnv(flags *Flags, env *sandboxEnv) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sandbox",
		Short: "Sandboxes on your own server (early access, self-hosted)",
		Long: "Run isolated sandboxes on a server you already manage with teploy.\n\n" +
			"Early access, self-hosted: the engine (teploy-sandbox) runs on your server as a\n" +
			"systemd service bound to the server's loopback, and these commands reach it over\n" +
			"the SSH connection. Nothing it serves is on a public address. Sandboxes hosted on\n" +
			"Teploy Cloud are not part of this.\n\n" +
			"Start with `teploy sandbox host <server>`. Every other command names the server\n" +
			"with --host (a servers.yml name or a host). See docs/sandbox.md.",
	}
	cmd.AddCommand(
		newSandboxHostCmd(flags, env),
		newSandboxNewCmd(flags, env),
		newSandboxLSCmd(flags, env),
		newSandboxExecCmd(flags, env),
		newSandboxStopCmd(flags, env),
		newSandboxResumeCmd(flags, env),
		newSandboxForkCmd(flags, env),
		newSandboxRMCmd(flags, env),
		newSandboxCPCmd(flags, env),
	)
	return cmd
}

// sandboxDaemonOptions are the flags that say where the engine is on the
// server. The defaults are what `teploy sandbox host` installs; a daemon
// set up another way may differ, and the flags say how.
type sandboxDaemonOptions struct {
	addr      string
	tokenFile string
	// allowRemoteAddr lets addr name an address off the server's loopback
	// (--allow-remote-addr). Off it, the token is never sent there.
	allowRemoteAddr bool
}

func (o sandboxDaemonOptions) withDefaults() sandboxDaemonOptions {
	if strings.TrimSpace(o.addr) == "" {
		o.addr = sandboxDefaultAddr
	}
	if strings.TrimSpace(o.tokenFile) == "" {
		o.tokenFile = sandboxTokenFile
	}
	return o
}

func bindSandboxDaemonFlags(cmd *cobra.Command, o *sandboxDaemonOptions) {
	cmd.Flags().StringVar(&o.addr, "addr", "", "the engine's host:port on the server (default "+sandboxDefaultAddr+")")
	cmd.Flags().StringVar(&o.tokenFile, "token-file", "", "the engine's bearer token file on the server (default "+sandboxTokenFile+")")
	cmd.Flags().BoolVar(&o.allowRemoteAddr, "allow-remote-addr", false, "let --addr name an address off the server's loopback (another host, or a name). The engine speaks plain HTTP, so the bearer token travels to it unencrypted: only on a network you trust")
}

// sandboxDaemon is the engine on one server: the SSH connection, and a
// client for the engine at its address there.
type sandboxDaemon struct {
	server    string // as the user named it
	host      string // the SSH host
	addr      string
	tokenFile string
	// allowRemoteAddr: addr may be off the server's loopback, because the
	// user passed --allow-remote-addr.
	allowRemoteAddr bool
	link            ssh.Forwarder
	client          *sandbox.Client
	health          sandbox.Health
}

// Close releases the SSH connection.
func (d *sandboxDaemon) Close() { _ = d.link.Close() }

// sshUser is the account to name in a command the user runs by hand.
func (d *sandboxDaemon) sshUser() string {
	if user := d.link.User(); user != "" {
		return user
	}
	return "root"
}

// openSandboxDaemon connects to the server and opens the engine on it.
// The caller owns the result and must Close it.
//
// When the engine cannot be reached, the error says which of the things
// that can be wrong on the server is wrong, after looking: not installed
// (with the command that installs it), installed but its service not
// running (with the last of its log), or running but not where it was
// asked for.
func openSandboxDaemon(ctx context.Context, env *sandboxEnv, flags *Flags, o sandboxDaemonOptions) (*sandboxDaemon, error) {
	if flags.Host == "" {
		return nil, errors.New("--host is required: the server (a servers.yml name or a host) whose sandboxes to use, e.g. `teploy sandbox ls --host prod`")
	}
	host, user, key, err := config.ResolveServer(flags.Host, flags.Host, flags.User, flags.Key)
	if err != nil {
		return nil, fmt.Errorf("resolving server %q: %w", flags.Host, err)
	}
	o = o.withDefaults()
	// The engine speaks plain HTTP, so the bearer token would travel to
	// whatever --addr names, unencrypted. It is only ever sent to the
	// server's loopback unless the user opted out of that with
	// --allow-remote-addr.
	parseAddr := sandbox.ParseAddr
	if o.allowRemoteAddr {
		parseAddr = sandbox.ParseAddrRemote
	}
	addr, _, err := parseAddr(o.addr)
	if err != nil {
		if errors.Is(err, sandbox.ErrOffLoopback) {
			return nil, fmt.Errorf("%w.\nTo use an engine on another address, pass --allow-remote-addr (the token then travels to it unencrypted, so only on a network you trust)", err)
		}
		return nil, err
	}
	link, err := env.connect(ctx, host, user, key)
	if err != nil {
		return nil, fmt.Errorf("connecting to %s: %w", flags.Host, err)
	}
	d := &sandboxDaemon{server: flags.Host, host: host, addr: addr, tokenFile: o.tokenFile, allowRemoteAddr: o.allowRemoteAddr, link: link}
	if err := d.open(ctx); err != nil {
		_ = link.Close()
		return nil, err
	}
	return d, nil
}

// open reads the engine's token on the server and checks that the engine
// answers at its address.
func (d *sandboxDaemon) open(ctx context.Context) error {
	token, err := readSandboxToken(ctx, d.link, d.tokenFile)
	if err != nil {
		return d.unavailable(ctx, err)
	}
	d.client, err = d.dialEngine(token)
	if err != nil {
		return err
	}
	health, err := d.client.Health(ctx)
	if err != nil {
		return d.unavailable(ctx, err)
	}
	d.health = health
	return nil
}

// errSandboxTokenMissing is the engine's token file not being at the path
// asked for, as told apart from one that cannot be read.
var errSandboxTokenMissing = errors.New("the engine's token file is not there")

// sandboxTokenMarker answers a token file that is not there.
const sandboxTokenMarker = "TEPLOY_SANDBOX_TOKEN_ABSENT"

// sandboxTokenError is a token file that is there, or may be, and could not
// be read: the account on the server lacks the right.
type sandboxTokenError struct{ err error }

func (e *sandboxTokenError) Error() string { return e.err.Error() }
func (e *sandboxTokenError) Unwrap() error { return e.err }

// readSandboxToken reads the engine's bearer token from the server. It
// travels only inside the SSH connection, and then only in an
// Authorization header: it is never printed, logged, or put in an error.
func readSandboxToken(ctx context.Context, link ssh.Forwarder, tokenFile string) (string, error) {
	out, err := link.Run(ctx, fmt.Sprintf(
		`f=%s; if [ ! -e "$f" ]; then printf '%s\n'; elif [ "$(id -u)" = 0 ]; then cat -- "$f"; else sudo -n cat -- "$f"; fi`,
		ssh.ShellQuote(tokenFile), sandboxTokenMarker))
	if err != nil {
		return "", &sandboxTokenError{err: fmt.Errorf("reading the engine's token from %s needs root or passwordless sudo: %w", tokenFile, err)}
	}
	token := strings.TrimSpace(out)
	if token == sandboxTokenMarker || token == "" {
		return "", errSandboxTokenMissing
	}
	return token, nil
}

// engineProbe is what the server says about the engine when asked.
type engineProbe struct {
	ok             bool // the server answered the probe at all
	systemd        bool
	unitPresent    bool
	managed        bool   // the unit was written by `teploy sandbox host`
	state          string // the service's systemd state: active, inactive, failed...
	version        string // "" when the binary is not there
	processRunning bool
	// dropins: the service has drop-ins (teploy-sandbox.service.d/*.conf),
	// which change how systemd runs it whatever its own file says.
	dropins bool
}

// sandboxProbeCommand is the one command that reads the probe. It always
// exits 0 and answers one `key=value` per line, and changes nothing.
func sandboxProbeCommand() string {
	return fmt.Sprintf(`if command -v systemctl >/dev/null 2>&1; then echo systemd=present; else echo systemd=absent; fi; `+
		`if systemctl cat %[1]s >/dev/null 2>&1; then echo unit=present; else echo unit=absent; fi; `+
		`if grep -qs '%[3]s' %[2]s; then echo managed=yes; else echo managed=no; fi; `+
		`echo "state=$(systemctl is-active %[1]s 2>/dev/null || true)"; `+
		`if [ -x %[4]s ]; then echo "version=$(%[4]s version 2>/dev/null || echo unknown)"; else echo version=absent; fi; `+
		`if pgrep -x teploy-sandbox >/dev/null 2>&1; then echo process=running; else echo process=none; fi; `+
		`if ls %5s/*.conf >/dev/null 2>&1; then echo dropins=present; else echo dropins=none; fi`,
		sandboxUnitName, sandboxUnitPath, sandboxUnitMarker, sandboxEnginePath, sandboxUnitPath+".d")
}

// probeSandboxEngine asks the server what is installed and running.
func probeSandboxEngine(ctx context.Context, link ssh.Forwarder) engineProbe {
	out, err := link.Run(ctx, sandboxProbeCommand())
	if err != nil {
		return engineProbe{}
	}
	probe := engineProbe{ok: true}
	for _, line := range strings.Split(out, "\n") {
		key, value, found := strings.Cut(strings.TrimSpace(line), "=")
		if !found {
			continue
		}
		switch key {
		case "systemd":
			probe.systemd = value == "present"
		case "unit":
			probe.unitPresent = value == "present"
		case "managed":
			probe.managed = value == "yes"
		case "state":
			probe.state = value
		case "version":
			if value != "absent" {
				probe.version = value
			}
		case "process":
			probe.processRunning = value == "running"
		case "dropins":
			probe.dropins = value == "present"
		}
	}
	return probe
}

// serviceLog is the last lines of the engine's journal, or "" when the
// server does not let this user read them. It is only ever shown to a user
// as the reason a service is not running.
func (d *sandboxDaemon) serviceLog(ctx context.Context) string {
	sudo := ""
	if d.sshUser() != "root" {
		sudo = "sudo -n "
	}
	logCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := d.link.Run(logCtx, sudo+"journalctl -u "+sandboxUnitName+" -n 15 --no-pager 2>&1 || true")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// dialEngine is a client for the engine at its address on the server, with
// the given token: every request travels on a connection forwarded through
// the SSH connection.
func (d *sandboxDaemon) dialEngine(token string) (*sandbox.Client, error) {
	dial := func(ctx context.Context, network, _ string) (net.Conn, error) {
		// Always the engine's address: a request cannot be steered to
		// another through its URL.
		return d.link.ForwardDial(ctx, d.addr)
	}
	// sandbox.New takes the loopback only; NewRemote is the opt-in.
	if d.allowRemoteAddr {
		return sandbox.NewRemote(d.addr, token, dial)
	}
	return sandbox.New(d.addr, token, dial)
}

// sandboxListener is what answers at the engine's address on the server.
type sandboxListener struct {
	engine  bool   // a sandbox engine
	version string // its version, when one answers
	other   bool   // something that is not one
}

// listenerAt asks what answers at the engine's address on the server. An
// error means the question could not be asked (sshd forbids the forward).
func (d *sandboxDaemon) listenerAt(ctx context.Context) (sandboxListener, error) {
	client, err := d.dialEngine("probe")
	if err != nil {
		return sandboxListener{}, err
	}
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	health, err := client.Health(dialCtx)
	switch {
	case err == nil:
		return sandboxListener{engine: true, version: health.Version}, nil
	case errors.Is(err, sandbox.ErrNotEngine):
		return sandboxListener{other: true}, nil
	}
	if _, prohibited := ssh.ForwardRefused(err); prohibited {
		return sandboxListener{}, fmt.Errorf("the SSH server on %s does not allow port forwarding, and the sandbox engine is reached through an SSH tunnel to %s.\nAllow it for this user (AllowTcpForwarding in sshd_config), or ask whoever administers the server to", d.server, d.addr)
	}
	if refused, _ := ssh.ForwardRefused(err); refused {
		return sandboxListener{}, nil // nothing listens
	}
	return sandboxListener{}, fmt.Errorf("could not tell whether anything listens at %s on %s: %w", d.addr, d.server, err)
}

// unavailable turns a failure to reach the engine into the error that says
// what is wrong on the server. The server's own account of what is
// installed decides; what failed on the way (the token, the tunnel) only
// tells the cases apart that the account leaves open.
func (d *sandboxDaemon) unavailable(ctx context.Context, cause error) error {
	if _, prohibited := ssh.ForwardRefused(cause); prohibited {
		return d.unavailableError(fmt.Errorf(
			"the SSH server on %s does not allow port forwarding, and the sandbox engine is reached through an SSH tunnel to %s.\nAllow it for this user (AllowTcpForwarding in sshd_config), or ask whoever administers the server to",
			d.server, d.addr))
	}
	probe := probeSandboxEngine(ctx, d.link)
	if !probe.ok {
		return d.unavailableError(fmt.Errorf(
			"the sandbox engine at %s on %s could not be reached (%v), and the server would not say what is installed there",
			d.addr, d.server, cause))
	}
	if !probe.systemd {
		return d.unavailableError(fmt.Errorf(
			"%s has no systemd, and the sandbox engine runs there as a systemd service. `teploy sandbox host` cannot install it on this server",
			d.server))
	}
	if probe.unitPresent && probe.state != "active" {
		return d.notRunning(ctx, probe)
	}
	var tokenErr *sandboxTokenError
	if errors.As(cause, &tokenErr) {
		// Whatever is installed, the token is what could not be read.
		return d.unavailableError(tokenErr.err)
	}
	if probe.unitPresent {
		if errors.Is(cause, errSandboxTokenMissing) {
			return d.unavailableError(fmt.Errorf(
				"the sandbox engine's token is not at %s on %s.\nIf this daemon was not installed by `teploy sandbox host`, pass --token-file with the path it was started with",
				d.tokenFile, d.server))
		}
		// The service is active, so the engine is up somewhere.
		return d.unavailableError(fmt.Errorf(
			"the sandbox engine's service is active on %s but nothing of its own answers at %s.\nIf this daemon was not installed by `teploy sandbox host`, pass --addr with the address it listens on",
			d.server, d.addr))
	}
	if probe.processRunning || probe.version != "" {
		return d.unavailableError(fmt.Errorf(
			"teploy-sandbox is on %s but is not run by systemd, so `teploy sandbox host` did not install it and these commands cannot find its service.\nTo use it, pass --addr and --token-file for the address and token file it was started with",
			d.server))
	}
	// No sandbox engine is installed as far as the server says. Something
	// may still be at the address: ask, unless what failed already said.
	listener := sandboxListener{other: true}
	if !errors.Is(cause, sandbox.ErrNotEngine) {
		found, err := d.listenerAt(ctx)
		if err != nil {
			return d.unavailableError(err)
		}
		listener = found
	}
	switch {
	case listener.engine:
		return d.unavailableError(fmt.Errorf(
			"the sandbox engine (version %s) already answers at %s on %s, and `teploy sandbox host` did not install it.\nTo use it, pass --token-file with the path its token is kept in (and --addr, if it is not at the address this command asked for)",
			listener.version, d.addr, d.server))
	case listener.other:
		return d.unavailableError(fmt.Errorf(
			"something that is not the sandbox engine answers at %s on %s, and no sandbox engine is installed there.\nInstall the engine with `teploy sandbox host %s` on a server where %s is free, or point --addr at the engine",
			d.addr, d.server, d.server, d.addr))
	}
	return d.unavailableError(fmt.Errorf(
		"the sandbox engine (teploy-sandbox) is not installed on %s\n\nInstall it with:\n  teploy sandbox host %s",
		d.server, d.server))
}

// notRunning is an engine whose service is installed and is not active,
// with the last of its log when the server lets it be read.
func (d *sandboxDaemon) notRunning(ctx context.Context, probe engineProbe) error {
	state := probe.state
	if state == "" {
		state = "not running"
	}
	message := fmt.Sprintf("the sandbox engine is installed on %s but its service is %s.\n\nShow why:\n  ssh %s@%s systemctl status %s\n  ssh %s@%s journalctl -u %s -n 50 --no-pager\nStart it with:\n  ssh %s@%s systemctl start %s",
		d.server, state,
		d.sshUser(), d.host, sandboxUnitName,
		d.sshUser(), d.host, sandboxUnitName,
		d.sshUser(), d.host, sandboxUnitName)
	if log := d.serviceLog(ctx); log != "" {
		message += "\n\nLast log lines:\n" + log
	}
	return d.unavailableError(errors.New(message))
}

func (d *sandboxDaemon) unavailableError(err error) error {
	return &sandboxUnavailableError{err: err}
}

// explain adds what a user can act on to an error from the engine: a token
// it refuses, a sandbox that is not there.
func (d *sandboxDaemon) explain(err error) error {
	if err == nil {
		return nil
	}
	if sandbox.NotSent(err) || errors.Is(err, errSandboxEngineUnavailable) || errors.Is(err, errSandboxOutcomeUnknown) {
		return err
	}
	if sandbox.IsUnauthorized(err) {
		return d.unavailableError(fmt.Errorf(
			"the sandbox engine at %s on %s refused the token in %s.\nIf this daemon was not installed by `teploy sandbox host`, it may keep its token elsewhere: pass --token-file with that path",
			d.addr, d.server, d.tokenFile))
	}
	if sandbox.IsNotFound(err) {
		return fmt.Errorf("no sandbox with that id on %s (list them with `teploy sandbox ls --host %s`): %w", d.server, d.server, err)
	}
	var apiErr *sandbox.APIError
	if !errors.As(err, &apiErr) {
		// Not the engine's answer: the connection to it failed.
		return fmt.Errorf("talking to the sandbox engine on %s at %s: %w", d.server, d.addr, err)
	}
	return fmt.Errorf("%w (on %s)", err, d.server) // the engine's own sentence, with the server named
}

// explainRun is explain for a request about one run, and says what a 409
// means for that run.
func (d *sandboxDaemon) explainRun(ctx context.Context, id string, err error) error {
	if err == nil || !sandbox.IsConflict(err) {
		return d.explain(err)
	}
	current, findErr := d.client.Find(ctx, id)
	if findErr != nil {
		return d.explain(err)
	}
	switch current.State {
	case "stopped":
		return &sandboxWrongStateError{err: fmt.Errorf("sandbox %s on %s is stopped. Resume it first:\n  teploy sandbox resume %s --host %s", id, d.server, id, d.server)}
	case "running":
		return fmt.Errorf("sandbox %s on %s is busy (an exec, a write or a capture is in flight, or it holds a lease): %w", id, d.server, err)
	}
	return d.explain(err)
}

// sandboxBase64Since is the first teploy-sandbox release whose exec
// answers in base64 when asked (the wire contract's `outputEncoding`).
const sandboxBase64Since = "0.1.0"

// sendsBytes says whether the engine can be asked for a command's output
// byte for byte: it is a release at or after the first that documents
// `outputEncoding: base64`. Anything else, a build that reports no version
// included, is asked for text, the default, which it certainly speaks: a
// daemon that ignored the field would have its text read as base64. With
// text, the engine replaces a byte that is not valid UTF-8 with U+FFFD.
func (d *sandboxDaemon) sendsBytes() bool {
	if _, _, ok := parseRelease(d.health.Version); !ok {
		return false
	}
	return releaseCompare(d.health.Version, sandboxBase64Since) >= 0
}

// checkDriver reads the engine's driver and refuses the request when the
// one asked for is not the one the engine runs. The CLI targets the docker
// driver; the vm driver is experimental and is opted into with --driver vm.
func (d *sandboxDaemon) checkDriver(ctx context.Context, requested string) error {
	capacity, err := d.client.Capacity(ctx)
	if err != nil {
		return d.explain(err)
	}
	switch {
	case requested == capacity.Driver:
		return nil
	case requested == "vm":
		return d.unavailableError(fmt.Errorf(
			"the vm driver is not available on %s: its sandbox engine runs the %s driver.\nThe vm driver is experimental (it needs KVM, a ZFS pool and a golden guest image) and `teploy sandbox host` does not install it. See the teploy-sandbox README, \"VM driver\"",
			d.server, capacity.Driver))
	case capacity.Driver == "vm":
		return d.unavailableError(fmt.Errorf(
			"the sandbox engine on %s runs the experimental vm driver, and this command asked for the docker driver.\nPass --driver vm to use it: with the vm driver every run is root inside the guest and warm caches are unavailable",
			d.server))
	default:
		return d.unavailableError(fmt.Errorf(
			"the sandbox engine on %s reports a driver this CLI does not know (%q); it targets the docker driver",
			d.server, capacity.Driver))
	}
}

// unknownOutcome reports a create or fork whose answer was lost. The label
// the CLI put on the run is how it is found again.
func (d *sandboxDaemon) unknownOutcome(verb, label string, err error) error {
	return &sandboxUnknownOutcomeError{err: fmt.Errorf(
		"the request to %s a sandbox on %s did not finish, so whether it happened is unknown (%v).\nList the sandboxes with `teploy sandbox ls --host %s` and look for the label %s=%s before trying again",
		verb, d.server, err, d.server, sandboxRunLabel, label)}
}

// sandboxRunLabel is the one label the CLI puts on every run it creates.
// A create is never sent twice, so after a lost answer this is how the run
// it may have made is told from any other.
const sandboxRunLabel = "teploy.cli-run"

// maxSandboxUserLabels leaves room for the CLI's own among the engine's 16.
const maxSandboxUserLabels = 15

func newSandboxRunLabel() (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generating a run label: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// parseKeyValue reads repeated KEY=VALUE flags into a map.
func parseKeyValue(pairs []string, flag string) (map[string]string, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	values := make(map[string]string, len(pairs))
	for _, pair := range pairs {
		key, value, found := strings.Cut(pair, "=")
		if !found || key == "" || strings.ContainsAny(pair, "\x00\n") {
			return nil, fmt.Errorf("%s %q is not KEY=VALUE (a key, an equals sign, a value, on one line)", flag, pair)
		}
		if _, duplicate := values[key]; duplicate {
			return nil, fmt.Errorf("%s %q is given twice", flag, key)
		}
		values[key] = value
	}
	return values, nil
}

// wholeSeconds turns a --ttl or --timeout flag into whole seconds for the
// engine. Zero means the flag was not given and the engine's default
// applies. A max of 0 is no upper bound.
func wholeSeconds(d time.Duration, flag string, max time.Duration) (int, error) {
	switch {
	case d == 0:
		return 0, nil
	case d < time.Second:
		return 0, fmt.Errorf("%s %s is under a second; the engine takes whole seconds", flag, d)
	case max > 0 && d > max:
		return 0, fmt.Errorf("%s %s is over the engine's limit of %s", flag, d, max)
	}
	return int(d / time.Second), nil
}

// sandboxDriverFlag validates --driver: the docker driver this CLI targets,
// or vm to opt into the experimental one.
func sandboxDriverFlag(driver string) error {
	switch driver {
	case "docker", "vm":
		return nil
	}
	return fmt.Errorf("--driver must be docker, or vm to opt into the experimental vm driver (got %q)", driver)
}

func sortedRunLabels(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, key := range keys {
		parts[i] = key + "=" + labels[key]
	}
	return strings.Join(parts, ",")
}

func sandboxTime(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// ls

func newSandboxLSCmd(flags *Flags, env *sandboxEnv) *cobra.Command {
	var o sandboxDaemonOptions
	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List the sandboxes on a server",
		Long: "List every sandbox on the server's engine, running and stopped, oldest first.\n\n" +
			"With --json, the envelope is {machine_interface, server, host, driver, runs[]}.\n\n" +
			"Example:\n  teploy sandbox ls --host prod",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSandboxList(cmd.Context(), flags, env, o)
		},
	}
	bindSandboxDaemonFlags(cmd, &o)
	return cmd
}

func runSandboxList(ctx context.Context, flags *Flags, env *sandboxEnv, o sandboxDaemonOptions) error {
	if ctx == nil {
		ctx = context.Background()
	}
	d, err := openSandboxDaemon(ctx, env, flags, o)
	if err != nil {
		return err
	}
	defer d.Close()
	runs, err := d.client.List(ctx)
	if err != nil {
		return d.explain(err)
	}
	capacity, err := d.client.Capacity(ctx)
	if err != nil {
		return d.explain(err)
	}
	// The engine promises no order of its own.
	sort.SliceStable(runs, func(i, j int) bool {
		if !runs[i].CreatedAt.Equal(runs[j].CreatedAt) {
			return runs[i].CreatedAt.Before(runs[j].CreatedAt)
		}
		return runs[i].ID < runs[j].ID
	})
	if flags.JSON {
		return writeJSONTo(env.out, sandboxRunsEnvelope(d, capacity.Driver, runs))
	}
	if len(runs) == 0 {
		fmt.Fprintf(env.out, "No sandboxes on %s. Create one with `teploy sandbox new --host %s --image <image>`.\n", d.server, d.server)
		return nil
	}
	fmt.Fprintf(env.out, "Sandboxes on %s (engine %s, %s driver):\n", d.server, d.health.Version, capacity.Driver)
	tw := tabwriter.NewWriter(env.out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTATE\tIMAGE\tEXPIRES\tLABELS")
	for _, run := range runs {
		deadline := "expires " + sandboxTime(run.ExpiresAt)
		if run.State == "stopped" && run.RetainUntil != nil {
			deadline = "kept until " + sandboxTime(*run.RetainUntil)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", run.ID, run.State, run.Image, deadline, sortedRunLabels(run.Labels))
	}
	return tw.Flush()
}

// new

type sandboxNewOptions struct {
	daemon   sandboxDaemonOptions
	image    string
	ttl      time.Duration
	network  string
	env      []string
	cpus     float64
	memoryMB int
	pids     int
	user     string
	labels   []string
	driver   string
	onExpire string
}

func newSandboxNewCmd(flags *Flags, env *sandboxEnv) *cobra.Command {
	var o sandboxNewOptions
	cmd := &cobra.Command{
		Use:   "new",
		Short: "Create a sandbox on a server",
		Long: "Create a sandbox from a Docker image and print its id. It gets its own\n" +
			"filesystem and network namespace, lives for --ttl, and is destroyed there\n" +
			"(or stopped, with --on-expire stop).\n\n" +
			"With --json, the envelope is {machine_interface, server, host, run}.\n\n" +
			"Examples:\n" +
			"  teploy sandbox new --host prod --image alpine:3.20\n" +
			"  teploy sandbox new --host prod --image node:22 --ttl 2h --network allowlist --env CI=true",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSandboxNew(cmd.Context(), flags, env, o)
		},
	}
	bindSandboxDaemonFlags(cmd, &o.daemon)
	cmd.Flags().StringVar(&o.image, "image", "", "Docker image the sandbox starts from (required)")
	cmd.Flags().DurationVar(&o.ttl, "ttl", 0, "how long the sandbox lives before the engine destroys (or stops) it (default 30m, at most 24h)")
	cmd.Flags().StringVar(&o.network, "network", "", "network: none (default), allowlist or open")
	cmd.Flags().StringArrayVar(&o.env, "env", nil, "KEY=VALUE in the sandbox's environment (repeatable)")
	cmd.Flags().Float64Var(&o.cpus, "cpus", 0, "CPUs the sandbox may use (default 1)")
	cmd.Flags().IntVar(&o.memoryMB, "memory-mb", 0, "memory in MiB (default 1024)")
	cmd.Flags().IntVar(&o.pids, "pids", 0, "processes the sandbox may have (default 256)")
	cmd.Flags().StringVar(&o.user, "user", "", "run as root instead of the image's sandbox user: default (or empty) or root")
	cmd.Flags().StringArrayVar(&o.labels, "label", nil, "KEY=VALUE label on the run (repeatable, at most 15)")
	cmd.Flags().StringVar(&o.driver, "driver", "docker", "docker (default), or vm to opt into the experimental vm driver")
	cmd.Flags().StringVar(&o.onExpire, "on-expire", "", "at the ttl: destroy the sandbox (default) or stop it and keep its filesystem")
	return cmd
}

// request is the create body, validated before anything reaches the engine.
// It also returns the label the CLI put on the run.
func (o sandboxNewOptions) request(mintLabel func() (string, error)) (sandbox.CreateRequest, string, error) {
	var req sandbox.CreateRequest
	if strings.TrimSpace(o.image) == "" {
		return req, "", errors.New("--image is required: the Docker image the sandbox starts from, e.g. --image alpine:3.20")
	}
	req.Image = strings.TrimSpace(o.image)
	if err := sandboxDriverFlag(o.driver); err != nil {
		return req, "", err
	}
	ttlSec, err := wholeSeconds(o.ttl, "--ttl", sandbox.MaxTTL)
	if err != nil {
		return req, "", err
	}
	req.TTLSec = ttlSec
	switch o.network {
	case "", "none", "allowlist", "open":
		req.Network = o.network
	default:
		return req, "", fmt.Errorf("--network must be none, allowlist or open (got %q)", o.network)
	}
	if req.Env, err = parseKeyValue(o.env, "--env"); err != nil {
		return req, "", err
	}
	if o.cpus < 0 || o.memoryMB < 0 || o.pids < 0 {
		return req, "", errors.New("--cpus, --memory-mb and --pids must not be negative")
	}
	if o.cpus > 0 || o.memoryMB > 0 || o.pids > 0 {
		req.Limits = &sandbox.Limits{CPUs: o.cpus, MemoryMB: o.memoryMB, Pids: o.pids}
	}
	switch o.user {
	case "", "default":
	case "root":
		req.User = "root"
	default:
		return req, "", fmt.Errorf("--user must be default or root (got %q)", o.user)
	}
	if len(o.labels) > maxSandboxUserLabels {
		return req, "", fmt.Errorf("at most %d --label: the engine takes 16 and the CLI adds one of its own", maxSandboxUserLabels)
	}
	if req.Labels, err = parseKeyValue(o.labels, "--label"); err != nil {
		return req, "", err
	}
	label, err := mintLabel()
	if err != nil {
		return req, "", err
	}
	if req.Labels == nil {
		req.Labels = map[string]string{}
	}
	req.Labels[sandboxRunLabel] = label
	switch o.onExpire {
	case "", "destroy":
	case "stop":
		req.OnExpire = "stop"
	default:
		return req, "", fmt.Errorf("--on-expire must be destroy or stop (got %q)", o.onExpire)
	}
	return req, label, nil
}

func runSandboxNew(ctx context.Context, flags *Flags, env *sandboxEnv, o sandboxNewOptions) error {
	if ctx == nil {
		ctx = context.Background()
	}
	req, label, err := o.request(env.newRunLabel)
	if err != nil {
		return err
	}
	d, err := openSandboxDaemon(ctx, env, flags, o.daemon)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.checkDriver(ctx, o.driver); err != nil {
		return err
	}
	run, err := d.client.Create(ctx, req)
	if err != nil {
		if sandbox.OutcomeKnown(err) {
			return d.explain(err)
		}
		return d.unknownOutcome("create", label, err)
	}
	if flags.JSON {
		return writeJSONTo(env.out, sandboxRunEnvelope(d, run))
	}
	fmt.Fprintf(env.out, "Created %s on %s (%s, expires %s)\n\n  teploy sandbox exec %s --host %s -- <command>\n",
		run.ID, d.server, run.Image, sandboxTime(run.ExpiresAt), run.ID, d.server)
	return nil
}
