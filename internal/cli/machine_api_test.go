package cli

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/useteploy/teploy/internal/ssh"
)

func TestMachineCommandsRegistered(t *testing.T) {
	root := NewRootCmd("test")
	for _, args := range [][]string{{"app", "list"}, {"server", "status"}} {
		cmd, _, err := root.Find(args)
		if err != nil {
			t.Fatalf("finding %v: %v", args, err)
		}
		if cmd == root || cmd.Name() != args[len(args)-1] {
			t.Fatalf("command %v not registered", args)
		}
	}
}

func TestCollectAppListCanonicalState(t *testing.T) {
	observedAt := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	container := `{"ID":"abc","Names":"blog-web-v2","Image":"example/blog:v2","State":"running","Status":"Up 2 hours","CreatedAt":"2026-07-22 10:00:00 +0000 UTC","Labels":"teploy.app=blog,teploy.process=web,teploy.version=v2"}`
	mock := ssh.NewMockExecutor("192.0.2.10",
		ssh.MockCommand{Match: "for f in /deployments/*/state.json", Output: "blog\n"},
		ssh.MockCommand{Match: "cat -- /deployments/blog/state.json", Output: `{"schema_version":2,"deployment_type":"container","ingress_mode":"external","domain":"blog.example.com","current_hash":"v2","current_ports":[49153],"previous_hash":"v1","previous_ports":[49152]}`},
		ssh.MockCommand{Match: "docker ps --all", Output: container},
		ssh.MockCommand{Match: "cat /deployments/blog/.lock/info", Output: `{"type":"manual","user":"alice","ts":"2026-07-22T11:00:00Z"}`},
		ssh.MockCommand{Match: "test -f /deployments/blog/.maintenance-block", Output: ""},
	)

	got := collectAppList(context.Background(), mock, observedAt)
	if len(got.Errors) != 0 || len(got.Apps) != 1 {
		t.Fatalf("unexpected app list: %#v", got)
	}
	app := got.Apps[0]
	if app.App != "blog" || app.Domain != "blog.example.com" || app.Type != "container" || app.Ingress != "external" {
		t.Fatalf("unexpected app identity: %#v", app)
	}
	if app.CurrentRelease.Version != "v2" || app.PreviousRelease.Version != "v1" {
		t.Fatalf("unexpected releases: %#v %#v", app.CurrentRelease, app.PreviousRelease)
	}
	if len(app.Processes) != 1 || app.Processes[0].Name != "web" || app.Processes[0].Running != 1 {
		t.Fatalf("unexpected processes: %#v", app.Processes)
	}
	if app.Lock == nil || app.Lock.User != "alice" || !app.Maintenance {
		t.Fatalf("unexpected lock/maintenance: lock=%#v maintenance=%v", app.Lock, app.Maintenance)
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if _, ok := decoded["observed_at"]; !ok {
		t.Fatalf("missing observed_at: %s", raw)
	}
}

func TestCollectAppListEmptyAndPartialError(t *testing.T) {
	empty := ssh.NewMockExecutor("empty", ssh.MockCommand{Match: "for f in /deployments/*/state.json", Output: ""})
	got := collectAppList(context.Background(), empty, time.Now())
	if got.Apps == nil || got.Errors == nil || len(got.Apps) != 0 || len(got.Errors) != 0 {
		t.Fatalf("empty response is not stable: %#v", got)
	}

	failed := ssh.NewMockExecutor("failed", ssh.MockCommand{Match: "for f in /deployments/*/state.json", Err: errors.New("permission denied")})
	got = collectAppList(context.Background(), failed, time.Now())
	if got.Apps == nil || len(got.Errors) != 1 || got.Errors[0].Scope != "apps" {
		t.Fatalf("partial error response = %#v", got)
	}
}

func TestCollectServerStatus(t *testing.T) {
	observedAt := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	container := `{"ID":"abc","Names":"blog-web-v2","Image":"example/blog:v2","State":"running","Status":"Up","CreatedAt":"2026-07-22 10:00:00 +0000 UTC","Labels":"teploy.app=blog,teploy.process=web,teploy.version=v2"}`
	image := `{"ID":"sha256:123","Repository":"example/blog","Tag":"v2","Size":"25MB","CreatedAt":"2026-07-22 09:00:00 +0000 UTC"}`
	caddy := `{"servers":{"srv0":{"routes":[{"match":[{"host":["blog.example.com"]}],"handle":[{"handler":"subroute","routes":[{"handle":[{"handler":"reverse_proxy","upstreams":[{"dial":"blog-web-v2:3000"}]}]}]}]}]}}}`
	mock := ssh.NewMockExecutor("192.0.2.10",
		ssh.MockCommand{Match: "cat /proc/uptime", Output: "3600.50 1200.00"},
		ssh.MockCommand{Match: "cat /proc/loadavg", Output: "0.10 0.20 0.30 1/100 1"},
		ssh.MockCommand{Match: "cat /proc/meminfo", Output: "MemTotal: 1000 kB\nMemAvailable: 400 kB\n"},
		ssh.MockCommand{Match: "df -B1 -P", Output: "Filesystem 1-blocks Used Available Capacity Mounted on\n/dev/vda1 1000 250 750 25% /\n"},
		ssh.MockCommand{Match: "docker version", Output: "29.0.0"},
		ssh.MockCommand{Match: "docker ps --all", Output: container},
		ssh.MockCommand{Match: "docker image ls", Output: image},
		ssh.MockCommand{Match: "docker exec caddy", Output: caddy},
	)

	got := collectServerStatus(context.Background(), mock, "prod", observedAt)
	if len(got.Errors) != 0 {
		t.Fatalf("unexpected errors: %#v", got.Errors)
	}
	if got.Uptime.Seconds != 3600.5 || got.Load.Fifteen != 0.3 {
		t.Fatalf("unexpected uptime/load: %#v %#v", got.Uptime, got.Load)
	}
	if got.Memory.TotalBytes != 1024000 || got.Memory.UsedBytes != 614400 {
		t.Fatalf("unexpected memory: %#v", got.Memory)
	}
	if len(got.Disks) != 1 || len(got.Docker.Containers) != 1 || len(got.Docker.Images) != 1 {
		t.Fatalf("unexpected inventory: %#v", got)
	}
	if !got.Caddy.Available || !hasCaddyUpstream(got.Caddy.Routes, "blog-web-v2:3000") {
		t.Fatalf("unexpected Caddy observation: %#v", got.Caddy)
	}
}

func hasCaddyUpstream(routes []caddyRouteDTO, want string) bool {
	for _, route := range routes {
		for _, upstream := range route.Upstreams {
			if upstream == want {
				return true
			}
		}
	}
	return false
}
