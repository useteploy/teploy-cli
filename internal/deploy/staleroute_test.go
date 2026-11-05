package deploy

// A release that stops being caddy-routed retires the managed block its
// predecessor left (plan U5): after the state commit, under the fence and an
// exact-block CAS. These run the real Deploy and Rollback over the mock, which
// evaluates the guards and the CAS against recorded file state.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy/internal/releasemeta"
	"github.com/useteploy/teploy/internal/ssh"
	"github.com/useteploy/teploy/internal/state"
)

const (
	staleWebBlock = "# TEPLOY BEGIN web\n# TEPLOY GENERATION 3\nweb.teploy-fixture.dev {\n\treverse_proxy web-web-old123:3000\n}\n# TEPLOY END web"
	otherBlock    = "# TEPLOY BEGIN other\nother.teploy-fixture.dev {\n\treverse_proxy other-web-a:3000\n}\n# TEPLOY END other"
)

func staleRouteMock(extra ...ssh.MockCommand) *ssh.MockExecutor {
	cmds := append(extra,
		ssh.MockCommand{Match: "mkdir -p /deployments/web", Output: ""},
		ssh.MockCommand{Match: "mkdir /deployments/web/.lock", Output: ""},
		ssh.MockCommand{Match: "if [ ! -e '/deployments/web/state.json' ]", Output: "absent"},
		ssh.MockCommand{Match: "if [ ! -e '/deployments/web/state' ]", Output: "present\ncurrent_port=49152\ncurrent_hash=old123\n"},
		ssh.MockCommand{Match: "docker ps --filter label=teploy.app=web", Output: ""},
		ssh.MockCommand{Match: "ss -tln", Output: ""},
		ssh.MockCommand{Match: "docker run", Output: "container-id"},
		ssh.MockCommand{Match: "docker inspect -f '{{.State.Status}}'", Output: "running"},
		ssh.MockCommand{Match: "docker stop", Output: ""},
		ssh.MockCommand{Match: "docker rm", Output: ""},
		ssh.MockCommand{Match: "curl -s -o /dev/null", Output: "200"},
		ssh.MockCommand{Match: "docker exec caddy caddy reload", Output: ""},
		ssh.MockCommand{Match: "a=$(docker exec caddy md5sum", Output: "TEPLOY_CADDY_OK"},
		ssh.MockCommand{Match: "mkdir /deployments/caddy/.lock", Output: ""},
		ssh.MockCommand{Match: "printf %s", Output: ""},
		ssh.MockCommand{Match: "rm -rf /deployments/web/.lock", Output: ""},
	)
	m := ssh.NewMockExecutor("1.2.3.4", cmds...)
	m.Files["/deployments/caddy/Caddyfile"] = []byte(staleWebBlock + "\n\n" + otherBlock + "\n")
	return m
}

func staleRouteDeployConfig() Config {
	return Config{App: "web", Image: "web:new", Version: "new456", Ingress: "host", ContainerPort: 3000,
		Health: HealthConfig{Timeout: 5 * time.Second, Interval: 10 * time.Millisecond}}
}

func TestDeploy_NonCaddyRetiresStaleBlockAfterCommit(t *testing.T) {
	for _, ingress := range []string{"host", "external"} {
		t.Run(ingress, func(t *testing.T) {
			m := staleRouteMock()
			cfg := staleRouteDeployConfig()
			cfg.Ingress = ingress
			if ingress == "external" {
				cfg.Domain = "web.teploy-fixture.dev"
			}
			var out bytes.Buffer
			if err := NewDeployer(m, &out).Deploy(context.Background(), cfg); err != nil {
				t.Fatalf("Deploy: %v\n%s", err, out.String())
			}
			caddyfile := string(m.Files["/deployments/caddy/Caddyfile"])
			if strings.Contains(caddyfile, "TEPLOY BEGIN web") {
				t.Errorf("stale block survived:\n%s", caddyfile)
			}
			if !strings.Contains(caddyfile, "TEPLOY BEGIN other") {
				t.Errorf("another app's block was removed:\n%s", caddyfile)
			}
			if !strings.Contains(out.String(), "Removing the stale Caddy route") {
				t.Errorf("removal not announced:\n%s", out.String())
			}
			// Order and composition: the state commit lands first, and the edit
			// runs under the deploy's fence plus the CAS on the exact block.
			commit, removal := -1, -1
			for i, c := range m.Calls {
				if commit < 0 && strings.Contains(c, "state.json.tmp-") {
					commit = i
				}
				if strings.Contains(c, "/deployments/caddy/Caddyfile") && strings.Contains(c, "mv ") {
					removal = i
				}
			}
			if commit < 0 || removal < 0 || commit > removal {
				t.Errorf("state commit (idx %d) must precede the guarded removal (idx %d)", commit, removal)
			}
		})
	}
}

func TestDeploy_NoBlockMakesNoCaddyEdit(t *testing.T) {
	m := staleRouteMock()
	m.Files["/deployments/caddy/Caddyfile"] = []byte(otherBlock + "\n")
	var out bytes.Buffer
	if err := NewDeployer(m, &out).Deploy(context.Background(), staleRouteDeployConfig()); err != nil {
		t.Fatalf("Deploy: %v\n%s", err, out.String())
	}
	for _, c := range m.Calls {
		if strings.Contains(c, "caddy reload") {
			t.Errorf("no block to remove, but Caddy was reloaded: %s", c)
		}
	}
}

func TestDeploy_NoCaddyfileIsNotAProblem(t *testing.T) {
	m := staleRouteMock()
	delete(m.Files, "/deployments/caddy/Caddyfile")
	var out bytes.Buffer
	if err := NewDeployer(m, &out).Deploy(context.Background(), staleRouteDeployConfig()); err != nil {
		t.Fatalf("Deploy: %v\n%s", err, out.String())
	}
	if strings.Contains(out.String(), "stale Caddy route") {
		t.Errorf("a server without a Caddyfile warned about routes:\n%s", out.String())
	}
}

func TestDeploy_FailureLeavesStaleBlockIntact(t *testing.T) {
	cases := map[string]ssh.MockCommand{
		"state commit fails": {Match: "UPLOAD:/deployments/web/state.json.tmp-", Err: fmt.Errorf("disk full")},
		"health check fails": {Match: "curl -s -o /dev/null", Output: "500"},
	}
	for name, failing := range cases {
		t.Run(name, func(t *testing.T) {
			m := staleRouteMock(failing)
			before := string(m.Files["/deployments/caddy/Caddyfile"])
			cfg := staleRouteDeployConfig()
			cfg.Health = HealthConfig{Timeout: 50 * time.Millisecond, Interval: 10 * time.Millisecond}
			var out bytes.Buffer
			if err := NewDeployer(m, &out).Deploy(context.Background(), cfg); err == nil {
				t.Fatalf("expected the deploy to fail\n%s", out.String())
			}
			if got := string(m.Files["/deployments/caddy/Caddyfile"]); got != before {
				t.Errorf("a failed deploy changed the route:\n%s", got)
			}
		})
	}
}

// swapOnCommand lands a successor's edit of the block between the deploy's
// resolution and its removal.
type swapOnCommand struct {
	*ssh.MockExecutor
	trigger string
	fired   bool
	content string
}

func (s *swapOnCommand) Run(ctx context.Context, cmd string) (string, error) {
	if !s.fired && strings.Contains(cmd, s.trigger) {
		s.fired = true
		s.MockExecutor.Files["/deployments/caddy/Caddyfile"] = []byte(s.content)
	}
	return s.MockExecutor.Run(ctx, cmd)
}

func TestDeploy_StaleRemovalCASRefusesAChangedBlock(t *testing.T) {
	successor := strings.Replace(staleWebBlock, "GENERATION 3", "GENERATION 9", 1)
	m := staleRouteMock()
	swap := &swapOnCommand{MockExecutor: m, trigger: "docker run", content: successor + "\n"}
	var out bytes.Buffer
	// The deploy itself stands: traffic is committed, the route edit is cleanup.
	if err := NewDeployer(swap, &out).Deploy(context.Background(), staleRouteDeployConfig()); err != nil {
		t.Fatalf("Deploy: %v\n%s", err, out.String())
	}
	if !swap.fired {
		t.Fatal("the swap never ran")
	}
	if got := string(m.Files["/deployments/caddy/Caddyfile"]); !strings.Contains(got, "GENERATION 9") {
		t.Errorf("the changed block was removed anyway:\n%s", got)
	}
	if !strings.Contains(out.String(), "was not removed") {
		t.Errorf("refusal not reported:\n%s", out.String())
	}
}

// --- rollback, both directions ---

const rollbackCaddyBlock = "# TEPLOY BEGIN myapp\n# TEPLOY GENERATION 7\nmyapp.com {\n\treverse_proxy myapp-web-v2:3000\n}\n# TEPLOY END myapp"

func hostRecord(t *testing.T) string {
	t.Helper()
	return marshalRecord(t, &releasemeta.Record{DeploymentType: "container", IngressMode: "host",
		Health: &releasemeta.Health{Path: "/health", TimeoutSeconds: 30, IntervalSeconds: 1},
		Ports:  []releasemeta.Port{{HostPort: 49152, ContainerPort: 3000, Primary: true, Fixed: true}}})
}

func caddyRecord(t *testing.T) string {
	t.Helper()
	return marshalRecord(t, &releasemeta.Record{DeploymentType: "container", IngressMode: "caddy", Domain: "myapp.com",
		Health: &releasemeta.Health{Path: "/health", TimeoutSeconds: 30, IntervalSeconds: 1},
		Ports:  []releasemeta.Port{{HostPort: 49152, ContainerPort: 3000, Primary: true}}})
}

func stateWithIngress(ingress, domain string) string {
	return fmt.Sprintf(`{"schema_version":2,"deployment_type":"container","ingress_mode":%q,"domain":%q,"updated_at":"2026-07-22T10:00:00Z","current_port":49153,"current_hash":"v2","previous_port":49152,"previous_hash":"v1"}`, ingress, domain)
}

func readState(t *testing.T, m *ssh.MockExecutor) state.AppState {
	t.Helper()
	var st state.AppState
	if err := json.Unmarshal(m.Files["/deployments/myapp/state.json"], &st); err != nil {
		t.Fatalf("state: %v", err)
	}
	return st
}

func TestRollback_CaddyToHostRetiresBlock(t *testing.T) {
	m := ssh.NewMockExecutor("1.2.3.4", rollbackFixtureCommands(hostRecord(t))...)
	m.Files["/deployments/caddy/Caddyfile"] = []byte(rollbackCaddyBlock + "\n\n" + otherBlock + "\n")
	var out bytes.Buffer
	if err := Rollback(context.Background(), m, &out, rollbackCfg()); err != nil {
		t.Fatalf("Rollback: %v\n%s", err, out.String())
	}
	caddyfile := string(m.Files["/deployments/caddy/Caddyfile"])
	if strings.Contains(caddyfile, "TEPLOY BEGIN myapp") {
		t.Errorf("block survived the rollback to a host release:\n%s", caddyfile)
	}
	if !strings.Contains(caddyfile, "TEPLOY BEGIN other") {
		t.Errorf("another app's block was removed:\n%s", caddyfile)
	}
	st := readState(t, m)
	if st.IngressMode != "host" || st.Domain != "" {
		t.Errorf("state should name the target release: ingress=%q domain=%q", st.IngressMode, st.Domain)
	}
}

func TestRollback_CaddyToHostFailureLeavesBlock(t *testing.T) {
	before := rollbackCaddyBlock + "\n"
	cfg := rollbackCfg()
	var out bytes.Buffer
	shortRecord := marshalRecord(t, &releasemeta.Record{DeploymentType: "container", IngressMode: "host",
		Health: &releasemeta.Health{Path: "/health", TimeoutSeconds: 1, IntervalSeconds: 1},
		Ports:  []releasemeta.Port{{HostPort: 49152, ContainerPort: 3000, Primary: true, Fixed: true}}})
	m := ssh.NewMockExecutor("1.2.3.4",
		append([]ssh.MockCommand{{Match: "curl -s -o /dev/null", Output: "500"}}, rollbackFixtureCommands(shortRecord)...)...)
	m.Files["/deployments/caddy/Caddyfile"] = []byte(before)
	if err := Rollback(context.Background(), m, &out, cfg); err == nil {
		t.Fatalf("expected the rollback to fail\n%s", out.String())
	}
	if got := string(m.Files["/deployments/caddy/Caddyfile"]); got != before {
		t.Errorf("a failed rollback changed the route:\n%s", got)
	}
}

func hostCurrentCommands(record string) []ssh.MockCommand {
	return append([]ssh.MockCommand{
		{Match: "if [ ! -e '/deployments/myapp/state.json' ]", Output: "present\n" + stateWithIngress("host", "")},
	}, rollbackFixtureCommands(record)...)
}

func TestRollback_HostToCaddyWritesRoute(t *testing.T) {
	m := ssh.NewMockExecutor("1.2.3.4", hostCurrentCommands(caddyRecord(t))...)
	m.Files["/deployments/caddy/Caddyfile"] = []byte(otherBlock + "\n")
	var out bytes.Buffer
	if err := Rollback(context.Background(), m, &out, rollbackCfg()); err != nil {
		t.Fatalf("Rollback: %v\n%s", err, out.String())
	}
	caddyfile := string(m.Files["/deployments/caddy/Caddyfile"])
	if !strings.Contains(caddyfile, "TEPLOY BEGIN myapp") || !strings.Contains(caddyfile, "myapp-web-v1:3000") {
		t.Errorf("route for the caddy target missing:\n%s", caddyfile)
	}
	if st := readState(t, m); st.IngressMode != "caddy" || st.Domain != "myapp.com" {
		t.Errorf("state should name the target release: ingress=%q domain=%q", st.IngressMode, st.Domain)
	}
}

func TestRollback_HostToCaddyCommitFailureRemovesWhatItWrote(t *testing.T) {
	m := ssh.NewMockExecutor("1.2.3.4",
		append([]ssh.MockCommand{{Match: "UPLOAD:/deployments/myapp/state.json.tmp-", Err: errors.New("disk full")}}, hostCurrentCommands(caddyRecord(t))...)...)
	m.Files["/deployments/caddy/Caddyfile"] = []byte(otherBlock + "\n")
	var out bytes.Buffer
	if err := Rollback(context.Background(), m, &out, rollbackCfg()); err == nil {
		t.Fatalf("expected the commit to fail\n%s", out.String())
	}
	if caddyfile := string(m.Files["/deployments/caddy/Caddyfile"]); strings.Contains(caddyfile, "TEPLOY BEGIN myapp") {
		t.Errorf("the rolled-back-from host release has no route; the written block must go:\n%s", caddyfile)
	}
}

func TestRollback_RestoreKeepsHTTPOnly(t *testing.T) {
	httpOnly := true
	rec := marshalRecord(t, &releasemeta.Record{DeploymentType: "container", IngressMode: "caddy", Domain: "myapp.com",
		Health: &releasemeta.Health{Path: "/health", TimeoutSeconds: 30, IntervalSeconds: 1},
		Caddy:  &releasemeta.CaddyRoute{TLSHTTPOnly: &httpOnly},
		Ports:  []releasemeta.Port{{HostPort: 49152, ContainerPort: 3000, Primary: true}}})
	m := ssh.NewMockExecutor("1.2.3.4", rollbackFixtureCommands(rec)...)
	m.Files["/deployments/caddy/Caddyfile"] = []byte(rollbackCaddyBlock + "\n")
	var out bytes.Buffer
	if err := Rollback(context.Background(), m, &out, rollbackCfg()); err != nil {
		t.Fatalf("Rollback: %v\n%s", err, out.String())
	}
	if caddyfile := string(m.Files["/deployments/caddy/Caddyfile"]); !strings.Contains(caddyfile, "http://myapp.com") {
		t.Errorf("the recorded http_only setting was lost:\n%s", caddyfile)
	}
}

// The record wins both ways: a folder that is http_only now, rolling back to a
// release recorded as HTTPS, must not drop HTTPS. A record too old to carry the
// field keeps what the caller resolved.
func TestRollback_RecordCanTurnHTTPOnlyOff(t *testing.T) {
	off := false
	for _, tc := range []struct {
		name     string
		recorded *bool
		want     string
	}{
		{"recorded off", &off, "myapp.com {"},
		{"old record", nil, "http://myapp.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := marshalRecord(t, &releasemeta.Record{DeploymentType: "container", IngressMode: "caddy", Domain: "myapp.com",
				Health: &releasemeta.Health{Path: "/health", TimeoutSeconds: 30, IntervalSeconds: 1},
				Caddy:  &releasemeta.CaddyRoute{TLSHTTPOnly: tc.recorded},
				Ports:  []releasemeta.Port{{HostPort: 49152, ContainerPort: 3000, Primary: true}}})
			m := ssh.NewMockExecutor("1.2.3.4", rollbackFixtureCommands(rec)...)
			m.Files["/deployments/caddy/Caddyfile"] = []byte(rollbackCaddyBlock + "\n")
			cfg := rollbackCfg()
			cfg.TLSHTTPOnly = true
			var out bytes.Buffer
			if err := Rollback(context.Background(), m, &out, cfg); err != nil {
				t.Fatalf("Rollback: %v\n%s", err, out.String())
			}
			caddyfile := string(m.Files["/deployments/caddy/Caddyfile"])
			if !strings.Contains(caddyfile, tc.want) {
				t.Errorf("want %q in:\n%s", tc.want, caddyfile)
			}
		})
	}
}

// A refused removal after a committed rollback is degraded, on the record.
func TestRollback_RetireFailureRecordedAsDegraded(t *testing.T) {
	m := ssh.NewMockExecutor("1.2.3.4",
		append([]ssh.MockCommand{{Match: "docker exec caddy caddy reload", Err: errors.New("reload refused")}}, rollbackFixtureCommands(hostRecord(t))...)...)
	m.Files["/deployments/caddy/Caddyfile"] = []byte(rollbackCaddyBlock + "\n")
	var out bytes.Buffer
	if err := Rollback(context.Background(), m, &out, rollbackCfg()); err != nil {
		t.Fatalf("the rollback itself stands: %v\n%s", err, out.String())
	}
	raw, ok := logEntryFromCalls(m)
	if !ok {
		t.Fatalf("no log entry\n%s", out.String())
	}
	var entry state.LogEntry
	if err := json.Unmarshal(raw, &entry); err != nil {
		t.Fatal(err)
	}
	if !entry.Success || !entry.Degraded || !strings.Contains(entry.DegradedReason, "stale Caddy route") {
		t.Errorf("log entry = %+v", entry)
	}
}

// Deploy's twin of the rollback guard: a caddy deploy over a host release whose
// commit fails takes out the block it wrote rather than rendering a route for a
// release that had none.
func TestDeploy_CaddyOverHostCommitFailureRemovesBlock(t *testing.T) {
	st := fmt.Sprintf(`{"schema_version":2,"deployment_type":"container","ingress_mode":"host","updated_at":"2026-07-22T10:00:00Z","current_port":3000,"current_hash":"old123"}`)
	m := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "UPLOAD:/deployments/web/state.json.tmp-", Err: errors.New("disk full")},
		ssh.MockCommand{Match: "mkdir -p /deployments/web", Output: ""},
		ssh.MockCommand{Match: "mkdir /deployments/web/.lock", Output: ""},
		ssh.MockCommand{Match: "if [ ! -e '/deployments/web/state.json' ]", Output: "present\n" + st},
		ssh.MockCommand{Match: "docker ps", Output: ""},
		ssh.MockCommand{Match: "ss -tln", Output: ""},
		ssh.MockCommand{Match: "docker run", Output: "container-id"},
		ssh.MockCommand{Match: "docker inspect -f '{{.State.Status}}'", Output: "running"},
		ssh.MockCommand{Match: "docker stop", Output: ""},
		ssh.MockCommand{Match: "docker rm", Output: ""},
		ssh.MockCommand{Match: "curl -s -o /dev/null", Output: "200"},
		ssh.MockCommand{Match: "docker exec caddy caddy reload", Output: ""},
		ssh.MockCommand{Match: "a=$(docker exec caddy md5sum", Output: "TEPLOY_CADDY_OK"},
		ssh.MockCommand{Match: "mkdir /deployments/caddy/.lock", Output: ""},
		ssh.MockCommand{Match: "printf %s", Output: ""},
		ssh.MockCommand{Match: "rm -rf /deployments/web/.lock", Output: ""},
	)
	m.Files["/deployments/caddy/Caddyfile"] = []byte(otherBlock + "\n")
	var out bytes.Buffer
	cfg := Config{App: "web", Image: "web:new", Version: "new456", Domain: "web.teploy-fixture.dev", ContainerPort: 3000,
		Health: HealthConfig{Timeout: 5 * time.Second, Interval: 10 * time.Millisecond}}
	if err := NewDeployer(m, &out).Deploy(context.Background(), cfg); err == nil {
		t.Fatalf("expected the commit to fail\n%s", out.String())
	}
	if caddyfile := string(m.Files["/deployments/caddy/Caddyfile"]); strings.Contains(caddyfile, "TEPLOY BEGIN web") {
		t.Errorf("the block must go:\n%s", caddyfile)
	}
}
