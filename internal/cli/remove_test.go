package cli

import (
	"context"
	"fmt"
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
		ssh.MockCommand{Match: "a=$(docker exec caddy md5sum", Output: "TEPLOY_CADDY_OK"},
		{Match: "cat /deployments/caddy/Caddyfile", Output: removeTestCaddyfile},
		{Match: "mv -f -- ", Output: ""},
		{Match: "docker exec caddy caddy reload", Output: ""},
		{Match: "rm -rf /deployments/caddy/.lock", Output: ""},
		{Match: "rmdir /deployments/caddy/.lock", Output: ""},
	}
}

// removeAutomationMocks answers the sudo probe and the one-round-trip
// unit/cron teardown with "nothing installed".
func removeAutomationMocks() []ssh.MockCommand {
	return []ssh.MockCommand{
		{Match: "id -u", Output: "1000"},
		{Match: "reload=0; ", Output: ""},
	}
}

func removeTestContainers() ssh.MockCommand {
	return ssh.MockCommand{
		Match: "docker ps --all --filter label=teploy.app=",
		Output: `{"ID":"aaa111","Names":"scratch-web-aaa","Image":"scratch-build-aaa","State":"running","Status":"Up 2 days","Labels":"teploy.app=scratch,teploy.process=web"}
{"ID":"bbb222","Names":"scratch-db","Image":"postgres:16","State":"running","Status":"Up 2 days","Labels":"teploy.app=scratch,teploy.role=accessory"}`,
	}
}

func TestExecuteRemoveStopsRestartingContainer(t *testing.T) {
	cmds := append([]ssh.MockCommand{
		{
			Match:  "docker ps --all --filter label=teploy.app=",
			Output: `{"ID":"ccc333","Names":"scratch-worker-ccc","Image":"scratch-build-ccc","State":"restarting","Status":"Restarting (1) 2 seconds ago","Labels":"teploy.app=scratch,teploy.process=worker"}`,
		},
		{Match: "docker stop -t 10 'scratch-worker-ccc'", Output: ""},
		{Match: "docker rm 'scratch-worker-ccc'", Output: ""},
		{Match: "find '/deployments/scratch'", Output: ""},
		{Match: "rmdir '/deployments/scratch'", Output: ""},
		{Match: "ls '/deployments/scratch'", Output: ""},
	}, caddyMocks()...)
	exec := ssh.NewMockExecutor("test-host", append(cmds, removeAutomationMocks()...)...)

	if _, err := executeRemove(context.Background(), exec, "scratch",
		[]string{"scratch.com"}, removeOptions{}, io.Discard); err != nil {
		t.Fatalf("executeRemove: %v", err)
	}
	var stopped bool
	for _, call := range exec.Calls {
		if call == "docker stop -t 10 'scratch-worker-ccc'" {
			stopped = true
		}
	}
	if !stopped {
		t.Errorf("expected a restarting container to be stopped before rm; calls: %v", exec.Calls)
	}
}

func TestExecuteRemovePurgeFallsBackToContainerForForeignOwnedData(t *testing.T) {
	cmds := append([]ssh.MockCommand{
		removeTestContainers(),
		{Match: "docker stop -t 10", Output: ""},
		{Match: "docker rm ", Output: ""},
		{Match: "rm -rf '/deployments/scratch'", Err: fmt.Errorf("rm: cannot remove: Permission denied")},
		{Match: "docker run --rm --user 0 -v '/deployments/scratch:/teploy-data' alpine:3 sh -c 'rm -rf /teploy-data/* /teploy-data/.[!.]*'", Output: ""},
		{Match: "rmdir '/deployments/scratch'", Output: ""},
	}, caddyMocks()...)
	exec := ssh.NewMockExecutor("test-host", append(cmds, removeAutomationMocks()...)...)

	if _, err := executeRemove(context.Background(), exec, "scratch",
		[]string{"scratch.com"}, removeOptions{Purge: true}, io.Discard); err != nil {
		t.Fatalf("executeRemove: %v", err)
	}
	var viaContainer bool
	for _, call := range exec.Calls {
		if strings.HasPrefix(call, "docker run --rm --user 0 -v '/deployments/scratch:/teploy-data'") {
			viaContainer = true
		}
	}
	if !viaContainer {
		t.Errorf("expected the purge to fall back to an in-container rm; calls: %v", exec.Calls)
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
	exec := ssh.NewMockExecutor("test-host", append(cmds, removeAutomationMocks()...)...)

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
	// TCL-56: retained data must keep its credentials — the state deletion
	// excludes secrets/ and .env alongside volumes/ and accessories/.
	var findCmd string
	for _, c := range exec.Calls {
		if strings.HasPrefix(c, "find '/deployments/scratch'") {
			findCmd = c
		}
	}
	for _, keep := range []string{"! -name 'volumes'", "! -name 'accessories'", "! -name 'secrets'", "! -name '.env'"} {
		if !strings.Contains(findCmd, keep) {
			t.Errorf("state deletion missing keep exclusion %s: %s", keep, findCmd)
		}
	}

	written := string(exec.Files["/deployments/caddy/Caddyfile"])
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
	exec := ssh.NewMockExecutor("test-host", append(cmds, removeAutomationMocks()...)...)

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
	exec := ssh.NewMockExecutor("test-host", append(cmds, removeAutomationMocks()...)...)

	sum, err := executeRemove(context.Background(), exec, "scratch",
		[]string{"scratch.com", "www.scratch.com"},
		removeOptions{Redirect: "https://akiroo.com"}, io.Discard)
	if err != nil {
		t.Fatalf("executeRemove: %v", err)
	}
	if sum.Route != "redirected" {
		t.Errorf("route = %q, want redirected", sum.Route)
	}

	written := string(exec.Files["/deployments/caddy/Caddyfile"])
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
	exec := ssh.NewMockExecutor("test-host", append(cmds, removeAutomationMocks()...)...)

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

func TestExecuteRemoveHoldsAppLockAndRetiresAutomation(t *testing.T) {
	cmds := append([]ssh.MockCommand{
		{Match: "[ -d '/deployments/scratch' ]", Output: ""},
		{Match: "mkdir /deployments/scratch/.lock", Output: ""},
		{Match: "id -u", Output: "1000"},
		{Match: "reload=0; ", Output: "unit:teploy-webhook-scratch.service\nunit:teploy-heal-scratch.timer\ncron:1\n"},
		removeTestContainers(),
		{Match: "docker stop -t 10 'scratch-web-aaa'", Output: ""},
		{Match: "docker rm 'scratch-web-aaa'", Output: ""},
		{Match: "find '/deployments/scratch'", Output: ""},
		{Match: "rmdir '/deployments/scratch'", Output: ""},
		{Match: "ls '/deployments/scratch'", Output: ""},
	}, caddyMocks()...)
	exec := ssh.NewMockExecutor("test-host", cmds...)

	sum, err := executeRemove(context.Background(), exec, "scratch",
		[]string{"scratch.com"}, removeOptions{}, io.Discard)
	if err != nil {
		t.Fatalf("executeRemove: %v", err)
	}
	if len(sum.RemovedAutomation) != 3 {
		t.Errorf("removed automation = %v, want webhook unit, heal timer and the cron entry", sum.RemovedAutomation)
	}

	idx := func(prefix string) int {
		for i, c := range exec.Calls {
			if strings.HasPrefix(c, prefix) {
				return i
			}
		}
		return -1
	}
	lock, teardown, stop, find := idx("mkdir /deployments/scratch/.lock"), idx("reload=0; "), idx("docker stop"), idx("find '/deployments/scratch'")
	release := -1
	for i, c := range exec.Calls {
		if strings.HasPrefix(c, "rm -rf -- '/deployments/scratch/.lock'") {
			release = i
		}
	}
	if lock < 0 || teardown < lock || stop < teardown || find < stop || release < find {
		t.Fatalf("want lock -> teardown -> containers -> state -> release, got lock=%d teardown=%d stop=%d find=%d release=%d\ncalls: %v",
			lock, teardown, stop, find, release, exec.Calls)
	}
	if !strings.Contains(exec.Calls[find], "! -name '.lock'") {
		t.Errorf("state deletion must keep the held lock until release: %s", exec.Calls[find])
	}

	script := exec.Calls[teardown]
	for _, want := range []string{"teploy-webhook-scratch.service", "teploy-heal-scratch.timer", "teploy-secret-audit-scratch.timer", "/deployments/scratch/scheduled-redeploy.sh"} {
		if !strings.Contains(script, want) {
			t.Errorf("teardown script misses %s", want)
		}
	}
	if strings.Contains(script, "teploy-backup:scratch") {
		t.Error("a default remove keeps the data, so it must keep its backup schedule")
	}
}

func TestExecuteRemovePurgeDropsBackupSchedules(t *testing.T) {
	exec := ssh.NewMockExecutor("test-host",
		ssh.MockCommand{Match: "id -u", Output: "0"},
		ssh.MockCommand{Match: "reload=0; ", Output: ""},
	)
	if _, err := removeAppAutomation(context.Background(), exec, "scratch", true); err != nil {
		t.Fatal(err)
	}
	script := exec.Calls[len(exec.Calls)-1]
	for _, want := range []string{"[teploy-backup:scratch]", "[teploy-accessory-backup:scratch:"} {
		if !strings.Contains(script, want) {
			t.Errorf("purge teardown misses %s", want)
		}
	}
	if strings.Contains(script, "sudo ") {
		t.Error("root needs no sudo")
	}
}

func TestExecuteRemoveRefusesWhileDeployRuns(t *testing.T) {
	exec := ssh.NewMockExecutor("test-host",
		ssh.MockCommand{Match: "[ -d '/deployments/scratch' ]", Output: ""},
		ssh.MockCommand{Match: "mkdir /deployments/scratch/.lock", Err: fmt.Errorf("exit status 1")},
		ssh.MockCommand{Match: "cat /deployments/scratch/.lock/info", Output: `{"type":"auto","owner":"x","ts":"2099-01-01T00:00:00Z"}`},
	)
	_, err := executeRemove(context.Background(), exec, "scratch", nil, removeOptions{}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "in progress") {
		t.Fatalf("want an in-progress refusal, got %v", err)
	}
	for _, c := range exec.Calls {
		if strings.HasPrefix(c, "docker") || strings.HasPrefix(c, "reload=0") {
			t.Fatalf("nothing may be touched while another operation holds the lock; ran %s", c)
		}
	}
}
