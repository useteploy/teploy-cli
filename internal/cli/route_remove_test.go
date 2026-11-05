package cli

// `teploy route remove` tests: only a block proven dead goes, the commit is a
// compare-and-swap on the exact block that was judged, and every refusal path
// leaves the Caddyfile untouched.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/caddy"
	"github.com/useteploy/teploy/internal/ssh"
)

const routeRemoveCaddyfile = `example.com {
	reverse_proxy other:80
}

# TEPLOY BEGIN ghost
ghost.example.com {
	reverse_proxy ghost-web-v1:3000
}
# TEPLOY END ghost
`

// routeRemoveExecutor models a server whose ghost app is entirely gone: a
// directory with a lock, no state, no containers, a dead upstream. Extra
// registrations are consulted first, so a test can override any read.
func routeRemoveExecutor(caddyfile string, extra ...ssh.MockCommand) *ssh.MockExecutor {
	cmds := append([]ssh.MockCommand{}, extra...)
	cmds = append(cmds,
		ssh.MockCommand{Match: "if [ -d '/deployments/ghost' ]", Output: ""},
		ssh.MockCommand{Match: "mkdir /deployments/ghost/.lock", Output: ""},
		ssh.MockCommand{Match: "docker ps --all ", Output: routeRemoveContainers()},
		ssh.MockCommand{Match: "printf '%s\\n'", Output: "2 ghost-web-v1\n"},
		ssh.MockCommand{Match: "mkdir /deployments/caddy/.lock", Output: ""},
		ssh.MockCommand{Match: "docker exec caddy caddy reload", Output: ""},
		ssh.MockCommand{Match: "a=$(docker exec caddy md5sum", Output: "TEPLOY_CADDY_OK"},
		ssh.MockCommand{Match: "python3 -c", Output: `{"seq": 1, "ts": "2026-11-05T12:00:00Z"}`},
	)
	m := ssh.NewMockExecutor("192.0.2.10", cmds...)
	if caddyfile != "" {
		m.Files["/deployments/caddy/Caddyfile"] = []byte(caddyfile)
	}
	return m
}

func routeRemoveContainers() string {
	return auditContainer("caddy", "caddy:2", "running", "2026-05-01 00:00:00 +0000 UTC", nil)
}

func recordedEvents(t *testing.T, inputs []string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, input := range inputs {
		var ev map[string]any
		if err := json.Unmarshal([]byte(input), &ev); err != nil {
			continue // a non-event stdin payload (the caddy adapt check)
		}
		if ev["kind"] == "route.remove" {
			out = append(out, ev)
		}
	}
	return out
}

func TestRouteRemove_RemovesProvenDeadBlock(t *testing.T) {
	m := routeRemoveExecutor(routeRemoveCaddyfile)
	res, err := executeRouteRemove(context.Background(), m, "ghost")
	if err != nil {
		t.Fatalf("executeRouteRemove: %v", err)
	}
	if res.Route != "removed" {
		t.Errorf("route = %s, want removed", res.Route)
	}
	if fmt.Sprint(res.Hosts) != fmt.Sprint([]string{"ghost.example.com"}) {
		t.Errorf("hosts = %v", res.Hosts)
	}
	written := string(m.Files["/deployments/caddy/Caddyfile"])
	if strings.Contains(written, "TEPLOY BEGIN ghost") || strings.Contains(written, "ghost.example.com") {
		t.Errorf("the ghost block survived: %q", written)
	}
	if !strings.Contains(written, "example.com {") {
		t.Errorf("an unrelated block was dropped: %q", written)
	}
	// The removal is committed under the exact-block CAS on the judged block.
	var casCommit bool
	for _, call := range m.Calls {
		if strings.Contains(call, "TEPLOY_ROUTE_CAS_MISMATCH") && strings.Contains(call, "mv -fT -- ") {
			casCommit = true
		}
	}
	if !casCommit {
		t.Errorf("the commit was not guarded by the route CAS; calls: %v", m.Calls)
	}
	// Recorded in the app's event history.
	evs := recordedEvents(t, m.Inputs)
	if len(evs) != 1 || evs[0]["outcome"] != "succeeded" {
		t.Fatalf("events = %v", evs)
	}
	if evs[0]["detail"].(map[string]any)["route"] != "removed" {
		t.Errorf("event detail = %v", evs[0]["detail"])
	}
}

func TestRouteRemove_AbsentBlockIsSuccess(t *testing.T) {
	m := routeRemoveExecutor("example.com {\n\treverse_proxy other:80\n}\n")
	res, err := executeRouteRemove(context.Background(), m, "ghost")
	if err != nil {
		t.Fatalf("an absent block must succeed: %v", err)
	}
	if res.Route != "absent" {
		t.Errorf("route = %s, want absent", res.Route)
	}
}

func TestRouteRemove_Refusals(t *testing.T) {
	tests := []struct {
		name   string
		expect string
		edited string // expected Caddyfile after the refusal ("" = unchanged)
		extra  []ssh.MockCommand
		files  map[string][]byte
	}{
		{
			name:   "live app",
			expect: "belongs to a live app",
			extra: []ssh.MockCommand{{Match: "docker ps --all ", Output: routeRemoveContainers() + "\n" +
				auditContainer("ghost-web-v1", "ghost:v1", "running", "2026-11-04 10:00:00 +0000 UTC",
					map[string]string{"teploy.app": "ghost", "teploy.process": "web", "teploy.version": "v1"})}},
			files: map[string][]byte{"/deployments/ghost/state.json": []byte(auditStateJSON("container", "caddy", "v1", "", nil))},
		},
		{
			name:   "container still carries the name",
			expect: "still carries this app's name",
			extra: []ssh.MockCommand{{Match: "docker ps --all ", Output: routeRemoveContainers() + "\n" +
				auditContainer("ghost-web-v1", "ghost:v1", "exited", "2026-11-04 10:00:00 +0000 UTC",
					map[string]string{"teploy.app": "ghost", "teploy.process": "web", "teploy.version": "v1"})}},
		},
		{
			name:   "upstream resolves",
			expect: "still resolves",
			extra:  []ssh.MockCommand{{Match: "printf '%s\\n'", Output: "0 ghost-web-v1\n"}},
		},
		{
			name:   "upstream unknowable",
			expect: "could not be proven dead",
			extra:  []ssh.MockCommand{{Match: "printf '%s\\n'", Err: errors.New("getent: temporary failure")}},
		},
		{
			name:   "unreadable state",
			expect: "state could not be read",
			extra:  []ssh.MockCommand{{Match: "if [ ! -e '/deployments/ghost/state.json'", Err: errors.New("permission denied")}},
		},
		{
			name:   "webhook block",
			expect: "autodeploy webhook",
			edited: "# TEPLOY BEGIN ghost\nghost.example.com {\n\treverse_proxy ghost-web-v1:3000\n\t# TEPLOY WEBHOOK BEGIN ghost\n\t# TEPLOY WEBHOOK END ghost\n}\n# TEPLOY END ghost\n",
		},
		{
			name:   "serving files that exist",
			expect: "serves files from",
			edited: "# TEPLOY BEGIN ghost\nghost.example.com {\n\troot * /deployments/ghost/site\n\tfile_server\n}\n# TEPLOY END ghost\n",
			extra:  []ssh.MockCommand{{Match: "if [ -e '/deployments/ghost/site'", Output: "yes"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := routeRemoveCaddyfile
			if tt.edited != "" {
				before = tt.edited
			}
			m := routeRemoveExecutor(before, tt.extra...)
			for p, data := range tt.files {
				m.Files[p] = data
			}
			_, err := executeRouteRemove(context.Background(), m, "ghost")
			if !errors.Is(err, errRouteRefused) {
				t.Fatalf("error = %v, want a route remove refusal", err)
			}
			if !strings.Contains(err.Error(), tt.expect) {
				t.Errorf("refusal = %q, want it to name %q", err, tt.expect)
			}
			if written := string(m.Files["/deployments/caddy/Caddyfile"]); written != before {
				t.Errorf("a refusal must not edit the Caddyfile:\n got: %q\nwant: %q", written, before)
			}
			for _, call := range m.Calls {
				if strings.Contains(call, "mv -fT -- ") {
					t.Errorf("a refusal must not commit: %s", call)
				}
			}
			evs := recordedEvents(t, m.Inputs)
			if len(evs) != 1 || evs[0]["outcome"] != "refused" {
				t.Errorf("refusal events = %v", evs)
			}
		})
	}
}

// concurrentRewriteExecutor models a deploy rewriting the app's block between
// `route remove` judging it and committing: the second read of the Caddyfile
// (inside the caddy client's mutate) answers different content.
type concurrentRewriteExecutor struct {
	*ssh.MockExecutor
	rewrite       string
	catSeen       int
	catCaddyfile  string
	eventAppended bool
}

func (e *concurrentRewriteExecutor) Run(ctx context.Context, cmd string) (string, error) {
	if cmd == e.catCaddyfile {
		e.catSeen++
		if e.catSeen == 1 {
			// A successor deploys a new block for the app between the
			// judgement (the framed read) and the commit (this plain cat
			// inside the caddy client's mutate).
			e.Files["/deployments/caddy/Caddyfile"] = []byte(e.rewrite)
		}
	}
	if strings.HasPrefix(cmd, "python3 -c") {
		e.eventAppended = true
	}
	return e.MockExecutor.Run(ctx, cmd)
}

// RunStream records the event append too: the ssh package's structured
// capture falls back to RunStream for executors it does not know, so the
// history append arrives here rather than through Run.
func (e *concurrentRewriteExecutor) RunStream(ctx context.Context, cmd string, stdout, stderr io.Writer) error {
	if strings.HasPrefix(cmd, "python3 -c") {
		e.eventAppended = true
	}
	return e.MockExecutor.RunStream(ctx, cmd, stdout, stderr)
}

func TestRouteRemove_ConcurrentRewriteIsRefused(t *testing.T) {
	base := routeRemoveExecutor(routeRemoveCaddyfile)
	rewritten := `example.com {
	reverse_proxy other:80
}

# TEPLOY BEGIN ghost
ghost.example.com {
	reverse_proxy ghost-web-v9:3000
}
# TEPLOY END ghost
`
	m := &concurrentRewriteExecutor{MockExecutor: base, rewrite: rewritten, catCaddyfile: "cat /deployments/caddy/Caddyfile"}
	_, err := executeRouteRemove(context.Background(), m, "ghost")
	if err == nil {
		t.Fatal("a block that changed under the removal must refuse")
	}
	var casErr *caddy.ErrRouteCAS
	if !errors.As(err, &casErr) {
		t.Fatalf("error = %v, want a route CAS refusal", err)
	}
	after := string(m.Files["/deployments/caddy/Caddyfile"])
	if after != rewritten {
		t.Errorf("a refused commit must leave the successor's block exactly as it is: %q", after)
	}
	if !strings.Contains(after, "TEPLOY BEGIN ghost") {
		t.Errorf("the app's block was removed despite the refusal: %q", after)
	}
	for path := range m.Files {
		if strings.Contains(path, ".tmp-") {
			t.Errorf("the staged caddyfile was not cleaned up: %s", path)
		}
	}
	// The wrapper's structured capture falls back to RunStream, so the event
	// payload is not captured in Inputs; the failure is still recorded — the
	// append call itself must have run.
	if !m.eventAppended {
		t.Error("the refused removal was not recorded in the event history")
	}
}
