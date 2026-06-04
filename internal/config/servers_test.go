package config

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadServers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "servers.yml")

	content := `servers:
  prod:
    host: "1.2.3.4"
    user: "deploy"
    role: "app"
  staging:
    host: "5.6.7.8"
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadServers(path)
	if err != nil {
		t.Fatalf("LoadServers failed: %v", err)
	}

	if len(cfg.Servers) != 2 {
		t.Fatalf("expected 2 servers, got %d", len(cfg.Servers))
	}

	prod := cfg.Servers["prod"]
	if prod.Host != "1.2.3.4" {
		t.Fatalf("expected host 1.2.3.4, got %s", prod.Host)
	}
	if prod.User != "deploy" {
		t.Fatalf("expected user deploy, got %s", prod.User)
	}
	if prod.Role != "app" {
		t.Fatalf("expected role app, got %s", prod.Role)
	}

	staging := cfg.Servers["staging"]
	if staging.Host != "5.6.7.8" {
		t.Fatalf("expected host 5.6.7.8, got %s", staging.Host)
	}
	if staging.User != "" {
		t.Fatalf("expected empty user, got %s", staging.User)
	}
}

func TestLoadServers_WithTags(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "servers.yml")

	content := `servers:
  app1:
    host: "10.0.0.1"
    user: "root"
    tags:
      SHARD: "1"
      GPU_ENABLED: "true"
  app2:
    host: "10.0.0.2"
    user: "root"
    tags:
      SHARD: "2"
`
	os.WriteFile(path, []byte(content), 0644)

	cfg, err := LoadServers(path)
	if err != nil {
		t.Fatalf("LoadServers with tags: %v", err)
	}

	app1 := cfg.Servers["app1"]
	if app1.Tags["SHARD"] != "1" {
		t.Errorf("expected SHARD=1 for app1, got %q", app1.Tags["SHARD"])
	}
	if app1.Tags["GPU_ENABLED"] != "true" {
		t.Errorf("expected GPU_ENABLED=true for app1, got %q", app1.Tags["GPU_ENABLED"])
	}

	app2 := cfg.Servers["app2"]
	if app2.Tags["SHARD"] != "2" {
		t.Errorf("expected SHARD=2 for app2, got %q", app2.Tags["SHARD"])
	}
	if _, ok := app2.Tags["GPU_ENABLED"]; ok {
		t.Error("app2 should not have GPU_ENABLED tag")
	}
}

func TestLoadServers_NoTags(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "servers.yml")

	content := `servers:
  app1:
    host: "10.0.0.1"
`
	os.WriteFile(path, []byte(content), 0644)

	cfg, err := LoadServers(path)
	if err != nil {
		t.Fatalf("LoadServers: %v", err)
	}

	app1 := cfg.Servers["app1"]
	if app1.Tags != nil && len(app1.Tags) > 0 {
		t.Errorf("expected no tags, got %v", app1.Tags)
	}
}

func TestLoadServers_NotFound(t *testing.T) {
	_, err := LoadServers("/nonexistent/servers.yml")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestResolveServer_FlagsOverride(t *testing.T) {
	host, user, key, err := ResolveServer("anything", "10.0.0.1", "admin", "/my/key")
	if err != nil {
		t.Fatal(err)
	}
	if host != "10.0.0.1" {
		t.Fatalf("expected host 10.0.0.1, got %s", host)
	}
	if user != "admin" {
		t.Fatalf("expected user admin, got %s", user)
	}
	if key != "/my/key" {
		t.Fatalf("expected key /my/key, got %s", key)
	}
}

func TestResolveServer_EnvVars(t *testing.T) {
	t.Setenv("TEPLOY_HOST", "env-host.com")
	t.Setenv("TEPLOY_USER", "envuser")
	t.Setenv("TEPLOY_SSH_KEY", "/env/key")

	host, user, key, err := ResolveServer("anything", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if host != "env-host.com" {
		t.Fatalf("expected host env-host.com, got %s", host)
	}
	if user != "envuser" {
		t.Fatalf("expected user envuser, got %s", user)
	}
	if key != "/env/key" {
		t.Fatalf("expected key /env/key, got %s", key)
	}
}

func TestResolveServer_FallbackToRawIP(t *testing.T) {
	// Clear env vars
	t.Setenv("TEPLOY_HOST", "")
	t.Setenv("TEPLOY_USER", "")
	t.Setenv("TEPLOY_SSH_KEY", "")

	host, user, _, err := ResolveServer("192.168.1.1", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if host != "192.168.1.1" {
		t.Fatalf("expected host 192.168.1.1, got %s", host)
	}
	if user != "root" {
		t.Fatalf("expected default user root, got %s", user)
	}
}

func TestAddServer(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".teploy", "servers.yml")

	// Creates file and parent directory from scratch.
	if err := AddServer(path, "prod", "1.2.3.4", "root", "app", ""); err != nil {
		t.Fatalf("AddServer: %v", err)
	}

	cfg, err := LoadServers(path)
	if err != nil {
		t.Fatalf("LoadServers: %v", err)
	}
	if len(cfg.Servers) != 1 {
		t.Fatalf("expected 1 server, got %d", len(cfg.Servers))
	}
	if cfg.Servers["prod"].Host != "1.2.3.4" {
		t.Fatalf("expected host 1.2.3.4, got %s", cfg.Servers["prod"].Host)
	}
	if cfg.Servers["prod"].User != "root" {
		t.Fatalf("expected user root, got %s", cfg.Servers["prod"].User)
	}
	if cfg.Servers["prod"].Role != "app" {
		t.Fatalf("expected role app, got %s", cfg.Servers["prod"].Role)
	}
}

func TestAddServer_Append(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "servers.yml")

	if err := AddServer(path, "prod", "1.2.3.4", "root", "app", ""); err != nil {
		t.Fatal(err)
	}
	if err := AddServer(path, "staging", "5.6.7.8", "deploy", "app", ""); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadServers(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Servers) != 2 {
		t.Fatalf("expected 2 servers, got %d", len(cfg.Servers))
	}
	if cfg.Servers["staging"].Host != "5.6.7.8" {
		t.Fatalf("expected host 5.6.7.8, got %s", cfg.Servers["staging"].Host)
	}
}

func TestAddServer_Update(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "servers.yml")

	AddServer(path, "prod", "1.2.3.4", "root", "app", "")
	AddServer(path, "prod", "10.0.0.1", "admin", "lb", "")

	cfg, _ := LoadServers(path)
	if cfg.Servers["prod"].Host != "10.0.0.1" {
		t.Fatalf("expected updated host 10.0.0.1, got %s", cfg.Servers["prod"].Host)
	}
	if cfg.Servers["prod"].User != "admin" {
		t.Fatalf("expected updated user admin, got %s", cfg.Servers["prod"].User)
	}
	if cfg.Servers["prod"].Role != "lb" {
		t.Fatalf("expected updated role lb, got %s", cfg.Servers["prod"].Role)
	}
}

func TestAddServer_WithRole(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "servers.yml")

	if err := AddServer(path, "lb1", "10.0.0.1", "root", "lb", ""); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadServers(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Servers["lb1"].Role != "lb" {
		t.Fatalf("expected role lb, got %s", cfg.Servers["lb1"].Role)
	}
}

func TestRemoveServer(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "servers.yml")

	AddServer(path, "prod", "1.2.3.4", "root", "app", "")
	AddServer(path, "staging", "5.6.7.8", "deploy", "app", "")

	if err := RemoveServer(path, "prod"); err != nil {
		t.Fatalf("RemoveServer: %v", err)
	}

	cfg, err := LoadServers(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Servers) != 1 {
		t.Fatalf("expected 1 server after removal, got %d", len(cfg.Servers))
	}
	if _, ok := cfg.Servers["prod"]; ok {
		t.Fatal("prod should have been removed")
	}
	if _, ok := cfg.Servers["staging"]; !ok {
		t.Fatal("staging should still exist")
	}
}

func TestRemoveServer_NotFound(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "servers.yml")

	AddServer(path, "prod", "1.2.3.4", "root", "app", "")

	err := RemoveServer(path, "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent server")
	}
}

func TestRemoveServer_NoFile(t *testing.T) {
	err := RemoveServer("/nonexistent/servers.yml", "prod")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestListServers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "servers.yml")

	AddServer(path, "app1", "10.0.0.1", "root", "app", "")
	AddServer(path, "app2", "10.0.0.2", "root", "app", "")
	AddServer(path, "lb1", "10.0.0.3", "root", "lb", "")

	servers, err := ListServers(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 3 {
		t.Fatalf("expected 3 servers, got %d", len(servers))
	}
}

func TestGetServersByRole(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "servers.yml")

	AddServer(path, "app1", "10.0.0.1", "root", "app", "")
	AddServer(path, "app2", "10.0.0.2", "root", "app", "")
	AddServer(path, "lb1", "10.0.0.3", "root", "lb", "")
	AddServer(path, "default1", "10.0.0.4", "root", "", "") // empty role defaults to app

	appServers, err := GetServersByRole(path, "app")
	if err != nil {
		t.Fatal(err)
	}
	if len(appServers) != 3 {
		t.Fatalf("expected 3 app servers (including default), got %d", len(appServers))
	}

	lbServers, err := GetServersByRole(path, "lb")
	if err != nil {
		t.Fatal(err)
	}
	if len(lbServers) != 1 {
		t.Fatalf("expected 1 lb server, got %d", len(lbServers))
	}
	if _, ok := lbServers["lb1"]; !ok {
		t.Fatal("expected lb1 in lb servers")
	}
}

func TestGetServersByRole_NoMatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "servers.yml")

	AddServer(path, "app1", "10.0.0.1", "root", "app", "")

	lbServers, err := GetServersByRole(path, "lb")
	if err != nil {
		t.Fatal(err)
	}
	if len(lbServers) != 0 {
		t.Fatalf("expected 0 lb servers, got %d", len(lbServers))
	}
}

func TestLoadApp(t *testing.T) {
	dir := t.TempDir()
	content := "app: myapp\ndomain: myapp.com\nserver: prod\nimage: myapp:latest\n"
	if err := os.WriteFile(filepath.Join(dir, "teploy.yml"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadApp(dir)
	if err != nil {
		t.Fatalf("LoadApp: %v", err)
	}
	if cfg.App != "myapp" {
		t.Errorf("expected app myapp, got %s", cfg.App)
	}
	if cfg.Domain != "myapp.com" {
		t.Errorf("expected domain myapp.com, got %s", cfg.Domain)
	}
	if cfg.Server != "prod" {
		t.Errorf("expected server prod, got %s", cfg.Server)
	}
	if cfg.Image != "myapp:latest" {
		t.Errorf("expected image myapp:latest, got %s", cfg.Image)
	}
}

func TestLoadApp_YAML(t *testing.T) {
	dir := t.TempDir()
	content := "app: myapp\ndomain: myapp.com\n"
	if err := os.WriteFile(filepath.Join(dir, "teploy.yaml"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadApp(dir)
	if err != nil {
		t.Fatalf("LoadApp: %v", err)
	}
	if cfg.App != "myapp" {
		t.Errorf("expected app myapp, got %s", cfg.App)
	}
}

func TestLoadApp_NotFound(t *testing.T) {
	dir := t.TempDir()
	_, err := LoadApp(dir)
	if err == nil {
		t.Fatal("expected error for missing teploy.yml")
	}
}

func TestLoadApp_WithProcesses(t *testing.T) {
	dir := t.TempDir()
	content := `app: myapp
domain: myapp.com
processes:
  web: "npm start"
  worker: "npm run worker"
`
	if err := os.WriteFile(filepath.Join(dir, "teploy.yml"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadApp(dir)
	if err != nil {
		t.Fatalf("LoadApp: %v", err)
	}

	if len(cfg.Processes) != 2 {
		t.Fatalf("expected 2 processes, got %d", len(cfg.Processes))
	}
	if cfg.Processes["web"] != "npm start" {
		t.Errorf("expected web process 'npm start', got %s", cfg.Processes["web"])
	}
	if cfg.Processes["worker"] != "npm run worker" {
		t.Errorf("expected worker process 'npm run worker', got %s", cfg.Processes["worker"])
	}
}

func TestLoadApp_WithVolumes(t *testing.T) {
	dir := t.TempDir()
	content := `app: myapp
domain: myapp.com
volumes:
  data: "/app/data"
  uploads: "/app/uploads"
`
	if err := os.WriteFile(filepath.Join(dir, "teploy.yml"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadApp(dir)
	if err != nil {
		t.Fatalf("LoadApp: %v", err)
	}

	if len(cfg.Volumes) != 2 {
		t.Fatalf("expected 2 volumes, got %d", len(cfg.Volumes))
	}
	if cfg.Volumes["data"] != "/app/data" {
		t.Errorf("expected volume data=/app/data, got %s", cfg.Volumes["data"])
	}
	if cfg.Volumes["uploads"] != "/app/uploads" {
		t.Errorf("expected volume uploads=/app/uploads, got %s", cfg.Volumes["uploads"])
	}
}

func TestLoadApp_WithAccessories(t *testing.T) {
	dir := t.TempDir()
	content := `app: myapp
domain: myapp.com
accessories:
  postgres:
    image: "postgres:16"
    port: 5432
    env:
      POSTGRES_PASSWORD: auto
      POSTGRES_DB: myapp
    volumes:
      data: "/var/lib/postgresql/data"
  redis:
    image: "redis:7"
    port: 6379
`
	if err := os.WriteFile(filepath.Join(dir, "teploy.yml"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadApp(dir)
	if err != nil {
		t.Fatalf("LoadApp: %v", err)
	}

	if len(cfg.Accessories) != 2 {
		t.Fatalf("expected 2 accessories, got %d", len(cfg.Accessories))
	}

	pg := cfg.Accessories["postgres"]
	if pg.Image != "postgres:16" {
		t.Errorf("expected postgres image postgres:16, got %s", pg.Image)
	}
	if pg.Port != 5432 {
		t.Errorf("expected postgres port 5432, got %d", pg.Port)
	}
	if pg.Env["POSTGRES_PASSWORD"] != "auto" {
		t.Errorf("expected POSTGRES_PASSWORD auto, got %s", pg.Env["POSTGRES_PASSWORD"])
	}
	if pg.Env["POSTGRES_DB"] != "myapp" {
		t.Errorf("expected POSTGRES_DB myapp, got %s", pg.Env["POSTGRES_DB"])
	}
	if pg.Volumes["data"] != "/var/lib/postgresql/data" {
		t.Errorf("expected volume data, got %s", pg.Volumes["data"])
	}

	redis := cfg.Accessories["redis"]
	if redis.Image != "redis:7" {
		t.Errorf("expected redis image redis:7, got %s", redis.Image)
	}
	if redis.Port != 6379 {
		t.Errorf("expected redis port 6379, got %d", redis.Port)
	}
}

func TestLoadApp_WithHooks(t *testing.T) {
	dir := t.TempDir()
	content := `app: myapp
domain: myapp.com
hooks:
  pre_deploy: "npm run migrate"
  post_deploy: "npm run cache:clear"
`
	if err := os.WriteFile(filepath.Join(dir, "teploy.yml"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadApp(dir)
	if err != nil {
		t.Fatalf("LoadApp: %v", err)
	}
	if cfg.Hooks.PreDeploy != "npm run migrate" {
		t.Errorf("expected pre_deploy 'npm run migrate', got %s", cfg.Hooks.PreDeploy)
	}
	if cfg.Hooks.PostDeploy != "npm run cache:clear" {
		t.Errorf("expected post_deploy 'npm run cache:clear', got %s", cfg.Hooks.PostDeploy)
	}
}

func TestLoadApp_WithNotifications(t *testing.T) {
	dir := t.TempDir()
	content := `app: myapp
domain: myapp.com
notifications:
  webhook: "https://hooks.slack.com/services/xxx"
`
	if err := os.WriteFile(filepath.Join(dir, "teploy.yml"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadApp(dir)
	if err != nil {
		t.Fatalf("LoadApp: %v", err)
	}
	if cfg.Notifications.Webhook != "https://hooks.slack.com/services/xxx" {
		t.Errorf("expected webhook URL, got %s", cfg.Notifications.Webhook)
	}
}

func TestLoadApp_WithStopTimeout(t *testing.T) {
	dir := t.TempDir()
	content := `app: myapp
domain: myapp.com
stop_timeout: 30
`
	if err := os.WriteFile(filepath.Join(dir, "teploy.yml"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadApp(dir)
	if err != nil {
		t.Fatalf("LoadApp: %v", err)
	}
	if cfg.StopTimeout != 30 {
		t.Errorf("expected stop_timeout 30, got %d", cfg.StopTimeout)
	}
}

func TestLoadApp_WithServersAndParallel(t *testing.T) {
	dir := t.TempDir()
	content := `app: myapp
domain: myapp.com
servers:
  - app1
  - app2
  - app3
parallel: 2
`
	if err := os.WriteFile(filepath.Join(dir, "teploy.yml"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadApp(dir)
	if err != nil {
		t.Fatalf("LoadApp: %v", err)
	}
	if len(cfg.Servers) != 3 {
		t.Fatalf("expected 3 servers, got %d", len(cfg.Servers))
	}
	if cfg.Servers[0] != "app1" {
		t.Errorf("expected first server app1, got %s", cfg.Servers[0])
	}
	if cfg.Servers[1] != "app2" {
		t.Errorf("expected second server app2, got %s", cfg.Servers[1])
	}
	if cfg.Servers[2] != "app3" {
		t.Errorf("expected third server app3, got %s", cfg.Servers[2])
	}
	if cfg.Parallel != 2 {
		t.Errorf("expected parallel 2, got %d", cfg.Parallel)
	}
}

func TestLoadApp_MissingApp(t *testing.T) {
	dir := t.TempDir()
	content := "domain: myapp.com\n"
	os.WriteFile(filepath.Join(dir, "teploy.yml"), []byte(content), 0644)

	_, err := LoadApp(dir)
	if err == nil {
		t.Fatal("expected error for missing app")
	}
}

func TestLoadApp_MissingDomain(t *testing.T) {
	dir := t.TempDir()
	content := "app: myapp\n"
	os.WriteFile(filepath.Join(dir, "teploy.yml"), []byte(content), 0644)

	_, err := LoadApp(dir)
	if err == nil {
		t.Fatal("expected error for missing domain")
	}
}

func TestLoadApp_InvalidAppName(t *testing.T) {
	dir := t.TempDir()
	tests := []string{
		"My App",    // spaces
		"my_app",    // underscores
		"../escape", // path traversal
		"-leading",  // leading hyphen
		"UPPER",     // uppercase
	}

	for _, name := range tests {
		content := fmt.Sprintf("app: %q\ndomain: test.com\n", name)
		os.WriteFile(filepath.Join(dir, "teploy.yml"), []byte(content), 0644)

		_, err := LoadApp(dir)
		if err == nil {
			t.Errorf("expected error for invalid app name %q", name)
		}
	}
}

func TestLoadApp_ValidAppNames(t *testing.T) {
	dir := t.TempDir()
	tests := []string{"myapp", "my-app", "app1", "a", "my-app-2"}

	for _, name := range tests {
		content := fmt.Sprintf("app: %s\ndomain: test.com\n", name)
		os.WriteFile(filepath.Join(dir, "teploy.yml"), []byte(content), 0644)

		_, err := LoadApp(dir)
		if err != nil {
			t.Errorf("unexpected error for valid app name %q: %v", name, err)
		}
	}
}

// AddServer must not lose tags/vpn_ip that were set in servers.yml when a server
// is re-added (e.g. to change its host) — that data drives per-host env
// injection and mesh routing, and dropping it silently broke deploys.
func TestAddServer_PreservesTagsAndVpnIP(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "servers.yml")

	// Seed a server with tags + vpn_ip (as if hand-edited in yaml).
	seed := "servers:\n  prod:\n    host: 1.2.3.4\n    user: deploy\n    role: app\n    vpn_ip: 100.64.0.1\n    tags:\n      region: us-east\n"
	if err := os.WriteFile(path, []byte(seed), 0644); err != nil {
		t.Fatal(err)
	}

	// Re-add to change only the host, with empty user/role/vpnIP.
	if err := AddServer(path, "prod", "5.6.7.8", "", "", ""); err != nil {
		t.Fatalf("AddServer: %v", err)
	}

	cfg, err := LoadServers(path)
	if err != nil {
		t.Fatal(err)
	}
	s := cfg.Servers["prod"]
	if s.Host != "5.6.7.8" {
		t.Errorf("host should update, got %q", s.Host)
	}
	if s.User != "deploy" {
		t.Errorf("user should be preserved, got %q", s.User)
	}
	if s.Role != "app" {
		t.Errorf("role should be preserved, got %q", s.Role)
	}
	if s.VpnIP != "100.64.0.1" {
		t.Errorf("vpn_ip should be preserved, got %q", s.VpnIP)
	}
	if s.Tags["region"] != "us-east" {
		t.Errorf("tags should be preserved, got %v", s.Tags)
	}
}
