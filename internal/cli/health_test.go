package cli

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/deploy"
	"github.com/useteploy/teploy/internal/ssh"
)

func TestHealthCommandHonorsHTTPContract(t *testing.T) {
	for _, stateOnly := range []bool{false, true} {
		mock := ssh.NewMockExecutor("host",
			ssh.MockCommand{Match: "if [ ! -e", Output: `present
{"schema_version":1,"app":"demo","hash":"v1","health":{"mode":"http","path":"/ready","timeout_seconds":1,"interval_seconds":1}}`},
			ssh.MockCommand{Match: "curl", Output: "404"},
			ssh.MockCommand{Match: "bash -c", Output: ""},
		)
		cfg, err := healthConfigForCommand(context.Background(), mock, &config.AppConfig{App: "demo", Health: config.AppHealthConfig{Mode: "http", Path: "/ready", TimeoutSeconds: 1, IntervalSeconds: 1}}, "v1", stateOnly)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Timeout != time.Second || cfg.Interval != time.Second {
			t.Fatalf("lost timing contract: %+v", cfg)
		}
		cfg.Timeout, cfg.Interval = 30*time.Millisecond, time.Millisecond
		err = deploy.NewDeployer(mock, nil).HealthCheckAtWithConfig(context.Background(), 8080, "", cfg)
		if err == nil {
			t.Fatal("HTTP 404 must fail even when TCP succeeds")
		}
		var sawPath bool
		for _, call := range mock.Calls {
			if strings.Contains(call, "curl") && strings.Contains(call, "/ready") {
				sawPath = true
			}
			if strings.HasPrefix(call, "bash -c") {
				t.Fatal("HTTP-only health used TCP fallback")
			}
		}
		if !sawPath {
			t.Fatal("configured readiness path was not probed")
		}
	}
}

func TestHealthCommandRefusesUnknownRecordedContract(t *testing.T) {
	for _, output := range []string{"absent", "present\n{broken", `present
{"schema_version":1,"app":"demo","hash":"v1"}`} {
		mock := ssh.NewMockExecutor("host", ssh.MockCommand{Match: "if [ ! -e", Output: output})
		_, err := healthConfigForCommand(context.Background(), mock, &config.AppConfig{App: "demo"}, "v1", true)
		if err == nil {
			t.Fatalf("unknown contract accepted: %q", output)
		}
	}
}

func TestHealthCommandTCPDoesNotProbeHTTP(t *testing.T) {
	mock := ssh.NewMockExecutor("host", ssh.MockCommand{Match: "bash -c", Output: ""})
	cfg, err := healthConfigForCommand(context.Background(), mock, &config.AppConfig{Health: config.AppHealthConfig{Mode: "tcp"}}, "v1", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := deploy.NewDeployer(mock, nil).HealthCheckAtWithConfig(context.Background(), 8080, "", cfg); err != nil {
		t.Fatal(err)
	}
	if len(mock.Calls) == 0 {
		t.Fatal("no probe ran")
	}
	for _, call := range mock.Calls {
		if strings.Contains(call, "curl") {
			t.Fatal("TCP health unexpectedly probed HTTP")
		}
	}
}
