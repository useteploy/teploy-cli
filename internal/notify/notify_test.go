package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewNotifier_EmptyURL(t *testing.T) {
	n := NewNotifier("", "")
	if n != nil {
		t.Error("expected nil notifier for empty URL")
	}
}

func TestSend_Success(t *testing.T) {
	var received Payload

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("expected json content type, got %s", r.Header.Get("Content-Type"))
		}
		if r.Header.Get("User-Agent") != "teploy/1.0" {
			t.Errorf("expected teploy user agent, got %s", r.Header.Get("User-Agent"))
		}

		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &received)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	n := NewNotifier(server.URL, "")
	err := n.Send(context.Background(), Payload{
		App:        "myapp",
		Server:     "1.2.3.4",
		Type:       "deploy",
		Success:    true,
		Hash:       "abc1234",
		DurationMs: 5000,
		Timestamp:  "2026-03-11T00:00:00Z",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if received.App != "myapp" {
		t.Errorf("expected app myapp, got %s", received.App)
	}
	if received.Type != "deploy" {
		t.Errorf("expected type deploy, got %s", received.Type)
	}
	if !received.Success {
		t.Error("expected success true")
	}
	if received.Hash != "abc1234" {
		t.Errorf("expected hash abc1234, got %s", received.Hash)
	}
	if received.Timestamp != "2026-03-11T00:00:00Z" {
		t.Errorf("expected timestamp preserved, got %s", received.Timestamp)
	}
}

func TestSend_DefaultTimestamp(t *testing.T) {
	var received Payload

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &received)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	n := NewNotifier(server.URL, "")
	n.Send(context.Background(), Payload{App: "myapp", Type: "deploy"})

	if received.Timestamp == "" {
		t.Error("expected auto-generated timestamp")
	}
}

func TestSend_ServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	n := NewNotifier(server.URL, "")
	err := n.Send(context.Background(), Payload{App: "myapp", Type: "deploy"})
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
}

func TestSend_UnreachableServer(t *testing.T) {
	n := NewNotifier("http://127.0.0.1:1", "") // port 1 is never listening
	err := n.Send(context.Background(), Payload{App: "myapp", Type: "deploy"})
	if err == nil {
		t.Fatal("expected error for unreachable server")
	}
}

func TestSend_FailurePayload(t *testing.T) {
	var received Payload

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &received)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	n := NewNotifier(server.URL, "")
	n.Send(context.Background(), Payload{
		App:     "myapp",
		Type:    "health_failure",
		Success: false,
		Message: "health check timed out",
	})

	if received.Success {
		t.Error("expected success false")
	}
	if received.Message != "health check timed out" {
		t.Errorf("expected failure message, got %s", received.Message)
	}
}
