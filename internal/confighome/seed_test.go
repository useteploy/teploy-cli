package confighome

import (
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/releasemeta"
)

// The seed rebuilt from what a folder deploy recorded must reproduce the
// effective config exactly.
func TestSeedReproducesFolderConfig(t *testing.T) {
	folder := []byte(`app: blog
server: prod
domain: Blog.example.com, www.blog.example.com
image: example/blog:1.4
port: 3000
replicas: 2
memory: 512m
volumes:
  data: /app/data
env:
  LOG_LEVEL: info
  PRICE: "5$"
processes:
  worker: bin/worker
health:
  path: /up
firewall:
  allow_ips: [10.0.0.0/8]
tls:
  internal: true
`)
	cfg, err := config.LoadAppBytes(folder)
	if err != nil {
		t.Fatal(err)
	}
	manifest, digest, err := config.NormalizeAndDigest(cfg, "example/blog:1.4")
	if err != nil {
		t.Fatal(err)
	}
	rec := &releasemeta.Record{ImageRef: "example/blog:1.4", ManifestSHA256: digest,
		Env: map[string]string{"LOG_LEVEL": "info", "PRICE": "5$"}, Processes: map[string]string{"worker": "bin/worker"}}
	seed, problems, err := Seed(rec, manifest)
	if err != nil || len(problems) > 0 {
		t.Fatalf("seed: %v %v\n%s", err, problems, seed)
	}
	ok, err := SeedMatches(seed, rec.ImageRef, digest)
	if err != nil || !ok {
		t.Fatalf("seed does not reproduce the deployed config (%v):\n%s", err, seed)
	}
	if !strings.Contains(string(seed), "PRICE: 5$$") {
		t.Fatalf("a literal $ must be escaped for Teploy home:\n%s", seed)
	}
	if _, err := Load(seed, "blog"); err != nil {
		t.Fatalf("the seed must be storable in Teploy: %v", err)
	}
}

func TestSeedRefusesWhatTheServerDoesNotKeep(t *testing.T) {
	cfg, _ := config.LoadAppBytes([]byte("app: blog\ndomain: b.example.com\nimage: x:1\nenv:\n  DB_URL: secret:DB_URL\nhooks:\n  pre_deploy: bin/migrate\naccessories:\n  db:\n    image: postgres:16\n"))
	manifest, _, _ := config.NormalizeAndDigest(cfg, "x:1")
	_, problems, err := Seed(&releasemeta.Record{ImageRef: "x:1"}, manifest)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(problems, "\n")
	for _, want := range []string{"DB_URL", "pre_deploy", "accessories"} {
		if !strings.Contains(joined, want) {
			t.Errorf("problems miss %q: %v", want, problems)
		}
	}
}
