// `teploy doctor` — the C09 diagnostic slice: diagnose local toolchain,
// SSH, Docker, registry, Caddy, disk, machine-interface compatibility and
// repair debt WITHOUT causing deployment. Every remote command is
// read-only (tests pin the executor's call log against an allowlist); a
// doctor run that fails checks never mutates server state.
//
// Human progress goes to stdout as a table; --json emits the versioned
// machine envelope {machine_interface, checks, summary}. Exit codes are
// stable: 0 when no check fails, 1 when any check fails, and never 2 —
// that code stays `drift --exit-code`'s CI signal (X02 D10).
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/deploy"
	"github.com/useteploy/teploy/internal/ssh"
)

// Check results — the closed enum carried in both output modes.
const (
	doctorOK   = "ok"
	doctorWarn = "warn"
	doctorFail = "fail"
)

// Disk headroom thresholds for the root filesystem (bytes / used-%).
// Below 2 GiB a deploy fails mid-flight (image layers, backups, attempt
// artifacts all land on /); below 10 GiB or above 85% used the next
// deploy is at risk.
const (
	doctorDiskFailAvailable = 2 << 30
	doctorDiskWarnAvailable = 10 << 30
	doctorDiskWarnUsedPct   = 85
)

// serverTeployBinaryPath is where `teploy autodeploy` installs the
// server-side teploy binary (autodeploy.go's deployment target).
const serverTeployBinaryPath = "/deployments/.bin/teploy"

// doctorCaddyAdminProbe reaches the Caddy admin API inside the caddy
// container over the SSH executor — the same probe machine.go's server
// status uses, so doctor and `server status` observe the same surface.
const doctorCaddyAdminProbe = "docker exec caddy sh -c 'wget -qO- http://localhost:2019/config/apps/http 2>/dev/null || curl -sf http://localhost:2019/config/apps/http'"

// doctorCheck is one diagnostic result. Name is a stable identifier
// automation codes against; Remediation is the operator's next action
// ("" when ok). All four keys are ALWAYS present in --json — a stable
// shape means a consumer never probes for optional keys.
type doctorCheck struct {
	Name        string `json:"name"`
	Result      string `json:"result"` // ok | warn | fail
	Detail      string `json:"detail"`
	Remediation string `json:"remediation"`
}

// doctorSummary counts each result class over the whole report.
type doctorSummary struct {
	OK   int `json:"ok"`
	Warn int `json:"warn"`
	Fail int `json:"fail"`
}

// doctorReport is the `doctor --json` envelope (MI-1 additive surface).
type doctorReport struct {
	MachineInterface int           `json:"machine_interface"`
	Checks           []doctorCheck `json:"checks"`
	Summary          doctorSummary `json:"summary"`
}

// doctorDeps carries the injectable surfaces: the local teploy version
// (compatibility comparison), the SSH connect (the existing connect path,
// whose errors already carry the key/auth/known_hosts diagnostics), and
// the local git probe.
type doctorDeps struct {
	localVersion string
	connect      func(ctx context.Context, host, user, keyPath string) (ssh.Executor, error)
	gitVersion   func(ctx context.Context) (string, error)
}

func defaultDoctorDeps(version string) doctorDeps {
	return doctorDeps{
		localVersion: version,
		connect: func(ctx context.Context, host, user, keyPath string) (ssh.Executor, error) {
			return ssh.Connect(ctx, ssh.ConnectConfig{Host: host, User: user, KeyPath: keyPath})
		},
		gitVersion: doctorGitVersion,
	}
}

func newDoctorCmd(flags *Flags, version string) *cobra.Command {
	var serverName string
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Diagnose toolchain, SSH, Docker, registry, Caddy, disk, compatibility and repair debt (read-only)",
		Long: "Runs every diagnostic read-only and never mutates server state — a doctor run\n" +
			"that fails checks deploys nothing.\n\n" +
			"Checks: git, config (teploy.yml or Compose grammar), SSH connectivity to the\n" +
			"app's server (or --server), remote Docker, remote disk headroom, registry\n" +
			"reachability for the configured image (auth failures distinguished from\n" +
			"unreachable), the Caddy admin API (caddy ingress only), teploy version\n" +
			"compatibility with the server's teploy binary if present, and outstanding\n" +
			"release-record repair debt.\n\n" +
			"Exit codes: 0 when no check fails, 1 when any check fails, 2 never (that\n" +
			"code stays drift --exit-code's CI signal).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDoctor(defaultDoctorDeps(version), flags, serverName, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&serverName, "server", "", "diagnose this server (name or host) instead of the app's configured one")
	return cmd
}

func runDoctor(deps doctorDeps, flags *Flags, serverName string, out io.Writer) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	appCfg, cfgErr := config.LoadApp(".")
	report, executor := doctorRun(ctx, deps, flags, serverName, appCfg, cfgErr)
	if executor != nil {
		// Closed before any os.Exit below — defers would be skipped.
		executor.Close()
	}
	if err := writeDoctorReport(out, report, flags.JSON); err != nil {
		return err
	}
	// The report is the successful OUTPUT of this command; the exit code
	// reports the diagnosis, not command failure. 1 (never 2 — drift's).
	if code := doctorExitCode(report); code != 0 {
		os.Exit(code)
	}
	return nil
}

// doctorRun executes every check and returns the report plus the open
// executor (nil when unreachable) — the caller owns closing it. Skips are
// honest about their class: a check skipped because its subject is not in
// play (host/external ingress, build-from-source image, optional server
// binary, no app identity) is ok; a check skipped because its input is
// unavailable (SSH down, config unreadable) is fail.
func doctorRun(ctx context.Context, deps doctorDeps, flags *Flags, serverName string, appCfg *config.AppConfig, cfgErr error) (doctorReport, ssh.Executor) {
	report := doctorReport{MachineInterface: MachineInterface, Checks: []doctorCheck{}}
	report.Checks = append(report.Checks, doctorGitCheck(ctx, deps))
	report.Checks = append(report.Checks, doctorConfigCheck(".", appCfg, cfgErr))

	var executor ssh.Executor
	host, user, key, hasTarget, err := doctorResolveTarget(flags, serverName, appCfg)
	switch {
	case err != nil:
		report.Checks = append(report.Checks, doctorCheck{
			Name: "ssh", Result: doctorFail,
			Detail: err.Error(), Remediation: "fix the server reference (see detail)",
		})
	case !hasTarget:
		report.Checks = append(report.Checks, doctorCheck{
			Name: "ssh", Result: doctorFail, Detail: "no server to diagnose",
			Remediation: "set 'server' in teploy.yml, or pass --server or --host",
		})
	default:
		ex, connectErr := deps.connect(ctx, host, user, key)
		if connectErr != nil {
			report.Checks = append(report.Checks, doctorCheck{
				Name: "ssh", Result: doctorFail,
				// The connect path's error already carries the key/auth
				// diagnostics, including the known_hosts algorithm naming.
				Detail: connectErr.Error(), Remediation: doctorSSHRemediation(connectErr),
			})
		} else {
			executor = ex
			report.Checks = append(report.Checks, doctorCheck{
				Name: "ssh", Result: doctorOK,
				Detail: fmt.Sprintf("connected to %s@%s", user, host),
			})
		}
	}

	skipFail := func(name, what, remediation string) {
		report.Checks = append(report.Checks, doctorCheck{
			Name: name, Result: doctorFail,
			Detail:      "skipped — " + what,
			Remediation: remediation,
		})
	}

	if executor == nil {
		const sshDown = "restore SSH connectivity (see the ssh check above), then re-run teploy doctor"
		for _, name := range []string{"docker", "disk", "registry", "caddy", "compatibility", "repair-debt"} {
			skipFail(name, "SSH unreachable", sshDown)
		}
	} else {
		report.Checks = append(report.Checks, doctorDockerCheck(ctx, executor))
		report.Checks = append(report.Checks, doctorDiskCheck(ctx, executor))
		if appCfg == nil {
			const cfgBroken = "fix the config (see the config check above), then re-run teploy doctor"
			skipFail("registry", "config unreadable — no image ref", cfgBroken)
			skipFail("caddy", "config unreadable — ingress unknown", cfgBroken)
		} else {
			report.Checks = append(report.Checks, doctorRegistryCheck(ctx, executor, appCfg))
			report.Checks = append(report.Checks, doctorCaddyCheck(ctx, executor, appCfg))
		}
		report.Checks = append(report.Checks, doctorCompatCheck(ctx, deps, executor))
		report.Checks = append(report.Checks, doctorRepairDebtCheck(ctx, executor, appCfg))
	}

	report.summarize()
	return report, executor
}

// doctorExitCode pins the documented semantics: 0 with no failing check
// (warnings included), 1 with any fail — and never 2, which stays
// `drift --exit-code`'s CI signal.
func doctorExitCode(report doctorReport) int {
	if report.Summary.Fail > 0 {
		return 1
	}
	return 0
}

func (r *doctorReport) summarize() {
	r.Summary = doctorSummary{}
	for _, c := range r.Checks {
		switch c.Result {
		case doctorOK:
			r.Summary.OK++
		case doctorWarn:
			r.Summary.Warn++
		case doctorFail:
			r.Summary.Fail++
		}
	}
}

func writeDoctorReport(out io.Writer, report doctorReport, jsonOutput bool) error {
	if jsonOutput {
		return json.NewEncoder(out).Encode(report)
	}
	for _, c := range report.Checks {
		fmt.Fprintf(out, "%-15s %-5s %s\n", c.Name, c.Result, c.Detail)
		if c.Remediation != "" {
			fmt.Fprintf(out, "%-15s %-5s fix:  %s\n", "", "", c.Remediation)
		}
	}
	fmt.Fprintf(out, "\nSummary: %d ok, %d warn, %d fail\n", report.Summary.OK, report.Summary.Warn, report.Summary.Fail)
	return nil
}

// doctorResolveTarget picks the diagnosis target: --server wins, then the
// app's configured server, then --host. ok=false means no target could be
// determined (the ssh check reports it); err is a failed resolution.
func doctorResolveTarget(flags *Flags, serverName string, appCfg *config.AppConfig) (host, user, key string, ok bool, err error) {
	switch {
	case serverName != "":
		host, user, key, err = config.ResolveServer(serverName, flags.Host, flags.User, flags.Key)
	case appCfg != nil:
		name := appCfg.Server
		if name == "" && len(appCfg.Servers) > 0 {
			name = appCfg.Servers[0]
		}
		if name == "" {
			return "", "", "", false, nil
		}
		host, user, key, err = config.ResolveServer(name, flags.Host, flags.User, flags.Key)
		if err == nil {
			// Honor teploy.yml's user: the same way validate and deploy
			// connect, so doctor diagnoses as the deploying account.
			user = config.EffectiveUser(user, flags.User, appCfg.User)
		}
	case flags.Host != "":
		host, user, key, err = config.ResolveServer(flags.Host, flags.Host, flags.User, flags.Key)
	}
	if err != nil || host == "" {
		return "", "", "", false, err
	}
	return host, user, key, true, nil
}

func doctorGitCheck(ctx context.Context, deps doctorDeps) doctorCheck {
	version, err := deps.gitVersion(ctx)
	if err != nil {
		// Warn, not fail: git-less boxes deploy prebuilt images fine.
		return doctorCheck{
			Name: "git", Result: doctorWarn, Detail: err.Error(),
			Remediation: "install git — provenance records, git template installs, and autodeploy checkouts need it",
		}
	}
	return doctorCheck{Name: "git", Result: doctorOK, Detail: version}
}

// doctorGitVersion probes the local toolchain. exec.LookPath first so a
// missing git is a clean "not found" rather than a shell error.
func doctorGitVersion(ctx context.Context) (string, error) {
	if _, err := exec.LookPath("git"); err != nil {
		return "", errors.New("git not found on PATH")
	}
	out, err := exec.CommandContext(ctx, "git", "--version").Output()
	if err != nil {
		return "", fmt.Errorf("running git --version: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// doctorConfigCheck surfaces the config grammar through the same loader
// deploy uses — the "new grammar errors" (Compose field contracts, health
// modes, publish specs, overlay rules) arrive verbatim in Detail.
func doctorConfigCheck(dir string, appCfg *config.AppConfig, cfgErr error) doctorCheck {
	if cfgErr != nil {
		remediation := "fix the configuration error above — the message names the file and the grammar problem"
		if errors.Is(cfgErr, config.ErrNoConfig) {
			remediation = "create a teploy.yml (teploy init) or a docker-compose file in this directory"
		}
		return doctorCheck{Name: "config", Result: doctorFail, Detail: cfgErr.Error(), Remediation: remediation}
	}
	return doctorCheck{Name: "config", Result: doctorOK, Detail: doctorConfigSource(dir) + " parsed and validated"}
}

// doctorConfigSource names the file LoadApp would load from dir, for the
// config check's detail line.
func doctorConfigSource(dir string) string {
	for _, name := range []string{"teploy.yml", "teploy.yaml", "teploy.toml"} {
		if _, err := os.Stat(dir + string(os.PathSeparator) + name); err == nil {
			return name
		}
	}
	for _, name := range []string{"docker-compose.yml", "docker-compose.yaml", "compose.yml", "compose.yaml"} {
		if _, err := os.Stat(dir + string(os.PathSeparator) + name); err == nil {
			return name + " (imported)"
		}
	}
	return "config"
}

// doctorSSHRemediation maps the connect path's self-describing failures
// to the next action. Display-only classification; verification always
// stays with the failing connection itself.
func doctorSSHRemediation(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "authentication failed"), strings.Contains(msg, "unable to authenticate"):
		return "fix SSH authentication: --user (root SSH is disabled on most distros), --key for a specific identity, or --password"
	case strings.Contains(msg, "host key mismatch"):
		return "scan every host-key algorithm (ssh-keyscan without -t), or — only after verifying the host legitimately changed — re-enroll it"
	case strings.Contains(msg, "no SSH keys found"):
		return "provide --key, set TEPLOY_SSH_KEY, or place a key at ~/.ssh/id_ed25519"
	default:
		return "resolve the SSH failure above — the message names the specific key, auth, or host-key problem — then re-run teploy doctor"
	}
}

// doctorDockerCheck proves the DAEMON answers (server version via the
// docker CLI on the target), not just that the binary exists.
func doctorDockerCheck(ctx context.Context, exec ssh.Executor) doctorCheck {
	out, err := exec.Run(ctx, "docker version --format '{{.Server.Version}}'")
	if err != nil {
		return doctorCheck{
			Name: "docker", Result: doctorFail, Detail: err.Error(),
			Remediation: "install Docker on the server (teploy setup provisions it) and check docker.sock permissions for the SSH user",
		}
	}
	version := strings.TrimSpace(out)
	if version == "" {
		return doctorCheck{
			Name: "docker", Result: doctorFail,
			Detail:      "docker answered but reported no server version",
			Remediation: "check the docker service on the server (systemctl status docker)",
		}
	}
	return doctorCheck{Name: "docker", Result: doctorOK, Detail: fmt.Sprintf("Docker server %s reachable", version)}
}

// doctorDiskCheck reports root-filesystem headroom via df over the
// executor — read-only, POSIX -P framing, bytes (-B1) so thresholds are
// exact.
func doctorDiskCheck(ctx context.Context, exec ssh.Executor) doctorCheck {
	out, err := exec.Run(ctx, "df -B1 -P /")
	if err != nil {
		return doctorCheck{
			Name: "disk", Result: doctorFail, Detail: err.Error(),
			Remediation: "check the df output on the server — it failed outright",
		}
	}
	avail, pct, parsed := parseDoctorDisk(out)
	if !parsed {
		return doctorCheck{
			Name: "disk", Result: doctorFail,
			Detail:      fmt.Sprintf("could not parse df output: %q", strings.TrimSpace(out)),
			Remediation: "inspect `df -B1 -P /` on the server",
		}
	}
	detail := fmt.Sprintf("%.1f GiB available on / (%d%% used)", float64(avail)/(1<<30), pct)
	switch {
	case avail < doctorDiskFailAvailable:
		return doctorCheck{
			Name: "disk", Result: doctorFail, Detail: detail,
			Remediation: "free disk space on / (docker system prune, teploy releases/preview prune, old backups) — deploys fail mid-flight below 2 GiB",
		}
	case avail < doctorDiskWarnAvailable || pct >= doctorDiskWarnUsedPct:
		return doctorCheck{
			Name: "disk", Result: doctorWarn, Detail: detail,
			Remediation: "free disk space on / before the next deploy (docker system prune, teploy releases prune)",
		}
	}
	return doctorCheck{Name: "disk", Result: doctorOK, Detail: detail}
}

// parseDoctorDisk reads `df -B1 -P` output: the LAST line's fields are
// fs, 1-blocks, used, available, capacity%, mounted-on (spaces in the
// mount point stay right of the numerics).
func parseDoctorDisk(raw string) (avail uint64, usedPercent int, ok bool) {
	lines := strings.Split(strings.TrimSpace(raw), "\n")
	if len(lines) < 2 {
		return 0, 0, false
	}
	fields := strings.Fields(lines[len(lines)-1])
	if len(fields) < 6 {
		return 0, 0, false
	}
	avail, err := strconv.ParseUint(fields[3], 10, 64)
	if err != nil {
		return 0, 0, false
	}
	usedPercent, err = strconv.Atoi(strings.TrimSuffix(fields[4], "%"))
	if err != nil {
		return 0, 0, false
	}
	return avail, usedPercent, true
}

// doctorRegistryCheck asks the SERVER's docker to resolve the configured
// image ref's manifest — a pure registry query (docker manifest inspect
// touches no local image state, unlike a pull). Auth failures are
// distinguished from unreachable registries so the remediation names the
// actual fix.
func doctorRegistryCheck(ctx context.Context, exec ssh.Executor, appCfg *config.AppConfig) doctorCheck {
	if appCfg.Image == "" {
		return doctorCheck{
			Name: "registry", Result: doctorOK,
			Detail: "no registry image ref — the image is built from source at deploy time",
		}
	}
	res := ssh.RunDetailed(ctx, exec, "docker manifest inspect "+ssh.ShellQuote(appCfg.Image))
	if res.Failed() {
		switch classifyRegistryFailure(res) {
		case "auth":
			return doctorCheck{
				Name: "registry", Result: doctorFail,
				Detail:      registryFailureDetail(res),
				Remediation: "store credentials on the server: teploy registry login <registry> — deploys pull as the server's docker",
			}
		case "missing":
			return doctorCheck{
				Name: "registry", Result: doctorFail,
				Detail:      registryFailureDetail(res),
				Remediation: "push the image to the registry, or correct the image ref in teploy.yml",
			}
		default:
			return doctorCheck{
				Name: "registry", Result: doctorFail,
				Detail:      registryFailureDetail(res),
				Remediation: "check the network path from the server to the registry (DNS, firewall, proxy)",
			}
		}
	}
	return doctorCheck{Name: "registry", Result: doctorOK, Detail: fmt.Sprintf("registry reachable for %s", appCfg.Image)}
}

// registryFailureDetail renders the structured failure for display: the
// command's own stderr when it produced some, the transport error
// otherwise.
func registryFailureDetail(res ssh.Result) string {
	if d := res.ExitErrorText(); d != "" {
		return d
	}
	return fmt.Sprintf("manifest inspect failed (exit status %d)", res.ExitCode)
}

// classifyRegistryFailure buckets a manifest-inspect failure into auth /
// missing / unreachable. Docker's CLI exits 1 for every failure class,
// so the exit code cannot distinguish them — classification reads the
// tool's own stderr (via the structured Result, NOT the folded
// executor error text, which mixes in teploy's own wrapper words).
// Display classification only; the detail always carries the underlying
// output verbatim.
func classifyRegistryFailure(res ssh.Result) string {
	msg := strings.ToLower(res.ExitErrorText())
	switch {
	case strings.Contains(msg, "unauthorized"), strings.Contains(msg, "authentication required"), strings.Contains(msg, "denied"):
		return "auth"
	case strings.Contains(msg, "no such manifest"), strings.Contains(msg, "manifest unknown"), strings.Contains(msg, "not found"):
		return "missing"
	default:
		return "unreachable"
	}
}

// doctorCaddyCheck probes the admin API inside the caddy container —
// only when teploy manages ingress. host/external ingress never routes
// through teploy's Caddy, so those are ok-skips, not failures.
func doctorCaddyCheck(ctx context.Context, exec ssh.Executor, appCfg *config.AppConfig) doctorCheck {
	switch appCfg.Ingress {
	case config.IngressExternal:
		return doctorCheck{
			Name: "caddy", Result: doctorOK,
			Detail: "ingress external — Caddy is not in the request path",
		}
	case config.IngressHost:
		return doctorCheck{
			Name: "caddy", Result: doctorOK,
			Detail: "ingress host — the app publishes directly on its bind port",
		}
	}
	if _, err := exec.Run(ctx, doctorCaddyAdminProbe); err != nil {
		return doctorCheck{
			Name: "caddy", Result: doctorFail, Detail: err.Error(),
			Remediation: "check the caddy container on the server (docker ps --filter name=^caddy$) — teploy setup provisions and self-heals it",
		}
	}
	return doctorCheck{Name: "caddy", Result: doctorOK, Detail: "Caddy admin API responding (ingress caddy)"}
}

// doctorCompatCheck compares this binary's version against the server's
// teploy binary when one is installed (autodeploy's /deployments/.bin).
// Absence is ok — the server binary is optional infrastructure; skew is
// a warning because scheduled redeploys and webhook builds run on it.
func doctorCompatCheck(ctx context.Context, deps doctorDeps, exec ssh.Executor) doctorCheck {
	res := ssh.RunDetailed(ctx, exec, ssh.ShellQuote(serverTeployBinaryPath)+" version")
	if res.ExitCode != 0 || res.Err != nil {
		// Exit code 127 is the remote shell's definitive "command not
		// found" — absence of the optional server binary. Structured
		// status, not a guess from the failure text (which breaks the
		// moment a wrapper message contains "not found" for another
		// reason, or localizes).
		if res.ExitCode == 127 {
			return doctorCheck{
				Name: "compatibility", Result: doctorOK,
				Detail: fmt.Sprintf("no server-side teploy binary (optional — autodeploy installs one at %s)", serverTeployBinaryPath),
			}
		}
		detail := res.ExitErrorText()
		if detail == "" {
			detail = fmt.Sprintf("exit status %d", res.ExitCode)
		}
		return doctorCheck{
			Name: "compatibility", Result: doctorWarn, Detail: detail,
			Remediation: fmt.Sprintf("inspect %s on the server — it exists but would not run", serverTeployBinaryPath),
		}
	}
	serverVersion := doctorServerTeployVersion(res.TrimmedStdout())
	if serverVersion == "" {
		return doctorCheck{
			Name: "compatibility", Result: doctorWarn,
			Detail:      "server teploy present but reported no version",
			Remediation: "re-run teploy autodeploy install to refresh the server binary",
		}
	}
	if serverVersion == deps.localVersion {
		return doctorCheck{
			Name: "compatibility", Result: doctorOK,
			Detail: fmt.Sprintf("local teploy %s matches the server binary", deps.localVersion),
		}
	}
	return doctorCheck{
		Name: "compatibility", Result: doctorWarn,
		Detail:      fmt.Sprintf("local teploy %s, server teploy %s", deps.localVersion, serverVersion),
		Remediation: "re-run teploy autodeploy install (or teploy autodeploy schedule) to refresh the server binary",
	}
}

// doctorServerTeployVersion reads the server binary's `version` output
// ("teploy v0.1.37" in the human format every release speaks).
func doctorServerTeployVersion(out string) string {
	fields := strings.Fields(strings.TrimSpace(out))
	if len(fields) == 0 {
		return ""
	}
	return fields[len(fields)-1]
}

// doctorRepairDebtCheck reports the C01-6 marker: outstanding
// release-record debt from a deploy whose record write failed after the
// live commit. A warn, not a fail — the next deploy repairs it before its
// own work; an UNREADABLE marker is visible too (unhealable debt must not
// be invisible debt).
func doctorRepairDebtCheck(ctx context.Context, exec ssh.Executor, appCfg *config.AppConfig) doctorCheck {
	if appCfg == nil {
		return doctorCheck{
			Name: "repair-debt", Result: doctorOK,
			Detail: "no app identity (config unreadable) — nothing to inspect",
		}
	}
	debt, err := deploy.ReadRepairDebt(ctx, exec, appCfg.App)
	if err != nil {
		return doctorCheck{
			Name: "repair-debt", Result: doctorWarn, Detail: err.Error(),
			Remediation: fmt.Sprintf("inspect or remove /deployments/%s/repair-debt.json on the server — the debt cannot be read", appCfg.App),
		}
	}
	if debt == nil {
		return doctorCheck{Name: "repair-debt", Result: doctorOK, Detail: "no outstanding release-record repair debt"}
	}
	return doctorCheck{
		Name: "repair-debt", Result: doctorWarn,
		Detail: fmt.Sprintf("release record for %s@%s missing after %d failed write/repair attempt(s): %s — the next deploy rebuilds it",
			debt.App, debt.Release, debt.Attempts, debt.Reason),
		Remediation: "re-run teploy deploy (the reconciler repairs the record before its own work); teploy status shows the same debt",
	}
}
