package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy/internal/ssh"
)

func b64(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}

func snapshotLines(lines ...string) string {
	return strings.Join(lines, "\n")
}

func TestCollectAppListCanonicalState(t *testing.T) {
	observedAt := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	stateJSON := `{"schema_version":2,"deployment_type":"container","ingress_mode":"external","domain":"blog.example.com","current_hash":"v2","current_ports":[49153],"previous_hash":"v1","previous_ports":[49152]}`
	lockJSON := `{"type":"manual","user":"alice","ts":"2026-07-22T11:00:00Z"}`
	container := `{"ID":"abc","Names":"blog-web-v2","Image":"example/blog:v2","State":"running","Status":"Up 2 hours","CreatedAt":"2026-07-22 10:00:00 +0000 UTC","Labels":{"teploy.app":"blog","teploy.process":"web","teploy.version":"v2"}}`
	mock := ssh.NewMockExecutor("192.0.2.10",
		ssh.MockCommand{Match: "for f in /deployments/*/state.json", Output: "blog\n"},
		ssh.MockCommand{Match: "for app in", Output: snapshotLines(
			"A blog",
			"S 0 "+b64(stateJSON),
			"G absent",
			"L "+b64(lockJSON),
			"M 1",
			"R 0",
		)},
		ssh.MockCommand{Match: "docker ps --all", Output: container},
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
	// The fleet path must stay O(1) in apps: discovery, one control-plane
	// bundle, one docker ps. (An image-ID fleet adds exactly one inspect.)
	if len(mock.Calls) != 3 {
		t.Fatalf("app list took %d round trips, want 3: %q", len(mock.Calls), mock.Calls)
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

// One app's corrupt state must fail that app alone; the healthy app and the
// legacy-state app (canonical absent, key=value present) stay clean — the
// per-app isolation the per-reader path had.
func TestCollectAppListPerAppStateIsolation(t *testing.T) {
	goodState := `{"schema_version":2,"deployment_type":"container","ingress_mode":"caddy","domain":"good.example.com","current_hash":"g1","current_ports":[49153]}`
	legacyState := "current_hash=l9\ncurrent_port=49160\ndomain=legacy.example.com\n"
	mock := ssh.NewMockExecutor("192.0.2.10",
		ssh.MockCommand{Match: "for f in /deployments/*/state.json", Output: "good\nlegacy\nrotten\n"},
		ssh.MockCommand{Match: "for app in", Output: snapshotLines(
			"A good",
			"S 0 "+b64(goodState),
			"G absent",
			"L ",
			"M 0",
			"R 0",
			"A legacy",
			"S absent",
			"G 0 "+b64(legacyState),
			"L ",
			"M 0",
			"R 1",
			"A rotten",
			"S 0 "+b64("{not json"),
			"G absent",
			"L ",
			"M 0",
			"R 0",
		)},
		ssh.MockCommand{Match: "docker ps --all", Output: ""},
	)

	got := collectAppList(context.Background(), mock, time.Now())
	if len(got.Errors) != 0 || len(got.Apps) != 3 {
		t.Fatalf("unexpected list: errors=%#v apps=%#v", got.Errors, got.Apps)
	}
	byApp := map[string]appStatusDTO{}
	for _, app := range got.Apps {
		byApp[app.App] = app
	}
	if app := byApp["good"]; len(app.Errors) != 0 || app.Domain != "good.example.com" || app.Type != "container" {
		t.Fatalf("healthy app contaminated: %#v", app)
	}
	app := byApp["legacy"]
	if len(app.Errors) != 0 || app.Domain != "legacy.example.com" || app.Type != "static" {
		t.Fatalf("legacy fallback wrong: %#v", app)
	}
	if app.CurrentRelease.Version != "l9" || len(app.CurrentRelease.Ports) != 1 || app.CurrentRelease.Ports[0] != 49160 {
		t.Fatalf("legacy releases wrong: %#v", app.CurrentRelease)
	}
	rotten := byApp["rotten"]
	if len(rotten.Errors) != 1 || rotten.Errors[0].Scope != "state" || !strings.Contains(rotten.Errors[0].Message, "parsing canonical state for rotten") {
		t.Fatalf("corrupt state not attributed to its app: %#v", rotten.Errors)
	}
}

// docker failing is a host-wide condition: one top-level error, apps still
// listed (with their state), no per-app containers anywhere.
func TestCollectAppListDockerFailureIsTopLevel(t *testing.T) {
	goodState := `{"schema_version":2,"deployment_type":"container","ingress_mode":"caddy","domain":"good.example.com","current_hash":"g1"}`
	mock := ssh.NewMockExecutor("192.0.2.10",
		ssh.MockCommand{Match: "for f in /deployments/*/state.json", Output: "good\n"},
		ssh.MockCommand{Match: "for app in", Output: snapshotLines(
			"A good", "S 0 "+b64(goodState), "G absent", "L ", "M 0", "R 0",
		)},
		ssh.MockCommand{Match: "docker ps --all", Err: errors.New("docker daemon is not running")},
	)

	got := collectAppList(context.Background(), mock, time.Now())
	if len(got.Errors) != 1 || got.Errors[0].Scope != "containers" || !strings.Contains(got.Errors[0].Message, "docker daemon") {
		t.Fatalf("docker failure not top-level: %#v", got.Errors)
	}
	if len(got.Apps) != 1 || got.Apps[0].App != "good" || len(got.Apps[0].Errors) != 0 || got.Apps[0].Domain != "good.example.com" {
		t.Fatalf("app lost to a docker failure: %#v", got.Apps)
	}
}

// Containers group by exact teploy.app label equality — the semantics of
// the per-app `--filter label=teploy.app=` docker used to run per app, so
// "speedballs" never swallows "speedballs-mp" containers and unlabeled or
// foreign-label containers attach to nothing.
func TestCollectAppListGroupsContainersByExactLabel(t *testing.T) {
	goodState := func(domain string) string {
		return `{"schema_version":2,"deployment_type":"container","ingress_mode":"caddy","domain":"` + domain + `","current_hash":"h1"}`
	}
	container := func(name, app string) string {
		return `{"ID":"` + name + `","Names":"` + name + `","Image":"example/img:v1","State":"running","Status":"Up","CreatedAt":"2026-10-01 10:00:00 +0000 UTC","Labels":{"teploy.app":"` + app + `","teploy.process":"web"}}`
	}
	mock := ssh.NewMockExecutor("192.0.2.10",
		ssh.MockCommand{Match: "for f in /deployments/*/state.json", Output: "speedballs\nspeedballs-mp\n"},
		ssh.MockCommand{Match: "for app in", Output: snapshotLines(
			"A speedballs", "S 0 "+b64(goodState("speedballs.xyz")), "G absent", "L ", "M 0", "R 0",
			"A speedballs-mp", "S 0 "+b64(goodState("mp.speedballs.xyz")), "G absent", "L ", "M 0", "R 0",
		)},
		ssh.MockCommand{Match: "docker ps --all", Output: strings.Join([]string{
			container("speedballs-web-1", "speedballs"),
			container("speedballs-mp-web-1", "speedballs-mp"),
			container("unlabeled-web-1", ""),
		}, "\n")},
	)

	got := collectAppList(context.Background(), mock, time.Now())
	if len(got.Errors) != 0 || len(got.Apps) != 2 {
		t.Fatalf("unexpected list: %#v", got)
	}
	for _, app := range got.Apps {
		switch app.App {
		case "speedballs":
			if len(app.Containers) != 1 || app.Containers[0].Name != "speedballs-web-1" {
				t.Fatalf("speedballs containers wrong: %#v", app.Containers)
			}
		case "speedballs-mp":
			if len(app.Containers) != 1 || app.Containers[0].Name != "speedballs-mp-web-1" {
				t.Fatalf("speedballs-mp containers wrong: %#v", app.Containers)
			}
		}
	}
}

func TestPrintAppListKeepsReadableAppsWhenOneFails(t *testing.T) {
	var buf bytes.Buffer
	err := printAppList(&buf, appListDTO{
		Host: "h",
		Apps: []appStatusDTO{
			{App: "blog", Type: "container", CurrentRelease: releaseStatusDTO{Version: "abc123"}},
			{App: "shop", Errors: []machineError{{Scope: "state", Message: "unsupported state schema version 3"}}},
		},
		Errors: []machineError{{Scope: "app:Bad Name", Message: "invalid app name"}},
	})
	got := buf.String()
	if err == nil {
		t.Fatal("a listing with problems must exit non-zero")
	}
	for _, want := range []string{"blog", "abc123", "shop", "(error)", "app:shop state: unsupported state schema version 3", "app:Bad Name: invalid app name"} {
		if !strings.Contains(got, want) {
			t.Errorf("output misses %q:\n%s", want, got)
		}
	}
}

func TestPrintAppListEmpty(t *testing.T) {
	var buf bytes.Buffer
	if err := printAppList(&buf, appListDTO{}); err != nil || !strings.Contains(buf.String(), "No deployed apps") {
		t.Fatalf("err=%v out=%q", err, buf.String())
	}
}
