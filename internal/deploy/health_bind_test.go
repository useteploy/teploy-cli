package deploy

import (
	"context"
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
