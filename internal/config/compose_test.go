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
