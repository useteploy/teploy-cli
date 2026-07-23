package deploy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy/internal/ssh"
)

func rollbackCfg() RollbackConfig {
	return RollbackConfig{
		App:    "myapp",
		Domain: "myapp.com",
		Health: HealthConfig{Timeout: 5 * time.Second, Interval: 10 * time.Millisecond},
	}
}

func TestRollback(t *testing.T) {
	currentManifest := json.RawMessage(`{"release":"v2"}`)
	previousManifest := json.RawMessage(`{"release":"v1"}`)
	stateContent := fmt.Sprintf(`{"schema_version":2,"deployment_type":"container","ingress_mode":"caddy","domain":"myapp.com","updated_at":"2026-07-22T10:00:00Z","manifest_sha256":"%x","source_revision":"rev-v2","image_ref":"myapp:v2","operation_id":"deploy-v2","generation":7,"applied_manifest":%s,"previous_release":{"hash":"v1","manifest_sha256":"%x","source_revision":"rev-v1","image_ref":"myapp:v1","applied_manifest":%s},"current_port":49153,"current_hash":"v2","previous_port":49152,"previous_hash":"v1"}`,
		sha256.Sum256(currentManifest), currentManifest, sha256.Sum256(previousManifest), previousManifest)
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "cat -- /deployments/myapp/state.json", Output: stateContent},
		ssh.MockCommand{Match: "docker ps --all --filter label=teploy.app='myapp'",
			Output: `{"ID":"aaa","Names":"myapp-web-v1","Image":"myapp:latest","State":"exited","Status":"Exited","Labels":"teploy.app=myapp,teploy.version=v1,teploy.process=web"}` + "\n" +
				`{"ID":"bbb","Names":"myapp-web-v2","Image":"myapp:latest","State":"running","Status":"Up 1h","Labels":"teploy.app=myapp,teploy.version=v2,teploy.process=web"}`,
		},
		// rollback uses Restart (inspect → rm -f → docker run) instead of bare
		// `docker start` so HostConfig.PortBindings actually re-apply on Docker 29.
		ssh.MockCommand{Match: "docker inspect 'myapp-web-v1'", Output: `[{"Config":{"Image":"myapp:latest","Labels":{"teploy.app":"myapp"}},"HostConfig":{"NetworkMode":"teploy","PortBindings":{"3000/tcp":[{"HostIp":"127.0.0.1","HostPort":"49152"}]},"RestartPolicy":{"Name":"no"}},"NetworkSettings":{"Networks":{"teploy":{"Aliases":["myapp"]}}}}]`},
		ssh.MockCommand{Match: "docker rm -f 'myapp-web-v1'", Output: ""},
		ssh.MockCommand{Match: "docker run", Output: ""},
		ssh.MockCommand{Match: "curl", Output: "200"},
		// Target container's host port (health check) resolved via docker inspect -f.
		ssh.MockCommand{Match: "docker inspect -f '{{range $p, $b := .NetworkSettings.Ports}}", Output: "49153"},
		// Target container's internal port (Caddy upstream) resolved via docker inspect -f.
		ssh.MockCommand{Match: "docker inspect -f '{{range $p, $_ := .NetworkSettings.Ports}}", Output: "3000/tcp"},
		ssh.MockCommand{Match: "caddy", Output: ""},
		ssh.MockCommand{Match: "cat /deployments/caddy/Caddyfile", Output: "{\n\tadmin 0.0.0.0:2019\n}\n"},
		ssh.MockCommand{Match: "mv /tmp/teploy_caddyfile.tmp", Output: ""},
		ssh.MockCommand{Match: "mkdir /deployments/caddy/.lock", Output: ""},
		ssh.MockCommand{Match: "docker exec caddy caddy reload", Output: ""},
		ssh.MockCommand{Match: "rmdir /deployments/caddy/.lock", Output: ""},
		ssh.MockCommand{Match: "docker stop", Output: ""},
		ssh.MockCommand{Match: "mkdir -p", Output: ""},
		ssh.MockCommand{Match: "cat /tmp", Output: ""},
		ssh.MockCommand{Match: "UPLOAD:", Output: ""},
	)

	var buf bytes.Buffer
	err := Rollback(context.Background(), mock, &buf, rollbackCfg())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "Rolling back") {
		t.Error("expected 'Rolling back' message")
	}
	if !strings.Contains(output, "Rolled back myapp to version v1") {
		t.Errorf("expected rollback success message, got: %s", output)
	}

	// Verify previous container was recreated (Restart = inspect → rm -f → run).
	var inspected, removed, ran bool
	for _, call := range mock.Calls {
		switch {
		case strings.HasPrefix(call, "docker inspect 'myapp-web-v1'"):
			inspected = true
		case strings.HasPrefix(call, "docker rm -f 'myapp-web-v1'"):
			removed = true
		case strings.HasPrefix(call, "docker run") && strings.Contains(call, "myapp-web-v1"):
			ran = true
		}
	}
	if !inspected || !removed || !ran {
		t.Errorf("expected Restart sequence (inspect+rm+run) for v1; inspected=%v removed=%v ran=%v",
			inspected, removed, ran)
	}

	// Verify Caddy was pointed at the container's *internal* port, not
	// the host-mapped PreviousPort (49152). The container exposes 3000.
	var caddyfile []byte
	for path, data := range mock.Files {
		if strings.HasSuffix(path, "teploy_caddyfile.tmp") {
			caddyfile = data
		}
	}
	if caddyfile == nil {
		t.Fatal("expected Caddyfile to be written")
	}
	if !strings.Contains(string(caddyfile), "myapp-web-v1:3000") {
		t.Errorf("rollback route should dial container's internal port (3000), got: %s", string(caddyfile))
	}
	if strings.Contains(string(caddyfile), ":49152") {
		t.Errorf("rollback route should not use host port 49152, got: %s", string(caddyfile))
	}

	// Verify current container was stopped.
	stopCalls := 0
	for _, call := range mock.Calls {
		if strings.HasPrefix(call, "docker stop") {
			stopCalls++
			if !strings.Contains(call, "myapp-web-v2") {
				t.Errorf("expected stop for v2 container, got: %s", call)
			}
		}
	}
	if stopCalls != 1 {
		t.Errorf("expected 1 stop call, got %d", stopCalls)
	}

	// Verify state was swapped.
	stateData := writtenState(t, mock, "myapp")
	if stateData.CurrentHash != "v1" || stateData.PreviousHash != "v2" || stateData.DeploymentType != "container" || stateData.IngressMode != "caddy" || stateData.Generation != 8 || stateData.OperationID == "" {
		t.Errorf("unexpected rollback state: %+v", stateData)
	}
	if stateData.SourceRevision != "rev-v1" || !bytes.Equal(stateData.AppliedManifest, previousManifest) || stateData.PreviousRelease == nil || stateData.PreviousRelease.Hash != "v2" || !bytes.Equal(stateData.PreviousRelease.AppliedManifest, currentManifest) {
		t.Errorf("rollback did not swap release metadata: %+v", stateData)
	}
}

func TestRollback_StateCommitFailureRestoresOriginalRouteAndWorkload(t *testing.T) {
	stateContent := "current_port=49153\ncurrent_hash=v2\nprevious_port=49152\nprevious_hash=v1\ndomain=myapp.com\n"
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "cat /deployments/myapp/state", Output: stateContent},
		ssh.MockCommand{Match: "docker ps --all --filter label=teploy.app='myapp'",
			Output: `{"ID":"aaa","Names":"myapp-web-v1","Image":"myapp:latest","State":"exited","Status":"Exited","Labels":"teploy.app=myapp,teploy.version=v1,teploy.process=web"}` + "\n" +
				`{"ID":"bbb","Names":"myapp-web-v2","Image":"myapp:latest","State":"running","Status":"Up 1h","Labels":"teploy.app=myapp,teploy.version=v2,teploy.process=web"}`,
		},
		ssh.MockCommand{Match: "docker inspect 'myapp-web-v1'", Output: `[{"Config":{"Image":"myapp:latest","Labels":{"teploy.app":"myapp"}},"HostConfig":{"NetworkMode":"teploy","PortBindings":{"3000/tcp":[{"HostIp":"127.0.0.1","HostPort":"49152"}]},"RestartPolicy":{"Name":"no"}},"NetworkSettings":{"Networks":{"teploy":{"Aliases":["myapp"]}}}}]`},
		ssh.MockCommand{Match: "docker rm -f 'myapp-web-v1'", Output: ""},
		ssh.MockCommand{Match: "docker run", Output: ""},
		ssh.MockCommand{Match: "docker inspect -f '{{range $p, $b := .NetworkSettings.Ports}}", Output: "49152"},
		ssh.MockCommand{Match: "docker inspect -f '{{range $p, $_ := .NetworkSettings.Ports}}", Output: "3000/tcp"},
		ssh.MockCommand{Match: "curl", Output: "200"},
		ssh.MockCommand{Match: "cat /deployments/caddy/Caddyfile", Output: "{\n\tadmin 0.0.0.0:2019\n}\n"},
		ssh.MockCommand{Match: "mv /tmp/teploy_caddyfile.tmp", Output: ""},
		ssh.MockCommand{Match: "mkdir /deployments/caddy/.lock", Output: ""},
		ssh.MockCommand{Match: "docker exec caddy caddy reload", Output: ""},
		ssh.MockCommand{Match: "rmdir /deployments/caddy/.lock", Output: ""},
		ssh.MockCommand{Match: "UPLOAD:/deployments/myapp/state.json.tmp-", Err: fmt.Errorf("disk full")},
		ssh.MockCommand{Match: "docker stop", Output: ""},
	)
	var out bytes.Buffer

	err := Rollback(context.Background(), mock, &out, rollbackCfg())
	if err == nil || !strings.Contains(err.Error(), "original route was restored") {
		t.Fatalf("expected fail-closed rollback state error, got %v", err)
	}
	if strings.Contains(out.String(), "Rolled back") {
		t.Fatalf("failed rollback reported success: %s", out.String())
	}
	for _, call := range mock.Calls {
		if strings.HasPrefix(call, "docker stop") && strings.Contains(call, "myapp-web-v2") {
			t.Fatalf("original workload was stopped after failed state commit: %s", call)
		}
	}
	restoredCaddyfile := string(mock.Files["/tmp/teploy_caddyfile.tmp"])
	if !strings.Contains(restoredCaddyfile, "myapp-web-v2:3000") {
		t.Fatalf("original route was not restored: %s", restoredCaddyfile)
	}
}

func TestRollback_NoState(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "cat /deployments/myapp/state", Err: fmt.Errorf("not found")},
	)

	err := Rollback(context.Background(), mock, &bytes.Buffer{}, rollbackCfg())
	if err == nil {
		t.Fatal("expected error for no state")
	}
	if !strings.Contains(err.Error(), "deploy first") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestRollback_NoPreviousDeploy(t *testing.T) {
	stateContent := "current_port=49152\ncurrent_hash=v1\n"
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "cat /deployments/myapp/state", Output: stateContent},
	)

	err := Rollback(context.Background(), mock, &bytes.Buffer{}, rollbackCfg())
	if err == nil {
		t.Fatal("expected error for no previous deploy")
	}
	if !strings.Contains(err.Error(), "no previous deploy") {
		t.Errorf("unexpected error: %v", err)
	}
	// Multi-server rollback-all (internal/cli/deploy.go's runMultiDeploy)
	// distinguishes "first deploy, nothing to roll back" from a genuine
	// rollback failure via this sentinel — it must actually be this error,
	// not just one with similar text.
	if !errors.Is(err, ErrNoPreviousDeploy) {
		t.Errorf("expected errors.Is(err, ErrNoPreviousDeploy), got: %v", err)
	}
}

func TestRollback_NoPreviousContainers(t *testing.T) {
	stateContent := "current_port=49153\ncurrent_hash=v2\nprevious_port=49152\nprevious_hash=v1\n"
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "cat /deployments/myapp/state", Output: stateContent},
		ssh.MockCommand{Match: "docker ps --all --filter label=teploy.app='myapp'",
			Output: `{"ID":"bbb","Names":"myapp-web-v2","Image":"myapp:latest","State":"running","Status":"Up 1h","Labels":"teploy.app=myapp,teploy.version=v2,teploy.process=web"}`,
		},
	)

	err := Rollback(context.Background(), mock, &bytes.Buffer{}, rollbackCfg())
	if err == nil {
		t.Fatal("expected error for missing previous containers")
	}
	if !strings.Contains(err.Error(), "no containers found for version") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestRollback_HealthCheckFails(t *testing.T) {
	stateContent := "current_port=49153\ncurrent_hash=v2\nprevious_port=49152\nprevious_hash=v1\n"
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "cat /deployments/myapp/state", Output: stateContent},
		ssh.MockCommand{Match: "docker ps --all --filter label=teploy.app='myapp'",
			Output: `{"ID":"aaa","Names":"myapp-web-v1","Image":"myapp:latest","State":"exited","Status":"Exited","Labels":"teploy.app=myapp,teploy.version=v1,teploy.process=web"}` + "\n" +
				`{"ID":"bbb","Names":"myapp-web-v2","Image":"myapp:latest","State":"running","Status":"Up 1h","Labels":"teploy.app=myapp,teploy.version=v2,teploy.process=web"}`,
		},
		// Restart (inspect → rm -f → docker run).
		ssh.MockCommand{Match: "docker inspect 'myapp-web-v1'", Output: `[{"Config":{"Image":"myapp:latest","Labels":{"teploy.app":"myapp"}},"HostConfig":{"NetworkMode":"teploy","PortBindings":{"3000/tcp":[{"HostIp":"127.0.0.1","HostPort":"49152"}]},"RestartPolicy":{"Name":"no"}},"NetworkSettings":{"Networks":{"teploy":{"Aliases":["myapp"]}}}}]`},
		ssh.MockCommand{Match: "docker rm -f 'myapp-web-v1'", Output: ""},
		ssh.MockCommand{Match: "docker run", Output: ""},
		ssh.MockCommand{Match: "docker inspect -f '{{range $p, $b := .NetworkSettings.Ports}}", Output: "49153"},
		ssh.MockCommand{Match: "curl", Err: fmt.Errorf("connection refused")},
		ssh.MockCommand{Match: "bash -c", Err: fmt.Errorf("connection refused")},
		ssh.MockCommand{Match: "docker stop", Output: ""},
	)

	cfg := rollbackCfg()
	cfg.Health = HealthConfig{Timeout: 100 * time.Millisecond, Interval: 10 * time.Millisecond}
	err := Rollback(context.Background(), mock, &bytes.Buffer{}, cfg)
	if err == nil {
		t.Fatal("expected error for health check failure")
	}
	if !strings.Contains(err.Error(), "health check failed") {
		t.Errorf("unexpected error: %v", err)
	}
}

// Rolling back TO a multi-replica previous version must start every previous
// replica, health-check each, and restore a LOAD-BALANCED route across them —
// not abort on a reconstructed single name or route to just one replica.
func TestRollback_MultiReplica(t *testing.T) {
	stateContent := "current_port=49153\ncurrent_hash=v2\nprevious_port=49152\nprevious_hash=v1\n" +
		"current_ports=49153,49155\nprevious_ports=49152,49154\n"
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "cat /deployments/myapp/state", Output: stateContent},
		ssh.MockCommand{Match: "docker ps --all --filter label=teploy.app='myapp'",
			Output: `{"ID":"a1","Names":"myapp-web-v1-1","Image":"myapp:latest","State":"exited","Status":"Exited","Labels":"teploy.app=myapp,teploy.version=v1,teploy.process=web"}` + "\n" +
				`{"ID":"a2","Names":"myapp-web-v1-2","Image":"myapp:latest","State":"exited","Status":"Exited","Labels":"teploy.app=myapp,teploy.version=v1,teploy.process=web"}` + "\n" +
				`{"ID":"b1","Names":"myapp-web-v2-1","Image":"myapp:latest","State":"running","Status":"Up 1h","Labels":"teploy.app=myapp,teploy.version=v2,teploy.process=web"}` + "\n" +
				`{"ID":"b2","Names":"myapp-web-v2-2","Image":"myapp:latest","State":"running","Status":"Up 1h","Labels":"teploy.app=myapp,teploy.version=v2,teploy.process=web"}`,
		},
		// Restart (inspect → rm -f → run) for each previous replica; prefix
		// "docker inspect 'myapp-web-v1" (open quote, no close) covers -1 and -2.
		ssh.MockCommand{Match: "docker inspect 'myapp-web-v1", Output: `[{"Config":{"Image":"myapp:latest","Labels":{"teploy.app":"myapp"}},"HostConfig":{"NetworkMode":"teploy","PortBindings":{"3000/tcp":[{"HostIp":"127.0.0.1","HostPort":"49152"}]},"RestartPolicy":{"Name":"no"}},"NetworkSettings":{"Networks":{"teploy":{"Aliases":["myapp"]}}}}]`},
		ssh.MockCommand{Match: "docker rm -f 'myapp-web-v1", Output: ""},
		ssh.MockCommand{Match: "docker run", Output: ""},
		ssh.MockCommand{Match: "docker inspect -f '{{range $p, $b := .NetworkSettings.Ports}}", Output: "49153"},
		ssh.MockCommand{Match: "curl", Output: "200"},
		ssh.MockCommand{Match: "docker inspect -f '{{range $p, $_ := .NetworkSettings.Ports}}", Output: "3000/tcp"},
		ssh.MockCommand{Match: "caddy", Output: ""},
		ssh.MockCommand{Match: "cat /deployments/caddy/Caddyfile", Output: "{\n\tadmin 0.0.0.0:2019\n}\n"},
		ssh.MockCommand{Match: "mv /tmp/teploy_caddyfile.tmp", Output: ""},
		ssh.MockCommand{Match: "mkdir /deployments/caddy/.lock", Output: ""},
		ssh.MockCommand{Match: "docker exec caddy caddy reload", Output: ""},
		ssh.MockCommand{Match: "rmdir /deployments/caddy/.lock", Output: ""},
		ssh.MockCommand{Match: "docker stop", Output: ""},
		ssh.MockCommand{Match: "printf %s", Output: ""},
		ssh.MockCommand{Match: "mkdir -p", Output: ""},
		ssh.MockCommand{Match: "UPLOAD:", Output: ""},
	)

	var buf bytes.Buffer
	if err := Rollback(context.Background(), mock, &buf, rollbackCfg()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "load-balanced across 2 replicas") {
		t.Errorf("expected load-balanced rollback route across 2 replicas, got:\n%s", out)
	}
	// Both previous replicas must be recreated (not just one).
	for _, name := range []string{"myapp-web-v1-1", "myapp-web-v1-2"} {
		found := false
		for _, c := range mock.Calls {
			if strings.HasPrefix(c, "docker rm -f "+ssh.ShellQuote(name)) {
				found = true
			}
		}
		if !found {
			t.Errorf("expected previous replica %s to be recreated", name)
		}
	}
	// The Caddyfile written must reference both upstreams.
	cf := string(mock.Files["/tmp/teploy_caddyfile.tmp"])
	if !strings.Contains(cf, "myapp-web-v1-1:3000") || !strings.Contains(cf, "myapp-web-v1-2:3000") {
		t.Errorf("expected both replica upstreams in Caddyfile, got:\n%s", cf)
	}
}

// TestRollback_ToSpecificHash is the direct regression test for Phase 8:
// container rollback previously could only ever go back exactly one
// version (state.AppState.PreviousHash), unlike type:static's --to. This
// proves rolling back to v1 works even though the immediately previous
// version is v2 — v1's containers are found by inspecting the live
// container set (teploy.version label + HostPort/InternalPort), not by
// any depth-tracking added to state.go (there isn't any — this mirrors
// type:static's existing simple 1-level-swap state semantics exactly).
func TestRollback_ToSpecificHash(t *testing.T) {
	stateContent := "current_port=49154\ncurrent_hash=v3\nprevious_port=49153\nprevious_hash=v2\n"
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "cat /deployments/myapp/state", Output: stateContent},
		ssh.MockCommand{Match: "docker ps --all --filter label=teploy.app='myapp'",
			Output: `{"ID":"aaa","Names":"myapp-web-v1","Image":"myapp:latest","State":"exited","Status":"Exited","Labels":"teploy.app=myapp,teploy.version=v1,teploy.process=web"}` + "\n" +
				`{"ID":"bbb","Names":"myapp-web-v2","Image":"myapp:latest","State":"exited","Status":"Exited","Labels":"teploy.app=myapp,teploy.version=v2,teploy.process=web"}` + "\n" +
				`{"ID":"ccc","Names":"myapp-web-v3","Image":"myapp:latest","State":"running","Status":"Up 1h","Labels":"teploy.app=myapp,teploy.version=v3,teploy.process=web"}`,
		},
		ssh.MockCommand{Match: "docker inspect 'myapp-web-v1'", Output: `[{"Config":{"Image":"myapp:latest","Labels":{"teploy.app":"myapp"}},"HostConfig":{"NetworkMode":"teploy","PortBindings":{"3000/tcp":[{"HostIp":"127.0.0.1","HostPort":"49152"}]},"RestartPolicy":{"Name":"no"}},"NetworkSettings":{"Networks":{"teploy":{"Aliases":["myapp"]}}}}]`},
		ssh.MockCommand{Match: "docker rm -f 'myapp-web-v1'", Output: ""},
		ssh.MockCommand{Match: "docker run", Output: ""},
		ssh.MockCommand{Match: "docker inspect -f '{{range $p, $b := .NetworkSettings.Ports}}", Output: "49152"},
		ssh.MockCommand{Match: "curl", Output: "200"},
		ssh.MockCommand{Match: "docker inspect -f '{{range $p, $_ := .NetworkSettings.Ports}}", Output: "3000/tcp"},
		ssh.MockCommand{Match: "caddy", Output: ""},
		ssh.MockCommand{Match: "cat /deployments/caddy/Caddyfile", Output: "{\n\tadmin 0.0.0.0:2019\n}\n"},
		ssh.MockCommand{Match: "mv /tmp/teploy_caddyfile.tmp", Output: ""},
		ssh.MockCommand{Match: "mkdir /deployments/caddy/.lock", Output: ""},
		ssh.MockCommand{Match: "docker exec caddy caddy reload", Output: ""},
		ssh.MockCommand{Match: "rmdir /deployments/caddy/.lock", Output: ""},
		ssh.MockCommand{Match: "docker stop", Output: ""},
		ssh.MockCommand{Match: "mkdir -p", Output: ""},
		ssh.MockCommand{Match: "cat /tmp", Output: ""},
		ssh.MockCommand{Match: "UPLOAD:", Output: ""},
	)

	cfg := rollbackCfg()
	cfg.ToHash = "v1"
	var buf bytes.Buffer
	if err := Rollback(context.Background(), mock, &buf, cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(buf.String(), "Rolled back myapp to version v1") {
		t.Errorf("expected rollback to v1, got: %s", buf.String())
	}

	// v3 (current) must be stopped; v2 (the merely-previous version, not
	// the target) must be left completely untouched.
	var stoppedV3, touchedV2 bool
	for _, c := range mock.Calls {
		if strings.HasPrefix(c, "docker stop") && strings.Contains(c, "myapp-web-v3") {
			stoppedV3 = true
		}
		if strings.Contains(c, "myapp-web-v2") {
			touchedV2 = true
		}
	}
	if !stoppedV3 {
		t.Error("expected current version (v3) to be stopped")
	}
	if touchedV2 {
		t.Error("v2 (previous, but not the --to target) should not be touched at all")
	}

	// State: v1 becomes current, v3 (what we just rolled back from) becomes
	// previous — a plain 1-level swap, not a deeper history stack.
	stateData := writtenState(t, mock, "myapp")
	if stateData.CurrentHash != "v1" || stateData.PreviousHash != "v3" {
		t.Errorf("unexpected --to rollback state: %+v", stateData)
	}
}

func TestRollback_ToHash_AlreadyCurrent(t *testing.T) {
	stateContent := "current_port=49153\ncurrent_hash=v2\nprevious_port=49152\nprevious_hash=v1\n"
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "cat /deployments/myapp/state", Output: stateContent},
	)

	cfg := rollbackCfg()
	cfg.ToHash = "v2" // same as current_hash
	err := Rollback(context.Background(), mock, &bytes.Buffer{}, cfg)
	if err == nil {
		t.Fatal("expected error when --to targets the already-current version")
	}
	if !strings.Contains(err.Error(), "already current") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestRollback_ToHash_PortCollisionReallocates reproduces a real failure
// found live-testing --to against a throwaway server: v1 originally ran on
// 49152; after v1 stopped, a later deploy (v3) reused 49152 (Docker's
// ephemeral allocator hands out freed ports); `rollback --to v1` then tried
// to recreate v1 on its original port 49152 while v3 was still running
// there (start-before-stop for zero-downtime) and docker run failed with
// "port is already allocated". Restart must detect the collision against
// the live current container and allocate a fresh port instead.
func TestRollback_ToHash_PortCollisionReallocates(t *testing.T) {
	stateContent := "current_port=49152\ncurrent_hash=v3\nprevious_port=49153\nprevious_hash=v2\n"
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "cat /deployments/myapp/state", Output: stateContent},
		ssh.MockCommand{Match: "docker ps --all --filter label=teploy.app='myapp'",
			Output: `{"ID":"aaa","Names":"myapp-web-v1","Image":"myapp:latest","State":"exited","Status":"Exited","Labels":"teploy.app=myapp,teploy.version=v1,teploy.process=web"}` + "\n" +
				`{"ID":"ccc","Names":"myapp-web-v3","Image":"myapp:latest","State":"running","Status":"Up 1h","Labels":"teploy.app=myapp,teploy.version=v3,teploy.process=web"}`,
		},
		// v1's original binding is 49152 — exactly what v3 (current, still
		// running) holds right now.
		ssh.MockCommand{Match: "docker inspect 'myapp-web-v1'", Output: `[{"Config":{"Image":"myapp:latest","Labels":{"teploy.app":"myapp"}},"HostConfig":{"NetworkMode":"teploy","PortBindings":{"3000/tcp":[{"HostIp":"127.0.0.1","HostPort":"49152"}]},"RestartPolicy":{"Name":"no"}},"NetworkSettings":{"Networks":{"teploy":{"Aliases":["myapp"]}}}}]`},
		ssh.MockCommand{Match: "docker rm -f 'myapp-web-v1'", Output: ""},
		ssh.MockCommand{Match: "ss -tln", Output: "State  Recv-Q  Send-Q  Local Address:Port  Peer Address:Port\nLISTEN 0 128 0.0.0.0:49152 0.0.0.0:*"},
		ssh.MockCommand{Match: "docker run", Output: ""},
		ssh.MockCommand{Match: "docker inspect -f '{{range $p, $b := .NetworkSettings.Ports}}", Output: "49153"},
		ssh.MockCommand{Match: "curl", Output: "200"},
		ssh.MockCommand{Match: "docker inspect -f '{{range $p, $_ := .NetworkSettings.Ports}}", Output: "3000/tcp"},
		ssh.MockCommand{Match: "caddy", Output: ""},
		ssh.MockCommand{Match: "cat /deployments/caddy/Caddyfile", Output: "{\n\tadmin 0.0.0.0:2019\n}\n"},
		ssh.MockCommand{Match: "mv /tmp/teploy_caddyfile.tmp", Output: ""},
		ssh.MockCommand{Match: "mkdir /deployments/caddy/.lock", Output: ""},
		ssh.MockCommand{Match: "docker exec caddy caddy reload", Output: ""},
		ssh.MockCommand{Match: "rmdir /deployments/caddy/.lock", Output: ""},
		ssh.MockCommand{Match: "docker stop", Output: ""},
		ssh.MockCommand{Match: "mkdir -p", Output: ""},
		ssh.MockCommand{Match: "cat /tmp", Output: ""},
		ssh.MockCommand{Match: "UPLOAD:", Output: ""},
	)

	cfg := rollbackCfg()
	cfg.ToHash = "v1"
	var buf bytes.Buffer
	if err := Rollback(context.Background(), mock, &buf, cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The recreated container must NOT have been asked to bind 49152 (still
	// held by v3 at the time Restart runs) — it must get a different port.
	for _, c := range mock.Calls {
		if strings.HasPrefix(c, "docker run") && strings.Contains(c, ":49152:") {
			t.Errorf("recreated container was bound to the colliding port 49152: %s", c)
		}
	}

	stateData := writtenState(t, mock, "myapp")
	if stateData.CurrentHash != "v1" {
		t.Errorf("expected current_hash=v1, got: %+v", stateData)
	}
	// Port written to state must be the freshly allocated one (from
	// HostPort() inspection), never the stale, now-colliding 49152.
	if stateData.CurrentPort == 49152 {
		t.Errorf("state still recorded the colliding port: %+v", stateData)
	}
}
