package cli

// `releases prune --keep N` and `server prune --build-cache --dangling`
// tests: the selection keeps exactly what the plan protects, and acting
// without knowing current, previous or the pins refuses.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/ssh"
)

// pruneFixture models a container app with versions v1..v4 on a server.
func pruneFixture(t *testing.T, manifest string, pins string, extra ...ssh.MockCommand) *ssh.MockExecutor {
	t.Helper()
	cmds := append([]ssh.MockCommand{}, extra...)
	cmds = append(cmds,
		ssh.MockCommand{Match: "if [ -d '/deployments/blog' ]", Output: "yes"},
		ssh.MockCommand{Match: "mkdir /deployments/blog/.lock", Output: ""},
		ssh.MockCommand{Match: "docker ps --all --filter label=teploy.app='blog'", Output: strings.Join([]string{
			auditContainer("blog-web-v4", "blog:v4", "running", "2026-11-04 09:00:00 +0000 UTC",
				map[string]string{"teploy.app": "blog", "teploy.process": "web", "teploy.version": "v4"}),
			auditContainer("blog-web-v3", "blog:v3", "exited", "2026-11-03 09:00:00 +0000 UTC",
				map[string]string{"teploy.app": "blog", "teploy.process": "web", "teploy.version": "v3"}),
			auditContainer("blog-web-v2", "blog:v2", "exited", "2026-11-02 09:00:00 +0000 UTC",
				map[string]string{"teploy.app": "blog", "teploy.process": "web", "teploy.version": "v2"}),
			auditContainer("blog-web-v1", "blog:v1", "exited", "2026-11-01 09:00:00 +0000 UTC",
				map[string]string{"teploy.app": "blog", "teploy.process": "web", "teploy.version": "v1"}),
		}, "\n")},
		ssh.MockCommand{Match: "docker rm -f", Output: ""},
		ssh.MockCommand{Match: "docker rmi", Output: ""},
		ssh.MockCommand{Match: "python3 -c", Output: `{"seq": 1, "ts": "2026-11-05T12:00:00Z"}`},
	)
	m := ssh.NewMockExecutor("192.0.2.10", cmds...)
	m.Files["/deployments/blog/state.json"] = []byte(auditStateJSON("container", "caddy", "v4", "v3", manifestMap(manifest)))
	if pins != "" {
		m.Files["/deployments/blog/pinned"] = []byte(pins)
	}
	return m
}

func manifestMap(s string) map[string]any {
	if s == "" {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		panic(err)
	}
	return m
}

func TestReleasesPrune_KeepsExactlyTheExpectedSet(t *testing.T) {
	m := pruneFixture(t, `{"container":{"keep_versions":2}}`, "v1\n")
	res, err := executeReleasesPrune(context.Background(), m, "blog", 2)
	if err != nil {
		t.Fatalf("executeReleasesPrune: %v", err)
	}
	if fmt.Sprint(res.Pruned) != fmt.Sprint([]string{"v2"}) {
		t.Errorf("pruned = %v, want [v2] (v4 current, v3 previous, v1 pinned)", res.Pruned)
	}
	if fmt.Sprint(res.Protected) != fmt.Sprint([]string{"v4", "v3", "v1"}) {
		t.Errorf("protected = %v, want [v4 v3 v1]", res.Protected)
	}
	for _, call := range m.Calls {
		if strings.HasPrefix(call, "docker rm -f") || strings.HasPrefix(call, "docker rmi") {
			for _, keep := range []string{"v1", "v3", "v4"} {
				if strings.Contains(call, keep) {
					t.Errorf("protected version %s was removed: %s", keep, call)
				}
			}
		}
	}
	if !sawCall(m, "docker rm -f 'blog-web-v2'") || !sawCall(m, "docker rmi 'blog:v2'") {
		t.Errorf("v2's container and image were not both removed; calls: %v", m.Calls)
	}
}

// keep 0 on the command means "everything the protections do not name" —
// deliberately different from keep_versions: 0 in teploy.yml.
func TestReleasesPrune_KeepZeroPrunesAllUnprotected(t *testing.T) {
	m := pruneFixture(t, `{"container":{"keep_versions":2}}`, "v1\n")
	res, err := executeReleasesPrune(context.Background(), m, "blog", 0)
	if err != nil {
		t.Fatalf("executeReleasesPrune: %v", err)
	}
	if fmt.Sprint(res.Pruned) != fmt.Sprint([]string{"v2"}) {
		t.Errorf("pruned = %v, want [v2]: current, previous and pins survive even --keep 0", res.Pruned)
	}
}

func TestReleasesPrune_NothingBeyondKeepIsSuccess(t *testing.T) {
	m := pruneFixture(t, `{"container":{"keep_versions":5}}`, "")
	res, err := executeReleasesPrune(context.Background(), m, "blog", 5)
	if err != nil {
		t.Fatalf("executeReleasesPrune: %v", err)
	}
	if len(res.Pruned) != 0 {
		t.Errorf("pruned = %v, want nothing", res.Pruned)
	}
	for _, call := range m.Calls {
		if strings.HasPrefix(call, "docker rm") || strings.HasPrefix(call, "docker rmi") {
			t.Errorf("nothing was beyond keep, yet: %s", call)
		}
	}
}

func TestReleasesPrune_RunsUnderTheAppLock(t *testing.T) {
	m := pruneFixture(t, `{"container":{"keep_versions":2}}`, "v1\n")
	if _, err := executeReleasesPrune(context.Background(), m, "blog", 2); err != nil {
		t.Fatal(err)
	}
	if !sawCall(m, "mkdir /deployments/blog/.lock 2>/dev/null") {
		t.Errorf("the prune never took the app lock; calls: %v", m.Calls)
	}
	// Container removals run under the lock's fence guard.
	var fenced bool
	for _, call := range m.Calls {
		if strings.HasPrefix(call, "grep -q ") && strings.Contains(call, "docker rm -f") {
			fenced = true
		}
	}
	if !fenced {
		t.Errorf("container removals were not fenced by the lock; calls: %v", m.Calls)
	}
}

func TestReleasesPrune_RefusesWhenItCannotKnowTheProtections(t *testing.T) {
	tests := []struct {
		name  string
		extra []ssh.MockCommand
		want  string
	}{
		{
			name:  "unreadable pins",
			extra: []ssh.MockCommand{{Match: "if [ ! -e '/deployments/blog/pinned'", Err: errors.New("permission denied")}},
			want:  "refusing to prune",
		},
		{
			name:  "unreadable state",
			extra: []ssh.MockCommand{{Match: "if [ ! -e '/deployments/blog/state.json'", Err: errors.New("permission denied")}},
			want:  "reading state",
		},
		{
			name:  "unreadable containers",
			extra: []ssh.MockCommand{{Match: "docker ps --all --filter label=teploy.app='blog'", Err: errors.New("docker: no daemon")}},
			want:  "refusing to prune",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := pruneFixture(t, `{"container":{"keep_versions":1}}`, "", tt.extra...)
			_, err := executeReleasesPrune(context.Background(), m, "blog", 1)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tt.want)
			}
			for _, call := range m.Calls {
				if strings.HasPrefix(call, "docker rm") || strings.HasPrefix(call, "docker rmi") {
					t.Errorf("a refused prune removed things: %s", call)
				}
			}
		})
	}
}

func TestReleasesPrune_StaticAppExplainsItself(t *testing.T) {
	m := pruneFixture(t, "", "")
	m.Files["/deployments/blog/state.json"] = []byte(auditStateJSON("static", "caddy", "s1", "", nil))
	_, err := executeReleasesPrune(context.Background(), m, "blog", 2)
	if err == nil || !strings.Contains(err.Error(), "keep_releases") {
		t.Fatalf("error = %v, want the static keep_releases explanation", err)
	}
}

func TestReleasesPrune_AlreadyRemovedAppIsSuccess(t *testing.T) {
	m := ssh.NewMockExecutor("192.0.2.10",
		ssh.MockCommand{Match: "if [ -d '/deployments/blog' ]", Output: "no"},
	)
	res, err := executeReleasesPrune(context.Background(), m, "blog", 2)
	if err != nil {
		t.Fatalf("an app with no directory must be a no-op success: %v", err)
	}
	if len(res.Pruned) != 0 {
		t.Errorf("pruned = %v", res.Pruned)
	}
}

func sawCall(m *ssh.MockExecutor, prefix string) bool {
	for _, call := range m.Calls {
		if strings.HasPrefix(call, prefix) {
			return true
		}
		if strings.Contains(call, prefix) { // a fenced/guarded composition
			return true
		}
	}
	return false
}

func TestServerPrune_RunsExactlyTheSelectedPrunes(t *testing.T) {
	m := ssh.NewMockExecutor("192.0.2.10",
		ssh.MockCommand{Match: "docker builder prune -a -f", Output: "Total reclaimed space: 1.5GB"},
		ssh.MockCommand{Match: "docker image prune -f", Output: "Total reclaimed space: 300MB"},
		ssh.MockCommand{Match: "python3 -c", Output: `{"seq": 1, "ts": "2026-11-05T12:00:00Z"}`},
	)
	res, err := executeServerPrune(context.Background(), m, "edge", true, true)
	if err != nil {
		t.Fatalf("executeServerPrune: %v", err)
	}
	if !res.BuildCache || !res.Dangling {
		t.Errorf("result flags = %+v", res)
	}
	if res.Output != "Total reclaimed space: 1.5GB; Total reclaimed space: 300MB" {
		t.Errorf("output = %q", res.Output)
	}
	// Only the two reclaim prunes ran: no tagged image, container or volume.
	for _, call := range m.Calls {
		if strings.Contains(call, "docker volume prune") || strings.Contains(call, "docker system prune") ||
			strings.Contains(call, "docker container prune") || strings.Contains(call, "-a -f --volumes") {
			t.Errorf("server prune went beyond build cache and dangling images: %s", call)
		}
	}
	// Recorded under the fleet pseudo-app.
	var fleetEvent bool
	for _, input := range m.Inputs {
		if strings.Contains(input, `"app":"_fleet"`) && strings.Contains(input, `"server.prune"`) {
			fleetEvent = true
		}
	}
	if !fleetEvent {
		t.Errorf("no server.prune event under _fleet; inputs: %v", m.Inputs)
	}
}

func TestServerPrune_OnlyTheSelectedFlagRuns(t *testing.T) {
	m := ssh.NewMockExecutor("192.0.2.10",
		ssh.MockCommand{Match: "docker image prune -f", Output: "Total reclaimed space: 300MB"},
		ssh.MockCommand{Match: "python3 -c", Output: `{"seq": 1, "ts": "2026-11-05T12:00:00Z"}`},
	)
	if _, err := executeServerPrune(context.Background(), m, "edge", false, true); err != nil {
		t.Fatal(err)
	}
	for _, call := range m.Calls {
		if strings.Contains(call, "builder prune") {
			t.Errorf("--build-cache was not asked for: %s", call)
		}
	}
}

func TestServerPrune_FailureIsReported(t *testing.T) {
	m := ssh.NewMockExecutor("192.0.2.10",
		ssh.MockCommand{Match: "docker builder prune -a -f", Err: errors.New("docker: no space left on device")},
		ssh.MockCommand{Match: "python3 -c", Output: `{"seq": 1, "ts": "2026-11-05T12:00:00Z"}`},
	)
	_, err := executeServerPrune(context.Background(), m, "edge", true, false)
	if err == nil {
		t.Fatal("a failed prune must surface its error")
	}
}

func TestServerPruneFlagValidation(t *testing.T) {
	var out bytes.Buffer
	cmd := newServerPruneCmd(&Flags{})
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "say what to prune") {
		t.Errorf("no flags: %v", err)
	}

	cmd = newServerPruneCmd(&Flags{})
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--build-cache"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "--host is required") {
		t.Errorf("missing --host: %v", err)
	}
}
