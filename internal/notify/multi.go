package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Channel represents a single notification destination.
type Channel struct {
	Type   string   // "webhook" or "email"
	URL    string   // webhook URL
	To     string   // email address
	Events []string // empty = all events
}

// MultiNotifier sends notifications to multiple channels filtered by event type.
type MultiNotifier struct {
	channels []Channel
	client   *http.Client
}

// NewMultiNotifier creates a notifier from a list of channels.
// Returns nil if no channels are configured.
func NewMultiNotifier(channels []Channel) *MultiNotifier {
	if len(channels) == 0 {
		return nil
	}
	return &MultiNotifier{
		channels: channels,
		client:   &http.Client{Timeout: 10 * time.Second},
	}
}

// Send fires notifications to all matching channels. Returns errors (non-blocking).
func (n *MultiNotifier) Send(ctx context.Context, p Payload) []error {
	if p.Timestamp == "" {
		p.Timestamp = time.Now().UTC().Format(time.RFC3339)
	}

	var errs []error
	for _, ch := range n.channels {
		if !matchesEvent(ch.Events, p.Type) {
			continue
		}

		switch ch.Type {
		case "webhook", "":
			if err := sendWebhook(ctx, n.client, ch.URL, p); err != nil {
				errs = append(errs, fmt.Errorf("webhook %s: %w", ch.URL, err))
			}
		case "slack":
			if err := sendSlack(ctx, n.client, ch.URL, p); err != nil {
				errs = append(errs, fmt.Errorf("slack %s: %w", ch.URL, err))
			}
		case "email":
			// Email not yet implemented — log warning silently.
			errs = append(errs, fmt.Errorf("email notifications not yet implemented (to: %s)", ch.To))
		}
	}
	return errs
}

// matchesEvent returns true if the event type matches the channel's filter.
// An empty filter matches all events.
func matchesEvent(filter []string, eventType string) bool {
	if len(filter) == 0 {
		return true
	}
	for _, e := range filter {
		if e == eventType {
			return true
		}
	}
	return false
}

// slackMessage is Slack's Incoming Webhook payload shape — a bare "text"
// field. Slack's webhook endpoint rejects the generic Payload JSON (no
// "text"/"blocks"/"attachments" key) with a 400 "invalid_payload", so
// type: slack can't just reuse sendWebhook's body.
type slackMessage struct {
	Text string `json:"text"`
}

func sendSlack(ctx context.Context, client *http.Client, url string, p Payload) error {
	status := "succeeded"
	if !p.Success {
		status = "failed"
	}
	text := fmt.Sprintf("*%s* %s on %s (%s)", p.App, p.Type, p.Server, status)
	if p.Message != "" {
		text += "\n" + p.Message
	}

	body, err := json.Marshal(slackMessage{Text: text})
	if err != nil {
		return fmt.Errorf("marshaling slack message: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "teploy/1.0")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("sending slack message: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("slack webhook returned status %d", resp.StatusCode)
	}
	return nil
}

func sendWebhook(ctx context.Context, client *http.Client, url string, p Payload) error {
	body, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("marshaling notification: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "teploy/1.0")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("sending notification: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("webhook returned status %d", resp.StatusCode)
	}
	return nil
}
