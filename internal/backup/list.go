package backup

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/useteploy/teploy/internal/ssh"
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
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	Accessory  string `json:"accessory,omitempty"`
	SizeBytes  int64  `json:"size_bytes"`
	CreatedAt  string `json:"created_at"`
	Key        string `json:"key"`
	Restorable bool   `json:"restorable"`
}

// listedIDRE matches the object names the CLI writes: the backup-id
// grammar (timestamp or timestamp+hex) with each branch's extension.
var listedIDRE = regexp.MustCompile(`^([0-9]{8}-[0-9]{6}(?:-[0-9a-f]{16})?)\.(tar\.gz|sql\.gz|archive\.gz|rdb\.gz)$`)

// ListBackupsRecursive lists every object under s3://<base>/<app>/ in one
// recursive call and classifies each by its key layout. Objects whose name
// does not match the CLI's naming (strays, partial uploads) are skipped.
func (c *Client) ListBackupsRecursive(ctx context.Context, app string, s3 S3Config) ([]ListedBackup, error) {
	if err := c.ensureAWSCLI(ctx); err != nil {
		return nil, err
	}
	root := fmt.Sprintf("s3://%s/%s/", s3.Base(), app)
	out, err := c.exec.Run(ctx, s3.AWS("s3 ls "+ssh.ShellQuote(root)+" --recursive"))
	if err != nil {
		return nil, fmt.Errorf("listing backups: %w", err)
	}
	var rows []ListedBackup
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		// Layout: DATE TIME SIZE KEY (SIZE is "0" or a plain integer; the
		// PRE pseudo-rows only appear in non-recursive listings).
		size, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil {
			continue
		}
		key := fields[3]
		row, ok := classifyListedKey(app, key)
		if !ok {
			continue
		}
		row.SizeBytes = size
		row.Key = root + key
		rows = append(rows, row)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].CreatedAt != rows[j].CreatedAt {
			return rows[i].CreatedAt > rows[j].CreatedAt
		}
		return rows[i].Key < rows[j].Key
	})
	return rows, nil
}

// classifyListedKey maps one recursive-ls key (relative to the bucket
// root, so "<app>/volumes/<id>.tar.gz") to a ListedBackup row.
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
		if len(segments) != 4 {
			return row, false
		}
		row.Kind = KindAccessory
		row.Accessory = segments[2]
	default:
		return row, false
	}
	return row, true
}

// EngineOf classifies an accessory image for restore-time decisions: the
// generic (tar) branch is the only one whose restore replaces files on
// disk, so it is the only one where the accessory container itself must be
// stopped (the engine branches restore through a running engine; redis
// stops and restarts itself inside its restore script).
func EngineOf(image string) string {
	switch {
	case isDBType(image, "postgres"):
		return "postgres"
	case isDBType(image, "mysql"):
		return "mysql"
	case isDBType(image, "mariadb"):
		return "mariadb"
	case isDBType(image, "mongo"):
		return "mongo"
	case isDBType(image, "redis"):
		return "redis"
	default:
		return "generic"
	}
}
