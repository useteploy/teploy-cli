package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/ssh"
)

// doctorCheckOrder pins the stable check-name sequence automation codes
// against (adding checks is additive at the end; renaming/removing is a
// machine-interface bump).
var doctorCheckOrder = []string{
	"git", "config", "ssh", "docker", "disk", "registry", "caddy", "compatibility", "repair-debt",
}

// doctorHappyMock registers every remote read a fully healthy target
// answers, for the app config returned by doctorTestApp.
func doctorHappyMock() *ssh.MockExecutor {
	return ssh.NewMockExecutor("192.0.2.10",
		ssh.MockCommand{Match: "docker version", Output: "27.3.1"},
		ssh.MockCommand{Match: "df -B1 -P", Output: "Filesystem 1-blocks Used Available Capacity Mounted on\n/dev/vda1 100000000000 20000000000 80000000000 20% /"},
		ssh.MockCommand{Match: "docker manifest inspect", Output: `{"schemaVersion":2}`},
		ssh.MockCommand{Match: "docker exec caddy", Output: `{}`},
		ssh.MockCommand{Match: "'/deployments/.bin/teploy' version", Output: "teploy v0.1.37"},
	)
}

func doctorTestApp() *config.AppConfig {
	return &config.AppConfig{App: "blog", Server: "192.0.2.10", Image: "example/blog:v1"}
}

func doctorTestDeps(mock *ssh.MockExecutor) doctorDeps {
	return doctorDeps{
		localVersion: "v0.1.37",
		connect: func(ctx context.Context, host, user, key string) (ssh.Executor, error) {
			return mock, nil
		},
		gitVersion: func(ctx context.Context) (string, error) {
			return "git version 2.39.5", nil
		},
	}
}

func doctorFindCheck(t *testing.T, report doctorReport, name string) doctorCheck {
	t.Helper()
	for _, c := range report.Checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("check %q missing from report: %+v", name, report.Checks)
	return doctorCheck{}
}

func TestDoctorCommandRegistered(t *testing.T) {
	root := NewRootCmd("test")
	cmd, _, err := root.Find([]string{"doctor"})
	if err != nil {
		t.Fatalf("finding doctor: %v", err)
	}
	if cmd == root || cmd.Name() != "doctor" {
		t.Fatalf("doctor command not registered")
	}
}

func TestDoctorAllChecksPass(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	mock := doctorHappyMock()
	report, ex := doctorRun(context.Background(), doctorTestDeps(mock), &Flags{}, "", doctorTestApp(), nil)
	if ex == nil {
		t.Fatal("expected an executor back")
	}
	if report.Summary != (doctorSummary{OK: 9, Warn: 0, Fail: 0}) {
		t.Fatalf("summary = %+v, want 9 ok", report.Summary)
	}
	if len(report.Checks) != len(doctorCheckOrder) {
		t.Fatalf("got %d checks, want %d", len(report.Checks), len(doctorCheckOrder))
	}
	for i, name := range doctorCheckOrder {
		if report.Checks[i].Name != name {
			t.Fatalf("check[%d] = %q, want %q (stable order)", i, report.Checks[i].Name, name)
		}
		if report.Checks[i].Result != "ok" {
			t.Fatalf("check %s = %s (%s), want ok", name, report.Checks[i].Result, report.Checks[i].Detail)
		}
	}
	sshCheck := doctorFindCheck(t, report, "ssh")
	if !strings.Contains(sshCheck.Detail, "192.0.2.10") {
		t.Fatalf("ssh detail must name the target: %+v", sshCheck)
	}
	dockerCheck := doctorFindCheck(t, report, "docker")
	if !strings.Contains(dockerCheck.Detail, "27.3.1") {
		t.Fatalf("docker detail must carry the version: %+v", dockerCheck)
	}
	assertDoctorReadOnlyCalls(t, mock.Calls)
}

func TestDoctorJSONStableShape(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	report, _ := doctorRun(context.Background(), doctorTestDeps(doctorHappyMock()), &Flags{}, "", doctorTestApp(), nil)
	var out bytes.Buffer
	if err := writeDoctorReport(&out, report, true); err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("doctor --json is not valid JSON: %q: %v", out.String(), err)
	}
	if len(doc) != 3 {
		t.Fatalf("top-level keys = %v, want exactly machine_interface/checks/summary", doc)
	}
	for _, key := range []string{"machine_interface", "checks", "summary"} {
		if _, ok := doc[key]; !ok {
			t.Fatalf("doctor envelope missing %q: %s", key, out.String())
		}
	}
	if doc["machine_interface"] != float64(MachineInterface) {
		t.Fatalf("machine_interface = %v, want %d", doc["machine_interface"], MachineInterface)
	}
	checks, ok := doc["checks"].([]any)
	if !ok || len(checks) != 9 {
		t.Fatalf("checks = %#v, want 9 entries", doc["checks"])
	}
	for i, raw := range checks {
		check, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("check %d is not an object: %#v", i, raw)
		}
		if len(check) != 4 {
			t.Fatalf("check %d keys = %v, want exactly name/result/detail/remediation", i, check)
		}
		for _, key := range []string{"name", "result"} {
			if _, ok := check[key]; !ok {
				t.Fatalf("check %d missing %q: %v", i, key, check)
			}
		}
		switch check["result"] {
		case "ok", "warn", "fail":
		default:
			t.Fatalf("check %d result %v outside the ok/warn/fail enum", i, check["result"])
		}
	}
	summary, ok := doc["summary"].(map[string]any)
	if !ok || len(summary) != 3 {
		t.Fatalf("summary = %#v, want exactly ok/warn/fail", doc["summary"])
	}
	var okCount, warnCount, failCount int
	for _, raw := range checks {
		check := raw.(map[string]any)
		switch check["result"] {
		case "ok":
			okCount++
		case "warn":
			warnCount++
		case "fail":
			failCount++
		}
	}
	if summary["ok"] != float64(okCount) || summary["warn"] != float64(warnCount) || summary["fail"] != float64(failCount) {
		t.Fatalf("summary %v disagrees with checks (ok=%d warn=%d fail=%d)", summary, okCount, warnCount, failCount)
	}
}

func TestDoctorHumanTable(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	// One failing remote check (docker unreachable) exercises the
	// remediation row rendering.
	mock := ssh.NewMockExecutor("192.0.2.10",
		ssh.MockCommand{Match: "df -B1 -P", Output: "Filesystem 1-blocks Used Available Capacity Mounted on\n/dev/vda1 100000000000 20000000000 80000000000 20% /"},
		ssh.MockCommand{Match: "docker manifest inspect", Output: `{}`},
		ssh.MockCommand{Match: "docker exec caddy", Output: `{}`},
		ssh.MockCommand{Match: "'/deployments/.bin/teploy' version", Output: "teploy v0.1.37"},
	)
	report, _ := doctorRun(context.Background(), doctorTestDeps(mock), &Flags{}, "", doctorTestApp(), nil)
	var out bytes.Buffer
	if err := writeDoctorReport(&out, report, false); err != nil {
		t.Fatal(err)
	}
	rendered := out.String()
	if !strings.Contains(rendered, "docker") || !strings.Contains(rendered, "fail") {
		t.Fatalf("table missing the failing docker row:\n%s", rendered)
	}
	if !strings.Contains(rendered, "fix:") {
		t.Fatalf("table missing the remediation line:\n%s", rendered)
	}
	if !strings.Contains(rendered, "Summary: 8 ok, 0 warn, 1 fail") {
		t.Fatalf("table summary line wrong:\n%s", rendered)
	}
	assertDoctorReadOnlyCalls(t, mock.Calls)
}

func TestDoctorConfigCheck(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "teploy.yml"), []byte("app: blog\nserver: prod\ndomain: blog.example.com\n"), 0600); err != nil {
			t.Fatal(err)
		}
		cfg, err := config.LoadApp(dir)
		if err != nil {
			t.Fatalf("fixture config must load: %v", err)
		}
		check := doctorConfigCheck(dir, cfg, nil)
		if check.Result != "ok" {
			t.Fatalf("config check = %+v, want ok", check)
		}
		if !strings.Contains(check.Detail, "teploy.yml") {
			t.Fatalf("config detail must name the file: %+v", check)
		}
	})

	t.Run("grammar error surfaced", func(t *testing.T) {
		dir := t.TempDir()
		source := "app: blog\nserver: prod\ndomain: blog.example.com\ningress: bogus-mode\n"
		if err := os.WriteFile(filepath.Join(dir, "teploy.yml"), []byte(source), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := config.LoadApp(dir)
		if err == nil {
			t.Fatal("fixture config must fail to load")
		}
		check := doctorConfigCheck(dir, nil, err)
		if check.Result != "fail" {
			t.Fatalf("config check = %+v, want fail", check)
		}
		if !strings.Contains(check.Detail, "bogus-mode") {
			t.Fatalf("config detail must surface the grammar error verbatim: %+v", check)
		}
	})

	t.Run("compose grammar error surfaced", func(t *testing.T) {
		dir := t.TempDir()
		source := "services:\n  web:\n    image: example/web:v1\n    ports: ['3000:3000']\n    hostname: fixed\n"
		if err := os.WriteFile(filepath.Join(dir, "compose.yml"), []byte(source), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := config.LoadApp(dir)
		if err == nil {
			t.Fatal("fixture compose must fail to import")
		}
		check := doctorConfigCheck(dir, nil, err)
		if check.Result != "fail" {
			t.Fatalf("compose config check = %+v, want fail", check)
		}
		if !strings.Contains(check.Detail, "hostname") {
			t.Fatalf("compose detail must surface the refusal: %+v", check)
		}
	})

	t.Run("no config", func(t *testing.T) {
		check := doctorConfigCheck(t.TempDir(), nil, config.ErrNoConfig)
		if check.Result != "fail" {
			t.Fatalf("no-config check = %+v, want fail", check)
		}
		if !strings.Contains(check.Remediation, "teploy init") {
			t.Fatalf("no-config remediation must point at init: %+v", check)
		}
	})
}

func TestDoctorGitCheck(t *testing.T) {
	deps := doctorTestDeps(doctorHappyMock())
	deps.gitVersion = func(ctx context.Context) (string, error) {
		return "git version 2.39.5", nil
	}
	if check := doctorGitCheck(context.Background(), deps); check.Result != "ok" {
		t.Fatalf("git check = %+v, want ok", check)
	}
	deps.gitVersion = func(ctx context.Context) (string, error) {
		return "", errors.New("git not found on PATH")
	}
	check := doctorGitCheck(context.Background(), deps)
	if check.Result != "warn" {
		t.Fatalf("missing git = %+v, want warn", check)
	}
	if !strings.Contains(check.Remediation, "install git") {
		t.Fatalf("git remediation must say install: %+v", check)
	}
}

func TestDoctorSSHUnreachable(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	// The exact enriched message shape ssh.Connect produces for a
	// known_hosts algorithm-coverage mismatch (078f610) — doctor must
	// carry it verbatim so the operator sees the algorithm names.
	connectErr := errors.New("host key mismatch for 192.0.2.10: server presented ssh-rsa, known_hosts has no matching entry (has ssh-ed25519) — scan all algorithms (ssh-keyscan without -t), not just one: ssh: handshake failed: knownhosts: key mismatch")
	deps := doctorDeps{
		localVersion: "v0.1.37",
		connect: func(ctx context.Context, host, user, key string) (ssh.Executor, error) {
			return nil, connectErr
		},
		gitVersion: func(ctx context.Context) (string, error) { return "git version 2.39.5", nil },
	}
	report, ex := doctorRun(context.Background(), deps, &Flags{}, "", doctorTestApp(), nil)
	if ex != nil {
		t.Fatal("no executor expected on connect failure")
	}
	sshCheck := doctorFindCheck(t, report, "ssh")
	if sshCheck.Result != "fail" {
		t.Fatalf("ssh check = %+v, want fail", sshCheck)
	}
	for _, frag := range []string{"ssh-rsa", "ssh-ed25519", "ssh-keyscan"} {
		if !strings.Contains(sshCheck.Detail, frag) {
			t.Fatalf("ssh detail must carry the known_hosts diagnostics (%s): %+v", frag, sshCheck)
		}
	}
	// Every remote check is skipped-with-fail, naming SSH as the reason.
	for _, name := range []string{"docker", "disk", "registry", "caddy", "compatibility", "repair-debt"} {
		check := doctorFindCheck(t, report, name)
		if check.Result != "fail" || !strings.Contains(check.Detail, "SSH unreachable") {
			t.Fatalf("%s check = %+v, want fail/skipped (SSH unreachable)", name, check)
		}
	}
	if code := doctorExitCode(report); code != 1 {
		t.Fatalf("exit code = %d, want 1 with a failing check", code)
	}
}

func TestDoctorNoTargetConfigured(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	deps := doctorTestDeps(doctorHappyMock())
	deps.gitVersion = func(ctx context.Context) (string, error) { return "", errors.New("git not found on PATH") }
	report, ex := doctorRun(context.Background(), deps, &Flags{}, "", nil, config.ErrNoConfig)
	if ex != nil {
		t.Fatal("no executor expected without a target")
	}
	sshCheck := doctorFindCheck(t, report, "ssh")
	if sshCheck.Result != "fail" || !strings.Contains(sshCheck.Remediation, "--server") {
		t.Fatalf("ssh check = %+v, want fail naming --server", sshCheck)
	}
	if code := doctorExitCode(report); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
}

func TestDoctorDockerCheck(t *testing.T) {
	ctx := context.Background()
	mock := ssh.NewMockExecutor("h", ssh.MockCommand{Match: "docker version", Output: "27.3.1"})
	if check := doctorDockerCheck(ctx, mock); check.Result != "ok" || !strings.Contains(check.Detail, "27.3.1") {
		t.Fatalf("docker check = %+v, want ok with version", check)
	}
	failed := ssh.NewMockExecutor("h", ssh.MockCommand{Match: "docker version", Err: errors.New("exit status 1: Cannot connect to the Docker daemon")})
	check := doctorDockerCheck(ctx, failed)
	if check.Result != "fail" || !strings.Contains(check.Detail, "Cannot connect") {
		t.Fatalf("docker check = %+v, want fail with the error", check)
	}
	if !strings.Contains(check.Remediation, "Docker") {
		t.Fatalf("docker remediation must point at Docker: %+v", check)
	}
}

func TestDoctorDiskCheck(t *testing.T) {
	ctx := context.Background()
	const header = "Filesystem 1-blocks Used Available Capacity Mounted on\n"

	t.Run("healthy", func(t *testing.T) {
		mock := ssh.NewMockExecutor("h", ssh.MockCommand{Match: "df -B1 -P", Output: header + "/dev/vda1 100000000000 20000000000 80000000000 20% /"})
		check := doctorDiskCheck(ctx, mock)
		if check.Result != "ok" || !strings.Contains(check.Detail, "74.5 GiB") {
			t.Fatalf("disk check = %+v, want ok with 74.5 GiB available", check)
		}
	})
	t.Run("low headroom warns", func(t *testing.T) {
		mock := ssh.NewMockExecutor("h", ssh.MockCommand{Match: "df -B1 -P", Output: header + "/dev/vda1 100000000000 96000000000 4000000000 96% /"})
		check := doctorDiskCheck(ctx, mock)
		if check.Result != "warn" {
			t.Fatalf("disk check = %+v, want warn below 10 GiB", check)
		}
	})
	t.Run("critical fails", func(t *testing.T) {
		mock := ssh.NewMockExecutor("h", ssh.MockCommand{Match: "df -B1 -P", Output: header + "/dev/vda1 100000000000 99000000000 1000000000 99% /"})
		check := doctorDiskCheck(ctx, mock)
		if check.Result != "fail" {
			t.Fatalf("disk check = %+v, want fail below 2 GiB", check)
		}
	})
	t.Run("df fails", func(t *testing.T) {
		mock := ssh.NewMockExecutor("h", ssh.MockCommand{Match: "df -B1 -P", Err: errors.New("exit status 1")})
		check := doctorDiskCheck(ctx, mock)
		if check.Result != "fail" {
			t.Fatalf("disk check = %+v, want fail", check)
		}
	})
	t.Run("parser", func(t *testing.T) {
		raw := header + "/dev/vda1 100 20 80 20% /"
		avail, pct, ok := parseDoctorDisk(raw)
		if !ok || avail != 80 || pct != 20 {
			t.Fatalf("parseDoctorDisk = %d %d %v, want 80 20 true", avail, pct, ok)
		}
		if _, _, ok := parseDoctorDisk("garbage"); ok {
			t.Fatal("parseDoctorDisk accepted garbage")
		}
		// Mount points with spaces keep the numeric fields aligned.
		avail, pct, ok = parseDoctorDisk(header + "/dev/vda1 100 20 80 20% /mnt with space")
		if !ok || avail != 80 || pct != 20 {
			t.Fatalf("parseDoctorDisk with spaced mount = %d %d %v", avail, pct, ok)
		}
	})
}

func TestDoctorRegistryCheck(t *testing.T) {
	ctx := context.Background()

	t.Run("reachable", func(t *testing.T) {
		mock := ssh.NewMockExecutor("h", ssh.MockCommand{Match: "docker manifest inspect", Output: `{}`})
		check := doctorRegistryCheck(ctx, mock, doctorTestApp())
		if check.Result != "ok" {
			t.Fatalf("registry check = %+v, want ok", check)
		}
	})
	t.Run("auth class distinguished", func(t *testing.T) {
		mock := ssh.NewMockExecutor("h", ssh.MockCommand{Match: "docker manifest inspect", Err: errors.New("denied: requested access to the resource is denied")})
		check := doctorRegistryCheck(ctx, mock, doctorTestApp())
		if check.Result != "fail" {
			t.Fatalf("registry check = %+v, want fail", check)
		}
		if !strings.Contains(check.Remediation, "teploy registry login") {
			t.Fatalf("auth-class remediation must name registry login: %+v", check)
		}
	})
	t.Run("unreachable class", func(t *testing.T) {
		mock := ssh.NewMockExecutor("h", ssh.MockCommand{Match: "docker manifest inspect", Err: errors.New(`Get "https://registry.example/v2/": dial tcp: lookup registry.example: no such host`)})
		check := doctorRegistryCheck(ctx, mock, doctorTestApp())
		if check.Result != "fail" {
			t.Fatalf("registry check = %+v, want fail", check)
		}
		if strings.Contains(check.Remediation, "registry login") {
			t.Fatalf("unreachable class must not ask for a login: %+v", check)
		}
	})
	t.Run("missing image", func(t *testing.T) {
		mock := ssh.NewMockExecutor("h", ssh.MockCommand{Match: "docker manifest inspect", Err: errors.New("no such manifest for revision v1 in registry")})
		check := doctorRegistryCheck(ctx, mock, doctorTestApp())
		if check.Result != "fail" || !strings.Contains(check.Remediation, "push") {
			t.Fatalf("missing-image check = %+v, want fail with push remediation", check)
		}
	})
	t.Run("build app has no ref to check", func(t *testing.T) {
		mock := ssh.NewMockExecutor("h")
		cfg := doctorTestApp()
		cfg.Image = ""
		check := doctorRegistryCheck(ctx, mock, cfg)
		if check.Result != "ok" || !strings.Contains(check.Detail, "built from source") {
			t.Fatalf("build-app check = %+v, want ok skip", check)
		}
		if len(mock.Calls) != 0 {
			t.Fatalf("registry check must not touch the server for a build app: %v", mock.Calls)
		}
	})
	t.Run("classifier", func(t *testing.T) {
		cases := map[string]string{
			"denied: requested access to the resource is denied":                    "auth",
			"unauthorized: authentication required":                                 "auth",
			"no such manifest for revision v1":                                      "missing",
			"manifest unknown":                                                      "missing",
			`Get "https://r.example/v2/": dial tcp: lookup r.example: no such host`: "unreachable",
			"i/o timeout": "unreachable",
		}
		for msg, want := range cases {
			// Docker's CLI exits 1 for every class; the classifier reads
			// the structured stderr, which mocks carry via the failure
			// error text.
			res := ssh.Result{ExitCode: 1, Stderr: []byte(msg)}
			if got := classifyRegistryFailure(res); got != want {
				t.Fatalf("classifyRegistryFailure(%q) = %q, want %q", msg, got, want)
			}
		}
		// A transport failure (no stderr, error set) is unreachable-class.
		res := ssh.Result{ExitCode: -1, Err: errors.New("ssh: connection timed out")}
		if got := classifyRegistryFailure(res); got != "unreachable" {
			t.Fatalf("transport failure classified %q, want unreachable", got)
		}
	})
}

func TestDoctorCaddyCheck(t *testing.T) {
	ctx := context.Background()

	t.Run("caddy ingress reachable", func(t *testing.T) {
		mock := ssh.NewMockExecutor("h", ssh.MockCommand{Match: "docker exec caddy", Output: `{}`})
		check := doctorCaddyCheck(ctx, mock, doctorTestApp())
		if check.Result != "ok" {
			t.Fatalf("caddy check = %+v, want ok", check)
		}
	})
	t.Run("admin api unreachable", func(t *testing.T) {
		mock := ssh.NewMockExecutor("h", ssh.MockCommand{Match: "docker exec caddy", Err: errors.New("exit status 1: Error response from daemon: No such container: caddy")})
		check := doctorCaddyCheck(ctx, mock, doctorTestApp())
		if check.Result != "fail" || !strings.Contains(check.Detail, "No such container") {
			t.Fatalf("caddy check = %+v, want fail naming the error", check)
		}
	})
	t.Run("host ingress skips caddy", func(t *testing.T) {
		mock := ssh.NewMockExecutor("h")
		cfg := doctorTestApp()
		cfg.Ingress = config.IngressHost
		check := doctorCaddyCheck(ctx, mock, cfg)
		if check.Result != "ok" || !strings.Contains(check.Detail, "host") {
			t.Fatalf("host-ingress check = %+v, want ok skip", check)
		}
		if len(mock.Calls) != 0 {
			t.Fatalf("host ingress must not probe caddy: %v", mock.Calls)
		}
	})
	t.Run("external ingress skips caddy", func(t *testing.T) {
		mock := ssh.NewMockExecutor("h")
		cfg := doctorTestApp()
		cfg.Ingress = config.IngressExternal
		check := doctorCaddyCheck(ctx, mock, cfg)
		if check.Result != "ok" {
			t.Fatalf("external-ingress check = %+v, want ok skip", check)
		}
		if len(mock.Calls) != 0 {
			t.Fatalf("external ingress must not probe caddy: %v", mock.Calls)
		}
	})
}

func TestDoctorCompatibilityCheck(t *testing.T) {
	ctx := context.Background()
	deps := doctorTestDeps(nil)

	t.Run("versions agree", func(t *testing.T) {
		mock := ssh.NewMockExecutor("h", ssh.MockCommand{Match: "'/deployments/.bin/teploy' version", Output: "teploy v0.1.37"})
		if check := doctorCompatCheck(ctx, deps, mock); check.Result != "ok" {
			t.Fatalf("compat check = %+v, want ok", check)
		}
	})
	t.Run("version skew warns", func(t *testing.T) {
		mock := ssh.NewMockExecutor("h", ssh.MockCommand{Match: "'/deployments/.bin/teploy' version", Output: "teploy v0.1.30"})
		check := doctorCompatCheck(ctx, deps, mock)
		if check.Result != "warn" {
			t.Fatalf("compat check = %+v, want warn", check)
		}
		if !strings.Contains(check.Detail, "v0.1.37") || !strings.Contains(check.Detail, "v0.1.30") {
			t.Fatalf("compat detail must name both versions: %+v", check)
		}
		if !strings.Contains(check.Remediation, "autodeploy") {
			t.Fatalf("compat remediation must name the refresh path: %+v", check)
		}
	})
	t.Run("server binary absent is not a failure", func(t *testing.T) {
		mock := ssh.NewMockExecutor("h", ssh.MockCommand{Match: "'/deployments/.bin/teploy' version", Err: errors.New("exit status 127: /bin/sh: 1: /deployments/.bin/teploy: not found")})
		check := doctorCompatCheck(ctx, deps, mock)
		if check.Result != "ok" {
			t.Fatalf("absent server binary = %+v, want ok", check)
		}
	})
	t.Run("absence is judged by exit code, not failure text", func(t *testing.T) {
		// A non-127 failure whose TEXT contains "not found" must not be
		// read as absence — that is exactly the text-parse the
		// structured result replaces.
		mock := ssh.NewMockExecutor("h", ssh.MockCommand{Match: "'/deployments/.bin/teploy' version", Err: errors.New("exit status 1: ld.so: object not found")})
		check := doctorCompatCheck(ctx, deps, mock)
		if check.Result != "warn" {
			t.Fatalf("non-127 failure with not-found text = %+v, want warn", check)
		}
	})
	t.Run("unreadable server binary warns", func(t *testing.T) {
		mock := ssh.NewMockExecutor("h", ssh.MockCommand{Match: "'/deployments/.bin/teploy' version", Err: errors.New("exit status 1: permission denied")})
		if check := doctorCompatCheck(ctx, deps, mock); check.Result != "warn" {
			t.Fatalf("unreadable server binary = %+v, want warn", check)
		}
	})
}

func TestDoctorRepairDebtCheck(t *testing.T) {
	ctx := context.Background()

	t.Run("no debt", func(t *testing.T) {
		mock := ssh.NewMockExecutor("h")
		if check := doctorRepairDebtCheck(ctx, mock, doctorTestApp()); check.Result != "ok" {
			t.Fatalf("repair-debt check = %+v, want ok", check)
		}
	})
	t.Run("outstanding debt warns", func(t *testing.T) {
		mock := ssh.NewMockExecutor("h")
		mock.Files["/deployments/blog/repair-debt.json"] = []byte(`{"schema_version":1,"app":"blog","release":"abc123","reason":"write failed","attempts":2,"first_failed_at":"2026-09-23T10:00:00Z","last_failed_at":"2026-09-23T10:00:00Z"}`)
		check := doctorRepairDebtCheck(ctx, mock, doctorTestApp())
		if check.Result != "warn" {
			t.Fatalf("repair-debt check = %+v, want warn", check)
		}
		if !strings.Contains(check.Detail, "abc123") {
			t.Fatalf("repair-debt detail must name the release: %+v", check)
		}
	})
	t.Run("unreadable marker warns", func(t *testing.T) {
		mock := ssh.NewMockExecutor("h", ssh.MockCommand{Match: "if [ ! -e '/deployments/blog/repair-debt.json'", Err: errors.New("permission denied")})
		if check := doctorRepairDebtCheck(ctx, mock, doctorTestApp()); check.Result != "warn" {
			t.Fatalf("unreadable marker = %+v, want warn", check)
		}
	})
	t.Run("no app identity", func(t *testing.T) {
		mock := ssh.NewMockExecutor("h")
		if check := doctorRepairDebtCheck(ctx, mock, nil); check.Result != "ok" {
			t.Fatalf("no-app check = %+v, want ok skip", check)
		}
		if len(mock.Calls) != 0 {
			t.Fatalf("no-app must not touch the server: %v", mock.Calls)
		}
	})
}

// TestDoctorNoEffectsWhenFailing drives a run where every remote check
// fails and asserts the executor ONLY ever saw read-only commands —
// doctor's no-deployment-effects contract.
func TestDoctorNoEffectsWhenFailing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	mock := ssh.NewMockExecutor("192.0.2.10",
		ssh.MockCommand{Match: "docker version", Err: errors.New("exit status 1: daemon down")},
		ssh.MockCommand{Match: "df -B1 -P", Err: errors.New("exit status 1")},
		ssh.MockCommand{Match: "docker manifest inspect", Err: errors.New("unauthorized: authentication required")},
		ssh.MockCommand{Match: "docker exec caddy", Err: errors.New("exit status 1: no such container")},
		ssh.MockCommand{Match: "'/deployments/.bin/teploy' version", Err: errors.New("exit status 127: not found")},
		ssh.MockCommand{Match: "if [ ! -e '/deployments/blog/repair-debt.json'", Err: errors.New("permission denied")},
	)
	report, _ := doctorRun(context.Background(), doctorTestDeps(mock), &Flags{}, "", doctorTestApp(), nil)
	if report.Summary.Fail == 0 {
		t.Fatal("expected failing checks")
	}
	if code := doctorExitCode(report); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	assertDoctorReadOnlyCalls(t, mock.Calls)
	if len(mock.Files) != 0 {
		t.Fatalf("doctor must not leave uploaded files behind: %v", mock.Files)
	}
}

func TestDoctorExitCode(t *testing.T) {
	ok := doctorReport{Checks: []doctorCheck{{Result: "ok"}, {Result: "warn"}}}
	ok.summarize()
	if code := doctorExitCode(ok); code != 0 {
		t.Fatalf("warn-only exit = %d, want 0", code)
	}
	failing := doctorReport{Checks: []doctorCheck{{Result: "ok"}, {Result: "fail"}}}
	failing.summarize()
	if code := doctorExitCode(failing); code != 1 {
		t.Fatalf("any-fail exit = %d, want 1", code)
	}
}

// TestDoctorEndToEndAllOK drives the cobra wiring on an all-healthy world
// (runDoctor os.Exit(1)s on any fail, so failing worlds are exercised via
// doctorRun + doctorExitCode above — drift's testing posture).
func TestDoctorEndToEndAllOK(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "teploy.yml"), []byte("app: blog\nserver: 192.0.2.10\ndomain: blog.example.com\nimage: example/blog:v1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	mock := doctorHappyMock()

	var out bytes.Buffer
	if err := runDoctor(doctorTestDeps(mock), &Flags{}, "", &out); err != nil {
		t.Fatalf("runDoctor: %v", err)
	}
	if !strings.Contains(out.String(), "Summary: 9 ok, 0 warn, 0 fail") {
		t.Fatalf("human report wrong:\n%s", out.String())
	}
	assertDoctorReadOnlyCalls(t, mock.Calls)
}

// assertDoctorReadOnlyCalls fails the test when any executed command is
// outside doctor's read-only allowlist — the no-deployment-effects proof.
func assertDoctorReadOnlyCalls(t *testing.T, calls []string) {
	t.Helper()
	readOnly := []string{
		"docker version",
		"docker manifest inspect",
		"docker exec caddy",
		"df ",
		"'/deployments/.bin/teploy' version",
		"if [ ! -e '/deployments/",
	}
	for _, call := range calls {
		allowed := false
		for _, prefix := range readOnly {
			if strings.HasPrefix(call, prefix) {
				allowed = true
				break
			}
		}
		if !allowed {
			t.Fatalf("doctor issued a non-read-only command: %q", call)
		}
	}
}
