package openbao

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/useteploy/teploy/internal/accessories"
	teplaudit "github.com/useteploy/teploy/internal/audit"
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
func (c *Client) ShipAudit(ctx context.Context, app, accessory, observeEndpoint, observeToken, observeSite string) (int, error) {
	if accessory == "" {
		accessory = defaultAccessory
	}
	if observeEndpoint == "" {
		return 0, fmt.Errorf("observe endpoint required (set audit.endpoint in teploy.yml)")
	}
	container := accessories.ContainerName(app, accessory)
	markerFile := fmt.Sprintf("/deployments/%s/accessories/%s/.audit-shipped", app, accessory)

	// How many lines shipped previously (0 if none).
	prev := 0
	if out, err := c.exec.Run(ctx, "cat "+markerFile+" 2>/dev/null || echo 0"); err == nil {
		if n, e := strconv.Atoi(strings.TrimSpace(out)); e == nil {
			prev = n
		}
	}

	// Read the whole audit log (JSON-per-line) from the container.
	out, err := c.docker.Exec(ctx, container, "cat /openbao/data/audit.log 2>/dev/null || true")
	if err != nil {
		return 0, fmt.Errorf("reading audit log: %w", err)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	total := len(lines)
	if total <= prev {
		return 0, nil // nothing new
	}

	shipped := 0
	for _, line := range lines[prev:] {
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
		if err := teplaudit.Emit(ctx, observeEndpoint, observeToken, observeSite, ev); err != nil {
			// Persist progress up to the last successful ship so we don't
			// re-send, then surface the error.
			c.persistShipMarker(ctx, markerFile, prev+shipped)
			return shipped, fmt.Errorf("emitting audit event: %w", err)
		}
		shipped++
	}

	c.persistShipMarker(ctx, markerFile, total)
	return shipped, nil
}

func (c *Client) persistShipMarker(ctx context.Context, markerFile string, count int) {
	_ = c.exec.Upload(ctx, strings.NewReader(strconv.Itoa(count)+"\n"), markerFile, "0600")
}
