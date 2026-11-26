package cli

// sandbox_host.go — `teploy sandbox host <server>`: install the sandbox
// engine on a server the user already manages with teploy, and run it as a
// systemd service bound to the server's loopback.
//
// The engine is a pinned GitHub release (teploy-sandbox): the binary is
// downloaded here, checked against the SHA-256 this CLI pins for it (the
// release's checksums.txt is checked too, as a second opinion), run once
// on the server to confirm the version, and only then published. It is
// never built from source on the server.
//
// What this leaves on the server is root-owned and says who put it there:
// the unit carries a marker, and a unit without the marker was set up by
// someone else and is never replaced.

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/useteploy/teploy/internal/backup"
	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/events"
	"github.com/useteploy/teploy/internal/sandbox"
	"github.com/useteploy/teploy/internal/ssh"
)

// sandboxEngineVersion is the teploy-sandbox release `sandbox host`
// installs. Bump it here, alone, to move servers to another release: a
// server that already has this version is left alone, and one with an
// older one is upgraded on its next `sandbox host`.
const sandboxEngineVersion = "v0.1.0"

// sandboxEngineSHA256 is what `sandbox host` installs, by platform: the
// SHA-256 of the release's binary, copied by hand from the release's
// checksums.txt and reviewed together with the version bump. The download
// is checked against THIS, not against a checksums.txt fetched with it: a
// binary and the checksums.txt beside it can be replaced together (the
// release, or the account that owns it), and this table cannot.
//
// TODO: v0.1.0 is not published yet. When it is, put each binary's
// SHA-256 here, read from the binaries themselves and compared with
// https://github.com/useteploy/teploy-sandbox/releases/download/v0.1.0/checksums.txt,
// before any CLI release that installs it. While a platform's entry is
// empty, `sandbox host` refuses to install on that platform rather than
// trust checksums.txt alone.
var sandboxEngineSHA256 = map[string]string{
	"linux/amd64": "",
	"linux/arm64": "",
}

const (
	sandboxEngineRepo  = "useteploy/teploy-sandbox"
	sandboxReleaseBase = "https://github.com/" + sandboxEngineRepo + "/releases/download"

	// Where the engine is on a server `sandbox host` installed it. The
	// other sandbox commands default to the same.
	sandboxDefaultAddr = "127.0.0.1:7439" // the server's loopback, never a public address
	sandboxTokenFile   = "/var/lib/teploy-sandbox/token"
	sandboxStateRoot   = "/var/lib/teploy-sandbox"
	sandboxEnginePath  = "/usr/local/bin/teploy-sandbox"
	sandboxUnitName    = "teploy-sandbox.service"
	sandboxUnitPath    = "/etc/systemd/system/" + sandboxUnitName

	// sandboxUnitMarker in a unit file says `teploy sandbox host` wrote it.
	sandboxUnitMarker = "Installed by: teploy sandbox host"
)

// What `sandbox host` did, in its --json document.
const (
	sandboxHostInstall   = "install"
	sandboxHostUpgrade   = "upgrade"
	sandboxHostStart     = "start"
	sandboxHostUnchanged = "unchanged"
)

func sandboxEngineURL(asset string) string {
	return sandboxReleaseBase + "/" + sandboxEngineVersion + "/" + asset
}

// sandboxSystemdUnit is the service `sandbox host` installs: the engine as
// root (it drives Docker and reads the host's /proc), listening on the
// server's loopback only, restarted when it fails.
func sandboxSystemdUnit() string {
	return fmt.Sprintf(`# %s (do not edit: the next run replaces this file)
[Unit]
Description=Teploy sandbox engine
Documentation=https://github.com/%s
Wants=docker.service
After=docker.service

[Service]
Type=simple
User=root
Environment=PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/snap/bin
ExecStart=%s serve --addr %s --driver docker --token-file %s --state-dir %s --cache-root %s
Restart=on-failure
RestartSec=2

[Install]
WantedBy=multi-user.target
`, sandboxUnitMarker, sandboxEngineRepo,
		sandboxEnginePath, sandboxDefaultAddr, sandboxTokenFile,
		sandboxStateRoot+"/state", sandboxStateRoot+"/cache")
}

func newSandboxHostCmd(flags *Flags, env *sandboxEnv) *cobra.Command {
	var (
		yes    bool
		driver string
	)
	cmd := &cobra.Command{
		Use:   "host <server>",
		Short: "Install and start the sandbox engine on a server",
		Long: "Install the sandbox engine (teploy-sandbox) on a server you already manage with\n" +
			"teploy, and run it as a systemd service. The server needs systemd and Docker (as\n" +
			"`teploy setup` leaves it) and root or passwordless sudo.\n\n" +
			"The engine is a pinned release: the binary is checked against the hash this CLI\n" +
			"pins for it (and the release's checksum) before it is installed, and never built\n" +
			"on the server. The service runs\n" +
			"as root, listens on the server's loopback only (never a public address), and is\n" +
			"reached by the other sandbox commands over SSH.\n\n" +
			"Running it again is safe: a server with this CLI's version is left as it is, one\n" +
			"with an older version is upgraded (which restarts the engine and destroys running\n" +
			"sandboxes; stopped ones are kept). A teploy-sandbox service this command did not\n" +
			"install is never replaced. Asks before installing or upgrading unless --yes.\n\n" +
			"Only the docker driver is installed. The vm driver is experimental and is opted\n" +
			"into per command with --driver vm on a server where it is already set up.\n\n" +
			"With --json, the envelope is {machine_interface, server, host, ok, action, version,\n" +
			"from, addr, token_file}; --json needs --yes.\n\n" +
			"Example:\n  teploy sandbox host prod",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSandboxHost(cmd.Context(), flags, env, args[0], driver, yes)
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask before installing or upgrading the engine")
	cmd.Flags().StringVar(&driver, "driver", "docker", "docker only: the vm driver is experimental and is not installed by this command")
	return cmd
}

// installSandboxEngine puts the pinned engine release on the server: the
// platform's binary and the release's checksums.txt are downloaded here
// (fetch), the binary is checked against the hash this CLI pins for it
// before anything is uploaded (and against checksums.txt after that), the
// candidate is run once on the server to confirm it is the release it
// claims, and only then is it published over whatever was there.
func installSandboxEngine(ctx context.Context, link ssh.Forwarder, fetch func(ctx context.Context, url string) ([]byte, error)) error {
	goos, goarch, err := serverPlatform(ctx, link)
	if err != nil {
		return err
	}
	if goos != "linux" {
		return fmt.Errorf("the sandbox engine has releases for linux servers only, and this server is %s", goos)
	}
	asset := fmt.Sprintf("teploy-sandbox_%s_%s", goos, goarch)
	pinned := sandboxEngineSHA256[goos+"/"+goarch]
	if pinned == "" {
		// A capability this CLI build lacks: there is nothing to check
		// the download against, so nothing is downloaded.
		return &sandboxUnavailableError{err: fmt.Errorf(
			"%w: %s on %s/%s is not pinned, so it cannot tell the real binary from a substituted one. Nothing was changed.\nThe hash is added to the CLI when that release is published: run `teploy update`, then this again",
			errSandboxUnpinned, sandboxEngineVersion, goos, goarch)}
	}
	binary, err := fetch(ctx, sandboxEngineURL(asset))
	if err != nil {
		return fmt.Errorf("teploy-sandbox %s: downloading %s: %w\n(there is no such release yet if this version has not been published; see https://github.com/%s/releases)",
			sandboxEngineVersion, asset, err, sandboxEngineRepo)
	}
	got := fmt.Sprintf("%x", sha256.Sum256(binary))
	if !strings.EqualFold(got, pinned) {
		return fmt.Errorf("%s of teploy-sandbox %s is not the binary this CLI pins: refusing to install (pinned %s, downloaded %s)", asset, sandboxEngineVersion, pinned, got)
	}
	checksums, err := fetch(ctx, sandboxEngineURL("checksums.txt"))
	if err != nil {
		return fmt.Errorf("teploy-sandbox %s: downloading checksums.txt: %w", sandboxEngineVersion, err)
	}
	published, err := checksumFor(checksums, asset)
	if err != nil {
		return fmt.Errorf("teploy-sandbox %s: %w", sandboxEngineVersion, err)
	}
	if !strings.EqualFold(got, published) {
		return fmt.Errorf("%s of teploy-sandbox %s does not match the release's checksums.txt either: refusing to install (pinned and downloaded %s, checksums.txt %s)", asset, sandboxEngineVersion, got, published)
	}
	candidate, cleanup, err := stageServerBinary(ctx, link, "teploy-sandbox", binary)
	if err != nil {
		return err
	}
	defer cleanup()
	if err := checkServerBinaryVersion(ctx, link, candidate, strings.TrimPrefix(sandboxEngineVersion, "v")); err != nil {
		return fmt.Errorf("the %s release of teploy-sandbox does not run on this server: %w", sandboxEngineVersion, err)
	}
	return publishServerBinary(ctx, link, candidate, sandboxEnginePath)
}

// installSandboxUnit writes the service unit into place, owned by root
// whoever uploaded it (a service that runs as root must not be editable
// without root), and has systemd read it again.
func installSandboxUnit(ctx context.Context, link ssh.Forwarder, sudo string) error {
	staged := "/tmp/" + sandboxUnitName
	if err := link.Upload(ctx, strings.NewReader(sandboxSystemdUnit()), staged, "0644"); err != nil {
		return fmt.Errorf("staging the engine's service: %w", err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_, _ = link.Run(cleanup, "rm -f -- "+ssh.ShellQuote(staged))
	}()
	if _, err := link.Run(ctx, fmt.Sprintf("%sinstall -o 0 -g 0 -m 0644 %s %s", sudo, ssh.ShellQuote(staged), ssh.ShellQuote(sandboxUnitPath))); err != nil {
		return fmt.Errorf("installing %s: %w", sandboxUnitPath, err)
	}
	if _, err := link.Run(ctx, sudo+"systemctl daemon-reload"); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w", err)
	}
	return nil
}

// sandboxHostPlan is what one run of `sandbox host` will do.
type sandboxHostPlan struct {
	action sandboxAction
	from   string // the version before, when there was one
}

type sandboxAction string

// rootAccess is how the command reaches root on the server: nothing for
// root, "sudo -n " for a user with passwordless sudo.
func (d *sandboxDaemon) rootAccess(ctx context.Context) (string, error) {
	if uid, err := d.link.Run(ctx, "id -u"); err == nil && strings.TrimSpace(uid) == "0" {
		return "", nil
	}
	if _, err := d.link.Run(ctx, "sudo -n true 2>/dev/null"); err == nil {
		return "sudo -n ", nil
	}
	return "", fmt.Errorf("installing the sandbox engine needs root or passwordless sudo on %s. Connect as root (--user root), or run `teploy setup %s` first", d.server, d.server)
}

// hostPreflight refuses a server the engine cannot run on, before anything
// is changed: it needs systemd and Docker, as `teploy setup` leaves them.
func (d *sandboxDaemon) hostPreflight(ctx context.Context, sudo string) error {
	if _, err := d.link.Run(ctx, "command -v systemctl"); err != nil {
		return fmt.Errorf("%s has no systemd, and the sandbox engine runs there as a systemd service. It cannot be installed on this server", d.server)
	}
	if _, err := d.link.Run(ctx, sudo+"docker info --format '{{.ServerVersion}}'"); err != nil {
		return fmt.Errorf("the sandbox engine runs sandboxes with Docker, and Docker is not usable on %s: %w\nRun `teploy setup %s` first (it installs Docker)", d.server, err, d.server)
	}
	return nil
}

// runningSandboxes is how many sandboxes are running on the engine, for the
// warning a restart needs. -1 when that could not be read.
func (d *sandboxDaemon) runningSandboxes(ctx context.Context) int {
	token, err := readSandboxToken(ctx, d.link, d.tokenFile)
	if err != nil {
		return -1
	}
	client, err := d.dialEngine(token)
	if err != nil {
		return -1
	}
	runs, err := client.List(ctx)
	if err != nil {
		return -1
	}
	running := 0
	for _, run := range runs {
		if run.State == "running" {
			running++
		}
	}
	return running
}

// planSandboxHost decides what this run does with what the server has.
// A service this command did not install is refused, and so is anything
// already at the engine's address on a server that has no engine.
func (d *sandboxDaemon) planSandboxHost(ctx context.Context, probe engineProbe) (sandboxHostPlan, error) {
	pinned := strings.TrimPrefix(sandboxEngineVersion, "v")
	if probe.unitPresent {
		if !probe.managed {
			return sandboxHostPlan{}, fmt.Errorf("teploy-sandbox is already installed on %s as %s, and `teploy sandbox host` did not install it. Nothing was changed: this command never replaces a service it did not write.\nTo use that daemon, pass --addr and --token-file to the other sandbox commands", d.server, sandboxUnitPath)
		}
		switch {
		case probe.version == pinned && probe.state == "active":
			return sandboxHostPlan{action: sandboxHostUnchanged, from: pinned}, nil
		case probe.version == pinned:
			return sandboxHostPlan{action: sandboxHostStart, from: pinned}, nil
		case releaseCompare(probe.version, pinned) > 0:
			return sandboxHostPlan{}, fmt.Errorf("%s already has teploy-sandbox %s, which is newer than the %s this CLI installs. Nothing was changed: update the CLI (`teploy update`) rather than downgrading the engine", d.server, probe.version, sandboxEngineVersion)
		default:
			return sandboxHostPlan{action: sandboxHostUpgrade, from: probe.version}, nil
		}
	}
	if probe.processRunning || probe.version != "" {
		return sandboxHostPlan{}, fmt.Errorf("teploy-sandbox is on %s but is not run by systemd (a binary at %s, or a process running), so `teploy sandbox host` did not install it. Nothing was changed.\nRemove it to have this command install and run the engine as a service, or pass --addr and --token-file for it to the other sandbox commands", d.server, sandboxEnginePath)
	}
	listener, err := d.listenerAt(ctx)
	if err != nil {
		return sandboxHostPlan{}, err
	}
	switch {
	case listener.engine:
		return sandboxHostPlan{}, fmt.Errorf("the sandbox engine (version %s) already listens at %s on %s, started outside `teploy sandbox host`. Nothing was changed: this command never replaces what it did not install.\nThe other sandbox commands can use it with --token-file for the path its token is kept in", listener.version, d.addr, d.server)
	case listener.other:
		return sandboxHostPlan{}, fmt.Errorf("something that is not the sandbox engine already listens at %s on %s. Nothing was changed: the engine this command installs listens at that address.\nMove what is there first, or install the engine by hand on another address", d.addr, d.server)
	}
	return sandboxHostPlan{action: sandboxHostInstall}, nil
}

// releaseCompare orders two versions as major.minor.patch, a -prerelease
// sorting below the same version without one: -1, 0 or 1. One that is not
// in that form ("dev") compares equal to any other, which reads as "not
// newer" where it is asked.
func releaseCompare(a, b string) int {
	aNumbers, aPre, aOK := parseRelease(a)
	bNumbers, bPre, bOK := parseRelease(b)
	if !aOK || !bOK {
		return 0
	}
	for i := range aNumbers {
		if aNumbers[i] != bNumbers[i] {
			if aNumbers[i] < bNumbers[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case aPre == bPre:
		return 0
	case aPre == "":
		return 1
	case bPre == "":
		return -1
	}
	return comparePrerelease(aPre, bPre)
}

// parseRelease is a version's three numbers and its pre-release, and
// whether it is a version at all.
func parseRelease(version string) ([3]int, string, bool) {
	numbers, prerelease, _ := strings.Cut(version, "-")
	var parts [3]int
	for i, field := range strings.Split(numbers, ".") {
		if i == 3 {
			return parts, "", false
		}
		value, err := strconv.Atoi(field)
		if err != nil || value < 0 || (len(field) > 1 && field[0] == '0') {
			return parts, "", false
		}
		parts[i] = value
	}
	return parts, prerelease, true
}

// comparePrerelease orders two pre-releases the way semver does: identifier
// by identifier (split on "."), two numbers numerically and below any
// word, two words as text, and the one that runs out first the lower.
func comparePrerelease(a, b string) int {
	aIDs, bIDs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(aIDs) && i < len(bIDs); i++ {
		aNumber, aErr := strconv.Atoi(aIDs[i])
		bNumber, bErr := strconv.Atoi(bIDs[i])
		aIsNumber, bIsNumber := aErr == nil, bErr == nil
		var lower bool
		switch {
		case aIsNumber && bIsNumber && aNumber != bNumber:
			lower = aNumber < bNumber
		case aIsNumber != bIsNumber:
			lower = aIsNumber
		case aIDs[i] != bIDs[i]:
			lower = aIDs[i] < bIDs[i]
		default:
			continue
		}
		if lower {
			return -1
		}
		return 1
	}
	switch {
	case len(aIDs) < len(bIDs):
		return -1
	case len(aIDs) > len(bIDs):
		return 1
	}
	return 0
}

// sandboxHostEnvelope is `sandbox host --yes --json`: what was done and the
// engine as it is now. version is without its v, as the engine reports it.
// An upgrade says which version it replaced.
func sandboxHostEnvelope(server, host string, plan sandboxHostPlan, version, addr, tokenFile string) sandboxHostResultDTO {
	res := sandboxHostResultDTO{
		MachineInterface: MachineInterface, Server: server, Host: host, OK: true,
		Action: string(plan.action), Version: version, Addr: addr, TokenFile: tokenFile,
	}
	if plan.action == sandboxHostUpgrade {
		res.From = plan.from // the version that was there before
	}
	return res
}

// sandboxHostEvent is what one run of `sandbox host` leaves in the server's
// history when it changed something.
func sandboxHostEvent(plan sandboxHostPlan) events.Event {
	detail := map[string]any{"action": string(plan.action), "version": strings.TrimPrefix(sandboxEngineVersion, "v")}
	if plan.action == sandboxHostUpgrade {
		detail["from"] = plan.from // the version that was there before
	}
	return events.Event{App: backup.FleetApp, Kind: events.KindSandboxHost, Outcome: events.Succeeded, Detail: detail}
}

// waitHealthy waits for the engine to answer at its address after a start,
// and says why it did not when it does not.
func (d *sandboxDaemon) waitHealthy(ctx context.Context, env *sandboxEnv) (sandbox.Health, error) {
	client, err := d.dialEngine("starting")
	if err != nil {
		return sandbox.Health{}, err
	}
	deadline := env.now().Add(env.startTimeout)
	for {
		attempt, cancel := context.WithTimeout(ctx, 5*time.Second)
		health, err := client.Health(attempt)
		cancel()
		if err == nil {
			return health, nil
		}
		if ctx.Err() != nil {
			return sandbox.Health{}, ctx.Err()
		}
		if !env.now().Before(deadline) {
			break
		}
		if err := env.sleep(ctx, env.pollInterval); err != nil {
			return sandbox.Health{}, err
		}
	}
	probe := probeSandboxEngine(ctx, d.link)
	state := probe.state
	if state == "" {
		state = "not running"
	}
	message := fmt.Sprintf("the sandbox engine's service did not answer at %s on %s within %s (its service is %s).\n\nShow why:\n  ssh %s@%s systemctl status %s\n  ssh %s@%s journalctl -u %s -n 50 --no-pager",
		d.addr, d.server, env.startTimeout, state,
		d.sshUser(), d.host, sandboxUnitName,
		d.sshUser(), d.host, sandboxUnitName)
	if log := d.serviceLog(ctx); log != "" {
		message += "\n\nLast log lines:\n" + log
	}
	return sandbox.Health{}, errors.New(message)
}

func runSandboxHost(ctx context.Context, flags *Flags, env *sandboxEnv, server, driver string, yes bool) error {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := signal.NotifyContext(ctx, os.Interrupt)
	defer cancel()
	if err := sandboxDriverFlag(driver); err != nil {
		return fmt.Errorf("--driver must be docker (got %q)", driver)
	}
	if driver == "vm" {
		return &sandboxUnavailableError{err: fmt.Errorf(
			"`teploy sandbox host` installs the docker driver only. The vm driver is experimental (it needs KVM, a ZFS pool and a golden guest image) and is not installed by this command.\nSet it up as the teploy-sandbox README describes, then pass --driver vm to the other sandbox commands to opt into it")}
	}
	if flags.JSON && !yes {
		return errors.New("--json needs --yes: the engine is never installed, upgraded or restarted on a prompt in machine mode")
	}
	host, user, key, err := config.ResolveServer(server, flags.Host, flags.User, flags.Key)
	if err != nil {
		return fmt.Errorf("resolving server %q: %w", server, err)
	}
	// Progress is for a person: under --json stdout holds only the document.
	progress := env.out
	if flags.JSON {
		progress = env.errOut
	}
	fmt.Fprintf(progress, "Connecting to %s@%s...\n", user, host)
	link, err := env.connect(ctx, host, user, key)
	if err != nil {
		return fmt.Errorf("connecting to %s: %w", server, err)
	}
	defer link.Close()
	d := &sandboxDaemon{server: server, host: host, addr: sandboxDefaultAddr, tokenFile: sandboxTokenFile, link: link}

	sudo, err := d.rootAccess(ctx)
	if err != nil {
		return err
	}
	if err := d.hostPreflight(ctx, sudo); err != nil {
		return err
	}
	probe := probeSandboxEngine(ctx, link)
	if !probe.ok {
		return fmt.Errorf("%s did not answer when asked what is installed there", server)
	}
	plan, err := d.planSandboxHost(ctx, probe)
	if err != nil {
		return err
	}

	pinned := strings.TrimPrefix(sandboxEngineVersion, "v")
	switch plan.action {
	case sandboxHostInstall, sandboxHostUpgrade:
		prompt := fmt.Sprintf("Install the sandbox engine (teploy-sandbox %s) on %s as a systemd service at %s, running as root with access to Docker?", sandboxEngineVersion, server, d.addr)
		if plan.action == sandboxHostUpgrade {
			lost := "Running sandboxes on it are destroyed; stopped ones are kept."
			if running := d.runningSandboxes(ctx); running >= 0 {
				lost = fmt.Sprintf("%d running sandbox(es) on it are destroyed; stopped ones are kept.", running)
			}
			prompt = fmt.Sprintf("Upgrade the sandbox engine on %s from %s to %s? Its service restarts, and %s", server, plan.from, sandboxEngineVersion, lost)
		}
		if !yes && !env.confirm(prompt) {
			return errSandboxAborted
		}
		fmt.Fprintf(progress, "Installing teploy-sandbox %s (%s) from %s...\n", sandboxEngineVersion, plan.action, sandboxEngineRepo)
		if err := installSandboxEngine(ctx, link, env.fetch); err != nil {
			return err
		}
		if plan.action == sandboxHostInstall {
			if _, err := link.Run(ctx, fmt.Sprintf("%sinstall -d -o 0 -g 0 -m 0700 %s", sudo, ssh.ShellQuote(sandboxStateRoot))); err != nil {
				return fmt.Errorf("creating %s: %w", sandboxStateRoot, err)
			}
		}
		fmt.Fprintln(progress, "Writing the systemd service...")
		if err := installSandboxUnit(ctx, link, sudo); err != nil {
			return err
		}
	case sandboxHostStart:
		fmt.Fprintf(progress, "The sandbox engine (%s) is installed on %s but its service is %s; starting it\n", sandboxEngineVersion, server, probe.state)
	}

	if _, err := link.Run(ctx, sudo+"systemctl enable "+sandboxUnitName); err != nil {
		return fmt.Errorf("enabling %s: %w", sandboxUnitName, err)
	}
	if plan.action != sandboxHostUnchanged {
		verb := "start"
		if plan.action == sandboxHostUpgrade {
			verb = "restart"
		}
		if _, err := link.Run(ctx, sudo+"systemctl "+verb+" "+sandboxUnitName); err != nil {
			return fmt.Errorf("systemctl %s %s: %w", verb, sandboxUnitName, err)
		}
	}
	fmt.Fprintln(progress, "Waiting for the engine to answer...")
	if _, err := d.waitHealthy(ctx, env); err != nil {
		return err
	}

	if plan.action != sandboxHostUnchanged {
		events.Record(ctx, link, sandboxHostEvent(plan))
	}
	res := sandboxHostEnvelope(server, host, plan, pinned, d.addr, d.tokenFile)
	if flags.JSON {
		return writeJSONTo(env.out, res)
	}
	switch plan.action {
	case sandboxHostInstall:
		fmt.Fprintf(env.out, "\nInstalled the sandbox engine (teploy-sandbox %s) on %s and started it at %s\n\n  teploy sandbox new --host %s --image alpine:3.20\n", sandboxEngineVersion, server, d.addr, server)
	case sandboxHostUpgrade:
		fmt.Fprintf(env.out, "\nUpgraded the sandbox engine on %s from %s to %s and restarted it\n", server, plan.from, sandboxEngineVersion)
	case sandboxHostStart:
		fmt.Fprintf(env.out, "\nStarted the sandbox engine's service on %s (%s)\n", server, sandboxEngineVersion)
	default:
		fmt.Fprintf(env.out, "\nThe sandbox engine on %s is up to date (%s) and running\n", server, sandboxEngineVersion)
	}
	return nil
}
