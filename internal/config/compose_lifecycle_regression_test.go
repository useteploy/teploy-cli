package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestComposePreservesBuildAndCommands(t *testing.T) {
	dir := t.TempDir()
	source := `services:
  web:
    build: {context: ./api, dockerfile: Dockerfile.prod}
    command: [node, "server file.js"]
    ports: ["3000:3000"]
  db:
    image: redis:7
    command: [redis-server, --appendonly, "yes"]
`
	if err := os.WriteFile(filepath.Join(dir, "compose.yml"), []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadCompose(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Context != "./api" || cfg.Dockerfile != "Dockerfile.prod" || cfg.Processes["web"] != "node 'server file.js'" || cfg.Accessories["db"].Command != "redis-server --appendonly yes" {
		t.Fatalf("discarded build/command: %+v", cfg)
	}
}
func TestComposeRejectsDifferentDockerfileWorkers(t *testing.T) {
	dir := t.TempDir()
	source := `services:
  web:
    build: {context: ., dockerfile: Dockerfile.web}
    ports: ["3000:3000"]
  worker:
    build: {context: ., dockerfile: Dockerfile.worker}
    command: node worker.js
`
	os.WriteFile(filepath.Join(dir, "compose.yml"), []byte(source), 0600)
	if _, err := LoadCompose(dir); err == nil || !strings.Contains(err.Error(), "independent build") {
		t.Fatalf("different image flattened: %v", err)
	}
}
