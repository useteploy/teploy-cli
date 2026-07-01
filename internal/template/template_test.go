package template

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGenerateSecrets(t *testing.T) {
	input := `app: plausible
env:
  SECRET_KEY_BASE: generate
  NORMAL_VAR: hello
  ANOTHER_SECRET: "generate"`

	result, generated := GenerateSecrets(input)

	// SECRET_KEY_BASE should be replaced.
	if strings.Contains(result, ": generate") {
		t.Errorf("still contains 'generate' after replacement:\n%s", result)
	}
	// NORMAL_VAR should be unchanged.
	if !strings.Contains(result, "NORMAL_VAR: hello") {
		t.Errorf("NORMAL_VAR was modified:\n%s", result)
	}
	// Generated values should be 64 chars hex.
	for _, line := range strings.Split(result, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "SECRET_KEY_BASE:") {
			val := strings.TrimSpace(strings.TrimPrefix(trimmed, "SECRET_KEY_BASE:"))
			if len(val) != 64 {
				t.Errorf("expected 64-char hex, got %d chars: %s", len(val), val)
			}
		}
	}

	// The returned map must report both generated keys with the exact
	// values written into the content — this is what runTemplateInstall
	// relies on to show the operator their credentials, since a template
	// deployed via `install` never otherwise writes the rendered content
	// (with real secret values) anywhere retrievable.
	if len(generated) != 2 {
		t.Fatalf("expected 2 generated secrets, got %d: %v", len(generated), generated)
	}
	for _, key := range []string{"SECRET_KEY_BASE", "ANOTHER_SECRET"} {
		val, ok := generated[key]
		if !ok {
			t.Errorf("expected %s in the generated map", key)
			continue
		}
		if len(val) != 64 {
			t.Errorf("generated[%s] = %q, expected 64-char hex", key, val)
		}
		if !strings.Contains(result, key+": "+val) {
			t.Errorf("generated map value for %s doesn't match what's actually in the rendered content", key)
		}
	}
}

func TestVariableSubstitution(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/teploy.yml") {
			w.Write([]byte(`app: plausible
domain: "{{domain}}"
env:
  BASE_URL: "https://{{domain}}"`))
			return
		}
		w.WriteHeader(404)
	}))
	defer srv.Close()

	reg := NewRegistry()
	reg.SetBaseURL(srv.URL)

	content, _, err := reg.Fetch(context.Background(), "plausible", map[string]string{
		"domain": "analytics.mysite.com",
	})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	if !strings.Contains(content, "domain: \"analytics.mysite.com\"") {
		t.Errorf("domain not substituted:\n%s", content)
	}
	if !strings.Contains(content, "https://analytics.mysite.com") {
		t.Errorf("BASE_URL not substituted:\n%s", content)
	}
}

func TestRegistryList(t *testing.T) {
	index := []Info{
		{Name: "plausible", Description: "Web analytics", Accessories: []string{"postgres", "clickhouse"}},
		{Name: "ghost", Description: "Blog platform", Accessories: []string{"mysql"}},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/index.json") {
			json.NewEncoder(w).Encode(index)
			return
		}
		w.WriteHeader(404)
	}))
	defer srv.Close()

	reg := NewRegistry()
	reg.SetBaseURL(srv.URL)

	templates, err := reg.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	if len(templates) != 2 {
		t.Fatalf("expected 2 templates, got %d", len(templates))
	}
	if templates[0].Name != "plausible" {
		t.Errorf("expected 'plausible', got %q", templates[0].Name)
	}
}

func TestRegistryFetch_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
	}))
	defer srv.Close()

	reg := NewRegistry()
	reg.SetBaseURL(srv.URL)

	_, _, err := reg.Fetch(context.Background(), "nonexistent", nil)
	if err == nil {
		t.Fatal("expected error for nonexistent template")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected 'not found' error, got: %v", err)
	}
}
