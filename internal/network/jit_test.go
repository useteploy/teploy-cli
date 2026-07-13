package network

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestTailscaleCreateGrant(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("missing bearer token")
		}
		json.NewDecoder(r.Body).Decode(&gotBody)
		json.NewEncoder(w).Encode(map[string]any{
			"id": "k123", "key": "tskey-auth-xyz", "expires": "2026-07-13T12:00:00Z",
		})
	}))
	defer srv.Close()

	t.Setenv("TAILSCALE_API_KEY", "tok")
	t.Setenv("TAILSCALE_TAILNET", "example.com")
	c, err := NewGrantClient("tailscale", srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	g, err := c.CreateGrant(context.Background(), 2*time.Hour, []string{"tag:contractor"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/api/v2/tailnet/example.com/keys" {
		t.Errorf("path = %s", gotPath)
	}
	if gotBody["expirySeconds"].(float64) != 7200 {
		t.Errorf("expirySeconds = %v", gotBody["expirySeconds"])
	}
	create := gotBody["capabilities"].(map[string]any)["devices"].(map[string]any)["create"].(map[string]any)
	if create["ephemeral"] != true || create["preauthorized"] != true {
		t.Errorf("nodes must be ephemeral + preauthorized: %v", create)
	}
	if g.Key != "tskey-auth-xyz" || g.ID != "k123" {
		t.Errorf("grant = %+v", g)
	}
}

func TestHeadscaleCreateAndRevoke(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		if r.URL.Path == "/api/v1/preauthkey" && r.Method == "POST" {
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			if body["user"] != "ops" || body["ephemeral"] != true {
				t.Errorf("create body = %v", body)
			}
			json.NewEncoder(w).Encode(map[string]any{
				"preAuthKey": map[string]any{"id": "7", "key": "hskey-abc", "expiration": "2026-07-13T12:00:00Z"},
			})
			return
		}
		w.Write([]byte("{}"))
	}))
	defer srv.Close()

	t.Setenv("HEADSCALE_API_KEY", "tok")
	t.Setenv("HEADSCALE_USER", "ops")
	c, err := NewGrantClient("headscale", srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	g, err := c.CreateGrant(context.Background(), time.Hour, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if g.Key != "hskey-abc" {
		t.Errorf("key = %s", g.Key)
	}
	if err := c.RevokeGrant(context.Background(), "hskey-abc"); err != nil {
		t.Fatal(err)
	}
	if paths[1] != "POST /api/v1/preauthkey/expire" {
		t.Errorf("revoke must expire the key: %v", paths)
	}
}

func TestGrantClientCredentialErrors(t *testing.T) {
	t.Setenv("TAILSCALE_API_KEY", "")
	t.Setenv("TS_API_KEY", "")
	if _, err := NewGrantClient("tailscale", ""); err == nil {
		t.Error("missing tailscale token must error with guidance")
	}
	t.Setenv("HEADSCALE_URL", "")
	if _, err := NewGrantClient("headscale", ""); err == nil {
		t.Error("missing headscale URL must error with guidance")
	}
	if _, err := NewGrantClient("netbird", ""); err == nil {
		t.Error("netbird must report not-implemented")
	}
}
