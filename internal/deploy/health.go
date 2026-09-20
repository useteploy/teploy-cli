package deploy

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/useteploy/teploy/internal/docker"
	"github.com/useteploy/teploy/internal/ssh"
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

// healthProbeHost is the address the server-side probe dials for a container
// published on bindHost.
//
// Docker publishes on exactly the address it was given, so a container bound to
// a specific IP is NOT reachable at localhost — probing there gets a connection
// refused for the life of the timeout. Since `ingress: host` deploys by
// recreate (the old container is stopped first), that turned every deploy of a
// specifically-bound app into a full outage: the new container is healthy, the
// probe cannot see it, the deploy fails, and nothing is left running.
//
// An empty bindHost is caddy/external ingress, which publishes on 127.0.0.1.
func healthProbeHost(bindHost string) string {
	switch bindHost {
	case "", "0.0.0.0", "::", "[::]":
		return "localhost"
	default:
		return bindHost
	}
}

// healthCheck polls the container until it responds healthy or the timeout expires.
//
// Strategy:
//  1. HTTP GET to {host}:{port}{path} — 200 means healthy.
//  2. If the endpoint returns 404, fall back to a TCP port check.
//  3. Connection refused means the app hasn't started yet — retry.
func (d *Deployer) healthCheck(ctx context.Context, port int, cfg HealthConfig, bindHost string) error {
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()

	host := healthProbeHost(bindHost)
	for {
		if d.checkHealth(ctx, host, port, cfg.Path) {
			return nil
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout after %s waiting for health check on %s:%d", cfg.Timeout, host, port)
		case <-time.After(cfg.Interval):
			// retry
		}
	}
}

// HealthCheckPublic runs a health check against the given port using default settings.
// This is the public entry point for on-demand health checks.
//
// It assumes the container is reachable at localhost. Prefer HealthCheckAt when
// a container name is available: an app deployed with a `bind:` address is NOT
// reachable at localhost, and this will report it unhealthy when it is fine.
func (d *Deployer) HealthCheckPublic(ctx context.Context, port int) error {
	return d.healthCheck(ctx, port, defaultHealthConfig(), "")
}

// HealthCheckAt health-checks a named container, probing whatever address it is
// actually published on rather than assuming localhost. Falls back to the
// localhost behavior when the address cannot be read.
func (d *Deployer) HealthCheckAt(ctx context.Context, port int, containerName string) error {
	bindHost := ""
	if containerName != "" {
		bindHost = docker.NewClient(d.exec).HostBindIP(ctx, containerName)
	}
	return d.healthCheck(ctx, port, defaultHealthConfig(), bindHost)
}

// checkHealth performs a single health check attempt.
//
// The URL is built with net.JoinHostPort (bracketing IPv6 literals) and
// validated before it reaches the remote shell, then passed as ONE
// single-quoted curl --url argument (TCL-16): the old unquoted
// interpolation let an ordinary query string containing '&' change shell
// parsing, and a bare IPv6 bind produced a malformed URL. --globoff keeps
// curl from treating {} and [] in the path as its own glob syntax, and the
// per-attempt connect/max deadlines bound each probe below the overall
// readiness timeout. --noproxy '*' (audit T20's contained half) makes the
// host-local probe ignore ambient proxy configuration — an inherited
// HTTP_PROXY made the probe ask a proxy about a loopback address.
func (d *Deployer) checkHealth(ctx context.Context, host string, port int, path string) bool {
	url, ok := probeURL(host, port, path)
	if !ok {
		return false
	}
	cmd := fmt.Sprintf(
		"curl -s -o /dev/null --noproxy '*' --globoff --connect-timeout 2 --max-time 5 -w '%%{http_code}' --url %s",
		ssh.ShellQuote(url),
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
			return d.checkTCP(ctx, host, port)
		}
	}
	return false
}

// probeURL renders the health-check URL and validates its inputs. The host
// must be an IP literal or "localhost" (it comes from the deploy config's
// bind address), and the path must be a request-path-shaped URI without
// control characters — anything else fails the attempt closed rather than
// interpolating into remote shell text.
func probeURL(host string, port int, path string) (string, bool) {
	if port < 1 || port > 65535 {
		return "", false
	}
	host = strings.Trim(host, "[]")
	if host != "localhost" && net.ParseIP(host) == nil {
		return "", false
	}
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") ||
		strings.ContainsAny(path, "\r\n\x00") {
		return "", false
	}
	p, err := url.ParseRequestURI(path)
	if err != nil || p.IsAbs() || p.Host != "" {
		return "", false
	}
	u := url.URL{
		Scheme:   "http",
		Host:     net.JoinHostPort(host, strconv.Itoa(port)),
		Path:     p.Path,
		RawPath:  p.RawPath,
		RawQuery: p.RawQuery,
	}
	return u.String(), true
}

// checkTCP verifies that a TCP connection can be established to the port.
// The /dev/tcp redirection runs inside a single-quoted bash -c argument, so
// neither the host nor the port can break out of it.
func (d *Deployer) checkTCP(ctx context.Context, host string, port int) bool {
	host = strings.Trim(host, "[]")
	if host != "localhost" && net.ParseIP(host) == nil {
		return false
	}
	cmd := fmt.Sprintf("bash -c '</dev/tcp/%s/%d' 2>/dev/null", host, port)
	_, err := d.exec.Run(ctx, cmd)
	return err == nil
}
