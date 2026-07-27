package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Payload is the JSON body sent to the webhook URL.
type Payload struct {
	App        string `json:"app"`
	Server     string `json:"server"`
	Type       string `json:"type"` // deploy, rollback, restart, backup, health_failure
	Success    bool   `json:"success"`
	Hash       string `json:"hash,omitempty"`
	Message    string `json:"message,omitempty"`
	DurationMs int64  `json:"duration_ms"`
	Timestamp  string `json:"timestamp"`
}

// Notifier sends webhook notifications. Fire-and-forget — errors are returned
// but should be logged as warnings, never blocking the operation.
type Notifier struct {
	webhookURL string
	secret     string
	client     *http.Client
}

// NewNotifier creates a notifier for the given webhook URL, signing deliveries
// with secret (see sign.go). An empty secret sends unsigned, which is what an
// install that has not configured one has always done.
// Returns nil if the URL is empty (no notifications configured).
//
// Command code should use cli.buildNotifier instead, which honours BOTH
// `notifications.webhook` and `notifications.channels`. Reaching for this
// constructor from a command is how rollback and stop/start/restart ended up
// silently ignoring every channels-configured receiver: only the legacy key was
// read, so those installs got deploy events and nothing else.
func NewNotifier(webhookURL, secret string) *Notifier {
	if webhookURL == "" {
		return nil
	}
	return &Notifier{
		webhookURL: webhookURL,
		secret:     secret,
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// Send fires a webhook notification with the given payload.
func (n *Notifier) Send(ctx context.Context, p Payload) error {
	if p.Timestamp == "" {
		p.Timestamp = time.Now().UTC().Format(time.RFC3339)
	}

	body, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("marshaling notification: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.webhookURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "teploy/1.0")
	signRequest(req, n.secret, body)

	resp, err := n.client.Do(req)
	if err != nil {
		return fmt.Errorf("sending notification: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("webhook returned status %d", resp.StatusCode)
	}
	return nil
}
