package cli

// `teploy server audit <server>`: what is left over on a server, with evidence,
// and the one command that fixes it where a command can do so safely. Read-only:
// every remote command here only reads (a test pins that). What could not be
// read is named in checks[] and yields no finding: it is never reported clean.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/useteploy/teploy/internal/backup"
	"github.com/useteploy/teploy/internal/caddy"
	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/deploy"
	"github.com/useteploy/teploy/internal/docker"
	"github.com/useteploy/teploy/internal/domains"
	"github.com/useteploy/teploy/internal/ssh"
	"github.com/useteploy/teploy/internal/state"
)

// Finding kinds. Stable identifiers: consumers code against them.
const (
	findingStaleRoute       = "stale_route"
	findingEmptyRegion      = "empty_managed_region"
	findingDeadUpstream     = "managed_route_dead_upstream"
	findingOrphanState      = "orphan_state"
	findingManualDead       = "manual_route_dead_upstream"
	findingManualNonLocal   = "manual_route_nonlocal_address"
	findingVersionsBeyond   = "versions_beyond_policy"
	findingBuildArtifacts   = "reclaimable_build_artifacts"
	findingLargeLayer       = "large_writable_layer"
	findingUnmanagedLarge   = "unmanaged_container"
	findingDanglingVolumes  = "dangling_volumes"
	findingCaddyfileNoCaddy = "caddyfile_without_caddy"
	findingSchedulerMissing = "scheduler_missing"
	findingDiskPressure     = "disk_pressure"
)

const (
	severityCritical = "critical"
	severityWarning  = "warning"
	severityInfo     = "info"

	auditCheckOK      = "ok"
	auditCheckUnknown = "unknown"

	auditStateOK          = "ok"
	auditStatePartial     = "partial"
	auditStateUnreachable = "unreachable"

	auditLargeLayerBytes     = 5_000_000_000
	auditLargeUnmanagedBytes = 1_000_000_000
	auditLargeCacheBytes     = 1_000_000_000
	auditTimeout             = 5 * time.Minute
)

// auditRemedy is what fixes a finding. Argv is null when nothing here can do it
// safely (report only); argv[0] is "teploy". Dash adds its own --actor and --json.
type auditRemedy struct {
	Argv        []string `json:"argv"`
	Destructive bool     `json:"destructive"`
	Confirm     bool     `json:"confirm"`
}

// auditFinding is one thing left over. ID is kind:subject, stable across runs
// so a finding can be dismissed; sizes and evidence may change under it.
type auditFinding struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Severity string `json:"severity"`
	Subject  string `json:"subject"`
	// App is set when the finding belongs to one app: the name a destructive
	// confirmation asks for.
	App       string `json:"app,omitempty"`
	Evidence  string `json:"evidence"`
	SizeBytes int64  `json:"size_bytes,omitempty"`
	// Related names the findings that would free space on a disk_pressure.
	Related []string    `json:"related,omitempty"`
	Remedy  auditRemedy `json:"remedy"`
}

// auditCheck says whether one source of evidence was read. An unknown check
// means findings depending on it are missing, not absent.
type auditCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

type auditDiskUse struct {
	Name      string `json:"name"`
	SizeBytes int64  `json:"size_bytes"`
}

// auditDisk is the root filesystem and what takes the space.
type auditDisk struct {
	Path           string         `json:"path"`
	AvailableBytes int64          `json:"available_bytes"`
	UsedPercent    int            `json:"used_percent"`
	Attribution    []auditDiskUse `json:"attribution"`
}

// serverAuditEnvelope is `server audit --json` (contracts/schema/
// server-audit-envelope.schema.json).
type serverAuditEnvelope struct {
	MachineInterface int            `json:"machine_interface"`
	Server           string         `json:"server"`
	Host             string         `json:"host"`
	AuditedAt        time.Time      `json:"audited_at"`
	State            string         `json:"state"`
	Error            string         `json:"error,omitempty"`
	Checks           []auditCheck   `json:"checks"`
	Disk             *auditDisk     `json:"disk"`
	Findings         []auditFinding `json:"findings"`
}

func newServerAuditCmd(flags *Flags) *cobra.Command {
	return &cobra.Command{
		Use:   "audit <server>",
		Short: "Report what is left over on a server, and what fixes it (read-only)",
		Long: `Reports stale and dead routes, state with no containers, versions beyond
keep_versions, build cache, dangling images and volumes, large writable layers,
large containers teploy does not manage, a Caddyfile with no Caddy, a missing
cron daemon and disk pressure, each with its evidence.

Changes nothing. Where a command can fix a finding safely, remedy.argv is that
command; hand-written Caddy blocks, volumes and other people's containers are
report only. A source that could not be read shows as unknown in checks[], and
the findings that depend on it are missing rather than clean. With --json the
command exits 0 whenever the envelope was produced; read state.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runServerAudit(flags, args[0], cmd.OutOrStdout())
		},
	}
}

func runServerAudit(flags *Flags, server string, out io.Writer) error {
	host, user, key, err := config.ResolveServer(server, flags.Host, flags.User, flags.Key)
	if err != nil {
		return fmt.Errorf("resolving server %q: %w", server, err)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	ctx, stop := context.WithTimeout(ctx, auditTimeout)
	defer stop()

	now := time.Now().UTC()
	var env serverAuditEnvelope
	dialCtx, dialCancel := context.WithTimeout(ctx, domainsConnectTimeout)
	exec, err := ssh.Connect(dialCtx, ssh.ConnectConfig{Host: host, User: user, KeyPath: key})
	dialCancel()
	if err != nil {
		env = serverAuditEnvelope{MachineInterface: MachineInterface, Server: server, Host: host, AuditedAt: now,
			State: auditStateUnreachable, Error: strings.Join(strings.Fields(err.Error()), " "), Checks: []auditCheck{}, Findings: []auditFinding{}}
	} else {
		defer exec.Close()
		env = auditServer(ctx, exec, server, host, now)
	}
	if flags.JSON {
		return json.NewEncoder(out).Encode(env)
	}
	return printServerAudit(out, env)
}

func printServerAudit(out io.Writer, env serverAuditEnvelope) error {
	if env.State == auditStateUnreachable {
		return fmt.Errorf("%s could not be audited: %s", env.Server, env.Error)
	}
	for _, f := range env.Findings {
		fmt.Fprintf(out, "%-8s  %s\n          %s\n", f.Severity, f.ID, f.Evidence)
		if len(f.Remedy.Argv) > 0 {
			fmt.Fprintf(out, "          fix: %s\n", strings.Join(f.Remedy.Argv, " "))
		}
	}
	if len(env.Findings) == 0 {
		fmt.Fprintf(out, "No findings on %s\n", env.Server)
	}
	for _, c := range env.Checks {
		if c.Status != auditCheckOK {
			fmt.Fprintf(out, "\nUnknown, not clean: %s: %s\n", c.Name, c.Detail)
		}
	}
	return nil
}

// auditFacts is everything read from a server. Each *Known flag separates "read,
// and there is none" from "could not read".
type auditFacts struct {
	server string
	now    time.Time

	apps      map[string]*state.AppState // present and not tombstoned
	appErrs   map[string]string          // an app whose state could not be read
	appsKnown bool

	caddyfile   string // present | absent | unknown
	regions     []caddy.ManagedRegion
	manualSites []caddy.RouteSite

	containers      []docker.Container
	containersKnown bool
	sizes           map[string]int64
	sizesKnown      bool

	resolves   map[string]string
	roots      map[string]string
	addrs      []string
	addrsKnown bool

	pins        map[string][]string
	pinErrs     map[string]string
	notices     map[string]time.Time
	noticeErrs  map[string]string
	df          *auditDisk
	usage       map[string]dockerUsage
	usageKnown  bool
	danglingIDs []string
	imagesKnown bool
	volumes     []string
	volsKnown   bool
	scheduler   *backup.SchedulerStatus

	checks []auditCheck
}

type dockerUsage struct {
	size, reclaimable int64
}

func (f *auditFacts) check(name string, err error) {
	if err != nil {
		f.checks = append(f.checks, auditCheck{Name: name, Status: auditCheckUnknown, Detail: strings.Join(strings.Fields(err.Error()), " ")})
		return
	}
	f.checks = append(f.checks, auditCheck{Name: name, Status: auditCheckOK})
}

// auditServer runs every read on one connected server and judges the result.
func auditServer(ctx context.Context, exec ssh.Executor, server, host string, now time.Time) serverAuditEnvelope {
	f := collectAuditFacts(ctx, exec, server, now)
	env := serverAuditEnvelope{MachineInterface: MachineInterface, Server: server, Host: host, AuditedAt: now,
		State: auditStateOK, Checks: f.checks, Disk: f.df, Findings: analyzeAudit(f)}
	for _, c := range f.checks {
		if c.Status != auditCheckOK {
			env.State = auditStatePartial
		}
	}
	return env
}

func collectAuditFacts(ctx context.Context, exec ssh.Executor, server string, now time.Time) *auditFacts {
	f := &auditFacts{server: server, now: now, apps: map[string]*state.AppState{}, appErrs: map[string]string{},
		sizes: map[string]int64{}, resolves: map[string]string{}, roots: map[string]string{},
		pins: map[string][]string{}, pinErrs: map[string]string{}, notices: map[string]time.Time{}, noticeErrs: map[string]string{},
		usage: map[string]dockerUsage{}}

	f.check("apps", f.readApps(ctx, exec))
	f.check("caddyfile", f.readCaddyfile(ctx, exec))
	containers, err := docker.NewClient(exec).ListAllContainers(ctx)
	f.containers, f.containersKnown = containers, err == nil
	f.check("containers", err)
	f.check("container_sizes", f.readSizes(ctx, exec))
	f.check("upstreams", f.readUpstreams(ctx, exec))
	f.check("static_roots", f.readRoots(ctx, exec))
	f.check("addresses", f.readAddrs(ctx, exec))
	f.check("docker_usage", f.readUsage(ctx, exec))
	f.check("dangling_images", f.readDanglingImages(ctx, exec))
	f.check("dangling_volumes", f.readDanglingVolumes(ctx, exec))
	f.check("disk", f.readDisk(ctx, exec))
	st, err := backup.ProbeScheduler(ctx, exec)
	if err == nil {
		f.scheduler = &st
	}
	f.check("scheduler", err)
	f.check("retention", f.readRetention(ctx, exec))
	return f
}

func (f *auditFacts) readApps(ctx context.Context, exec ssh.Executor) error {
	out, err := exec.Run(ctx, domains.AppListCommand)
	if err != nil {
		return err
	}
	var names []string
	for _, n := range strings.Fields(out) {
		if config.ValidateName(n) == nil {
			names = append(names, n)
		}
	}
	snaps, err := state.ReadAppSnapshots(ctx, exec, names)
	if err != nil {
		return err
	}
	f.appsKnown = true
	var bad []string
	for _, snap := range snaps {
		switch {
		case snap.StateErr != nil:
			f.appErrs[snap.App] = snap.StateErr.Error()
			bad = append(bad, snap.App)
		case snap.State != nil && !snap.State.Tombstone:
			f.apps[snap.App] = snap.State
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("could not read the state of %s", strings.Join(bad, ", "))
	}
	return nil
}

func (f *auditFacts) readCaddyfile(ctx context.Context, exec ssh.Executor) error {
	f.caddyfile = "unknown"
	content, present, err := caddy.NewClient(exec).ReadCaddyfile(ctx)
	if err != nil {
		return err
	}
	if !present {
		f.caddyfile = "absent"
		return nil
	}
	regions, err := caddy.ManagedRegions(content)
	if err != nil {
		return err
	}
	sites, err := caddy.InspectRoutes(content)
	if err != nil {
		return err
	}
	f.caddyfile, f.regions = "present", regions
	for _, site := range sites {
		if site.Owner == "" {
			f.manualSites = append(f.manualSites, site)
		}
	}
	return nil
}

// auditSizesCommand is bounded: --size walks every writable layer.
const auditSizesCommand = `t=; command -v timeout >/dev/null 2>&1 && t='timeout 120'; $t docker ps --all --size --format '{"Names":{{json .Names}},"Size":{{json .Size}}}'`

func (f *auditFacts) readSizes(ctx context.Context, exec ssh.Executor) error {
	out, err := exec.Run(ctx, auditSizesCommand)
	if err != nil {
		return err
	}
	for _, line := range doctorLines(out) {
		var row struct{ Names, Size string }
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			return fmt.Errorf("unrecognized docker ps output: %w", err)
		}
		size, ok := parseDockerSize(strings.SplitN(row.Size, " (", 2)[0])
		if !ok {
			return fmt.Errorf("unrecognized container size %q", row.Size)
		}
		f.sizes[row.Names] = size
	}
	f.sizesKnown = true
	return nil
}

var dockerSizeRE = regexp.MustCompile(`^([0-9.]+)\s*([A-Za-z]*)$`)

// parseDockerSize reads docker's human sizes (powers of 1000: go-units).
func parseDockerSize(s string) (int64, bool) {
	m := dockerSizeRE.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, false
	}
	n, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, false
	}
	mult := map[string]float64{"": 1, "B": 1, "kB": 1e3, "KB": 1e3, "MB": 1e6, "GB": 1e9, "TB": 1e12, "PB": 1e15,
		"KiB": 1 << 10, "MiB": 1 << 20, "GiB": 1 << 30, "TiB": 1 << 40}[m[2]]
	if mult == 0 {
		return 0, false
	}
	return int64(n * mult), true
}

func (f *auditFacts) caddyRunning() bool {
	return f.containersKnown && f.caddyContainerState() == "running"
}

// caddyContainerState is the caddy container's state, "" when there is none.
func (f *auditFacts) caddyContainerState() string {
	for _, c := range f.containers {
		if c.Name == "caddy" {
			return c.State
		}
	}
	return ""
}

func (f *auditFacts) readUpstreams(ctx context.Context, exec ssh.Executor) error {
	wanted := map[string]bool{}
	add := func(sites []caddy.RouteSite) {
		for _, site := range sites {
			for _, dial := range site.Upstreams {
				if name, _ := domains.UpstreamHost(dial); name != "" {
					wanted[name] = true
				}
			}
		}
	}
	add(f.manualSites)
	for _, r := range f.regions {
		add(r.Sites)
	}
	if len(wanted) == 0 {
		return nil
	}
	if !f.caddyRunning() {
		return fmt.Errorf("no running caddy container to resolve %d upstream name(s) in", len(wanted))
	}
	names := make([]string, 0, len(wanted))
	for n := range wanted {
		names = append(names, n)
	}
	answers, err := domains.ResolveNames(ctx, exec, names)
	f.resolves = answers
	if err != nil {
		return err
	}
	var missing []string
	for _, n := range names {
		if answers[n] == "" {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("could not resolve %s", strings.Join(missing, ", "))
	}
	return nil
}

// readRoots checks the file roots of managed regions, the only thing besides an
// upstream that can make a block with no app state still serve something.
func (f *auditFacts) readRoots(ctx context.Context, exec ssh.Executor) error {
	var firstErr error
	for _, r := range f.regions {
		for _, root := range caddy.RegionRoots(r.Text) {
			if _, seen := f.roots[root]; seen {
				continue
			}
			verdict, err := readRootExists(ctx, exec, root)
			if err != nil && firstErr == nil {
				firstErr = err
			}
			f.roots[root] = verdict
		}
	}
	return firstErr
}

func readRootExists(ctx context.Context, exec ssh.Executor, root string) (string, error) {
	if strings.Contains(root, "{") {
		return domains.ResolvesUnknown, nil // a placeholder: only Caddy can expand it
	}
	out, err := exec.Run(ctx, "if [ -e "+ssh.ShellQuote(root)+" ]; then echo yes; else echo no; fi")
	if err != nil {
		return domains.ResolvesUnknown, err
	}
	return strings.TrimSpace(out), nil
}

func (f *auditFacts) readAddrs(ctx context.Context, exec ssh.Executor) error {
	out, err := exec.Run(ctx, "hostname -I")
	if err != nil {
		return err
	}
	f.addrs = append(strings.Fields(out), exec.Host())
	f.addrsKnown = true
	return nil
}

func (f *auditFacts) readUsage(ctx context.Context, exec ssh.Executor) error {
	out, err := exec.Run(ctx, `docker system df --format '{{json .}}'`)
	if err != nil {
		return err
	}
	for _, line := range doctorLines(out) {
		var row struct{ Type, Size, Reclaimable string }
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			return fmt.Errorf("unrecognized docker system df output: %w", err)
		}
		size, ok1 := parseDockerSize(row.Size)
		reclaim, ok2 := parseDockerSize(strings.SplitN(row.Reclaimable, " (", 2)[0])
		if !ok1 || !ok2 {
			return fmt.Errorf("unrecognized docker system df row %q", line)
		}
		f.usage[row.Type] = dockerUsage{size, reclaim}
	}
	f.usageKnown = len(f.usage) > 0
	if !f.usageKnown {
		return fmt.Errorf("docker system df returned nothing")
	}
	return nil
}

func (f *auditFacts) readDanglingImages(ctx context.Context, exec ssh.Executor) error {
	out, err := exec.Run(ctx, "docker images -q -f dangling=true")
	if err != nil {
		return err
	}
	f.danglingIDs, f.imagesKnown = doctorLines(out), true
	return nil
}

func (f *auditFacts) readDanglingVolumes(ctx context.Context, exec ssh.Executor) error {
	out, err := exec.Run(ctx, "docker volume ls -q -f dangling=true")
	if err != nil {
		return err
	}
	f.volumes, f.volsKnown = doctorLines(out), true
	return nil
}

func (f *auditFacts) readDisk(ctx context.Context, exec ssh.Executor) error {
	out, err := exec.Run(ctx, "df -B1 -P /")
	if err != nil {
		return err
	}
	avail, pct, ok := parseDoctorDisk(out)
	if !ok {
		return fmt.Errorf("could not parse df output")
	}
	disk := &auditDisk{Path: "/", AvailableBytes: int64(avail), UsedPercent: pct, Attribution: []auditDiskUse{}}
	for _, row := range []struct{ name, key string }{{"images", "Images"}, {"container_layers", "Containers"}, {"volumes", "Local Volumes"}, {"build_cache", "Build Cache"}} {
		if u, ok := f.usage[row.key]; ok {
			disk.Attribution = append(disk.Attribution, auditDiskUse{row.name, u.size})
		}
	}
	// Only when it matters: du walks the tree.
	if diskUnderPressure(disk) {
		if out, err := exec.Run(ctx, `t=; command -v timeout >/dev/null 2>&1 && t='timeout 60'; $t du -sxb /deployments 2>/dev/null`); err == nil {
			if fields := strings.Fields(out); len(fields) == 2 {
				if n, err := strconv.ParseInt(fields[0], 10, 64); err == nil {
					disk.Attribution = append(disk.Attribution, auditDiskUse{"deployments", n})
				}
			}
		}
	}
	f.df = disk
	return nil
}

func diskUnderPressure(d *auditDisk) bool {
	return d.AvailableBytes < doctorDiskWarnAvailable || d.UsedPercent >= doctorDiskWarnUsedPct
}

// readRetention reads pins and notices for the apps with versions beyond policy.
func (f *auditFacts) readRetention(ctx context.Context, exec ssh.Executor) error {
	if !f.containersKnown {
		return fmt.Errorf("container list unavailable")
	}
	var firstErr error
	for app, st := range f.apps {
		keep, explicit, keepAll := appliedKeepPolicy(st)
		if keepAll || st.DeploymentType == "static" {
			continue
		}
		if len(docker.PrunableVersions(docker.ContainersByApp(f.containers, app), keep, retentionProtected(st, nil, docker.ContainersByApp(f.containers, app))...)) == 0 {
			continue
		}
		pins, err := state.ReadPins(ctx, exec, app)
		if err != nil {
			f.pinErrs[app] = err.Error()
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		f.pins[app] = pins
		if explicit {
			continue
		}
		data, present, err := state.ReadRemoteFile(ctx, exec, deploy.PruneNoticePath(app))
		if err != nil {
			f.noticeErrs[app] = err.Error()
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if t, ok := deploy.ParsePruneNotice(data); present && ok {
			f.notices[app] = t
		}
	}
	return firstErr
}

// appliedKeepPolicy reads the retention the running release was deployed with.
// A manifest with no keep_versions, or the 0 an unset field records, is the
// default: an explicit 0 records keep_all_versions.
func appliedKeepPolicy(st *state.AppState) (keep int, explicit, keepAll bool) {
	var m struct {
		Container struct {
			KeepVersions int  `json:"keep_versions"`
			KeepAll      bool `json:"keep_all_versions"`
		} `json:"container"`
	}
	if len(st.AppliedManifest) > 0 {
		_ = json.Unmarshal(st.AppliedManifest, &m)
	}
	switch {
	case m.Container.KeepAll:
		return 0, true, true
	case m.Container.KeepVersions > 0:
		return m.Container.KeepVersions, true, false
	}
	return config.DefaultKeepVersions, false, false
}

// retentionProtected is what no prune may touch: current, previous, pinned, and
// any version with a running container.
func retentionProtected(st *state.AppState, pins []string, containers []docker.Container) []string {
	protected := append([]string{st.CurrentHash, st.PreviousHash}, pins...)
	for _, c := range containers {
		if v := c.Labels["teploy.version"]; v != "" && c.State == "running" && !slices.Contains(protected, v) {
			protected = append(protected, v)
		}
	}
	return protected
}

// ownedContainers are the containers that make an app's name still in use: its
// own, its previews', and unlabelled ones named after it (older deploys).
func ownedContainers(containers []docker.Container, app string) []docker.Container {
	var owned []docker.Container
	for _, c := range containers {
		label := c.Labels["teploy.app"]
		switch {
		case label == app, strings.HasPrefix(label, app+"-preview-"):
			owned = append(owned, c)
		case label == "" && strings.HasPrefix(c.Name, app+"-"):
			owned = append(owned, c)
		}
	}
	return owned
}

// routeFacts is what deciding on one managed route needs: the same judgement for
// the audit and for `route remove`, so the remedy offered is the one that runs.
type routeFacts struct {
	app        string
	region     string
	sites      []caddy.RouteSite
	stateKnown bool
	st         *state.AppState
	known      bool // containers were listed
	containers []docker.Container
	resolves   map[string]string
	roots      map[string]string
}

// removalRefusal says why a route must not be removed, "" when it may be. Only
// what is proven dead can go: an unreadable source refuses like a live app does.
func removalRefusal(r routeFacts) string {
	if !r.stateKnown {
		return "its app state could not be read"
	}
	if !r.known {
		return "its containers could not be listed"
	}
	if caddy.RegionHasWebhook(r.region) {
		return "it carries the autodeploy webhook route; the next deploy retires it"
	}
	if r.st != nil && routedByCaddy(r.st) {
		if r.st.DeploymentType == "static" {
			return "it belongs to a live static app"
		}
		for _, c := range ownedContainers(r.containers, r.app) {
			if c.Labels["teploy.role"] != "accessory" {
				return fmt.Sprintf("it belongs to a live app: %s has container %s (%s)", r.app, c.Name, c.State)
			}
		}
	}
	if r.st == nil {
		for _, c := range ownedContainers(r.containers, r.app) {
			return fmt.Sprintf("container %s (%s) still carries this app's name", c.Name, c.State)
		}
	}
	for _, site := range r.sites {
		for _, dial := range site.Upstreams {
			switch dialVerdict(dial, r.resolves) {
			case domains.ResolvesNo:
			case domains.ResolvesYes:
				return "upstream " + dial + " still resolves"
			default:
				return "upstream " + dial + " could not be proven dead"
			}
		}
	}
	for _, root := range caddy.RegionRoots(r.region) {
		switch r.roots[root] {
		case "no":
		case "yes":
			return "it serves files from " + root + ", which exists"
		default:
			return "whether " + root + " exists could not be read"
		}
	}
	return ""
}

func routedByCaddy(st *state.AppState) bool {
	return st.IngressMode == "" || st.IngressMode == config.IngressCaddy
}

func dialVerdict(dial string, resolves map[string]string) string {
	name, verdict := domains.UpstreamHost(dial)
	if name == "" {
		return verdict
	}
	if v := resolves[name]; v != "" {
		return v
	}
	return domains.ResolvesUnknown
}

func dialsByVerdict(sites []caddy.RouteSite, resolves map[string]string, verdict string) []string {
	var dials []string
	for _, site := range sites {
		for _, dial := range site.Upstreams {
			if dialVerdict(dial, resolves) == verdict && !slices.Contains(dials, dial) {
				dials = append(dials, dial)
			}
		}
	}
	return dials
}

func siteHosts(sites []caddy.RouteSite) []string {
	var hosts []string
	for _, site := range sites {
		hosts = append(hosts, site.Hosts...)
	}
	return hosts
}

func analyzeAudit(f *auditFacts) []auditFinding {
	findings := []auditFinding{}
	add := func(kind, severity, subject, app, evidence string, size int64, remedy auditRemedy) {
		findings = append(findings, auditFinding{ID: kind + ":" + subject, Kind: kind, Severity: severity, Subject: subject,
			App: app, Evidence: evidence, SizeBytes: size, Remedy: remedy})
	}
	teploy := func(args ...string) []string { return append([]string{"teploy"}, args...) }

	// Managed routes. An app whose state could not be read is not judged.
	if f.appsKnown {
		for _, r := range f.regions {
			if _, bad := f.appErrs[r.App]; bad {
				continue
			}
			st := f.apps[r.App]
			facts := routeFacts{app: r.App, region: r.Text, sites: r.Sites, stateKnown: true, st: st,
				known: f.containersKnown, containers: f.containers, resolves: f.resolves, roots: f.roots}
			refusal := removalRefusal(facts)
			removeRemedy := auditRemedy{}
			note := ""
			if refusal == "" {
				removeRemedy.Argv = teploy("route", "remove", "--app", r.App, "--host", f.server)
			} else {
				note = "; not removable: " + refusal
			}
			hosts := strings.Join(siteHosts(r.Sites), ", ")
			switch {
			case len(r.Sites) == 0:
				add(findingEmptyRegion, severityInfo, r.App, r.App, "the managed region holds no site block"+note, 0, removeRemedy)
			case st == nil:
				dead := dialsByVerdict(r.Sites, f.resolves, domains.ResolvesNo)
				live := dialsByVerdict(r.Sites, f.resolves, domains.ResolvesYes)
				// A live preview has a route and no app state of its own.
				if strings.Contains(r.App, "-preview-") && len(live) > 0 && len(dead) == 0 {
					continue
				}
				evidence := fmt.Sprintf("no app %s on this server, but its managed block serves %s", r.App, hosts)
				if len(dead) > 0 {
					evidence += " and routes to " + strings.Join(dead, ", ") + ", which does not resolve"
				}
				add(findingStaleRoute, severityWarning, r.App, r.App, evidence+note, 0, removeRemedy)
			case !routedByCaddy(st):
				add(findingStaleRoute, severityWarning, r.App, r.App,
					fmt.Sprintf("%s is deployed with ingress %s, but a managed block still serves %s%s", r.App, st.IngressMode, hosts, note), 0, removeRemedy)
			default:
				dead := dialsByVerdict(r.Sites, f.resolves, domains.ResolvesNo)
				if len(dead) == 0 {
					continue
				}
				severity, remedy, evidence := deadRouteVerdict(f, r, st, dead, hosts)
				add(findingDeadUpstream, severity, r.App, r.App, evidence, 0, remedy)
			}
		}
	}

	// Hand-written blocks are never edited: report only.
	for _, site := range f.manualSites {
		subject := strings.Join(site.Hosts, ",")
		if dead := dialsByVerdict([]caddy.RouteSite{site}, f.resolves, domains.ResolvesNo); len(dead) > 0 {
			add(findingManualDead, severityWarning, subject, "", fmt.Sprintf("hand-written block for %s routes to %s, which does not resolve", subject, strings.Join(dead, ", ")), 0, auditRemedy{})
		}
		if !f.addrsKnown {
			continue
		}
		for _, host := range site.Hosts {
			if ip := net.ParseIP(host); ip != nil && !ip.IsLoopback() && !slices.Contains(f.addrs, ip.String()) {
				add(findingManualNonLocal, severityInfo, subject, "", fmt.Sprintf("hand-written block is addressed to %s, which is not an address of this server", host), 0, auditRemedy{})
				break
			}
		}
	}

	if f.caddyfile == "present" && f.containersKnown && f.caddyContainerState() != "running" {
		how := "no caddy container"
		if s := f.caddyContainerState(); s != "" {
			how = "the caddy container is " + s
		}
		severity := severityInfo
		routes := len(f.regions) + len(f.manualSites)
		if routes > 0 {
			severity = severityWarning
		}
		add(findingCaddyfileNoCaddy, severity, "caddy", "", fmt.Sprintf("/deployments/caddy/Caddyfile exists with %d route block(s), but %s", routes, how), 0, auditRemedy{})
	}

	// State with no containers.
	if f.appsKnown && f.containersKnown {
		for _, app := range sortedAppNames(f.apps) {
			st := f.apps[app]
			if st.DeploymentType == "static" || len(ownedContainers(f.containers, app)) > 0 {
				continue
			}
			add(findingOrphanState, severityWarning, app, app,
				fmt.Sprintf("%s has deploy state (release %s) but no containers; removing keeps its volumes, accessory data and credentials", app, st.CurrentHash), 0,
				auditRemedy{Argv: teploy("remove", "--app", app, "--host", f.server, "-y"), Destructive: true, Confirm: true})
		}
	}

	// Versions beyond policy.
	if f.appsKnown && f.containersKnown {
		for _, app := range sortedAppNames(f.apps) {
			st := f.apps[app]
			keep, explicit, keepAll := appliedKeepPolicy(st)
			if keepAll || st.DeploymentType == "static" {
				continue
			}
			if _, bad := f.pinErrs[app]; bad {
				continue
			}
			owned := docker.ContainersByApp(f.containers, app)
			prunable := docker.PrunableVersions(owned, keep, retentionProtected(st, f.pins[app], owned)...)
			if len(prunable) == 0 {
				continue
			}
			var size int64
			for _, c := range owned {
				if slices.Contains(prunable, c.Labels["teploy.version"]) {
					size += f.sizes[c.Name]
				}
			}
			evidence := fmt.Sprintf("%d version(s) beyond keep_versions %d: %s", len(prunable), keep, strings.Join(prunable, ", "))
			severity := severityInfo
			if explicit {
				evidence += "; the next deploy removes them"
			} else {
				severity = severityWarning
				evidence += "; keep_versions is not set, so " + defaultPruneWarning(f, app)
			}
			add(findingVersionsBeyond, severity, app, app, evidence, size,
				auditRemedy{Argv: teploy("releases", "prune", "--app", app, "--host", f.server, "--keep", strconv.Itoa(keep)), Destructive: true, Confirm: true})
		}
	}

	// Build cache and dangling images: rebuildable, so no confirmation.
	if f.usageKnown || f.imagesKnown {
		cache := f.usage["Build Cache"].reclaimable
		if cache >= auditLargeCacheBytes || len(f.danglingIDs) > 0 {
			evidence := fmt.Sprintf("%d dangling image(s)", len(f.danglingIDs))
			if f.usageKnown {
				evidence = fmt.Sprintf("%s of unused build cache and %s", humanBytes(cache), evidence)
			} else {
				evidence += " (the build cache size could not be read)"
			}
			add(findingBuildArtifacts, severityInfo, "docker", "", evidence, cache,
				auditRemedy{Argv: teploy("server", "prune", "--host", f.server, "--build-cache", "--dangling")})
		}
	}

	// Large containers: never touched, whoever owns them.
	if f.sizesKnown && f.containersKnown {
		for _, c := range f.containers {
			size := f.sizes[c.Name]
			managed := c.Labels["teploy.app"] != "" || c.Name == "caddy" || belongsToKnownApp(c.Name, f.apps)
			switch {
			case managed && size >= auditLargeLayerBytes:
				add(findingLargeLayer, severityWarning, c.Name, c.Labels["teploy.app"],
					fmt.Sprintf("%s (%s) has written %s to its container layer, which a redeploy discards: move that data to a volume", c.Name, c.State, humanBytes(size)), size, auditRemedy{})
			case !managed && size >= auditLargeUnmanagedBytes:
				add(findingUnmanagedLarge, severityWarning, c.Name, "",
					fmt.Sprintf("%s (%s) is not managed by teploy and holds %s in its container layer", c.Name, c.State, humanBytes(size)), size, auditRemedy{})
			}
		}
	}

	// Volumes may hold the only copy of something: there is never a remedy.
	if f.volsKnown && len(f.volumes) > 0 {
		add(findingDanglingVolumes, severityInfo, "docker", "",
			fmt.Sprintf("%d volume(s) attached to no container, %s reclaimable: %s", len(f.volumes), humanBytes(f.usage["Local Volumes"].reclaimable), doctorSample(f.volumes)),
			f.usage["Local Volumes"].reclaimable, auditRemedy{})
	}

	if f.scheduler != nil && !f.scheduler.Daemon && f.scheduler.HasJobs() {
		severity := severityWarning
		if f.scheduler.HasTeployJobs() {
			severity = severityCritical
		}
		add(findingSchedulerMissing, severity, "cron", "", "jobs are scheduled but no cron daemon is running, so none of them run: "+backup.SchedulerRemedy(), 0, auditRemedy{})
	}

	if f.df != nil && diskUnderPressure(f.df) {
		severity := severityWarning
		if f.df.AvailableBytes < doctorDiskFailAvailable {
			severity = severityCritical
		}
		uses := make([]string, 0, len(f.df.Attribution))
		attribution := append([]auditDiskUse{}, f.df.Attribution...)
		sort.Slice(attribution, func(i, j int) bool { return attribution[i].SizeBytes > attribution[j].SizeBytes })
		for _, u := range attribution {
			uses = append(uses, u.Name+" "+humanBytes(u.SizeBytes))
		}
		var related []string
		for _, x := range findings {
			switch x.Kind {
			case findingVersionsBeyond, findingBuildArtifacts, findingLargeLayer, findingUnmanagedLarge, findingDanglingVolumes:
				related = append(related, x.ID)
			}
		}
		sort.Strings(related)
		findings = append(findings, auditFinding{ID: findingDiskPressure + ":/", Kind: findingDiskPressure, Severity: severity, Subject: "/",
			Evidence: fmt.Sprintf("%s available on / (%d%% used); %s", humanBytes(f.df.AvailableBytes), f.df.UsedPercent, strings.Join(uses, ", ")), Related: related})
	}

	rank := map[string]int{severityCritical: 0, severityWarning: 1, severityInfo: 2}
	sort.SliceStable(findings, func(i, j int) bool {
		if rank[findings[i].Severity] != rank[findings[j].Severity] {
			return rank[findings[i].Severity] < rank[findings[j].Severity]
		}
		return findings[i].ID < findings[j].ID
	})
	return findings
}

// deadRouteVerdict words a caddy-routed app's dead upstream. Redeploy is offered
// only when it can help: a static app has no containers to bring back, and an
// upstream named after another app (a caddy_extra proxy) stays dead through this
// app's redeploy.
func deadRouteVerdict(f *auditFacts, r caddy.ManagedRegion, st *state.AppState, dead []string, hosts string) (string, auditRemedy, string) {
	live := len(dialsByVerdict(r.Sites, f.resolves, domains.ResolvesYes)) > 0
	evidence := fmt.Sprintf("%s routes to %s, which does not resolve in Caddy's network", hosts, strings.Join(dead, ", "))
	ownDead := false
	for _, dial := range dead {
		if name, _ := domains.UpstreamHost(dial); strings.HasPrefix(name, r.App+"-") {
			ownDead = true
		}
	}
	severity := severityCritical
	if live || st.DeploymentType == "static" {
		severity = severityWarning // part of the site still serves
	}
	switch {
	case st.DeploymentType == "static":
		return severity, auditRemedy{}, evidence + fmt.Sprintf("; %s itself serves files, so only those paths fail. A redeploy cannot help: fix the app's caddy_extra or deploy what it proxies to", r.App)
	case !ownDead:
		return severity, auditRemedy{}, evidence + "; that upstream is not this app's, so a redeploy cannot help: fix the app's caddy_extra or deploy what it proxies to"
	}
	return severity, auditRemedy{Argv: []string{"teploy", "redeploy", "--app", r.App, "--host", f.server}, Confirm: true}, evidence
}

func defaultPruneWarning(f *auditFacts, app string) string {
	if detail, bad := f.noticeErrs[app]; bad {
		return "when automatic pruning starts could not be read (" + detail + ")"
	}
	if t, ok := f.notices[app]; ok {
		return "deploys after " + t.Add(deploy.DefaultPruneNotice).Format("2006-01-02") + " remove them automatically; set keep_versions (0 keeps everything) or pin what you need"
	}
	return "the next deploy starts a 7-day notice, then deploys remove them automatically; set keep_versions (0 keeps everything) or pin what you need"
}

func belongsToKnownApp(name string, apps map[string]*state.AppState) bool {
	for app := range apps {
		if strings.HasPrefix(name, app+"-") {
			return true
		}
	}
	return false
}

func sortedAppNames(apps map[string]*state.AppState) []string {
	names := make([]string, 0, len(apps))
	for n := range apps {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func humanBytes(n int64) string {
	switch {
	case n >= 1e12:
		return fmt.Sprintf("%.1f TB", float64(n)/1e12)
	case n >= 1e9:
		return fmt.Sprintf("%.1f GB", float64(n)/1e9)
	case n >= 1e6:
		return fmt.Sprintf("%.1f MB", float64(n)/1e6)
	}
	return fmt.Sprintf("%d B", n)
}
