package backup

import (
	"context"
	"errors"
	"fmt"
	"regexp"
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
	sidecars []pruneObject            // globals/, meta/, databases/<db>/meta/, teploy/ siblings
	incoming []pruneObject            // verification-pending uploads
}

func idOf(file string) string {
	id, _, _ := strings.Cut(file, ".")
	return id
}

// loadPruneTree lists the prefix across the current location and owned previous
// ones. Anything it does not recognise is never touched.
func (c *Client) loadPruneTree(ctx context.Context, app, prefix string, s3 S3Config) (pruneTree, error) {
	tree := pruneTree{series: map[string][]pruneObject{}}
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
			case len(segs) == 1:
				tree.root = append(tree.root, obj)
			case segs[0] == incomingDir:
				tree.incoming = append(tree.incoming, obj)
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
					tree.sidecars = append(tree.sidecars, pruneObject{key: "s3://" + loc.Bucket + "/" + k.Key, id: idOf(segs[len(segs)-1]), entry: e, loc: loc})
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
//   - With primary objects, the primary series is selected by the policy; each
//     database's objects follow their id's fate, except that a database's newest
//     good object always stays (a database whose dump has been failing keeps its
//     last good copy) and ids newer than the newest primary stay (a killed run's
//     extras are harmless until a newer primary supersedes them).
//   - Without primary objects (adopted containers), each database is its own
//     series, selected by the policy.
//   - Sidecars go when no surviving object has their id and they are older than
//     the newest backup.
func planPrune(tree pruneTree, p RetentionPolicy, now time.Time) (primaries, sidecars []pruneObject) {
	surviving := map[string]bool{}
	byName := func(objs []pruneObject, del []string) []pruneObject {
		names := map[string]bool{}
		for _, n := range del {
			names[n] = true
		}
		byEntry := map[string]pruneObject{}
		for _, o := range objs {
			if names[o.entry.name] {
				byEntry[o.entry.name] = o
			} else {
				surviving[o.id] = true
			}
		}
		out := make([]pruneObject, 0, len(del))
		for _, n := range del { // the selection's order: newest first
			if o, ok := byEntry[n]; ok {
				out = append(out, o)
			}
		}
		return out
	}
	rootEntries := entriesOf(tree.root)
	primaries = byName(tree.root, selectForDeletion(rootEntries, p, now))
	rootKept := map[string]bool{}
	var newestRoot time.Time
	for _, o := range tree.root {
		if surviving[o.id] {
			rootKept[o.id] = true
		}
		if !o.entry.bad && o.entry.time.After(newestRoot) {
			newestRoot = o.entry.time
		}
	}
	dbs := make([]string, 0, len(tree.series))
	for db := range tree.series {
		dbs = append(dbs, db)
	}
	sort.Strings(dbs)
	for _, db := range dbs {
		objs := tree.series[db]
		if newestRoot.IsZero() {
			primaries = append(primaries, byName(objs, selectForDeletion(entriesOf(objs), p, now))...)
			continue
		}
		keep := selectKept(entriesOf(objs), RetentionPolicy{}, now) // the newest good one
		var del []string
		for _, o := range objs {
			if rootKept[o.id] || keep[o.entry.name] || !o.entry.time.Before(newestRoot) {
				continue
			}
			del = append(del, o.entry.name)
		}
		primaries = append(primaries, byName(objs, del)...)
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

// CleanStaleIncoming removes pending uploads a killed run left under a prefix.
// Prune does this too; this is for a target with no retention rule. Best effort.
func (c *Client) CleanStaleIncoming(ctx context.Context, app, prefix string, s3 S3Config) {
	tree, err := c.loadPruneTree(ctx, app, prefix, s3)
	if err != nil {
		return
	}
	now := time.Now().UTC()
	for _, o := range tree.incoming {
		if now.Sub(o.entry.time) > tmpLeftoverMinutes*time.Minute {
			c.exec.Run(ctx, o.loc.AWS("s3 rm "+ssh.ShellQuote(o.key)))
		}
	}
}

// PruneBackups deletes backups under s3://<base>/<app>/<prefix>/ that fall
// outside the retention policy, with every sidecar they keep beside them,
// returning the deleted backups' names. A zero policy is a no-op. A namespace
// another server also writes to is refused. A refused delete (bucket locks,
// keys without delete rights) does not stop the rest: errors are returned
// together with what was deleted.
func (c *Client) PruneBackups(ctx context.Context, app, prefix string, s3 S3Config, p RetentionPolicy) ([]string, error) {
	if p.IsZero() {
		return nil, nil
	}
	if err := c.ensureAWSCLI(ctx); err != nil {
		return nil, err
	}
	if err := c.checkNamespaceOwner(ctx, s3, app); err != nil {
		return nil, err
	}
	tree, err := c.loadPruneTree(ctx, app, prefix, s3)
	if err != nil {
		return nil, err
	}
	primaries, sidecars := planPrune(tree, p, time.Now().UTC())

	var deleted []string
	var errs []error
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
