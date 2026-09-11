package template

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
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

// teploy-cli-02: two accessories generating the same env key must both be
// retrievable from the generated map with distinct identities; unique keys
// keep their historical bare form.
func TestGenerateSecrets_DuplicateKeysArePathQualified(t *testing.T) {
	input := `app: dual
accessories:
  db1:
    image: postgres:16
    env:
      PASSWORD: generate
  db2:
    image: mysql:8
    env:
      PASSWORD: generate
  cache:
    image: redis:7
    env:
      REDIS_PASS: generate
`

	result, generated := GenerateSecrets(input)

	if len(generated) != 3 {
		t.Fatalf("expected 3 generated secrets, got %d: %v", len(generated), generated)
	}
	p1, ok1 := generated["accessories.db1.env.PASSWORD"]
	p2, ok2 := generated["accessories.db2.env.PASSWORD"]
	if !ok1 || !ok2 {
		t.Fatalf("expected path-qualified keys for the colliding PASSWORD entries, got: %v", generated)
	}
	if p1 == p2 {
		t.Error("the two generated PASSWORDs must be distinct values")
	}
	if !strings.Contains(result, "PASSWORD: "+p1) || !strings.Contains(result, "PASSWORD: "+p2) {
		t.Error("both generated values must appear in the rendered content")
	}
	// Unique keys stay bare.
	if _, ok := generated["REDIS_PASS"]; !ok {
		t.Errorf("unique keys must keep their bare form, got: %v", generated)
	}
}

// teploy-cli-01: substituted values must survive as exact strings when the
// rendered template is parsed — quotes, colon-space, hash, newlines,
// true/number lookalikes, leading zeros, and placeholders embedded in
// larger plain and quoted scalars.
func TestFetch_VariableValuesRoundTripThroughYAML(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/teploy.yml") {
			w.Write([]byte(`app: plausible
domain: {{domain}}
env:
  SECRET: {{db_password}}
  EMBEDDED: pre-{{db_password}}-post
  QUOTED: "{{db_password}}"
  SINGLE: '{{db_password}}'
  URL: postgres://user:{{db_password}}@db:5432/app
  PLAIN_SAFE: {{plain}}
  BOOLEANISH: {{db_password}}
accessories:
  db:
    image: postgres:16
    env:
      POSTGRES_PASSWORD: {{db_password}} # trailing comment
`))
			return
		}
		w.WriteHeader(404)
	}))
	defer srv.Close()

	password := "p\"ss: #x\ntrue\t007\\end"
	reg := NewRegistry()
	reg.SetBaseURL(srv.URL)

	content, _, err := reg.Fetch(context.Background(), "plausible", map[string]string{
		"domain":      "analytics.mysite.com",
		"db_password": password,
		"plain":       "hello.example.com",
	})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	var doc map[string]interface{}
	if err := yaml.Unmarshal([]byte(content), &doc); err != nil {
		t.Fatalf("rendered template must be valid YAML: %v\n%s", err, content)
	}

	env := doc["env"].(map[string]interface{})
	for _, key := range []string{"SECRET", "EMBEDDED", "QUOTED", "SINGLE", "URL", "BOOLEANISH"} {
		got, ok := env[key].(string)
		if !ok {
			t.Errorf("%s: not a string: %T", key, env[key])
			continue
		}
		switch key {
		case "EMBEDDED":
			if want := "pre-" + password + "-post"; got != want {
				t.Errorf("EMBEDDED = %q, want %q", got, want)
			}
		case "URL":
			if want := "postgres://user:" + password + "@db:5432/app"; got != want {
				t.Errorf("URL = %q, want %q", got, want)
			}
		case "PLAIN_SAFE":
			// covered below
		default:
			if got != password {
				t.Errorf("%s = %q, want %q", key, got, password)
			}
		}
	}
	if got := env["PLAIN_SAFE"]; got != "hello.example.com" {
		t.Errorf("PLAIN_SAFE = %v, want hello.example.com (safe values stay unquoted)", got)
	}
	if _, isStr := env["BOOLEANISH"].(string); !isStr || env["BOOLEANISH"].(string) != password {
		t.Errorf("BOOLEANISH must round-trip as the exact string, got %#v", env["BOOLEANISH"])
	}

	acc := doc["accessories"].(map[string]interface{})["db"].(map[string]interface{})
	accEnv := acc["env"].(map[string]interface{})
	if got := accEnv["POSTGRES_PASSWORD"]; got != password {
		t.Errorf("accessory POSTGRES_PASSWORD (with trailing comment) = %#v, want exact value", got)
	}
	if !strings.Contains(content, "# trailing comment") {
		t.Errorf("trailing comment must survive substitution:\n%s", content)
	}
}

// A value that YAML would resolve to a bool or number must stay a string.
func TestFetch_LookalikeValuesStayStrings(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("app: x\nenv:\n  A: {{v1}}\n  B: {{v2}}\n  C: {{v3}}\n"))
	}))
	defer srv.Close()

	reg := NewRegistry()
	reg.SetBaseURL(srv.URL)
	content, _, err := reg.Fetch(context.Background(), "x", map[string]string{
		"v1": "true", "v2": "007", "v3": "1.5",
	})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	var doc struct {
		Env map[string]string `yaml:"env"`
	}
	if err := yaml.Unmarshal([]byte(content), &doc); err != nil {
		t.Fatalf("parse: %v\n%s", err, content)
	}
	for key, want := range map[string]string{"A": "true", "B": "007", "C": "1.5"} {
		if doc.Env[key] != want {
			t.Errorf("env[%s] = %q, want exact string %q", key, doc.Env[key], want)
		}
	}
}

// teploy-cli-03: an unreplaced placeholder is a missing variable and must
// fail before anything is deployed.
func TestFetch_MissingVariablesFail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("app: x\nenv:\n  PASSWORD: {{db_password}}\n  TOKEN: {{api_token}}\n"))
	}))
	defer srv.Close()

	reg := NewRegistry()
	reg.SetBaseURL(srv.URL)

	_, _, err := reg.Fetch(context.Background(), "x", map[string]string{"domain": "x.com"})
	if err == nil {
		t.Fatal("expected missing-variable error")
	}
	if !strings.Contains(err.Error(), "db_password") || !strings.Contains(err.Error(), "api_token") {
		t.Errorf("error must name the missing variables, got: %v", err)
	}

	// nil vars (the `template info` preview path) skips the check — the
	// raw template with placeholders is the thing being shown.
	if _, _, err := reg.Fetch(context.Background(), "x", nil); err != nil {
		t.Errorf("preview with nil vars must not fail on placeholders: %v", err)
	}
}

// teploy-cli-03: malformed template YAML fails at fetch, before deploy.
func TestFetch_MalformedTemplateFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("app: x\nenv: [unclosed\n"))
	}))
	defer srv.Close()

	reg := NewRegistry()
	reg.SetBaseURL(srv.URL)

	_, _, err := reg.Fetch(context.Background(), "x", map[string]string{"domain": "x.com"})
	if err == nil || !strings.Contains(err.Error(), "not valid YAML") {
		t.Errorf("expected invalid-YAML error, got: %v", err)
	}
}

// teploy-cli-03: template names are one path component — no traversal,
// no slashes, no query fragments.
func TestFetch_InvalidTemplateName(t *testing.T) {
	reg := NewRegistry()
	for _, name := range []string{"../evil", "a/b", "x?y", "x#y", "", ".hidden"} {
		if _, _, err := reg.Fetch(context.Background(), name, nil); err == nil {
			t.Errorf("expected rejection for template name %q", name)
		}
	}
}

// teploy-cli-03: duplicate catalog entries are rejected.
func TestRegistryList_DuplicateEntries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]Info{
			{Name: "plausible", Description: "a"},
			{Name: "plausible", Description: "b"},
		})
	}))
	defer srv.Close()

	reg := NewRegistry()
	reg.SetBaseURL(srv.URL)

	if _, err := reg.List(context.Background()); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("expected duplicate-entry error, got: %v", err)
	}
}

// teploy-cli-03: oversized responses fail instead of buffering whole.
func TestRegistry_SizeLimits(t *testing.T) {
	big := strings.Repeat("x", 1<<20+10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/index.json") {
			w.Write([]byte(big))
			return
		}
		w.Write([]byte(big))
	}))
	defer srv.Close()

	reg := NewRegistry()
	reg.SetBaseURL(srv.URL)

	if _, err := reg.List(context.Background()); err == nil || !strings.Contains(err.Error(), "size limit") {
		t.Errorf("expected index size-limit error, got: %v", err)
	}
	if _, _, err := reg.Fetch(context.Background(), "x", nil); err == nil || !strings.Contains(err.Error(), "size limit") {
		t.Errorf("expected template size-limit error, got: %v", err)
	}
}
