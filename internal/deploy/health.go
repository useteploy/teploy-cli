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

// Health probe modes (C03). The mode selects what the readiness gate runs;
// every mode shares the same total deadline and interval semantics.
//
// auto is the compatibility mode and the default when mode is unset: HTTP
// GET first, and a 404/3xx answer falls back to a TCP dial — the exact
// behavior every teploy deploy used before modes existed (register
// F47/TCL-17/A22). http is status-based only (200 = ready, no fallback);
// tcp dials the port and never speaks HTTP.
const (
	HealthModeHTTP = "http"
	HealthModeTCP  = "tcp"
	HealthModeAuto = "auto"
)

// HealthConfig configures health check behavior.
type HealthConfig struct {
	// Mode selects the probe: HealthModeHTTP, HealthModeTCP, or
	// HealthModeAuto. Empty means auto (documented compat default).
	Mode string
	// Path is the URL path checked in http/auto mode. Default: "/health".
	// Irrelevant (and rejected at config load) in tcp mode.
	Path string
	// Timeout is the TOTAL time to wait for healthy (default 30s) — not a
	// per-attempt bound: the gate fails at this deadline however many
	// attempts fit inside it.
	Timeout  time.Duration
	Interval time.Duration
}

// defaultHealthConfig returns a HealthConfig with all default values applied.
func defaultHealthConfig() HealthConfig {
	return HealthConfig{}.withDefaults()
}

func (h HealthConfig) withDefaults() HealthConfig {
	if h.Path == "" {
		h.Path = "/health"
	}
	if h.Mode == "" {
		h.Mode = HealthModeAuto
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

// healthCheck polls the container until it reports ready or the timeout
// expires. cfg.Timeout is the TOTAL deadline: the loop stops there however
// many attempts fit, each HTTP attempt is additionally bounded by curl's
// --connect-timeout/--max-time, and the executor cancels the remote command
// when the deadline context dies — there is no unbounded retry.
//
// The probe itself is selected by cfg.Mode (see HealthMode* constants):
// http runs the status check only, tcp the dial only, and auto (the
// historical behavior, now named) runs the status check with the 404/3xx
// TCP fallback.
func (d *Deployer) healthCheck(ctx context.Context, port int, cfg HealthConfig, bindHost string) error {
	cfg = cfg.withDefaults()
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()

	host := healthProbeHost(bindHost)
	for {
		if d.probeOnce(ctx, host, port, cfg) {
			return nil
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout after %s waiting for health check (mode %s) on %s:%d", cfg.Timeout, cfg.Mode, host, port)
		case <-time.After(cfg.Interval):
			// retry
		}
	}
}

// probeOnce runs ONE readiness attempt under the configured mode.
func (d *Deployer) probeOnce(ctx context.Context, host string, port int, cfg HealthConfig) bool {
	switch cfg.Mode {
	case HealthModeHTTP:
		return d.checkHTTP(ctx, host, port, cfg.Path)
	case HealthModeTCP:
		return d.checkTCP(ctx, host, port)
	default:
		return d.checkHealth(ctx, host, port, cfg.Path)
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

// checkHealth performs a single AUTO-mode attempt (the compatibility
// strategy): the HTTP status check, with exactly a 404 or a 3xx falling
// back to a TCP dial — every other answer (5xx, no response, malformed
// probe) is retried until the deadline. This is the historical behavior,
// preserved verbatim as the named compat mode.
func (d *Deployer) checkHealth(ctx context.Context, host string, port int, path string) bool {
	code := d.httpStatus(ctx, host, port, path)
	if code == "200" {
		return true
	}
	// A 404 (no /health endpoint) or a 3xx redirect means the app is
	// listening but the health path isn't a 200 — for example WordPress
	// 301-redirects /health to its canonical HTTPS URL. Fall back to a TCP
	// check rather than failing the deploy.
	if code == "404" || strings.HasPrefix(code, "3") {
		return d.checkTCP(ctx, host, port)
	}
	return false
}

// checkHTTP performs a single HTTP-mode attempt: true only on a 200. No
// fallback — a 404/3xx fails the attempt and the gate retries or times out.
func (d *Deployer) checkHTTP(ctx context.Context, host string, port int, path string) bool {
	return d.httpStatus(ctx, host, port, path) == "200"
}

// httpStatus issues one bounded curl request and reports the HTTP status
// code it observed, or "" when the request could not be made or answered
// (unbuildable URL, transport error, empty reply).
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
func (d *Deployer) httpStatus(ctx context.Context, host string, port int, path string) string {
	url, ok := probeURL(host, port, path)
	if !ok {
		return ""
	}
	cmd := fmt.Sprintf(
		"curl -s -o /dev/null --noproxy '*' --globoff --connect-timeout 2 --max-time 5 -w '%%{http_code}' --url %s",
		ssh.ShellQuote(url),
	)
	output, err := d.exec.Run(ctx, cmd)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(output)
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

// readinessSummary renders the one-line description of the readiness gate
// surfaced in deploy/rollback output BEFORE the gate runs, so the operator
// knows what is being gated and for how long. cfg must already carry its
// defaults (withDefaults).
func readinessSummary(cfg HealthConfig, port int) string {
	switch cfg.Mode {
	case HealthModeHTTP:
		return fmt.Sprintf("HTTP GET %s (%s deadline)", cfg.Path, cfg.Timeout)
	case HealthModeTCP:
		return fmt.Sprintf("TCP :%d (%s)", port, cfg.Timeout)
	default:
		return fmt.Sprintf("auto — HTTP then TCP fallback (compat, %s deadline)", cfg.Timeout)
	}
}

// drainSummary renders the request-drain half of the surfaced stop policy
// (C03): 0 keeps the historical stop-immediately behavior; N names the
// window the predecessor keeps serving in-flight requests after the
// traffic switch.
func drainSummary(drainSeconds int) string {
	if drainSeconds <= 0 {
		return "disabled — the predecessor stops immediately after the switch"
	}
	return fmt.Sprintf("%ds window before predecessor retirement", drainSeconds)
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
