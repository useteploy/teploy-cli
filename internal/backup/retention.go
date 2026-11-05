package backup

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/useteploy/teploy/internal/ssh"
)

// RetentionPolicy decides which backups to keep. The zero value keeps
// everything (a prune with a zero policy is a no-op). The rules are a union
// over the GOOD backups, newest first, with restic `forget` semantics:
//
//   - KeepLast keeps the N most recent, regardless of age.
//   - KeepDaily, KeepWeekly, KeepMonthly each keep the most recent backup of
//     the last N calendar days, ISO weeks or months that have one (UTC, the
//     timezone backup ids are written in). A period with no backup does not
//     use up N, and one backup can satisfy several rules.
//   - MaxAgeDays keeps everything younger than that many days (restic's
//     keep-within): it is what makes "hourly for 48h, daily for a week" a
//     policy. Backups older than the window go, unless another rule keeps them.
//
// With only MaxAgeDays set, everything older than the window is deleted; with
// only KeepLast, everything past the first N. Independently of any rule, the
// newest good backup is never deleted.
type RetentionPolicy struct {
	KeepLast    int
	KeepDaily   int
	KeepWeekly  int
	KeepMonthly int
	MaxAgeDays  int
	// IgnoreClock lets a prune run despite a clock disagreement (--ignore-clock).
	IgnoreClock bool
}

// IsZero reports whether the policy prunes nothing.
func (p RetentionPolicy) IsZero() bool {
	return p.KeepLast <= 0 && p.KeepDaily <= 0 && p.KeepWeekly <= 0 && p.KeepMonthly <= 0 && p.MaxAgeDays <= 0
}

// backupTimestampRe pulls the 20060102-150405 timestamp out of a backup key.
// It matches both naming conventions in use: the Go path's bare
// "<timestamp>.tar.gz" and the scheduled shell path's
// "<app>-backup-<timestamp>.tar.gz", plus the accessory suffixes
// (.pgdump/.sql.gz/.archive.gz/.rdb.gz, each optionally .age).
var backupTimestampRe = regexp.MustCompile(`\d{8}-\d{6}`)

type backupEntry struct {
	name string
	time time.Time
	// bad marks an object below its engine's plausibility floor: an empty-output
	// failure that must never count as a good copy.
	bad bool
}

// parseBackupEntries extracts (name, timestamp) pairs from `aws s3 ls` output.
// Lines without a parseable timestamp (directory prefixes, stray output) are
// skipped so they can never be selected for deletion.
func parseBackupEntries(lsOutput string) []backupEntry {
	var entries []backupEntry
	for _, line := range strings.Split(strings.TrimSpace(lsOutput), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		name := fields[len(fields)-1]
		e, ok := newBackupEntry(name)
		if ok {
			entries = append(entries, e)
		}
	}
	return entries
}

func newBackupEntry(name string) (backupEntry, bool) {
	stamp := backupTimestampRe.FindString(name)
	if stamp == "" {
		return backupEntry{}, false
	}
	t, err := time.Parse("20060102-150405", stamp)
	if err != nil {
		return backupEntry{}, false
	}
	return backupEntry{name: name, time: t}, true
}

// selectKept returns the names the policy keeps among the good entries.
func selectKept(entries []backupEntry, p RetentionPolicy, now time.Time) map[string]bool {
	keep := map[string]bool{}
	var good []backupEntry
	for _, e := range entries {
		if !e.bad {
			good = append(good, e)
		}
	}
	sort.SliceStable(good, func(i, j int) bool {
		if !good[i].time.Equal(good[j].time) {
			return good[i].time.After(good[j].time)
		}
		return good[i].name > good[j].name
	})
	if len(good) == 0 {
		return keep
	}
	keep[good[0].name] = true // the newest good backup is never deleted

	var cutoff time.Time
	if p.MaxAgeDays > 0 {
		cutoff = now.Add(-time.Duration(p.MaxAgeDays) * 24 * time.Hour)
	}
	type rule struct {
		left   int
		bucket func(time.Time) string
		last   string
	}
	rules := []*rule{
		{left: p.KeepDaily, bucket: func(t time.Time) string { return t.UTC().Format("2006-01-02") }},
		{left: p.KeepWeekly, bucket: func(t time.Time) string {
			y, w := t.UTC().ISOWeek()
			return fmt.Sprintf("%d-%02d", y, w)
		}},
		{left: p.KeepMonthly, bucket: func(t time.Time) string { return t.UTC().Format("2006-01") }},
	}
	for i, e := range good {
		if i < p.KeepLast {
			keep[e.name] = true
		}
		if p.MaxAgeDays > 0 && !e.time.Before(cutoff) {
			keep[e.name] = true
		}
		for _, r := range rules {
			if r.left <= 0 {
				continue
			}
			if b := r.bucket(e.time); b != r.last {
				r.last = b
				r.left--
				keep[e.name] = true
			}
		}
	}
	return keep
}

// selectForDeletion returns the backup names to delete, newest first. Entries
// the policy keeps stay; so does a bad entry newer than the newest good one (it
// may still be somebody's in-flight state, and it is reported, not trusted). If
// there is no good entry at all, nothing is deleted.
func selectForDeletion(entries []backupEntry, p RetentionPolicy, now time.Time) []string {
	// A zero policy never deletes — guard here too, not only in PruneBackups,
	// so this can't silently wipe everything if called directly.
	if p.IsZero() {
		return nil
	}
	keep := selectKept(entries, p, now)
	if len(keep) == 0 {
		return nil
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].time.After(entries[j].time) })
	var newestGood time.Time
	for _, e := range entries {
		if !e.bad {
			newestGood = e.time
			break
		}
	}
	var out []string
	for _, e := range entries {
		if keep[e.name] {
			continue
		}
		if e.bad && !e.time.Before(newestGood) {
			continue
		}
		out = append(out, e.name)
	}
	return out
}

// pruneObject is one object in a prune's listing, with its bucket-relative key.
type pruneObject struct {
	key   string
	id    string
	entry backupEntry
	loc   S3Config
}

// pruneTree is a prefix's objects sorted into what prune decides on.
type pruneTree struct {
	root     []pruneObject            // primary objects
	series   map[string][]pruneObject // databases/<db>/<id>: per-database objects
	sidecars []pruneObject            // globals/, meta/, safety/, databases/<db>/meta/, teploy/ siblings
	incoming []pruneObject            // verification-pending uploads, at any depth
	// safety are ids of safety copies a destructive operation took, by the time
	// they were taken: protected for safetyWindow.
	safety map[string]time.Time
}

// safetyWindow is how long a safety copy outlasts every retention rule: long enough
// to notice that a restore or an upgrade went wrong, and go back.
const safetyWindow = 7 * 24 * time.Hour

// safetyDir marks an id as a safety copy: <prefix>/safety/<id>.safety.
const safetyDir = "safety"

// maxClockSkew is how far this machine's clock may be from the store's, and an
// object's date from now, before a prune refuses: selection is all arithmetic on
// those two numbers, and a wrong clock deletes real backups with a clean status.
const maxClockSkew = time.Hour

func idOf(file string) string {
	id, _, _ := strings.Cut(file, ".")
	return id
}

// loadPruneTree lists the prefix across the current location and owned previous
// ones. Anything it does not recognise is never touched.
func (c *Client) loadPruneTree(ctx context.Context, app, prefix string, s3 S3Config) (pruneTree, error) {
	tree := pruneTree{series: map[string][]pruneObject{}, safety: map[string]time.Time{}}
	for _, loc := range s3.locations() {
		if loc.isPrevious && !loc.previousOwned {
			continue
		}
		keys, err := c.listTree(ctx, loc, app+"/"+prefix+"/")
		if err != nil {
			return tree, err
		}
		for _, k := range keys {
			rel, ok := loc.relToBase(k.Key)
			if !ok {
				continue
			}
			rest, ok := strings.CutPrefix(rel, app+"/"+prefix+"/")
			if !ok {
				continue
			}
			segs := strings.Split(rest, "/")
			file := segs[len(segs)-1]
			e, ok := newBackupEntry(file)
			if !ok {
				continue
			}
			e.bad = k.Size < artifactFloor(file)
			obj := pruneObject{key: "s3://" + loc.Bucket + "/" + k.Key, id: idOf(file), entry: e, loc: loc}
			switch {
			case slices.Contains(segs, incomingDir):
				tree.incoming = append(tree.incoming, obj)
			case len(segs) == 1:
				tree.root = append(tree.root, obj)
			case len(segs) == 2 && segs[0] == safetyDir:
				tree.safety[obj.id] = e.time
				tree.sidecars = append(tree.sidecars, obj)
			case len(segs) == 3 && segs[0] == "databases":
				obj.entry.name = segs[1] + "/" + file
				tree.series[segs[1]] = append(tree.series[segs[1]], obj)
			case len(segs) == 4 && segs[0] == "databases" && segs[2] == "meta":
				tree.sidecars = append(tree.sidecars, obj)
			case len(segs) == 2 && (segs[0] == "globals" || segs[0] == "meta"):
				tree.sidecars = append(tree.sidecars, obj)
			}
		}
		// A volume backup's server-data sibling sits beside its prefix.
		if prefix == "volumes" {
			keys, err := c.listTree(ctx, loc, app+"/teploy/")
			if err != nil {
				return tree, err
			}
			for _, k := range keys {
				rel, ok := loc.relToBase(k.Key)
				if !ok || !strings.HasPrefix(rel, app+"/teploy/") {
					continue
				}
				segs := strings.Split(k.Key, "/")
				if e, ok := newBackupEntry(segs[len(segs)-1]); ok {
					obj := pruneObject{key: "s3://" + loc.Bucket + "/" + k.Key, id: idOf(segs[len(segs)-1]), entry: e, loc: loc}
					if slices.Contains(segs, incomingDir) {
						tree.incoming = append(tree.incoming, obj)
					} else {
						tree.sidecars = append(tree.sidecars, obj)
					}
				}
			}
		}
	}
	return tree, nil
}

func entriesOf(objs []pruneObject) []backupEntry {
	out := make([]backupEntry, len(objs))
	for i, o := range objs {
		out[i] = o.entry
	}
	return out
}

// planPrune decides what goes:
//
//   - The primary series and each database's series are each selected by the
//     policy on their own: a database failing in the last run of each day still
//     gets its keep_daily, and a primary that disappeared and came back does not
//     decide a database's history.
//   - An id the primary series keeps is also kept in every database's series (a
//     consistent point in time), as an extra keep and never a reason to delete.
//   - A safety copy a destructive operation took outlasts every rule for
//     safetyWindow.
//   - Sidecars go when no surviving object has their id and they are older than
//     the newest backup.
func planPrune(tree pruneTree, p RetentionPolicy, now time.Time) (primaries, sidecars []pruneObject) {
	surviving := map[string]bool{}
	protected := func(id string) bool {
		t, ok := tree.safety[id]
		return ok && now.Sub(t) < safetyWindow
	}
	pick := func(objs []pruneObject, keepToo func(pruneObject) bool) []pruneObject {
		del := selectForDeletion(entriesOf(objs), p, now)
		byEntry := map[string]pruneObject{}
		for _, o := range objs {
			byEntry[o.entry.name] = o
		}
		doomed := map[string]bool{}
		out := make([]pruneObject, 0, len(del))
		for _, n := range del { // the selection's order: newest first
			o := byEntry[n]
			if protected(o.id) || (keepToo != nil && keepToo(o)) {
				continue
			}
			doomed[n] = true
			out = append(out, o)
		}
		for _, o := range objs {
			if !doomed[o.entry.name] {
				surviving[o.id] = true
			}
		}
		return out
	}
	primaries = pick(tree.root, nil)
	rootKept := map[string]bool{}
	for id := range surviving {
		rootKept[id] = true
	}
	dbs := make([]string, 0, len(tree.series))
	for db := range tree.series {
		dbs = append(dbs, db)
	}
	sort.Strings(dbs)
	for _, db := range dbs {
		primaries = append(primaries, pick(tree.series[db], func(o pruneObject) bool { return rootKept[o.id] })...)
	}
	var newest time.Time
	for _, objs := range append([][]pruneObject{tree.root}, seriesValues(tree.series)...) {
		for _, o := range objs {
			if o.entry.time.After(newest) {
				newest = o.entry.time
			}
		}
	}
	for _, o := range tree.sidecars {
		if !surviving[o.id] && o.entry.time.Before(newest) {
			sidecars = append(sidecars, o)
		}
	}
	// Pending uploads a killed run left behind. Younger ones may be a live run.
	for _, o := range tree.incoming {
		if now.Sub(o.entry.time) > tmpLeftoverMinutes*time.Minute {
			sidecars = append(sidecars, o)
		}
	}
	return primaries, sidecars
}

func seriesValues(m map[string][]pruneObject) [][]pruneObject {
	out := make([][]pruneObject, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

// CleanStaleIncoming removes pending uploads a killed run left anywhere in a
// namespace, DR bundles included, whatever its retention. Best effort, and only in
// a namespace this server alone writes to.
func (c *Client) CleanStaleIncoming(ctx context.Context, app string, s3 S3Config) {
	owners, err := c.namespaceOwners(ctx, s3, app)
	fp := c.fingerprint(ctx)
	if err != nil || fp == "" || slices.ContainsFunc(owners, func(o string) bool { return o != fp }) {
		return
	}
	keys, err := c.listTree(ctx, s3, app+"/")
	if err != nil {
		return
	}
	now := time.Now().UTC()
	for _, k := range keys {
		segs := strings.Split(k.Key, "/")
		if !slices.Contains(segs, incomingDir) {
			continue
		}
		if e, ok := newBackupEntry(segs[len(segs)-1]); ok && now.Sub(e.time) > tmpLeftoverMinutes*time.Minute {
			c.exec.Run(ctx, s3.AWS("s3 rm "+ssh.ShellQuote("s3://"+s3.Bucket+"/"+k.Key)))
		}
	}
}

// ErrClockSkew marks a prune refused because dates cannot be trusted.
var ErrClockSkew = errors.New("clock disagreement")

// checkPruneClock refuses arithmetic on dates that cannot be right: this machine's
// clock against the store's, and any backup dated in the future.
func checkPruneClock(tree pruneTree, storeNow, now time.Time) error {
	if !storeNow.IsZero() {
		if d := now.Sub(storeNow); d > maxClockSkew || d < -maxClockSkew {
			return fmt.Errorf("%w: this machine's clock says %s and the store's says %s, so nothing was pruned (a wrong clock deletes real backups). Fix the clock, or pass --ignore-clock", ErrClockSkew, now.Format(time.RFC3339), storeNow.Format(time.RFC3339))
		}
	}
	for _, objs := range append([][]pruneObject{tree.root}, seriesValues(tree.series)...) {
		for _, o := range objs {
			if o.entry.time.Sub(now) > maxClockSkew {
				return fmt.Errorf("%w: %s is dated in the future (now %s), and would count as the newest backup, so nothing was pruned. Remove it if it is wrong, or pass --ignore-clock", ErrClockSkew, o.key, now.Format(time.RFC3339))
			}
		}
	}
	return nil
}

// PruneBackups deletes backups under s3://<base>/<app>/<prefix>/ that fall
// outside the retention policy, with every sidecar they keep beside them,
// returning the deleted backups' names. A zero policy is a no-op. A namespace this
// server cannot prove is its alone is refused, and so is a clock that cannot be
// trusted. A refused delete (bucket locks, keys without delete rights) does not
// stop the rest: errors are returned together with what was deleted.
func (c *Client) PruneBackups(ctx context.Context, app, prefix string, s3 S3Config, p RetentionPolicy) ([]string, error) {
	if p.IsZero() {
		return nil, nil
	}
	if err := c.ensureAWSCLI(ctx); err != nil {
		return nil, err
	}
	storeNow, err := c.claimNamespace(ctx, s3, app)
	if err != nil {
		return nil, err
	}
	// A previous location is checked on its own: owned means this server's old
	// objects, and a marker from another server there overrides that word.
	var errs []error
	cfg := s3
	cfg.PreviousPrefixes = nil
	for _, prev := range s3.PreviousPrefixes {
		if !prev.Owned {
			continue
		}
		loc := s3
		loc.Prefix, loc.PreviousPrefixes = prev.Prefix, nil
		if _, cerr := c.claimNamespace(ctx, loc, app); cerr != nil {
			errs = append(errs, cerr)
			continue
		}
		cfg.PreviousPrefixes = append(cfg.PreviousPrefixes, prev)
	}
	tree, err := c.loadPruneTree(ctx, app, prefix, cfg)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	if !p.IgnoreClock {
		if err := checkPruneClock(tree, storeNow, now); err != nil {
			return nil, err
		}
	}
	primaries, sidecars := planPrune(tree, p, now)

	var deleted []string
	for _, o := range primaries {
		if _, err := c.exec.Run(ctx, o.loc.AWS("s3 rm "+ssh.ShellQuote(o.key))); err != nil {
			errs = append(errs, fmt.Errorf("deleting %s: %w", o.key, err))
			continue
		}
		fmt.Fprintf(c.out, "Pruned %s\n", o.key)
		deleted = append(deleted, o.entry.name)
	}
	for _, o := range sidecars {
		if _, err := c.exec.Run(ctx, o.loc.AWS("s3 rm "+ssh.ShellQuote(o.key))); err != nil {
			errs = append(errs, fmt.Errorf("deleting %s: %w", o.key, err))
		}
	}
	return deleted, errors.Join(errs...)
}
