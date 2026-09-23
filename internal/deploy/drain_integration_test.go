//go:build integration

// Fixture-gated verification of C03's request-drain acceptance against a
// REAL SSH+Docker host with a REAL Caddy: a blue/green switch driven
// through the production caddy.Client (lock, adapt gate, guarded commit,
// reload, delivery verification), a continuous request stream across the
// switch, and a long in-flight request that must complete inside the
// drain window — ZERO failed requests for the declared fixture.
//
// Same fixture contract as the other harnesses:
//
//	TEPLOY_FAULT_HOST=127.0.0.1:50075 \
//	TEPLOY_FAULT_USER=tyler \
//	TEPLOY_FAULT_KEY=~/.colima/_lima/_config/user \
//	go test -tags integration -run TestDrainIntegration -v ./internal/deploy
//
// Needs image pulls (caddy:2-alpine, python:3-alpine, curlimages/curl) and
// skips when /deployments/caddy already exists (a provisioned real caddy
// would conflict with the fixture's container name). Disposable fixture
// only: creates and removes drainprobe-* containers plus
// /deployments/{caddy,drainprobe}.
//
// Honest scope: Caddy's config-level routing cannot count in-flight
// requests per upstream, so the drain POLICY is the time window between
// the switch and the predecessor stop plus the SIGTERM→SIGKILL ladder
// inside docker stop. This fixture proves exactly that promise: normal
// requests never fail across the switch, and a request that started
// BEFORE the switch completes inside the window. The negative control
// (stop -t 0 with an in-flight request) proves the fixture can detect a
// broken promise — the drain window is what saves the long request.
package deploy

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy/internal/caddy"
	"github.com/useteploy/teploy/internal/ssh"
)

// drainServerPy is the blue/green fixture app: 200 on everything, a
// ?ms=N /slow path for long requests, and graceful SIGTERM (finish
// in-flight, then exit) so docker stop's ladder is observable.
const drainServerPy = `import os, signal, threading, time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
NAME = os.environ.get("SERVER_NAME", "?").encode()
class H(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path.startswith("/slow"):
            ms = 1000
            if "ms=" in self.path:
                try: ms = int(self.path.split("ms=")[-1].split("&")[0])
                except ValueError: pass
            time.sleep(ms / 1000.0)
        body = NAME
        try:
            self.send_response(200)
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
        except Exception:
            pass
    def log_message(self, *a):
        pass
srv = ThreadingHTTPServer(("0.0.0.0", 8080), H)
def stop(sig, frm):
    threading.Thread(target=srv.shutdown, daemon=True).start()
signal.signal(signal.SIGTERM, stop)
signal.signal(signal.SIGINT, stop)
srv.serve_forever()
`

const (
	drainNet       = "teploy"
	drainAppPrefix = "drainprobe"
	drainBlue      = drainAppPrefix + "-web-blue"
	drainGreen     = drainAppPrefix + "-web-green"
	drainCaddyName = "caddy" // the production reload/adapt paths hardcode this name
	drainLoad      = drainAppPrefix + "-load"
	drainSlowTmpl  = drainAppPrefix + "-slow"
)

func drainRun(t *testing.T, exec ssh.Executor, ctx context.Context, cmd string) {
	t.Helper()
	if out, err := exec.Run(ctx, cmd); err != nil {
		t.Fatalf("%s: %v\n%s", cmd, err, out)
	}
}

func drainContainerOut(t *testing.T, exec ssh.Executor, ctx context.Context, name string) (string, string) {
	t.Helper()
	out, err := exec.Run(ctx, "docker inspect -f '{{.State.Status}}:{{.State.ExitCode}}' "+name+" 2>/dev/null || true")
	if err != nil {
		t.Fatalf("inspecting %s: %v", name, err)
	}
	fields := strings.SplitN(strings.TrimSpace(out), ":", 2)
	if len(fields) != 2 {
		return "", ""
	}
	return fields[0], fields[1]
}

// TestDrainIntegration_BlueGreenZeroFailedRequests is C03's declared
// blue/green fixture: continuous traffic through a real Caddy across a
// production-shaped route switch, a drain window, then a graceful stop —
// zero failed requests, and the long request completes on blue.
func TestDrainIntegration_BlueGreenZeroFailedRequests(t *testing.T) {
	host, user, key, _ := reconcileFixtureEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	exec, err := ssh.Connect(ctx, ssh.ConnectConfig{Host: host, User: user, KeyPath: key})
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	// Close AFTER the cleanup work: registered as the FIRST cleanup so
	// LIFO runs it LAST — a plain defer would close the session before
	// the resource cleanups' commands could run on it.
	t.Cleanup(func() { exec.Close() })
	if out, err := exec.Run(ctx, "docker version --format '{{.Server.Version}}'"); err != nil {
		t.Skipf("fixture host has no reachable docker daemon (%v)", err)
	} else {
		t.Logf("fixture docker server %s", strings.TrimSpace(out))
	}
	// A provisioned caddy tree means a real setup lives here — do not
	// fight it for the container name.
	if out, _ := exec.Run(ctx, "test -e /deployments/caddy/Caddyfile && echo yes || echo no"); strings.TrimSpace(out) == "yes" {
		t.Skip("fixture has a provisioned /deployments/caddy — remove it or use a disposable host for the drain fixture")
	}

	// Images + network.
	for _, image := range []string{"caddy:2-alpine", "python:3-alpine", "curlimages/curl:latest"} {
		if out, err := exec.Run(ctx, "docker pull "+image); err != nil {
			t.Skipf("cannot pull %s (%v) — pre-pull it on the fixture", image, err)
		} else if out != "" {
			t.Logf("pulled %s", image)
		}
	}
	drainRun(t, exec, ctx, "docker network create "+drainNet+" 2>/dev/null || true")

	// Cleanup on exit: containers, fixture dirs, the caddy container we
	// temporarily own. The teploy network is left alone (shared).
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer ccancel()
		for _, n := range []string{drainLoad, drainBlue, drainGreen, drainCaddyName} {
			exec.Run(cctx, "docker rm -f "+n)
		}
		for i := 0; i < 8; i++ {
			exec.Run(cctx, fmt.Sprintf("docker rm -f %s-%d", drainSlowTmpl, i))
		}
		exec.Run(cctx, "rm -rf /deployments/caddy /deployments/"+drainAppPrefix)
	})

	// Fixture app + caddy bootstrap.
	drainRun(t, exec, ctx, "mkdir -p /deployments/caddy /deployments/"+drainAppPrefix)
	if err := exec.Upload(ctx, strings.NewReader(drainServerPy), "/deployments/"+drainAppPrefix+"/server.py", "0644"); err != nil {
		t.Fatalf("uploading server.py: %v", err)
	}
	if err := exec.Upload(ctx, strings.NewReader("{\n\tadmin 127.0.0.1:2019\n}\n"), "/deployments/caddy/Caddyfile", "0644"); err != nil {
		t.Fatalf("uploading initial Caddyfile: %v", err)
	}
	appRun := func(name, serverName string) string {
		return fmt.Sprintf(
			"docker run -d --name %s --network %s --label teploy.app=%s -e SERVER_NAME=%s -v /deployments/%s/server.py:/srv/server.py:ro python:3-alpine python /srv/server.py",
			name, drainNet, drainAppPrefix, serverName, drainAppPrefix)
	}
	drainRun(t, exec, ctx, appRun(drainBlue, "blue"))
	drainRun(t, exec, ctx, fmt.Sprintf(
		"docker run -d --name %s --network %s -v /deployments/caddy:/etc/caddy -p 127.0.0.1:0:80 caddy:2-alpine", drainCaddyName, drainNet))

	waitHTTP := func(container string, timeout time.Duration) bool {
		deadline := time.Now().Add(timeout)
		for time.Now().Before(deadline) {
			out, err := exec.Run(ctx, fmt.Sprintf(
				"docker exec %s python -c \"import urllib.request;urllib.request.urlopen('http://127.0.0.1:8080/id',timeout=2)\" 2>/dev/null && echo ok || true", container))
			if err == nil && strings.TrimSpace(out) == "ok" {
				return true
			}
			time.Sleep(300 * time.Millisecond)
		}
		return false
	}
	if !waitHTTP(drainBlue, 30*time.Second) {
		t.Fatal("blue never became ready — fixture broken")
	}

	// Caddy's in-network IP is the site address (an IP literal forces the
	// http:// scheme — no ACME for a fixture host).
	caddyIP := strings.TrimSpace(mustOut(t, exec, ctx,
		"docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "+drainCaddyName))
	if caddyIP == "" || !strings.Contains(caddyIP, ".") {
		t.Fatalf("no IPv4 for the caddy container (got %q)", caddyIP)
	}

	// The PRODUCTION switch machinery: real lock, real adapt gate, real
	// reload, real delivery verification.
	client := caddy.NewClient(exec)
	switchRoute := func(upstream string) error {
		return client.SetRoute(ctx, drainAppPrefix, caddyIP, upstream, 8080, caddy.TLS{}, "", nil, caddy.Firewall{}, caddy.Access{})
	}
	if err := switchRoute(drainBlue); err != nil {
		t.Fatalf("routing to blue through the production client: %v", err)
	}
	probeOnce := func() string {
		out, err := exec.Run(ctx, fmt.Sprintf(
			"docker run --rm --network %s curlimages/curl:latest -s --max-time 5 -H %s http://%s:80/id",
			drainNet, ssh.ShellQuote("Host: "+caddyIP), drainCaddyName))
		if err != nil {
			return "err:" + err.Error()
		}
		return strings.TrimSpace(out)
	}
	if got := probeOnce(); got != "blue" {
		t.Fatalf("fixture sanity: expected blue through caddy, got %q", got)
	}

	// Continuous load: ~8 req/s for the whole scenario; every non-200 or
	// connection error is a failure line. Runs detached, results in a
	// volume the assertions read afterwards.
	drainRun(t, exec, ctx, "docker volume create "+drainAppPrefix+"-out 2>/dev/null || true")
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer ccancel()
		exec.Run(cctx, "docker volume rm "+drainAppPrefix+"-out")
	})
	loadLoop := fmt.Sprintf(
		`i=0; fails=0; while [ $i -lt 90 ]; do `+
			`code=$(curl -s -o /dev/null -w '%%{http_code}' --max-time 5 -H %s http://%s:80/id 2>/dev/null); `+
			`if [ "$code" != "200" ]; then fails=$((fails+1)); echo "fail:$i:$code" >> /out/fail.log; fi; `+
			`i=$((i+1)); sleep 0.15; done; echo $fails > /out/failcount`,
		ssh.ShellQuote("Host: "+caddyIP), drainCaddyName)
	drainRun(t, exec, ctx, fmt.Sprintf(
		"docker run -d --user root --name %s --network %s -v %s:/out curlimages/curl:latest sh -c %s",
		drainLoad, drainNet, drainAppPrefix+"-out", ssh.ShellQuote(loadLoop)))

	// A long request (~3.5s) on BLUE, started ~1s BEFORE the switch: it is
	// in flight across the switch and must complete inside the drain
	// window (switch + 5s) with blue's body.
	startSlow := func(tag string) {
		inner := fmt.Sprintf("curl -s --max-time 10 -H %s http://%s:80/slow?ms=3500 -o /out/%s.body -w '%%{http_code}' > /out/%s.code",
			ssh.ShellQuote("Host: "+caddyIP), drainCaddyName, tag, tag)
		drainRun(t, exec, ctx, fmt.Sprintf(
			"docker run -d --user root --name %s --network %s -v %s:/out curlimages/curl:latest sh -c %s",
			tag, drainNet, drainAppPrefix+"-out", ssh.ShellQuote(inner)))
	}
	startSlow(drainSlowTmpl + "-0")
	time.Sleep(1 * time.Second)

	// The green deploy: start, readiness-gate, switch, drain, retire.
	drainRun(t, exec, ctx, appRun(drainGreen, "green"))
	if !waitHTTP(drainGreen, 30*time.Second) {
		t.Fatal("green never became ready — failing health must never switch traffic; fixture stopped before the switch")
	}
	if err := switchRoute(drainGreen); err != nil {
		t.Fatalf("switching to green: %v", err)
	}
	t.Logf("switched to green; draining 5s before retiring blue")
	time.Sleep(5 * time.Second)
	drainRun(t, exec, ctx, "docker stop -t 5 "+drainBlue)

	// Assertions. Zero failed requests for the declared fixture.
	deadline := time.Now().Add(30 * time.Second)
	failcount := "?"
	for time.Now().Before(deadline) {
		out, _ := exec.Run(ctx, "docker run --rm -v "+drainAppPrefix+"-out:/out curlimages/curl:latest sh -c 'cat /out/failcount' 2>/dev/null || true")
		if s := strings.TrimSpace(out); s != "" {
			failcount = s
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if failcount != "0" {
		failLog := mustOut(t, exec, ctx, "docker run --rm -v "+drainAppPrefix+"-out:/out curlimages/curl:latest sh -c 'cat /out/fail.log 2>/dev/null || true'")
		t.Fatalf("blue/green fixture reported FAILED requests (count=%s):\n%s", failcount, failLog)
	}

	slowStatus, slowCode := drainContainerOut(t, exec, ctx, drainSlowTmpl+"-0")
	if slowStatus != "exited" || slowCode != "0" {
		t.Fatalf("the long in-flight request must complete inside the drain window (status=%s exit=%s)", slowStatus, slowCode)
	}
	slowBody := strings.TrimSpace(mustOut(t, exec, ctx, "docker run --rm -v "+drainAppPrefix+"-out:/out curlimages/curl:latest sh -c 'cat /out/"+drainSlowTmpl+"-0.body 2>/dev/null || true'"))
	if slowBody != "blue" {
		t.Fatalf("the long request should have been served end-to-end by blue, got %q", slowBody)
	}
	if got := probeOnce(); got != "green" {
		t.Fatalf("post-switch traffic must serve green, got %q", got)
	}
	t.Logf("drain fixture PASS: 90/90 requests ok, long request completed on blue within the window, traffic on green")
}

// TestDrainIntegration_NoDrainKillsLongRequest is the negative control:
// without the window (stop -t 0 immediately after the switch), an
// in-flight request is killed — proving the fixture detects a broken
// drain promise and that the window above is what saves the request.
func TestDrainIntegration_NoDrainKillsLongRequest(t *testing.T) {
	host, user, key, _ := reconcileFixtureEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	exec, err := ssh.Connect(ctx, ssh.ConnectConfig{Host: host, User: user, KeyPath: key})
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	// Close AFTER the cleanup work: registered as the FIRST cleanup so
	// LIFO runs it LAST — a plain defer would close the session before
	// the resource cleanups' commands could run on it.
	t.Cleanup(func() { exec.Close() })
	if out, _ := exec.Run(ctx, "test -e /deployments/caddy/Caddyfile && echo yes || echo no"); strings.TrimSpace(out) == "yes" {
		t.Skip("fixture has a provisioned /deployments/caddy — remove it or use a disposable host for the drain fixture")
	}
	drainRun(t, exec, ctx, "mkdir -p /deployments/caddy /deployments/"+drainAppPrefix)
	if err := exec.Upload(ctx, strings.NewReader(drainServerPy), "/deployments/"+drainAppPrefix+"/server.py", "0644"); err != nil {
		t.Fatalf("uploading server.py: %v", err)
	}
	if err := exec.Upload(ctx, strings.NewReader("{\n\tadmin 127.0.0.1:2019\n}\n"), "/deployments/caddy/Caddyfile", "0644"); err != nil {
		t.Fatalf("uploading initial Caddyfile: %v", err)
	}
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer ccancel()
		for _, n := range []string{drainBlue, drainGreen, drainCaddyName} {
			exec.Run(cctx, "docker rm -f "+n)
		}
		exec.Run(cctx, "docker rm -f "+drainSlowTmpl+"-neg")
		exec.Run(cctx, "rm -rf /deployments/caddy /deployments/"+drainAppPrefix)
	})
	appRun := func(name, serverName string) string {
		return fmt.Sprintf(
			"docker run -d --name %s --network %s --label teploy.app=%s -e SERVER_NAME=%s -v /deployments/%s/server.py:/srv/server.py:ro python:3-alpine python /srv/server.py",
			name, drainNet, drainAppPrefix, serverName, drainAppPrefix)
	}
	drainRun(t, exec, ctx, appRun(drainBlue, "blue"))
	drainRun(t, exec, ctx, fmt.Sprintf(
		"docker run -d --name %s --network %s -v /deployments/caddy:/etc/caddy caddy:2-alpine", drainCaddyName, drainNet))
	deadline := time.Now().Add(30 * time.Second)
	ready := false
	for time.Now().Before(deadline) {
		out, _ := exec.Run(ctx, fmt.Sprintf(
			"docker exec %s python -c \"import urllib.request;urllib.request.urlopen('http://127.0.0.1:8080/id',timeout=2)\" 2>/dev/null && echo ok || true", drainBlue))
		if strings.TrimSpace(out) == "ok" {
			ready = true
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if !ready {
		t.Fatal("blue never became ready — fixture broken")
	}
	caddyIP := strings.TrimSpace(mustOut(t, exec, ctx,
		"docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "+drainCaddyName))
	client := caddy.NewClient(exec)
	if err := client.SetRoute(ctx, drainAppPrefix, caddyIP, drainBlue, 8080, caddy.TLS{}, "", nil, caddy.Firewall{}, caddy.Access{}); err != nil {
		t.Fatalf("routing to blue: %v", err)
	}

	// Long request in flight, then an IMMEDIATE stop with no grace. The
	// request's VERDICT is the pair (http code, body): served-by-blue
	// means it survived; anything else (connection reset, caddy's 502)
	// means the no-grace kill broke it — which is the point.
	drainRun(t, exec, ctx, "docker volume create "+drainAppPrefix+"-out 2>/dev/null || true")
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer ccancel()
		exec.Run(cctx, "docker volume rm "+drainAppPrefix+"-out")
	})
	slowInner := fmt.Sprintf("curl -s --max-time 10 -H %s http://%s:80/slow?ms=3000 -o /out/neg.body -w '%%{http_code}' > /out/neg.code; echo $? > /out/neg.exit",
		ssh.ShellQuote("Host: "+caddyIP), drainCaddyName)
	drainRun(t, exec, ctx, fmt.Sprintf(
		"docker run -d --user root --name %s --network %s -v %s:/out curlimages/curl:latest sh -c %s",
		drainSlowTmpl+"-neg", drainNet, drainAppPrefix+"-out", ssh.ShellQuote(slowInner)))
	time.Sleep(700 * time.Millisecond)
	drainRun(t, exec, ctx, "docker stop -t 0 "+drainBlue)

	status, _ := drainContainerOut(t, exec, ctx, drainSlowTmpl+"-neg")
	waitDeadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(waitDeadline) && status != "exited" {
		time.Sleep(300 * time.Millisecond)
		status, _ = drainContainerOut(t, exec, ctx, drainSlowTmpl+"-neg")
	}
	readOut := func(name string) string {
		return strings.TrimSpace(mustOut(t, exec, ctx, "docker run --rm -v "+drainAppPrefix+"-out:/out curlimages/curl:latest sh -c 'cat /out/"+name+" 2>/dev/null || true'"))
	}
	httpCode, body, exit := readOut("neg.code"), readOut("neg.body"), readOut("neg.exit")
	if httpCode == "200" && body == "blue" && exit == "0" {
		t.Fatalf("negative control broken: the request survived a no-grace kill (code=%s body=%s exit=%s) — the fixture cannot demonstrate the drain promise", httpCode, body, exit)
	}
	t.Logf("negative control PASS: without drain/grace the in-flight request broke (code=%s body=%q exit=%s)", httpCode, body, exit)
}

func mustOut(t *testing.T, exec ssh.Executor, ctx context.Context, cmd string) string {
	t.Helper()
	out, err := exec.Run(ctx, cmd)
	if err != nil {
		t.Fatalf("%s: %v", cmd, err)
	}
	return out
}
