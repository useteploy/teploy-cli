package cli

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/ssh"
)

const removeTestCaddyfile = `example.com {
	reverse_proxy other:80
}

# TEPLOY BEGIN scratch
scratch.com, www.scratch.com {
	reverse_proxy scratch-web-aaa:80
}
# TEPLOY END scratch
`

// caddyMocks returns the command responses the caddy mutate path needs.
func caddyMocks() []ssh.MockCommand {
	return []ssh.MockCommand{
		{Match: "[ -f /deployments/caddy/Caddyfile ]", Output: ""},
		{Match: "mkdir /deployments/caddy/.lock", Output: ""},
		{Match: "cat /deployments/caddy/Caddyfile", Output: removeTestCaddyfile},
		{Match: "mv /tmp/teploy_caddyfile.tmp", Output: ""},
		{Match: "docker exec caddy caddy reload", Output: ""},
		{Match: "rm -rf /deployments/caddy/.lock", Output: ""},
		{Match: "rmdir /deployments/caddy/.lock", Output: ""},
	}
}

func removeTestContainers() ssh.MockCommand {
	return ssh.MockCommand{
		Match: "docker ps --all --filter label=teploy.app=",
		Output: `{"ID":"aaa111","Names":"scratch-web-aaa","Image":"scratch-build-aaa","State":"running","Status":"Up 2 days","Labels":"teploy.app=scratch,teploy.process=web"}
{"ID":"bbb222","Names":"scratch-db","Image":"postgres:16","State":"running","Status":"Up 2 days","Labels":"teploy.app=scratch,teploy.role=accessory"}`,
	}
}

func TestExecuteRemoveDefaultPreservesData(t *testing.T) {
	cmds := append([]ssh.MockCommand{
		removeTestContainers(),
		{Match: "docker stop -t 10 'scratch-web-aaa'", Output: ""},
		{Match: "docker rm 'scratch-web-aaa'", Output: ""},
		{Match: "find '/deployments/scratch'", Output: ""},
		{Match: "rmdir '/deployments/scratch'", Output: ""},
		{Match: "ls '/deployments/scratch'", Output: "volumes"},
	}, caddyMocks()...)
	exec := ssh.NewMockExecutor("test-host", cmds...)

	sum, err := executeRemove(context.Background(), exec, "scratch",
		[]string{"scratch.com", "www.scratch.com"}, removeOptions{}, io.Discard)
	if err != nil {
		t.Fatalf("executeRemove: %v", err)
	}

	if len(sum.RemovedContainers) != 1 || sum.RemovedContainers[0] != "scratch-web-aaa" {
		t.Errorf("removed containers = %v, want [scratch-web-aaa]", sum.RemovedContainers)
	}
	if len(sum.KeptAccessories) != 1 || sum.KeptAccessories[0] != "scratch-db" {
		t.Errorf("kept accessories = %v, want [scratch-db]", sum.KeptAccessories)
	}
	if sum.Route != "removed" {
		t.Errorf("route = %q, want removed", sum.Route)
	}
	if len(sum.PreservedData) != 1 || sum.PreservedData[0] != "/deployments/scratch/volumes" {
		t.Errorf("preserved = %v, want [/deployments/scratch/volumes]", sum.PreservedData)
	}

	written := string(exec.Files["/tmp/teploy_caddyfile.tmp"])
	if written == "" {
		t.Fatal("no Caddyfile written")
	}
	if strings.Contains(written, "TEPLOY BEGIN scratch") {
		t.Error("managed scratch block not removed from Caddyfile")
	}
	if !strings.Contains(written, "example.com {") {
		t.Error("unrelated block was dropped from Caddyfile")
	}

	// The accessory container must never be stopped or removed by default.
	for _, call := range exec.Calls {
		if strings.Contains(call, "scratch-db") && (strings.HasPrefix(call, "docker stop") || strings.HasPrefix(call, "docker rm")) {
			t.Errorf("accessory container touched without --purge: %s", call)
		}
	}
}

func TestExecuteRemovePurge(t *testing.T) {
	cmds := append([]ssh.MockCommand{
		removeTestContainers(),
		{Match: "docker stop -t 10", Output: ""},
		{Match: "docker rm ", Output: ""},
		{Match: "rm -rf '/deployments/scratch'", Output: ""},
	}, caddyMocks()...)
	exec := ssh.NewMockExecutor("test-host", cmds...)

	sum, err := executeRemove(context.Background(), exec, "scratch",
		nil, removeOptions{Purge: true}, io.Discard)
	if err != nil {
		t.Fatalf("executeRemove: %v", err)
	}
	if len(sum.RemovedContainers) != 2 {
		t.Errorf("removed containers = %v, want both (web + accessory)", sum.RemovedContainers)
	}
	if len(sum.KeptAccessories) != 0 {
		t.Errorf("kept accessories = %v, want none under --purge", sum.KeptAccessories)
	}

	purged := false
	for _, call := range exec.Calls {
		if strings.HasPrefix(call, "rm -rf '/deployments/scratch'") {
			purged = true
		}
	}
	if !purged {
		t.Error("expected rm -rf of the whole app dir under --purge")
	}
}

func TestExecuteRemoveRedirect(t *testing.T) {
	cmds := append([]ssh.MockCommand{
		removeTestContainers(),
		{Match: "docker stop -t 10", Output: ""},
		{Match: "docker rm ", Output: ""},
		{Match: "find '/deployments/scratch'", Output: ""},
		{Match: "rmdir '/deployments/scratch'", Output: ""},
		{Match: "ls '/deployments/scratch'", Output: ""},
	}, caddyMocks()...)
	exec := ssh.NewMockExecutor("test-host", cmds...)

	sum, err := executeRemove(context.Background(), exec, "scratch",
		[]string{"scratch.com", "www.scratch.com"},
		removeOptions{Redirect: "https://akiroo.com"}, io.Discard)
	if err != nil {
		t.Fatalf("executeRemove: %v", err)
	}
	if sum.Route != "redirected" {
		t.Errorf("route = %q, want redirected", sum.Route)
	}

	written := string(exec.Files["/tmp/teploy_caddyfile.tmp"])
	if strings.Contains(written, "TEPLOY BEGIN scratch") {
		t.Error("managed block should be gone after redirect")
	}
	want := "scratch.com, www.scratch.com {\n\tredir https://akiroo.com permanent\n}"
	if !strings.Contains(written, want) {
		t.Errorf("redirect block missing.\nwant substring:\n%s\ngot:\n%s", want, written)
	}
}

func TestExecuteRemoveNoCaddyfile(t *testing.T) {
	cmds := []ssh.MockCommand{
		removeTestContainers(),
		{Match: "docker stop -t 10", Output: ""},
		{Match: "docker rm ", Output: ""},
		// No "[ -f ... ]" mock: the check fails, so no Caddy edit happens.
		{Match: "find '/deployments/scratch'", Output: ""},
		{Match: "rmdir '/deployments/scratch'", Output: ""},
		{Match: "ls '/deployments/scratch'", Output: ""},
	}
	exec := ssh.NewMockExecutor("test-host", cmds...)

	sum, err := executeRemove(context.Background(), exec, "scratch",
		nil, removeOptions{}, io.Discard)
	if err != nil {
		t.Fatalf("executeRemove: %v", err)
	}
	if sum.Route != "none" {
		t.Errorf("route = %q, want none", sum.Route)
	}
	for _, call := range exec.Calls {
		if strings.HasPrefix(call, "cat /deployments/caddy/Caddyfile") {
			t.Error("Caddyfile was read despite the existence check failing")
		}
	}
}

func TestSplitDomains(t *testing.T) {
	got := splitDomains("a.com, www.a.com,b.com , ")
	want := []string{"a.com", "www.a.com", "b.com"}
	if len(got) != len(want) {
		t.Fatalf("splitDomains = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("splitDomains[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
