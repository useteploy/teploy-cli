package deploy

// C01-8/9 + A12/T05 acceptance coverage: generation identity for rollback
// and the exact-block CAS, proven through the REAL Rollback and
// DeployFenced entry points over the mock executor (the mock evaluates the
// composed guards, the generation sidecar CAS, the container label checks
// and the route CAS against recorded file state — the same evaluation the
// server shell performs). The racing shapes:
//
//   - deploy vs rollback: the successor's route switch lands between the
//     rollback's resolution and its own switch — the exact-block CAS
//     refuses with BOTH generations named; nothing lands.
//   - rollback vs rollback: same shape, the successor being another
//     rollback (its block names its own target).
//   - generation bump mid-flight: the successor commits (sidecar + state)
//     between the rollback's switch and its commit — the commit refuses
//     (ErrGenerationFenced) and the authority keeps the successor.
//   - a delayed SSH effect landing post-takeover: the retirement stop
//     addresses the container by ID and reads its immutable generation
//     label at execution — a newer generation's container is refused, no
//     matter when the command lands.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy/internal/caddy"
	"github.com/useteploy/teploy/internal/ssh"
	"github.com/useteploy/teploy/internal/state"
)

// genRollbackMocks is the rollback fixture: state at generation 7 (current
// v2, previous v1), v1 stopped (ID aaa), v2 running (ID bbb), a Caddyfile
// whose myapp block is the generation-7 route to v2.
func genRollbackMocks(t *testing.T, caddyfile string) *ssh.MockExecutor {
	t.Helper()
	stateContent := `{"schema_version":2,"deployment_type":"container","ingress_mode":"caddy","domain":"myapp.com","updated_at":"2026-09-24T00:00:00Z","operation_id":"op-v2","generation":7,"current_port":49153,"current_hash":"v2","previous_port":49152,"previous_hash":"v1"}`
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "mkdir -p /deployments/myapp", Output: ""},
		ssh.MockCommand{Match: "mkdir /deployments/myapp/.lock", Output: ""},
		ssh.MockCommand{Match: "if [ ! -e '/deployments/myapp/state.json' ]", Output: "present\n" + stateContent},
		ssh.MockCommand{Match: "docker ps --all --filter label=teploy.app='myapp'",
			Output: `{"ID":"aaa","Names":"myapp-web-v1","Image":"myapp:latest","State":"exited","Status":"Exited","Labels":"teploy.app=myapp,teploy.version=v1,teploy.process=web,teploy.generation=5"}` + "\n" +
				`{"ID":"bbb","Names":"myapp-web-v2","Image":"myapp:latest","State":"running","Status":"Up 1h","Labels":"teploy.app=myapp,teploy.version=v2,teploy.process=web,teploy.generation=7"}`,
		},
		ssh.MockCommand{Match: "docker inspect 'myapp-web-v1'", Output: `[{"Config":{"Image":"myapp:latest","Labels":{"teploy.app":"myapp","teploy.version":"v1","teploy.generation":"5"}},"HostConfig":{"NetworkMode":"teploy","PortBindings":{"3000/tcp":[{"HostIp":"127.0.0.1","HostPort":"49152"}]},"RestartPolicy":{"Name":"no"}},"NetworkSettings":{"Networks":{"teploy":{"Aliases":["myapp"]}}}}]`},
		ssh.MockCommand{Match: "docker rm -f 'myapp-web-v1'", Output: ""},
		ssh.MockCommand{Match: "docker run", Output: "aaa"},
		ssh.MockCommand{Match: "curl -s -o /dev/null", Output: "200"},
		ssh.MockCommand{Match: "docker inspect -f '{{range $p, $b := .NetworkSettings.Ports}}{{range $b}}{{.HostIp}}", Output: "127.0.0.1 "},
		ssh.MockCommand{Match: "docker inspect -f '{{range $p, $b := .NetworkSettings.Ports}}", Output: "49152"},
		ssh.MockCommand{Match: "docker inspect -f '{{range $p, $_ := .NetworkSettings.Ports}}", Output: "3000/tcp"},
		ssh.MockCommand{Match: "docker exec caddy caddy reload", Output: ""},
		ssh.MockCommand{Match: "a=$(docker exec caddy md5sum", Output: "TEPLOY_CADDY_OK"},
		ssh.MockCommand{Match: "mkdir /deployments/caddy/.lock", Output: ""},
		ssh.MockCommand{Match: "docker stop", Output: ""},
		ssh.MockCommand{Match: "docker rm ", Output: ""},
		ssh.MockCommand{Match: "cat /tmp", Output: ""},
		ssh.MockCommand{Match: "printf %s", Output: ""},
		ssh.MockCommand{Match: "rm -rf /deployments/myapp/.lock", Output: ""},
	)
	// The Caddyfile lives in Files (not a cat registration) so the mock's
	// shell-mode CAS evaluation reads the same bytes the framed reads do.
	mock.Files["/deployments/caddy/Caddyfile"] = []byte(caddyfile)
	mock.Files["/deployments/myapp/.generation"] = []byte("7\n")
	return mock
}

// genV2Block is the generation-7 managed block routing to v2.
const genV2Block = "# TEPLOY BEGIN myapp\n# TEPLOY GENERATION 7\nmyapp.com {\n\treverse_proxy myapp-web-v2:3000\n}\n# TEPLOY END myapp"

// successorBlock models a successor generation's switch: stamped 8, naming
// ITS containers (a deploy's v3 candidates, or another rollback's target —
// the CAS refuses either way, which is the point).
func successorBlock(upstream string) string {
	return "# TEPLOY BEGIN myapp\n# TEPLOY GENERATION 8\nmyapp.com {\n\treverse_proxy " + upstream + ":3000\n}\n# TEPLOY END myapp"
}

// raceSwapper wraps the mock, landing a successor's committed world (route
// block + state.json + generation sidecar) the moment the trigger command
// runs — modeling the racing client's commit landing mid-flight.
type raceSwapper struct {
	*ssh.MockExecutor
	trigger        string
	fired          bool
	successorFile  string
	successorState string
}

func (r *raceSwapper) Run(ctx context.Context, cmd string) (string, error) {
	if !r.fired && strings.Contains(cmd, r.trigger) {
		r.fired = true
		r.MockExecutor.Files["/deployments/caddy/Caddyfile"] = []byte(r.successorFile)
		if r.successorState != "" {
			r.MockExecutor.Files["/deployments/myapp/state.json"] = []byte(r.successorState)
		}
		r.MockExecutor.Files["/deployments/myapp/.generation"] = []byte("8\n")
	}
	return r.MockExecutor.Run(ctx, cmd)
}

// TestRollback_RouteSwitchCASRefusesAfterSuccessorSwitched is the
// deploy-vs-rollback race: the successor's route switch (generation 8,
// naming its own containers) lands between the rollback's resolution and
// its switch. The exact-block CAS refuses — BOTH generations named, the
// successor's upstreams as evidence — nothing lands, and the target the
// rollback started is cleaned up.
func TestRollback_RouteSwitchCASRefusesAfterSuccessorSwitched(t *testing.T) {
	for _, tc := range []struct {
		name      string
		upstream  string
		successor string
	}{
		{"deploy vs rollback", "myapp-web-v3", "a deploy switched to its v3 candidates"},
		{"rollback vs rollback", "myapp-web-v1", "another rollback switched back to its target first"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := genRollbackMocks(t, genV2Block+"\n")
			swap := &raceSwapper{
				MockExecutor:  base,
				trigger:       "docker inspect 'myapp-web-v1'", // after resolve, before the switch
				successorFile: successorBlock(tc.upstream) + "\n",
			}
			var buf bytes.Buffer
			err := Rollback(context.Background(), swap, &buf, rollbackCfg())
			var cas *caddy.ErrRouteCAS
			if !errors.As(err, &cas) {
				t.Fatalf("expected *ErrRouteCAS from the refused switch, got %v\noutput:\n%s", err, buf.String())
			}
			if cas.ExpectedGeneration != 7 || !cas.FoundStamped || cas.FoundGeneration != 8 {
				t.Errorf("the refusal must name both generations: %+v", cas)
			}
			if len(cas.FoundUpstreams) == 0 || !strings.Contains(cas.FoundUpstreams[0], tc.upstream) {
				t.Errorf("the refusal must name the successor's upstreams: %+v", cas)
			}
			if !swap.fired {
				t.Fatal("test bug: the racing switch never fired")
			}
			// Nothing landed: the live block is still the successor's.
			if got := string(base.Files["/deployments/caddy/Caddyfile"]); got != successorBlock(tc.upstream)+"\n" {
				t.Errorf("a refused switch must not modify the Caddyfile, got:\n%s", got)
			}
			for _, c := range base.Calls {
				if strings.HasPrefix(c, "docker exec caddy caddy reload") {
					t.Error("no reload may run after a refused switch")
				}
			}
		})
	}
}

// TestRollback_CommitRefusedOverMidFlightGenerationBump is the
// generation-bump-mid-flight race: the rollback's switch lands, then the
// successor COMMITS (state.json + sidecar at generation 8) before the
// rollback's own commit. The commit's generation CAS refuses; the
// authority keeps the successor's state; the rollback's own route is
// restored (the CAS set still owns it) and its target is stopped.
func TestRollback_CommitRefusedOverMidFlightGenerationBump(t *testing.T) {
	base := genRollbackMocks(t, genV2Block+"\n")
	successorState := `{"schema_version":2,"deployment_type":"container","ingress_mode":"caddy","updated_at":"2026-09-24T00:01:00Z","operation_id":"successor","generation":8,"current_hash":"v3","previous_hash":"v2"}`
	swap := &raceSwapper{
		MockExecutor:   base,
		trigger:        "docker exec caddy caddy reload", // the rollback's switch landed
		successorFile:  "",                               // the successor did NOT touch the route here
		successorState: successorState,
	}
	var buf bytes.Buffer
	err := Rollback(context.Background(), swap, &buf, rollbackCfg())
	if !state.GenerationFenced(err) {
		t.Fatalf("expected a generation-fenced commit refusal, got %v\noutput:\n%s", err, buf.String())
	}
	if !strings.Contains(err.Error(), "expected predecessor 7") || !strings.Contains(err.Error(), "generation 8") {
		t.Errorf("the refusal must name both generations: %v", err)
	}
	// The successor's authority is intact.
	var st state.AppState
	if err := json.Unmarshal(base.Files["/deployments/myapp/state.json"], &st); err != nil {
		t.Fatalf("successor state.json unreadable: %v", err)
	}
	if st.Generation != 8 || st.CurrentHash != "v3" {
		t.Errorf("a refused commit must not touch the successor's state: %+v", st)
	}
	if string(base.Files["/deployments/myapp/.generation"]) != "8\n" {
		t.Errorf("the sidecar must keep the successor's generation: %q", base.Files["/deployments/myapp/.generation"])
	}
	// The rollback's own uncommitted switch was undone: the live block is
	// the generation-7 predecessor route again (stamped 7).
	live := string(base.Files["/deployments/caddy/Caddyfile"])
	if !strings.Contains(live, "# TEPLOY GENERATION 7") || !strings.Contains(live, "myapp-web-v2:3000") {
		t.Errorf("the CAS-owned route must be restored to the predecessor block, got:\n%s", live)
	}
}

// TestRollback_RetirementRefusesNewerGenerationContainer is the
// delayed-SSH-effect acceptance: the retirement stop command executes LONG
// after the world moved on (takeover + same-hash redeploy put a generation
// 8 container under the same name/ID the rollback resolved). The stop
// reads the container's immutable generation label at execution time and
// refuses — the newer generation's container keeps running, and the
// rollback reports the degraded retirement instead of silently yo-yo'ing.
func TestRollback_RetirementRefusesNewerGenerationContainer(t *testing.T) {
	base := genRollbackMocks(t, genV2Block+"\n")
	// The v2 container was re-created by generation 8 under the same ID.
	base.GenerationLabels = map[string]string{"bbb": "8"}
	var buf bytes.Buffer
	if err := Rollback(context.Background(), base, &buf, rollbackCfg()); err != nil {
		t.Fatalf("a refused retirement is degraded cleanup, not a failed rollback: %v\n%s", err, buf.String())
	}
	out := buf.String()
	if !strings.Contains(out, "newer generation") {
		t.Errorf("the retirement refusal must be reported, got:\n%s", out)
	}
	// The newer generation's container was never stopped.
	for _, c := range base.Calls {
		if strings.HasPrefix(c, "docker stop") {
			t.Errorf("a generation-fenced stop must never execute, saw: %s", c)
		}
	}
	if !strings.Contains(out, "Rolled back myapp to version v1") {
		t.Errorf("the rollback itself must complete, got:\n%s", out)
	}
}

// TestRollback_HappyPathStampsAndSidecar pins the happy-path identity
// surface: the rollback's switched block is stamped with the generation it
// creates (8), and its commit publishes the sidecar — the world after a
// successful rollback is fully generation-attributable.
func TestRollback_HappyPathStampsAndSidecar(t *testing.T) {
	base := genRollbackMocks(t, genV2Block+"\n")
	var buf bytes.Buffer
	if err := Rollback(context.Background(), base, &buf, rollbackCfg()); err != nil {
		t.Fatalf("Rollback: %v\n%s", err, buf.String())
	}
	live := string(base.Files["/deployments/caddy/Caddyfile"])
	if !strings.Contains(live, "# TEPLOY GENERATION 8") || !strings.Contains(live, "myapp-web-v1:3000") {
		t.Errorf("the rolled-back route must be stamped with the new generation:\n%s", live)
	}
	if gen := strings.TrimSpace(string(base.Files["/deployments/myapp/.generation"])); gen != "8" {
		t.Errorf("the commit must publish the sidecar at the new generation, got %q", gen)
	}
}

// TestDeploy_RouteSwitchCASRefusesAfterSuccessorSwitched is the deploy-side
// twin: a successor's block lands between the deploy's resolution and its
// switch — the switch refuses with both generations named and the
// candidates are cleaned up.
func TestDeploy_RouteSwitchCASRefusesAfterSuccessorSwitched(t *testing.T) {
	app := "fency"
	// A standing generation-7 world: state.json names it (drop the fixture's
	// "absent" state registration so the Files-seeded state is read).
	mocks := make([]ssh.MockCommand, 0, len(fenceHappyPathMocks(app))+2)
	for _, m := range fenceHappyPathMocks(app) {
		if m.Match == "if [ ! -e '/deployments/"+app+"/state.json' ]" {
			continue
		}
		mocks = append(mocks, m)
	}
	mocks = append(mocks,
		ssh.MockCommand{Match: "docker stop", Output: ""},
		ssh.MockCommand{Match: "docker rm -f", Output: ""},
	)
	base := ssh.NewMockExecutor("1.2.3.4", mocks...)
	base.Files["/deployments/"+app+"/state.json"] = []byte(`{"schema_version":2,"deployment_type":"container","ingress_mode":"caddy","updated_at":"2026-09-24T00:00:00Z","operation_id":"op-7","generation":7,"current_hash":"old","current_port":49152}`)
	predBlock := "# TEPLOY BEGIN " + app + "\n# TEPLOY GENERATION 7\nfency.com {\n\treverse_proxy " + app + "-web-old:3000\n}\n# TEPLOY END " + app
	base.Files["/deployments/caddy/Caddyfile"] = []byte("{\n\tadmin 0.0.0.0:2019\n}\n\n" + predBlock + "\n")
	base.Files["/deployments/"+app+"/.generation"] = []byte("7\n")
	lk, err := state.AcquireLockFenced(context.Background(), base, app)
	if err != nil {
		t.Fatalf("AcquireLockFenced: %v", err)
	}
	swap := &raceSwapper{
		MockExecutor:  base,
		trigger:       "curl -s -o /dev/null", // the health gate: after resolve, before the switch
		successorFile: "{\n\tadmin 0.0.0.0:2019\n}\n\n" + successorBlockFency() + "\n",
	}
	var buf bytes.Buffer
	d := NewDeployer(swap, &buf)
	err = d.DeployFenced(context.Background(), Config{
		App:     app,
		Domain:  "fency.com",
		Image:   "fency:latest",
		Version: "abc123",
		Health:  HealthConfig{Timeout: 5 * time.Second, Interval: 10 * time.Millisecond},
	}, lk)
	var cas *caddy.ErrRouteCAS
	if !errors.As(err, &cas) {
		t.Fatalf("expected *ErrRouteCAS from the refused switch, got %v\noutput:\n%s", err, buf.String())
	}
	if cas.ExpectedGeneration != 7 || !cas.FoundStamped || cas.FoundGeneration != 8 {
		t.Errorf("the refusal must name both generations: %+v", cas)
	}
	if got := string(base.Files["/deployments/caddy/Caddyfile"]); !strings.Contains(got, successorUpstreamFency) {
		t.Errorf("a refused switch must leave the successor's block live, got:\n%s", got)
	}
	if !strings.Contains(buf.String(), "cleanup incomplete") && !anyCall(base, "docker stop") {
		t.Errorf("the deploy must clean up its started candidates:\n%s", buf.String())
	}
}

func successorBlockFency() string {
	return "# TEPLOY BEGIN fency\n# TEPLOY GENERATION 8\nfency.com {\n\treverse_proxy " + successorUpstreamFency + ":3000\n}\n# TEPLOY END fency"
}

const successorUpstreamFency = "fency-web-newgen"

func anyCall(mock *ssh.MockExecutor, prefix string) bool {
	for _, c := range mock.Calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

// TestDeploy_HappyPathStampsGeneration pins the deploy-side identity
// surface: candidates are labeled with the generation the deploy creates
// and the switched block carries its stamp.
func TestDeploy_HappyPathStampsGeneration(t *testing.T) {
	app := "fency"
	base := ssh.NewMockExecutor("1.2.3.4", fenceHappyPathMocks(app)...)
	lk, err := state.AcquireLockFenced(context.Background(), base, app)
	if err != nil {
		t.Fatalf("AcquireLockFenced: %v", err)
	}
	var buf bytes.Buffer
	d := NewDeployer(base, &buf)
	if err := d.DeployFenced(context.Background(), Config{
		App:     app,
		Domain:  "fency.com",
		Image:   "fency:latest",
		Version: "abc123",
		Health:  HealthConfig{Timeout: 5 * time.Second, Interval: 10 * time.Millisecond},
	}, lk); err != nil {
		t.Fatalf("DeployFenced: %v\n%s", err, buf.String())
	}
	var labeledRun bool
	for _, c := range base.Calls {
		if strings.HasPrefix(c, "docker run") && strings.Contains(c, "--label 'teploy.generation=1'") {
			labeledRun = true
		}
	}
	if !labeledRun {
		t.Error("first deploy must stamp its candidates with generation 1")
	}
	live := string(base.Files["/deployments/caddy/Caddyfile"])
	if !strings.Contains(live, "# TEPLOY GENERATION 1") {
		t.Errorf("the switched block must be stamped with the deploy's generation:\n%s", live)
	}
	if gen := strings.TrimSpace(string(base.Files["/deployments/"+app+"/.generation"])); gen != "1" {
		t.Errorf("the commit must publish the sidecar, got %q", gen)
	}
}
