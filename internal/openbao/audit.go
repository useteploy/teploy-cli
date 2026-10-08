package openbao

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/useteploy/teploy/internal/accessories"
	teplaudit "github.com/useteploy/teploy/internal/audit"
	"github.com/useteploy/teploy/internal/state"
)

// OpenBaoAuditEntry is the subset of an OpenBao audit log line we forward.
type OpenBaoAuditEntry struct {
	Time string `json:"time"`
	Type string `json:"type"` // "request" | "response"
	Auth struct {
		DisplayName   string `json:"display_name"`
		PolicyResults struct {
			Allowed bool `json:"allowed"`
		} `json:"policy_results"`
	} `json:"auth"`
	Request struct {
		Operation     string `json:"operation"`
		Path          string `json:"path"`
		MountType     string `json:"mount_type"`
		RemoteAddress string `json:"remote_address"`
	} `json:"request"`
	Error string `json:"error"`
}

// ToObserveEvent transforms an OpenBao audit entry into a Teploy audit event
// (pure/testable). Returns (event, true) for entries worth forwarding, or
// ok=false to skip (non-response records, or non-secret system paths). Only
// "response" records are forwarded — they carry the allow/deny result, and
// forwarding both request+response would double every access.
func ToObserveEvent(app string, e OpenBaoAuditEntry) (teplaudit.Event, bool) {
	if e.Type != "response" {
		return teplaudit.Event{}, false
	}
	// Skip OpenBao's own token/system bookkeeping; keep secret/DB access.
	switch e.Request.MountType {
	case "kv", "database":
	default:
		return teplaudit.Event{}, false
	}

	actor := e.Auth.DisplayName
	if actor == "" {
		actor = "unknown"
	}
	res := "success"
	if e.Error != "" || (e.Request.Operation != "" && !e.Auth.PolicyResults.Allowed) {
		res = "denied"
	}

	op := e.Request.Operation
	if op == "" {
		op = "access"
	}
	return teplaudit.Event{
		Actor:  actor,
		Action: "vault." + e.Request.MountType + "." + op,
		Target: e.Request.Path,
		Result: res,
		Metadata: map[string]any{
			"mount":  e.Request.MountType,
			"vault":  app,
			"source": e.Request.RemoteAddress,
			"time":   e.Time,
		},
	}, true
}

// ShipAudit reads new OpenBao audit-log entries and forwards them to an observe
// instance's tamper-evident trail. It tracks how many lines have already been
// shipped (in a server-side marker file) so repeated runs are idempotent and
// only new access events are forwarded. Returns the number shipped.
func (c *Client) ShipAudit(ctx context.Context, app, accessory, observeEndpoint, observeToken, observeSite string) (shipped int, retErr error) {
	lock, err := state.AcquireLockFenced(ctx, c.exec, app)
	if err != nil {
		return 0, err
	}
	defer state.ReleaseLockFenced(c.exec, lock, app)
	lock.StartRenewal(c.exec)
	if accessory == "" {
		accessory = defaultAccessory
	}
	if observeEndpoint == "" {
		return 0, fmt.Errorf("observe endpoint required (set audit.endpoint in teploy.yml)")
	}
	container := accessories.ContainerName(app, accessory)
	markerFile := fmt.Sprintf("/deployments/%s/accessories/%s/.audit-shipped", app, accessory)

	type cursor struct {
		Lines  int    `json:"lines"`
		Prefix string `json:"prefix"`
	}
	var previous cursor
	marker, err := c.exec.Run(ctx, "if test -f "+markerFile+"; then cat "+markerFile+"; elif test -e "+markerFile+"; then exit 1; else printf '{}'; fi")
	if err != nil {
		return 0, fmt.Errorf("reading audit cursor: %w", err)
	}
	if err := json.Unmarshal([]byte(marker), &previous); err != nil {
		if _, legacyErr := strconv.Atoi(strings.TrimSpace(marker)); legacyErr != nil {
			return 0, fmt.Errorf("invalid audit cursor: %w", err)
		}
		// A legacy line count cannot establish log identity; replay once rather than lose rotated events.
		fmt.Fprintln(c.out, "Migrating legacy audit cursor: existing events may be replayed with stable source_event_id")
	}
	prev := previous.Lines

	// Read the whole audit log (JSON-per-line) from the container.
	out, err := c.docker.Exec(ctx, container, "cat /openbao/data/audit.log")
	if err != nil {
		return 0, fmt.Errorf("reading audit log: %w", err)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	total := len(lines)
	prefix := func(n int) string {
		sum := sha256.Sum256([]byte(strings.Join(lines[:n], "\n")))
		return hex.EncodeToString(sum[:])
	}
	if prev < 0 || prev > total || (prev > 0 && previous.Prefix != prefix(prev)) {
		prev = 0
	}
	checkpoint := func(n int) error {
		data, _ := json.Marshal(cursor{Lines: n, Prefix: prefix(n)})
		if err := lock.Check(ctx, c.exec); err != nil {
			return err
		}
		return c.exec.Upload(ctx, strings.NewReader(string(data)+"\n"), markerFile, "0600")
	}
	if total == prev {
		return 0, nil
	}

	for offset, line := range lines[prev:] {
		line = strings.TrimSpace(line)
		if line == "" || !strings.HasPrefix(line, "{") {
			continue
		}
		var entry OpenBaoAuditEntry
		if json.Unmarshal([]byte(line), &entry) != nil {
			continue
		}
		ev, ok := ToObserveEvent(app, entry)
		if !ok {
			continue
		}
		sum := sha256.Sum256([]byte(app + "\x00" + accessory + "\x00" + line))
		ev.Metadata["source_event_id"] = hex.EncodeToString(sum[:])
		if err := teplaudit.Emit(ctx, observeEndpoint, observeToken, observeSite, ev); err != nil {
			// Persist progress up to the last successful ship so we don't
			// re-send, then surface the error.
			return shipped, errors.Join(fmt.Errorf("emitting audit event: %w", err), checkpoint(prev+offset))
		}
		shipped++
	}

	if err := checkpoint(total); err != nil {
		return shipped, fmt.Errorf("persisting audit cursor: %w", err)
	}
	return shipped, nil
}
