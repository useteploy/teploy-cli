package backup

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/useteploy/teploy/internal/ssh"
)

// RetentionPolicy decides which backups to keep. The zero value keeps
// everything (a prune with a zero policy is a no-op).
//
// KeepLast is a floor: the N most recent backups are ALWAYS kept, regardless
// of age. MaxAgeDays deletes backups older than that many days — but never
// drops below the KeepLast floor. Setting only MaxAgeDays (KeepLast 0) will
// delete every backup older than the window, so pair it with KeepLast unless
// you truly want no age-based floor.
type RetentionPolicy struct {
	KeepLast   int
	MaxAgeDays int
}

// IsZero reports whether the policy prunes nothing.
func (p RetentionPolicy) IsZero() bool {
	return p.KeepLast <= 0 && p.MaxAgeDays <= 0
}

// backupTimestampRe pulls the 20060102-150405 timestamp out of a backup key.
// It matches both naming conventions in use: the Go path's bare
// "<timestamp>.tar.gz" and the scheduled shell path's
// "<app>-backup-<timestamp>.tar.gz", plus the accessory suffixes
// (.sql.gz/.archive.gz/.rdb.gz).
var backupTimestampRe = regexp.MustCompile(`\d{8}-\d{6}`)

type backupEntry struct {
	name string
	time time.Time
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
		stamp := backupTimestampRe.FindString(name)
		if stamp == "" {
			continue
		}
		t, err := time.Parse("20060102-150405", stamp)
		if err != nil {
			continue
		}
		entries = append(entries, backupEntry{name: name, time: t})
	}
	return entries
}

// selectForDeletion returns the backup names to delete, newest-first order
// preserved. The newest KeepLast entries are never returned (the floor).
func selectForDeletion(entries []backupEntry, p RetentionPolicy, now time.Time) []string {
	// A zero policy never deletes — guard here too, not only in PruneBackups,
	// so this can't silently wipe everything if called directly.
	if p.IsZero() {
		return nil
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].time.After(entries[j].time) })

	protected := p.KeepLast
	if protected < 0 {
		protected = 0
	}

	var cutoff time.Time
	if p.MaxAgeDays > 0 {
		cutoff = now.Add(-time.Duration(p.MaxAgeDays) * 24 * time.Hour)
	}

	var out []string
	for i, e := range entries {
		if i < protected {
			continue // always keep the newest N
		}
		if p.MaxAgeDays > 0 {
			if e.time.Before(cutoff) {
				out = append(out, e.name)
			}
			continue
		}
		// Count-based only: everything beyond the floor goes.
		out = append(out, e.name)
	}
	return out
}

// PruneBackups deletes backups under s3://<bucket>/<app>/<prefix>/ that fall
// outside the retention policy, returning the deleted keys' names. A zero
// policy is a no-op. Errors stop pruning and return what was deleted so far.
func (c *Client) PruneBackups(ctx context.Context, app, prefix string, s3 S3Config, p RetentionPolicy) ([]string, error) {
	if p.IsZero() {
		return nil, nil
	}

	lines, err := c.ListBackups(ctx, app, prefix, s3)
	if err != nil {
		return nil, err
	}

	entries := parseBackupEntries(strings.Join(lines, "\n"))
	toDelete := selectForDeletion(entries, p, time.Now().UTC())

	var deleted []string
	for _, name := range toDelete {
		key := fmt.Sprintf("s3://%s/%s/%s/%s", s3.Bucket, app, prefix, name)
		if _, err := c.exec.Run(ctx, s3.AWS("s3 rm "+ssh.ShellQuote(key))); err != nil {
			return deleted, fmt.Errorf("deleting %s: %w", name, err)
		}
		fmt.Fprintf(c.out, "Pruned %s\n", key)
		deleted = append(deleted, name)
	}
	return deleted, nil
}
