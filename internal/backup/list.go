package backup

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Listing (D3): `backup list --json` reports parsed rows across the
// volumes, accessories and teploy prefixes, not raw `aws s3 ls` lines.

// BackupKind names the prefix family a listed backup belongs to.
const (
	KindVolumes    = "volumes"
	KindAccessory  = "accessory"
	KindTeployData = "teploy"
)

// ListedBackup is one parsed object under the app's S3 layout.
type ListedBackup struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Accessory string `json:"accessory,omitempty"`
	// Database names one database's own object (`backup restore --database`);
	// empty for an accessory's primary object.
	Database   string `json:"database,omitempty"`
	SizeBytes  int64  `json:"size_bytes"`
	CreatedAt  string `json:"created_at"`
	Key        string `json:"key"`
	Restorable bool   `json:"restorable"`
	// Encrypted says the object is age-encrypted (.age).
	Encrypted bool `json:"encrypted"`
	// Previous says the object sits under an old prefix of the bucket: found
	// and restorable, never pruned unless that prefix is marked owned.
	Previous bool `json:"previous_prefix,omitempty"`
}

// listedIDRE matches the object names the CLI writes: the backup-id
// grammar (timestamp or timestamp+hex) with each branch's extension.
var listedIDRE = regexp.MustCompile(`^([0-9]{8}-[0-9]{6}(?:-[0-9a-f]{16})?)\.(tar\.gz|pgdump|sql\.gz|archive\.gz|rdb\.gz)(\.age)?$`)

// ListBackupsRecursive lists every object under s3://<base>/<app>/ in one
// recursive call per location (current prefix, then any previous one) and
// classifies each by its key layout. Objects whose name does not match the
// CLI's naming (strays, pending uploads) are skipped, and so are objects below
// their engine's plausibility floor: neither is a backup anyone should choose.
func (c *Client) ListBackupsRecursive(ctx context.Context, app string, s3 S3Config) ([]ListedBackup, error) {
	rows, _, err := c.listBackupsAndPending(ctx, app, s3)
	return rows, err
}

// listBackupsAndPending is ListBackupsRecursive plus the bucket-relative keys of
// verification-pending uploads, which status and doctor report.
func (c *Client) listBackupsAndPending(ctx context.Context, app string, s3 S3Config) ([]ListedBackup, []string, error) {
	if err := c.ensureAWSCLI(ctx); err != nil {
		return nil, nil, err
	}
	var rows []ListedBackup
	var pending []string
	for _, loc := range s3.locations() {
		keys, err := c.listTree(ctx, loc, app+"/")
		if err != nil {
			return nil, nil, fmt.Errorf("listing backups: %w", err)
		}
		for _, k := range keys {
			rel, ok := loc.relToBase(k.Key)
			if !ok {
				continue
			}
			if strings.Contains(rel, "/"+incomingDir+"/") {
				pending = append(pending, k.Key)
				continue
			}
			row, ok := classifyListedKey(app, rel)
			if !ok {
				continue
			}
			row.SizeBytes = k.Size
			if k.Size < artifactFloor(k.Key) {
				row.Restorable = false
			}
			// Listing keys are relative to the BUCKET root (they already carry
			// the prefix), so the full URI is rebuilt from the bucket.
			row.Key = "s3://" + loc.Bucket + "/" + k.Key
			row.Previous = loc.isPrevious
			rows = append(rows, row)
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].CreatedAt != rows[j].CreatedAt {
			return rows[i].CreatedAt > rows[j].CreatedAt
		}
		return rows[i].Key < rows[j].Key
	})
	return rows, pending, nil
}

// classifyListedKey maps one key relative to the base (the bucket root, or the
// configured prefix) to a ListedBackup row: "<app>/volumes/<id>.tar.gz",
// "<app>/accessories/<name>/<id><ext>" or
// "<app>/accessories/<name>/databases/<db>/<id><ext>".
func classifyListedKey(app, key string) (ListedBackup, bool) {
	var row ListedBackup
	segments := strings.Split(key, "/")
	if len(segments) < 3 || segments[0] != app {
		return row, false
	}
	m := listedIDRE.FindStringSubmatch(segments[len(segments)-1])
	if m == nil {
		return row, false
	}
	id := m[1]
	row.ID = id
	row.Encrypted = m[3] != ""
	if ts, err := time.Parse("20060102-150405", id[:15]); err == nil {
		row.CreatedAt = ts.UTC().Format(time.RFC3339)
	}
	row.Restorable = ValidateDate(id) == nil
	switch segments[1] {
	case "volumes":
		if len(segments) != 3 || m[2] != "tar.gz" {
			return row, false
		}
		row.Kind = KindVolumes
	case "teploy":
		if len(segments) != 3 || m[2] != "tar.gz" {
			return row, false
		}
		row.Kind = KindTeployData
		// Listed for visibility (the server-config sibling of a volume
		// backup), but `backup restore` restores volumes, not this row.
		row.Restorable = false
	case "accessories":
		switch {
		case len(segments) == 4:
		case len(segments) == 6 && segments[3] == "databases":
			row.Database = decodeDBSegment(segments[4])
		default:
			return row, false
		}
		row.Kind = KindAccessory
		row.Accessory = segments[2]
	default:
		return row, false
	}
	return row, true
}
