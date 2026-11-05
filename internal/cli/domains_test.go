package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy/internal/dns"
	"github.com/useteploy/teploy/internal/domains"
	"github.com/useteploy/teploy/internal/ssh"
)

var domainsFixtureNow = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

// domainsFixtureExecutor models one server with one of each finding.
func domainsFixtureExecutor() *ssh.MockExecutor {
	frame := func(name, st string) string {
		return fmt.Sprintf("A %s\nS 0 %s\nG absent\nL \nM 0\nR 0\n", name, base64.StdEncoding.EncodeToString([]byte(st)))
	}
	state := func(ingress, domain string) string {
		return fmt.Sprintf(`{"schema_version":2,"deployment_type":"container","ingress_mode":%q,"domain":%q,"updated_at":"2026-11-01T00:00:00Z","current_hash":"v1","generation":2}`, ingress, domain)
	}
	first := fmt.Sprintf(`{"level":"info","ts":%d,"msg":"start"}`, domainsFixtureNow.Add(-30*24*time.Hour).Unix())
	failing := fmt.Sprintf(`{"level":"error","ts":%d,"logger":"tls.obtain","msg":"will retry","error":"[dash.example.com] Obtain: no solvers","elapsed":864000}`, domainsFixtureNow.Add(-time.Hour).Unix())
	m := ssh.NewMockExecutor("192.0.2.10",
		ssh.MockCommand{Match: "for f in /deployments/*/state.json", Output: "blog\ndash\nfront\n"},
		ssh.MockCommand{Match: "for app in", Output: frame("blog", state("caddy", "blog.dev, www.blog.dev")) + frame("dash", state("host", "dash.example.com")) + frame("front", state("external", "front.dev"))},
		ssh.MockCommand{Match: "hostname -I", Output: "192.0.2.10 172.17.0.1"},
		ssh.MockCommand{Match: "curl -4", Output: "ip=192.0.2.10\n"},
		ssh.MockCommand{Match: "o=$(docker container inspect", Output: "TEPLOY_CADDY_RUNNING=true\nTEPLOY_LOG_FIRST " + first + "\n" + failing + "\nTEPLOY_LOG_RC=0\n"},
		ssh.MockCommand{Match: "printf '%s\\n'", Output: "0 blog-web-v1\n2 dash-web-old\n"},
	)
	m.Files["/deployments/caddy/Caddyfile"] = []byte("# TEPLOY BEGIN blog\nblog.dev, www.blog.dev {\n\treverse_proxy blog-web-v1:3000\n}\n# TEPLOY END blog\n\n# TEPLOY BEGIN dash\ndash.example.com {\n\treverse_proxy dash-web-old:3456\n}\n# TEPLOY END dash\n\nhand.dev {\n\treverse_proxy 127.0.0.1:9000\n}\n")
	return m
}

func domainsFixtureEnvelope(t *testing.T) domains.Envelope {
	t.Helper()
	cache := domains.NewDNSCacheWith(func(ctx context.Context, host string) dns.PublicAnswer {
		switch host {
		case "www.blog.dev":
			return dns.PublicAnswer{Answers: []string{"104.16.0.1"}}
		case "hand.dev":
			return dns.PublicAnswer{NotFound: true}
		}
		return dns.PublicAnswer{Answers: []string{"192.0.2.10"}}
	})
	targets := []domainsTarget{{name: "down", host: "198.51.100.1"}, {name: "edge", host: "192.0.2.10"}}
	connect := func(ctx context.Context, t domainsTarget) (ssh.Executor, error) {
		if t.name == "down" {
			return nil, errors.New("dial tcp 198.51.100.1:22: i/o timeout")
		}
		return domainsFixtureExecutor(), nil
	}
	opts := domains.Options{Now: func() time.Time { return domainsFixtureNow }}
	return sweepDomains(context.Background(), targets, connect, opts, cache, domainsFixtureNow)
}

func TestContractsDomainsCheckGolden(t *testing.T) {
	writeFixture(t, "domains-check-envelope/valid/sweep.json", domainsFixtureEnvelope(t))
}

func TestSweepDomains_AnUnreachableServerIsReportedNotHealthy(t *testing.T) {
	env := domainsFixtureEnvelope(t)
	var down *domains.Server
	for i := range env.Servers {
		if env.Servers[i].Server == "down" {
			down = &env.Servers[i]
		}
	}
	if down == nil || down.State != domains.ServerUnreachable || down.Error == "" {
		t.Fatalf("server missing or healthy: %+v", down)
	}
	for _, d := range env.Domains {
		if d.Server == "down" {
			t.Errorf("rows must not be invented for an unreachable server: %+v", d)
		}
	}
	var buf bytes.Buffer
	if err := printDomains(&buf, env); err == nil {
		t.Error("the human form must exit non-zero when a server could not be checked")
	}
	if !strings.Contains(buf.String(), "could not be checked") {
		t.Errorf("output: %s", buf.String())
	}
}

// A dead server costs its own budget, not the sweep's: the others finish.
func TestSweepDomains_SlowServerDoesNotStallOthers(t *testing.T) {
	cache := domains.NewDNSCacheWith(func(ctx context.Context, host string) dns.PublicAnswer {
		return dns.PublicAnswer{Answers: []string{"192.0.2.10"}}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	connect := func(ctx context.Context, t domainsTarget) (ssh.Executor, error) {
		if t.name == "slow" {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return domainsFixtureExecutor(), nil
	}
	start := time.Now()
	env := sweepDomains(ctx, []domainsTarget{{name: "slow", host: "a"}, {name: "edge", host: "b"}}, connect, domains.Options{}, cache, domainsFixtureNow)
	if time.Since(start) > 5*time.Second {
		t.Fatal("the sweep ignored its deadline")
	}
	if env.Servers[0].State != domains.ServerUnreachable {
		t.Errorf("slow = %+v", env.Servers[0])
	}
	if env.Servers[1].State == domains.ServerUnreachable || len(env.Domains) == 0 {
		t.Errorf("the healthy server's result was lost: %+v", env.Servers[1])
	}
}

func TestDomainsCheck_RejectsBadInput(t *testing.T) {
	flags := &Flags{Host: "192.0.2.10"}
	var buf bytes.Buffer
	if err := runDomainsCheck(context.Background(), flags, &buf, "", "../etc; rm", time.Hour); err == nil {
		t.Error("a hostile --app must be refused before anything runs")
	}
	if err := runDomainsCheck(context.Background(), flags, &buf, "", "", -time.Hour); err == nil {
		t.Error("a negative window must be refused")
	}
}
