package config

import (
	"bytes"
	"testing"
)

func TestNormalizeAndDigestEquivalentDefaults(t *testing.T) {
	a := &AppConfig{App: "web", Domain: "B.example.com, a.example.com", Image: "web:latest"}
	b := &AppConfig{
		App: "web", Domain: "a.example.com,b.example.com", Type: TypeContainer,
		Ingress: IngressCaddy, Image: "web:latest", Port: 80, Replicas: 1, StopTimeout: 10,
		Health: AppHealthConfig{Path: "/health", TimeoutSeconds: 30, IntervalSeconds: 1},
	}

	manifestA, digestA, err := NormalizeAndDigest(a, a.Image)
	if err != nil {
		t.Fatal(err)
	}
	manifestB, digestB, err := NormalizeAndDigest(b, b.Image)
	if err != nil {
		t.Fatal(err)
	}
	if digestA != digestB || !bytes.Equal(manifestA, manifestB) {
		t.Fatalf("equivalent configs differed:\n%s\n%s", manifestA, manifestB)
	}
}

func TestNormalizeAndDigestRedactsSecretValues(t *testing.T) {
	const secret = "do-not-persist-this-value"
	cfg := &AppConfig{
		App: "web", Domain: "web.example.com", Image: "web:latest",
		Env: map[string]string{"DATABASE_URL": secret},
		Accessories: map[string]AccessoryConfig{
			"db": {Image: "postgres:17", Env: map[string]string{"POSTGRES_PASSWORD": secret}},
		},
		Access: AccessConfig{BasicAuth: map[string]string{"admin": secret}},
	}

	manifest, _, err := NormalizeAndDigest(cfg, cfg.Image)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(manifest, []byte(secret)) {
		t.Fatalf("secret value persisted in manifest: %s", manifest)
	}
	for _, key := range []string{"DATABASE_URL", "POSTGRES_PASSWORD", "admin"} {
		if !bytes.Contains(manifest, []byte(key)) {
			t.Errorf("redacted manifest omitted key %q: %s", key, manifest)
		}
	}
}
