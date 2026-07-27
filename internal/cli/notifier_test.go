package cli

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/notify"
)

// recorder collects what a webhook receiver actually saw.
type recorder struct {
	mu   sync.Mutex
	hits []recordedHit
}

type recordedHit struct {
	body []byte
	ts   string
	sig  string
}

func (r *recorder) server(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.hits = append(r.hits, recordedHit{
			body: body,
			ts:   req.Header.Get("X-Teploy-Timestamp"),
			sig:  req.Header.Get("X-Teploy-Signature"),
		})
		r.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.hits)
}

func (r *recorder) first() recordedHit {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.hits[0]
}

func verifyHit(secret string, h recordedHit) bool {
	sig := strings.TrimPrefix(h.sig, "sha256=")
	got, err := hex.DecodeString(sig)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(h.ts))
	mac.Write([]byte("."))
	mac.Write(h.body)
	return hmac.Equal(got, mac.Sum(nil))
}

func TestBuildNotifier_NilWhenNothingConfigured(t *testing.T) {
	if n := buildNotifier(&config.AppConfig{App: "app"}); n != nil {
		t.Error("expected nil notifier when no webhook and no channels are configured")
	}
}

// The regression this guards: rollback and stop/start/restart used to read only
// `notifications.webhook`, so an install configured entirely with `channels`
// received deploy events and silently nothing else. buildNotifier must deliver
// a rollback to a channels-only config.
func TestBuildNotifier_ChannelsOnlyReceivesRollback(t *testing.T) {
	var rec recorder
	srv := rec.server(t)
	defer srv.Close()

	cfg := &config.AppConfig{
		App: "akiroo-lite",
		Notifications: config.NotificationsConfig{
			Channels: []config.NotificationChannelConfig{
				{Type: "webhook", URL: srv.URL},
			},
		},
	}

	n := buildNotifier(cfg)
	if n == nil {
		t.Fatal("buildNotifier returned nil for a channels-only config")
	}
	if errs := n.Send(context.Background(), notify.Payload{
		App: "akiroo-lite", Type: "rollback", Success: true,
	}); len(errs) > 0 {
		t.Fatalf("send: %v", errs)
	}
	if rec.count() != 1 {
		t.Fatalf("receiver saw %d deliveries, want 1", rec.count())
	}
	if !strings.Contains(string(rec.first().body), `"type":"rollback"`) {
		t.Errorf("delivered body was not a rollback: %s", rec.first().body)
	}
}

func TestBuildNotifier_LegacyWebhookStillWorks(t *testing.T) {
	var rec recorder
	srv := rec.server(t)
	defer srv.Close()

	cfg := &config.AppConfig{
		App:           "app",
		Notifications: config.NotificationsConfig{Webhook: srv.URL},
	}
	if errs := buildNotifier(cfg).Send(context.Background(), notify.Payload{
		App: "app", Type: "restart", Success: true,
	}); len(errs) > 0 {
		t.Fatalf("send: %v", errs)
	}
	if rec.count() != 1 {
		t.Errorf("legacy webhook saw %d deliveries, want 1", rec.count())
	}
}

// Both forms configured means both receivers hear about it — the legacy key is
// not a fallback that channels suppress.
func TestBuildNotifier_BothFormsDeliverToBoth(t *testing.T) {
	var legacy, channel recorder
	legacySrv := legacy.server(t)
	defer legacySrv.Close()
	channelSrv := channel.server(t)
	defer channelSrv.Close()

	cfg := &config.AppConfig{
		App: "app",
		Notifications: config.NotificationsConfig{
			Webhook: legacySrv.URL,
			Channels: []config.NotificationChannelConfig{
				{Type: "webhook", URL: channelSrv.URL},
			},
		},
	}
	if errs := buildNotifier(cfg).Send(context.Background(), notify.Payload{
		App: "app", Type: "deploy", Success: true,
	}); len(errs) > 0 {
		t.Fatalf("send: %v", errs)
	}
	if legacy.count() != 1 || channel.count() != 1 {
		t.Errorf("legacy=%d channel=%d, want 1 each", legacy.count(), channel.count())
	}
}

func TestBuildNotifier_SignsWithResolvedSecret(t *testing.T) {
	var rec recorder
	srv := rec.server(t)
	defer srv.Close()

	const secret = "cfg-secret"
	cfg := &config.AppConfig{
		App: "app",
		Notifications: config.NotificationsConfig{
			Webhook: srv.URL,
			Secret:  secret,
		},
	}
	if errs := buildNotifier(cfg).Send(context.Background(), notify.Payload{
		App: "app", Type: "deploy", Success: true,
	}); len(errs) > 0 {
		t.Fatalf("send: %v", errs)
	}
	if !verifyHit(secret, rec.first()) {
		t.Error("delivery was not signed with the configured secret")
	}
}

func TestBuildNotifier_SignsFromEnvWhenConfigOmitsSecret(t *testing.T) {
	var rec recorder
	srv := rec.server(t)
	defer srv.Close()

	const secret = "env-secret"
	t.Setenv(config.WebhookSecretEnv, secret)

	cfg := &config.AppConfig{
		App:           "app",
		Notifications: config.NotificationsConfig{Webhook: srv.URL},
	}
	if errs := buildNotifier(cfg).Send(context.Background(), notify.Payload{
		App: "app", Type: "deploy", Success: true,
	}); len(errs) > 0 {
		t.Fatalf("send: %v", errs)
	}
	if !verifyHit(secret, rec.first()) {
		t.Error("delivery was not signed with the secret from the environment")
	}
}

// A channel's own secret wins over the top-level one, so two receivers can
// verify with different secrets.
func TestBuildNotifier_PerChannelSecretOverrides(t *testing.T) {
	var rec recorder
	srv := rec.server(t)
	defer srv.Close()

	cfg := &config.AppConfig{
		App: "app",
		Notifications: config.NotificationsConfig{
			Secret: "top-level",
			Channels: []config.NotificationChannelConfig{
				{Type: "webhook", URL: srv.URL, Secret: "channel-own"},
			},
		},
	}
	if errs := buildNotifier(cfg).Send(context.Background(), notify.Payload{
		App: "app", Type: "deploy", Success: true,
	}); len(errs) > 0 {
		t.Fatalf("send: %v", errs)
	}
	if verifyHit("top-level", rec.first()) {
		t.Error("channel was signed with the top-level secret despite its own")
	}
	if !verifyHit("channel-own", rec.first()) {
		t.Error("channel was not signed with its own secret")
	}
}

// Event filters still apply: a channel scoped to deploy must not receive a
// rollback. Without this, unifying rollback onto buildNotifier would have
// widened what existing channels receive.
func TestBuildNotifier_EventFilterStillApplies(t *testing.T) {
	var rec recorder
	srv := rec.server(t)
	defer srv.Close()

	cfg := &config.AppConfig{
		App: "app",
		Notifications: config.NotificationsConfig{
			Channels: []config.NotificationChannelConfig{
				{Type: "webhook", URL: srv.URL, Events: []string{"deploy"}},
			},
		},
	}
	n := buildNotifier(cfg)
	if errs := n.Send(context.Background(), notify.Payload{App: "app", Type: "rollback"}); len(errs) > 0 {
		t.Fatalf("send: %v", errs)
	}
	if rec.count() != 0 {
		t.Errorf("deploy-only channel received a rollback (%d deliveries)", rec.count())
	}
	if errs := n.Send(context.Background(), notify.Payload{App: "app", Type: "deploy"}); len(errs) > 0 {
		t.Fatalf("send: %v", errs)
	}
	if rec.count() != 1 {
		t.Errorf("deploy-only channel did not receive a deploy (%d deliveries)", rec.count())
	}
}
