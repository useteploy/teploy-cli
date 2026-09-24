package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/network"
	"github.com/useteploy/teploy/internal/ssh"
)

// The README used to document a `setup --harden` flag that does not exist:
// hardening is on by default and is skipped with --no-harden. This pins the
// actual flag surface so the docs and the command cannot drift apart again.
func TestSetupCmd_HardenFlagNaming(t *testing.T) {
	cmd := newSetupCmd(&Flags{})

	if cmd.Flags().Lookup("no-harden") == nil {
		t.Error("setup must register --no-harden (hardening is on by default)")
	}
	if cmd.Flags().Lookup("harden") != nil {
		t.Error("setup must not register --harden; hardening is on by default and skipped with --no-harden")
	}
}

func TestSetupServer(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "whoami", Output: "root"},
		ssh.MockCommand{Match: "docker --version", Output: "Docker version 24.0.0, build abc123"},
		ssh.MockCommand{Match: "rsync --version", Output: "rsync  version 3.2.7"},
		ssh.MockCommand{Match: "ufw status", Err: fmt.Errorf("command not found")},
		ssh.MockCommand{Match: "systemctl is-active firewalld", Err: fmt.Errorf("inactive")},
		ssh.MockCommand{Match: "docker info", Output: ""},
		ssh.MockCommand{Match: "docker network", Output: "teploy"},
		ssh.MockCommand{Match: "mkdir", Output: ""},
		ssh.MockCommand{Match: "chown", Output: ""},
		ssh.MockCommand{Match: "[ -f /deployments/caddy/Caddyfile ]", Output: "absent"},
		ssh.MockCommand{Match: "docker ps -a --filter name=", Output: ""},
		ssh.MockCommand{Match: "docker run", Output: "caddy_container_id"},
	)

	var buf bytes.Buffer
	if err := setupServer(context.Background(), mock, &buf, true); err != nil {
		t.Fatalf("setupServer: %v", err)
	}

	output := buf.String()

	if !strings.Contains(output, "Docker already installed") {
		t.Error("should report Docker already installed")
	}
	if !strings.Contains(output, "No active firewall") {
		t.Error("should report no firewall")
	}
	if !strings.Contains(output, "Caddy started") {
		t.Error("should report Caddy started")
	}
	if !strings.Contains(output, "Server provisioned successfully") {
		t.Error("should report success")
	}

	// Verify Caddyfile was uploaded with correct content.
	content, ok := mock.Files["/deployments/caddy/Caddyfile"]
	if !ok {
		t.Fatal("Caddyfile not uploaded")
	}
	if !strings.Contains(string(content), "admin 127.0.0.1:2019") {
		t.Errorf("Caddyfile missing loopback-bound admin config, got: %s", string(content))
	}

	// Verify Caddy docker run command contains required flags.
	var caddyCmd string
	for _, call := range mock.Calls {
		if strings.Contains(call, "docker") && strings.Contains(call, "run") && strings.Contains(call, "caddy") {
			caddyCmd = call
		}
	}
	if caddyCmd == "" {
		t.Fatal("no docker run command found")
	}
	for _, want := range []string{
		"--restart always",
		"--name caddy",
		"--network teploy",
		"-p 80:80",
		"-p 443:443",
		"caddy_data:/data",
		"/deployments/caddy:/etc/caddy",
	} {
		if !strings.Contains(caddyCmd, want) {
			t.Errorf("Caddy command missing %q\ngot: %s", want, caddyCmd)
		}
	}
	// Must mount the directory, not the single file (stale-bind-mount bug).
	if strings.Contains(caddyCmd, "/deployments/caddy/Caddyfile:/etc/caddy/Caddyfile") {
		t.Errorf("Caddy must not single-file-mount the Caddyfile\ngot: %s", caddyCmd)
	}
	// Caddyfile-authoritative model: no --resume, and the admin API is not
	// published to the host (reached via docker exec only).
	if strings.Contains(caddyCmd, "--resume") {
		t.Errorf("Caddy command must not use --resume\ngot: %s", caddyCmd)
	}
	if strings.Contains(caddyCmd, "2019:2019") {
		t.Errorf("admin API must not be host-published\ngot: %s", caddyCmd)
	}
}

func TestSetupServer_InstallDocker(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "whoami", Output: "root"},
		ssh.MockCommand{Match: "docker --version", Err: fmt.Errorf("not found"), Once: true},
		ssh.MockCommand{Match: "which curl", Output: "/usr/bin/curl"},
		ssh.MockCommand{Match: "sh -c", Output: ""},                               // install stream
		ssh.MockCommand{Match: "docker --version", Output: "Docker version 24.0"}, // verify after install
		ssh.MockCommand{Match: "usermod", Output: ""},
		ssh.MockCommand{Match: "ufw status", Err: fmt.Errorf("not found")},
		ssh.MockCommand{Match: "systemctl", Err: fmt.Errorf("inactive")},
		ssh.MockCommand{Match: "docker info", Output: ""},
		ssh.MockCommand{Match: "docker network", Output: "teploy"},
		ssh.MockCommand{Match: "mkdir", Output: ""},
		ssh.MockCommand{Match: "chown", Output: ""},
		ssh.MockCommand{Match: "[ -f /deployments/caddy/Caddyfile ]", Output: "absent"},
		ssh.MockCommand{Match: "docker ps -a --filter name=", Output: ""},
		ssh.MockCommand{Match: "docker run", Output: "caddy_id"},
	)

	var buf bytes.Buffer
	if err := setupServer(context.Background(), mock, &buf, true); err != nil {
		t.Fatalf("setupServer: %v", err)
	}

	if !strings.Contains(buf.String(), "Installing Docker") {
		t.Error("should report Docker installation")
	}
	if !strings.Contains(buf.String(), "Docker installed") {
		t.Error("should report Docker installed after verification")
	}
}

func TestSetupServer_CaddyAlreadyRunning(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "whoami", Output: "root"},
		ssh.MockCommand{Match: "docker --version", Output: "Docker version 24.0.0"},
		ssh.MockCommand{Match: "rsync --version", Output: "rsync  version 3.2.7"},
		ssh.MockCommand{Match: "ufw status", Err: fmt.Errorf("not found")},
		ssh.MockCommand{Match: "systemctl", Err: fmt.Errorf("inactive")},
		ssh.MockCommand{Match: "docker info", Output: ""},
		ssh.MockCommand{Match: "docker network", Output: "teploy"},
		ssh.MockCommand{Match: "mkdir", Output: ""},
		ssh.MockCommand{Match: "chown", Output: ""},
		ssh.MockCommand{Match: "[ -f /deployments/caddy/Caddyfile ]", Output: "absent"},
		ssh.MockCommand{Match: "sed -i", Output: ""},
		ssh.MockCommand{Match: "docker ps -a --filter name=", Output: "caddy"},
		// Existing Caddy already on the new model: no --resume, directory-
		// mounted (/etc/caddy), AND has the /deployments mount type:static
		// deploys need. All three checks pass → skip recreation.
		ssh.MockCommand{Match: "docker inspect -f '{{join .Config.Cmd", Output: "caddy run --config /etc/caddy/Caddyfile --adapter caddyfile"},
		ssh.MockCommand{Match: "docker inspect -f '{{range .Mounts}}", Output: "/data /config /etc/caddy /deployments "},
		ssh.MockCommand{Match: "docker inspect -f '{{.State.Running}}'", Output: "true"},
	)

	var buf bytes.Buffer
	if err := setupServer(context.Background(), mock, &buf, true); err != nil {
		t.Fatalf("setupServer: %v", err)
	}

	if !strings.Contains(buf.String(), "Caddy already running") {
		t.Error("should report Caddy already running")
	}

	for _, call := range mock.Calls {
		if strings.Contains(call, "docker") && strings.Contains(call, "run") && strings.Contains(call, "-d") {
			t.Error("should not start Caddy when already running")
		}
	}
}

func TestSetupServer_CaddyUpgradePreservesNetworksAndCaddyfile(t *testing.T) {
	// Simulates an existing server running the legacy admin-API model: Caddy
	// was launched WITH --resume, the Caddyfile holds real production routes,
	// and Caddy is on multiple networks. Migrating to the Caddyfile-
	// authoritative model must recreate Caddy WITHOUT --resume, preserve the
	// Caddyfile, and reattach non-teploy networks.
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "whoami", Output: "root"},
		ssh.MockCommand{Match: "docker --version", Output: "Docker version 24.0.0"},
		ssh.MockCommand{Match: "rsync --version", Output: "rsync  version 3.2.7"},
		ssh.MockCommand{Match: "ufw status", Err: fmt.Errorf("not found")},
		ssh.MockCommand{Match: "systemctl", Err: fmt.Errorf("inactive")},
		ssh.MockCommand{Match: "docker info", Output: ""},
		ssh.MockCommand{Match: "docker network", Output: "teploy"},
		ssh.MockCommand{Match: "mkdir", Output: ""},
		ssh.MockCommand{Match: "chown", Output: ""},
		ssh.MockCommand{Match: "[ -f /deployments/caddy/Caddyfile ]", Output: "present"},
		ssh.MockCommand{Match: "sed -i", Output: ""},
		ssh.MockCommand{Match: "docker ps -a --filter name=", Output: "caddy"},
		// Legacy Caddy cmd: launched WITH --resume, must migrate.
		ssh.MockCommand{Match: "docker inspect -f '{{join .Config.Cmd", Output: "caddy run --config /etc/caddy/Caddyfile --adapter caddyfile --resume"},
		ssh.MockCommand{Match: "docker inspect -f '{{range .Mounts}}", Output: "/data /config /etc/caddy/Caddyfile "},
		ssh.MockCommand{Match: "docker inspect -f '{{json .Mounts}}'", Output: `[{"Type":"volume","Name":"caddy_data","Source":"/var/lib/docker/volumes/caddy_data/_data","Destination":"/data","RW":true},{"Type":"volume","Name":"caddy_config","Source":"/var/lib/docker/volumes/caddy_config/_data","Destination":"/config","RW":true},{"Type":"bind","Source":"/deployments/caddy/Caddyfile","Destination":"/etc/caddy/Caddyfile","RW":true}]`},
		// Extra networks the existing caddy is attached to.
		ssh.MockCommand{Match: "docker inspect -f '{{range $k,$v := .NetworkSettings.Networks}}", Output: "teploy dokploy-network bridge "},
		ssh.MockCommand{Match: "docker rm -f caddy", Output: ""},
		ssh.MockCommand{Match: "docker run", Output: "caddy_id"},
		ssh.MockCommand{Match: "docker network connect dokploy-network caddy", Output: ""},
		ssh.MockCommand{Match: "docker network connect bridge caddy", Output: ""},
	)

	var buf bytes.Buffer
	if err := setupServer(context.Background(), mock, &buf, true); err != nil {
		t.Fatalf("setupServer: %v", err)
	}

	// The stub Caddyfile must NOT have been uploaded — the existing one is preserved.
	if _, uploaded := mock.Files["/deployments/caddy/Caddyfile"]; uploaded {
		t.Error("Caddyfile should have been preserved, not overwritten with stub")
	}
	if !strings.Contains(buf.String(), "Existing Caddyfile preserved") {
		t.Error("should report existing Caddyfile preserved")
	}

	// The upgraded container must launch WITHOUT --resume (Caddyfile-
	// authoritative) while keeping the /config volume.
	var runCmd string
	for _, c := range mock.Calls {
		if strings.HasPrefix(c, "docker run") {
			runCmd = c
		}
	}
	if !strings.Contains(runCmd, "caddy_config:/config") {
		t.Errorf("recreated Caddy missing %q\ngot: %s", "caddy_config:/config", runCmd)
	}
	if strings.Contains(runCmd, "--resume") {
		t.Errorf("recreated Caddy must not use --resume\ngot: %s", runCmd)
	}

	// Must reattach dokploy-network and bridge, but not the base teploy network.
	foundDokploy, foundBridge, foundTeployReattach := false, false, false
	for _, c := range mock.Calls {
		if strings.Contains(c, "docker network connect dokploy-network caddy") {
			foundDokploy = true
		}
		if strings.Contains(c, "docker network connect bridge caddy") {
			foundBridge = true
		}
		if strings.Contains(c, "docker network connect teploy caddy") {
			foundTeployReattach = true
		}
	}
	if !foundDokploy {
		t.Error("should reattach dokploy-network to recreated Caddy")
	}
	if !foundBridge {
		t.Error("should reattach bridge network to recreated Caddy")
	}
	if foundTeployReattach {
		t.Error("should not reattach base teploy network — already attached via docker run")
	}
}

// TestSetupServer_PreservesForeignMountsOnRecreate reproduces a real,
// confirmed production risk: a server adopted from other tooling (or
// running an older teploy) can have Caddy mounts teploy itself never
// created — e.g. a hand-added /srv/static bind mount serving live static
// sites, discovered live on a real box with ~12 domains depending on it.
// Recreating Caddy (here, because it's missing the new /deployments mount)
// must preserve those foreign mounts exactly, the same way it already
// preserves extra networks — otherwise every site served through them
// 404s the instant the new container starts, without teploy ever having
// touched the Caddyfile or any of those sites' actual content.
func TestSetupServer_PreservesForeignMountsOnRecreate(t *testing.T) {
	mountsJSON := `[` +
		`{"Type":"volume","Name":"caddy_data","Source":"/var/lib/docker/volumes/caddy_data/_data","Destination":"/data","RW":true},` +
		`{"Type":"bind","Source":"/deployments/caddy","Destination":"/etc/caddy","RW":true},` +
		`{"Type":"bind","Source":"/deployments/static","Destination":"/srv/static","RW":false},` +
		`{"Type":"bind","Source":"/deployments/dreamlucidgroup","Destination":"/srv/static/dreamlucidgroup","RW":false},` +
		`{"Type":"volume","Name":"caddy_config","Source":"/var/lib/docker/volumes/caddy_config/_data","Destination":"/config","RW":true}` +
		`]`

	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "whoami", Output: "root"},
		ssh.MockCommand{Match: "docker --version", Output: "Docker version 24.0.0"},
		ssh.MockCommand{Match: "rsync --version", Output: "rsync  version 3.2.7"},
		ssh.MockCommand{Match: "ufw status", Err: fmt.Errorf("not found")},
		ssh.MockCommand{Match: "systemctl", Err: fmt.Errorf("inactive")},
		ssh.MockCommand{Match: "docker info", Output: ""},
		ssh.MockCommand{Match: "docker network", Output: "teploy"},
		ssh.MockCommand{Match: "mkdir", Output: ""},
		ssh.MockCommand{Match: "chown", Output: ""},
		ssh.MockCommand{Match: "[ -f /deployments/caddy/Caddyfile ]", Output: "absent"},
		ssh.MockCommand{Match: "sed -i", Output: ""},
		ssh.MockCommand{Match: "docker ps -a --filter name=", Output: "caddy"},
		// No --resume, directory-mounted, but missing /deployments — the
		// new static-mount check triggers a recreate.
		ssh.MockCommand{Match: "docker inspect -f '{{join .Config.Cmd", Output: "caddy run --config /etc/caddy/Caddyfile --adapter caddyfile"},
		ssh.MockCommand{Match: "docker inspect -f '{{range .Mounts}}", Output: "/data /config /etc/caddy /srv/static /srv/static/dreamlucidgroup "},
		ssh.MockCommand{Match: "docker inspect -f '{{range $k,$v := .NetworkSettings.Networks}}", Output: "teploy "},
		ssh.MockCommand{Match: "docker inspect -f '{{json .Mounts}}'", Output: mountsJSON},
		ssh.MockCommand{Match: "docker rm -f caddy", Output: ""},
		ssh.MockCommand{Match: "docker run", Output: "caddy_id"},
	)

	var buf bytes.Buffer
	if err := setupServer(context.Background(), mock, &buf, true); err != nil {
		t.Fatalf("setupServer: %v", err)
	}

	var runCmd string
	for _, c := range mock.Calls {
		if strings.HasPrefix(c, "docker run") {
			runCmd = c
		}
	}
	for _, want := range []string{
		"/deployments/static:/srv/static:ro",
		"/deployments/dreamlucidgroup:/srv/static/dreamlucidgroup:ro",
	} {
		if !strings.Contains(runCmd, want) {
			t.Errorf("recreated Caddy must preserve the foreign mount %q, got: %s", want, runCmd)
		}
	}
	// teploy's own mounts must still be there too — this isn't instead of
	// the standard set, it's in addition to it.
	if !strings.Contains(runCmd, "/deployments:/deployments:ro") {
		t.Errorf("recreated Caddy missing its own new static mount, got: %s", runCmd)
	}
	if !strings.Contains(buf.String(), "Additional mounts to preserve") {
		t.Error("should report the foreign mounts being preserved")
	}
}

// TestSetupServer_MigratesLegacyFileMount covers a server already on the
// no-resume model but still using the old single-file Caddyfile bind mount.
// That mount is pinned to a stale inode after teploy's atomic Caddyfile
// writes, so setup must recreate Caddy onto the directory mount even though
// --resume is absent.
func TestSetupServer_MigratesLegacyFileMount(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "whoami", Output: "root"},
		ssh.MockCommand{Match: "docker --version", Output: "Docker version 24.0.0"},
		ssh.MockCommand{Match: "rsync --version", Output: "rsync  version 3.2.7"},
		ssh.MockCommand{Match: "ufw status", Err: fmt.Errorf("not found")},
		ssh.MockCommand{Match: "systemctl", Err: fmt.Errorf("inactive")},
		ssh.MockCommand{Match: "docker info", Output: ""},
		ssh.MockCommand{Match: "docker network", Output: "teploy"},
		ssh.MockCommand{Match: "mkdir", Output: ""},
		ssh.MockCommand{Match: "chown", Output: ""},
		ssh.MockCommand{Match: "[ -f /deployments/caddy/Caddyfile ]", Output: "absent"},
		ssh.MockCommand{Match: "sed -i", Output: ""},
		ssh.MockCommand{Match: "docker ps -a --filter name=", Output: "caddy"},
		// No --resume, but the legacy single-file mount is present → recreate.
		ssh.MockCommand{Match: "docker inspect -f '{{join .Config.Cmd", Output: "caddy run --config /etc/caddy/Caddyfile --adapter caddyfile"},
		ssh.MockCommand{Match: "docker inspect -f '{{range .Mounts}}", Output: "/data /config /etc/caddy/Caddyfile "},
		ssh.MockCommand{Match: "docker inspect -f '{{json .Mounts}}'", Output: `[{"Type":"volume","Name":"caddy_data","Source":"/var/lib/docker/volumes/caddy_data/_data","Destination":"/data","RW":true},{"Type":"volume","Name":"caddy_config","Source":"/var/lib/docker/volumes/caddy_config/_data","Destination":"/config","RW":true},{"Type":"bind","Source":"/deployments/caddy/Caddyfile","Destination":"/etc/caddy/Caddyfile","RW":true}]`},
		ssh.MockCommand{Match: "docker inspect -f '{{range $k,$v := .NetworkSettings.Networks}}", Output: "teploy "},
		ssh.MockCommand{Match: "docker rm -f caddy", Output: ""},
		ssh.MockCommand{Match: "docker run", Output: "caddy_id"},
	)

	var buf bytes.Buffer
	if err := setupServer(context.Background(), mock, &buf, true); err != nil {
		t.Fatalf("setupServer: %v", err)
	}

	// Must have removed and recreated Caddy.
	removed, recreatedDirMount := false, false
	for _, c := range mock.Calls {
		if strings.HasPrefix(c, "docker rm -f caddy") {
			removed = true
		}
		if strings.HasPrefix(c, "docker run") && strings.Contains(c, "/deployments/caddy:/etc/caddy") {
			recreatedDirMount = true
		}
	}
	if !removed {
		t.Error("legacy file-mount Caddy should have been removed")
	}
	if !recreatedDirMount {
		t.Error("recreated Caddy must use the directory mount /deployments/caddy:/etc/caddy")
	}
	if !strings.Contains(buf.String(), "single-file Caddyfile mount") {
		t.Errorf("should explain the file-mount migration reason, got:\n%s", buf.String())
	}
}

// TestSetupServer_MigratesMissingStaticMount reproduces a real bug found
// live: teploy setup never bind-mounted anything at /deployments (only the
// narrower /deployments/caddy, for the Caddyfile itself), so type:static
// deploys were completely non-functional on every server it had ever
// provisioned — Deploy writes releases to /deployments/<app>/current on
// the host, but that path didn't exist inside the Caddy container at all.
// `teploy deploy` reported success (the release really did land on disk)
// while every request 404'd. setupServer must detect a Caddy container
// missing this mount and recreate it, the same way it already does for
// the two older legacy-mount conditions.
func TestSetupServer_MigratesMissingStaticMount(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "whoami", Output: "root"},
		ssh.MockCommand{Match: "docker --version", Output: "Docker version 24.0.0"},
		ssh.MockCommand{Match: "rsync --version", Output: "rsync  version 3.2.7"},
		ssh.MockCommand{Match: "ufw status", Err: fmt.Errorf("not found")},
		ssh.MockCommand{Match: "systemctl", Err: fmt.Errorf("inactive")},
		ssh.MockCommand{Match: "docker info", Output: ""},
		ssh.MockCommand{Match: "docker network", Output: "teploy"},
		ssh.MockCommand{Match: "mkdir", Output: ""},
		ssh.MockCommand{Match: "chown", Output: ""},
		ssh.MockCommand{Match: "[ -f /deployments/caddy/Caddyfile ]", Output: "absent"},
		ssh.MockCommand{Match: "sed -i", Output: ""},
		ssh.MockCommand{Match: "docker ps -a --filter name=", Output: "caddy"},
		// Already on the directory-mount model (no --resume, /etc/caddy
		// directory mount) — but missing the /deployments mount entirely,
		// exactly what every server provisioned before this fix looks like.
		ssh.MockCommand{Match: "docker inspect -f '{{join .Config.Cmd", Output: "caddy run --config /etc/caddy/Caddyfile --adapter caddyfile"},
		ssh.MockCommand{Match: "docker inspect -f '{{range .Mounts}}", Output: "/data /config /etc/caddy "},
		ssh.MockCommand{Match: "docker inspect -f '{{json .Mounts}}'", Output: `[{"Type":"volume","Name":"caddy_data","Source":"/var/lib/docker/volumes/caddy_data/_data","Destination":"/data","RW":true},{"Type":"volume","Name":"caddy_config","Source":"/var/lib/docker/volumes/caddy_config/_data","Destination":"/config","RW":true},{"Type":"bind","Source":"/deployments/caddy","Destination":"/etc/caddy","RW":true}]`},
		ssh.MockCommand{Match: "docker inspect -f '{{range $k,$v := .NetworkSettings.Networks}}", Output: "teploy "},
		ssh.MockCommand{Match: "docker rm -f caddy", Output: ""},
		ssh.MockCommand{Match: "docker run", Output: "caddy_id"},
	)

	var buf bytes.Buffer
	if err := setupServer(context.Background(), mock, &buf, true); err != nil {
		t.Fatalf("setupServer: %v", err)
	}

	removed, recreatedWithStaticMount := false, false
	for _, c := range mock.Calls {
		if strings.HasPrefix(c, "docker rm -f caddy") {
			removed = true
		}
		if strings.HasPrefix(c, "docker run") && strings.Contains(c, "/deployments:/deployments:ro") {
			recreatedWithStaticMount = true
		}
	}
	if !removed {
		t.Error("Caddy missing the /deployments mount should have been removed")
	}
	if !recreatedWithStaticMount {
		t.Error("recreated Caddy must include the /deployments:/deployments:ro mount")
	}
	if !strings.Contains(buf.String(), "type:static deploys require") {
		t.Errorf("should explain the missing-static-mount migration reason, got:\n%s", buf.String())
	}
}

func TestSetupServer_UFWActive(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "whoami", Output: "root"},
		ssh.MockCommand{Match: "docker --version", Output: "Docker version 24.0.0"},
		ssh.MockCommand{Match: "rsync --version", Output: "rsync  version 3.2.7"},
		ssh.MockCommand{Match: "ufw status", Output: "Status: active\n\nTo Action From\n22/tcp ALLOW Anywhere"},
		ssh.MockCommand{Match: "ufw allow 80", Output: "Rule added"},
		ssh.MockCommand{Match: "ufw allow 443", Output: "Rule added"},
		ssh.MockCommand{Match: "docker info", Output: ""},
		ssh.MockCommand{Match: "docker network", Output: "teploy"},
		ssh.MockCommand{Match: "mkdir", Output: ""},
		ssh.MockCommand{Match: "chown", Output: ""},
		ssh.MockCommand{Match: "[ -f /deployments/caddy/Caddyfile ]", Output: "absent"},
		ssh.MockCommand{Match: "docker ps -a --filter name=", Output: ""},
		ssh.MockCommand{Match: "docker run", Output: "caddy_id"},
	)

	var buf bytes.Buffer
	if err := setupServer(context.Background(), mock, &buf, true); err != nil {
		t.Fatalf("setupServer: %v", err)
	}

	if !strings.Contains(buf.String(), "Opened ports 80 and 443") {
		t.Error("should report ports opened")
	}
}

// TestInstallSudoViaSu_SecretTransport pins the C08 transport for the
// root password: it travels the session stdin (recorded in Inputs),
// never a command string, and no /tmp script artifact is uploaded.
func TestInstallSudoViaSu_SecretTransport(t *testing.T) {
	mock := ssh.NewMockExecutor("h",
		ssh.MockCommand{Match: "su -c", Output: "TEPLOY_SUDO_OK\n"},
	)
	err := installSudoViaSu(context.Background(), mock, "tyler", "root-pw-123")
	if err != nil {
		t.Fatalf("installSudoViaSu: %v", err)
	}
	for _, call := range mock.Calls {
		if strings.Contains(call, "root-pw-123") {
			t.Errorf("root password in command argv: %s", call)
		}
		if strings.HasPrefix(call, "UPLOAD:") && strings.Contains(call, "/tmp/") {
			t.Errorf("the su path must not stage a script artifact: %s", call)
		}
	}
	if len(mock.Inputs) != 1 || mock.Inputs[0] != "root-pw-123\n" {
		t.Fatalf("root password must ride stdin as one line, inputs: %v", mock.Inputs)
	}
	if call := mock.Calls[0]; !strings.HasPrefix(call, "su -c ") || !strings.Contains(call, "TEPLOY_SUDO_OK") {
		t.Fatalf("unexpected su command shape: %s", call)
	}
}

// TestInstallSudoViaSu_WrongPassword keeps the actionable failure.
func TestInstallSudoViaSu_WrongPassword(t *testing.T) {
	mock := ssh.NewMockExecutor("h",
		ssh.MockCommand{Match: "su -c", Err: errors.New("exit status 1: su: Authentication failure")},
	)
	err := installSudoViaSu(context.Background(), mock, "tyler", "nope")
	if err == nil || !strings.Contains(err.Error(), "wrong root password") {
		t.Fatalf("wrong password = %v, want wrong-root-password error", err)
	}
}

// TestVPNJoinCommand_NoCredentialInArgv pins the join shape: the key
// file is read, removed, and fed via the provider env var — the literal
// credential appears nowhere in the command.
func TestVPNJoinCommand_NoCredentialInArgv(t *testing.T) {
	cfg := network.Config{Provider: "tailscale", AuthKey: "tskey-auth-secret123"}
	cmd := vpnJoinCommand("tailscale", cfg, "sudo ", "/tmp/teploy-vpn-key-abc")
	if strings.Contains(cmd, "tskey-auth-secret123") {
		t.Fatalf("auth key leaked into the join command: %s", cmd)
	}
	for _, want := range []string{
		"nohup sh -c ",
		"k=$(cat /tmp/teploy-vpn-key-abc)",
		"rm -f -- /tmp/teploy-vpn-key-abc",
		`export TS_AUTHKEY="$k"`,
		"exec tailscale up --accept-routes",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("join command missing %q:\n%s", want, cmd)
		}
	}

	headscale := network.Config{Provider: "headscale", AuthKey: "tskey-secret", Server: "https://hs.example.com"}
	cmd = vpnJoinCommand("headscale", headscale, "", "/tmp/k")
	if strings.Contains(cmd, "tskey-secret") {
		t.Fatalf("headscale auth key leaked: %s", cmd)
	}
	if !strings.Contains(cmd, "--login-server=") || !strings.Contains(cmd, "https://hs.example.com") {
		t.Errorf("headscale login server missing: %s", cmd)
	}

	nb := network.Config{Provider: "netbird", SetupKey: "nb-secret"}
	cmd = vpnJoinCommand("netbird", nb, "", "/tmp/k")
	if strings.Contains(cmd, "nb-secret") {
		t.Fatalf("netbird setup key leaked: %s", cmd)
	}
	if !strings.Contains(cmd, `export NB_SETUP_KEY="$k"`) || !strings.Contains(cmd, "exec netbird up") {
		t.Errorf("netbird join shape wrong: %s", cmd)
	}
}

// TestJoinVPNMesh_StagesPrivateFileAndCleansUp pins the staging
// mechanics: the credential is uploaded 0600 exactly once, and the join
// command references that path.
func TestJoinVPNMesh_StagesPrivateFileAndCleansUp(t *testing.T) {
	mock := ssh.NewMockExecutor("h",
		ssh.MockCommand{Match: "nohup sh -c", Output: ""},
	)
	cfg := network.Config{Provider: "tailscale", AuthKey: "tskey-auth-x"}
	if err := joinVPNMesh(context.Background(), mock, "", "tailscale", cfg); err != nil {
		t.Fatalf("joinVPNMesh: %v", err)
	}
	uploads := 0
	for _, call := range mock.Calls {
		if strings.HasPrefix(call, "UPLOAD:") {
			uploads++
			if !strings.Contains(call, "mode 0600") || !strings.Contains(call, "/tmp/teploy-vpn-key-") {
				t.Errorf("credential must upload 0600 to the key path: %s", call)
			}
		}
		if strings.Contains(call, "tskey-auth-x") {
			t.Errorf("credential in command argv: %s", call)
		}
	}
	if uploads != 1 {
		t.Fatalf("expected exactly one credential upload, got %d (calls: %v)", uploads, mock.Calls)
	}
}

// TestRegistryLoginOnServer_PasswordNeverInArgv pins the registry login
// transport: the password rides stdin (docker --password-stdin), and no
// command string — including the printf pipe form — carries it.
func TestRegistryLoginOnServer_PasswordNeverInArgv(t *testing.T) {
	mock := ssh.NewMockExecutor("h",
		ssh.MockCommand{Match: "docker login", Output: "Login Succeeded"},
	)
	if err := registryLoginOnServer(context.Background(), mock, "ghcr.io", "user", "pw-hunter2"); err != nil {
		t.Fatalf("registryLoginOnServer: %v", err)
	}
	if len(mock.Calls) != 1 {
		t.Fatalf("expected one command, got %v", mock.Calls)
	}
	call := mock.Calls[0]
	if !strings.HasPrefix(call, "docker login 'ghcr.io' -u 'user' --password-stdin") || strings.Contains(call, "printf") {
		t.Errorf("login command shape wrong: %s", call)
	}
	if strings.Contains(call, "pw-hunter2") {
		t.Errorf("password in command argv: %s", call)
	}
	if len(mock.Inputs) != 1 || mock.Inputs[0] != "pw-hunter2" {
		t.Fatalf("password must ride stdin verbatim, inputs: %v", mock.Inputs)
	}
}

// TestRegistryLoginOnServer_FailureSurfacesDiagnostics pins that a
// refused login names docker's own stderr (RunInput's discard-both
// contract used to hide it).
func TestRegistryLoginOnServer_FailureSurfacesDiagnostics(t *testing.T) {
	mock := ssh.NewMockExecutor("h",
		ssh.MockCommand{Match: "docker login", Err: errors.New("exit status 1: Error response from daemon: unauthorized")},
	)
	err := registryLoginOnServer(context.Background(), mock, "ghcr.io", "user", "bad")
	if err == nil || !strings.Contains(err.Error(), "unauthorized") {
		t.Fatalf("refused login = %v, want docker's stderr surfaced", err)
	}
}

// TestSetupServer_PreflightListsStagesAndAffectedResources pins the
// C08 preflight: before touching anything, setup states every stage
// and what it affects — the affected-resource list the operator reads
// before confirming a run against an existing box.
func TestSetupServer_PreflightListsStagesAndAffectedResources(t *testing.T) {
	stages := setupStages()
	if len(stages) < 5 {
		t.Fatalf("expected the provisioning plan to have stages, got %d", len(stages))
	}
	seen := map[string]bool{}
	for _, s := range stages {
		if s.name == "" || s.affects == "" {
			t.Fatalf("every stage needs a name and an affected-resource description: %+v", s)
		}
		if seen[s.name] {
			t.Fatalf("stage names must be unique for stage-named errors: %q", s.name)
		}
		seen[s.name] = true
	}

	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "whoami", Output: "root"},
		ssh.MockCommand{Match: "docker --version", Output: "Docker version 24.0.0"},
		ssh.MockCommand{Match: "rsync --version", Output: "rsync  version 3.2.7"},
		ssh.MockCommand{Match: "ufw status", Err: fmt.Errorf("command not found")},
		ssh.MockCommand{Match: "systemctl is-active firewalld", Err: fmt.Errorf("inactive")},
		ssh.MockCommand{Match: "docker info", Output: ""},
		ssh.MockCommand{Match: "docker network", Output: "teploy"},
		ssh.MockCommand{Match: "mkdir", Output: ""},
		ssh.MockCommand{Match: "chown", Output: ""},
		ssh.MockCommand{Match: "[ -f /deployments/caddy/Caddyfile ]", Output: "absent"},
		ssh.MockCommand{Match: "docker ps -a --filter name=", Output: ""},
		ssh.MockCommand{Match: "docker run", Output: "caddy_id"},
	)
	var buf bytes.Buffer
	if err := setupServer(context.Background(), mock, &buf, true); err != nil {
		t.Fatalf("setupServer: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "Preflight") {
		t.Error("the run must open with the preflight list")
	}
	for _, s := range stages {
		if !strings.Contains(out, s.name+": "+s.affects) {
			t.Errorf("preflight must list stage %q with its affected resources", s.name)
		}
	}
}

// TestSetupServer_InterruptedRunIsResumable pins C08's interrupted-setup
// acceptance: a connection death mid-setup fails with the stage named,
// and re-running setup against the SAME partially-provisioned server
// completes by skipping what already happened — no package reinstalls,
// no second Caddyfile write, no second container start.
func TestSetupServer_InterruptedRunIsResumable(t *testing.T) {
	// Phase 1: the connection dies at the docker-network stage. Every
	// earlier stage's work (docker present, dirs created) has already
	// happened on the server.
	phase1 := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "whoami", Output: "root"},
		ssh.MockCommand{Match: "docker --version", Output: "Docker version 24.0.0"},
		ssh.MockCommand{Match: "rsync --version", Output: "rsync  version 3.2.7"},
		ssh.MockCommand{Match: "ufw status", Err: fmt.Errorf("command not found")},
		ssh.MockCommand{Match: "systemctl is-active firewalld", Err: fmt.Errorf("inactive")},
		ssh.MockCommand{Match: "docker info", Output: ""},
		ssh.MockCommand{Match: "docker network inspect teploy", Err: errors.New("ssh: connection reset by peer")},
	)
	var out1 bytes.Buffer
	err := setupServer(context.Background(), phase1, &out1, true)
	if err == nil {
		t.Fatal("a mid-setup connection death must fail the run")
	}
	if !strings.Contains(err.Error(), `setup stage "docker network"`) {
		t.Fatalf("the failure must name the stage that died, got: %v", err)
	}
	if !strings.Contains(err.Error(), "connection reset") {
		t.Fatalf("the failure must carry the transport error, got: %v", err)
	}

	// Phase 2: the operator re-runs setup. The server now has docker,
	// rsync, the teploy network, the directories, the Caddyfile, and a
	// running modern caddy — everything the interrupted run got to.
	phase2 := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "whoami", Output: "root"},
		ssh.MockCommand{Match: "docker --version", Output: "Docker version 24.0.0"},
		ssh.MockCommand{Match: "rsync --version", Output: "rsync  version 3.2.7"},
		ssh.MockCommand{Match: "ufw status", Err: fmt.Errorf("command not found")},
		ssh.MockCommand{Match: "systemctl is-active firewalld", Err: fmt.Errorf("inactive")},
		ssh.MockCommand{Match: "docker info", Output: ""},
		ssh.MockCommand{Match: "docker network inspect teploy", Output: "teploy"},
		ssh.MockCommand{Match: "mkdir", Output: ""},
		ssh.MockCommand{Match: "chown", Output: ""},
		ssh.MockCommand{Match: "[ -f /deployments/caddy/Caddyfile ]", Output: "present"},
		ssh.MockCommand{Match: "sed -i", Output: ""},
		ssh.MockCommand{Match: "docker ps -a --filter name=", Output: "caddy"},
		ssh.MockCommand{Match: "docker inspect -f '{{join .Config.Cmd", Output: "caddy run --config /etc/caddy/Caddyfile --adapter caddyfile"},
		ssh.MockCommand{Match: "docker inspect -f '{{range .Mounts}}", Output: "/data /config /etc/caddy /deployments "},
		ssh.MockCommand{Match: "docker inspect -f '{{.State.Running}}'", Output: "true"},
	)
	var out2 bytes.Buffer
	if err := setupServer(context.Background(), phase2, &out2, true); err != nil {
		t.Fatalf("the re-run must complete: %v", err)
	}
	for _, want := range []string{
		"Docker already installed",
		"rsync already installed",
		"Existing Caddyfile preserved",
		"Caddy already running",
		"Server provisioned successfully",
	} {
		if !strings.Contains(out2.String(), want) {
			t.Errorf("re-run output missing %q:\n%s", want, out2.String())
		}
	}
	// The re-run must not redo completed work: no installer script, no
	// package install, no Caddyfile write, no container start. (The
	// network stage's compound `inspect || create` text always names
	// create; the registration pins that INSPECT succeeded, so the
	// create side never fired.)
	for _, call := range phase2.Calls {
		for _, forbidden := range []string{"get.docker.com", "apt-get install", "docker run"} {
			if strings.Contains(call, forbidden) {
				t.Errorf("the resumed run must not repeat %q: %s", forbidden, call)
			}
		}
	}
	if _, uploaded := phase2.Files["/deployments/caddy/Caddyfile"]; uploaded {
		t.Error("the resumed run must not rewrite an existing Caddyfile")
	}
}

// TestSetupServer_ConnectionLossRecoversMidStage pins the connection
// recovery wiring end-to-end at the flow level: setupServer running
// through a ReconnectingExecutor survives a one-shot transport death
// mid-stage and completes, the dead command retried exactly once.
// Registrations that model ran-and-failed commands carry "exit status
// N" (the mock contract) so the wrapper classifies them as completed —
// only the network-stage registration models a transport death.
func TestSetupServer_ConnectionLossRecoversMidStage(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "whoami", Output: "root"},
		ssh.MockCommand{Match: "docker --version", Output: "Docker version 24.0.0"},
		ssh.MockCommand{Match: "rsync --version", Output: "rsync  version 3.2.7"},
		ssh.MockCommand{Match: "ufw status", Err: fmt.Errorf("exit status 1: ufw: command not found")},
		ssh.MockCommand{Match: "systemctl is-active firewalld", Err: fmt.Errorf("exit status 3: inactive")},
		ssh.MockCommand{Match: "docker info", Output: ""},
		// First attempt at the network stage dies with a transport
		// error; the redialed retry finds the network (inspect OK).
		ssh.MockCommand{Match: "docker network inspect teploy", Err: errors.New("ssh: connection reset by peer"), Once: true},
		ssh.MockCommand{Match: "docker network inspect teploy", Output: "teploy"},
		ssh.MockCommand{Match: "mkdir", Output: ""},
		ssh.MockCommand{Match: "chown", Output: ""},
		ssh.MockCommand{Match: "[ -f /deployments/caddy/Caddyfile ]", Output: "absent"},
		ssh.MockCommand{Match: "docker ps -a --filter name=", Output: ""},
		ssh.MockCommand{Match: "docker run", Output: "caddy_id"},
	)
	wrapper, err := ssh.NewReconnectingExecutor(context.Background(), func(ctx context.Context) (ssh.Executor, error) {
		return mock, nil
	}, 1)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if setupErr := setupServer(context.Background(), wrapper, &buf, true); setupErr != nil {
		t.Fatalf("a one-shot connection loss mid-stage must recover, got: %v", setupErr)
	}
	if !strings.Contains(buf.String(), "Server provisioned successfully") {
		t.Errorf("recovery must complete the flow:\n%s", buf.String())
	}
	attempts := 0
	for _, call := range mock.Calls {
		if strings.HasPrefix(call, "docker network inspect teploy") {
			attempts++
		}
	}
	if attempts != 2 {
		t.Fatalf("the dead network command must be retried exactly once, attempts = %d", attempts)
	}
}

// TestInstallAuthorizedKey_GuardedAgainstDuplicateAppend pins the
// resumability guard for the password-path key install: the append is
// guarded by a membership check, so an interrupted setup re-run cannot
// stack duplicate authorized_keys entries (the old bare `echo >>`
// appended on every attempt).
func TestInstallAuthorizedKey_GuardedAgainstDuplicateAppend(t *testing.T) {
	pubKey := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExample teploy-test"
	mock := ssh.NewMockExecutor("h",
		ssh.MockCommand{Match: "mkdir -p ~/.ssh", Output: ""},
	)
	for i := 0; i < 2; i++ { // first install, then the re-run after an interrupted setup
		if err := installAuthorizedKey(context.Background(), mock, pubKey); err != nil {
			t.Fatalf("installAuthorizedKey attempt %d: %v", i+1, err)
		}
	}
	guarded := "mkdir -p ~/.ssh && grep -qF " + ssh.ShellQuote(pubKey) +
		" ~/.ssh/authorized_keys 2>/dev/null || echo " + ssh.ShellQuote(pubKey) +
		" >> ~/.ssh/authorized_keys; chmod 700 ~/.ssh && chmod 600 ~/.ssh/authorized_keys"
	if len(mock.Calls) != 2 {
		t.Fatalf("expected one command per attempt, calls: %v", mock.Calls)
	}
	for i, call := range mock.Calls {
		if call != guarded {
			t.Fatalf("attempt %d must use the guarded append:\ngot:  %s\nwant: %s", i+1, call, guarded)
		}
	}
}
