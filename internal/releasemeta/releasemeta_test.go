package releasemeta

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/docker"
	"github.com/useteploy/teploy/internal/ssh"
	"github.com/useteploy/teploy/internal/state"
)

func recordJSON(t *testing.T, rec Record) string {
	t.Helper()
	rec.SchemaVersion = SchemaVersion
	data, err := json.Marshal(&rec)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestRead_AbsentVsPresentVsUnreadable(t *testing.T) {
	rec := recordJSON(t, Record{App: "myapp", Hash: "v1", DeploymentType: "container", IngressMode: "caddy"})

	t.Run("absent", func(t *testing.T) {
		mock := ssh.NewMockExecutor("1.2.3.4",
			ssh.MockCommand{Match: "if [ ! -e '/deployments/myapp/meta/v1.json' ]", Output: "absent"},
		)
		got, err := Read(context.Background(), mock, "myapp", "v1")
		if err != nil || got != nil {
			t.Fatalf("expected (nil, nil) for a confirmed-missing record, got (%v, %v)", got, err)
		}
	})

	t.Run("present", func(t *testing.T) {
		mock := ssh.NewMockExecutor("1.2.3.4",
			ssh.MockCommand{Match: "if [ ! -e '/deployments/myapp/meta/v1.json' ]", Output: "present\n" + rec},
		)
		got, err := Read(context.Background(), mock, "myapp", "v1")
		if err != nil || got == nil {
			t.Fatalf("expected the record, got (%v, %v)", got, err)
		}
		if got.App != "myapp" || got.Hash != "v1" || got.DeploymentType != "container" {
			t.Errorf("record parsed wrong: %+v", got)
		}
	})

	t.Run("malformed is an error, not absence", func(t *testing.T) {
		mock := ssh.NewMockExecutor("1.2.3.4",
			ssh.MockCommand{Match: "if [ ! -e '/deployments/myapp/meta/v1.json' ]", Output: "present\n{not json"},
		)
		got, err := Read(context.Background(), mock, "myapp", "v1")
		if err == nil || got != nil {
			t.Fatalf("malformed record must be an error (F15 discipline), got (%v, %v)", got, err)
		}
	})

	t.Run("transport failure is an error, not absence", func(t *testing.T) {
		// The mock answers framed reads from its recorded file state when
		// it has one, so the transport failure is modeled explicitly.
		mock := ssh.NewMockExecutor("1.2.3.4",
			ssh.MockCommand{Match: "if [ ! -e '/deployments/myapp/meta/v1.json' ]", Err: fmt.Errorf("ssh: connection reset")},
		)
		if _, err := Read(context.Background(), mock, "myapp", "v1"); err == nil {
			t.Fatal("transport failure must be an error, never silent absence")
		}
	})

	t.Run("future schema rejected", func(t *testing.T) {
		future := strings.Replace(rec, fmt.Sprintf(`"schema_version":%d`, SchemaVersion), `"schema_version":99`, 1)
		mock := ssh.NewMockExecutor("1.2.3.4",
			ssh.MockCommand{Match: "if [ ! -e '/deployments/myapp/meta/v1.json' ]", Output: "present\n" + future},
		)
		if _, err := Read(context.Background(), mock, "myapp", "v1"); err == nil || !strings.Contains(err.Error(), "schema") {
			t.Fatalf("expected schema rejection, got %v", err)
		}
	})
}

func TestWrite_RoundTripAndPathValidation(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "mkdir -p /deployments/myapp/meta", Output: ""},
		ssh.MockCommand{Match: "if [ ! -e '/deployments/myapp/meta/v1.json' ]", Output: "absent"},
		ssh.MockCommand{Match: "UPLOAD:", Output: ""},
		ssh.MockCommand{Match: "mv", Output: ""},
	)
	rec := &Record{
		App: "myapp", Hash: "v1",
		DeploymentType: "container", IngressMode: "host", Domain: "myapp.io",
		Ports: []Port{{HostPort: 7460, ContainerPort: 7460, Bind: "0.0.0.0", Primary: true, Fixed: true}},
		EnvFiles: []string{"/deployments/myapp/.env"},
	}
	if err := Write(context.Background(), mock, rec); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// 0600: records can carry resolved env from backfills.
	var uploadCall string
	for _, c := range mock.Calls {
		if strings.HasPrefix(c, "UPLOAD:/deployments/myapp/meta/v1.json.tmp-") {
			uploadCall = c
		}
	}
	if uploadCall == "" {
		t.Fatal("record was not uploaded to its atomic temp path")
	}
	if !strings.Contains(uploadCall, "mode 0600") {
		t.Errorf("record must be written 0600, got: %s", uploadCall)
	}
	written, ok := mock.Files["/deployments/myapp/meta/v1.json"]
	if !ok {
		t.Fatal("record not committed into place")
	}
	if !strings.Contains(string(written), `"hash":"v1"`) || !strings.Contains(string(written), `"fixed":true`) {
		t.Errorf("unexpected record content: %s", written)
	}

	// Invalid release ids never reach a remote path.
	for _, bad := range []string{"", "../state", "a/b", ".hidden", strings.Repeat("x", 129)} {
		if _, err := Path("myapp", bad); err == nil {
			t.Errorf("Path accepted invalid hash %q", bad)
		}
	}
	if _, err := Path("", "v1"); err == nil {
		t.Error("Path accepted an empty app")
	}
}

func TestPrimaryPortSelection(t *testing.T) {
	rec := &Record{Ports: []Port{
		{HostPort: 51820, ContainerPort: 51820, Proto: "udp", Fixed: true},
		{HostPort: 49152, ContainerPort: 3000, Primary: true},
	}}
	if p, ok := PrimaryContainerPort(rec); !ok || p != 3000 {
		t.Errorf("PrimaryContainerPort = (%d, %v), want (3000, true)", p, ok)
	}
	if p, ok := PrimaryContainerPort(&Record{}); ok || p != 0 {
		t.Errorf("PrimaryContainerPort on a record without ports = (%d, %v)", p, ok)
	}
}

func TestHasFixedHostPorts(t *testing.T) {
	if HasFixedHostPorts(nil) {
		t.Error("nil record has no fixed ports")
	}
	if !HasFixedHostPorts(&Record{Publish: []string{"127.0.0.1:3001:3001"}}) {
		t.Error("publish entries are fixed host ports")
	}
	if !HasFixedHostPorts(&Record{Ports: []Port{{HostPort: 7460, ContainerPort: 7460, Fixed: true}}}) {
		t.Error("a Fixed-flagged port must count")
	}
	if HasFixedHostPorts(&Record{Ports: []Port{{HostPort: 49152, ContainerPort: 3000}}}) {
		t.Error("ephemeral blue/green ports are not fixed")
	}
}

func TestPortFromPublishSpec(t *testing.T) {
	cases := []struct {
		in   string
		want Port
		ok   bool
	}{
		{"0.0.0.0:3001:3001", Port{Bind: "0.0.0.0", HostPort: 3001, ContainerPort: 3001, Fixed: true}, true},
		{"127.0.0.1:9100:9000", Port{Bind: "127.0.0.1", HostPort: 9100, ContainerPort: 9000, Fixed: true}, true},
		{"3001:3001", Port{HostPort: 3001, ContainerPort: 3001, Fixed: true}, true},
		{"53:53/udp", Port{HostPort: 53, ContainerPort: 53, Proto: "udp", Fixed: true}, true},
		{"3000", Port{ContainerPort: 3000, Fixed: true}, true},
		{"[::]:80:80", Port{}, false}, // IPv6 multi-colon: raw-only, by design
		{"", Port{}, false},
	}
	for _, tc := range cases {
		got, ok := PortFromPublishSpec(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Errorf("PortFromPublishSpec(%q) = (%+v, %v), want (%+v, %v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

// backfillInspect is a rich inspect for the target's web container: multiple
// bindings (primary 3000 via PORT env, plus a fixed 51820/udp publish), env,
// a named-volume mount, labels, resource limits, and a healthcheck override.
func backfillInspect() string {
	return fmt.Sprintf(`[{
  "Image": "sha256:%s",
  "Config": {
    "Image": "myapp:v1",
    "Env": ["PORT=3000", "API_KEY=resolved-secret"],
    "Cmd": ["npm", "run", "start"],
    "Labels": {"teploy.app": "myapp", "teploy.version": "v1", "teploy.process": "web"},
    "Healthcheck": {"Test": ["NONE"]}
  },
  "HostConfig": {
    "NetworkMode": "teploy",
    "PortBindings": {
      "3000/tcp": [{"HostIp": "127.0.0.1", "HostPort": "49152"}],
      "9100/udp": [{"HostIp": "0.0.0.0", "HostPort": "9100"}]
    },
    "Binds": [],
    "Mounts": [{"Type": "volume", "Source": "myapp-uploads", "Target": "/uploads"}],
    "RestartPolicy": {"Name": "unless-stopped"},
    "Memory": 536870912,
    "NanoCpus": 0,
    "LogConfig": {"Type": "json-file", "Config": {"max-size": "10m"}}
  },
  "NetworkSettings": {"Networks": {"teploy": {"Aliases": ["myapp"]}}}
}]`, strings.Repeat("e", 64))
}

func backfillContainers() string {
	return `{"ID":"aaa","Names":"myapp-web-v1","Image":"myapp:v1","State":"exited","Status":"Exited","Labels":"teploy.app=myapp,teploy.version=v1,teploy.process=web"}` + "\n" +
		`{"ID":"bbb","Names":"myapp-worker-v1","Image":"myapp:v1","State":"exited","Status":"Exited","Labels":"teploy.app=myapp,teploy.version=v1,teploy.process=worker"}`
}

// The convergence migration (F14's "release-0"): an install whose releases
// predate the store gets a record synthesized from the live containers the
// first time rollback needs one.
func TestBackfill_FromLiveContainers(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "docker ps --all --filter label=teploy.app='myapp'", Output: backfillContainers()},
		ssh.MockCommand{Match: "docker inspect 'myapp-web-v1'", Output: backfillInspect()},
		ssh.MockCommand{Match: "mkdir -p /deployments/myapp/meta", Output: ""},
		ssh.MockCommand{Match: "UPLOAD:", Output: ""},
		ssh.MockCommand{Match: "mv", Output: ""},
	)
	containers, err := docker.ParseContainers(backfillContainers())
	if err != nil {
		t.Fatal(err)
	}
	st := &state.AppState{IngressMode: "caddy", Domain: "myapp.com", DeploymentType: "container"}

	rec, warn := Backfill(context.Background(), mock, docker.NewClient(mock), containers, "myapp", "v1", st)
	if rec == nil {
		t.Fatalf("Backfill returned no record (warn: %v)", warn)
	}
	if warn != nil {
		t.Errorf("unexpected persistence warning: %v", warn)
	}

	if !rec.Backfilled {
		t.Error("backfilled record must be flagged")
	}
	if rec.ImageDigest != "sha256:"+strings.Repeat("e", 64) || rec.ImageRef != "myapp:v1" {
		t.Errorf("image identity not captured: %s / %s", rec.ImageDigest, rec.ImageRef)
	}
	// Resolved env from inspect — the recoverable form of env references.
	if rec.Env["API_KEY"] != "resolved-secret" || rec.Env["PORT"] != "3000" {
		t.Errorf("resolved env not captured: %v", rec.Env)
	}
	if len(rec.EnvFiles) != 0 {
		t.Errorf("env-file references are NOT recoverable; recording any would lie: %v", rec.EnvFiles)
	}
	// Ports: 3000 primary (via PORT env), 51820 fixed (outside ephemeral range).
	var primary, fixed *Port
	for i := range rec.Ports {
		if rec.Ports[i].Primary {
			primary = &rec.Ports[i]
		}
		if rec.Ports[i].ContainerPort == 9100 {
			fixed = &rec.Ports[i]
		}
	}
	if primary == nil || primary.ContainerPort != 3000 || primary.HostPort != 49152 {
		t.Errorf("primary port not selected via PORT env: %+v", rec.Ports)
	}
	if fixed == nil || !fixed.Fixed {
		t.Errorf("fixed publish port not detected: %+v", rec.Ports)
	}
	if rec.Volumes["myapp-uploads"] != "/uploads" {
		t.Errorf("named volume not captured: %v", rec.Volumes)
	}
	if rec.Memory != "536870912b" {
		t.Errorf("memory not captured: %q", rec.Memory)
	}
	if rec.Processes["worker"] != "" || rec.Processes["web"] == "" {
		t.Errorf("process set not captured: %v", rec.Processes)
	}
	if rec.Cmd != "npm run start" {
		t.Errorf("web cmd not captured: %q", rec.Cmd)
	}
	if rec.Recreate == nil || !rec.Recreate.NoHealthcheck {
		t.Errorf("recreate spec not embedded: %+v", rec.Recreate)
	}
	if rec.Health != nil || rec.Caddy != nil {
		t.Errorf("health/caddy config is not recoverable from containers; recording any would lie: %+v %+v", rec.Health, rec.Caddy)
	}

	// The record was persisted: a second Read finds it without another
	// backfill.
	mock2 := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "if [ ! -e '/deployments/myapp/meta/v1.json' ]",
			Output: "present\n" + string(mock.Files["/deployments/myapp/meta/v1.json"])},
	)
	reread, err := Read(context.Background(), mock2, "myapp", "v1")
	if err != nil || reread == nil || !reread.Backfilled {
		t.Fatalf("backfilled record did not round-trip: (%v, %v)", reread, err)
	}
}

func TestBackfill_NoWebContainerFails(t *testing.T) {
	containers, err := docker.ParseContainers(`{"ID":"bbb","Names":"myapp-worker-v1","Image":"myapp:v1","State":"exited","Status":"Exited","Labels":"teploy.app=myapp,teploy.version=v1,teploy.process=worker"}`)
	if err != nil {
		t.Fatal(err)
	}
	mock := ssh.NewMockExecutor("1.2.3.4")
	if _, err := Backfill(context.Background(), mock, docker.NewClient(mock), containers, "myapp", "v1",
		&state.AppState{IngressMode: "caddy"}); err == nil || !strings.Contains(err.Error(), "no web container") {
		t.Fatalf("expected no-web-container error, got %v", err)
	}
}

// A persistence failure still returns the synthesized record (usable
// in-memory) alongside a warning — the store must never be why a rollback
// that used to work stops working.
func TestBackfill_PersistFailureStillReturnsRecord(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "docker ps --all", Output: backfillContainers()},
		ssh.MockCommand{Match: "docker inspect 'myapp-web-v1'", Output: backfillInspect()},
		ssh.MockCommand{Match: "mkdir -p /deployments/myapp/meta", Err: fmt.Errorf("disk full")},
	)
	containers, _ := docker.ParseContainers(backfillContainers())
	rec, warn := Backfill(context.Background(), mock, docker.NewClient(mock), containers, "myapp", "v1",
		&state.AppState{IngressMode: "caddy"})
	if rec == nil {
		t.Fatal("record must still be returned for in-memory use")
	}
	if warn == nil || !strings.Contains(warn.Error(), "persisting") {
		t.Errorf("expected a persistence warning, got %v", warn)
	}
}
