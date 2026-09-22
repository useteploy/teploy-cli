package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadCompose_BasicMapping(t *testing.T) {
	dir := t.TempDir()
	compose := `
services:
  web:
    build: .
    ports: ["3000:3000"]
    depends_on: [db, redis]
  worker:
    build: .
    command: npm run worker
  db:
    image: postgres:16
    volumes:
      - pgdata:/var/lib/postgresql/data
    environment:
      POSTGRES_PASSWORD: pass
  redis:
    image: redis:7
volumes:
  pgdata:
`
	os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte(compose), 0644)

	cfg, err := LoadCompose(dir)
	if err != nil {
		t.Fatalf("LoadCompose: %v", err)
	}
	if cfg == nil {
		t.Fatal("expected non-nil config")
	}

	// Web + worker processes.
	if cfg.Processes["web"] != "" {
		t.Errorf("web process should have empty command, got %q", cfg.Processes["web"])
	}
	if cfg.Processes["worker"] != "npm run worker" {
		t.Errorf("worker process command = %q, want 'npm run worker'", cfg.Processes["worker"])
	}

	// Accessories.
	pg, ok := cfg.Accessories["db"]
	if !ok {
		t.Fatal("expected postgres accessory 'db'")
	}
	if pg.Image != "postgres:16" {
		t.Errorf("postgres image = %s, want postgres:16", pg.Image)
	}
	if pg.Port != 5432 {
		t.Errorf("postgres port = %d, want 5432", pg.Port)
	}
	if pg.Env["POSTGRES_PASSWORD"] != "pass" {
		t.Errorf("postgres env = %v", pg.Env)
	}

	redis, ok := cfg.Accessories["redis"]
	if !ok {
		t.Fatal("expected redis accessory")
	}
	if redis.Image != "redis:7" {
		t.Errorf("redis image = %s, want redis:7", redis.Image)
	}
	if redis.Port != 6379 {
		t.Errorf("redis port = %d, want 6379", redis.Port)
	}

	// No pre-built image (uses build context).
	if cfg.Image != "" {
		t.Errorf("expected no image for build-based service, got %s", cfg.Image)
	}
}

func TestLoadCompose_NoComposeFile(t *testing.T) {
	dir := t.TempDir()
	cfg, err := LoadCompose(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg != nil {
		t.Fatalf("expected nil config, got %+v", cfg)
	}
}

func TestLoadCompose_ComposeYml(t *testing.T) {
	dir := t.TempDir()
	compose := `
services:
  app:
    image: myapp:latest
    ports: ["8080:8080"]
`
	os.WriteFile(filepath.Join(dir, "compose.yml"), []byte(compose), 0644)

	cfg, err := LoadCompose(dir)
	if err != nil {
		t.Fatalf("LoadCompose: %v", err)
	}
	if cfg == nil {
		t.Fatal("expected config from compose.yml")
	}
	if cfg.Image != "myapp:latest" {
		t.Errorf("expected image myapp:latest, got %s", cfg.Image)
	}
}

func TestLoadCompose_NoPorts(t *testing.T) {
	dir := t.TempDir()
	compose := `
services:
  worker:
    build: .
    command: npm run worker
`
	os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte(compose), 0644)

	_, err := LoadCompose(dir)
	if err == nil {
		t.Fatal("expected error when no service has ports")
	}
}

func TestLoadCompose_ImageService(t *testing.T) {
	dir := t.TempDir()
	compose := `
services:
  web:
    image: ghcr.io/myorg/myapp:v1
    ports: ["3000:3000"]
`
	os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte(compose), 0644)

	cfg, err := LoadCompose(dir)
	if err != nil {
		t.Fatalf("LoadCompose: %v", err)
	}
	if cfg.Image != "ghcr.io/myorg/myapp:v1" {
		t.Errorf("expected image from compose, got %s", cfg.Image)
	}
}

func TestLoadCompose_BuildContextStruct(t *testing.T) {
	dir := t.TempDir()
	compose := `
services:
  web:
    build:
      context: .
      dockerfile: Dockerfile.prod
    ports: ["3000:3000"]
  worker:
    build:
      context: .
    command: node worker.js
`
	os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte(compose), 0644)

	cfg, err := LoadCompose(dir)
	if err != nil {
		t.Fatalf("LoadCompose: %v", err)
	}
	if cfg.Processes["worker"] != "node worker.js" {
		t.Errorf("expected worker process, got %q", cfg.Processes["worker"])
	}
}

func TestLoadCompose_EnvironmentList(t *testing.T) {
	dir := t.TempDir()
	compose := `
services:
  web:
    build: .
    ports: ["3000:3000"]
  db:
    image: postgres:16
    environment:
      - POSTGRES_PASSWORD=secret
      - POSTGRES_DB=mydb
`
	os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte(compose), 0644)

	cfg, err := LoadCompose(dir)
	if err != nil {
		t.Fatalf("LoadCompose: %v", err)
	}
	pg := cfg.Accessories["db"]
	if pg.Env["POSTGRES_PASSWORD"] != "secret" {
		t.Errorf("expected POSTGRES_PASSWORD=secret, got %v", pg.Env)
	}
	if pg.Env["POSTGRES_DB"] != "mydb" {
		t.Errorf("expected POSTGRES_DB=mydb, got %v", pg.Env)
	}
}

func TestLoadCompose_TeployYmlWins(t *testing.T) {
	dir := t.TempDir()

	// Write both files.
	os.WriteFile(filepath.Join(dir, "teploy.yml"), []byte("app: myapp\ndomain: myapp.com\n"), 0644)
	os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte("services:\n  web:\n    image: other\n    ports: ['3000:3000']\n"), 0644)

	cfg, err := LoadApp(dir)
	if err != nil {
		t.Fatalf("LoadApp: %v", err)
	}
	// teploy.yml should win.
	if cfg.App != "myapp" {
		t.Errorf("expected app from teploy.yml, got %s", cfg.App)
	}
}

func TestIsAccessoryImage(t *testing.T) {
	tests := []struct {
		image string
		want  bool
	}{
		{"postgres:16", true},
		{"redis:7", true},
		{"mysql:8", true},
		{"mongo:latest", true},
		{"myapp:latest", false},
		{"ghcr.io/myorg/myapp:v1", false},
		{"", false}, // Registry ports: the colon before the last slash is a registry
		// host port, not a tag separator (teploy-cli-08 twin — this used
		// to reduce to "registry.example" and disable classification).
		{"registry.example:5000/postgres:16", true},
		{"registry.example:5000/redis", true},
		{"ghcr.io/registry:5000/namespace/mysql:8", true},
		{"postgres@sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", true},
		{"registry.example:5000/myapp:latest", false},
	}

	for _, tt := range tests {
		if got := isAccessoryImage(tt.image); got != tt.want {
			t.Errorf("isAccessoryImage(%q) = %v, want %v", tt.image, got, tt.want)
		}
	}
}

func TestParseCommand(t *testing.T) {
	// String command.
	if got := parseCommand("npm run worker"); got != "npm run worker" {
		t.Errorf("string command: got %q", got)
	}

	// List command.
	list := []interface{}{"npm", "run", "worker"}
	if got := parseCommand(list); got != "npm run worker" {
		t.Errorf("list command: got %q", got)
	}

	// Nil command.
	if got := parseCommand(nil); got != "" {
		t.Errorf("nil command: got %q", got)
	}

	// teploy-cli-15: exec-form argument boundaries must survive the later
	// `sh -c` re-parse. Each argument containing spaces, quotes, empty
	// strings, or "$" is shell-quoted; safe arguments stay bare.
	tests := []struct {
		args []interface{}
		want string
	}{
		{[]interface{}{"sh", "-c", "printf 'hello world'"}, "sh -c 'printf '\"'\"'hello world'\"'\"''"},
		{[]interface{}{"echo", "a b", ""}, `echo 'a b' ''`},
		{[]interface{}{"echo", "$HOME"}, `echo '$HOME'`},
		{[]interface{}{"echo", `say "hi"`}, `echo 'say "hi"'`},
		{[]interface{}{"node", "worker.js", "--flag=1"}, `node worker.js --flag=1`},
	}
	for _, tt := range tests {
		if got := parseCommand(tt.args); got != tt.want {
			t.Errorf("parseCommand(%v) = %q, want %q", tt.args, got, tt.want)
		}
	}

	// The joined form must re-split (via sh -c semantics) into the exact
	// original argv: quoting a shell word and re-parsing it yields the
	// same single argument.
	original := []interface{}{"sh", "-c", "printf 'hello world'"}
	joined := parseCommand(original)
	parts := splitShellWords(t, joined)
	if len(parts) != len(original) {
		t.Fatalf("joined command %q re-splits into %d words, want %d", joined, len(parts), len(original))
	}
	for i := range parts {
		if parts[i] != original[i] {
			t.Errorf("re-split word %d = %q, want %q (joined: %s)", i, parts[i], original[i], joined)
		}
	}
}

// splitShellWords re-splits a `sh -c` command line the way /bin/sh would,
// honoring single- and double-quoted spans (covers the '"'"' idiom
// ShellQuote emits).
func splitShellWords(t *testing.T, s string) []string {
	t.Helper()
	var parts []string
	var cur strings.Builder
	hasWord := false
	flush := func() {
		if hasWord {
			parts = append(parts, cur.String())
			cur.Reset()
			hasWord = false
		}
	}
	for i := 0; i < len(s); {
		switch c := s[i]; {
		case c == '\'':
			j := i + 1
			for j < len(s) && s[j] != '\'' {
				j++
			}
			cur.WriteString(s[i+1 : j])
			hasWord = true
			i = j + 1
		case c == '"':
			j := i + 1
			for j < len(s) && s[j] != '"' {
				j++
			}
			cur.WriteString(s[i+1 : j])
			hasWord = true
			i = j + 1
		case c == ' ' || c == '\t':
			flush()
			i++
		default:
			cur.WriteByte(c)
			hasWord = true
			i++
		}
	}
	flush()
	return parts
}

// TestLoadCompose_DatabaseWithPortsIsNeverWeb: a database that publishes
// ports must classify as an accessory, and the app service must be picked
// regardless of map insertion order (teploy-cli-14 — the old loop took the
// first service with ports, so the same file imported differently per run).
func TestLoadCompose_DatabaseWithPortsIsNeverWeb(t *testing.T) {
	// db listed FIRST: the order that used to mispick the database as web.
	compose := `
services:
  db:
    image: postgres:16
    ports: ["5432:5432"]
    environment:
      POSTGRES_PASSWORD: pass
  web:
    image: myapp:latest
    ports: ["3000:3000"]
`
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte(compose), 0644)

	cfg, err := LoadCompose(dir)
	if err != nil {
		t.Fatalf("LoadCompose: %v", err)
	}
	if cfg.Image != "myapp:latest" {
		t.Errorf("web image = %q, want myapp:latest", cfg.Image)
	}
	pg, ok := cfg.Accessories["db"]
	if !ok {
		t.Fatal("expected postgres to classify as an accessory despite published ports")
	}
	if pg.Image != "postgres:16" {
		t.Errorf("postgres image = %q", pg.Image)
	}
	if _, ok := cfg.Accessories["db"]; !ok {
		t.Fatal("expected postgres accessory")
	}
	if len(cfg.Processes) != 0 {
		t.Errorf("web with an empty command collapses to no processes entry, got %v", cfg.Processes)
	}
}

// TestLoadCompose_AmbiguousPortsFailsClearly: two non-accessory services
// with published ports have no principled automatic pick — import must
// fail naming both, deterministically, instead of choosing by map order.
func TestLoadCompose_AmbiguousPortsFailsClearly(t *testing.T) {
	compose := `
services:
  web:
    image: myapp:latest
    ports: ["3000:3000"]
  api:
    image: myapi:latest
    ports: ["8080:8080"]
`
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte(compose), 0644)

	_, err := LoadCompose(dir)
	if err == nil {
		t.Fatal("expected an ambiguity error")
	}
	if !strings.Contains(err.Error(), "ambiguous") ||
		!strings.Contains(err.Error(), "api") ||
		!strings.Contains(err.Error(), "web") {
		t.Errorf("error must name the ambiguity and both candidates, got: %v", err)
	}
}

// TestLoadCompose_OnlyAccessoryWithPorts: when every port-publishing
// service is a known accessory, the error must say so rather than
// importing the database as the app.
func TestLoadCompose_OnlyAccessoryWithPorts(t *testing.T) {
	compose := `
services:
  db:
    image: postgres:16
    ports: ["5432:5432"]
`
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte(compose), 0644)

	_, err := LoadCompose(dir)
	if err == nil {
		t.Fatal("expected an error when only an accessory publishes ports")
	}
	if !strings.Contains(err.Error(), "accessory") {
		t.Errorf("error should mention the accessory situation, got: %v", err)
	}
}

// TestLoadCompose_PreservesWebContainerPort: the import contract from the
// product evaluation (C05) — a supported one-image short-port Compose file
// must import with its declared INTERNAL web port. "8080:3000" means
// container 3000 bound to host 8080 in Compose; the container port is the
// application port, and the host binding is Compose host plumbing teploy
// does not preserve. This used to import with Port=0 (deployed as :80).
func TestLoadCompose_PreservesWebContainerPort(t *testing.T) {
	compose := `
services:
  web:
    image: example/web:v1
    ports: ["8080:3000"]
`
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "compose.yml"), []byte(compose), 0644)

	cfg, err := LoadCompose(dir)
	if err != nil {
		t.Fatalf("supported short-port Compose fixture must import: %v", err)
	}
	if cfg == nil {
		t.Fatal("expected non-nil config")
	}
	if cfg.Port != 3000 {
		t.Errorf("web container port = %d, want 3000 (the host binding 8080 is not the application port)", cfg.Port)
	}
	if cfg.Image != "example/web:v1" {
		t.Errorf("web image = %q, want example/web:v1", cfg.Image)
	}
}

// TestLoadCompose_PortShortForms covers the supported short-form port
// grammar: host:container, bare container port (quoted and unquoted),
// IPv4- and bracketed-IPv6-prefixed bindings, and the same container port
// published through several bindings — one application port, not two.
func TestLoadCompose_PortShortForms(t *testing.T) {
	tests := []struct {
		name  string
		ports string
		want  int
	}{
		{"host:container", `["8080:3000"]`, 3000},
		{"bare container port", `["3000"]`, 3000},
		{"bare unquoted number", "[3000]", 3000},
		{"ipv4-prefixed", `["127.0.0.1:8080:3000"]`, 3000},
		{"ipv6-prefixed", `["[::1]:8080:3000"]`, 3000},
		{"same container port twice", `["8080:3000", "127.0.0.1:8081:3000"]`, 3000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			compose := "services:\n  web:\n    image: example/web:v1\n    ports: " + tt.ports + "\n"
			dir := t.TempDir()
			os.WriteFile(filepath.Join(dir, "compose.yml"), []byte(compose), 0644)

			cfg, err := LoadCompose(dir)
			if err != nil {
				t.Fatalf("supported short form must import: %v", err)
			}
			if cfg.Port != tt.want {
				t.Errorf("application port = %d, want %d", cfg.Port, tt.want)
			}
		})
	}
}

// TestLoadCompose_MultipleAppPortsRefused: two distinct container ports on
// the web service have no principled single application port — the import
// must refuse and name both rather than pick one.
func TestLoadCompose_MultipleAppPortsRefused(t *testing.T) {
	compose := `
services:
  web:
    image: myapp:latest
    ports: ["8080:3000", "8081:3001"]
`
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte(compose), 0644)

	_, err := LoadCompose(dir)
	if err == nil {
		t.Fatal("expected an ambiguity error for multiple container ports")
	}
	if !strings.Contains(err.Error(), "ambiguous") ||
		!strings.Contains(err.Error(), "3000") ||
		!strings.Contains(err.Error(), "3001") {
		t.Errorf("error must name the ambiguity and both ports, got: %v", err)
	}
}

// TestLoadCompose_NonTCPPorts: a non-TCP publish is not an application
// port (teploy serves HTTP over TCP) but is preserved verbatim as a
// publish; a service with ONLY non-TCP ports is refused with the reason.
func TestLoadCompose_NonTCPPorts(t *testing.T) {
	mixed := `
services:
  web:
    image: example/dns:v1
    ports: ["8080:3000", "53:53/udp"]
`
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "compose.yml"), []byte(mixed), 0644)

	cfg, err := LoadCompose(dir)
	if err != nil {
		t.Fatalf("mixed TCP + UDP service must import: %v", err)
	}
	if cfg.Port != 3000 {
		t.Errorf("application port = %d, want 3000 (the TCP port)", cfg.Port)
	}
	if len(cfg.Publish) != 1 || cfg.Publish[0] != "53:53/udp" {
		t.Errorf("UDP entry must be preserved verbatim in publish, got %v", cfg.Publish)
	}

	udpOnly := `
services:
  web:
    image: example/dns:v1
    ports: ["53:53/udp"]
`
	dir = t.TempDir()
	os.WriteFile(filepath.Join(dir, "compose.yml"), []byte(udpOnly), 0644)

	_, err = LoadCompose(dir)
	if err == nil {
		t.Fatal("expected an error when only a UDP port is published")
	}
	if !strings.Contains(err.Error(), "TCP") {
		t.Errorf("error must explain the TCP requirement, got: %v", err)
	}
}

// TestLoadCompose_PortRangeRefused: port ranges are outside the supported
// grammar and must be refused at import with the reason, before any
// deploy effect.
func TestLoadCompose_PortRangeRefused(t *testing.T) {
	compose := `
services:
  web:
    image: myapp:latest
    ports: ["3000-3005:3000-3005"]
`
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte(compose), 0644)

	_, err := LoadCompose(dir)
	if err == nil {
		t.Fatal("expected an error for a port range")
	}
	if !strings.Contains(err.Error(), "web") || !strings.Contains(err.Error(), "3000-3005") {
		t.Errorf("error must name the service and the offending entry, got: %v", err)
	}
}

// TestLoadCompose_LongFormPortsRefused: Compose long-form ports objects
// (target/published maps) are outside the supported grammar — refuse
// naming the service and the supported alternative.
func TestLoadCompose_LongFormPortsRefused(t *testing.T) {
	compose := `
services:
  web:
    image: myapp:latest
    ports:
      - target: 3000
        published: 8080
`
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte(compose), 0644)

	_, err := LoadCompose(dir)
	if err == nil {
		t.Fatal("expected an error for long-form ports")
	}
	if !strings.Contains(err.Error(), "web") || !strings.Contains(err.Error(), "ports") {
		t.Errorf("error must name the service and its ports, got: %v", err)
	}
}

// TestLoadCompose_RefusesIndependentBuild: a service built from a context
// different from the web service's cannot be preserved by the single-image
// process model — the import must refuse naming the service and its
// build, never silently flatten it into a worker of the app's image
// (which deployed the wrong code under the right command).
func TestLoadCompose_RefusesIndependentBuild(t *testing.T) {
	tests := []struct {
		name    string
		compose string
	}{
		{
			"build-based web",
			`
services:
  web:
    build: ./web
    ports: ["3000:3000"]
  jobs:
    build: ./jobs
    command: python jobs.py
`,
		},
		{
			"image-based web",
			`
services:
  web:
    image: myapp:latest
    ports: ["3000:3000"]
  jobs:
    build: ./jobs
    command: python jobs.py
`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			os.WriteFile(filepath.Join(dir, "compose.yml"), []byte(tt.compose), 0644)

			cfg, err := LoadCompose(dir)
			if err == nil {
				t.Fatalf("unsupported independent build must be refused, not flattened: config=%+v", cfg)
			}
			detail := strings.ToLower(err.Error())
			if !strings.Contains(detail, "jobs") || !strings.Contains(detail, "build") {
				t.Fatalf("refusal must identify the service and its build, got: %v", err)
			}
			if !strings.Contains(detail, "./jobs") {
				t.Fatalf("refusal must name the unsupported build context, got: %v", err)
			}
		})
	}
}

// Compose importer field classification — the declaration of the supported
// grammar (C05: every supplied field must be preserved, explicitly
// translated, or rejected; never silently dropped). Before this slice the
// importer used non-strict yaml.Unmarshal, so every field below was
// SILENTLY IGNORED: a file using healthcheck, networks, secrets, configs,
// profiles or deploy imported "successfully" while dropping those
// semantics.
//
//	Field            | Before          | Now
//	-----------------+-----------------+------------------------------------------
//	healthcheck      | silently ignored| TRANSLATE for the web service: exec-form
//	                 |                 | ["CMD","curl"|"wget",...,"http://localhost:
//	                 |                 | <app-port>/path"] maps the path to
//	                 |                 | health.path and interval to
//	                 |                 | health.interval_seconds. disable: true
//	                 |                 | and test: ["NONE"] map to
//	                 |                 | healthcheck.web.disable (--no-healthcheck).
//	                 |                 | Workers: disable/NONE translate to
//	                 |                 | healthcheck.<process>.disable; other tests
//	                 |                 | are rejected (teploy has no per-process
//	                 |                 | HTTP gate). Accessories: ignored — inert
//	                 |                 | under teploy, which supervises accessories
//	                 |                 | via --restart always + running-state checks
//	                 |                 | and never queries docker health. timeout/
//	                 |                 | retries/start_period are deliberately NOT
//	                 |                 | translated: compose timeout is per-probe,
//	                 |                 | teploy's health.timeout_seconds is the total
//	                 |                 | deploy-gate window (default 30s) — setting
//	                 |                 | it from a per-probe value would break
//	                 |                 | slow-starting apps; retries/start_period are
//	                 |                 | subsumed by that total window.
//	networks         | silently ignored| TOLERATE exactly the no-op equivalent:
//	                 |                 | ["default"] or {default: {}} — the implicit
//	                 |                 | default network compose attaches anyway.
//	                 |                 | Anything else REJECTED (teploy runs every
//	                 |                 | container on its own managed network).
//	restart          | silently ignored| TOLERATE "always"/"unless-stopped" (teploy
//	                 |                 | runs app containers --restart unless-stopped,
//	                 |                 | accessories --restart always; the delta for
//	                 |                 | "always" is only after a manual stop + daemon
//	                 |                 | restart, which teploy's lifecycle owns).
//	                 |                 | Everything else ("no", "on-failure", ...)
//	                 |                 | REJECTED — those change crash semantics.
//	env_file         | silently ignored| REJECTED: an opaque file reference with
//	                 |                 | compose-specific interpolation rules the
//	                 |                 | importer cannot resolve; teploy's env_files
//	                 |                 | is a deliberate teploy.yml opt-in. Empty
//	                 |                 | values tolerated.
//	secrets          | silently ignored| REJECTED (no secret-file model in the
//	                 |                 | import; empty list tolerated).
//	configs          | silently ignored| REJECTED (no config-file model in the
//	                 |                 | import; empty list tolerated).
//	profiles         | silently ignored| Services under non-default profiles are
//	                 |                 | SKIPPED entirely, deliberately: `docker
//	                 |                 | compose up` without --profile does not
//	                 |                 | deploy them, so importing them would deploy
//	                 |                 | something compose itself would not.
//	extends          | silently ignored| REJECTED (inheritance cannot be resolved
//	                 |                 | losslessly).
//	deploy           | silently ignored| Only no-op defaults tolerated ({}, or a
//	                 |                 | block containing just replicas: 1 and/or
//	                 |                 | mode: replicated — compose defaults).
//	                 |                 | Everything else (resources, replicas != 1,
//	                 |                 | mode: global, ...) REJECTED.
//	labels           | silently ignored| IGNORED — container metadata with no deploy
//	                 |                 | semantics; teploy manages its own teploy.*
//	                 |                 | labels for lifecycle.
//	depends_on       | parsed, unused  | TOLERATED deliberately. Compose semantics
//	                 |                 | are startup ordering; teploy ensures every
//	                 |                 | accessory is RUNNING before any app
//	                 |                 | container starts (cli/deploy.go "Ensure
//	                 |                 | accessories are running" step 9,
//	                 |                 | cli/singledeploy.go — sorted, before the
//	                 |                 | app containers), which honors the common
//	                 |                 | app-after-db ordering by construction. The
//	                 |                 | delta: condition: service_healthy /
//	                 |                 | service_completed_successfully readiness
//	                 |                 | gates are NOT waited for — the app must
//	                 |                 | tolerate an unreachable dependency at boot
//	                 |                 | (teploy's deploy health gate still gates
//	                 |                 | traffic).
//	container_name   | silently ignored| REJECTED — teploy owns container naming
//	                 |                 | ({app}-{process}-{version}) for lifecycle.
//	hostname         | silently ignored| REJECTED — identity with no model home;
//	                 |                 | software deriving identity from hostname
//	                 |                 | would silently change behavior.
//	working_dir      | silently ignored| REJECTED — no model home (set WORKDIR in
//	                 |                 | the image).
//	entrypoint       | silently ignored| REJECTED — no model home (bake into the
//	                 |                 | image's ENTRYPOINT).
//	privileged       | silently ignored| false (explicit default) tolerated; true
//	                 |                 | REJECTED — security-relevant, teploy runs
//	                 |                 | unprivileged containers.
//	cap_add          | silently ignored| Empty list tolerated; non-empty REJECTED —
//	                 |                 | security-relevant capabilities.
//
// Fields outside this table (the rest of the Compose spec) are still
// silently ignored — full Compose breadth remains open under C05.
func TestLoadCompose_FieldClassificationInventory(t *testing.T) {
	tests := []struct {
		name    string
		compose string
		wantErr []string // substrings the error must contain; empty = must import
		check   func(t *testing.T, cfg *AppConfig)
	}{
		{
			name: "healthcheck web translates",
			compose: `
services:
  web:
    image: example/web:v1
    ports: ["3000:3000"]
    healthcheck:
      test: ["CMD", "curl", "-f", "http://localhost:3000/healthz"]
      interval: 10s
`,
			check: func(t *testing.T, cfg *AppConfig) {
				if cfg.Health.Path != "/healthz" {
					t.Errorf("health.path = %q, want /healthz", cfg.Health.Path)
				}
				if cfg.Health.IntervalSeconds != 10 {
					t.Errorf("health.interval_seconds = %d, want 10", cfg.Health.IntervalSeconds)
				}
			},
		},
		{
			name: "healthcheck web disable translates",
			compose: `
services:
  web:
    image: example/web:v1
    ports: ["3000:3000"]
    healthcheck:
      disable: true
`,
			check: func(t *testing.T, cfg *AppConfig) {
				if !cfg.Healthcheck["web"].Disable {
					t.Errorf("healthcheck.web.disable = false, want true")
				}
			},
		},
		{
			name: "networks non-default rejected",
			compose: `
services:
  web:
    image: example/web:v1
    ports: ["3000:3000"]
    networks: [frontend]
`,
			wantErr: []string{"web", "networks"},
		},
		{
			name: "restart always tolerated",
			compose: `
services:
  web:
    image: example/web:v1
    ports: ["3000:3000"]
    restart: always
`,
		},
		{
			name: "restart no rejected",
			compose: `
services:
  web:
    image: example/web:v1
    ports: ["3000:3000"]
    restart: "no"
`,
			wantErr: []string{"web", "restart"},
		},
		{
			name: "env_file rejected",
			compose: `
services:
  web:
    image: example/web:v1
    ports: ["3000:3000"]
    env_file: .env
`,
			wantErr: []string{"web", "env_file"},
		},
		{
			name: "secrets rejected",
			compose: `
services:
  web:
    image: example/web:v1
    ports: ["3000:3000"]
    secrets: [db_password]
`,
			wantErr: []string{"web", "secrets"},
		},
		{
			name: "configs rejected",
			compose: `
services:
  web:
    image: example/web:v1
    ports: ["3000:3000"]
    configs: [app_config]
`,
			wantErr: []string{"web", "configs"},
		},
		{
			name: "profiles skipped deliberately",
			compose: `
services:
  web:
    image: example/web:v1
    ports: ["3000:3000"]
  migrate:
    image: migrate/migrate:v4
    profiles: [tools]
`,
			check: func(t *testing.T, cfg *AppConfig) {
				if _, ok := cfg.Accessories["migrate"]; ok {
					t.Errorf("profiled service must be skipped, got accessory %v", cfg.Accessories)
				}
			},
		},
		{
			name: "extends rejected",
			compose: `
services:
  web:
    image: example/web:v1
    ports: ["3000:3000"]
    extends:
      service: base
`,
			wantErr: []string{"web", "extends"},
		},
		{
			name: "deploy resources rejected",
			compose: `
services:
  web:
    image: example/web:v1
    ports: ["3000:3000"]
    deploy:
      resources:
        limits:
          memory: 512M
`,
			wantErr: []string{"web", "deploy"},
		},
		{
			name: "deploy replicas rejected",
			compose: `
services:
  web:
    image: example/web:v1
    ports: ["3000:3000"]
    deploy:
      replicas: 3
`,
			wantErr: []string{"web", "deploy"},
		},
		{
			name: "labels ignored",
			compose: `
services:
  web:
    image: example/web:v1
    ports: ["3000:3000"]
    labels:
      com.example.team: platform
`,
		},
		{
			name: "depends_on tolerated",
			compose: `
services:
  web:
    image: example/web:v1
    ports: ["3000:3000"]
    depends_on: [db]
  db:
    image: postgres:16
    environment:
      POSTGRES_PASSWORD: pass
`,
			check: func(t *testing.T, cfg *AppConfig) {
				if _, ok := cfg.Accessories["db"]; !ok {
					t.Fatal("expected db accessory under tolerated depends_on")
				}
			},
		},
		{
			name: "container_name rejected",
			compose: `
services:
  web:
    image: example/web:v1
    ports: ["3000:3000"]
    container_name: my-app
`,
			wantErr: []string{"web", "container_name"},
		},
		{
			name: "hostname rejected",
			compose: `
services:
  web:
    image: example/web:v1
    ports: ["3000:3000"]
    hostname: app-1
`,
			wantErr: []string{"web", "hostname"},
		},
		{
			name: "working_dir rejected",
			compose: `
services:
  web:
    image: example/web:v1
    ports: ["3000:3000"]
    working_dir: /srv/app
`,
			wantErr: []string{"web", "working_dir"},
		},
		{
			name: "entrypoint rejected",
			compose: `
services:
  web:
    image: example/web:v1
    ports: ["3000:3000"]
    entrypoint: ["/bin/sh", "-c"]
`,
			wantErr: []string{"web", "entrypoint"},
		},
		{
			name: "privileged rejected",
			compose: `
services:
  web:
    image: example/web:v1
    ports: ["3000:3000"]
    privileged: true
`,
			wantErr: []string{"web", "privileged"},
		},
		{
			name: "cap_add rejected",
			compose: `
services:
  web:
    image: example/web:v1
    ports: ["3000:3000"]
    cap_add: [NET_ADMIN]
`,
			wantErr: []string{"web", "cap_add"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte(tt.compose), 0644)

			cfg, err := LoadCompose(dir)
			if len(tt.wantErr) > 0 {
				if err == nil {
					t.Fatalf("expected refusal, imported: %+v", cfg)
				}
				for _, sub := range tt.wantErr {
					if !strings.Contains(err.Error(), sub) {
						t.Errorf("error must contain %q, got: %v", sub, err)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("expected import, got: %v", err)
			}
			if cfg == nil {
				t.Fatal("expected non-nil config")
			}
			if tt.check != nil {
				tt.check(t, cfg)
			}
		})
	}
}

// TestLoadCompose_TranslatesHealthcheck: the web service's healthcheck has
// real homes in AppConfig — health.path/interval_seconds for an HTTP probe,
// healthcheck.<process>.disable for the disabling forms. The translation is
// narrow on purpose (repo precedent: ParsePublishSpec) — anything the
// model cannot represent faithfully is refused naming the service.
func TestLoadCompose_TranslatesHealthcheck(t *testing.T) {
	tests := []struct {
		name    string
		compose string
		wantErr []string
		check   func(t *testing.T, cfg *AppConfig)
	}{
		{
			name: "exec curl with interval, timeout deliberately not translated",
			compose: `
services:
  web:
    image: example/web:v1
    ports: ["8080:3000"]
    healthcheck:
      test: ["CMD", "curl", "-fsSL", "http://localhost:3000/healthz"]
      interval: 10s
      timeout: 5s
      retries: 3
      start_period: 20s
`,
			check: func(t *testing.T, cfg *AppConfig) {
				if cfg.Health.Path != "/healthz" {
					t.Errorf("health.path = %q, want /healthz", cfg.Health.Path)
				}
				if cfg.Health.IntervalSeconds != 10 {
					t.Errorf("health.interval_seconds = %d, want 10", cfg.Health.IntervalSeconds)
				}
				// compose timeout is per-probe; teploy's timeout_seconds is
				// the TOTAL deploy-gate window — translating 5s would cap
				// the whole gate at 5s and break slow starters.
				if cfg.Health.TimeoutSeconds != 0 {
					t.Errorf("health.timeout_seconds = %d, want 0 (not translated from compose per-probe timeout)", cfg.Health.TimeoutSeconds)
				}
			},
		},
		{
			name: "exec wget spider",
			compose: `
services:
  web:
    image: example/web:v1
    ports: ["3000:3000"]
    healthcheck:
      test: ["CMD", "wget", "-q", "--spider", "http://localhost:3000/health"]
`,
			check: func(t *testing.T, cfg *AppConfig) {
				if cfg.Health.Path != "/health" {
					t.Errorf("health.path = %q, want /health", cfg.Health.Path)
				}
			},
		},
		{
			name: "interval as bare seconds number",
			compose: `
services:
  web:
    image: example/web:v1
    ports: ["3000:3000"]
    healthcheck:
      test: ["CMD", "curl", "-f", "http://localhost:3000/health"]
      interval: 10
`,
			check: func(t *testing.T, cfg *AppConfig) {
				if cfg.Health.IntervalSeconds != 10 {
					t.Errorf("health.interval_seconds = %d, want 10", cfg.Health.IntervalSeconds)
				}
			},
		},
		{
			name: "url without path maps to root",
			compose: `
services:
  web:
    image: example/web:v1
    ports: ["3000:3000"]
    healthcheck:
      test: ["CMD", "curl", "-f", "http://localhost:3000"]
`,
			check: func(t *testing.T, cfg *AppConfig) {
				if cfg.Health.Path != "/" {
					t.Errorf("health.path = %q, want /", cfg.Health.Path)
				}
			},
		},
		{
			name: "test NONE disables",
			compose: `
services:
  web:
    image: example/web:v1
    ports: ["3000:3000"]
    healthcheck:
      test: ["NONE"]
`,
			check: func(t *testing.T, cfg *AppConfig) {
				if !cfg.Healthcheck["web"].Disable {
					t.Errorf("healthcheck.web.disable = false, want true")
				}
				if cfg.Health.Path != "" {
					t.Errorf("health.path = %q, want empty under disabled healthcheck", cfg.Health.Path)
				}
			},
		},
		{
			name: "worker disable translates to per-process no-healthcheck",
			compose: `
services:
  web:
    build: .
    ports: ["3000:3000"]
  worker:
    build: .
    command: npm run worker
    healthcheck:
      test: ["NONE"]
`,
			check: func(t *testing.T, cfg *AppConfig) {
				if !cfg.Healthcheck["worker"].Disable {
					t.Errorf("healthcheck.worker.disable = false, want true")
				}
			},
		},
		{
			name: "accessory healthcheck inert",
			compose: `
services:
  web:
    image: example/web:v1
    ports: ["3000:3000"]
  db:
    image: postgres:16
    environment:
      POSTGRES_PASSWORD: pass
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U postgres"]
      interval: 10s
`,
			check: func(t *testing.T, cfg *AppConfig) {
				if cfg.Health.Path != "" || len(cfg.Healthcheck) != 0 {
					t.Errorf("accessory healthcheck must be inert, got health=%+v healthcheck=%v", cfg.Health, cfg.Healthcheck)
				}
			},
		},
		{
			name: "cmd-shell form refused",
			compose: `
services:
  web:
    image: example/web:v1
    ports: ["3000:3000"]
    healthcheck:
      test: ["CMD-SHELL", "curl -f http://localhost:3000/health || exit 1"]
`,
			wantErr: []string{"web", "healthcheck"},
		},
		{
			name: "string test form refused",
			compose: `
services:
  web:
    image: example/web:v1
    ports: ["3000:3000"]
    healthcheck:
      test: curl -f http://localhost:3000/health
`,
			wantErr: []string{"web", "healthcheck"},
		},
		{
			name: "non-http probe refused",
			compose: `
services:
  web:
    image: example/web:v1
    ports: ["3000:3000"]
    healthcheck:
      test: ["CMD", "pg_isready", "-U", "postgres"]
`,
			wantErr: []string{"web", "healthcheck"},
		},
		{
			name: "wrong port refused",
			compose: `
services:
  web:
    image: example/web:v1
    ports: ["8080:3000"]
    healthcheck:
      test: ["CMD", "curl", "-f", "http://localhost:8080/health"]
`,
			wantErr: []string{"web", "healthcheck", "3000"},
		},
		{
			name: "https refused",
			compose: `
services:
  web:
    image: example/web:v1
    ports: ["3000:3000"]
    healthcheck:
      test: ["CMD", "curl", "-f", "https://localhost:3000/health"]
`,
			wantErr: []string{"web", "healthcheck"},
		},
		{
			name: "external host refused",
			compose: `
services:
  web:
    image: example/web:v1
    ports: ["3000:3000"]
    healthcheck:
      test: ["CMD", "curl", "-f", "http://example.com/health"]
`,
			wantErr: []string{"web", "healthcheck"},
		},
		{
			name: "two urls refused",
			compose: `
services:
  web:
    image: example/web:v1
    ports: ["3000:3000"]
    healthcheck:
      test: ["CMD", "curl", "-f", "http://localhost:3000/a", "http://localhost:3000/b"]
`,
			wantErr: []string{"web", "healthcheck"},
		},
		{
			name: "query string refused",
			compose: `
services:
  web:
    image: example/web:v1
    ports: ["3000:3000"]
    healthcheck:
      test: ["CMD", "curl", "-f", "http://localhost:3000/health?ready"]
`,
			wantErr: []string{"web", "healthcheck"},
		},
		{
			name: "sub-second interval refused",
			compose: `
services:
  web:
    image: example/web:v1
    ports: ["3000:3000"]
    healthcheck:
      test: ["CMD", "curl", "-f", "http://localhost:3000/health"]
      interval: 500ms
`,
			wantErr: []string{"web", "interval"},
		},
		{
			name: "worker http test refused (no per-process gate)",
			compose: `
services:
  web:
    build: .
    ports: ["3000:3000"]
  worker:
    build: .
    command: npm run worker
    healthcheck:
      test: ["CMD", "curl", "-f", "http://localhost:3000/health"]
`,
			wantErr: []string{"worker", "healthcheck"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte(tt.compose), 0644)

			cfg, err := LoadCompose(dir)
			if len(tt.wantErr) > 0 {
				if err == nil {
					t.Fatalf("expected refusal, imported: %+v", cfg)
				}
				for _, sub := range tt.wantErr {
					if !strings.Contains(err.Error(), sub) {
						t.Errorf("error must contain %q, got: %v", sub, err)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("expected import, got: %v", err)
			}
			if tt.check != nil {
				tt.check(t, cfg)
			}
		})
	}
}

// TestLoadCompose_RejectsSemanticFields: fields whose silent loss changes
// deployment semantics are refused BEFORE any effect, naming the service,
// the field, why teploy cannot preserve it, and the teploy.yml alternative.
// Accessory services get the same treatment — a network on postgres is
// lost exactly as silently as one on web.
func TestLoadCompose_RejectsSemanticFields(t *testing.T) {
	tests := []struct {
		name    string
		compose string
		wantErr []string
	}{
		{
			name: "networks on accessory",
			compose: `
services:
  web:
    image: example/web:v1
    ports: ["3000:3000"]
  db:
    image: postgres:16
    networks: [backend]
`,
			wantErr: []string{"db", "networks"},
		},
		{
			name: "networks map form with alias",
			compose: `
services:
  web:
    image: example/web:v1
    ports: ["3000:3000"]
    networks:
      default:
        aliases: [app-1]
`,
			wantErr: []string{"web", "networks"},
		},
		{
			name: "secrets on accessory",
			compose: `
services:
  web:
    image: example/web:v1
    ports: ["3000:3000"]
  db:
    image: postgres:16
    secrets: [db_cert]
`,
			wantErr: []string{"db", "secrets"},
		},
		{
			name: "restart on-failure",
			compose: `
services:
  web:
    image: example/web:v1
    ports: ["3000:3000"]
    restart: on-failure
`,
			wantErr: []string{"web", "restart", "on-failure"},
		},
		{
			name: "deploy mode global",
			compose: `
services:
  web:
    image: example/web:v1
    ports: ["3000:3000"]
    deploy:
      mode: global
`,
			wantErr: []string{"web", "deploy"},
		},
		{
			name: "privileged on accessory",
			compose: `
services:
  web:
    image: example/web:v1
    ports: ["3000:3000"]
  sidecar:
    image: busybox:1
    privileged: true
`,
			wantErr: []string{"sidecar", "privileged"},
		},
		{
			name: "entrypoint string form",
			compose: `
services:
  web:
    image: example/web:v1
    ports: ["3000:3000"]
    entrypoint: /docker-entrypoint.sh
`,
			wantErr: []string{"web", "entrypoint"},
		},
		{
			name: "env_file list form",
			compose: `
services:
  web:
    image: example/web:v1
    ports: ["3000:3000"]
    env_file:
      - .env.shared
      - .env.local
`,
			wantErr: []string{"web", "env_file"},
		},
		{
			name: "extends string form",
			compose: `
services:
  web:
    image: example/web:v1
    ports: ["3000:3000"]
    extends: base
`,
			wantErr: []string{"web", "extends"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte(tt.compose), 0644)

			cfg, err := LoadCompose(dir)
			if err == nil {
				t.Fatalf("expected refusal, imported: %+v", cfg)
			}
			if !strings.Contains(err.Error(), "teploy.yml") {
				t.Errorf("refusal must point at teploy.yml, got: %v", err)
			}
			for _, sub := range tt.wantErr {
				if !strings.Contains(err.Error(), sub) {
					t.Errorf("error must contain %q, got: %v", sub, err)
				}
			}
		})
	}
}

// TestLoadCompose_ToleratesNoOpEquivalents: values that are exact no-ops
// under Compose semantics import unchanged — this is deliberate
// tolerance of the DEFAULT case only, not acceptance of the field.
func TestLoadCompose_ToleratesNoOpEquivalents(t *testing.T) {
	compose := `
services:
  web:
    image: example/web:v1
    ports: ["3000:3000"]
    networks: [default]
    restart: unless-stopped
    privileged: false
    cap_add: []
    secrets: []
    configs: []
    env_file: []
    deploy:
      replicas: 1
      mode: replicated
  db:
    image: postgres:16
    environment:
      POSTGRES_PASSWORD: pass
    networks:
      default: {}
    restart: always
    deploy: {}
`
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte(compose), 0644)

	cfg, err := LoadCompose(dir)
	if err != nil {
		t.Fatalf("no-op equivalents must import, got: %v", err)
	}
	if cfg.Port != 3000 {
		t.Errorf("port = %d, want 3000", cfg.Port)
	}
	if _, ok := cfg.Accessories["db"]; !ok {
		t.Fatal("expected db accessory")
	}
}

// TestLoadCompose_IgnoresMetadataFields: labels and depends_on have no
// deployment semantics teploy loses — labels are container metadata, and
// depends_on's startup ordering is honored by construction (accessories
// are ensured running before any app container starts; see
// cli/deploy.go "Ensure accessories are running"). The readiness-condition
// delta is documented in the classification table above.
func TestLoadCompose_IgnoresMetadataFields(t *testing.T) {
	compose := `
services:
  web:
    build: .
    ports: ["3000:3000"]
    labels:
      - "com.example.owner=platform"
      - "com.example.service=web"
    depends_on:
      db:
        condition: service_healthy
      redis:
        condition: service_started
  db:
    image: postgres:16
    environment:
      POSTGRES_PASSWORD: pass
  redis:
    image: redis:7
`
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte(compose), 0644)

	cfg, err := LoadCompose(dir)
	if err != nil {
		t.Fatalf("metadata fields must import, got: %v", err)
	}
	if _, ok := cfg.Accessories["db"]; !ok {
		t.Fatal("expected db accessory")
	}
	if _, ok := cfg.Accessories["redis"]; !ok {
		t.Fatal("expected redis accessory")
	}
	if cfg.Processes["web"] != "" || len(cfg.Processes) != 0 {
		t.Errorf("processes = %v, want collapsed single empty-command web (nil map)", cfg.Processes)
	}
}
