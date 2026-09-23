package deploy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/useteploy/teploy/internal/releasemeta"
	"github.com/useteploy/teploy/internal/ssh"
)

// --- probe dispatch ---

// http mode is status-based ONLY: a 404 must not fall back to the TCP dial
// (that fallback is auto's documented compat behavior).
func TestHealthCheck_HTTPModeHasNoTCPFallback(t *testing.T) {
	mock := ssh.NewMockExecutor("h",
		ssh.MockCommand{Match: "curl", Output: "404"},
		ssh.MockCommand{Match: "bash -c", Output: ""},
	)
	d := &Deployer{exec: mock, out: nopWriter{}}

	cfg := HealthConfig{Mode: "http", Timeout: 300 * time.Millisecond, Interval: 10 * time.Millisecond}
	if err := d.healthCheck(context.Background(), 3456, cfg, ""); err == nil {
		t.Fatal("http mode must fail on 404 (no TCP fallback)")
	}
	for _, c := range mock.Calls {
		if strings.HasPrefix(c, "bash -c") {
			t.Errorf("http mode must never dial: %s", c)
		}
	}
}

func TestHealthCheck_HTTPModePassesOn200(t *testing.T) {
	mock := ssh.NewMockExecutor("h", ssh.MockCommand{Match: "curl", Output: "200"})
	d := &Deployer{exec: mock, out: nopWriter{}}

	cfg := HealthConfig{Mode: "http", Timeout: 2 * time.Second, Interval: 10 * time.Millisecond}
	if err := d.healthCheck(context.Background(), 3456, cfg, ""); err != nil {
		t.Fatalf("healthCheck: %v", err)
	}
}

// tcp mode never issues the HTTP probe — the dial is the whole gate.
func TestHealthCheck_TCPModeDialsWithoutCurl(t *testing.T) {
	mock := ssh.NewMockExecutor("h", ssh.MockCommand{Match: "bash -c", Output: ""})
	d := &Deployer{exec: mock, out: nopWriter{}}

	cfg := HealthConfig{Mode: "tcp", Timeout: 2 * time.Second, Interval: 10 * time.Millisecond}
	if err := d.healthCheck(context.Background(), 3456, cfg, ""); err != nil {
		t.Fatalf("healthCheck tcp: %v", err)
	}
	for _, c := range mock.Calls {
		if strings.HasPrefix(c, "curl") {
			t.Errorf("tcp mode must never run the HTTP probe: %s", c)
		}
	}
}

func TestHealthCheck_TCPModeFailsWhenDialFails(t *testing.T) {
	mock := ssh.NewMockExecutor("h",
		ssh.MockCommand{Match: "bash -c", Err: fmt.Errorf("connection refused")},
		ssh.MockCommand{Match: "curl", Output: "200"},
	)
	d := &Deployer{exec: mock, out: nopWriter{}}

	cfg := HealthConfig{Mode: "tcp", Timeout: 300 * time.Millisecond, Interval: 10 * time.Millisecond}
	if err := d.healthCheck(context.Background(), 3456, cfg, ""); err == nil {
		t.Fatal("tcp mode must fail when the dial fails")
	}
	for _, c := range mock.Calls {
		if strings.HasPrefix(c, "curl") {
			t.Errorf("tcp mode must never run the HTTP probe: %s", c)
		}
	}
}

// Explicit auto behaves exactly like the historical default: HTTP first,
// 404/3xx falls back to the TCP dial.
func TestHealthCheck_AutoModeExplicitFallsBack(t *testing.T) {
	mock := ssh.NewMockExecutor("h",
		ssh.MockCommand{Match: "curl", Output: "301"},
		ssh.MockCommand{Match: "bash -c", Output: ""},
	)
	d := &Deployer{exec: mock, out: nopWriter{}}

	cfg := HealthConfig{Mode: "auto", Timeout: 2 * time.Second, Interval: 10 * time.Millisecond}
	if err := d.healthCheck(context.Background(), 3456, cfg, ""); err != nil {
		t.Fatalf("healthCheck auto: %v", err)
	}
	var sawDial bool
	for _, c := range mock.Calls {
		if strings.HasPrefix(c, "bash -c") {
			sawDial = true
		}
	}
	if !sawDial {
		t.Error("auto mode must fall back to the TCP dial on 3xx")
	}
}

// --- surfaced readiness line ---

func TestReadinessSummary(t *testing.T) {
	cases := []struct {
		name string
		cfg  HealthConfig
		port int
		want string
	}{
		{"http", HealthConfig{Mode: "http", Path: "/healthz", Timeout: 30 * time.Second, Interval: time.Second}, 3000, "HTTP GET /healthz (30s deadline)"},
		{"tcp", HealthConfig{Mode: "tcp", Timeout: 30 * time.Second}, 3000, "TCP :3000 (30s)"},
		{"auto-empty", HealthConfig{Timeout: 30 * time.Second}, 3000, "auto — HTTP then TCP fallback (compat, 30s deadline)"},
		{"auto-explicit", HealthConfig{Mode: "auto", Timeout: 45 * time.Second}, 3000, "auto — HTTP then TCP fallback (compat, 45s deadline)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg.withDefaults()
			cfg.Timeout = tc.cfg.Timeout
			if tc.cfg.Path != "" {
				cfg.Path = tc.cfg.Path
			}
			if got := readinessSummary(cfg, tc.port); got != tc.want {
				t.Errorf("readinessSummary = %q, want %q", got, tc.want)
			}
		})
	}
}

// --- total deadline ---

// blockingExecutor models a probe endpoint that never answers: every
// command hangs until its context dies. The gate must still fail within
// the configured timeout — the timeout is a TOTAL deadline, not a
// per-attempt bound on unbounded retries.
type blockingExecutor struct {
	mu    sync.Mutex
	calls []string
}

func (b *blockingExecutor) Run(ctx context.Context, cmd string) (string, error) {
	b.mu.Lock()
	b.calls = append(b.calls, cmd)
	b.mu.Unlock()
	<-ctx.Done()
	return "", ctx.Err()
}

func (b *blockingExecutor) RunStream(ctx context.Context, cmd string, stdout, stderr io.Writer) error {
	_, err := b.Run(ctx, cmd)
	return err
}

func (b *blockingExecutor) RunInput(ctx context.Context, cmd string, stdin io.Reader) error {
	return nil
}

func (b *blockingExecutor) Upload(ctx context.Context, content io.Reader, remotePath string, mode string) error {
	return nil
}

func (b *blockingExecutor) Close() error { return nil }
func (b *blockingExecutor) Host() string { return "h" }
func (b *blockingExecutor) User() string { return "root" }
func (b *blockingExecutor) Calls() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.calls...)
}

func TestHealthCheck_NeverRespondingProbeFailsWithinDeadline(t *testing.T) {
	exec := &blockingExecutor{}
	d := &Deployer{exec: exec, out: nopWriter{}}

	const timeout = 400 * time.Millisecond
	cfg := HealthConfig{Mode: "http", Timeout: timeout, Interval: 10 * time.Millisecond}
	start := time.Now()
	err := d.healthCheck(context.Background(), 3456, cfg, "")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("a never-responding probe must fail")
	}
	if elapsed > timeout+2*time.Second {
		t.Errorf("gate must fail within deadline+slack, took %s (deadline %s)", elapsed, timeout)
	}
	if len(exec.Calls()) == 0 {
		t.Error("probe never attempted")
	}
}

// --- shared execution-plan validator ---

func TestConfigValidate_HealthModeEnum(t *testing.T) {
	base := Config{App: "myapp", Domain: "myapp.com", Image: "myapp:latest", Version: "abc123"}
	for _, mode := range []string{"", "http", "tcp", "auto"} {
		c := base
		c.Health = HealthConfig{Mode: mode}
		if err := c.validate(); err != nil {
			t.Errorf("mode %q: unexpected validate error: %v", mode, err)
		}
	}
	c := base
	c.Health = HealthConfig{Mode: "grpc"}
	err := c.validate()
	if err == nil {
		t.Fatal("expected validate error for unknown health mode")
	}
	if !strings.Contains(err.Error(), "health mode") {
		t.Errorf("error should name the health mode, got: %v", err)
	}
}

// --- deploy-path wiring ---

// firstDeployMock is the TestDeploy_FirstDeploy script, parameterized on the
// probe commands the gate is expected to run.
func firstDeployMock(healthCurl, dial *ssh.MockCommand) *ssh.MockExecutor {
	cmds := []ssh.MockCommand{
		{Match: "mkdir -p /deployments/myapp", Output: ""},
		{Match: "mkdir /deployments/myapp/.lock", Output: ""},
		{Match: "if [ ! -e '/deployments/myapp/state.json' ]", Output: "absent"},
		{Match: "if [ ! -e '/deployments/myapp/state' ]", Output: "absent"},
		{Match: "ss -tln", Output: ssOutput},
		{Match: "docker run", Output: "abc123def456"},
		{Match: "docker inspect -f '{{.Image}}'", Output: "sha256:" + strings.Repeat("a", 64)},
		{Match: "docker inspect", Output: "running"},
	}
	if healthCurl != nil {
		cmds = append(cmds, *healthCurl)
	}
	if dial != nil {
		cmds = append(cmds, *dial)
	}
	cmds = append(cmds,
		ssh.MockCommand{Match: "curl -sf http://localhost:2019/config/apps/http/servers/srv0", Output: `{"listen":[":80",":443"]}`},
		ssh.MockCommand{Match: "curl -sf -X PATCH", Err: fmt.Errorf("not found")},
		ssh.MockCommand{Match: "curl -sf -X POST http://localhost:2019/config/apps/http/servers/srv0/routes", Output: ""},
		ssh.MockCommand{Match: "rm -f /tmp/teploy_caddy", Output: ""},
		ssh.MockCommand{Match: "cat /deployments/caddy/Caddyfile", Output: "{\n\tadmin 0.0.0.0:2019\n}\n"},
		ssh.MockCommand{Match: "mv /tmp/teploy_caddyfile.tmp", Output: ""},
		ssh.MockCommand{Match: "mkdir /deployments/caddy/.lock", Output: ""},
		ssh.MockCommand{Match: "a=$(docker exec caddy md5sum", Output: "TEPLOY_CADDY_OK"},
		ssh.MockCommand{Match: "docker exec caddy caddy reload", Output: ""},
		ssh.MockCommand{Match: "rmdir /deployments/caddy/.lock", Output: ""},
		ssh.MockCommand{Match: "printf %s", Output: ""},
		ssh.MockCommand{Match: "rm -rf /deployments/myapp/.lock", Output: ""},
	)
	return ssh.NewMockExecutor("1.2.3.4", cmds...)
}

func firstDeployConfig(health HealthConfig) Config {
	manifest := json.RawMessage(`{"app":"myapp","env_keys":["TOKEN"]}`)
	return Config{
		App:             "myapp",
		Domain:          "myapp.com",
		Image:           "myapp:latest",
		Version:         "abc123",
		Health:          health,
		ManifestSHA256:  fmt.Sprintf("%x", sha256.Sum256(manifest)),
		AppliedManifest: manifest,
	}
}

// The deploy output states which mode and deadline gate the traffic switch
// BEFORE the gate runs, and an http-mode deploy probes the configured path.
func TestDeploy_HTTPModeSurfacesReadinessLineBeforeGate(t *testing.T) {
	mock := firstDeployMock(&ssh.MockCommand{Match: "curl -s -o /dev/null", Output: "200"}, nil)
	var buf bytes.Buffer
	d := NewDeployer(mock, &buf)

	cfg := firstDeployConfig(HealthConfig{Mode: "http", Path: "/healthz", Timeout: 5 * time.Second, Interval: 10 * time.Millisecond})
	if err := d.Deploy(context.Background(), cfg); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	output := buf.String()
	line := "Readiness: HTTP GET /healthz (5s deadline)"
	if !strings.Contains(output, line) {
		t.Errorf("output must state the readiness gate up front, got: %s", output)
	}
	if strings.Index(output, line) > strings.Index(output, "Health check passed") {
		t.Errorf("readiness line must precede the gate result, got: %s", output)
	}
	var probed string
	for _, c := range mock.Calls {
		if strings.HasPrefix(c, "curl -s -o /dev/null") {
			probed = c
		}
	}
	if !strings.Contains(probed, "/healthz") {
		t.Errorf("http mode must probe the configured path, got: %s", probed)
	}
}

// A tcp-mode deploy gates on the dial alone: no HTTP probe command is ever
// issued, the surfaced line names TCP + the port, and the release record
// carries the mode for rollback.
func TestDeploy_TCPModeGatesOnDialNeverCurl(t *testing.T) {
	mock := firstDeployMock(nil, &ssh.MockCommand{Match: "bash -c", Output: ""})
	var buf bytes.Buffer
	d := NewDeployer(mock, &buf)

	cfg := firstDeployConfig(HealthConfig{Mode: "tcp", Timeout: 5 * time.Second, Interval: 10 * time.Millisecond})
	if err := d.Deploy(context.Background(), cfg); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "Readiness: TCP :49152 (5s)") {
		t.Errorf("output must surface the TCP gate, got: %s", output)
	}
	var sawDial bool
	for _, c := range mock.Calls {
		if strings.HasPrefix(c, "curl -s -o /dev/null") {
			t.Errorf("tcp-mode deploy must never run the HTTP probe: %s", c)
		}
		if strings.HasPrefix(c, "bash -c") {
			sawDial = true
		}
	}
	if !sawDial {
		t.Error("tcp-mode deploy must gate on the dial")
	}
	var recordedMode string
	for _, data := range mock.Files {
		if bytes.Contains(data, []byte(`"health"`)) {
			var probe struct {
				Health *struct {
					Mode string `json:"mode"`
				} `json:"health"`
			}
			if json.Unmarshal(data, &probe) == nil && probe.Health != nil {
				recordedMode = probe.Health.Mode
			}
		}
	}
	if recordedMode != "tcp" {
		t.Errorf("release record must carry health mode tcp for rollback, got %q", recordedMode)
	}
}

// The auto default (mode empty) surfaces as the named compat mode.
func TestDeploy_AutoDefaultSurfacesCompatLine(t *testing.T) {
	mock := firstDeployMock(&ssh.MockCommand{Match: "curl -s -o /dev/null", Output: "200"}, nil)
	var buf bytes.Buffer
	d := NewDeployer(mock, &buf)

	cfg := firstDeployConfig(HealthConfig{Timeout: 5 * time.Second, Interval: 10 * time.Millisecond})
	if err := d.Deploy(context.Background(), cfg); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if !strings.Contains(buf.String(), "Readiness: auto — HTTP then TCP fallback (compat, 5s deadline)") {
		t.Errorf("auto default must surface the compat line, got: %s", buf.String())
	}
}

// --- rollback record forwarding ---

func recordWithHealthMode(mode string) *releasemeta.Record {
	rec := &releasemeta.Record{App: "myapp", Hash: "v1", IngressMode: "caddy"}
	if mode != "" {
		rec.Health = &releasemeta.Health{Mode: mode, Path: "/rec", TimeoutSeconds: 20, IntervalSeconds: 2}
	}
	return rec
}

func TestApplyRecordToRollback_ForwardsHealthMode(t *testing.T) {
	cfg := &RollbackConfig{App: "myapp", Domain: "myapp.com"}
	health := HealthConfig{Path: "/healthz", Timeout: 30 * time.Second, Interval: time.Second}
	rec := recordWithHealthMode("tcp")
	applyRecordToRollback(cfg, rec, &health)
	if health.Mode != "tcp" {
		t.Errorf("rollback health mode = %q, want tcp from the record", health.Mode)
	}
	// An old record without a mode keeps whatever the config said (compat).
	health = HealthConfig{Mode: "http"}
	applyRecordToRollback(cfg, recordWithHealthMode(""), &health)
	if health.Mode != "http" {
		t.Errorf("modeless record must not override, got %q", health.Mode)
	}
}
