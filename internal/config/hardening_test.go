package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadApp_AppNameTooLong(t *testing.T) {
	dir := t.TempDir()
	name := strings.Repeat("a", 64) // 64 chars, max is 63
	content := fmt.Sprintf("app: %s\ndomain: test.com\n", name)
	os.WriteFile(filepath.Join(dir, "teploy.yml"), []byte(content), 0644)

	_, err := LoadApp(dir)
	if err == nil {
		t.Fatal("expected error for app name > 63 chars")
	}
	if !strings.Contains(err.Error(), "too long") {
		t.Errorf("expected 'too long' error, got: %v", err)
	}
}

func TestLoadApp_AppNameExactly63(t *testing.T) {
	dir := t.TempDir()
	name := strings.Repeat("a", 63) // exactly 63
	content := fmt.Sprintf("app: %s\ndomain: test.com\n", name)
	os.WriteFile(filepath.Join(dir, "teploy.yml"), []byte(content), 0644)

	_, err := LoadApp(dir)
	if err != nil {
		t.Errorf("app name of exactly 63 chars should be valid: %v", err)
	}
}

func TestLoadApp_InvalidAccessoryName(t *testing.T) {
	dir := t.TempDir()
	content := `app: myapp
domain: myapp.com
accessories:
  "../escape":
    image: "postgres:16"
`
	os.WriteFile(filepath.Join(dir, "teploy.yml"), []byte(content), 0644)

	_, err := LoadApp(dir)
	if err == nil {
		t.Fatal("expected error for path traversal in accessory name")
	}
}

func TestLoadApp_InvalidProcessName(t *testing.T) {
	dir := t.TempDir()
	content := `app: myapp
domain: myapp.com
processes:
  "web; rm -rf /": "npm start"
`
	os.WriteFile(filepath.Join(dir, "teploy.yml"), []byte(content), 0644)

	_, err := LoadApp(dir)
	if err == nil {
		t.Fatal("expected error for shell injection in process name")
	}
}

func TestLoadApp_ValidAccessoryNames(t *testing.T) {
	dir := t.TempDir()
	content := `app: myapp
domain: myapp.com
accessories:
  postgres:
    image: "postgres:16"
  my-redis:
    image: "redis:7"
`
	os.WriteFile(filepath.Join(dir, "teploy.yml"), []byte(content), 0644)

	_, err := LoadApp(dir)
	if err != nil {
		t.Errorf("valid accessory names should pass: %v", err)
	}
}

func TestLoadApp_InvalidDomain(t *testing.T) {
	dir := t.TempDir()
	domains := []string{
		"domain$(whoami).com",
		"domain;rm.com",
		"domain with spaces.com",
		"domain\ninjected",
	}

	for _, domain := range domains {
		content := fmt.Sprintf("app: myapp\ndomain: %q\n", domain)
		os.WriteFile(filepath.Join(dir, "teploy.yml"), []byte(content), 0644)

		_, err := LoadApp(dir)
		if err == nil {
			t.Errorf("expected error for invalid domain %q", domain)
		}
	}
}

func TestLoadApp_ValidDomains(t *testing.T) {
	dir := t.TempDir()
	domains := []string{"myapp.com", "sub.myapp.com", "192.168.1.1.nip.io", "my-app.example.com"}

	for _, domain := range domains {
		content := fmt.Sprintf("app: myapp\ndomain: %s\n", domain)
		os.WriteFile(filepath.Join(dir, "teploy.yml"), []byte(content), 0644)

		_, err := LoadApp(dir)
		if err != nil {
			t.Errorf("valid domain %q should pass: %v", domain, err)
		}
	}
}

func TestLoadApp_InvalidVolumeName(t *testing.T) {
	dir := t.TempDir()
	content := `app: myapp
domain: myapp.com
volumes:
  "../../escape": "/app/data"
`
	os.WriteFile(filepath.Join(dir, "teploy.yml"), []byte(content), 0644)

	_, err := LoadApp(dir)
	if err == nil {
		t.Fatal("expected error for path traversal in volume name")
	}
}

func TestLoadApp_HostBindVolumes(t *testing.T) {
	dir := t.TempDir()
	content := `app: myapp
domain: myapp.com
volumes:
  "/srv/trusted-clone": "/srv/trusted-clone"
  "/srv/creds": "/home/node/.ssh:ro"
`
	os.WriteFile(filepath.Join(dir, "teploy.yml"), []byte(content), 0644)

	cfg, err := LoadApp(dir)
	if err != nil {
		t.Fatalf("absolute-path volume keys are host binds and must load: %v", err)
	}
	if got := cfg.Volumes["/srv/creds"]; got != "/home/node/.ssh:ro" {
		t.Fatalf("bind mount mode suffix must survive load, got %q", got)
	}
	if !IsHostBindVolume("/srv/trusted-clone") || IsHostBindVolume("app-data") {
		t.Fatal("IsHostBindVolume must key on the leading slash, nothing else")
	}

	// A relative destination is refused whichever side of the mapping failed.
	bad := `app: myapp
domain: myapp.com
volumes:
  "/srv/clone": "relative/path"
`
	os.WriteFile(filepath.Join(dir, "teploy.yml"), []byte(bad), 0644)
	if _, err := LoadApp(dir); err == nil {
		t.Fatal("expected error for a relative volume destination")
	}
}

func TestLoadApp_TOML(t *testing.T) {
	dir := t.TempDir()
	content := `app = "myapp"
domain = "myapp.com"
server = "1.2.3.4"
port = 3000
build_local = true
stop_timeout = 30

[hooks]
pre_deploy = "npm run migrate"
post_deploy = "npm run seed"

[volumes]
data = "/app/data"

[processes]
web = "npm start"
worker = "npm run worker"
`
	os.WriteFile(filepath.Join(dir, "teploy.toml"), []byte(content), 0644)

	cfg, err := LoadApp(dir)
	if err != nil {
		t.Fatalf("LoadApp TOML: %v", err)
	}
	if cfg.App != "myapp" {
		t.Errorf("expected app myapp, got %s", cfg.App)
	}
	if cfg.Domain != "myapp.com" {
		t.Errorf("expected domain myapp.com, got %s", cfg.Domain)
	}
	if cfg.Server != "1.2.3.4" {
		t.Errorf("expected server 1.2.3.4, got %s", cfg.Server)
	}
	if cfg.Port != 3000 {
		t.Errorf("expected port 3000, got %d", cfg.Port)
	}
	if !cfg.BuildLocal {
		t.Error("expected build_local true")
	}
	if cfg.StopTimeout != 30 {
		t.Errorf("expected stop_timeout 30, got %d", cfg.StopTimeout)
	}
	if cfg.Hooks.PreDeploy != "npm run migrate" {
		t.Errorf("expected pre_deploy hook, got %q", cfg.Hooks.PreDeploy)
	}
	if cfg.Volumes["data"] != "/app/data" {
		t.Errorf("expected volume data=/app/data, got %q", cfg.Volumes["data"])
	}
	if cfg.Processes["worker"] != "npm run worker" {
		t.Errorf("expected worker process, got %q", cfg.Processes["worker"])
	}
}

func TestLoadApp_TOMLWithAccessories(t *testing.T) {
	dir := t.TempDir()
	content := `app = "myapp"
domain = "myapp.com"

[accessories.postgres]
image = "postgres:16"
port = 5432

[accessories.postgres.env]
POSTGRES_PASSWORD = "secret"
`
	os.WriteFile(filepath.Join(dir, "teploy.toml"), []byte(content), 0644)

	cfg, err := LoadApp(dir)
	if err != nil {
		t.Fatalf("LoadApp TOML with accessories: %v", err)
	}
	pg, ok := cfg.Accessories["postgres"]
	if !ok {
		t.Fatal("expected postgres accessory")
	}
	if pg.Image != "postgres:16" {
		t.Errorf("expected image postgres:16, got %s", pg.Image)
	}
	if pg.Port != 5432 {
		t.Errorf("expected port 5432, got %d", pg.Port)
	}
	if pg.Env["POSTGRES_PASSWORD"] != "secret" {
		t.Errorf("expected env var, got %q", pg.Env["POSTGRES_PASSWORD"])
	}
}

func TestLoadApp_YAMLTakesPrecedenceOverTOML(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "teploy.yml"), []byte("app: fromyaml\ndomain: yaml.com\n"), 0644)
	os.WriteFile(filepath.Join(dir, "teploy.toml"), []byte("app = \"fromtoml\"\ndomain = \"toml.com\"\n"), 0644)

	cfg, err := LoadApp(dir)
	if err != nil {
		t.Fatalf("LoadApp: %v", err)
	}
	if cfg.App != "fromyaml" {
		t.Errorf("YAML should take precedence, got app=%s", cfg.App)
	}
}

func TestLoadApp_SpecialCharsInAppName(t *testing.T) {
	dir := t.TempDir()
	names := []string{
		"app$(whoami)",
		"app;ls",
		"app`id`",
		"app|cat",
		"app&bg",
		"app\ninjected",
	}

	for _, name := range names {
		content := fmt.Sprintf("app: %q\ndomain: test.com\n", name)
		os.WriteFile(filepath.Join(dir, "teploy.yml"), []byte(content), 0644)

		_, err := LoadApp(dir)
		if err == nil {
			t.Errorf("expected error for shell-dangerous app name %q", name)
		}
	}
}

func TestLoadApp_ValidPlatform(t *testing.T) {
	dir := t.TempDir()
	platforms := []string{"linux/amd64", "linux/arm64", "linux/arm/v7"}
	for _, p := range platforms {
		content := fmt.Sprintf("app: myapp\ndomain: myapp.com\nplatform: %s\n", p)
		os.WriteFile(filepath.Join(dir, "teploy.yml"), []byte(content), 0644)

		cfg, err := LoadApp(dir)
		if err != nil {
			t.Errorf("valid platform %q should pass: %v", p, err)
		}
		if cfg.Platform != p {
			t.Errorf("expected platform %q, got %q", p, cfg.Platform)
		}
	}
}

func TestLoadApp_InvalidPlatform(t *testing.T) {
	dir := t.TempDir()
	platforms := []string{"not valid", "linux", "linux/amd64;rm", "$(whoami)/amd64"}
	for _, p := range platforms {
		content := fmt.Sprintf("app: myapp\ndomain: myapp.com\nplatform: %q\n", p)
		os.WriteFile(filepath.Join(dir, "teploy.yml"), []byte(content), 0644)

		_, err := LoadApp(dir)
		if err == nil {
			t.Errorf("expected error for invalid platform %q", p)
		}
	}
}

func TestLoadAppWithDestination(t *testing.T) {
	dir := t.TempDir()

	// Base config.
	base := "app: myapp\ndomain: myapp.com\nserver: prod-server\nport: 3000\n"
	os.WriteFile(filepath.Join(dir, "teploy.yml"), []byte(base), 0644)

	// Staging overlay.
	overlay := "domain: staging.myapp.com\nserver: staging-server\nport: 3001\n"
	os.WriteFile(filepath.Join(dir, "teploy.staging.yml"), []byte(overlay), 0644)

	cfg, err := LoadAppWithDestination(dir, "staging", OverlayOptions{})
	if err != nil {
		t.Fatalf("LoadAppWithDestination: %v", err)
	}

	// App should come from base (not overridden in overlay).
	if cfg.App != "myapp" {
		t.Errorf("expected app myapp, got %s", cfg.App)
	}
	// Domain, server, port should come from overlay.
	if cfg.Domain != "staging.myapp.com" {
		t.Errorf("expected domain staging.myapp.com, got %s", cfg.Domain)
	}
	if cfg.Server != "staging-server" {
		t.Errorf("expected server staging-server, got %s", cfg.Server)
	}
	if cfg.Port != 3001 {
		t.Errorf("expected port 3001, got %d", cfg.Port)
	}
}

func TestLoadAppWithDestination_TOML(t *testing.T) {
	dir := t.TempDir()

	base := "app: myapp\ndomain: myapp.com\n"
	os.WriteFile(filepath.Join(dir, "teploy.yml"), []byte(base), 0644)

	overlay := "domain = \"staging.myapp.com\"\nserver = \"staging-box\"\n"
	os.WriteFile(filepath.Join(dir, "teploy.staging.toml"), []byte(overlay), 0644)

	cfg, err := LoadAppWithDestination(dir, "staging", OverlayOptions{})
	if err != nil {
		t.Fatalf("LoadAppWithDestination TOML: %v", err)
	}
	if cfg.Domain != "staging.myapp.com" {
		t.Errorf("expected staging domain, got %s", cfg.Domain)
	}
}

func TestLoadAppWithDestination_MergesMaps(t *testing.T) {
	dir := t.TempDir()

	base := `app: myapp
domain: myapp.com
volumes:
  data: /app/data
  logs: /app/logs
processes:
  web: "npm start"
  worker: "npm run worker"
`
	os.WriteFile(filepath.Join(dir, "teploy.yml"), []byte(base), 0644)

	overlay := `volumes:
  data: /staging/data
processes:
  worker: "npm run staging-worker"
`
	os.WriteFile(filepath.Join(dir, "teploy.staging.yml"), []byte(overlay), 0644)

	cfg, err := LoadAppWithDestination(dir, "staging", OverlayOptions{})
	if err != nil {
		t.Fatalf("LoadAppWithDestination: %v", err)
	}

	// data should be overridden, logs should be preserved.
	if cfg.Volumes["data"] != "/staging/data" {
		t.Errorf("expected overridden volume, got %s", cfg.Volumes["data"])
	}
	if cfg.Volumes["logs"] != "/app/logs" {
		t.Errorf("expected preserved volume, got %s", cfg.Volumes["logs"])
	}
	// worker overridden, web preserved.
	if cfg.Processes["worker"] != "npm run staging-worker" {
		t.Errorf("expected overridden worker, got %s", cfg.Processes["worker"])
	}
	if cfg.Processes["web"] != "npm start" {
		t.Errorf("expected preserved web, got %s", cfg.Processes["web"])
	}
}

func TestLoadAppWithDestination_NotFound(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "teploy.yml"), []byte("app: myapp\ndomain: myapp.com\n"), 0644)

	_, err := LoadAppWithDestination(dir, "production", OverlayOptions{})
	if err == nil {
		t.Fatal("expected error when destination overlay not found")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected 'not found' error, got: %v", err)
	}
}

func TestLoadApp_WithAssets(t *testing.T) {
	dir := t.TempDir()
	content := `app: myapp
domain: myapp.com
assets:
  path: /app/public/assets
  keep_days: 14
`
	os.WriteFile(filepath.Join(dir, "teploy.yml"), []byte(content), 0644)

	cfg, err := LoadApp(dir)
	if err != nil {
		t.Fatalf("LoadApp with assets: %v", err)
	}
	if cfg.Assets.Path != "/app/public/assets" {
		t.Errorf("expected asset path /app/public/assets, got %s", cfg.Assets.Path)
	}
	if cfg.Assets.KeepDays != 14 {
		t.Errorf("expected keep_days 14, got %d", cfg.Assets.KeepDays)
	}
}

func TestLoadApp_AssetsTOML(t *testing.T) {
	dir := t.TempDir()
	content := `app = "myapp"
domain = "myapp.com"

[assets]
path = "/app/dist/static"
keep_days = 30
`
	os.WriteFile(filepath.Join(dir, "teploy.toml"), []byte(content), 0644)

	cfg, err := LoadApp(dir)
	if err != nil {
		t.Fatalf("LoadApp TOML with assets: %v", err)
	}
	if cfg.Assets.Path != "/app/dist/static" {
		t.Errorf("expected asset path, got %s", cfg.Assets.Path)
	}
	if cfg.Assets.KeepDays != 30 {
		t.Errorf("expected keep_days 30, got %d", cfg.Assets.KeepDays)
	}
}

func TestLoadAppWithDestination_AssetsOverride(t *testing.T) {
	dir := t.TempDir()

	base := `app: myapp
domain: myapp.com
assets:
  path: /app/public/assets
  keep_days: 7
`
	os.WriteFile(filepath.Join(dir, "teploy.yml"), []byte(base), 0644)

	overlay := `assets:
  path: /app/dist/static
  keep_days: 30
`
	os.WriteFile(filepath.Join(dir, "teploy.staging.yml"), []byte(overlay), 0644)

	cfg, err := LoadAppWithDestination(dir, "staging", OverlayOptions{})
	if err != nil {
		t.Fatalf("LoadAppWithDestination: %v", err)
	}
	if cfg.Assets.Path != "/app/dist/static" {
		t.Errorf("expected overridden asset path, got %s", cfg.Assets.Path)
	}
	if cfg.Assets.KeepDays != 30 {
		t.Errorf("expected overridden keep_days 30, got %d", cfg.Assets.KeepDays)
	}
}
