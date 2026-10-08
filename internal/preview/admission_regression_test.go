package preview

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/useteploy/teploy/internal/ssh"
	"sort"
	"strings"
	"sync"
	"testing"
)

func TestPreviewAuthorityFailurePreservesAccess(t *testing.T) {
	mock := previewMockExecutor("test", ssh.MockCommand{Match: "cat " + previewStatePath("demo", "feature"), Err: errors.New("permission denied")})
	mgr := NewManager(mock, &bytes.Buffer{})
	if err := mgr.Deploy(context.Background(), DeployConfig{App: "demo", Branch: "feature", Repo: "repo", Image: "nginx", BaseDomain: "example.com"}); err == nil {
		t.Fatal("unknown authority must fail closed")
	}
	for _, call := range mock.Calls {
		if !strings.HasPrefix(call, "if test -f ") {
			t.Fatalf("effect after failed authority read: %s", call)
		}
	}
}

func TestPreviewKnownShortHashCollisionRefusesOldIdentity(t *testing.T) {
	left, right := "audit-branch-68411", "audit-branch-91902"
	if PreviewID("demo", left) == PreviewID("demo", right) {
		t.Fatal("known 32-bit collision survived")
	}
	mock := previewMockExecutor("test")
	record, _ := json.Marshal(State{Branch: left, Repo: "repo", ID: "demo-p-65337a47"})
	mock.Files[previewDir("demo")+"/demo-p-65337a47.json"] = record
	mgr := NewManager(mock, &bytes.Buffer{})
	err := mgr.Destroy(context.Background(), "demo", right)
	var ambiguous *AmbiguousPreviewError
	if !errors.As(err, &ambiguous) {
		t.Fatalf("old canonical collision must refuse: %v", err)
	}
	if len(mock.Files) != 1 {
		t.Fatal("collision record changed")
	}
}

func TestPreviewTeardownFailureRetainsAuthority(t *testing.T) {
	mock := previewMockExecutor("test", ssh.MockCommand{Match: "cat /deployments/caddy/Caddyfile", Err: errors.New("transport down")})
	path := previewStatePath("demo", "feature")
	record, _ := json.Marshal(State{Branch: "feature", ID: PreviewID("demo", "feature"), Container: "demo-candidate", Route: "demo-route"})
	mock.Files[path] = record
	if err := NewManager(mock, &bytes.Buffer{}).Destroy(context.Background(), "demo", "feature"); err == nil {
		t.Fatal("failed route teardown reported success")
	}
	if string(mock.Files[path]) != string(record) {
		t.Fatal("failed teardown erased authority")
	}
}

func previewMockExecutor(host string, commands ...ssh.MockCommand) *ssh.MockExecutor {
	// The destroy path proves teardown (R2-07/r5): route absence activation,
	// full immutable-ID inventory, and idempotent stop/remove of whatever
	// the inventory still shows. An empty inventory models a server whose
	// preview container is already gone.
	commands = append(commands, ssh.MockCommand{Match: "docker inspect -f '{{.Image}}'", Output: "sha256:" + strings.Repeat("d", 64)}, ssh.MockCommand{Match: "mkdir -p", Output: ""}, ssh.MockCommand{Match: "mkdir /deployments/", Output: ""}, ssh.MockCommand{Match: "mv -f --", Output: ""}, ssh.MockCommand{Match: "rm -rf", Output: ""}, ssh.MockCommand{Match: "docker ps --all --no-trunc --format", Output: ""}, ssh.MockCommand{Match: "docker exec caddy caddy reload", Output: ""}, ssh.MockCommand{Match: "docker stop", Output: ""}, ssh.MockCommand{Match: "docker rm", Output: ""})
	return ssh.NewMockExecutor(host, commands...)
}

// previewInventoryExecutor models the destroy path's real dependency: the
// full immutable-ID inventory changes as effects land. A stopped container
// reports exited; a removed one leaves the inventory. Tests that assert a
// preview's container was (not) stopped wrap previewMockExecutor with it.
type previewInventoryExecutor struct {
	*ssh.MockExecutor
	mu         sync.Mutex
	containers map[string]string // container name -> immutable 64-hex ID
	stopped    map[string]bool
}

func newPreviewInventoryExecutor(mock *ssh.MockExecutor, names ...string) *previewInventoryExecutor {
	e := &previewInventoryExecutor{MockExecutor: mock, containers: map[string]string{}, stopped: map[string]bool{}}
	for i, name := range names {
		e.containers[name] = strings.Repeat("a", 62) + fmt.Sprintf("%02d", i+1)
	}
	return e
}
func (e *previewInventoryExecutor) Run(ctx context.Context, cmd string) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	// Effects arrive composed behind the app-lease fence guard; the effect
	// is the tail after the guard fragment.
	if i := strings.LastIndex(cmd, "exit 75; }; "); i >= 0 && strings.HasPrefix(cmd, "grep -q ") {
		cmd = cmd[i+len("exit 75; }; "):]
	}
	if strings.Contains(cmd, "docker ps --all --no-trunc --format") {
		var rows []string
		for name, id := range e.containers {
			state := "running"
			if e.stopped[id] {
				state = "exited"
			}
			rows = append(rows, fmt.Sprintf(`{"ID":%q,"Names":%q,"State":%q,"Labels":{"teploy.app":"demo"}}`, id, name, state))
		}
		sort.Strings(rows)
		return strings.Join(rows, "\n"), nil
	}
	for prefix, apply := range map[string]func(string){
		"docker stop -t 5 ": func(id string) { e.stopped[id] = true },
		"docker rm ":        func(id string) { delete(e.containers, nameOfID(e.containers, id)) },
	} {
		if rest, ok := strings.CutPrefix(cmd, prefix); ok {
			apply(strings.Trim(strings.TrimSpace(rest), "'"))
		}
	}
	return e.MockExecutor.Run(ctx, cmd)
}
func nameOfID(containers map[string]string, id string) string {
	for name, containerID := range containers {
		if containerID == id {
			return name
		}
	}
	return ""
}
