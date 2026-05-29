package docker

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/ssh"
)

// TestParseContainers_CreatedAtAndLabels covers the new fields added for
// version pruning. ParseContainers must extract CreatedAt and the
// teploy.version label so PruneVersions can group and rank by recency.
func TestParseContainers_CreatedAtAndLabels(t *testing.T) {
	output := `{"ID":"abc123","Names":"myapp-web-v1","Image":"myapp:v1","State":"running","Status":"Up 1h","CreatedAt":"2026-05-28 21:00:00 -0700 PDT","Labels":"teploy.app=myapp,teploy.process=web,teploy.version=v1"}`

	cs, err := ParseContainers(output)
	if err != nil {
		t.Fatalf("ParseContainers: %v", err)
	}
	if len(cs) != 1 {
		t.Fatalf("expected 1 container, got %d", len(cs))
	}
	c := cs[0]
	if c.CreatedAt != "2026-05-28 21:00:00 -0700 PDT" {
		t.Errorf("CreatedAt = %q", c.CreatedAt)
	}
	if c.Labels["teploy.version"] != "v1" {
		t.Errorf("Labels[teploy.version] = %q, want v1", c.Labels["teploy.version"])
	}
	if c.Labels["teploy.app"] != "myapp" {
		t.Errorf("Labels[teploy.app] = %q, want myapp", c.Labels["teploy.app"])
	}
}

// TestPruneVersions_KeepsNewest sets up three versions and verifies that
// only the oldest gets pruned when keep=2.
func TestPruneVersions_KeepsNewest(t *testing.T) {
	listOutput := strings.Join([]string{
		// v3 is the newest.
		mkPS("c1", "myapp-web-v3", "myapp:v3", "2026-05-28 21:30:00 -0700 PDT", "v3", "web"),
		mkPS("c2", "myapp-worker-v3", "myapp:v3", "2026-05-28 21:30:01 -0700 PDT", "v3", "worker"),
		mkPS("c3", "myapp-web-v2", "myapp:v2", "2026-05-28 21:00:00 -0700 PDT", "v2", "web"),
		mkPS("c4", "myapp-worker-v2", "myapp:v2", "2026-05-28 21:00:01 -0700 PDT", "v2", "worker"),
		mkPS("c5", "myapp-web-v1", "myapp:v1", "2026-05-28 20:00:00 -0700 PDT", "v1", "web"),
		mkPS("c6", "myapp-worker-v1", "myapp:v1", "2026-05-28 20:00:01 -0700 PDT", "v1", "worker"),
	}, "\n")

	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "docker ps --all --filter label=teploy.app=myapp", Output: listOutput},
		ssh.MockCommand{Match: "docker rm -f myapp-web-v1", Output: ""},
		ssh.MockCommand{Match: "docker rm -f myapp-worker-v1", Output: ""},
		ssh.MockCommand{Match: "docker rmi myapp:v1", Output: ""},
	)
	client := NewClient(mock)

	pruned, err := client.PruneVersions(context.Background(), "myapp", 2, "v3")
	if err != nil {
		t.Fatalf("PruneVersions: %v", err)
	}
	if len(pruned) != 1 || pruned[0] != "v1" {
		t.Errorf("expected ['v1'] pruned, got %v", pruned)
	}
	// v2 and v3 must NOT appear in any rm command.
	for _, call := range mock.Calls {
		if strings.HasPrefix(call, "docker rm -f") || strings.HasPrefix(call, "docker rmi") {
			if strings.Contains(call, "v2") || strings.Contains(call, "v3") {
				t.Errorf("v2 or v3 should not be removed; got: %s", call)
			}
		}
	}
}

// TestPruneVersions_ProtectsExplicit ensures explicitly-named protected
// versions stay even if they're not in the top-`keep` newest. This is the
// safety net for the same-version redeploy edge case.
func TestPruneVersions_ProtectsExplicit(t *testing.T) {
	listOutput := strings.Join([]string{
		mkPS("c1", "myapp-web-vA", "myapp:vA", "2026-05-28 21:30:00 -0700 PDT", "vA", "web"),
		mkPS("c2", "myapp-web-vB", "myapp:vB", "2026-05-28 21:00:00 -0700 PDT", "vB", "web"),
		mkPS("c3", "myapp-web-vC", "myapp:vC", "2026-05-28 20:00:00 -0700 PDT", "vC", "web"),
	}, "\n")

	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "docker ps --all --filter label=teploy.app=myapp", Output: listOutput},
		ssh.MockCommand{Match: "docker rm -f myapp-web-vB", Output: ""},
		ssh.MockCommand{Match: "docker rmi myapp:vB", Output: ""},
	)
	client := NewClient(mock)

	// keep=1 (so only vA is in top-newest), but protect vC explicitly.
	// vB is the only one that should be pruned.
	pruned, err := client.PruneVersions(context.Background(), "myapp", 1, "vC")
	if err != nil {
		t.Fatalf("PruneVersions: %v", err)
	}
	if len(pruned) != 1 || pruned[0] != "vB" {
		t.Errorf("expected ['vB'] pruned, got %v", pruned)
	}
}

// TestPruneVersions_NoOp confirms that with no containers (or all within
// keep) nothing is removed and nothing returned.
func TestPruneVersions_NoOp(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "docker ps --all --filter label=teploy.app=myapp", Output: ""},
	)
	client := NewClient(mock)
	pruned, err := client.PruneVersions(context.Background(), "myapp", 5)
	if err != nil {
		t.Fatalf("PruneVersions: %v", err)
	}
	if len(pruned) != 0 {
		t.Errorf("expected nothing pruned, got %v", pruned)
	}
}

// TestPruneVersions_IgnoresUnlabeled verifies that accessory containers
// (caddy, postgres) without a teploy.version label are not considered
// candidates for pruning.
func TestPruneVersions_IgnoresUnlabeled(t *testing.T) {
	listOutput := strings.Join([]string{
		mkPS("c1", "myapp-web-v2", "myapp:v2", "2026-05-28 21:30:00 -0700 PDT", "v2", "web"),
		mkPS("c2", "myapp-web-v1", "myapp:v1", "2026-05-28 20:00:00 -0700 PDT", "v1", "web"),
		// caddy has the app label but no version label.
		`{"ID":"cc","Names":"caddy","Image":"caddy:2","State":"running","Status":"Up","CreatedAt":"2026-05-01 00:00:00 -0700 PDT","Labels":"teploy.app=myapp,teploy.role=accessory"}`,
	}, "\n")

	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "docker ps --all --filter label=teploy.app=myapp", Output: listOutput},
		ssh.MockCommand{Match: "docker rm -f myapp-web-v1", Output: ""},
		ssh.MockCommand{Match: "docker rmi myapp:v1", Output: ""},
	)
	client := NewClient(mock)

	_, err := client.PruneVersions(context.Background(), "myapp", 1)
	if err != nil {
		t.Fatalf("PruneVersions: %v", err)
	}
	for _, call := range mock.Calls {
		if strings.Contains(call, "caddy") && (strings.HasPrefix(call, "docker rm") || strings.HasPrefix(call, "docker rmi")) {
			t.Errorf("caddy must never be pruned; got: %s", call)
		}
	}
}

func mkPS(id, name, image, createdAt, version, process string) string {
	return fmt.Sprintf(
		`{"ID":%q,"Names":%q,"Image":%q,"State":"exited","Status":"Exited (0) 1h ago","CreatedAt":%q,"Labels":"teploy.app=myapp,teploy.process=%s,teploy.version=%s"}`,
		id, name, image, createdAt, process, version,
	)
}
