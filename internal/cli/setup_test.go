package cli

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/ssh"
)

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
		ssh.MockCommand{Match: "test -s /deployments/caddy/Caddyfile", Err: fmt.Errorf("no such file")},
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
		ssh.MockCommand{Match: "test -s /deployments/caddy/Caddyfile", Output: ""},
		ssh.MockCommand{Match: "sed -i", Output: ""},
		ssh.MockCommand{Match: "docker ps -a --filter name=", Output: "caddy"},
		// Existing Caddy already on the new model: no --resume, directory-
		// mounted (/etc/caddy), AND has the /deployments mount type:static
		// deploys need. All three checks pass → skip recreation.
		ssh.MockCommand{Match: "docker inspect -f '{{join .Config.Cmd", Output: "caddy run --config /etc/caddy/Caddyfile --adapter caddyfile"},
		ssh.MockCommand{Match: "docker inspect -f '{{range .Mounts}}", Output: "/data /config /etc/caddy /deployments "},
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
		ssh.MockCommand{Match: "test -s /deployments/caddy/Caddyfile", Output: ""},
		ssh.MockCommand{Match: "sed -i", Output: ""},
		ssh.MockCommand{Match: "docker ps -a --filter name=", Output: "caddy"},
		// Legacy Caddy cmd: launched WITH --resume, must migrate.
		ssh.MockCommand{Match: "docker inspect -f '{{join .Config.Cmd", Output: "caddy run --config /etc/caddy/Caddyfile --adapter caddyfile --resume"},
		ssh.MockCommand{Match: "docker inspect -f '{{range .Mounts}}", Output: "/data /config /etc/caddy/Caddyfile "},
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
		ssh.MockCommand{Match: "test -s /deployments/caddy/Caddyfile", Output: ""},
		ssh.MockCommand{Match: "sed -i", Output: ""},
		ssh.MockCommand{Match: "docker ps -a --filter name=", Output: "caddy"},
		// No --resume, but the legacy single-file mount is present → recreate.
		ssh.MockCommand{Match: "docker inspect -f '{{join .Config.Cmd", Output: "caddy run --config /etc/caddy/Caddyfile --adapter caddyfile"},
		ssh.MockCommand{Match: "docker inspect -f '{{range .Mounts}}", Output: "/data /config /etc/caddy/Caddyfile "},
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
		ssh.MockCommand{Match: "test -s /deployments/caddy/Caddyfile", Output: ""},
		ssh.MockCommand{Match: "sed -i", Output: ""},
		ssh.MockCommand{Match: "docker ps -a --filter name=", Output: "caddy"},
		// Already on the directory-mount model (no --resume, /etc/caddy
		// directory mount) — but missing the /deployments mount entirely,
		// exactly what every server provisioned before this fix looks like.
		ssh.MockCommand{Match: "docker inspect -f '{{join .Config.Cmd", Output: "caddy run --config /etc/caddy/Caddyfile --adapter caddyfile"},
		ssh.MockCommand{Match: "docker inspect -f '{{range .Mounts}}", Output: "/data /config /etc/caddy "},
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
		ssh.MockCommand{Match: "test -s /deployments/caddy/Caddyfile", Err: fmt.Errorf("no such file")},
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
