package deploy

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/ssh"
	"github.com/useteploy/teploy/internal/state"
)

// newRouteMock builds a mock whose Caddyfile machinery satisfies one
// caddy.mutate transaction (lock, read, adapt, atomic write, reload, verify,
// release). record seeds the predecessor release's F14 record ("" = none);
// extra commands (registered first, so they win) model the live docker
// world.
func newRouteMock(t *testing.T, record string, extra []ssh.MockCommand) *ssh.MockExecutor {
	t.Helper()
	cmds := append([]ssh.MockCommand{}, extra...)
	cmds = append(cmds,
		ssh.MockCommand{Match: "mkdir /deployments/caddy/.lock", Output: ""},
		ssh.MockCommand{Match: "docker exec caddy caddy reload", Output: ""},
		ssh.MockCommand{Match: "a=$(docker exec caddy md5sum", Output: "TEPLOY_CADDY_OK"},
		ssh.MockCommand{Match: "rmdir /deployments/caddy/.lock", Output: ""},
	)
	mock := ssh.NewMockExecutor("1.2.3.4", cmds...)
	mock.Files["/deployments/caddy/Caddyfile"] = []byte("{\n\tadmin 0.0.0.0:2019\n}\n")
	if record != "" {
		mock.Files["/deployments/myapp/meta/old123.json"] = []byte(record)
	}
	return mock
}

// TestRestorePreviousRoute_RendersRouteFromRecordedReceipt is the C01-7 core
// regression: compensating a traffic switch must render the previous route
// from the RECORDED receipt (the predecessor release's F14 record) — domain,
// upstream container port, and TLS all come from the record, never from the
// current config or a live inspect. The live world here disagrees on every
// axis (state's domain, cfg's domain, and an inspect that would answer a
// different port): the receipt wins.
func TestRestorePreviousRoute_RendersRouteFromRecordedReceipt(t *testing.T) {
	record := `{"schema_version":1,"app":"myapp","hash":"old123","created_at":"2026-09-20T10:00:00Z",` +
		`"replicas":1,"domain":"receipt.example.com",` +
		`"ports":[{"host_port":49152,"container_port":3000,"primary":true}],` +
		`"caddy":{"tls_cert":"/etc/caddy/tls/att/myapp/old123.deadbeefdeadbeef/myapp.crt","tls_key":"/etc/caddy/tls/att/myapp/old123.deadbeefdeadbeef/myapp.key"},` +
		`"health":{"path":"/readyz"}}`

	mock := newRouteMock(t, record, nil)
	d := NewDeployer(mock, new(bytes.Buffer))

	current := &state.AppState{SchemaVersion: 2, CurrentHash: "old123", Domain: "live.example.com", CurrentPorts: []int{49152}}
	cfg := Config{App: "myapp", Domain: "cfg.example.com", Version: "new456"}
	if err := d.restorePreviousRoute(context.Background(), cfg, current); err != nil {
		t.Fatalf("restorePreviousRoute: %v", err)
	}

	caddyfile := string(mock.Files["/deployments/caddy/Caddyfile"])
	for _, want := range []string{
		"receipt.example.com",   // hosts from the record, not state/cfg
		"myapp-web-old123:3000", // upstream name + RECORDED container port
		"/etc/caddy/tls/att/myapp/old123.deadbeefdeadbeef/myapp.crt", // recorded TLS
	} {
		if !strings.Contains(caddyfile, want) {
			t.Errorf("route must be rendered from the recorded receipt; Caddyfile missing %q:\n%s", want, caddyfile)
		}
	}
	for _, banned := range []string{"live.example.com", "cfg.example.com", ":8080"} {
		if strings.Contains(caddyfile, banned) {
			t.Errorf("route must not use non-recorded values; Caddyfile contains %q:\n%s", banned, caddyfile)
		}
	}
	// The receipt path consults NO live inspect for the route.
	for _, c := range mock.Calls {
		if strings.Contains(c, ".NetworkSettings.Ports") {
			t.Errorf("the recorded receipt must replace the live port inspect, got: %s", c)
		}
	}
}

// TestRestorePreviousRoute_SameVersionUsesReplacedNaming keeps the
// same-version contract inside the receipt path: the predecessor containers
// were renamed aside (_replaced) by this same-version attempt, and the
// restored route must point at those names.
func TestRestorePreviousRoute_SameVersionUsesReplacedNaming(t *testing.T) {
	record := `{"schema_version":1,"app":"myapp","hash":"old123","created_at":"2026-09-20T10:00:00Z",` +
		`"replicas":1,"domain":"receipt.example.com",` +
		`"ports":[{"host_port":49152,"container_port":3000,"primary":true}]}`
	mock := newRouteMock(t, record, nil)
	d := NewDeployer(mock, new(bytes.Buffer))

	current := &state.AppState{SchemaVersion: 2, CurrentHash: "old123", CurrentPorts: []int{49152}}
	cfg := Config{App: "myapp", Domain: "myapp.com", Version: "old123"} // same version
	if err := d.restorePreviousRoute(context.Background(), cfg, current); err != nil {
		t.Fatalf("restorePreviousRoute: %v", err)
	}
	caddyfile := string(mock.Files["/deployments/caddy/Caddyfile"])
	if !strings.Contains(caddyfile, "myapp-web-old123_replaced:3000") {
		t.Errorf("same-version compensation must route at the renamed predecessor, got:\n%s", caddyfile)
	}
}

// TestRestorePreviousRoute_NoRecordFallsBackLoudly pins the legacy path:
// with NO record (a pre-F14 install), the route is reconstructed from live
// inspection exactly as before — but the fallback is NAMED in the output,
// never silent.
func TestRestorePreviousRoute_NoRecordFallsBackLoudly(t *testing.T) {
	mock := newRouteMock(t, "", []ssh.MockCommand{
		// Live inspect answers the container's port.
		ssh.MockCommand{Match: "docker inspect -f '{{range $p, $_ := .NetworkSettings.Ports}}", Output: "8080/tcp "},
	})
	var buf bytes.Buffer
	d := NewDeployer(mock, &buf)

	current := &state.AppState{SchemaVersion: 2, CurrentHash: "old123", Domain: "live.example.com", CurrentPorts: []int{49152}}
	cfg := Config{App: "myapp", Domain: "cfg.example.com", Version: "new456"}
	if err := d.restorePreviousRoute(context.Background(), cfg, current); err != nil {
		t.Fatalf("restorePreviousRoute: %v", err)
	}

	caddyfile := string(mock.Files["/deployments/caddy/Caddyfile"])
	if !strings.Contains(caddyfile, "myapp-web-old123:8080") {
		t.Errorf("the legacy fallback reconstructs from live inspect, got:\n%s", caddyfile)
	}
	if !strings.Contains(caddyfile, "live.example.com") {
		t.Errorf("the legacy fallback uses the state's domain, got:\n%s", caddyfile)
	}
	if !strings.Contains(buf.String(), "live inspection") {
		t.Errorf("the fallback must be loud — output must name the live-inspection reconstruction, got:\n%s", buf.String())
	}
}

// TestRestorePreviousRoute_RecordDisagreesWithLiveInspect_RecordWins is the
// point of the fix: the record is AUTHORITATIVE for what teploy switched
// away FROM. A live inspect that succeeds and disagrees (8080 vs the
// recorded 3000) must never win — and the receipt path must not even consult
// it. Multi-replica: upstreams and the health path come from the record.
func TestRestorePreviousRoute_RecordDisagreesWithLiveInspect_RecordWins(t *testing.T) {
	record := `{"schema_version":1,"app":"myapp","hash":"old123","created_at":"2026-09-20T10:00:00Z",` +
		`"replicas":2,"domain":"receipt.example.com",` +
		`"ports":[{"host_port":49152,"container_port":3000,"primary":true},{"host_port":49153,"container_port":3000}],` +
		`"health":{"path":"/readyz"}}`
	mock := newRouteMock(t, record, []ssh.MockCommand{
		// The inspect WOULD answer — with a port that disagrees.
		ssh.MockCommand{Match: "docker inspect -f '{{range $p, $_ := .NetworkSettings.Ports}}", Output: "8080/tcp "},
	})
	var buf bytes.Buffer
	d := NewDeployer(mock, &buf)

	current := &state.AppState{SchemaVersion: 2, CurrentHash: "old123", Domain: "live.example.com", CurrentPorts: []int{49152, 49153}}
	cfg := Config{App: "myapp", Domain: "cfg.example.com", Version: "new456"}
	if err := d.restorePreviousRoute(context.Background(), cfg, current); err != nil {
		t.Fatalf("restorePreviousRoute: %v", err)
	}

	caddyfile := string(mock.Files["/deployments/caddy/Caddyfile"])
	for _, want := range []string{
		"receipt.example.com",
		"myapp-web-old123-1:3000",
		"myapp-web-old123-2:3000",
		"/readyz", // recorded LB health path
	} {
		if !strings.Contains(caddyfile, want) {
			t.Errorf("compensation must come from the record even when live inspect disagrees; missing %q:\n%s", want, caddyfile)
		}
	}
	if strings.Contains(caddyfile, ":8080") {
		t.Errorf("the disagreeing live inspect must lose, got:\n%s", caddyfile)
	}
	for _, c := range mock.Calls {
		if strings.Contains(c, ".NetworkSettings.Ports") {
			t.Errorf("a readable record must not consult live inspect at all, got: %s", c)
		}
	}
	if strings.Contains(buf.String(), "live inspection") {
		t.Errorf("a record-driven compensation is not a fallback; the loud-fallback message must not fire, got:\n%s", buf.String())
	}
}
