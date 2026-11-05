package domains

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/useteploy/teploy/internal/dns"
	"github.com/useteploy/teploy/internal/ssh"
)

var checkNow = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

func ts(t time.Time) string { return fmt.Sprintf("%.3f", float64(t.UnixMilli())/1000) }

func TestParseObtainOutput(t *testing.T) {
	window := 14 * 24 * time.Hour
	oldLine := fmt.Sprintf(`{"level":"info","ts":%s,"msg":"start"}`, ts(checkNow.Add(-30*24*time.Hour)))
	retry := func(at time.Time, id string, elapsed float64) string {
		return fmt.Sprintf(`{"level":"error","ts":%s,"logger":"tls.obtain","msg":"will retry","error":"[%s] Obtain: acme failed","attempt":3,"elapsed":%f}`, ts(at), id, elapsed)
	}
	issuer := func(at time.Time, id string) string {
		return fmt.Sprintf(`{"level":"error","ts":%s,"logger":"tls.obtain","msg":"could not get certificate from issuer","identifier":"%s","error":"[%s] acme failed"}`, ts(at), id, id)
	}
	frame := func(first string, lines ...string) string {
		return "TEPLOY_CADDY_RUNNING=true\nTEPLOY_LOG_FIRST " + first + "\n" + strings.Join(lines, "\n") + "\nTEPLOY_LOG_RC=0\n"
	}

	t.Run("failing identifier carries how long it has failed", func(t *testing.T) {
		at := checkNow.Add(-time.Hour)
		res := parseObtainOutput(frame(oldLine, issuer(at, "bad.dev"), retry(at, "bad.dev", 3*86400)), checkNow, window)
		s := res.stats["bad.dev"]
		if s == nil || !s.Failing || s.Failures != 1 {
			t.Fatalf("stats = %+v", s)
		}
		want := at.Add(-3 * 24 * time.Hour)
		if s.FailingSince.Sub(want).Abs() > time.Second {
			t.Errorf("failing since %v, want about %v", s.FailingSince, want)
		}
		if res.logs != LogsOK || res.coverageSeconds != int64(window/time.Second) {
			t.Errorf("logs=%s coverage=%d", res.logs, res.coverageSeconds)
		}
	})
	t.Run("a later success clears the streak", func(t *testing.T) {
		at := checkNow.Add(-2 * time.Hour)
		good := fmt.Sprintf(`{"level":"info","ts":%s,"logger":"tls.obtain","msg":"certificate obtained successfully","identifier":"ok.dev"}`, ts(at.Add(time.Hour)))
		res := parseObtainOutput(frame(oldLine, retry(at, "ok.dev", 60), good), checkNow, window)
		s := res.stats["ok.dev"]
		if s.Failing || s.Failures != 1 || s.LastSuccessAt.IsZero() || !s.FailingSince.IsZero() {
			t.Errorf("stats = %+v", s)
		}
	})
	t.Run("short logs", func(t *testing.T) {
		recent := fmt.Sprintf(`{"level":"info","ts":%s,"msg":"start"}`, ts(checkNow.Add(-10*time.Minute)))
		if res := parseObtainOutput(frame(recent), checkNow, window); res.logs != LogsShort {
			t.Errorf("logs = %s, want short", res.logs)
		}
	})
	t.Run("empty logs are short, not clean", func(t *testing.T) {
		if res := parseObtainOutput(frame(""), checkNow, window); res.logs != LogsShort {
			t.Errorf("logs = %s", res.logs)
		}
	})
	t.Run("an interrupted read is unavailable", func(t *testing.T) {
		out := "TEPLOY_CADDY_RUNNING=true\nTEPLOY_LOG_FIRST " + oldLine + "\nTEPLOY_LOG_RC=124\n"
		res := parseObtainOutput(out, checkNow, window)
		if res.logs != LogsUnavailable || res.problem == "" {
			t.Errorf("logs=%s problem=%q", res.logs, res.problem)
		}
	})
	t.Run("console log format cannot read as zero failures", func(t *testing.T) {
		res := parseObtainOutput(frame("2026/11/04 INFO starting"), checkNow, window)
		if res.logs != LogsUnavailable {
			t.Errorf("logs = %s", res.logs)
		}
	})
	t.Run("no caddy", func(t *testing.T) {
		res := parseObtainOutput("TEPLOY_CADDY_ABSENT\n", checkNow, window)
		if res.container != "absent" || res.logs != LogsNoCaddy {
			t.Errorf("%+v", res)
		}
	})
	t.Run("stopped caddy", func(t *testing.T) {
		res := parseObtainOutput("TEPLOY_CADDY_RUNNING=false\nTEPLOY_LOG_FIRST "+oldLine+"\nTEPLOY_LOG_RC=0\n", checkNow, window)
		if res.container != "stopped" || res.logs != LogsOK {
			t.Errorf("%+v", res)
		}
	})
	t.Run("hostile log text is cleaned", func(t *testing.T) {
		line := `{"level":"error","ts":` + ts(checkNow) + `,"msg":"will retry","identifier":"x.dev","error":"failed at https://user:hunter2@acme.dev/x \u001b[31mred\u0007"}`
		res := parseObtainOutput(frame(oldLine, line), checkNow, window)
		got := res.stats["x.dev"].LastError
		if strings.Contains(got, "hunter2") || strings.ContainsAny(got, "\x1b\x07") {
			t.Errorf("stored error kept what it must not: %q", got)
		}
	})
	t.Run("truncation is surfaced", func(t *testing.T) {
		lines := make([]string, maxLogLines)
		for i := range lines {
			lines[i] = retry(checkNow, "x.dev", 1)
		}
		if res := parseObtainOutput(frame(oldLine, lines...), checkNow, window); !res.truncated {
			t.Error("truncated flag not set")
		}
	})
}

// fleetMock answers the commands one check runs. The Caddyfile and state files
// go through the mock's framed reads.
func fleetMock(caddyfile, logs string, apps map[string]string, resolves string, extra ...ssh.MockCommand) *ssh.MockExecutor {
	var names []string
	for name := range apps {
		names = append(names, name)
	}
	cmds := append(extra,
		ssh.MockCommand{Match: "for f in /deployments/*/state.json", Output: strings.Join(names, "\n")},
		ssh.MockCommand{Match: "for app in", Output: snapshotOutput(apps)},
		ssh.MockCommand{Match: "hostname -I", Output: "192.0.2.10 100.64.0.5 172.17.0.1"},
		ssh.MockCommand{Match: "curl -4", Output: ""},
		ssh.MockCommand{Match: "o=$(docker container inspect", Output: logs},
		ssh.MockCommand{Match: "printf '%s\\n'", Output: resolves},
	)
	m := ssh.NewMockExecutor("100.64.0.5", cmds...)
	m.Files["/deployments/caddy/Caddyfile"] = []byte(caddyfile)
	return m
}

// snapshotOutput renders the framing state.ReadAppSnapshots parses.
func snapshotOutput(apps map[string]string) string {
	var b strings.Builder
	for name, st := range apps {
		fmt.Fprintf(&b, "A %s\nS 0 %s\nG absent\nL \nM 0\nR 0\n", name, base64.StdEncoding.EncodeToString([]byte(st)))
	}
	return b.String()
}

func appState(ingress, domain string) string {
	return fmt.Sprintf(`{"schema_version":2,"deployment_type":"container","ingress_mode":%q,"domain":%q,"updated_at":"2026-11-01T00:00:00Z","current_hash":"v1","generation":2}`, ingress, domain)
}

func logsOK(lines ...string) string {
	first := fmt.Sprintf(`{"level":"info","ts":%s,"msg":"start"}`, ts(checkNow.Add(-30*24*time.Hour)))
	return "TEPLOY_CADDY_RUNNING=true\nTEPLOY_LOG_FIRST " + first + "\n" + strings.Join(append(lines, "TEPLOY_LOG_RC=0"), "\n") + "\n"
}

func staticDNS(answers map[string]dns.PublicAnswer) *DNSCache {
	return NewDNSCacheWith(func(ctx context.Context, host string) dns.PublicAnswer {
		if a, ok := answers[host]; ok {
			return a
		}
		return dns.PublicAnswer{Answers: []string{"192.0.2.10"}}
	})
}

func runCheck(t *testing.T, m *ssh.MockExecutor, cache *DNSCache, opts Options) (Server, map[string]Domain) {
	t.Helper()
	opts.Now = func() time.Time { return checkNow }
	srv, rows := CheckServer(context.Background(), m, "s1", opts, cache)
	byHost := map[string]Domain{}
	for _, r := range rows {
		byHost[r.Host] = r
	}
	return srv, byHost
}

func TestCheckServer_JoinsTheLayers(t *testing.T) {
	caddyfile := `# TEPLOY BEGIN blog
blog.dev, www.blog.dev {
	reverse_proxy blog-web-v1:3000
}
# TEPLOY END blog

# TEPLOY BEGIN dash
dash.example.com {
	reverse_proxy dash-web-old:3456
}
# TEPLOY END dash

# TEPLOY BEGIN gone
gone.dev {
	reverse_proxy gone-web-v1:80
}
# TEPLOY END gone

hand.dev {
	reverse_proxy 127.0.0.1:9000
}

http://plain.dev {
	reverse_proxy blog-web-v1:3000
}
`
	failing := fmt.Sprintf(`{"level":"error","ts":%s,"logger":"tls.obtain","msg":"will retry","error":"[dash.example.com] Obtain: no such host","elapsed":864000}`, ts(checkNow.Add(-time.Hour)))
	m := fleetMock(caddyfile, logsOK(failing), map[string]string{
		"blog":  appState("caddy", "blog.dev, www.blog.dev"),
		"dash":  appState("host", "dash.example.com"),
		"front": appState("external", "front.dev"),
		"label": appState("host", "label.dev"),
	}, "0 blog-web-v1\n2 dash-web-old\n2 gone-web-v1\n")
	cache := staticDNS(map[string]dns.PublicAnswer{
		"www.blog.dev": {Answers: []string{"104.16.0.1"}},
		"hand.dev":     {NotFound: true},
		"gone.dev":     {Answers: []string{"198.51.100.9"}},
		"plain.dev":    {Err: fmt.Errorf("timeout")},
	})
	srv, rows := runCheck(t, m, cache, Options{})

	if srv.State != ServerOK || !srv.PublicAddressKnown || srv.Caddy.Logs != LogsOK {
		t.Fatalf("server = %+v", srv)
	}
	check := func(host string, f func(Domain)) {
		t.Helper()
		r, ok := rows[host]
		if !ok {
			t.Fatalf("no row for %s", host)
		}
		f(r)
	}
	check("blog.dev", func(r Domain) {
		if r.App != "blog" || r.DNS.Status != dns.StatusOK || r.Route.Kind != RouteManaged || r.Route.UpstreamResolves != ResolvesYes ||
			r.TLS.Status != TLSOK || r.TLS.Failures == nil || *r.TLS.Failures != 0 {
			t.Errorf("%+v", r)
		}
	})
	check("www.blog.dev", func(r Domain) {
		if r.DNS.Status != dns.StatusProxied {
			t.Errorf("%+v", r.DNS)
		}
	})
	check("dash.example.com", func(r Domain) {
		if r.Route.Kind != RouteStaleManaged || r.Route.Owner != "dash" || r.Route.UpstreamResolves != ResolvesNo {
			t.Errorf("route %+v", r.Route)
		}
		if r.Skip != SkipReservedName || r.DNS.Status != dns.StatusSkipped {
			t.Errorf("a reserved name is not looked up: skip=%s dns=%+v", r.Skip, r.DNS)
		}
		if r.TLS.Status != TLSFailing || r.TLS.FailingSince == nil || *r.TLS.Failures != 1 {
			t.Errorf("tls %+v", r.TLS)
		}
	})
	check("gone.dev", func(r Domain) {
		if r.Route.Kind != RouteOrphanManaged || r.DNS.Status != dns.StatusWrongTarget {
			t.Errorf("%+v", r)
		}
	})
	check("hand.dev", func(r Domain) {
		if r.App != "" || r.Route.Kind != RouteManual || r.DNS.Status != dns.StatusNXDomain || r.Route.UpstreamResolves != ResolvesYes {
			t.Errorf("%+v", r)
		}
	})
	check("plain.dev", func(r Domain) {
		if r.TLS.Status != TLSNotApplicable || r.DNS.Status != dns.StatusUnknown || r.Route.TLS != "http_only" {
			t.Errorf("%+v", r)
		}
	})
	check("front.dev", func(r Domain) {
		if r.Skip != SkipExternalLabel || r.DNS.Status != dns.StatusSkipped || r.Route.Kind != RouteNone {
			t.Errorf("%+v", r)
		}
	})
	check("label.dev", func(r Domain) {
		if r.Skip != SkipHostLabel || r.Route.Kind != RouteNone || r.Ingress != "host" {
			t.Errorf("%+v", r)
		}
	})
}

func TestCheckServer_NeverReadsHealthyWithoutEvidence(t *testing.T) {
	caddyfile := "blog.dev {\n\treverse_proxy blog-web-v1:3000\n}\n"
	t.Run("logs unreadable: certificate unknown, not zero", func(t *testing.T) {
		m := fleetMock(caddyfile, "", map[string]string{}, "0 blog-web-v1\n",
			ssh.MockCommand{Match: "o=$(docker container inspect", Err: fmt.Errorf("connection reset")})
		srv, rows := runCheck(t, m, staticDNS(nil), Options{})
		r := rows["blog.dev"]
		if r.TLS.Status != TLSUnknown || r.TLS.Failures != nil {
			t.Errorf("tls = %+v", r.TLS)
		}
		if srv.State != ServerPartial || len(srv.Issues) == 0 {
			t.Errorf("server = %+v", srv)
		}
	})
	t.Run("short logs: certificate unknown, not zero", func(t *testing.T) {
		recent := fmt.Sprintf(`{"level":"info","ts":%s,"msg":"start"}`, ts(checkNow.Add(-5*time.Minute)))
		logs := "TEPLOY_CADDY_RUNNING=true\nTEPLOY_LOG_FIRST " + recent + "\nTEPLOY_LOG_RC=0\n"
		_, rows := runCheck(t, fleetMock(caddyfile, logs, map[string]string{}, "0 blog-web-v1\n"), staticDNS(nil), Options{})
		if r := rows["blog.dev"]; r.TLS.Status != TLSUnknown || r.TLS.Failures != nil {
			t.Errorf("tls = %+v", r.TLS)
		}
	})
	t.Run("short logs with a failure still report it", func(t *testing.T) {
		recent := fmt.Sprintf(`{"level":"info","ts":%s,"msg":"start"}`, ts(checkNow.Add(-5*time.Minute)))
		bad := fmt.Sprintf(`{"level":"error","ts":%s,"msg":"will retry","error":"[blog.dev] Obtain: x"}`, ts(checkNow))
		logs := "TEPLOY_CADDY_RUNNING=true\nTEPLOY_LOG_FIRST " + recent + "\n" + bad + "\nTEPLOY_LOG_RC=0\n"
		_, rows := runCheck(t, fleetMock(caddyfile, logs, map[string]string{}, "0 blog-web-v1\n"), staticDNS(nil), Options{})
		if r := rows["blog.dev"]; r.TLS.Status != TLSFailing {
			t.Errorf("tls = %+v", r.TLS)
		}
	})
	t.Run("upstream check failed: unknown, not resolving", func(t *testing.T) {
		m := fleetMock(caddyfile, logsOK(), map[string]string{}, "", ssh.MockCommand{Match: "printf '%s\\n'", Err: fmt.Errorf("exec failed")})
		srv, rows := runCheck(t, m, staticDNS(nil), Options{})
		if r := rows["blog.dev"]; r.Route.UpstreamResolves != ResolvesUnknown {
			t.Errorf("route = %+v", r.Route)
		}
		if srv.State != ServerPartial {
			t.Errorf("state = %s", srv.State)
		}
	})
	t.Run("unparseable Caddyfile: route unknown, app rows stay", func(t *testing.T) {
		m := fleetMock("import sites/*\n", logsOK(), map[string]string{"blog": appState("caddy", "blog.dev")}, "")
		srv, rows := runCheck(t, m, staticDNS(nil), Options{})
		if r := rows["blog.dev"]; r.Route.Kind != RouteUnknown || r.Route.UpstreamResolves != ResolvesUnknown {
			t.Errorf("route = %+v", r.Route)
		}
		if srv.Caddy.Caddyfile != "unparseable" || srv.State != ServerPartial {
			t.Errorf("server = %+v", srv)
		}
	})
	t.Run("no public address known: a foreign answer is unknown", func(t *testing.T) {
		m := ssh.NewMockExecutor("100.64.0.5",
			ssh.MockCommand{Match: "for f in /deployments/*/state.json", Output: "blog"},
			ssh.MockCommand{Match: "for app in", Output: snapshotOutput(map[string]string{"blog": appState("caddy", "blog.dev")})},
			ssh.MockCommand{Match: "hostname -I", Output: "192.168.1.5"},
			ssh.MockCommand{Match: "curl -4", Output: ""},
			ssh.MockCommand{Match: "o=$(docker container inspect", Output: logsOK()},
		)
		srv, rows := runCheck(t, m, staticDNS(map[string]dns.PublicAnswer{"blog.dev": {Answers: []string{"198.51.100.9"}}}), Options{})
		if srv.PublicAddressKnown {
			t.Error("no public address here")
		}
		if r := rows["blog.dev"]; r.DNS.Status != dns.StatusUnknown {
			t.Errorf("dns = %+v", r.DNS)
		}
	})
}

// Hostile strings from a state file or a Caddyfile reach a resolver or a shell
// only after validation, and always quoted.
func TestCheckServer_HostileNames(t *testing.T) {
	evil := "$(touch /tmp/pwned).dev"
	caddyfile := "# TEPLOY BEGIN blog\nblog.dev {\n\treverse_proxy 'x;rm -rf /':80\n\treverse_proxy {$UPSTREAM}:80\n\treverse_proxy good-name:80\n}\n# TEPLOY END blog\n"
	m := fleetMock(caddyfile, logsOK(), map[string]string{"blog": appState("caddy", "blog.dev, "+evil)}, "0 good-name\n")
	looked := map[string]bool{}
	cache := NewDNSCacheWith(func(ctx context.Context, host string) dns.PublicAnswer {
		looked[host] = true
		return dns.PublicAnswer{Answers: []string{"192.0.2.10"}}
	})
	_, rows := runCheck(t, m, cache, Options{})
	if looked[evil] {
		t.Error("an invalid name went to the resolver")
	}
	if r := rows[evil]; r.Skip != SkipInvalidName || r.DNS.Status != dns.StatusSkipped {
		t.Errorf("row = %+v", r)
	}
	for _, c := range m.Calls {
		if strings.Contains(c, "pwned") || strings.Contains(c, "rm -rf") || strings.Contains(c, "UPSTREAM") {
			t.Errorf("a hostile string reached a command: %s", c)
		}
	}
	if r := rows["blog.dev"]; r.Route.UpstreamResolves != ResolvesUnknown {
		t.Errorf("an uncheckable upstream cannot read as resolving: %+v", r.Route)
	}
	var quoted bool
	for _, c := range m.Calls {
		if strings.HasPrefix(c, "printf '%s\\n' ") && strings.Contains(c, "'good-name'") {
			quoted = true
		}
	}
	if !quoted {
		t.Error("upstream names must be shell-quoted")
	}
}

func TestCheckServer_AppFilterKeepsOwnedRows(t *testing.T) {
	caddyfile := "# TEPLOY BEGIN blog\nblog.dev {\n\treverse_proxy a:1\n}\n# TEPLOY END blog\n\nhand.dev {\n\trespond ok\n}\n"
	m := fleetMock(caddyfile, logsOK(), map[string]string{"blog": appState("caddy", "blog.dev"), "other": appState("caddy", "other.dev")}, "0 a\n")
	_, rows := runCheck(t, m, staticDNS(nil), Options{App: "blog"})
	if len(rows) != 1 || rows["blog.dev"].App != "blog" {
		t.Errorf("rows = %+v", rows)
	}
}

// One name on several servers is asked once; a stuck lookup ends with the DNS
// budget and reads unknown.
func TestDNSCache_SharesAndBounds(t *testing.T) {
	var calls atomic.Int32
	cache := NewDNSCacheWith(func(ctx context.Context, host string) dns.PublicAnswer {
		calls.Add(1)
		return dns.PublicAnswer{Answers: []string{"192.0.2.10"}}
	})
	for i := 0; i < 3; i++ {
		cache.get(context.Background(), "a.dev")
	}
	if calls.Load() != 1 {
		t.Errorf("asked %d times", calls.Load())
	}

	stuck := NewDNSCacheWith(func(ctx context.Context, host string) dns.PublicAnswer {
		<-ctx.Done()
		return dns.PublicAnswer{Err: ctx.Err()}
	})
	m := fleetMock("blog.dev {\n\trespond ok\n}\n", logsOK(), map[string]string{}, "")
	start := time.Now()
	_, rows := runCheck(t, m, stuck, Options{DNSBudget: 50 * time.Millisecond})
	if time.Since(start) > 5*time.Second {
		t.Fatal("a stuck resolver stalled the check")
	}
	if rows["blog.dev"].DNS.Status != dns.StatusUnknown {
		t.Errorf("dns = %+v", rows["blog.dev"].DNS)
	}
}

func TestLogCommand_IsBoundedAndConstant(t *testing.T) {
	cmd := logCommand(14 * 24 * time.Hour)
	for _, want := range []string{"--since 1209600s", "timeout 30", "tail -n 2000", "docker container inspect", "TEPLOY_LOG_RC"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("command lacks %q: %s", want, cmd)
		}
	}
}

// The envelope's null is the contract: never 0 when the count is unknown.
func TestTLSResult_UnknownFailuresEncodeAsNull(t *testing.T) {
	raw, err := json.Marshal(TLSResult{Status: TLSUnknown})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"failures":null`) {
		t.Errorf("got %s", raw)
	}
}
