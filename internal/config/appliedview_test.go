package config

import (
	"strings"
	"testing"
)

// roundTripManifest normalizes a config and parses the result back.
func roundTripManifest(t *testing.T, cfg *AppConfig, appliedImage string) *AppliedManifestView {
	t.Helper()
	data, _, err := NormalizeAndDigest(cfg, appliedImage)
	if err != nil {
		t.Fatalf("NormalizeAndDigest: %v", err)
	}
	view, err := ParseAppliedManifest(data)
	if err != nil {
		t.Fatalf("ParseAppliedManifest: %v", err)
	}
	return view
}

func TestParseAppliedManifest_RoundTripContainer(t *testing.T) {
	cfg := &AppConfig{
		App:       "blog",
		Domain:    "blog.example.com",
		Port:      3000,
		Replicas:  2,
		Memory:    "512m",
		CPU:       "1.5",
		Env:       map[string]string{"TOKEN": "secret-value", "MODE": "prod"},
		EnvFiles:  []string{".env.production"},
		Volumes:   map[string]string{"data": "/var/lib/blog"},
		Publish:   []string{"127.0.0.1:9100:9000"},
		Processes: map[string]string{"web": "", "worker": "rake jobs"},
	}
	view := roundTripManifest(t, cfg, "registry/blog:abc123")
	if view.App != "blog" || view.DeploymentType != TypeContainer || view.IngressMode != IngressCaddy {
		t.Errorf("identity fields: %+v", view)
	}
	if view.Domain != "blog.example.com" {
		t.Errorf("domain = %q", view.Domain)
	}
	c := view.Container
	if c == nil {
		t.Fatal("container section missing")
	}
	if c.Image != "registry/blog:abc123" || c.Port != 3000 || c.Replicas != 2 {
		t.Errorf("container basics: %+v", c)
	}
	if c.Memory != "512m" || c.CPU != "1.5" {
		t.Errorf("resources: %+v", c)
	}
	if strings.Join(c.EnvKeys, ",") != "MODE,TOKEN" {
		t.Errorf("env keys = %v (want sorted, values never recorded)", c.EnvKeys)
	}
	if len(c.EnvFiles) != 1 || c.EnvFiles[0] != ".env.production" {
		t.Errorf("env files = %v", c.EnvFiles)
	}
	if c.Volumes["data"] != "/var/lib/blog" {
		t.Errorf("volumes = %v", c.Volumes)
	}
	if len(c.Publish) != 1 || c.Publish[0] != "127.0.0.1:9100:9000" {
		t.Errorf("publish = %v", c.Publish)
	}
	if strings.Join(c.Processes, ",") != "web,worker" {
		t.Errorf("processes = %v", c.Processes)
	}
	// Secrets never enter the manifest — the round trip must not carry values.
	data, _, _ := NormalizeAndDigest(cfg, "registry/blog:abc123")
	if strings.Contains(string(data), "secret-value") {
		t.Error("env VALUE leaked into the applied manifest")
	}
}

func TestParseAppliedManifest_RoundTripAccessories(t *testing.T) {
	cfg := &AppConfig{
		App:  "blog",
		Port: 80,
		Accessories: map[string]AccessoryConfig{
			"db": {Image: "postgres:16", Port: 5432, Env: map[string]string{"POSTGRES_PASSWORD": "x"}, Volumes: map[string]string{"pgdata": "/var/lib/postgresql/data"}},
		},
	}
	view := roundTripManifest(t, cfg, "")
	db, ok := view.Accessories["db"]
	if !ok {
		t.Fatal("accessory db missing")
	}
	if db.Image != "postgres:16" || db.Port != 5432 {
		t.Errorf("accessory basics: %+v", db)
	}
	if strings.Join(db.EnvKeys, ",") != "POSTGRES_PASSWORD" {
		t.Errorf("accessory env keys = %v", db.EnvKeys)
	}
	if db.Volumes["pgdata"] != "/var/lib/postgresql/data" {
		t.Errorf("accessory volumes = %v", db.Volumes)
	}
}

func TestParseAppliedManifest_StaticHasNoContainer(t *testing.T) {
	cfg := &AppConfig{App: "site", Type: TypeStatic, Source: "dist"}
	view := roundTripManifest(t, cfg, "")
	if view.Container != nil {
		t.Errorf("static manifest must not carry a container section, got %+v", view.Container)
	}
	if view.DeploymentType != TypeStatic {
		t.Errorf("deployment type = %q", view.DeploymentType)
	}
}

func TestParseAppliedManifest_RefusesMalformed(t *testing.T) {
	cases := []struct {
		name string
		data string
	}{
		{"empty", ""},
		{"not json", "teploy"},
		{"non-object root", `["app"]`},
		{"container not object", `{"container": 3}`},
		{"accessories not object", `{"accessories": []}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseAppliedManifest([]byte(tc.data)); err == nil {
				t.Fatalf("expected refusal for %q", tc.data)
			}
		})
	}
}

func TestParseAppliedManifest_NullSectionsTolerated(t *testing.T) {
	view, err := ParseAppliedManifest([]byte(`{"app":"x","container":null,"accessories":null}`))
	if err != nil {
		t.Fatalf("null sections must parse: %v", err)
	}
	if view.Container != nil || view.Accessories != nil {
		t.Errorf("null sections must stay nil: %+v", view)
	}
	if view.App != "x" {
		t.Errorf("app = %q", view.App)
	}
}
