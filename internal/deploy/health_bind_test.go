package deploy

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/ssh"
)

// A container published on a specific IP is NOT reachable at localhost, so a
// health probe hardcoded to localhost can never succeed. Because `ingress:
// host` deploys by recreate (old container stopped first), that turned every
// deploy of a specifically-bound app into a full outage: app healthy, probe
// blind, deploy failed, nothing left running. Observed live on 2026-08-08
// when dash moved from bind 0.0.0.0 to its tailnet address.
func TestHealthProbeHost(t *testing.T) {
	cases := map[string]string{
		"":               "localhost", // caddy/external ingress publishes on 127.0.0.1
		"0.0.0.0":        "localhost",
		"::":             "localhost",
		"[::]":           "localhost",
		"100.108.123.49": "100.108.123.49", // tailnet-bound: must be probed there
		"192.168.1.84":   "192.168.1.84",
		"127.0.0.1":      "127.0.0.1",
	}
	for bind, want := range cases {
		if got := healthProbeHost(bind); got != want {
			t.Errorf("healthProbeHost(%q) = %q, want %q", bind, got, want)
		}
	}
}

func TestHealthCheck_ProbesTheBoundAddressNotLocalhost(t *testing.T) {
	mock := ssh.NewMockExecutor("h", ssh.MockCommand{Match: "curl", Output: "200"})
	d := &Deployer{exec: mock, out: nopWriter{}}

	if err := d.healthCheck(context.Background(), 3456, defaultHealthConfig(), "100.108.123.49"); err != nil {
		t.Fatalf("healthCheck: %v", err)
	}
	var probed string
	for _, c := range mock.Calls {
		if strings.HasPrefix(c, "curl") {
			probed = c
		}
	}
	if !strings.Contains(probed, "http://100.108.123.49:3456") {
		t.Errorf("probe should dial the bound address, got: %s", probed)
	}
	if strings.Contains(probed, "localhost") {
		t.Errorf("probe must not fall back to localhost for a specifically-bound app: %s", probed)
	}
}

// The TCP fallback (used when the health path 404s or redirects) has to dial
// the same address, or a bound app with no /health endpoint still fails.
func TestHealthCheck_TCPFallbackAlsoUsesTheBoundAddress(t *testing.T) {
	mock := ssh.NewMockExecutor("h",
		ssh.MockCommand{Match: "curl", Output: "404"},
		ssh.MockCommand{Match: "bash -c", Output: ""},
	)
	d := &Deployer{exec: mock, out: nopWriter{}}

	if err := d.healthCheck(context.Background(), 3456, defaultHealthConfig(), "100.108.123.49"); err != nil {
		t.Fatalf("healthCheck with TCP fallback: %v", err)
	}
	var tcp string
	for _, c := range mock.Calls {
		if strings.HasPrefix(c, "bash -c") {
			tcp = c
		}
	}
	if !strings.Contains(tcp, "/dev/tcp/100.108.123.49/3456") {
		t.Errorf("TCP fallback should dial the bound address, got: %s", tcp)
	}
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

// `teploy health` on an app deployed with a bind address must probe that
// address. HealthCheckPublic assumes localhost, which reports a perfectly
// healthy bound app as failed — the on-demand twin of the deploy-time bug
// above.
func TestHealthCheckAt_ReadsTheBindFromTheContainer(t *testing.T) {
	mock := ssh.NewMockExecutor("h",
		ssh.MockCommand{Match: "docker inspect", Output: "100.108.123.49 "},
		ssh.MockCommand{Match: "curl", Output: "200"},
	)
	d := &Deployer{exec: mock, out: nopWriter{}}

	if err := d.HealthCheckAt(context.Background(), 3000, "observe-web-abc1234"); err != nil {
		t.Fatalf("HealthCheckAt: %v", err)
	}
	var probed string
	for _, c := range mock.Calls {
		if strings.HasPrefix(c, "curl") {
			probed = c
		}
	}
	if !strings.Contains(probed, "http://100.108.123.49:3000") {
		t.Errorf("should probe the container's bind address, got: %s", probed)
	}
}

// An all-interfaces container is reachable at localhost, and probing the
// literal 0.0.0.0 would not be.
func TestHealthCheckAt_AllInterfacesProbesLocalhost(t *testing.T) {
	mock := ssh.NewMockExecutor("h",
		ssh.MockCommand{Match: "docker inspect", Output: "0.0.0.0 "},
		ssh.MockCommand{Match: "curl", Output: "200"},
	)
	d := &Deployer{exec: mock, out: nopWriter{}}

	if err := d.HealthCheckAt(context.Background(), 7460, "ship-web-abc1234"); err != nil {
		t.Fatalf("HealthCheckAt: %v", err)
	}
	for _, c := range mock.Calls {
		if strings.HasPrefix(c, "curl") && !strings.Contains(c, "localhost") {
			t.Errorf("0.0.0.0 should be probed as localhost, got: %s", c)
		}
	}
}

// An unreadable container (renamed, removed, inspect fails) must not break the
// check — fall back to the historical localhost behavior.
func TestHealthCheckAt_FallsBackWhenBindUnreadable(t *testing.T) {
	mock := ssh.NewMockExecutor("h",
		ssh.MockCommand{Match: "docker inspect", Err: errNotFound},
		ssh.MockCommand{Match: "curl", Output: "200"},
	)
	d := &Deployer{exec: mock, out: nopWriter{}}

	if err := d.HealthCheckAt(context.Background(), 3000, "gone"); err != nil {
		t.Fatalf("HealthCheckAt should fall back, got: %v", err)
	}
}

var errNotFound = fmt.Errorf("no such container")
