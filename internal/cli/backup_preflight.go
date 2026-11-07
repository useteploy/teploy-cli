package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/useteploy/teploy/internal/backup"
	"github.com/useteploy/teploy/internal/events"
	"github.com/useteploy/teploy/internal/secret"
	"github.com/useteploy/teploy/internal/ssh"
)

// `teploy server backup-preflight`: what a server needs before backups can start,
// asked before anyone is asked for a key. A probe that fails reads as unknown, never
// as fine. --install-tools and --upgrade-binary are the only things here that change
// the server, each only on request.

// Preflight states.
const (
	PreflightOK      = "ok"
	PreflightMissing = "missing"
	PreflightOld     = "outdated"
	PreflightNoFleet = "no_fleet_engine"
	PreflightUnknown = "unknown"

	PreflightConfigured = "configured"
	PreflightAbsent     = "absent"

	ManagedNo  = "no"
	ManagedYes = "yes"
)

// Action results of --install-tools and --upgrade-binary.
const (
	ActionAlreadyDone = "already_done"
	ActionDone        = "done"
	ActionFailed      = "failed"
	ActionRefused     = "refused"
)

// preflightTool is one tool. State: ok, missing, outdated (aws below its floor) or
// unknown.
type preflightTool struct {
	State   string `json:"state"`
	Version string `json:"version,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

type preflightTools struct {
	AWS     preflightTool `json:"aws"`
	Age     preflightTool `json:"age"`
	Python3 preflightTool `json:"python3"`
}

// preflightBinary is the server's teploy, at the path cron lines run. State: ok,
// missing, no_fleet_engine or unknown. Nulls mean unknown.
type preflightBinary struct {
	Path        string `json:"path"`
	State       string `json:"state"`
	Version     string `json:"version,omitempty"`
	FleetEngine *bool  `json:"fleet_engine"`
	CLIVersion  string `json:"cli_version"`
	SameAsCLI   *bool  `json:"same_as_cli"`
	Detail      string `json:"detail,omitempty"`
}

// preflightDisk is the free space where backups are built. FreeBytes null: unknown.
type preflightDisk struct {
	Path      string `json:"path"`
	FreeBytes *int64 `json:"free_bytes"`
	Detail    string `json:"detail,omitempty"`
}

// preflightTarget is the stored target. State: configured, absent or unknown.
type preflightTarget struct {
	State          string `json:"state"`
	Bucket         string `json:"bucket,omitempty"`
	Endpoint       string `json:"endpoint,omitempty"`
	Prefix         string `json:"prefix,omitempty"`
	HasCredentials bool   `json:"has_credentials"`
	Detail         string `json:"detail,omitempty"`
}

// preflightPolicy is the stored policy. State: configured, absent or unknown.
// ManagedElsewhere says whether something other than this user looks after it (no,
// yes or unknown), with the evidence in ManagedReasons; HeartbeatHosts lets a
// consumer tell its own heartbeat from someone else's.
type preflightPolicy struct {
	State            string   `json:"state"`
	Path             string   `json:"path,omitempty"`
	Interval         string   `json:"interval,omitempty"`
	Encrypted        bool     `json:"encrypted"`
	HeartbeatHosts   []string `json:"heartbeat_hosts,omitempty"`
	CronInstalled    *bool    `json:"cron_installed"`
	ManagedElsewhere string   `json:"managed_elsewhere"`
	ManagedReasons   []string `json:"managed_reasons,omitempty"`
	Detail           string   `json:"detail,omitempty"`
}

// preflightToolAction is one tool's result under --install-tools.
type preflightToolAction struct {
	Tool   string `json:"tool"`
	Action string `json:"action"`
	Detail string `json:"detail,omitempty"`
}

// preflightUpgrade is the result of --upgrade-binary.
type preflightUpgrade struct {
	Action string `json:"action"`
	From   string `json:"from,omitempty"`
	To     string `json:"to,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// backupPreflightDTO is `server backup-preflight --json` (backup-setup-v1). The
// command exits nonzero only when a requested action failed or was refused; the report
// is printed either way.
type backupPreflightDTO struct {
	MachineInterface int             `json:"machine_interface"`
	Host             string          `json:"host"`
	ObservedAt       time.Time       `json:"observed_at"`
	Reachable        bool            `json:"reachable"`
	Error            string          `json:"error,omitempty"`
	Binary           preflightBinary `json:"binary"`
	Tools            preflightTools  `json:"tools"`
	Disk             preflightDisk   `json:"disk"`
	Target           preflightTarget `json:"target"`
	Policy           preflightPolicy `json:"policy"`
	// ToolsInstall and BinaryUpgrade appear only when asked for.
	ToolsInstall  []preflightToolAction `json:"tools_install,omitempty"`
	BinaryUpgrade *preflightUpgrade     `json:"binary_upgrade,omitempty"`
}

func newServerBackupPreflightCmd(flags *Flags) *cobra.Command {
	var installTools, upgradeBinary bool
	cmd := &cobra.Command{
		Use:   "backup-preflight",
		Short: "Report what this server needs before backups can start",
		Long: "Read-only by default: whether the server answers, its teploy binary's version\n" +
			"and whether that has the fleet engine, aws against its minimum version, age,\n" +
			"python3, free disk where backups are built, and whether a target and policy\n" +
			"exist and something else looks after the policy. A probe that fails reads as\n" +
			"unknown, never as fine. An unreachable server is a report, not an error.\n\n" +
			"--install-tools installs aws, age and python3 where missing, and nothing else\n" +
			"(no hardening, no other `teploy setup` steps). --upgrade-binary installs the\n" +
			"release matching this CLI's version as the server's teploy, verified before it\n" +
			"replaces the old one; it never downgrades. Nothing upgrades without this flag.\n" +
			"Either exits nonzero when it fails or refuses, after printing the report.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runServerBackupPreflight(flags, installTools, upgradeBinary)
		},
	}
	cmd.Flags().BoolVar(&installTools, "install-tools", false, "install aws, age and python3 where missing (nothing else)")
	cmd.Flags().BoolVar(&upgradeBinary, "upgrade-binary", false, "install this CLI's version as the server's teploy binary")
	return cmd
}

func runServerBackupPreflight(flags *Flags, installTools, upgradeBinary bool) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	host, user, key, err := configResolveServerForTarget(flags)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	var dto backupPreflightDTO
	var actionErr error
	executor, err := ssh.Connect(ctx, ssh.ConnectConfig{Host: host, User: user, KeyPath: key})
	if err != nil {
		dto = unreachablePreflight(host, scheduledTeployPath(), cliBuildVersion, err, now)
		if installTools || upgradeBinary {
			actionErr = fmt.Errorf("this server did not answer, so nothing was installed: %w", err)
		}
	} else {
		defer executor.Close()
		dto, actionErr = backupPreflightOn(ctx, executor, preflightOptions{
			InstallTools: installTools, UpgradeBinary: upgradeBinary, Binary: scheduledTeployPath(), CLIVersion: cliBuildVersion, Now: now, InstallRelease: installTeployRelease,
		})
	}
	if flags.JSON {
		return errors.Join(actionErr, json.NewEncoder(os.Stdout).Encode(dto))
	}
	printBackupPreflight(dto)
	return actionErr
}

type preflightOptions struct {
	InstallTools, UpgradeBinary bool
	Binary, CLIVersion          string
	Now                         time.Time
	// InstallRelease is the release pipeline (a seam: tests have no network).
	InstallRelease func(ctx context.Context, exec ssh.Executor, remotePath, tag string, capabilities ...string) (string, error)
}

// backupPreflightOn reports, acts if asked, and reports again after acting so the
// report describes the server as it now is.
func backupPreflightOn(ctx context.Context, exec ssh.Executor, o preflightOptions) (backupPreflightDTO, error) {
	pctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	dto := collectBackupPreflight(pctx, exec, o.Binary, o.CLIVersion, o.Now)
	cancel()
	if !o.InstallTools && !o.UpgradeBinary {
		return dto, nil
	}
	var errs []error
	var actions []preflightToolAction
	var upgrade *preflightUpgrade
	if o.InstallTools {
		actx, acancel := context.WithTimeout(ctx, 20*time.Minute)
		var err error
		actions, err = installBackupTools(actx, exec, dto.Tools, o.Binary, o.CLIVersion)
		acancel()
		errs = append(errs, err)
	}
	if o.UpgradeBinary {
		actx, acancel := context.WithTimeout(ctx, 10*time.Minute)
		u, err := upgradeServerBinary(actx, exec, dto.Binary, o)
		acancel()
		upgrade = &u
		errs = append(errs, err)
	}
	pctx, cancel = context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	dto = collectBackupPreflight(pctx, exec, o.Binary, o.CLIVersion, o.Now)
	dto.ToolsInstall, dto.BinaryUpgrade = actions, upgrade
	return dto, errors.Join(errs...)
}

// unreachablePreflight is the report for a server that did not answer: everything is
// unknown, which is what the tri-state exists for.
func unreachablePreflight(host, bin, cliVersion string, err error, now time.Time) backupPreflightDTO {
	dto := unknownPreflight(host, bin, cliVersion, now)
	dto.Error = oneLine(err.Error())
	return dto
}

func unknownPreflight(host, bin, cliVersion string, now time.Time) backupPreflightDTO {
	unknown := preflightTool{State: PreflightUnknown}
	return backupPreflightDTO{
		MachineInterface: MachineInterface, Host: host, ObservedAt: now,
		Binary: preflightBinary{Path: bin, State: PreflightUnknown, CLIVersion: cliVersion},
		Tools:  preflightTools{AWS: unknown, Age: unknown, Python3: unknown},
		Disk:   preflightDisk{Path: backup.DefaultTmpDir},
		Target: preflightTarget{State: PreflightUnknown},
		Policy: preflightPolicy{State: PreflightUnknown, ManagedElsewhere: PreflightUnknown},
	}
}

const (
	preflightProbeMarker   = "# teploy-preflight-probe"
	preflightManagedMarker = "# teploy-preflight-managed"
)

// preflightProbeScript prints tab-separated facts and, last, "end": a run cut short
// leaves the rest unknown. It sees the PATH a run sees, not this session's.
func preflightProbeScript(bin, tmpDir string) string {
	q := ssh.ShellQuote
	return preflightProbeMarker + `
PATH="$PATH:` + strings.Join(serverToolPaths, ":") + `"
b=` + q(bin) + `
if [ -e "$b" ]; then
  if v=$("$b" version 2>/dev/null); then printf 'binary_version\t%s\n' "$v"; else printf 'binary_broken\t1\n'; fi
  if h=$("$b" backup run --help 2>/dev/null); then case "$h" in *--all*) printf 'fleet\tyes\n';; *) printf 'fleet\tno\n';; esac; fi
else
  printf 'binary_missing\t1\n'
fi
if command -v aws >/dev/null 2>&1; then
  if v=$(aws --version 2>&1); then printf 'aws\t%s\n' "$v"; else printf 'aws_broken\t%s\n' "$v"; fi
else
  printf 'aws_missing\t1\n'
fi
if command -v age >/dev/null 2>&1 && command -v age-keygen >/dev/null 2>&1; then
  if v=$(age --version 2>&1); then printf 'age\t%s\n' "$v"; else printf 'age_broken\t%s\n' "$v"; fi
else
  printf 'age_missing\t1\n'
fi
if command -v python3 >/dev/null 2>&1; then
  if v=$(python3 --version 2>&1); then printf 'python3\t%s\n' "$v"; else printf 'python3_broken\t%s\n' "$v"; fi
else
  printf 'python3_missing\t1\n'
fi
if v=$(df -Pk -- ` + q(tmpDir) + ` 2>/dev/null); then printf 'disk\t%s\n' "$(printf '%s\n' "$v" | awk 'NR==2{print $4}')"; fi
printf 'end\t1\n'`
}

// collectBackupPreflight only reads. Each part fails to its own unknown.
func collectBackupPreflight(ctx context.Context, exec ssh.Executor, bin, cliVersion string, now time.Time) backupPreflightDTO {
	dto := unknownPreflight(exec.Host(), bin, cliVersion, now)

	policy, policyErr := backup.ReadFleetPolicy(ctx, exec)
	if policy.TmpDir != "" {
		dto.Disk.Path = policy.TmpDir
	}
	res := ssh.RunDetailed(ctx, exec, "sh -c "+ssh.ShellQuote(preflightProbeScript(bin, dto.Disk.Path)))
	if res.Err != nil {
		dto.Error = oneLine(res.Err.Error())
		return dto
	}
	dto.Reachable = true
	facts := map[string]string{}
	for _, line := range strings.Split(string(res.Stdout), "\n") {
		if k, v, ok := strings.Cut(line, "\t"); ok {
			facts[k] = v
		}
	}
	if _, done := facts["end"]; !done || res.ExitCode != 0 {
		dto.Error = "the probe did not finish, so what it did not reach is unknown"
	}
	dto.Binary = readBinaryFacts(facts, bin, cliVersion)
	dto.Tools = preflightTools{AWS: readAWSFacts(facts), Age: readToolFacts(facts, "age"), Python3: readToolFacts(facts, "python3")}
	dto.Disk = readDiskFacts(facts, dto.Disk.Path)
	dto.Target = readPreflightTarget(ctx, exec)
	dto.Policy = readPreflightPolicy(ctx, exec, policy, policyErr)
	return dto
}

func readBinaryFacts(f map[string]string, bin, cliVersion string) preflightBinary {
	b := preflightBinary{Path: bin, State: PreflightUnknown, CLIVersion: cliVersion}
	if _, missing := f["binary_missing"]; missing {
		no := false
		b.State, b.FleetEngine = PreflightMissing, &no
		return b
	}
	if _, broken := f["binary_broken"]; broken {
		b.Detail = "the binary is there and did not run"
	}
	if v, ok := f["binary_version"]; ok {
		b.Version = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(v), "teploy"))
		same := normalizeVersion(b.Version) == normalizeVersion(cliVersion)
		b.SameAsCLI = &same
	}
	if v, ok := f["fleet"]; ok {
		yes := v == "yes"
		b.FleetEngine = &yes
	}
	switch {
	case b.Version == "" || b.FleetEngine == nil:
		if b.Detail == "" && b.Version == "" {
			b.Detail = "could not read its version"
		}
	case *b.FleetEngine:
		b.State = PreflightOK
	default:
		b.State = PreflightNoFleet
	}
	return b
}

func readAWSFacts(f map[string]string) preflightTool {
	t := readToolFacts(f, "aws")
	if t.State != PreflightOK {
		return t
	}
	version := backup.AWSCLIVersion(t.Version)
	if version == "" {
		return preflightTool{State: PreflightUnknown, Detail: "could not read its version: " + oneLine(t.Version)}
	}
	t.Version = version
	if problem := backup.AWSCLIVersionProblem("aws-cli/" + version); problem != "" {
		t.State, t.Detail = PreflightOld, problem
	}
	return t
}

func readToolFacts(f map[string]string, name string) preflightTool {
	if _, missing := f[name+"_missing"]; missing {
		return preflightTool{State: PreflightMissing}
	}
	if v, broken := f[name+"_broken"]; broken {
		return preflightTool{State: PreflightUnknown, Detail: "it is there and did not run: " + oneLine(v)}
	}
	if v, ok := f[name]; ok {
		return preflightTool{State: PreflightOK, Version: strings.TrimSpace(v)}
	}
	return preflightTool{State: PreflightUnknown}
}

func readDiskFacts(f map[string]string, path string) preflightDisk {
	d := preflightDisk{Path: path}
	v, ok := f["disk"]
	if !ok {
		d.Detail = "could not read the free space there"
		return d
	}
	kb, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil {
		d.Detail = "could not read the free space there"
		return d
	}
	free := kb * 1024
	d.FreeBytes = &free
	return d
}

func readPreflightTarget(ctx context.Context, exec ssh.Executor) preflightTarget {
	t, err := backup.ReadTarget(ctx, exec)
	if err != nil {
		return preflightTarget{State: PreflightUnknown, Detail: oneLine(err.Error())}
	}
	if !TargetConfigured(t) {
		return preflightTarget{State: PreflightAbsent}
	}
	return preflightTarget{State: PreflightConfigured, Bucket: t.Bucket, Endpoint: t.Endpoint, Prefix: t.Prefix, HasCredentials: t.HasCredentials}
}

func readPreflightPolicy(ctx context.Context, exec ssh.Executor, p backup.FleetPolicy, err error) preflightPolicy {
	if err != nil {
		return preflightPolicy{State: PreflightUnknown, ManagedElsewhere: PreflightUnknown, Detail: oneLine(err.Error())}
	}
	if !p.Configured() {
		return preflightPolicy{State: PreflightAbsent, ManagedElsewhere: ManagedNo}
	}
	out := preflightPolicy{State: PreflightConfigured, Path: p.Path, Interval: p.Interval, Encrypted: p.Recipient != "", ManagedElsewhere: ManagedNo}
	for _, h := range backup.MaskedHeartbeats(p.HeartbeatURLs) {
		out.HeartbeatHosts = append(out.HeartbeatHosts, strings.TrimSuffix(h, "/..."))
	}
	if lines, cerr := backup.ReadCrontab(ctx, exec); cerr == nil {
		found := false
		for _, line := range lines {
			if m := backup.FindCronMarker(line); m != nil && m.Kind == backup.KindFleet {
				found = true
			}
		}
		out.CronInstalled = &found
		if !found && p.Interval != "" {
			out.ManagedReasons = append(out.ManagedReasons, "the policy has an interval and this user's crontab has no line for it, so something else may run it")
		}
	}
	reasons, known := policyFileManagedReasons(ctx, exec, p.Path)
	out.ManagedReasons = append(out.ManagedReasons, reasons...)
	switch {
	case len(out.ManagedReasons) > 0:
		out.ManagedElsewhere = ManagedYes
	case !known || out.CronInstalled == nil:
		out.ManagedElsewhere = PreflightUnknown
	}
	return out
}

// policyFileManagedReasons looks at the policy file itself. teploy writes it as a
// regular file, owned by root or the connecting user, that this user can replace.
// Anything else got there another way, such as configuration management.
func policyFileManagedReasons(ctx context.Context, exec ssh.Executor, path string) ([]string, bool) {
	script := preflightManagedMarker + `
p=` + ssh.ShellQuote(path) + `
S=
if [ "$(id -u)" != 0 ] && sudo -n true 2>/dev/null; then S="sudo -n"; fi
if $S test -L "$p"; then printf 'symlink\t1\n'; fi
if o=$($S stat -c %U "$p" 2>/dev/null); then printf 'owner\t%s\n' "$o"; fi
if [ -n "$S" ] || [ -w "$p" ]; then printf 'writable\t1\n'; fi
printf 'me\t%s\n' "$(id -un)"
printf 'end\t1\n'`
	res := ssh.RunDetailed(ctx, exec, "sh -c "+ssh.ShellQuote(script))
	if res.Failed() {
		return nil, false
	}
	f := map[string]string{}
	for _, line := range strings.Split(string(res.Stdout), "\n") {
		if k, v, ok := strings.Cut(line, "\t"); ok {
			f[k] = strings.TrimSpace(v)
		}
	}
	_, known := f["end"]
	if !known {
		return nil, false
	}
	var reasons []string
	if _, ok := f["symlink"]; ok {
		reasons = append(reasons, path+" is a symlink")
	}
	owner, haveOwner := f["owner"]
	if haveOwner && owner != "root" && owner != f["me"] {
		reasons = append(reasons, path+" is owned by "+owner)
	}
	if _, ok := f["writable"]; !ok {
		reasons = append(reasons, "this user cannot replace "+path)
	}
	return reasons, haveOwner
}

// installBackupTools installs aws, age and python3 where the report says they are not
// fine, through what `teploy setup` uses and nothing else of it. python3 first: AWS's
// bundle is unpacked with it. One failing does not stop the next. Recorded only when
// something was tried.
func installBackupTools(ctx context.Context, exec ssh.Executor, tools preflightTools, bin, cliVersion string) ([]preflightToolAction, error) {
	sudo := sudoPrefixFor(ctx, exec)
	const noninteractive = "DEBIAN_FRONTEND=noninteractive "
	steps := []struct {
		name  string
		state string
		run   func() error
	}{
		{"python3", tools.Python3.State, func() error {
			out, err := exec.Run(ctx, sudo+"sh -c "+ssh.ShellQuote(noninteractive+"apt-get update -qq && "+noninteractive+"apt-get install -y -qq python3"))
			return commandError(out, err)
		}},
		{"age", tools.Age.State, func() error { return secret.NewManager(exec).EnsureAge(ctx) }},
		{"aws", tools.AWS.State, func() error {
			// The package index may be stale on a server `teploy setup` never saw.
			_, _ = exec.Run(ctx, sudo+"sh -c "+ssh.ShellQuote(noninteractive+"apt-get update -qq"))
			out, err := exec.Run(ctx, sudo+"sh -c "+ssh.ShellQuote(backup.AWSCLIInstallScript))
			return commandError(out, err)
		}},
	}
	var actions []preflightToolAction
	var errs []error
	tried := false
	for _, s := range steps {
		if s.state == PreflightOK {
			actions = append(actions, preflightToolAction{Tool: s.name, Action: ActionAlreadyDone})
			continue
		}
		tried = true
		if err := s.run(); err != nil {
			actions = append(actions, preflightToolAction{Tool: s.name, Action: ActionFailed, Detail: oneLine(err.Error())})
			errs = append(errs, fmt.Errorf("installing %s: %w", s.name, err))
			continue
		}
		actions = append(actions, preflightToolAction{Tool: s.name, Action: ActionDone})
	}
	// Exit status alone is not success: an install that ran and left a tool that does
	// not run, or an aws still below its floor, did not work.
	after := collectBackupToolStates(ctx, exec, bin, cliVersion)
	for i, a := range actions {
		if a.Action != ActionDone {
			continue
		}
		if st := after[a.Tool]; st != PreflightOK {
			actions[i].Action = ActionFailed
			actions[i].Detail = "the install finished and the tool still reads as " + st
			errs = append(errs, fmt.Errorf("installing %s: it still reads as %s", a.Tool, st))
		}
	}
	err := errors.Join(errs...)
	if tried {
		var done, failed []string
		for _, a := range actions {
			switch a.Action {
			case ActionDone:
				done = append(done, a.Tool)
			case ActionFailed:
				failed = append(failed, a.Tool)
			}
		}
		events.Record(ctx, exec, events.Event{App: backup.FleetApp, Kind: events.KindServerTools, Outcome: events.Outcome(err), Detail: events.ErrDetail(map[string]any{"installed": done, "failed": failed}, err)})
	}
	return actions, err
}

func collectBackupToolStates(ctx context.Context, exec ssh.Executor, bin, cliVersion string) map[string]string {
	res := ssh.RunDetailed(ctx, exec, "sh -c "+ssh.ShellQuote(preflightProbeScript(bin, backup.DefaultTmpDir)))
	f := map[string]string{}
	if !res.Failed() {
		for _, line := range strings.Split(string(res.Stdout), "\n") {
			if k, v, ok := strings.Cut(line, "\t"); ok {
				f[k] = v
			}
		}
	}
	return map[string]string{"aws": readAWSFacts(f).State, "age": readToolFacts(f, "age").State, "python3": readToolFacts(f, "python3").State}
}

func commandError(out string, err error) error {
	if err == nil {
		return nil
	}
	if s := strings.TrimSpace(out); s != "" {
		return fmt.Errorf("%w: %s", err, s)
	}
	return err
}

// upgradeServerBinary installs the release matching the running CLI, through the
// verified path every other server install uses. It refuses what cannot be that: a
// development CLI, a binary path that is not the managed one, a downgrade.
func upgradeServerBinary(ctx context.Context, exec ssh.Executor, b preflightBinary, o preflightOptions) (preflightUpgrade, error) {
	want := normalizeVersion(o.CLIVersion)
	u := preflightUpgrade{From: b.Version, To: want}
	refuse := func(format string, args ...any) (preflightUpgrade, error) {
		u.Action, u.Detail = ActionRefused, fmt.Sprintf(format, args...)
		err := errors.New("refusing to upgrade the server's teploy: " + u.Detail)
		events.Record(ctx, exec, events.Event{App: backup.FleetApp, Kind: events.KindServerBinary, Outcome: events.Refused, Detail: events.ErrDetail(map[string]any{"from": u.From, "to": u.To}, err)})
		return u, err
	}
	wantParts, ok := parseVersion(want)
	if !ok {
		return refuse("this CLI is version %q, which is not a release", o.CLIVersion)
	}
	if o.Binary != serverTeployPath {
		return refuse("TEPLOY_SERVER_BINARY names %s, which this path does not manage", o.Binary)
	}
	if b.Version != "" {
		if have, ok := parseVersion(normalizeVersion(b.Version)); ok {
			switch compareVersions(have, wantParts) {
			case 0:
				if b.State == PreflightOK {
					u.Action = ActionAlreadyDone
					return u, nil
				}
			case 1:
				return refuse("the server has %s, newer than this CLI's %s; upgrade this CLI instead", b.Version, want)
			}
		}
	}
	record := func(err error) {
		events.Record(ctx, exec, events.Event{App: backup.FleetApp, Kind: events.KindServerBinary, Outcome: events.Outcome(err), Detail: events.ErrDetail(map[string]any{"from": u.From, "to": u.To}, err)})
	}
	fail := func(err error) (preflightUpgrade, error) {
		u.Action, u.Detail = ActionFailed, oneLine(err.Error())
		record(err)
		return u, err
	}
	if _, err := o.InstallRelease(ctx, exec, serverTeployPath, "v"+want, "backup run"); err != nil {
		return fail(fmt.Errorf("installing teploy %s: %w", want, err))
	}
	after := collectBackupPreflight(ctx, exec, o.Binary, o.CLIVersion, o.Now).Binary
	if normalizeVersion(after.Version) != want {
		return fail(fmt.Errorf("after installing, the server's teploy reads as %q, not %s", after.Version, want))
	}
	if after.State != PreflightOK {
		return fail(fmt.Errorf("after installing %s, the server's teploy reads as %s", want, after.State))
	}
	u.Action = ActionDone
	record(nil)
	return u, nil
}

func normalizeVersion(v string) string {
	return strings.TrimPrefix(strings.TrimSpace(v), "v")
}

// parseVersion reads a plain release version: 0.1.54.
func parseVersion(v string) ([3]int, bool) {
	var out [3]int
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

func compareVersions(a, b [3]int) int {
	for i := range a {
		switch {
		case a[i] < b[i]:
			return -1
		case a[i] > b[i]:
			return 1
		}
	}
	return 0
}

func printBackupPreflight(d backupPreflightDTO) {
	if !d.Reachable {
		fmt.Printf("%s did not answer: %s\nEverything below it is unknown.\n", d.Host, d.Error)
		return
	}
	if d.Error != "" {
		fmt.Printf("Warning: %s\n", d.Error)
	}
	version := d.Binary.Version
	if version == "" {
		version = "version unknown"
	}
	fmt.Printf("teploy on server: %s (%s, %s; this CLI is %s)\n", d.Binary.State, version, d.Binary.Path, d.Binary.CLIVersion)
	tool := func(name string, t preflightTool) {
		fmt.Printf("%-16s  %s", name+":", t.State)
		if t.Version != "" {
			fmt.Printf(" (%s)", t.Version)
		}
		if t.Detail != "" {
			fmt.Printf(": %s", t.Detail)
		}
		fmt.Println()
	}
	tool("aws", d.Tools.AWS)
	tool("age", d.Tools.Age)
	tool("python3", d.Tools.Python3)
	if d.Disk.FreeBytes != nil {
		fmt.Printf("%-16s  %.1f GB free at %s\n", "disk:", float64(*d.Disk.FreeBytes)/(1<<30), d.Disk.Path)
	} else {
		fmt.Printf("%-16s  unknown at %s\n", "disk:", d.Disk.Path)
	}
	fmt.Printf("%-16s  %s\n", "target:", d.Target.State)
	fmt.Printf("%-16s  %s (managed elsewhere: %s)\n", "policy:", d.Policy.State, d.Policy.ManagedElsewhere)
	for _, r := range d.Policy.ManagedReasons {
		fmt.Printf("  %s\n", r)
	}
	for _, a := range d.ToolsInstall {
		fmt.Printf("install %s: %s", a.Tool, a.Action)
		if a.Detail != "" {
			fmt.Printf(": %s", a.Detail)
		}
		fmt.Println()
	}
	if u := d.BinaryUpgrade; u != nil {
		fmt.Printf("upgrade teploy to %s: %s", u.To, u.Action)
		if u.Detail != "" {
			fmt.Printf(": %s", u.Detail)
		}
		fmt.Println()
	}
}
