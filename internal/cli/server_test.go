package cli

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

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
