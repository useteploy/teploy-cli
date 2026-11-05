package backup

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/useteploy/teploy/internal/events"
	"github.com/useteploy/teploy/internal/ssh"
)

// Fleet status: one read-only call Dash's backups page builds on, and the facts
// doctor checks. Env is read for detection and never reported; heartbeat URLs
// appear only as a count and hosts; credentials only as a boolean.

// Coverage states.
const (
	CoverageCovered   = "covered"
	CoverageStale     = "stale"
	CoverageFailing   = "failing"
	CoverageUncovered = "uncovered"
	CoverageExcluded  = "excluded"
	CoverageAdopted   = "adopted"
)

// StatusObject is the newest good object of a series.
type StatusObject struct {
	ID        string `json:"id"`
	Key       string `json:"key"`
	SizeBytes int64  `json:"size_bytes"`
	CreatedAt string `json:"created_at"`
	Encrypted bool   `json:"encrypted"`
	Previous  bool   `json:"previous_prefix,omitempty"`
}

// StatusVerify is the last verify-backup of a target.
type StatusVerify struct {
	At     time.Time `json:"at"`
	OK     bool      `json:"ok"`
	ID     string    `json:"id,omitempty"`
	Metric string    `json:"metric,omitempty"`
	// Database is set when the verify was of one database's series.
	Database string `json:"database,omitempty"`
}

// StatusDatabase is one database with its own series.
type StatusDatabase struct {
	Name   string        `json:"name"`
	Newest *StatusObject `json:"newest,omitempty"`
}

// StatusEntry is one DB-like container, or one app's volumes.
type StatusEntry struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"` // accessory | adopted | unmanaged | volumes
	App       string `json:"app,omitempty"`
	Accessory string `json:"accessory,omitempty"`
	Engine    string `json:"engine,omitempty"`
	Detection string `json:"detection,omitempty"`
	State     string `json:"state,omitempty"`
	// Unmanaged: no teploy labels. Adopted: a policy entry covers it anyway.
	Unmanaged bool `json:"unmanaged"`
	Adopted   bool `json:"adopted"`
	// Coverage: covered | stale | failing | uncovered | excluded | adopted.
	Coverage string `json:"coverage"`
	Reason   string `json:"reason,omitempty"`
	// Schedule: policy | own (a per-app cron line) | none.
	Schedule    string     `json:"schedule"`
	LastSuccess *time.Time `json:"last_success,omitempty"`
	LastFailure *time.Time `json:"last_failure,omitempty"`
	// LastError is the last failure's message (event-log bounded, no secrets).
	LastError          string           `json:"last_error,omitempty"`
	Newest             *StatusObject    `json:"newest,omitempty"`
	Databases          []StatusDatabase `json:"databases,omitempty"`
	LastVerify         *StatusVerify    `json:"last_verify,omitempty"`
	UncoveredDatabases []string         `json:"uncovered_databases,omitempty"`
	// SizeBytes is the measured tree (volumes, generic accessories).
	SizeBytes int64 `json:"size_bytes,omitempty"`
}

// StatusTarget summarizes the stored target (never credentials).
type StatusTarget struct {
	Configured       bool             `json:"configured"`
	Bucket           string           `json:"bucket,omitempty"`
	Region           string           `json:"region,omitempty"`
	Endpoint         string           `json:"endpoint,omitempty"`
	Prefix           string           `json:"prefix,omitempty"`
	HasCredentials   bool             `json:"has_credentials"`
	PreviousPrefixes []PreviousPrefix `json:"previous_prefixes,omitempty"`
	// Reachable says the listing this report needed worked.
	Reachable bool   `json:"reachable"`
	Error     string `json:"error,omitempty"`
}

// StatusPolicy summarizes the policy (no heartbeat tokens).
type StatusPolicy struct {
	Configured     bool           `json:"configured"`
	Path           string         `json:"path,omitempty"`
	Interval       string         `json:"interval,omitempty"`
	CronInstalled  bool           `json:"cron_installed"`
	Retention      FleetRetention `json:"retention"`
	Excludes       []string       `json:"excludes,omitempty"`
	Adopted        []AdoptedEntry `json:"adopted,omitempty"`
	Volumes        bool           `json:"volumes"`
	VolumeMaxGB    float64        `json:"volume_max_gb,omitempty"`
	HeartbeatCount int            `json:"heartbeat_count"`
	HeartbeatHosts []string       `json:"heartbeat_hosts,omitempty"`
	// Recipient is the owner's recovery PUBLIC key.
	Recipient string `json:"recipient,omitempty"`
}

// StatusLastRun is the last `backup run --all`.
type StatusLastRun struct {
	At            time.Time `json:"at"`
	Status        string    `json:"status"`
	TargetsOK     int       `json:"targets_ok"`
	TargetsFailed int       `json:"targets_failed"`
	Omissions     []string  `json:"omissions,omitempty"`
	Error         string    `json:"error,omitempty"`
	Stale         bool      `json:"stale"`
}

// FleetStatus is the whole report.
type FleetStatus struct {
	Target StatusTarget `json:"target"`
	Policy StatusPolicy `json:"policy"`
	// Encrypted says new objects will be encrypted (a recovery key is set).
	Encrypted bool `json:"encrypted"`
	AWSCLI    bool `json:"aws_cli"`
	AgeCLI    bool `json:"age_cli"`
	// RunLock names a run in progress.
	LastRun *StatusLastRun `json:"last_run,omitempty"`
	Entries []StatusEntry  `json:"entries"`
	// UncoveredDatabases lists every database, across containers, no backup
	// holds: "<container>/<database>", or "<container>" when nothing covers it.
	UncoveredDatabases []string `json:"uncovered_databases"`
	// LeftoverWorkspaces are temp directories killed runs left.
	LeftoverWorkspaces []string `json:"leftover_workspaces,omitempty"`
	// LeftoverUploads are bucket keys of uploads a killed run never published.
	LeftoverUploads []string `json:"leftover_uploads,omitempty"`
	Warnings        []string `json:"warnings,omitempty"`
}

// ReadFleetStatus builds the report. A failure to read one thing (the bucket,
// one container's databases) is reported in the report; only a failure to read
// the server itself is an error.
func ReadFleetStatus(ctx context.Context, exec ssh.Executor, now time.Time) (FleetStatus, error) {
	c := NewClient(exec, io.Discard)
	st := FleetStatus{Entries: []StatusEntry{}, UncoveredDatabases: []string{}}

	policy, err := ReadFleetPolicy(ctx, exec)
	if err != nil {
		return st, err
	}
	target, err := ReadTarget(ctx, exec)
	if err != nil {
		return st, err
	}
	st.Policy = StatusPolicy{
		Configured: policy.Configured(), Path: policy.Path, Interval: policy.Interval, Retention: policy.Retention,
		Excludes: policy.Excludes, Adopted: policy.Adopted, Volumes: policy.Volumes, VolumeMaxGB: policy.VolumeMaxGB,
		HeartbeatCount: len(policy.HeartbeatURLs), Recipient: policy.Recipient,
	}
	for _, h := range MaskedHeartbeats(policy.HeartbeatURLs) {
		st.Policy.HeartbeatHosts = append(st.Policy.HeartbeatHosts, strings.TrimSuffix(h, "/..."))
	}
	st.Encrypted = policy.Recipient != ""
	st.Target = StatusTarget{
		Configured: target.Bucket != "", Bucket: target.Bucket, Region: target.Region, Endpoint: target.Endpoint,
		Prefix: target.Prefix, HasCredentials: target.HasCredentials, PreviousPrefixes: target.PreviousPrefixes,
	}
	if out, err := exec.Run(ctx, "command -v aws >/dev/null 2>&1 && echo aws; command -v age >/dev/null 2>&1 && echo age; true"); err == nil {
		for _, t := range splitLines(out) {
			st.AWSCLI = st.AWSCLI || t == "aws"
			st.AgeCLI = st.AgeCLI || t == "age"
		}
	}
	ownCron := map[string]bool{}
	if cron, err := ReadCrontab(ctx, exec); err == nil {
		for _, line := range cron {
			if m := FindCronMarker(line); m != nil {
				if m.Kind == KindFleet {
					st.Policy.CronInstalled = true
				} else if m.Form != CronFormCustom {
					ownCron[m.App+"/"+m.Accessory] = true
				}
			}
		}
	}
	facts, err := c.Inventory(ctx)
	if err != nil {
		return st, err
	}
	adopted := map[string]*AdoptedEntry{}
	for i := range policy.Adopted {
		adopted[policy.Adopted[i].Container] = &policy.Adopted[i]
	}

	// One listing and one event log per namespace.
	var cfg S3Config
	if target.Bucket != "" {
		region := target.Region
		if region == "" {
			region = "us-east-1"
		}
		cfg = S3Config{Bucket: target.Bucket, Region: region, Endpoint: target.Endpoint, Prefix: target.Prefix, StoredTarget: target.Path, PreviousPrefixes: target.PreviousPrefixes}
	}
	if target.Bucket != "" && st.AWSCLI {
		if err := c.ProbeTarget(ctx, cfg); err != nil {
			st.Target.Error = oneLine(err.Error())
		} else {
			st.Target.Reachable = true
		}
	}
	listed := map[string][]ListedBackup{}
	listNS := func(app string) []ListedBackup {
		if rows, ok := listed[app]; ok {
			return rows
		}
		var rows []ListedBackup
		if st.Target.Reachable {
			lctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			defer cancel()
			r, pending, err := c.listBackupsAndPending(lctx, app, cfg)
			if err != nil {
				st.Target.Reachable = false
				st.Target.Error = oneLine(err.Error())
			}
			rows = r
			for _, k := range pending {
				segs := strings.Split(k, "/")
				if e, ok := newBackupEntry(segs[len(segs)-1]); ok && now.Sub(e.time) > tmpLeftoverMinutes*time.Minute {
					st.LeftoverUploads = append(st.LeftoverUploads, k)
				}
			}
		}
		listed[app] = rows
		return rows
	}
	logs := map[string][]events.Event{}
	logNS := func(app string) []events.Event {
		if evs, ok := logs[app]; ok {
			return evs
		}
		evs, err := events.Read(ctx, exec, app, 0)
		if err != nil {
			st.Warnings = append(st.Warnings, fmt.Sprintf("could not read the %s event log: %v", app, err))
		}
		logs[app] = evs
		return evs
	}

	stale := 2 * policy.IntervalDuration()
	decide := func(e *StatusEntry, excluded bool, covered bool) {
		switch {
		case excluded:
			e.Coverage = CoverageExcluded
		case !covered:
			e.Coverage = CoverageUncovered
		case e.LastFailure != nil && (e.LastSuccess == nil || e.LastFailure.After(*e.LastSuccess)):
			e.Coverage = CoverageFailing
		case e.LastSuccess == nil:
			e.Coverage, e.Reason = CoverageStale, "no backup has succeeded yet"
		default:
			limit := stale
			if e.Schedule == "own" || limit == 0 {
				limit = 48 * time.Hour
			}
			if now.Sub(*e.LastSuccess) > limit {
				e.Coverage, e.Reason = CoverageStale, fmt.Sprintf("the last success is older than %s", limit)
			} else if e.Adopted {
				e.Coverage = CoverageAdopted
			} else {
				e.Coverage = CoverageCovered
			}
		}
	}

	for i := range facts {
		f := &facts[i]
		if isScratch(f.Name) {
			continue
		}
		a, isAdopted := adopted[f.Name]
		isAccessory := f.Role == "accessory" && f.App != "" && f.Accessory != "" && f.Name == ContainerName(f.App, f.Accessory)
		if !isAdopted && !isAccessory && (f.EngineErr != nil || !f.Engine.IsDB()) {
			continue
		}
		e := StatusEntry{Name: f.Name, Engine: f.Engine.Engine, Detection: f.Engine.Source, State: f.State, Schedule: "none", Unmanaged: f.Unmanaged(), Adopted: isAdopted}
		var app, name string
		switch {
		case isAdopted:
			e.Kind, app, name = TargetAdopted, AdoptedApp, f.Name
			if a.Engine != "" {
				e.Engine, e.Detection = a.Engine, "policy"
			}
			e.Accessory = f.Name
		case isAccessory:
			e.Kind, app, name = TargetAccessory, f.App, f.Accessory
			e.App, e.Accessory = f.App, f.Accessory
		default:
			e.Kind = "unmanaged"
		}
		excluded := policy.Excluded(app, name, f.Name)
		covered := false
		switch {
		case isAdopted:
			covered, e.Schedule = policy.Configured(), "policy"
		case isAccessory && ownCron[f.App+"/"+f.Accessory]:
			covered, e.Schedule = true, "own"
		case isAccessory && policy.Configured() && policy.Interval != "":
			covered, e.Schedule = true, "policy"
		}
		if app != "" {
			fillEntryEvents(&e, logNS(app), name)
			fillEntryObjects(&e, listNS(app), name)
		}
		decide(&e, excluded, covered)
		if e.Coverage == CoverageUncovered && e.Reason == "" {
			if e.Kind == "unmanaged" {
				e.Reason = "no policy entry covers this container (adopt it or exclude it)"
			} else {
				e.Reason = "no backup policy or schedule covers this accessory"
			}
		}
		if e.Coverage == CoverageUncovered {
			st.UncoveredDatabases = append(st.UncoveredDatabases, f.Name)
		}
		// What the server holds beyond what this entry's backups cover.
		if !excluded && covered && f.State == "running" && app != "" {
			opts := BackupOptions{AllDatabases: e.Schedule == "policy"}
			if isAdopted && !a.Databases.All {
				opts = BackupOptions{Databases: a.Databases.Names}
			}
			e.UncoveredDatabases = c.statusUncovered(ctx, app, name, f, e.Engine, opts, &st)
			for _, db := range e.UncoveredDatabases {
				st.UncoveredDatabases = append(st.UncoveredDatabases, f.Name+"/"+db)
			}
		}
		st.Entries = append(st.Entries, e)
	}
	for _, a := range policy.Adopted {
		found := false
		for _, f := range facts {
			found = found || f.Name == a.Container
		}
		if !found {
			e := StatusEntry{Name: a.Container, Kind: TargetAdopted, Accessory: a.Container, Engine: a.Engine, Adopted: true, Unmanaged: true, Schedule: "policy",
				Coverage: CoverageFailing, Reason: "adopted container does not exist on this server", State: "missing"}
			fillEntryEvents(&e, logNS(AdoptedApp), a.Container)
			fillEntryObjects(&e, listNS(AdoptedApp), a.Container)
			st.Entries = append(st.Entries, e)
		}
	}
	if trees, err := c.listVolumeTrees(ctx); err != nil {
		st.Warnings = append(st.Warnings, err.Error())
	} else {
		for _, tr := range trees {
			e := StatusEntry{Name: tr.App + "/volumes", Kind: TargetVolumes, App: tr.App, Schedule: "none", SizeBytes: tr.KB * 1024}
			guard := policy.VolumeMaxGB > 0 && float64(e.SizeBytes) > policy.VolumeMaxGB*(1<<30)
			excluded := policy.Excluded(tr.App, "volumes", "") || guard
			covered := false
			switch {
			case ownCron[tr.App+"/"]:
				covered, e.Schedule = true, "own"
			case policy.Volumes && policy.Interval != "":
				covered, e.Schedule = true, "policy"
			}
			fillEntryEvents(&e, logNS(tr.App), "")
			fillEntryObjects(&e, listNS(tr.App), "")
			decide(&e, excluded, covered)
			if guard {
				e.Reason = fmt.Sprintf("%.2f GB is over the policy's volume_max_gb %.2f", float64(e.SizeBytes)/(1<<30), policy.VolumeMaxGB)
			}
			st.Entries = append(st.Entries, e)
		}
	}
	// The last fleet run.
	for _, ev := range logNS(FleetApp) {
		if ev.Kind != events.KindBackupAll {
			continue
		}
		run := &StatusLastRun{At: ev.TS, Status: detailString(ev.Detail, "status"), TargetsOK: detailInt(ev.Detail, "targets_ok"), TargetsFailed: detailInt(ev.Detail, "targets_failed"), Error: detailString(ev.Detail, "error")}
		if list, ok := ev.Detail["omissions"].([]any); ok {
			for _, o := range list {
				if s, ok := o.(string); ok {
					run.Omissions = append(run.Omissions, s)
				}
			}
		}
		st.LastRun = run
	}
	if st.LastRun != nil && stale > 0 && now.Sub(st.LastRun.At) > stale {
		st.LastRun.Stale = true
	}
	if out, err := exec.Run(ctx, LeftoverWorkspacesProbe()); err == nil {
		st.LeftoverWorkspaces = splitLines(out)
	}
	sort.Strings(st.UncoveredDatabases)
	return st, nil
}

// statusUncovered reads what a running database server holds beyond this
// entry's coverage. Read-only (coverageWith's mongo cleanup is not reached).
func (c *Client) statusUncovered(ctx context.Context, app, name string, f *ContainerFact, engine string, opts BackupOptions, st *FleetStatus) []string {
	if engine != EnginePostgres && engine != EngineMySQL && engine != EngineMariaDB {
		return nil
	}
	qctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var t pgTarget
	if engine == EnginePostgres {
		db, user := postgresDBAndUser(app, f.Env)
		if app == AdoptedApp {
			db = ""
		}
		t = newPGTarget(f.Name, db, user, f.Env)
	}
	cov, err := c.coverageWith(qctx, app, name, EngineDetection{Engine: engine}, t, f.Env, opts)
	if err != nil {
		st.Warnings = append(st.Warnings, fmt.Sprintf("%s: %v", f.Name, err))
		return nil
	}
	return append(cov.Uncovered, cov.Missing...)
}

// fillEntryEvents sets the success, failure and verify facts from an event log.
// name is the accessory ("" for an app's volumes). Events are oldest first.
func fillEntryEvents(e *StatusEntry, evs []events.Event, name string) {
	for _, ev := range evs {
		acc := detailString(ev.Detail, "accessory")
		if acc != name {
			continue
		}
		switch ev.Kind {
		case events.KindBackupRun:
			if name == "" && detailString(ev.Detail, "kind") != KindVolumes {
				continue
			}
			ts := ev.TS
			switch ev.Outcome {
			case events.Succeeded:
				e.LastSuccess = &ts
				// An older failure's error is no longer the current story, but
				// the timestamp stays: it says the target has failed before.
			case events.Failed:
				e.LastFailure = &ts
				e.LastError = detailString(ev.Detail, "error")
			}
		case events.KindBackupVerify:
			e.LastVerify = &StatusVerify{At: ev.TS, OK: ev.Outcome == events.Succeeded, ID: detailString(ev.Detail, "date"), Metric: detailString(ev.Detail, "metric"), Database: detailString(ev.Detail, "database")}
		}
	}
}

// fillEntryObjects sets the newest object of each series from a listing (newest
// first). Below-floor objects are not Restorable and never count.
func fillEntryObjects(e *StatusEntry, rows []ListedBackup, name string) {
	seen := map[string]bool{}
	for _, r := range rows {
		switch {
		case name == "" && r.Kind != KindVolumes:
			continue
		case name != "" && (r.Kind != KindAccessory || r.Accessory != name):
			continue
		case !r.Restorable:
			continue
		}
		if seen[r.Database] {
			continue
		}
		seen[r.Database] = true
		obj := &StatusObject{ID: r.ID, Key: r.Key, SizeBytes: r.SizeBytes, CreatedAt: r.CreatedAt, Encrypted: r.Encrypted, Previous: r.Previous}
		if r.Database == "" {
			e.Newest = obj
		} else {
			e.Databases = append(e.Databases, StatusDatabase{Name: r.Database, Newest: obj})
			if e.Newest == nil || r.CreatedAt > e.Newest.CreatedAt {
				e.Newest = obj
			}
		}
	}
	sort.Slice(e.Databases, func(i, j int) bool { return e.Databases[i].Name < e.Databases[j].Name })
	// Without any event yet, a good object is still evidence of a success: the
	// history may predate the event log.
	if e.LastSuccess == nil && e.Newest != nil {
		if t, err := time.Parse(time.RFC3339, e.Newest.CreatedAt); err == nil {
			e.LastSuccess = &t
		}
	}
}

func detailString(d map[string]any, k string) string {
	if s, ok := d[k].(string); ok {
		return s
	}
	return ""
}

func detailInt(d map[string]any, k string) int {
	if n, ok := d[k].(float64); ok {
		return int(n)
	}
	return 0
}
