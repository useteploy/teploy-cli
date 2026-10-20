package releasemeta

// list.go — reading EVERY release record of an app in one round trip,
// for `teploy releases` (server-authority plan D2): the list comes from
// the records this package writes, not from container inspection.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/ssh"
)

// listMarker delimits records in the combined stream. Write emits exactly
// one line of JSON per record, so a delimited cat of the directory is a
// valid stream even when a record was hand-edited to span lines.
const listMarker = "__TEPLOY_META__"

// listScript prints, for every *.json under the app's meta directory,
// the file name then its bytes. A missing directory prints nothing (an
// app deployed before this store existed has no records at all).
const listScript = `d=%s; [ -d "$d" ] || exit 0; for f in "$d"/*.json; do [ -f "$f" ] || continue; printf '\n` + listMarker + `%%s\n' "${f##*/}"; cat -- "$f"; done`

// List returns every release record for app, newest first (CreatedAt
// descending; equal timestamps fall back to hash order for determinism).
// Unparsable, wrong-schema or identity-mismatched files are returned as
// warning strings rather than failing the listing — one damaged record
// must not hide the releases a rollback needs.
func List(ctx context.Context, exec ssh.Executor, app string) ([]*Record, []string, error) {
	if err := config.ValidateName(app); err != nil {
		return nil, nil, err
	}
	dir := fmt.Sprintf("%s/%s/meta", deploymentsDir, app)
	out, err := exec.Run(ctx, fmt.Sprintf(listScript, ssh.ShellQuote(dir)))
	if err != nil {
		return nil, nil, fmt.Errorf("listing release records for %s: %w", app, err)
	}
	var records []*Record
	var warns []string
	// The transport may trim the output's leading newline, so a chunk can
	// begin with the marker itself; TrimLeft keeps the split robust
	// either way.
	for _, chunk := range strings.Split(out, listMarker) {
		chunk = strings.TrimLeft(chunk, "\n\r")
		if chunk == "" {
			continue
		}
		name, body, ok := strings.Cut(chunk, "\n")
		if !ok {
			warns = append(warns, fmt.Sprintf("meta/%s: empty record", name))
			continue
		}
		var rec Record
		if err := json.Unmarshal([]byte(strings.TrimSpace(body)), &rec); err != nil {
			warns = append(warns, fmt.Sprintf("meta/%s: unparsable (%v)", name, err))
			continue
		}
		want := rec.Hash + ".json"
		if rec.App != app || (name != want && name != rec.Hash) {
			warns = append(warns, fmt.Sprintf("meta/%s: record describes %s@%s — skipped", name, rec.App, rec.Hash))
			continue
		}
		if rec.SchemaVersion != SchemaVersion {
			warns = append(warns, fmt.Sprintf("meta/%s: schema version %d — skipped", name, rec.SchemaVersion))
			continue
		}
		records = append(records, &rec)
	}
	sort.Slice(records, func(i, j int) bool {
		a, b := records[i], records[j]
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.After(b.CreatedAt)
		}
		return a.Hash < b.Hash
	})
	return records, warns, nil
}
