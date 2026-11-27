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
	"io"
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
// v0.1.0: read from the release's binaries, matched against its
// checksums.txt, and reproduced byte for byte from the tag (go1.26.11,
// the release workflow's flags) on 2026-11-27. While a platform's entry
// is empty, `sandbox host` refuses to install on that platform rather
// than trust checksums.txt alone.
var sandboxEngineSHA256 = map[string]string{
	"linux/amd64": "a871704de3ce7dcb73316a9bbb26c7aa011e6ccce1b4776e80d2a927f9ee6585",
	"linux/arm64": "fffd99f1d139a842714558062117943a0bdd3a1b83106338e4309b1997a20725",
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
	// sandboxEnginePrevPath is where an upgrade keeps the binary it
	// replaces, to put back if the new engine does not answer.
	sandboxEnginePrevPath = sandboxEnginePath + ".prev"
	sandboxUnitName       = "teploy-sandbox.service"
	sandboxUnitPath       = "/etc/systemd/system/" + sandboxUnitName

	// sandboxUnitMarker in a unit file says `teploy sandbox host` wrote it.
	sandboxUnitMarker = "Installed by: teploy sandbox host"
)

// What `sandbox host` did, in its --json document.
const (
	sandboxHostInstall   = "install"
	sandboxHostUpgrade   = "upgrade"
	sandboxHostRepair    = "repair"
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
			"on the server. The service runs as root, listens on the server's loopback only\n" +
			"(never a public address), and is reached by the other sandbox commands over SSH.\n\n" +
			"Running it again is safe. A server with this CLI's version and service file is left\n" +
			"as it is. One with an older version is upgraded, and one whose service file was\n" +
			"changed since this command wrote it has it rewritten: both restart the engine if\n" +
			"it is running. On a restart the engine, by its own rule, stops the running\n" +
			"sandboxes made with `--on-expire stop` and keeps them, destroys every other\n" +
			"running sandbox, keeps the stopped ones, and cuts off every command still running\n" +
			"in one. An upgrade keeps the binary it replaces as teploy-sandbox.prev, and puts\n" +
			"it back if the new engine does not answer. A teploy-sandbox service this command\n" +
			"did not install is never replaced, and one with systemd drop-ins is refused: they\n" +
			"change how the engine runs, whatever its service file says. Asks before anything\n" +
			"that restarts the engine, and before an install, unless --yes.\n\n" +
			"Only the docker driver is installed. The vm driver is experimental and is opted\n" +
			"into per command with --driver vm on a server where it is already set up.\n\n" +
			"With --json, the envelope is {machine_interface, server, host, ok, action, version,\n" +
			"from, addr, token_file}, action being install, upgrade, repair, start or unchanged;\n" +
			"--json needs --yes.\n\n" +
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

// fetchSandboxEngine downloads the pinned engine release for the server's
// platform and checks it here, before anything is uploaded: against the
// hash this CLI pins for it, then against the release's checksums.txt.
func fetchSandboxEngine(ctx context.Context, link ssh.Forwarder, fetch func(ctx context.Context, url string) ([]byte, error)) ([]byte, error) {
	goos, goarch, err := serverPlatform(ctx, link)
	if err != nil {
		return nil, err
	}
	if goos != "linux" {
		return nil, fmt.Errorf("the sandbox engine has releases for linux servers only, and this server is %s", goos)
	}
	asset := fmt.Sprintf("teploy-sandbox_%s_%s", goos, goarch)
	pinned := sandboxEngineSHA256[goos+"/"+goarch]
	if pinned == "" {
		// A capability this CLI build lacks: there is nothing to check
		// the download against, so nothing is downloaded.
		return nil, &sandboxUnavailableError{err: fmt.Errorf(
			"%w: %s on %s/%s is not pinned, so it cannot tell the real binary from a substituted one. Nothing was changed.\nThe hash is added to the CLI when that release is published: run `teploy update`, then this again",
			errSandboxUnpinned, sandboxEngineVersion, goos, goarch)}
	}
	binary, err := fetch(ctx, sandboxEngineURL(asset))
	if err != nil {
		return nil, fmt.Errorf("teploy-sandbox %s: downloading %s: %w\n(there is no such release yet if this version has not been published; see https://github.com/%s/releases)",
			sandboxEngineVersion, asset, err, sandboxEngineRepo)
	}
	got := fmt.Sprintf("%x", sha256.Sum256(binary))
	if !strings.EqualFold(got, pinned) {
		return nil, fmt.Errorf("%s of teploy-sandbox %s is not the binary this CLI pins: refusing to install (pinned %s, downloaded %s)", asset, sandboxEngineVersion, pinned, got)
	}
	checksums, err := fetch(ctx, sandboxEngineURL("checksums.txt"))
	if err != nil {
		return nil, fmt.Errorf("teploy-sandbox %s: downloading checksums.txt: %w", sandboxEngineVersion, err)
	}
	published, err := checksumFor(checksums, asset)
	if err != nil {
		return nil, fmt.Errorf("teploy-sandbox %s: %w", sandboxEngineVersion, err)
	}
	if !strings.EqualFold(got, published) {
		return nil, fmt.Errorf("%s of teploy-sandbox %s does not match the release's checksums.txt either: refusing to install (pinned and downloaded %s, checksums.txt %s)", asset, sandboxEngineVersion, got, published)
	}
	return binary, nil
}

// installSandboxUnit stages the service unit in dir, the run's private
// staging directory (where the binary is too), and installs it from
// there, owned by root whoever uploaded it (a service that runs as root
// must not be editable without root). It does not have systemd read its
// units again: daemonReload does, once, after the last unit was installed.
func installSandboxUnit(ctx context.Context, link ssh.Forwarder, sudo, dir, unit string) error {
	staged, err := uploadStaged(ctx, link, dir, sandboxUnitName, []byte(unit), "0644")
	if err != nil {
		return fmt.Errorf("staging the engine's service: %w", err)
	}
	if _, err := link.Run(ctx, fmt.Sprintf("%sinstall -o 0 -g 0 -m 0644 %s %s", sudo, ssh.ShellQuote(staged), ssh.ShellQuote(sandboxUnitPath))); err != nil {
		return fmt.Errorf("installing %s: %w", sandboxUnitPath, err)
	}
	return nil
}

// daemonReload has systemd read its units again, after one was installed.
func daemonReload(ctx context.Context, link ssh.Forwarder, sudo string) error {
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

// runningSandboxes is how many running sandboxes a restart of the engine
// destroys, and how many it stops and keeps: on a graceful shutdown the
// engine stops the runs made with `--on-expire stop` and destroys every
// other running one (restartCost). Both -1 when that could not be read.
func (d *sandboxDaemon) runningSandboxes(ctx context.Context) (destroyed, kept int) {
	token, err := readSandboxToken(ctx, d.link, d.tokenFile)
	if err != nil {
		return -1, -1
	}
	client, err := d.dialEngine(token)
	if err != nil {
		return -1, -1
	}
	runs, err := client.List(ctx)
	if err != nil {
		return -1, -1
	}
	for _, run := range runs {
		if run.State != "running" {
			continue
		}
		if run.OnExpire == "stop" {
			kept++
			continue
		}
		destroyed++
	}
	return destroyed, kept
}

// unitOnServer is the service file as it is on the server. ok is false when
// it could not be read: the caller cannot then tell whether it is as this
// CLI writes it.
func (d *sandboxDaemon) unitOnServer(ctx context.Context, sudo string) (unit string, ok bool) {
	out, err := d.link.Run(ctx, sudo+"cat -- "+ssh.ShellQuote(sandboxUnitPath))
	if err != nil {
		return "", false
	}
	return out, true
}

// planSandboxHost decides what this run does with what the server has.
// A service this command did not install is refused, and so is anything
// already at the engine's address on a server that has no engine.
//
// With this CLI's version installed, the service file decides: as this CLI
// writes it, the service is left alone or started; different (changed by
// hand, or written by another CLI version), it is rewritten (repair).
func (d *sandboxDaemon) planSandboxHost(ctx context.Context, probe engineProbe, unitDiffers bool) (sandboxHostPlan, error) {
	pinned := strings.TrimPrefix(sandboxEngineVersion, "v")
	if probe.unitPresent {
		if !probe.managed {
			return sandboxHostPlan{}, fmt.Errorf("teploy-sandbox is already installed on %s as %s, and `teploy sandbox host` did not install it. Nothing was changed: this command never replaces a service it did not write.\nTo use that daemon, pass --addr and --token-file to the other sandbox commands", d.server, sandboxUnitPath)
		}
		if probe.dropins {
			// What systemd runs is the service file and its drop-ins together:
			// with one there, "the service file is as this CLI writes it"
			// would say nothing about how the engine runs (its address
			// included), and a restart would run it with the drop-in in
			// effect.
			where := probe.dropinPaths
			if where == "" {
				where = sandboxUnitPath + ".d/*.conf"
			}
			return sandboxHostPlan{}, fmt.Errorf("teploy-sandbox's service on %s has drop-ins (%s), which `teploy sandbox host` neither wrote nor reads: they change how the engine runs, whatever its service file says. Nothing was changed.\nMove them away and run this again, or pass --addr and --token-file to the other sandbox commands to use the daemon as it runs", d.server, where)
		}
		switch {
		case probe.version == pinned && !unitDiffers && probe.state == "active":
			return sandboxHostPlan{action: sandboxHostUnchanged, from: pinned}, nil
		case probe.version == pinned && !unitDiffers:
			return sandboxHostPlan{action: sandboxHostStart, from: pinned}, nil
		case probe.version == pinned:
			return sandboxHostPlan{action: sandboxHostRepair, from: pinned}, nil
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
// An upgrade says which version it replaced (from); one over a missing
// binary has none to name, and the field is left out.
func sandboxHostEnvelope(server, host string, plan sandboxHostPlan, version, addr, tokenFile string) sandboxHostResultDTO {
	res := sandboxHostResultDTO{
		MachineInterface: MachineInterface, Server: server, Host: host, OK: true,
		Action: string(plan.action), Version: version, Addr: addr, TokenFile: tokenFile,
	}
	if plan.action == sandboxHostUpgrade {
		res.From = plan.from // the version that was there before, if there was one
	}
	return res
}

// sandboxHostEvent is what one run of `sandbox host` leaves in the server's
// history when it changed something.
func sandboxHostEvent(plan sandboxHostPlan) events.Event {
	detail := map[string]any{"action": string(plan.action), "version": strings.TrimPrefix(sandboxEngineVersion, "v")}
	if plan.action == sandboxHostUpgrade && plan.from != "" {
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

// sandboxHostPrompt is what a run asks before it changes the engine, and ""
// when it has nothing to ask. A run that restarts a running engine says
// what that costs (restartCost); one that only starts a stopped service,
// or rewrites the file of one that is not running, has nothing to lose.
func sandboxHostPrompt(d *sandboxDaemon, plan sandboxHostPlan, state string, destroyed, kept int) string {
	const missing = "Its service is installed there, but its binary is missing from " + sandboxEnginePath + "."
	switch plan.action {
	case sandboxHostInstall:
		return fmt.Sprintf("Install the sandbox engine (teploy-sandbox %s) on %s as a systemd service at %s, running as root with access to Docker?", sandboxEngineVersion, d.server, d.addr)
	case sandboxHostUpgrade:
		if plan.from == "" {
			if cost := restartCost(state, destroyed, kept); cost != "" {
				return fmt.Sprintf("Install the sandbox engine (teploy-sandbox %s) on %s? %s The service restarts, and %s.", sandboxEngineVersion, d.server, missing, cost)
			}
			return fmt.Sprintf("Install the sandbox engine (teploy-sandbox %s) on %s? %s The service will be started.", sandboxEngineVersion, d.server, missing)
		}
		if cost := restartCost(state, destroyed, kept); cost != "" {
			return fmt.Sprintf("Upgrade the sandbox engine on %s from %s to %s? The service restarts, and %s.", d.server, plan.from, sandboxEngineVersion, cost)
		}
		return fmt.Sprintf("Upgrade the sandbox engine on %s from %s to %s? Its service is not running; it will be started.", d.server, plan.from, sandboxEngineVersion)
	case sandboxHostRepair:
		cost := restartCost(state, destroyed, kept)
		if cost == "" {
			return "" // nothing is running: the file is rewritten and the service started
		}
		return fmt.Sprintf("Rewrite the sandbox engine's service file on %s? It was changed since `teploy sandbox host` wrote it. The service restarts, and %s.", d.server, cost)
	}
	return ""
}

// restartCost says what restarting the engine costs the sandboxes on it, as
// the end of a sentence ("The service restarts, and ..."), for a service
// that is running. "" when it is not: nothing outlives the engine, so a
// service that is not running has no sandbox to lose.
//
// It is the engine's own rule for a graceful shutdown, which a systemd
// restart is (its README, "Surviving a restart"): the running sandboxes
// made with `--on-expire stop` are stopped and kept, every other running
// sandbox is destroyed, the stopped ones stay, and every command still
// running in one is cut off. A crash stops nothing; this is not a crash.
//
// destroyed and kept are what runningSandboxes counted. When it could not
// count (-1), the rule is said without numbers rather than guessed at.
func restartCost(state string, destroyed, kept int) string {
	if state != "active" {
		return ""
	}
	var sandboxes string
	switch {
	case destroyed < 0:
		sandboxes = "running sandboxes on it are destroyed, except the ones made with `--on-expire stop`, which are stopped and kept"
	case destroyed == 0 && kept == 0:
		return "no sandbox is lost (none is running on it)"
	case kept == 0:
		sandboxes = fmt.Sprintf("%d running sandbox(es) on it are destroyed", destroyed)
	case destroyed == 0:
		sandboxes = fmt.Sprintf("%d running sandbox(es) on it, the ones made with `--on-expire stop`, are stopped and kept", kept)
	default:
		sandboxes = fmt.Sprintf("%d running sandbox(es) on it are destroyed and %d, the ones made with `--on-expire stop`, are stopped and kept", destroyed, kept)
	}
	return sandboxes + "; stopped ones are kept, and every command still running in one is cut off"
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
	// The service file as it is, when there is one. It is rewritten only if
	// it differs from what this CLI writes (a re-run of the same version
	// changes nothing), and what it was is kept to put back if the rewrite
	// is what leaves the engine not answering. One that cannot be read
	// counts as different: it is rewritten.
	unitBefore, unitReadable := "", false
	if probe.unitPresent {
		unitBefore, unitReadable = d.unitOnServer(ctx, sudo)
	}
	unitDiffers := !unitReadable || strings.TrimSpace(unitBefore) != strings.TrimSpace(sandboxSystemdUnit())
	plan, err := d.planSandboxHost(ctx, probe, unitDiffers)
	if err != nil {
		return err
	}

	pinned := strings.TrimPrefix(sandboxEngineVersion, "v")
	// What a restart costs, for the prompt: only a running engine has
	// sandboxes on it to count.
	destroyed, kept := -1, -1
	if probe.state == "active" {
		destroyed, kept = d.runningSandboxes(ctx)
	}
	if prompt := sandboxHostPrompt(d, plan, probe.state, destroyed, kept); prompt != "" && !yes && !env.confirm(prompt) {
		return errSandboxAborted
	}

	// stage is this run's private staging directory on the server: the
	// binary's candidate and the service file are put there, and nowhere
	// else. It is removed when the run ends, however it ends.
	var stage string
	cleanup := func() {}
	defer func() { cleanup() }()
	// What to put back if the engine does not answer after this run: the
	// version whose binary an upgrade kept as teploy-sandbox.prev, and the
	// service file this run rewrote.
	previousBinary, previousUnit := "", ""
	switch plan.action {
	case sandboxHostInstall, sandboxHostUpgrade:
		fmt.Fprintf(progress, "Installing teploy-sandbox %s (%s) from %s...\n", sandboxEngineVersion, plan.action, sandboxEngineRepo)
		binary, err := fetchSandboxEngine(ctx, link, env.fetch)
		if err != nil {
			return err
		}
		if stage, cleanup, err = createStagingDir(ctx, link); err != nil {
			return err
		}
		candidate, err := uploadStaged(ctx, link, stage, "teploy-sandbox", binary, "0700")
		if err != nil {
			return err
		}
		// The candidate runs once on the server before it is published:
		// a binary that does not report this release is never installed.
		if err := checkServerBinaryVersion(ctx, link, candidate, pinned); err != nil {
			return fmt.Errorf("the %s release of teploy-sandbox does not run on this server: %w", sandboxEngineVersion, err)
		}
		if plan.action == sandboxHostUpgrade && probe.version != "" {
			fmt.Fprintf(progress, "Keeping the current binary as %s, to put back if the new engine does not answer...\n", sandboxEnginePrevPath)
			if _, err := link.Run(ctx, fmt.Sprintf("%scp -p -- %s %s", sudo, ssh.ShellQuote(sandboxEnginePath), ssh.ShellQuote(sandboxEnginePrevPath))); err != nil {
				return fmt.Errorf("keeping the current engine as %s: %w", sandboxEnginePrevPath, err)
			}
			previousBinary = plan.from
		}
		if err := publishServerBinary(ctx, link, candidate, sandboxEnginePath); err != nil {
			return err
		}
		if plan.action == sandboxHostInstall {
			if _, err := link.Run(ctx, fmt.Sprintf("%sinstall -d -o 0 -g 0 -m 0700 %s", sudo, ssh.ShellQuote(sandboxStateRoot))); err != nil {
				return fmt.Errorf("creating %s: %w", sandboxStateRoot, err)
			}
		}
	case sandboxHostStart:
		fmt.Fprintf(progress, "The sandbox engine (%s) is installed on %s but its service is %s; starting it\n", sandboxEngineVersion, server, probe.state)
	}

	if unitDiffers {
		if stage == "" {
			if stage, cleanup, err = createStagingDir(ctx, link); err != nil {
				return err
			}
		}
		fmt.Fprintln(progress, "Writing the systemd service...")
		if err := installSandboxUnit(ctx, link, sudo, stage, sandboxSystemdUnit()); err != nil {
			return err
		}
		if err := daemonReload(ctx, link, sudo); err != nil {
			return err
		}
		if probe.unitPresent {
			previousUnit = unitBefore // there was one; this run replaced it
		}
	}

	if _, err := link.Run(ctx, sudo+"systemctl enable "+sandboxUnitName); err != nil {
		return fmt.Errorf("enabling %s: %w", sandboxUnitName, err)
	}
	// The service is started when it is not running, and restarted when
	// this run replaced what it runs (a new binary, or another service
	// file). A re-run that changed nothing touches it.
	start, restarted := "start", false
	switch {
	case plan.action == sandboxHostUpgrade, plan.action == sandboxHostRepair && probe.state == "active":
		start, restarted = "restart", true
	case plan.action == sandboxHostUnchanged:
		start = ""
	}
	if start != "" {
		if _, err := link.Run(ctx, sudo+"systemctl "+start+" "+sandboxUnitName); err != nil {
			return fmt.Errorf("systemctl %s %s: %w", start, sandboxUnitName, err)
		}
	}
	fmt.Fprintln(progress, "Waiting for the engine to answer...")
	if _, err := d.waitHealthy(ctx, env); err != nil {
		if previousBinary != "" || previousUnit != "" {
			// The way back does not hang on the wait that failed, or on a
			// Ctrl-C that ended it: it has time of its own.
			wayBack, cancelWayBack := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
			defer cancelWayBack()
			return d.putBack(wayBack, env, progress, sudo, stage, previousBinary, previousUnit, probe.state, err)
		}
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
		if plan.from == "" {
			fmt.Fprintf(env.out, "\nUpgraded the sandbox engine on %s to %s (its binary was missing) and restarted it\n", server, sandboxEngineVersion)
			break
		}
		if previousBinary == "" {
			fmt.Fprintf(env.out, "\nUpgraded the sandbox engine on %s from %s to %s and restarted it\n", server, plan.from, sandboxEngineVersion)
			break
		}
		fmt.Fprintf(env.out, "\nUpgraded the sandbox engine on %s from %s to %s and restarted it. The previous binary is kept as %s\n", server, plan.from, sandboxEngineVersion, sandboxEnginePrevPath)
	case sandboxHostRepair:
		if restarted {
			fmt.Fprintf(env.out, "\nRewrote the sandbox engine's service file on %s (it had been changed) and restarted the engine\n", server)
		} else {
			fmt.Fprintf(env.out, "\nRewrote the sandbox engine's service file on %s (it had been changed) and started the engine\n", server)
		}
	case sandboxHostStart:
		fmt.Fprintf(env.out, "\nStarted the sandbox engine's service on %s (%s)\n", server, sandboxEngineVersion)
	default:
		fmt.Fprintf(env.out, "\nThe sandbox engine on %s is up to date (%s) and running\n", server, sandboxEngineVersion)
	}
	return nil
}

// putBack is what `sandbox host` does when the engine does not answer after
// the run changed it, and there is something to put back: the binary an
// upgrade kept (as teploy-sandbox.prev) and the service file the run
// rewrote. Each goes back where it was; if the service was active before
// the run it is restarted and waited for, and if it was not running before
// the failed start is stopped and the service is left stopped. The error
// returned says all of it: why the new one was not accepted, and what
// became of what was there.
func (d *sandboxDaemon) putBack(ctx context.Context, env *sandboxEnv, progress io.Writer, sudo, stage, previousBinary, previousUnit, priorState string, cause error) error {
	// The engine that was there: the version whose binary was kept, or,
	// for a rewritten service file, this CLI's own version.
	there := strings.TrimPrefix(sandboxEngineVersion, "v")
	if previousBinary != "" {
		there = previousBinary
	}
	what := "the service file"
	switch {
	case previousBinary != "" && previousUnit != "":
		what = "the previous binary (" + previousBinary + ") and the service file"
	case previousBinary != "":
		what = "the previous binary (" + previousBinary + ")"
	}
	wasActive := priorState == "active"
	priorLabel := priorState
	if priorLabel == "" {
		priorLabel = "not running"
	}
	if wasActive {
		fmt.Fprintf(progress, "\nThe engine did not answer with what this run changed. Putting back %s and starting the service again...\n", what)
	} else {
		fmt.Fprintf(progress, "\nThe engine did not answer with what this run changed. Putting back %s without starting the service (it was %s before)...\n", what, priorLabel)
	}
	// If the service was not running before this run, stop the failed start
	// before restoring files so systemd's Restart=on-failure cannot launch
	// the restored binary behind our back.
	var stopErr error
	if !wasActive {
		_, stopErr = d.link.Run(ctx, sudo+"systemctl stop "+sandboxUnitName)
	}
	// Both are tried whatever the other did, and the error says what each
	// one came to: an old binary under a new service file (or the other way
	// round) is not "put back", and the person reading it must know which
	// half to finish by hand.
	var failed []string
	var done []string
	if previousBinary != "" {
		if _, err := d.link.Run(ctx, fmt.Sprintf("%smv -f -- %s %s", sudo, ssh.ShellQuote(sandboxEnginePrevPath), ssh.ShellQuote(sandboxEnginePath))); err != nil {
			failed = append(failed, fmt.Sprintf("The previous engine (%s) could not be put back: %v.\nIt is still at %s: put it back with `mv -f %s %s` on %s.",
				previousBinary, err, sandboxEnginePrevPath, sandboxEnginePrevPath, sandboxEnginePath, d.server))
		} else {
			done = append(done, "The previous engine ("+previousBinary+") was put back.")
		}
	}
	if previousUnit != "" {
		if err := installSandboxUnit(ctx, d.link, sudo, stage, previousUnit); err != nil {
			failed = append(failed, fmt.Sprintf("The service file that was there could not be put back: %v.\n%s still holds the one this run wrote.", err, sandboxUnitPath))
		} else if err := daemonReload(ctx, d.link, sudo); err != nil {
			failed = append(failed, fmt.Sprintf("The service file that was there was put back, but systemd did not reload it: %v.", err))
		} else {
			done = append(done, "The service file that was there was put back.")
		}
	}
	if len(failed) > 0 {
		if stopErr != nil {
			failed = append(failed, fmt.Sprintf("Stopping the failed start did not succeed: %v.", stopErr))
		}
		msg := strings.Join(append(done, failed...), "\n")
		if wasActive {
			return fmt.Errorf("%v\n\n%s\nFinish that by hand, then `systemctl daemon-reload` and `systemctl restart %s` on %s. The service was not restarted", cause, msg, sandboxUnitName, d.server)
		}
		return fmt.Errorf("%v\n\n%s\nFinish that by hand, then `systemctl daemon-reload` on %s. The service was not started (it was %s before this run)", cause, msg, d.server, priorLabel)
	}
	if !wasActive {
		if stopErr != nil {
			return fmt.Errorf("%v\n\nThe engine that was there (%s) was put back, and its service was not started (it was %s before this run), but stopping the failed start did not succeed: %w", cause, there, priorLabel, stopErr)
		}
		return fmt.Errorf("%v\n\nThe engine that was there (%s) was put back, and its service was left stopped (it was %s before this run). Nothing was upgraded", cause, there, priorLabel)
	}
	if _, err := d.link.Run(ctx, sudo+"systemctl restart "+sandboxUnitName); err != nil {
		return fmt.Errorf("%v\n\nIt was put back, but its service could not be restarted: %w", cause, err)
	}
	if _, err := d.waitHealthy(ctx, env); err != nil {
		return fmt.Errorf("%v\n\nThe engine that was there (%s) was put back and restarted, but it does not answer either:\n%v", cause, there, err)
	}
	fmt.Fprintln(progress, "It answers again.")
	return fmt.Errorf("%v\n\nThe engine that was there (%s) was put back and restarted, and it answers again. Nothing was upgraded", cause, there)
}
