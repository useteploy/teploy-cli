package audit

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestEmit_Disabled(t *testing.T) {
	// A blank endpoint is a no-op and never errors.
	if err := Emit(context.Background(), "", "tok", "s", Event{Action: "deploy.run"}); err != nil {
		t.Fatalf("disabled emit should be a no-op: %v", err)
	}
}

func TestEmit_PostsEvent(t *testing.T) {
	var gotPath, gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	err := Emit(context.Background(), srv.URL, "editor-tok", "site1", Event{
		Actor:    "alice",
		Action:   "deploy.run",
		Target:   "myapp@v1.2.3",
		Result:   "success",
		Metadata: map[string]any{"server": "box1"},
	})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}

	if gotPath != "/api/v1/audit" {
		t.Errorf("posted to %q, want /api/v1/audit", gotPath)
	}
	if gotAuth != "Bearer editor-tok" {
		t.Errorf("auth header = %q", gotAuth)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(gotBody), &payload); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	for k, want := range map[string]any{
		"actor": "alice", "action": "deploy.run", "target": "myapp@v1.2.3",
		"result": "success", "actor_type": "user", "site_id": "site1",
	} {
		if payload[k] != want {
			t.Errorf("payload[%q] = %v, want %v", k, payload[k], want)
		}
	}
}

func TestEmit_DefaultsActorAndSite(t *testing.T) {
	var payload map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &payload)
		w.WriteHeader(201)
	}))
	defer srv.Close()

	if err := Emit(context.Background(), srv.URL, "", "", Event{Action: "deploy.run"}); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if payload["site_id"] != "default" {
		t.Errorf("site should default to 'default', got %v", payload["site_id"])
	}
	if payload["actor"] == "" || payload["actor"] == nil {
		t.Error("actor should default to the OS user, got empty")
	}
}

func TestEmit_ErrorsOnBadStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	if err := Emit(context.Background(), srv.URL, "bad", "s", Event{Action: "deploy.run"}); err == nil {
		t.Fatal("expected error on 403")
	}
}
