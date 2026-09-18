package docker

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/ssh"
)

// richInspect is a container inspect fixture exercising every field the
// RecreateSpec must preserve (audit F20). Fields that older code dropped —
// entrypoint override, stop timeout/signal, log config, extra hosts,
// sysctls, tmpfs, capabilities, security opts, privileged/read-only, a
// second network — are all present.
func richInspect(name string) string {
	return fmt.Sprintf(`[{
  "Image": "sha256:%s",
  "Config": {
    "Image": "myapp:v9",
    "Env": ["PORT=3000", "TOKEN=sec;ret"],
    "Cmd": ["npm", "start"],
    "Entrypoint": ["./entrypoint.sh"],
    "WorkingDir": "/app dir",
    "User": "1000:1000",
    "Labels": {"teploy.app": "myapp", "teploy.version": "v9"},
    "StopSignal": "SIGQUIT",
    "StopTimeout": 25,
    "Healthcheck": {"Test": ["NONE"]}
  },
  "HostConfig": {
    "NetworkMode": "teploy",
    "PortBindings": {
      "3000/tcp": [{"HostIp": "127.0.0.1", "HostPort": "49152"}],
      "51820/udp": [{"HostIp": "0.0.0.0", "HostPort": "51820"}]
    },
    "Binds": ["/deployments/myapp/volumes/data:/data:ro"],
    "Mounts": [{"Type": "volume", "Source": "myapp-uploads", "Target": "/uploads"}],
    "RestartPolicy": {"Name": "unless-stopped"},
    "Memory": 536870912,
    "NanoCpus": 1500000000,
    "LogConfig": {"Type": "json-file", "Config": {"max-size": "10m"}},
    "ExtraHosts": ["host.docker.internal:host-gateway"],
    "Sysctls": {"net.core.somaxconn": "1024"},
    "Tmpfs": {"/scratch": "size=64m"},
    "CapAdd": ["NET_ADMIN"],
    "CapDrop": ["CHOWN"],
    "SecurityOpt": ["no-new-privileges"],
    "Privileged": false,
    "ReadonlyRootfs": true
  },
  "NetworkSettings": {
    "Networks": {
      "teploy": {"Aliases": ["myapp", "extra-alias"]},
      "mesh": {"Aliases": ["myapp-mesh"]}
    }
  }
}]`, strings.Repeat("a", 64))
}

func TestInspectRecreate_CapturesEveryPreservedField(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "docker inspect 'myapp-web-v9'", Output: richInspect("myapp-web-v9")},
	)
	spec, err := NewClient(mock).InspectRecreate(context.Background(), "myapp-web-v9")
	if err != nil {
		t.Fatalf("InspectRecreate: %v", err)
	}

	if spec.ImageID != "sha256:"+strings.Repeat("a", 64) || spec.ImageRef != "myapp:v9" {
		t.Errorf("image identity not captured: %+v", spec)
	}
	if len(spec.Entrypoint) != 1 || spec.Entrypoint[0] != "./entrypoint.sh" {
		t.Errorf("entrypoint not captured: %v", spec.Entrypoint)
	}
	if spec.StopTimeout != 25 || spec.StopSignal != "SIGQUIT" {
		t.Errorf("stop settings not captured: %d/%q", spec.StopTimeout, spec.StopSignal)
	}
	if spec.LogDriver != "json-file" || len(spec.LogOpts) != 1 || spec.LogOpts[0] != "max-size=10m" {
		t.Errorf("log config not captured: %s %v", spec.LogDriver, spec.LogOpts)
	}
	if len(spec.ExtraHosts) != 1 || spec.ExtraHosts[0] != "host.docker.internal:host-gateway" {
		t.Errorf("extra hosts not captured: %v", spec.ExtraHosts)
	}
	if spec.Sysctls["net.core.somaxconn"] != "1024" {
		t.Errorf("sysctls not captured: %v", spec.Sysctls)
	}
	if spec.Tmpfs["/scratch"] != "size=64m" {
		t.Errorf("tmpfs not captured: %v", spec.Tmpfs)
	}
	if len(spec.CapAdd) != 1 || spec.CapAdd[0] != "NET_ADMIN" || len(spec.CapDrop) != 1 || spec.CapDrop[0] != "CHOWN" {
		t.Errorf("capabilities not captured: %v %v", spec.CapAdd, spec.CapDrop)
	}
	if len(spec.SecurityOpt) != 1 || spec.SecurityOpt[0] != "no-new-privileges" {
		t.Errorf("security opts not captured: %v", spec.SecurityOpt)
	}
	if !spec.ReadonlyRootfs || spec.Privileged {
		t.Errorf("privileged/read-only not captured: %v/%v", spec.Privileged, spec.ReadonlyRootfs)
	}
	if len(spec.Networks) != 2 || spec.Networks[0] != "teploy" || spec.Networks[1] != "mesh" {
		t.Errorf("networks not captured (primary first): %v", spec.Networks)
	}
	// Aliases: primary network only, name auto-alias skipped, sorted.
	if len(spec.Aliases) != 2 || spec.Aliases[0] != "extra-alias" || spec.Aliases[1] != "myapp" {
		t.Errorf("aliases not captured as expected: %v", spec.Aliases)
	}
	if len(spec.PortBindings) != 2 {
		t.Fatalf("bindings not captured: %+v", spec.PortBindings)
	}
	udp := spec.PortBindings[1]
	if udp.ContainerPort != 51820 || udp.Proto != "udp" || udp.HostPort != 51820 {
		t.Errorf("udp binding not normalized: %+v", udp)
	}
	tcp := spec.PortBindings[0]
	if tcp.ContainerPort != 3000 || tcp.Proto != "tcp" || tcp.HostPort != 49152 || tcp.HostIP != "127.0.0.1" {
		t.Errorf("tcp binding not normalized: %+v", tcp)
	}
	if !spec.NoHealthcheck {
		t.Error("explicit-NONE healthcheck marker not captured")
	}
}

// TestRecreate_RendersEveryPreservedField is the F20 regression pin: the
// rendered docker run must carry every inspect field the spec captured.
// Each field that silently disappears from this command after a refactor
// is a field recreation stopped preserving.
func TestRecreate_RendersEveryPreservedField(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "docker inspect 'myapp-web-v9'", Output: richInspect("myapp-web-v9")},
		ssh.MockCommand{Match: "docker rm -f", Output: ""},
		ssh.MockCommand{Match: "docker run", Output: ""},
	)
	if err := NewClient(mock).Restart(context.Background(), "myapp-web-v9", nil); err != nil {
		t.Fatalf("Restart: %v", err)
	}

	var run string
	for _, c := range mock.Calls {
		if strings.HasPrefix(c, "docker run ") {
			run = c
		}
	}
	if run == "" {
		t.Fatal("no docker run issued")
	}

	for _, want := range []string{
		"--name 'myapp-web-v9'",
		"--network 'teploy'",
		"--network 'mesh'",
		"--network-alias 'extra-alias'",
		"--network-alias 'myapp'",
		"-p '127.0.0.1:49152:3000/tcp'",
		"-p '0.0.0.0:51820:51820/udp'",
		"-e 'PORT=3000'",
		"-e 'TOKEN=sec;ret'",
		"-v '/deployments/myapp/volumes/data:/data:ro'",
		"--mount 'type=volume,target=/uploads,source=myapp-uploads'",
		"--memory 536870912b",
		"--cpus 1.5",
		"--label 'teploy.app=myapp'",
		"--label 'teploy.version=v9'",
		"--no-healthcheck",
		"--restart 'unless-stopped'",
		"-w '/app dir'",
		"-u '1000:1000'",
		"--entrypoint './entrypoint.sh'",
		"--stop-timeout 25",
		"--stop-signal 'SIGQUIT'",
		"--log-opt 'max-size=10m'",
		"--add-host 'host.docker.internal:host-gateway'",
		"--sysctl 'net.core.somaxconn=1024'",
		"--tmpfs '/scratch:size=64m'",
		"--cap-add 'NET_ADMIN'",
		"--cap-drop 'CHOWN'",
		"--security-opt 'no-new-privileges'",
		"--read-only",
		"'sha256:" + strings.Repeat("a", 64) + "'",
		"'npm' 'start'",
	} {
		if !strings.Contains(run, want) {
			t.Errorf("rendered run is missing %s\n  run: %s", want, run)
		}
	}
	if strings.Contains(run, "--privileged") {
		t.Errorf("privileged rendered for an unprivileged container: %s", run)
	}
}

// A multi-element entrypoint that MATCHES the image's own must be dropped
// from the command (the recreated container inherits it from the immutable
// image), while one that DIFFERS cannot be represented through the CLI and
// must fail closed instead of silently mangling it.
func TestRecreate_MultiElementEntrypoint(t *testing.T) {
	inspect := fmt.Sprintf(`[{
  "Image": "sha256:%s",
  "Config": {"Image": "myapp:v9", "Entrypoint": ["docker-entrypoint.sh", "serve"], "Labels": {}},
  "HostConfig": {"PortBindings": {}, "RestartPolicy": {}},
  "NetworkSettings": {"Networks": {"teploy": {"Aliases": ["myapp"]}}}
}]`, strings.Repeat("b", 64))

	t.Run("matches image", func(t *testing.T) {
		mock := ssh.NewMockExecutor("1.2.3.4",
			ssh.MockCommand{Match: "docker inspect 'c'", Output: inspect},
			ssh.MockCommand{Match: "docker image inspect", Output: `["docker-entrypoint.sh","serve"]`},
			ssh.MockCommand{Match: "docker rm -f", Output: ""},
			ssh.MockCommand{Match: "docker run", Output: ""},
		)
		if err := NewClient(mock).Restart(context.Background(), "c", nil); err != nil {
			t.Fatalf("Restart with image-own entrypoint: %v", err)
		}
		var run string
		for _, c := range mock.Calls {
			if strings.HasPrefix(c, "docker run ") {
				run = c
			}
		}
		if run == "" {
			t.Fatal("no docker run issued")
		}
		if strings.Contains(run, "--entrypoint") {
			t.Errorf("image-own entrypoint was rendered as an override: %s", run)
		}
	})

	t.Run("differs from image", func(t *testing.T) {
		mock := ssh.NewMockExecutor("1.2.3.4",
			ssh.MockCommand{Match: "docker inspect 'c'", Output: inspect},
			ssh.MockCommand{Match: "docker image inspect", Output: `["other-entrypoint"]`},
		)
		err := NewClient(mock).Restart(context.Background(), "c", nil)
		if err == nil || !strings.Contains(err.Error(), "multi-element entrypoint") {
			t.Fatalf("expected fail-closed multi-element entrypoint error, got %v", err)
		}
		for _, c := range mock.Calls {
			if strings.HasPrefix(c, "docker rm -f") {
				t.Fatalf("container was removed despite unrepresentable spec: %s", c)
			}
		}
	})
}

// The recreated container keeps the immutable image ID even when the tag has
// moved (F19's rule, retained through the RecreateSpec refactor).
func TestRecreate_PrefersImmutableImageID(t *testing.T) {
	inspect := fmt.Sprintf(`[{
  "Image": "sha256:%s",
  "Config": {"Image": "myapp:latest", "Labels": {}},
  "HostConfig": {"PortBindings": {}, "RestartPolicy": {}},
  "NetworkSettings": {"Networks": {}}
}]`, strings.Repeat("c", 64))
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "docker inspect 'c'", Output: inspect},
		ssh.MockCommand{Match: "docker rm -f", Output: ""},
		ssh.MockCommand{Match: "docker run", Output: ""},
	)
	if err := NewClient(mock).Restart(context.Background(), "c", nil); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	var run string
	for _, c := range mock.Calls {
		if strings.HasPrefix(c, "docker run ") {
			run = c
		}
	}
	if !strings.Contains(run, "'sha256:"+strings.Repeat("c", 64)+"'") {
		t.Errorf("recreate did not pin the immutable image ID: %s", run)
	}
	if strings.Contains(run, "myapp:latest") {
		t.Errorf("recreate used the mutable tag reference: %s", run)
	}
}

// HostPortFor resolves the host binding for a specific CONTAINER port — the
// primary-port lookup a fields[0] read cannot do for multi-port containers
// (TCL-14).
func TestHostPortFor_SelectsTheNamedContainerPort(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "docker inspect -f '{{json .NetworkSettings.Ports}}'",
			Output: `{"3000/tcp":[{"HostIp":"127.0.0.1","HostPort":"49152"}],"51820/udp":[{"HostIp":"0.0.0.0","HostPort":"51820"}]}`},
	)
	port, err := NewClient(mock).HostPortFor(context.Background(), "c", 3000)
	if err != nil {
		t.Fatalf("HostPortFor: %v", err)
	}
	if port != 49152 {
		t.Errorf("HostPortFor(3000) = %d, want 49152", port)
	}
	if _, err := NewClient(mock).HostPortFor(context.Background(), "c", 9999); err == nil {
		t.Error("expected an error for an unbound container port")
	}
}

// The avoidPorts collision reallocation (the --to rollback landmine) must
// survive the RecreateSpec refactor intact.
func TestRecreate_AvoidPortsReallocatesCollidingBinding(t *testing.T) {
	inspect := fmt.Sprintf(`[{
  "Image": "sha256:%s",
  "Config": {"Image": "myapp:latest", "Labels": {}},
  "HostConfig": {"PortBindings": {"3000/tcp":[{"HostIp":"127.0.0.1","HostPort":"49152"}]}, "RestartPolicy": {}},
  "NetworkSettings": {"Networks": {}}
}]`, strings.Repeat("d", 64))
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "docker inspect 'c'", Output: inspect},
		ssh.MockCommand{Match: "ss -tln", Output: "LISTEN 0 128 0.0.0.0:49152 0.0.0.0:*"},
		ssh.MockCommand{Match: "docker rm -f", Output: ""},
		ssh.MockCommand{Match: "docker run", Output: ""},
	)
	if err := NewClient(mock).Restart(context.Background(), "c", map[int]bool{49152: true}); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	var run string
	for _, c := range mock.Calls {
		if strings.HasPrefix(c, "docker run ") {
			run = c
		}
	}
	if strings.Contains(run, ":49152:") {
		t.Errorf("colliding port was not reallocated: %s", run)
	}
	if !strings.Contains(run, "-p '127.0.0.1:49153:3000/tcp'") {
		t.Errorf("expected reallocation to the next free port 49153: %s", run)
	}
}
