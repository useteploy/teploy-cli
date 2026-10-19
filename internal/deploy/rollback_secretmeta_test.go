package deploy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/ssh"
)

// Rollback never needs a secret value from release metadata: the target
// release's record holds only key names (env_redacted), and the container
// is recreated from docker's own record of it, secrets included, published
// via a private env file. The record stays value-free afterwards.
func TestRollback_WorksWithRedactedReleaseRecord(t *testing.T) {
	currentManifest := json.RawMessage(`{"release":"v2"}`)
	previousManifest := json.RawMessage(`{"release":"v1"}`)
	stateContent := fmt.Sprintf(`{"schema_version":2,"deployment_type":"container","ingress_mode":"caddy","domain":"myapp.com","updated_at":"2026-07-22T10:00:00Z","manifest_sha256":"%x","source_revision":"rev-v2","image_ref":"myapp:v2","operation_id":"deploy-v2","generation":7,"applied_manifest":%s,"previous_release":{"hash":"v1","manifest_sha256":"%x","source_revision":"rev-v1","image_ref":"myapp:v1","applied_manifest":%s},"current_port":49153,"current_hash":"v2","previous_port":49152,"previous_hash":"v1"}`,
		sha256.Sum256(currentManifest), currentManifest, sha256.Sum256(previousManifest), previousManifest)
	redacted := `{"schema_version":1,"app":"myapp","hash":"v1","deployment_type":"container","ingress_mode":"caddy","domain":"myapp.com",` +
		`"recreate":{"name":"myapp-web-v1","env":["PORT=3000"],"env_redacted":[{"key":"DB_PASSWORD","source":"secret_store"}]},` +
		`"env_redacted":[{"key":"DB_PASSWORD","source":"secret_store"}]}`
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "if [ ! -e '/deployments/myapp/state.json' ]", Output: "present\n" + stateContent},
		ssh.MockCommand{Match: "mkdir -p /deployments/myapp", Output: ""},
		ssh.MockCommand{Match: "mkdir /deployments/myapp/.lock", Output: ""},
		ssh.MockCommand{Match: "cat /deployments/myapp/.lock/info", Err: fmt.Errorf("none")},
		ssh.MockCommand{Match: "docker ps --all --filter label=teploy.app='myapp'",
			Output: `{"ID":"aaa","Names":"myapp-web-v1","Image":"myapp:latest","State":"exited","Status":"Exited","Labels":"teploy.app=myapp,teploy.version=v1,teploy.process=web"}` + "\n" +
				`{"ID":"bbb","Names":"myapp-web-v2","Image":"myapp:latest","State":"running","Status":"Up 1h","Labels":"teploy.app=myapp,teploy.version=v2,teploy.process=web"}`,
		},
		ssh.MockCommand{Match: "docker inspect 'myapp-web-v1'", Output: `[{"Config":{"Image":"myapp:latest","Env":["PORT=3000","DB_PASSWORD=live-secret"],"Labels":{"teploy.app":"myapp"}},"HostConfig":{"NetworkMode":"teploy","PortBindings":{"3000/tcp":[{"HostIp":"127.0.0.1","HostPort":"49152"}]},"RestartPolicy":{"Name":"no"}},"NetworkSettings":{"Networks":{"teploy":{"Aliases":["myapp"]}}}}]`},
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
	mock.Files["/deployments/myapp/meta/v1.json"] = []byte(redacted)

	var buf bytes.Buffer
	if err := Rollback(context.Background(), mock, &buf, rollbackCfg()); err != nil {
		t.Fatalf("rollback with a redacted record failed: %v\n%s", err, buf.String())
	}
	if !strings.Contains(buf.String(), "Rolled back myapp to version v1") {
		t.Fatalf("rollback did not complete: %s", buf.String())
	}
	// The secret reached the recreated container through a private env
	// file built from docker's live record — never from metadata.
	// (The staged file is removed after docker run, so assert the upload and
	// its use rather than its lingering content.)
	uploaded, used := false, false
	for _, c := range mock.Calls {
		if strings.HasPrefix(c, "UPLOAD:/tmp/.teploy-recreate-env-") {
			uploaded = true
		}
		if strings.HasPrefix(c, "docker run") && strings.Contains(c, "myapp-web-v1") && strings.Contains(c, "--env-file '/tmp/.teploy-recreate-env-") {
			used = true
		}
	}
	if !uploaded || !used {
		t.Errorf("recreate must publish docker's live env via a private env file (uploaded=%v used=%v)", uploaded, used)
	}
	for _, c := range mock.Calls {
		if strings.Contains(c, "live-secret") {
			t.Error("secret value appeared in a command line")
		}
	}
	if strings.Contains(string(mock.Files["/deployments/myapp/meta/v1.json"]), "live-secret") {
		t.Error("rollback wrote a secret value into release metadata")
	}
	if strings.Contains(buf.String(), "live-secret") {
		t.Error("rollback output contains a secret value")
	}
}
