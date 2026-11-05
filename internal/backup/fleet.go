package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/useteploy/teploy/internal/ssh"
)

// The fleet run (G4): what `teploy backup run --all` does on a server. Targets
// are enumerated AT RUN TIME (labelled accessories, app volumes, adopted
// containers), run one at a time (a small host's disk and RAM are shared with
// production), and one failing never stops the others.

// Target kinds.
const (
	TargetAccessory = "accessory"
	TargetVolumes   = "volumes"
	TargetAdopted   = "adopted"
)

// Target statuses.
const (
	TargetOK       = "ok"
	TargetFailed   = "failed"
	TargetExcluded = "excluded" // left out by policy or the size guard
	TargetOwnCron  = "own-schedule"
)

// Run statuses.
const (
	RunOK      = "ok"
	RunPartial = "partial"
	RunFailed  = "failed"
)

// ContainerFact is what the inventory knows about one container. Env is parsed
// for engine detection and credentials; it is never reported.
type ContainerFact struct {
	Name, Image, State   string
	Role, App, Accessory string
	Labels               map[string]string
	Env                  map[string]string
	Argv                 []string
	Engine               EngineDetection
	EngineErr            error
}

// Unmanaged reports a container with no teploy role.
func (f ContainerFact) Unmanaged() bool { return f.Role == "" }

// unsupportedDatabases are images of databases teploy has no dump method for.
// They classify as generic, so nothing else would ever mention them; the same
// family liveDBMarkers refuses to tar from a running container.
var unsupportedDatabases = map[string]bool{
	"clickhouse": true, "clickhouse-server": true, "elasticsearch": true, "opensearch": true,
	"qdrant": true, "keydb": true, "cassandra": true, "scylla": true, "couchdb": true,
	"couchbase": true, "influxdb": true, "neo4j": true, "cockroach": true, "rethinkdb": true,
	"etcd": true, "milvus": true, "weaviate": true, "typesense": true, "meilisearch": true,
}

// UnsupportedDatabase names a database teploy cannot back up in this container,
// "" when it can or this is no database. A teploy.engine label asserting generic
// is the operator saying it is plain files.
func (f ContainerFact) UnsupportedDatabase() string {
	if f.EngineErr != nil || f.Engine.IsDB() || f.Engine.Source == "label" || f.Engine.Source == "env-not-server" {
		return ""
	}
	if name := imageName(f.Image); unsupportedDatabases[name] {
		return name
	}
	return ""
}

// DBLike reports a container holding a database, whether or not teploy can dump it.
func (f ContainerFact) DBLike() bool {
	return (f.EngineErr == nil && f.Engine.IsDB()) || f.UnsupportedDatabase() != ""
}

// inventoryFormat emits one JSON object per container.
const inventoryFormat = `{"name":{{json .Name}},"image":{{json .Config.Image}},"state":{{json .State.Status}},"labels":{{json .Config.Labels}},"env":{{json .Config.Env}},"entrypoint":{{json .Config.Entrypoint}},"cmd":{{json .Config.Cmd}}}`

// Inventory reads every container on the server in one call.
func (c *Client) Inventory(ctx context.Context) ([]ContainerFact, error) {
	// Not `docker ps | xargs`: a dead daemon would read as an empty server, and a
	// run with no targets reports success.
	cmd := "ids=$(docker ps -aq --no-trunc) || exit 1; [ -z \"$ids\" ] || docker inspect --format " + ssh.ShellQuote(inventoryFormat) + " $ids"
	res := ssh.RunDetailedLimit(ctx, c.exec, cmd, 64<<20)
	if res.Err != nil {
		return nil, fmt.Errorf("listing containers: %w", res.Err)
	}
	if res.Truncated {
		return nil, fmt.Errorf("listing containers: output too large to read safely")
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("listing containers: %s", oneLine(res.ExitErrorText()))
	}
	return parseInventory(string(res.Stdout))
}

func parseInventory(out string) ([]ContainerFact, error) {
	var facts []ContainerFact
	for _, line := range splitLines(out) {
		var raw struct {
			Name       string            `json:"name"`
			Image      string            `json:"image"`
			State      string            `json:"state"`
			Labels     map[string]string `json:"labels"`
			Env        []string          `json:"env"`
			Entrypoint []string          `json:"entrypoint"`
			Cmd        []string          `json:"cmd"`
		}
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			return nil, fmt.Errorf("parsing the container inventory: %w", err)
		}
		f := ContainerFact{
			Name: strings.TrimPrefix(raw.Name, "/"), Image: raw.Image, State: raw.State, Labels: raw.Labels,
			Role: raw.Labels["teploy.role"], App: raw.Labels["teploy.app"], Accessory: raw.Labels["teploy.accessory"],
			Env: map[string]string{}, Argv: append(append([]string{}, raw.Entrypoint...), raw.Cmd...),
		}
		for _, kv := range raw.Env {
			if k, v, ok := strings.Cut(kv, "="); ok {
				f.Env[k] = v
			}
		}
		// Older accessories carry no teploy.accessory label; their name says.
		if f.Role == "accessory" && f.Accessory == "" && f.App != "" {
			f.Accessory = strings.TrimPrefix(f.Name, f.App+"-")
		}
		f.Engine, f.EngineErr = DetectEngine(f.Image, f.Env, f.Labels, f.Argv)
		facts = append(facts, f)
	}
	sort.Slice(facts, func(i, j int) bool { return facts[i].Name < facts[j].Name })
	return facts, nil
}

// isScratch reports teploy's own throwaway containers.
func isScratch(name string) bool {
	return strings.HasPrefix(name, "teploy-backup-verify-") || strings.HasPrefix(name, "teploy-dr-")
}

// FleetTarget is one thing a run backs up.
type FleetTarget struct {
	Kind      string
	App, Name string
	Container string
	Fact      *ContainerFact
	Adopted   *AdoptedEntry
	// Retention is the policy's, or the per-app override's.
	Retention RetentionPolicy
	Status    string // pre-decided TargetExcluded / TargetOwnCron / TargetFailed
	Reason    string
	SizeBytes int64
	// SizeGuard says the exclusion is volume_max_gb's: the run records an event
	// so it is visible in the app's history, not only in this run's output.
	SizeGuard bool
}

// FleetTargetResult is one target's outcome.
type FleetTargetResult struct {
	Kind      string `json:"kind"`
	App       string `json:"app,omitempty"`
	Accessory string `json:"accessory,omitempty"`
	Container string `json:"container,omitempty"`
	Engine    string `json:"engine,omitempty"`
	// Status: ok | failed | excluded | own-schedule.
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
	ID     string `json:"id,omitempty"`
	// SizeBytes is stored bytes for a backup, the measured tree for an exclusion.
	SizeBytes  int64          `json:"size_bytes"`
	DurationMs int64          `json:"duration_ms"`
	Encrypted  bool           `json:"encrypted"`
	Objects    []BackupObject `json:"objects,omitempty"`
	Pruned     int            `json:"pruned"`
	Warnings   []string       `json:"warnings,omitempty"`
	// FailedDatabases are databases of this target that were not backed up.
	FailedDatabases map[string]string `json:"failed_databases,omitempty"`
	// UncoveredDatabases are databases on this server this target left out.
	UncoveredDatabases []string     `json:"uncovered_databases,omitempty"`
	Result             BackupResult `json:"-"`
	// SizeGuard says volume_max_gb excluded this target.
	SizeGuard bool `json:"size_guard,omitempty"`
}

// HeartbeatResult reports one heartbeat without its URL.
type HeartbeatResult struct {
	Host   string `json:"host"`
	OK     bool   `json:"ok"`
	Status int    `json:"status,omitempty"`
	Error  string `json:"error,omitempty"`
}

// FleetRunResult is the outcome of one run.
type FleetRunResult struct {
	// Status: ok (every target ok; omissions are listed, see Warnings) | partial
	// (some failed) | failed (none succeeded, or the run could not start).
	Status     string              `json:"status"`
	StartedAt  time.Time           `json:"started_at"`
	DurationMs int64               `json:"duration_ms"`
	Encrypted  bool                `json:"encrypted"`
	Targets    []FleetTargetResult `json:"targets"`
	// UncoveredContainers are DB-like containers no target covers.
	UncoveredContainers []string `json:"uncovered_containers,omitempty"`
	// Omissions name everything left out of this run: exclusions, size-guard
	// hits, uncovered containers and databases. A clean status with omissions is
	// never just "ok" to a reader of this field.
	Omissions  []string          `json:"omissions,omitempty"`
	Warnings   []string          `json:"warnings,omitempty"`
	Heartbeats []HeartbeatResult `json:"heartbeats,omitempty"`
	// Cleaned counts a killed run's leftovers this run removed.
	CleanedWorkspaces int    `json:"cleaned_workspaces,omitempty"`
	AbortedUploads    int    `json:"aborted_uploads,omitempty"`
	Error             string `json:"error,omitempty"`
}

var (
	fleetAppRE  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	fleetNameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)
)

// volumeTree is one app's non-empty volumes directory.
type volumeTree struct {
	App string
	KB  int64
}

// listVolumeTrees sizes every app's volumes directory.
func (c *Client) listVolumeTrees(ctx context.Context) ([]volumeTree, error) {
	out, err := c.exec.Run(ctx, `for d in /deployments/*/volumes; do [ -d "$d" ] || continue; [ -n "$(ls -A "$d" 2>/dev/null)" ] || continue; a=$(basename "$(dirname "$d")"); kb=$(du -sk "$d" 2>/dev/null | cut -f1); printf '%s\t%s\n' "${kb:-0}" "$a"; done; true`)
	if err != nil {
		return nil, fmt.Errorf("sizing app volumes: %w", err)
	}
	var trees []volumeTree
	for _, line := range splitLines(out) {
		kbs, app, ok := strings.Cut(line, "\t")
		if !ok || !fleetAppRE.MatchString(app) {
			continue
		}
		kb, _ := strconv.ParseInt(kbs, 10, 64)
		trees = append(trees, volumeTree{App: app, KB: kb})
	}
	return trees, nil
}

func (c *Client) duKB(ctx context.Context, dir string) (int64, bool) {
	out, err := c.exec.Run(ctx, "du -sk "+ssh.ShellQuote(dir)+" 2>/dev/null | cut -f1")
	if err != nil {
		return 0, false
	}
	kb, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	return kb, err == nil
}

// EnumerateFleet lists this run's targets and the DB-like containers none of
// them cover. It reads the server now; nothing is cached between runs.
func (c *Client) EnumerateFleet(ctx context.Context, p FleetPolicy) ([]FleetTarget, []string, []string, error) {
	facts, err := c.Inventory(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	cron, err := ReadCrontab(ctx, c.exec)
	if err != nil {
		return nil, nil, nil, err
	}
	var warnings []string
	ownCron := map[string]bool{}
	for _, line := range cron {
		// A hand-written job under a teploy marker (custom) is not an override:
		// nothing says it works (two of them failed nightly for weeks), and the
		// policy covering the database as well is how they get retired.
		if m := FindCronMarker(line); m != nil && m.Kind != KindFleet && m.Form != CronFormCustom {
			ownCron[m.App+"/"+m.Accessory] = true
			if m.Form == CronFormLegacy {
				warnings = append(warnings, fmt.Sprintf("cron marker %s is a pre-D3 shell line: it uploads plaintext whatever this policy says. Rewrite it with `teploy server backup-target set`", m.Tag))
			}
		}
	}
	policies := map[string]AppPolicy{}
	readAppPolicy := func(app string) AppPolicy {
		if ap, ok := policies[app]; ok {
			return ap
		}
		ap, err := ReadPolicy(ctx, c.exec, app)
		if err != nil {
			fmt.Fprintf(c.out, "Warning: %v\n", err)
		}
		policies[app] = ap
		return ap
	}
	base := p.Retention.Policy()
	// A per-app schedule overrides the policy only while its cron line runs it, and
	// then this run skips the target. A leftover backup-schedule.json with no line
	// must not decide this policy's retention: applying one deleted seeded history
	// in a single run, with a clean status.
	staleStore := func(kp *KindPolicy, label string) {
		if kp != nil && (kp.KeepLast != 0 || kp.MaxAgeDays != 0) {
			warnings = append(warnings, fmt.Sprintf("%s has retention in its backup-schedule.json (keep_last %d, max_age_days %d) but no cron line runs it, so the policy's retention applies; the leftover has no effect", label, kp.KeepLast, kp.MaxAgeDays))
		}
	}
	adopted := map[string]*AdoptedEntry{}
	for i := range p.Adopted {
		adopted[p.Adopted[i].Container] = &p.Adopted[i]
	}
	seenAdopted := map[string]bool{}

	var targets []FleetTarget
	var uncovered []string
	for i := range facts {
		f := &facts[i]
		if isScratch(f.Name) {
			continue
		}
		if a, ok := adopted[f.Name]; ok {
			seenAdopted[f.Name] = true
			t := FleetTarget{Kind: TargetAdopted, App: AdoptedApp, Name: f.Name, Container: f.Name, Fact: f, Adopted: a, Retention: base}
			if p.Excluded(AdoptedApp, f.Name, f.Name) {
				t.Status, t.Reason = TargetExcluded, "excluded by policy"
			}
			targets = append(targets, t)
			continue
		}
		if f.Role == "accessory" {
			t := FleetTarget{Kind: TargetAccessory, App: f.App, Name: f.Accessory, Container: f.Name, Fact: f}
			switch {
			case !fleetAppRE.MatchString(f.App) || !fleetNameRE.MatchString(f.Accessory):
				t.Status, t.Reason = TargetFailed, fmt.Sprintf("container %s carries unusable teploy labels", f.Name)
			case f.Name != ContainerName(f.App, f.Accessory):
				t.Status, t.Reason = TargetFailed, fmt.Sprintf("container %s does not match its labels (expected %s), so it cannot be addressed; adopt it by name", f.Name, ContainerName(f.App, f.Accessory))
			default:
				t.Retention = base
				if !ownCron[f.App+"/"+f.Accessory] {
					if ap := readAppPolicy(f.App); ap.Accessories != nil {
						staleStore(ap.Accessories[f.Accessory], f.Name)
					}
				}
				switch {
				case p.Excluded(f.App, f.Accessory, f.Name):
					t.Status, t.Reason = TargetExcluded, "excluded by policy"
				case ownCron[f.App+"/"+f.Accessory]:
					t.Status, t.Reason = TargetOwnCron, "has its own backup schedule (a per-app schedule overrides the policy)"
				case p.VolumeMaxGB > 0 && f.EngineErr == nil && f.Engine.Engine == EngineGeneric:
					// A generic accessory is a raw tar: the same full copy a
					// volume archive is, under the same cap.
					if kb, ok := c.duKB(ctx, fmt.Sprintf("%s/%s/accessories/%s", deploymentsDir, f.App, f.Accessory)); ok {
						t.SizeBytes = kb * 1024
						if float64(t.SizeBytes) > p.VolumeMaxGB*(1<<30) {
							t.Status, t.Reason, t.SizeGuard = TargetExcluded, fmt.Sprintf("accessory files are %.2f GB, over the policy's volume_max_gb %.2f; not backed up", float64(t.SizeBytes)/(1<<30), p.VolumeMaxGB), true
						}
					}
				}
			}
			targets = append(targets, t)
			continue
		}
		if f.DBLike() && !p.Excluded("", "", f.Name) {
			uncovered = append(uncovered, f.Name)
		}
	}
	for _, a := range p.Adopted {
		if !seenAdopted[a.Container] {
			entry := a
			targets = append(targets, FleetTarget{Kind: TargetAdopted, App: AdoptedApp, Name: a.Container, Container: a.Container, Adopted: &entry, Retention: base,
				Status: TargetFailed, Reason: "adopted container " + a.Container + " does not exist on this server"})
		}
	}
	if p.Volumes {
		trees, err := c.listVolumeTrees(ctx)
		if err != nil {
			return targets, uncovered, warnings, err
		}
		for _, tr := range trees {
			t := FleetTarget{Kind: TargetVolumes, App: tr.App, SizeBytes: tr.KB * 1024, Retention: base}
			if !ownCron[tr.App+"/"] {
				staleStore(readAppPolicy(tr.App).Volumes, tr.App+"/volumes")
			}
			switch {
			case p.Excluded(tr.App, "volumes", ""):
				t.Status, t.Reason = TargetExcluded, "excluded by policy"
			case ownCron[tr.App+"/"]:
				t.Status, t.Reason = TargetOwnCron, "has its own backup schedule (a per-app schedule overrides the policy)"
			case p.VolumeMaxGB > 0 && float64(t.SizeBytes) > p.VolumeMaxGB*(1<<30):
				t.Status, t.Reason, t.SizeGuard = TargetExcluded, fmt.Sprintf("volumes are %.2f GB, over the policy's volume_max_gb %.2f; not backed up", float64(t.SizeBytes)/(1<<30), p.VolumeMaxGB), true
			}
			targets = append(targets, t)
		}
	}
	return targets, uncovered, warnings, nil
}

// RunFleetTarget backs one target up and, only if that fully succeeded, prunes.
func (c *Client) RunFleetTarget(ctx context.Context, t FleetTarget, s3 S3Config, opts BackupOptions) FleetTargetResult {
	r := FleetTargetResult{Kind: t.Kind, App: t.App, Accessory: t.Name, Container: t.Container, Status: t.Status, Reason: t.Reason, SizeBytes: t.SizeBytes, Encrypted: s3.Encrypted(), SizeGuard: t.SizeGuard}
	if t.Status != "" {
		return r
	}
	var res BackupResult
	var err error
	prefix := "volumes"
	switch t.Kind {
	case TargetVolumes:
		res, err = c.BackupVolumesWith(ctx, t.App, s3, opts)
	default:
		prefix = "accessories/" + t.Name
		o := opts
		o.AllDatabases = true
		if t.Adopted != nil {
			if serr := c.SetEngineOverride(t.Container, t.Adopted.Engine); serr != nil {
				err = serr
				break
			}
			if !t.Adopted.Databases.All {
				o.AllDatabases = false
				o.Databases = t.Adopted.Databases.Names
			}
		}
		res, err = c.AccessoryBackupWith(ctx, t.App, t.Name, t.Fact.Image, t.Fact.Env, s3, o)
	}
	r.Result = res
	r.Engine, r.ID, r.DurationMs, r.Objects, r.Warnings, r.FailedDatabases, r.UncoveredDatabases =
		res.Engine, res.ID, res.DurationMs, res.Objects, res.Warnings, res.FailedDatabases, res.UncoveredDatabases
	r.SizeBytes = res.SizeBytes
	if err != nil {
		// Nothing is pruned when any part of this target failed: the run that
		// could not make a new copy is the last one to delete old copies.
		r.Status, r.Reason = TargetFailed, err.Error()
		return r
	}
	r.Status = TargetOK
	// Nothing stored is no new copy: the run that made none is the last one to
	// delete old ones. And a target that used to have backups and now has nothing
	// to back up (every database dropped) is an outage, not a quiet success.
	if len(res.Objects) == 0 {
		rows, lerr := c.ListBackupsRecursive(ctx, t.App, s3)
		switch {
		case lerr != nil:
			r.Status, r.Reason = TargetFailed, "stored nothing, and could not check for earlier backups: "+lerr.Error()
		default:
			for _, row := range rows {
				if row.Accessory == t.Name && row.Restorable {
					r.Status, r.Reason = TargetFailed, "this target has earlier backups and now holds nothing to back up: its databases are gone. Nothing was pruned"
					break
				}
			}
		}
		return r
	}
	c.CleanStaleIncoming(ctx, t.App, s3)
	if t.Retention.IsZero() {
		return r
	}
	deleted, perr := c.PruneBackups(ctx, t.App, prefix, s3, t.Retention)
	r.Pruned = len(deleted)
	if perr != nil {
		r.Warnings = append(r.Warnings, "prune: "+perr.Error())
	}
	return r
}

// SummarizeFleet sets the run status and collects omissions.
func SummarizeFleet(res *FleetRunResult) {
	ok, failed := 0, 0
	for _, t := range res.Targets {
		label := t.Container
		if label == "" {
			label = t.App + "/" + t.Kind
		}
		switch t.Status {
		case TargetOK:
			ok++
			if len(t.Objects) == 0 {
				res.Omissions = append(res.Omissions, label+": holds no database yet, so nothing was stored")
			}
		case TargetFailed:
			failed++
		case TargetExcluded:
			res.Omissions = append(res.Omissions, label+": "+t.Reason)
		}
		for _, db := range t.UncoveredDatabases {
			res.Omissions = append(res.Omissions, label+": database "+db+" not backed up")
		}
	}
	for _, c := range res.UncoveredContainers {
		res.Omissions = append(res.Omissions, c+": a database container no policy entry covers (adopt or exclude it)")
	}
	switch {
	case failed == 0:
		res.Status = RunOK
	case ok > 0:
		res.Status = RunPartial
	default:
		res.Status = RunFailed
	}
}

// ---- heartbeat ----

// heartbeatTimeout bounds one heartbeat attempt.
const heartbeatTimeout = 10 * time.Second

// heartbeatURL is the URL to ping: as configured on success; on failure with
// status=down (Dash and Uptime Kuma push monitors) or healthchecks.io's /fail.
func heartbeatURL(raw string, up bool) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("unparseable heartbeat URL")
	}
	if up {
		return u.String(), nil
	}
	if strings.HasSuffix(u.Hostname(), "hc-ping.com") {
		u.Path = strings.TrimSuffix(u.Path, "/") + "/fail"
		return u.String(), nil
	}
	q := u.Query()
	q.Set("status", "down")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// SendHeartbeats pings every URL. A heartbeat failing never fails the backup,
// and no URL (they carry tokens) reaches a result, a log or an error: net/http's
// errors embed the URL, so only their cause is kept.
func SendHeartbeats(ctx context.Context, urls []string, up bool, client *http.Client) []HeartbeatResult {
	if client == nil {
		client = &http.Client{Timeout: heartbeatTimeout}
	}
	out := make([]HeartbeatResult, 0, len(urls))
	for _, raw := range urls {
		r := HeartbeatResult{}
		parsed, perr := url.Parse(raw)
		if perr != nil {
			r.Error = "unparseable heartbeat URL"
			out = append(out, r)
			continue
		}
		r.Host = parsed.Host
		target, err := heartbeatURL(raw, up)
		if err != nil {
			r.Error = err.Error()
			out = append(out, r)
			continue
		}
		for attempt := 0; attempt < 3 && !r.OK; attempt++ {
			if attempt > 0 {
				select {
				case <-ctx.Done():
				case <-time.After(2 * time.Second):
				}
			}
			r.Status, r.Error = 0, ""
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
			if err != nil {
				r.Error = "building the request failed"
				break
			}
			resp, err := client.Do(req)
			if err != nil {
				var ue *url.Error
				if errors.As(err, &ue) {
					r.Error = ue.Err.Error()
				} else {
					r.Error = "request failed"
				}
				continue
			}
			io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
			resp.Body.Close()
			r.Status = resp.StatusCode
			r.OK = resp.StatusCode >= 200 && resp.StatusCode < 300
			if !r.OK {
				r.Error = "unexpected status " + strconv.Itoa(resp.StatusCode)
			}
		}
		out = append(out, r)
	}
	return out
}

// ProbeTarget says whether the target answers through the exact path a run uses
// (aws, stored credentials), reading under the base: a key scoped to one prefix
// cannot list the bucket root, and an empty prefix is an answer.
func (c *Client) ProbeTarget(ctx context.Context, s3 S3Config) error {
	pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_, err := c.listTree(pctx, s3, ownersDir+"-probe/")
	return err
}

// RunLockPath is where the overlap lock lives: beside the policy.
func RunLockPath(policyPath string) string {
	if policyPath != "" {
		return path.Dir(policyPath) + "/backup-run.lock"
	}
	if d := os.Getenv("HOME"); d != "" {
		return d + "/.config/teploy/backup-run.lock"
	}
	return "/tmp/teploy-backup-run.lock"
}
