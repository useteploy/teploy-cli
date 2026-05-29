package deploy

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// HealthConfig configures health check behavior.
type HealthConfig struct {
	Path     string        // URL path to check (default "/health")
	Timeout  time.Duration // total time to wait for healthy (default 30s)
	Interval time.Duration // time between checks (default 1s)
}

// defaultHealthConfig returns a HealthConfig with all default values applied.
func defaultHealthConfig() HealthConfig {
	return HealthConfig{}.withDefaults()
}

func (h HealthConfig) withDefaults() HealthConfig {
	if h.Path == "" {
		h.Path = "/health"
	}
	if h.Timeout == 0 {
		h.Timeout = 30 * time.Second
	}
	if h.Interval == 0 {
		h.Interval = time.Second
	}
	return h
}

// healthCheck polls the container until it responds healthy or the timeout expires.
//
// Strategy:
//  1. HTTP GET to localhost:{port}{path} — 200 means healthy.
//  2. If the endpoint returns 404, fall back to a TCP port check.
//  3. Connection refused means the app hasn't started yet — retry.
func (d *Deployer) healthCheck(ctx context.Context, port int, cfg HealthConfig) error {
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()

	for {
		if d.checkHealth(ctx, port, cfg.Path) {
			return nil
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout after %s waiting for health check on port %d", cfg.Timeout, port)
		case <-time.After(cfg.Interval):
			// retry
		}
	}
}

// HealthCheckPublic runs a health check against the given port using default settings.
// This is the public entry point for on-demand health checks.
func (d *Deployer) HealthCheckPublic(ctx context.Context, port int) error {
	return d.healthCheck(ctx, port, defaultHealthConfig())
}

// checkHealth performs a single health check attempt.
func (d *Deployer) checkHealth(ctx context.Context, port int, path string) bool {
	cmd := fmt.Sprintf(
		"curl -s -o /dev/null -w '%%{http_code}' http://localhost:%d%s",
		port, path,
	)
	output, err := d.exec.Run(ctx, cmd)
	if err == nil {
		code := strings.TrimSpace(output)
		if code == "200" {
			return true
		}
		// A 404 (no /health endpoint) or a 3xx redirect means the app is
		// listening but the health path isn't a 200 — for example WordPress
		// 301-redirects /health to its canonical HTTPS URL. Fall back to a TCP
		// check rather than failing the deploy. A 5xx or "000" (no response)
		// falls through and is retried until the timeout.
		if code == "404" || strings.HasPrefix(code, "3") {
			return d.checkTCP(ctx, port)
		}
	}
	return false
}

// checkTCP verifies that a TCP connection can be established to the port.
func (d *Deployer) checkTCP(ctx context.Context, port int) bool {
	cmd := fmt.Sprintf("bash -c '</dev/tcp/localhost/%d' 2>/dev/null", port)
	_, err := d.exec.Run(ctx, cmd)
	return err == nil
}
