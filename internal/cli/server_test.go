package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/useteploy/teploy/internal/config"
)

// runServerCmd executes a server subcommand against a private HOME so the
// tests write their own ~/.teploy/servers.yml and never touch the real one.
func runServerCmd(t *testing.T, args ...string) error {
	t.Helper()
	root := NewRootCmd("test")
	root.SetArgs(append([]string{"server"}, args...))
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	return root.Execute()
}

func seedServersFile(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	path := filepath.Join(dir, ".teploy", "servers.yml")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	seed := "servers:\n" +
		"  prod:\n" +
		"    host: 1.2.3.4\n" +
		"    user: deploy\n" +
		"    role: app\n" +
		"    vpn_ip: 100.64.0.7\n" +
		"    tags:\n" +
		"      region: us-east\n" +
		"  staging:\n" +
		"    host: 5.6.7.8\n"
	if err := os.WriteFile(path, []byte(seed), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestServerRenameCmd is the command-level UPSTREAM-2 regression: `teploy
// server rename` moves the whole record — tags and vpn_ip included — in one
// invocation, where dash previously had to remove+add and lost them.
func TestServerRenameCmd(t *testing.T) {
	path := seedServersFile(t)

	if err := runServerCmd(t, "rename", "prod", "production"); err != nil {
		t.Fatalf("server rename: %v", err)
	}

	cfg, err := config.LoadServers(path)
	if err != nil {
		t.Fatal(err)
	}
	s, ok := cfg.Servers["production"]
	if !ok {
		t.Fatal("renamed entry missing")
	}
	if _, exists := cfg.Servers["prod"]; exists {
		t.Error("old name still present")
	}
	if s.VpnIP != "100.64.0.7" || s.Tags["region"] != "us-east" || s.User != "deploy" {
		t.Errorf("rename lost metadata: %+v tags=%v", s, s.Tags)
	}
	if cfg.Servers["staging"].Host != "5.6.7.8" {
		t.Errorf("unrelated entry changed: %v", cfg.Servers["staging"])
	}
}

func TestServerRenameCmd_DestinationCollision(t *testing.T) {
	path := seedServersFile(t)

	err := runServerCmd(t, "rename", "prod", "staging")
	if err == nil {
		t.Fatal("renaming onto an existing name must fail")
	}
	if !errors.Is(err, config.ErrServerExists) {
		t.Fatalf("err = %v, want ErrServerExists", err)
	}

	cfg, err2 := config.LoadServers(path)
	if err2 != nil {
		t.Fatal(err2)
	}
	if cfg.Servers["prod"].Host != "1.2.3.4" || cfg.Servers["staging"].Host != "5.6.7.8" {
		t.Fatalf("failed rename modified the config: %v", cfg.Servers)
	}
}

func TestServerUpdateCmd(t *testing.T) {
	path := seedServersFile(t)

	if err := runServerCmd(t, "update", "prod", "--host", "9.9.9.9", "--vpn-ip", ""); err != nil {
		t.Fatalf("server update: %v", err)
	}

	cfg, err := config.LoadServers(path)
	if err != nil {
		t.Fatal(err)
	}
	s := cfg.Servers["prod"]
	if s.Host != "9.9.9.9" {
		t.Errorf("host not updated: %q", s.Host)
	}
	if s.VpnIP != "" {
		t.Errorf("explicit empty --vpn-ip should clear the field: %q", s.VpnIP)
	}
	if s.User != "deploy" || s.Role != "app" || s.Tags["region"] != "us-east" {
		t.Errorf("unspecified fields changed: %+v tags=%v", s, s.Tags)
	}
}

func TestServerUpdateCmd_NoFlagsRejected(t *testing.T) {
	path := seedServersFile(t)

	if err := runServerCmd(t, "update", "prod"); err == nil {
		t.Fatal("update with no fields must be rejected")
	}

	// Nothing changed.
	cfg, err := config.LoadServers(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Servers["prod"].Host != "1.2.3.4" {
		t.Errorf("rejected update modified the config: %+v", cfg.Servers["prod"])
	}
}

func TestServerUpdateCmd_BadRoleRejected(t *testing.T) {
	path := seedServersFile(t)

	if err := runServerCmd(t, "update", "prod", "--role", "db"); err == nil {
		t.Fatal("invalid role must be rejected")
	}

	cfg, err := config.LoadServers(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Servers["prod"].Role != "app" {
		t.Errorf("rejected update modified the config: %+v", cfg.Servers["prod"])
	}
}

// serverListFixedTime is the deterministic clock for the envelope tests.
var serverListFixedTime = time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)

// TestServerListJSONEnvelopeShape pins the MI 2 reshape decode-side: the
// root IS the envelope {machine_interface, servers, observed_at} and IS
// NOT the bare map-of-servers the pre-MI-2 CLI emitted (that shape is gone
// on the wire — the reason this is a contract bump, not a field add).
func TestServerListJSONEnvelopeShape(t *testing.T) {
	path := seedServersFile(t)
	servers, err := config.ListServers(path)
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := writeServerList(&buf, servers, true, serverListFixedTime); err != nil {
		t.Fatalf("writeServerList: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("server list --json is not valid JSON: %q: %v", buf.String(), err)
	}
	if decoded["machine_interface"] != float64(MachineInterface) {
		t.Fatalf("machine_interface = %v, want %d", decoded["machine_interface"], MachineInterface)
	}
	if _, bare := decoded["prod"]; bare {
		t.Fatalf("bare-map root survived the reshape (a server name is a root key): %s", buf.String())
	}
	entries, ok := decoded["servers"].([]any)
	if !ok || len(entries) != 2 {
		t.Fatalf("servers = %#v, want the two seeded entries", decoded["servers"])
	}
	first, _ := entries[0].(map[string]any)
	if first["name"] != "prod" || first["host"] != "1.2.3.4" || first["user"] != "deploy" || first["role"] != "app" {
		t.Fatalf("first entry did not carry the per-server fields: %#v", first)
	}
	if first["vpn_ip"] != "100.64.0.7" {
		t.Fatalf("vpn_ip not carried over by the reshape: %#v", first)
	}
	if entries[1].(map[string]any)["name"] != "staging" {
		t.Fatalf("entries not sorted by name: %s", buf.String())
	}
	if decoded["observed_at"] != serverListFixedTime.Format(time.RFC3339Nano) {
		t.Fatalf("observed_at = %v", decoded["observed_at"])
	}
}

// TestServerListJSONEnvelopeStableIDAndEmpty pins the S4 stable id riding
// the envelope (present when the record has one, absent for id-less
// legacy entries — omitempty, never an empty string) and the empty-fleet
// shape: `servers` is an array (empty, not null), never a null map.
func TestServerListJSONEnvelopeStableIDAndEmpty(t *testing.T) {
	withID := map[string]config.Server{
		"prod": {ID: "srv-0123456789abcdef", Host: "192.0.2.10", User: "deploy"},
	}
	var buf bytes.Buffer
	if err := writeServerList(&buf, withID, true, serverListFixedTime); err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Servers []struct {
			Name string `json:"name"`
			ID   string `json:"id"`
		} `json:"servers"`
	}
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Servers) != 1 || decoded.Servers[0].Name != "prod" || decoded.Servers[0].ID != "srv-0123456789abcdef" {
		t.Fatalf("stable id not carried: %s", buf.String())
	}

	buf.Reset()
	if err := writeServerList(&buf, nil, true, serverListFixedTime); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Servers == nil || len(decoded.Servers) != 0 {
		t.Fatalf("empty fleet must decode as an empty (non-null) array: %s", buf.String())
	}
}

// TestServerListCommandJSONEndToEnd runs the real cobra command against a
// private HOME so the wiring (flags.JSON, HOME resolution, encoder) is
// exercised together.
func TestServerListCommandJSONEndToEnd(t *testing.T) {
	seedServersFile(t)

	var out bytes.Buffer
	root := NewRootCmd("test")
	root.SetOut(&out)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"server", "list", "--json"})
	if err := root.Execute(); err != nil {
		t.Fatalf("server list --json: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("output not JSON: %q: %v", out.String(), err)
	}
	if decoded["machine_interface"] != float64(MachineInterface) {
		t.Fatalf("machine_interface = %v, want %d", decoded["machine_interface"], MachineInterface)
	}
	if _, ok := decoded["servers"].([]any); !ok {
		t.Fatalf("servers missing from the envelope: %s", out.String())
	}
}

// TestServerListHumanUnchanged pins the human table output: the MI 2
// reshape touched the machine surface only.
func TestServerListHumanUnchanged(t *testing.T) {
	path := seedServersFile(t)
	servers, err := config.ListServers(path)
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := writeServerList(&buf, servers, false, serverListFixedTime); err != nil {
		t.Fatal(err)
	}
	want := "NAME     HOST     USER    ROLE\n" +
		"prod     1.2.3.4  deploy  app\n" +
		"staging  5.6.7.8  root    app\n"
	if buf.String() != want {
		t.Fatalf("human output changed:\n got: %q\nwant: %q", buf.String(), want)
	}
}
