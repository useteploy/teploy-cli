package cloud

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testKey = "tp_test_0123456789abcdefSECRET"

func newTestClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := New(srv.URL, testKey, "org_1")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestClientSendsAuthHeaders(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+testKey {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("X-Organization-Id"); got != "org_1" {
			t.Errorf("X-Organization-Id = %q", got)
		}
		w.Write([]byte(`{"success":true,"data":[]}`))
	})
	if _, err := c.ListServers(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestClientAcceptsBothListShapes(t *testing.T) {
	for name, body := range map[string]string{
		"array": `{"success":true,"data":[{"id":"s1","name":"web","ipAddress":"203.0.113.5","status":"active","priceMonthlyCents":600}]}`,
		"items": `{"success":true,"data":{"items":[{"id":"s1","name":"web","ipAddress":"203.0.113.5","status":"active","priceMonthlyCents":600}],"total":1}}`,
	} {
		t.Run(name, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) })
			got, err := c.ListServers(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 || got[0].ID != "s1" || got[0].IPAddress != "203.0.113.5" || got[0].PriceMonthlyCents != 600 {
				t.Fatalf("got %+v", got)
			}
		})
	}
}

func TestClientErrorEnvelope(t *testing.T) {
	cases := []struct {
		status        int
		body          string
		want, wantNot string
		wantCode      string
	}{
		{401, `{"success":false,"error":{"code":"UNAUTHORIZED","message":"Invalid API key"}}`, "teploy cloud login", "", "UNAUTHORIZED"},
		{402, `{"success":false,"error":{"code":"PAYMENT_REQUIRED","message":"No card"}}`, "Add a card at https://cloud.teploy.com", "", "PAYMENT_REQUIRED"},
		{400, `{"success":false,"error":{"code":"BAD_REQUEST","message":"Add a payment method first"}}`, "Add a card", "", "BAD_REQUEST"},
		{403, `{"success":false,"error":{"code":"trust_limit","message":"New accounts may run 2 servers."}}`, "New accounts may run 2 servers.", "code", "trust_limit"},
		{503, `{"success":false,"error":{"code":"platform_capacity","message":"No capacity right now."}}`, "No capacity right now.", "HTTP", "platform_capacity"},
		{502, `<html>bad gateway</html>`, "HTTP 502", "", "unknown"},
	}
	for _, tc := range cases {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			w.Write([]byte(tc.body))
		})
		_, err := c.ListServers(context.Background())
		var ae *APIError
		if !errors.As(err, &ae) {
			t.Fatalf("%d: want APIError, got %v", tc.status, err)
		}
		if ae.Code != tc.wantCode || ae.Status != tc.status {
			t.Errorf("%d: code %q status %d", tc.status, ae.Code, ae.Status)
		}
		msg := err.Error()
		if !strings.Contains(msg, tc.want) {
			t.Errorf("%d: %q does not contain %q", tc.status, msg, tc.want)
		}
		if tc.wantNot != "" && strings.Contains(msg, tc.wantNot) {
			t.Errorf("%d: %q should be the message as is", tc.status, msg)
		}
		if strings.Contains(msg, testKey) {
			t.Errorf("error leaks the key: %q", msg)
		}
	}
}

func TestClientSuccessFalseIsError(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"success":false,"error":{"code":"X","message":"nope"}}`))
	})
	if _, err := c.ListServers(context.Background()); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("got %v", err)
	}
}

func TestValidateBaseURL(t *testing.T) {
	ok := []string{"https://cloud.teploy.com", "https://cloud.teploy.com/", "http://localhost:8080", "http://127.0.0.1:3000"}
	for _, u := range ok {
		if _, err := ValidateBaseURL(u); err != nil {
			t.Errorf("%s: %v", u, err)
		}
	}
	bad := []string{"http://cloud.teploy.com", "http://10.0.0.1", "ftp://x", "cloud.teploy.com", "https://u:p@cloud.teploy.com"}
	for _, u := range bad {
		if _, err := ValidateBaseURL(u); err == nil {
			t.Errorf("%s accepted", u)
		}
	}
	if _, err := New("http://example.com", testKey, "org"); err == nil {
		t.Error("New accepted plain http to a remote host")
	}
}

func TestResolveBaseURLEnv(t *testing.T) {
	t.Setenv(BaseURLEnv, "")
	if u, _ := ResolveBaseURL(""); u != DefaultBaseURL {
		t.Errorf("default = %q", u)
	}
	if u, _ := ResolveBaseURL("http://localhost:9000"); u != "http://localhost:9000" {
		t.Errorf("stored = %q", u)
	}
	t.Setenv(BaseURLEnv, "http://evil.example")
	if _, err := ResolveBaseURL(""); err == nil {
		t.Error("env with non-https remote URL accepted")
	}
}

func TestCreateServerSendsIdempotencyKey(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Idempotency-Key") != "k1" {
			t.Errorf("Idempotency-Key = %q", r.Header.Get("Idempotency-Key"))
		}
		w.WriteHeader(201)
		w.Write([]byte(`{"success":true,"data":{"id":"s1","status":"provisioning"}}`))
	})
	s, err := c.CreateServer(context.Background(), CreateServerRequest{Name: "web"}, "k1")
	if err != nil || s.ID != "s1" {
		t.Fatalf("%v %+v", err, s)
	}
}

func TestCredentialsFileMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", FileName)
	if err := SaveCredentials(path, &Credentials{OrgID: "o", APIKey: "k"}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %o", fi.Mode().Perm())
	}
	got, err := LoadCredentials(path)
	if err != nil || got.APIKey != "k" || got.OrgID != "o" {
		t.Fatalf("%v %+v", err, got)
	}
	if removed, err := RemoveCredentials(path); !removed || err != nil {
		t.Fatalf("%v %v", removed, err)
	}
	if _, err := LoadCredentials(path); !errors.Is(err, ErrNotLoggedIn) {
		t.Fatalf("got %v", err)
	}
}
