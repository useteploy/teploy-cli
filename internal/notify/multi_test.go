package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestMultiNotifier_FiltersByEvent(t *testing.T) {
	var deployHits, healthHits int32

	deploySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&deployHits, 1)
		w.WriteHeader(200)
	}))
	defer deploySrv.Close()

	healthSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&healthHits, 1)
		w.WriteHeader(200)
	}))
	defer healthSrv.Close()

	n := NewMultiNotifier([]Channel{
		{Type: "webhook", URL: deploySrv.URL, Events: []string{"deploy", "rollback"}},
		{Type: "webhook", URL: healthSrv.URL, Events: []string{"health_failure"}},
	})

	// Send a deploy event — should only hit deploy webhook.
	n.Send(context.Background(), Payload{Type: "deploy", App: "myapp"})
	if atomic.LoadInt32(&deployHits) != 1 {
		t.Errorf("deploy webhook: expected 1 hit, got %d", deployHits)
	}
	if atomic.LoadInt32(&healthHits) != 0 {
		t.Errorf("health webhook: expected 0 hits, got %d", healthHits)
	}

	// Send a health_failure event — should only hit health webhook.
	n.Send(context.Background(), Payload{Type: "health_failure", App: "myapp"})
	if atomic.LoadInt32(&deployHits) != 1 {
		t.Errorf("deploy webhook: expected 1 hit, got %d", deployHits)
	}
	if atomic.LoadInt32(&healthHits) != 1 {
		t.Errorf("health webhook: expected 1 hit, got %d", healthHits)
	}
}

func TestMultiNotifier_AllEventsWhenEmpty(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	n := NewMultiNotifier([]Channel{
		{Type: "webhook", URL: srv.URL}, // no events filter = all
	})

	n.Send(context.Background(), Payload{Type: "deploy", App: "myapp"})
	n.Send(context.Background(), Payload{Type: "rollback", App: "myapp"})
	n.Send(context.Background(), Payload{Type: "health_failure", App: "myapp"})

	if atomic.LoadInt32(&hits) != 3 {
		t.Errorf("expected 3 hits, got %d", hits)
	}
}

func TestMultiNotifier_NilWhenEmpty(t *testing.T) {
	n := NewMultiNotifier(nil)
	if n != nil {
		t.Error("expected nil notifier for empty channels")
	}
}

func TestMultiNotifier_SlackSendsTextPayload(t *testing.T) {
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	n := NewMultiNotifier([]Channel{
		{Type: "slack", URL: srv.URL},
	})
	n.Send(context.Background(), Payload{App: "myapp", Type: "deploy", Server: "web1", Success: true})

	var got slackMessage
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("slack payload wasn't valid JSON with a text field: %v (body: %s)", err, body)
	}
	if !strings.Contains(got.Text, "myapp") || !strings.Contains(got.Text, "deploy") {
		t.Errorf("slack text = %q, want it to mention app and event type", got.Text)
	}
}

func TestMultiNotifier_UnknownTypeIsNoOp(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	// discord isn't a recognized channel type — config.validate() rejects it
	// before it ever reaches here, but MultiNotifier itself should still not
	// panic or send anything for an unrecognized type slipping through.
	n := NewMultiNotifier([]Channel{
		{Type: "discord", URL: srv.URL},
	})
	n.Send(context.Background(), Payload{Type: "deploy", App: "myapp"})

	if atomic.LoadInt32(&hits) != 0 {
		t.Errorf("expected 0 hits for unrecognized channel type, got %d", hits)
	}
}

func TestMatchesEvent(t *testing.T) {
	tests := []struct {
		filter []string
		event  string
		want   bool
	}{
		{nil, "deploy", true},
		{[]string{}, "deploy", true},
		{[]string{"deploy"}, "deploy", true},
		{[]string{"deploy"}, "rollback", false},
		{[]string{"deploy", "rollback"}, "rollback", true},
	}

	for _, tt := range tests {
		got := matchesEvent(tt.filter, tt.event)
		if got != tt.want {
			t.Errorf("matchesEvent(%v, %q) = %v, want %v", tt.filter, tt.event, got, tt.want)
		}
	}
}
