// Package audit emits deployment audit events to a teploy-observe instance so
// the compliance/audit trail spans deploys, rollbacks, and scale — "who shipped
// what version where." It POSTs to observe's /api/v1/audit producer endpoint;
// it's fire-and-forget from the caller's side (a failed emit must never fail a
// deploy). Disabled unless an endpoint is configured in teploy.yml `audit:`.
package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/user"
	"strings"
	"time"
)

// Event is one deployment audit event.
type Event struct {
	Actor    string         // who ran it (defaults to the OS user)
	Action   string         // dotted verb, e.g. deploy.run, deploy.rollback
	Target   string         // what was acted on, e.g. "myapp@v1.2.3"
	Result   string         // success | failure
	Metadata map[string]any // extra context (server, version, ...)
}

// Emit POSTs the event to observe's audit endpoint. A blank endpoint is a no-op
// (auditing not configured). Errors are returned for the caller to log, never
// to abort on.
func Emit(ctx context.Context, endpoint, token, site string, ev Event) error {
	if strings.TrimSpace(endpoint) == "" {
		return nil
	}
	if ev.Actor == "" {
		ev.Actor = currentUser()
	}
	if site == "" {
		site = "default"
	}
	if ev.Metadata == nil {
		ev.Metadata = map[string]any{}
	}

	body, err := json.Marshal(map[string]any{
		"site_id":    site,
		"actor":      ev.Actor,
		"actor_type": "user",
		"action":     ev.Action,
		"target":     ev.Target,
		"result":     ev.Result,
		"metadata":   ev.Metadata,
	})
	if err != nil {
		return err
	}

	url := strings.TrimRight(endpoint, "/") + "/api/v1/audit"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("audit emit: observe returned %d", resp.StatusCode)
	}
	return nil
}

func currentUser() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return "unknown"
}
