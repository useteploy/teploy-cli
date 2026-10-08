package preview

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/ssh"
)

type round3PreviewExecutor struct {
	*ssh.MockExecutor
	absent           bool
	failAuthority    bool
	lostRemoveReply  bool
	unknownInventory bool
	authorityPath    string
}

func (e *round3PreviewExecutor) Run(ctx context.Context, command string) (string, error) {
	if strings.Contains(command, "docker ps --all --no-trunc") {
		if e.unknownInventory {
			return "", errors.New("daemon unavailable")
		}
		if e.absent {
			return "", nil
		}
		return `{"ID":"owned-preview-id","Names":"demo-preview","State":"exited","Labels":{"teploy.app":"demo"}}`, nil
	}
	if strings.Contains(command, "docker rm 'owned-preview-id'") {
		e.absent = true
		if e.lostRemoveReply {
			e.lostRemoveReply = false
			return "", errors.New("lost remove reply")
		}
		return "", nil
	}
	if strings.Contains(command, "rm -f -- '"+e.authorityPath+"'") && e.failAuthority {
		e.failAuthority = false
		return "", errors.New("authority unlink failed")
	}
	return e.MockExecutor.Run(ctx, command)
}
func TestRound3PreviewDestroyConvergesAfterRemovalAndAuthorityFailure(t *testing.T) {
	for _, lostReply := range []bool{false, true} {
		mock := previewMockExecutor("host")
		path := previewStatePath("demo", "feature")
		record, _ := json.Marshal(State{Branch: "feature", ID: PreviewID("demo", "feature"), Container: "demo-preview", Route: "demo-route"})
		mock.Files[path] = record
		mock.Files["/deployments/caddy/Caddyfile"] = []byte("# no preview route\n")
		executor := &round3PreviewExecutor{MockExecutor: mock, authorityPath: path, failAuthority: true, lostRemoveReply: lostReply}
		manager := NewManager(executor, &bytes.Buffer{})
		if err := manager.Destroy(context.Background(), "demo", "feature"); err == nil {
			t.Fatal("authority failure reported success")
		}
		if !executor.absent || len(mock.Files[path]) == 0 {
			t.Fatal("partial teardown did not retain authority")
		}
		if err := manager.Destroy(context.Background(), "demo", "feature"); err != nil {
			t.Fatalf("proven absence not retryable: %v", err)
		}
		if _, exists := mock.Files[path]; exists {
			t.Fatal("converged retry retained authority")
		}
	}
}
func TestRound3PreviewUnknownInventoryRetainsAuthority(t *testing.T) {
	mock := previewMockExecutor("host")
	path := previewStatePath("demo", "feature")
	record, _ := json.Marshal(State{Branch: "feature", ID: PreviewID("demo", "feature"), Container: "demo-preview"})
	mock.Files[path] = record
	mock.Files["/deployments/caddy/Caddyfile"] = []byte("# no preview route\n")
	executor := &round3PreviewExecutor{MockExecutor: mock, authorityPath: path, unknownInventory: true}
	if err := NewManager(executor, &bytes.Buffer{}).Destroy(context.Background(), "demo", "feature"); err == nil {
		t.Fatal("unknown Docker state treated as absence")
	}
	if string(mock.Files[path]) != string(record) {
		t.Fatal("unknown outcome erased authority")
	}
}
