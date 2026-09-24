package deploy

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/ssh"
)

// rollbackMocks is TestRollback's server model with the web containers'
// docker ps Image and the state's previous_release parameterized.
func rollbackMocks(image, previousRelease string, extra ...ssh.MockCommand) *ssh.MockExecutor {
	stateContent := `{"schema_version":2,"deployment_type":"container","ingress_mode":"caddy","domain":"myapp.com","updated_at":"2026-07-22T10:00:00Z","image_ref":"myapp:v2","operation_id":"deploy-v2","generation":7,` + previousRelease + `"current_port":49153,"current_hash":"v2","previous_port":49152,"previous_hash":"v1"}`
	cmds := append(extra,
		ssh.MockCommand{Match: "if [ ! -e '/deployments/myapp/state.json' ]", Output: "present\n" + stateContent},
		ssh.MockCommand{Match: "mkdir -p /deployments/myapp", Output: ""},
		ssh.MockCommand{Match: "mkdir /deployments/myapp/.lock", Output: ""},
		ssh.MockCommand{Match: "cat /deployments/myapp/.lock/info", Err: fmt.Errorf("none")},
		ssh.MockCommand{Match: "docker ps --all --filter label=teploy.app='myapp'",
			Output: `{"ID":"aaa","Names":"myapp-web-v1","Image":"` + image + `","State":"exited","Status":"Exited","Labels":"teploy.app=myapp,teploy.version=v1,teploy.process=web"}` + "\n" +
				`{"ID":"bbb","Names":"myapp-web-v2","Image":"fedcba987654","State":"running","Status":"Up 1h","Labels":"teploy.app=myapp,teploy.version=v2,teploy.process=web"}`,
		},
		ssh.MockCommand{Match: "docker inspect 'myapp-web-v1'", Output: `[{"Image":"sha256:0123456789ab` + strings.Repeat("0", 52) + `","Config":{"Image":"sha256:0123456789ab` + strings.Repeat("0", 52) + `","Labels":{"teploy.app":"myapp"}},"HostConfig":{"NetworkMode":"teploy","PortBindings":{"3000/tcp":[{"HostIp":"127.0.0.1","HostPort":"49152"}]},"RestartPolicy":{"Name":"no"}},"NetworkSettings":{"Networks":{"teploy":{"Aliases":["myapp"]}}}}]`},
		ssh.MockCommand{Match: "docker rm -f 'myapp-web-v1'", Output: ""},
		ssh.MockCommand{Match: "docker run", Output: ""},
		ssh.MockCommand{Match: "curl", Output: "200"},
		ssh.MockCommand{Match: "docker inspect -f '{{range $p, $b := .NetworkSettings.Ports}}{{range $b}}{{.HostIp}}", Output: "127.0.0.1 "},
		ssh.MockCommand{Match: "docker inspect -f '{{range $p, $b := .NetworkSettings.Ports}}", Output: "49153"},
		ssh.MockCommand{Match: "docker inspect -f '{{range $p, $_ := .NetworkSettings.Ports}}", Output: "3000/tcp"},
		ssh.MockCommand{Match: "caddy", Output: ""},
		ssh.MockCommand{Match: "cat /deployments/caddy/Caddyfile", Output: "{\n\tadmin 0.0.0.0:2019\n}\n"},
		ssh.MockCommand{Match: "mv /tmp/teploy_caddyfile.tmp", Output: ""},
		ssh.MockCommand{Match: "mkdir /deployments/caddy/.lock", Output: ""},
		ssh.MockCommand{Match: "a=$(docker exec caddy md5sum", Output: "TEPLOY_CADDY_OK"},
		ssh.MockCommand{Match: "docker exec caddy caddy reload", Output: ""},
		ssh.MockCommand{Match: "rmdir /deployments/caddy/.lock", Output: ""},
		ssh.MockCommand{Match: "docker stop", Output: ""},
		ssh.MockCommand{Match: "mkdir -p", Output: ""},
		ssh.MockCommand{Match: "cat /tmp", Output: ""},
		ssh.MockCommand{Match: "UPLOAD:", Output: ""},
	)
	return ssh.NewMockExecutor("1.2.3.4", cmds...)
}

// Ship wave-9 lesson, rollback leg: web containers are created by image ID
// (A52), so docker ps reports a bare short ID. A rollback with no release
// record for the target recorded that ID as ImageRef — an unpullable
// 12-hex string. It must record the image's tag.
func TestRollback_ImageRefResolvesIDCreatedContainerToTag(t *testing.T) {
	full := "sha256:0123456789ab" + strings.Repeat("0", 52)
	mock := rollbackMocks("0123456789ab", "",
		ssh.MockCommand{Match: "docker image inspect --format", Output: full + ` ["myapp-build-v1:latest"]` + "\n"},
	)
	if err := Rollback(context.Background(), mock, &bytes.Buffer{}, rollbackCfg()); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if got := writtenState(t, mock, "myapp").ImageRef; got != "myapp-build-v1:latest" {
		t.Fatalf("ImageRef after rollback = %q, want the tag myapp-build-v1:latest", got)
	}
}

// The target's release record, when present, is the requested reference
// and wins over whatever the container reports.
func TestRollback_ImageRefPrefersReleaseRecord(t *testing.T) {
	mock := rollbackMocks("myapp:latest", `"previous_release":{"hash":"v1","image_ref":"myapp:v1"},`)
	if err := Rollback(context.Background(), mock, &bytes.Buffer{}, rollbackCfg()); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if got := writtenState(t, mock, "myapp").ImageRef; got != "myapp:v1" {
		t.Fatalf("ImageRef after rollback = %q, want the release record's myapp:v1", got)
	}
}
