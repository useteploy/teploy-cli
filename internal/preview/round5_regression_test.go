package preview

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy/internal/ssh"
)

type renewalDuringLeaseExecutor struct {
	*ssh.MockExecutor
	path     string
	renewed  State
	didRenew bool
}

func (e *renewalDuringLeaseExecutor) Run(ctx context.Context, cmd string) (string, error) {
	if !e.didRenew && strings.Contains(cmd, ".lock") {
		e.didRenew = true
		data, _ := json.Marshal(e.renewed)
		e.Files[e.path] = data
	}
	return e.MockExecutor.Run(ctx, cmd)
}
func TestRound5ManualDestroyComparesListedGenerationAfterLease(t *testing.T) {
	mock := previewMockExecutor("host")
	path := previewStatePath("demo", "feature")
	before := State{ID: PreviewID("demo", "feature"), Branch: "feature", OwnershipID: "owner", Generation: 1, UpdatedAt: ptrTime(time.Now().UTC()), ExpiresAt: time.Now().Add(-time.Hour), Container: "demo-preview"}
	data, _ := json.Marshal(before)
	mock.Files[path] = data
	renewed := before
	renewed.Generation++
	renewed.UpdatedAt = ptrTime(before.UpdatedAt.Add(time.Second))
	exec := &renewalDuringLeaseExecutor{MockExecutor: mock, path: path, renewed: renewed}
	if err := NewManager(exec, io.Discard).Destroy(context.Background(), "demo", "feature"); err == nil {
		t.Fatal("renewed preview destroyed")
	}
	if !exec.didRenew {
		t.Fatal("fixture did not reach actual lease wrapper")
	}
	for _, cmd := range mock.Calls {
		if strings.Contains(cmd, "docker stop") || strings.Contains(cmd, "docker rm") || strings.Contains(cmd, "caddy reload") || strings.Contains(cmd, "rm -f -- '"+path+"'") {
			t.Fatalf("effect after renewal mismatch: %s", cmd)
		}
	}
	actual, _ := NewManager(mock, io.Discard).Observe(context.Background(), "demo", "feature")
	if actual == nil || actual.Generation != 2 {
		t.Fatal("authority erased", actual)
	}
}
func TestRound5CompareDestroyPartialRetryAndUnknownAbsence(t *testing.T) {
	mock := previewMockExecutor("host")
	path := previewStatePath("demo", "feature")
	admitted := State{ID: PreviewID("demo", "feature"), Branch: "feature", OwnershipID: "owner", Generation: 3, UpdatedAt: ptrTime(time.Now().UTC()), Container: "demo-preview", Route: "demo-route"}
	observed := admitted.UpdatedAt
	data, _ := json.Marshal(admitted)
	mock.Files[path] = data
	mock.Files["/deployments/caddy/Caddyfile"] = []byte("# absent route\n")
	exec := &round3PreviewExecutor{MockExecutor: mock, authorityPath: path, failAuthority: true}
	mgr := NewManager(exec, io.Discard)
	if err := mgr.CompareDestroy(context.Background(), "demo", "feature", "owner", 3, observed); err == nil {
		t.Fatal("partial teardown read as success")
	}
	if _, ok := mock.Files[path]; !ok {
		t.Fatal("partial teardown erased authority")
	}
	if err := mgr.CompareDestroy(context.Background(), "demo", "feature", "owner", 3, observed); err != nil {
		t.Fatal("same-generation retry refused proven absence", err)
	}
	if proved, err := mgr.ProveDestroyed(context.Background(), "demo", admitted); err != nil || !proved {
		t.Fatal("journal evidence could not prove teardown", proved, err)
	}
	exec.unknownInventory = true
	if proved, err := mgr.ProveDestroyed(context.Background(), "demo", admitted); err == nil || proved {
		t.Fatal("unknown inventory fabricated absence")
	}
}

func ptrTime(t time.Time) *time.Time { return &t }
