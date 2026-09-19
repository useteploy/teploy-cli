package deploy

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy/internal/ssh"
)

// TestConfigValidate_IdentityGrammar is the A17 regression: the shared
// execution-plan validator rejects shell/path-hostile identities, unknown
// ingress modes, and the publish+replicas fixed-port conflict before any
// command is issued.
func TestConfigValidate_IdentityGrammar(t *testing.T) {
	base := func() Config {
		return Config{App: "myapp", Domain: "myapp.com", Image: "i:latest", Version: "abc123"}
	}
	cases := []struct {
		name string
		mut  func(*Config)
		want string
	}{
		{"app path metacharacters", func(c *Config) { c.App = "../escape" }, "app"},
		{"app uppercase", func(c *Config) { c.App = "MyApp" }, "app"},
		{"version traversal", func(c *Config) { c.Version = "../../etc" }, "version"},
		{"process name metacharacters", func(c *Config) { c.Processes = map[string]string{"w;rm": "x"} }, "process"},
		{"unknown ingress", func(c *Config) { c.Ingress = "carrier-pigeon" }, "ingress"},
		{"publish with replicas", func(c *Config) {
			c.Publish = []string{"0.0.0.0:3001:3001"}
			c.Replicas = 2
		}, "single replica"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base()
			tc.mut(&cfg)
			err := cfg.validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected rejection mentioning %q, got %v", tc.want, err)
			}
		})
	}
	// The accepted shapes still pass.
	for _, ok := range []Config{
		{App: "my-app2", Domain: "d.com", Image: "i", Version: "v1.2.3"},
		{App: "a", Image: "i", Version: "sha256-abcdef", Ingress: "host"},
		{App: "a", Domain: "d.com", Image: "i", Version: "v", Ingress: "external"},
		{App: "a", Domain: "d.com", Image: "i", Version: "v", Publish: []string{"0.0.0.0:3001:3001"}, Replicas: 1},
	} {
		if err := ok.validate(); err != nil {
			t.Errorf("valid config rejected: %v (%+v)", err, ok)
		}
	}
}

// TestDeploy_NormalizesDefaultContainerPort is the A18 regression: a
// Config built with ContainerPort == 0 must deploy with 80 everywhere —
// the Caddy upstream dial must never read "container:0".
func TestDeploy_NormalizesDefaultContainerPort(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "mkdir -p /deployments/myapp", Output: ""},
		ssh.MockCommand{Match: "mkdir /deployments/myapp/.lock", Output: ""},
		ssh.MockCommand{Match: "if [ ! -e '/deployments/myapp/state.json' ]", Output: "absent"},
		ssh.MockCommand{Match: "if [ ! -e '/deployments/myapp/state' ]", Output: "absent"},
		ssh.MockCommand{Match: "ss -tln", Output: ssOutput},
		ssh.MockCommand{Match: "docker run", Output: "abc123"},
		ssh.MockCommand{Match: "docker inspect", Output: "running"},
		ssh.MockCommand{Match: "curl -s -o /dev/null", Output: "200"},
		ssh.MockCommand{Match: "cat /deployments/caddy/Caddyfile", Output: "{\n\tadmin 0.0.0.0:2019\n}\n"},
		ssh.MockCommand{Match: "mkdir /deployments/caddy/.lock", Output: ""},
		ssh.MockCommand{Match: "a=$(docker exec caddy md5sum", Output: "TEPLOY_CADDY_OK"},
		ssh.MockCommand{Match: "docker exec caddy caddy reload", Output: ""},
		ssh.MockCommand{Match: "rmdir /deployments/caddy/.lock", Output: ""},
		ssh.MockCommand{Match: "printf %s", Output: ""},
	)
	var buf bytes.Buffer
	d := NewDeployer(mock, &buf)
	err := d.Deploy(context.Background(), Config{
		App:    "myapp",
		Domain: "myapp.com",
		Image:  "myapp:latest",
		Version: "abc123",
		Health: HealthConfig{Timeout: 5 * time.Second, Interval: 10 * time.Millisecond},
	})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	// The rendered route block dials the normalized container port.
	caddyfile := string(mock.Files["/deployments/caddy/Caddyfile"])
	if !strings.Contains(caddyfile, "myapp-web-abc123:80") {
		t.Errorf("expected the Caddy upstream to dial :80 for a default-valued ContainerPort, Caddyfile: %q", caddyfile)
	}
	if strings.Contains(caddyfile, "abc123:0") {
		t.Errorf("a zero container port leaked into the route: %q", caddyfile)
	}
}

// TestDeploy_AllCreatesUseResolvedImageID is the A52 regression: when the
// image ID resolves, EVERY container creation (web + workers) runs the
// immutable ID, never the mutable tag — a concurrent re-tag between creates
// can no longer mix images within one release.
func TestDeploy_AllCreatesUseResolvedImageID(t *testing.T) {
	imageID := "sha256:" + strings.Repeat("c0ffee", 10) + "abcd"
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "mkdir -p /deployments/myapp", Output: ""},
		ssh.MockCommand{Match: "mkdir /deployments/myapp/.lock", Output: ""},
		ssh.MockCommand{Match: "if [ ! -e '/deployments/myapp/state.json' ]", Output: "absent"},
		ssh.MockCommand{Match: "if [ ! -e '/deployments/myapp/state' ]", Output: "absent"},
		ssh.MockCommand{Match: "ss -tln", Output: ssOutput},
		ssh.MockCommand{Match: "docker image inspect --format '{{.Id}}'", Output: imageID},
		ssh.MockCommand{Match: "docker run", Output: "abc123"},
		ssh.MockCommand{Match: "docker inspect", Output: "running"},
		ssh.MockCommand{Match: "curl -s -o /dev/null", Output: "200"},
		ssh.MockCommand{Match: "cat /deployments/caddy/Caddyfile", Output: "{\n\tadmin 0.0.0.0:2019\n}\n"},
		ssh.MockCommand{Match: "mkdir /deployments/caddy/.lock", Output: ""},
		ssh.MockCommand{Match: "a=$(docker exec caddy md5sum", Output: "TEPLOY_CADDY_OK"},
		ssh.MockCommand{Match: "docker exec caddy caddy reload", Output: ""},
		ssh.MockCommand{Match: "rmdir /deployments/caddy/.lock", Output: ""},
		ssh.MockCommand{Match: "printf %s", Output: ""},
	)
	var buf bytes.Buffer
	d := NewDeployer(mock, &buf)
	err := d.Deploy(context.Background(), Config{
		App:       "myapp",
		Domain:    "myapp.com",
		Image:     "myapp:latest",
		Version:   "abc123",
		Processes: map[string]string{"web": "", "worker": "npm run worker"},
		Health:    HealthConfig{Timeout: 5 * time.Second, Interval: 10 * time.Millisecond},
	})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	creates := 0
	for _, c := range mock.Calls {
		if !strings.HasPrefix(c, "docker run --detach") {
			continue
		}
		creates++
		if !strings.Contains(c, "'"+imageID+"'") {
			t.Errorf("container created from a non-pinned reference: %s", c)
		}
	}
	if creates != 2 {
		t.Fatalf("expected web+worker creates, got %d", creates)
	}
	// The requested reference remains the recorded provenance.
	if s := string(mock.Files["/deployments/myapp/state.json"]); !strings.Contains(s, `"image_ref":"myapp:latest"`) {
		t.Errorf("state must keep the requested image ref as provenance: %s", s)
	}
}

// TestDeploy_WorkerCrashLoopFailsDeploy is the A23 regression: a worker
// whose container exits (or restart-loops) right after the detached run
// fails the deploy — it used to be recorded as success while no jobs were
// consumed.
func TestDeploy_WorkerCrashLoopFailsDeploy(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "mkdir -p /deployments/myapp", Output: ""},
		ssh.MockCommand{Match: "mkdir /deployments/myapp/.lock", Output: ""},
		ssh.MockCommand{Match: "if [ ! -e '/deployments/myapp/state.json' ]", Output: "absent"},
		ssh.MockCommand{Match: "if [ ! -e '/deployments/myapp/state' ]", Output: "absent"},
		ssh.MockCommand{Match: "ss -tln", Output: ssOutput},
		ssh.MockCommand{Match: "docker run", Output: "abc123"},
		// The worker's full state shows it exited immediately (this stub
		// must win over the generic running one below).
		ssh.MockCommand{Match: "docker inspect -f '{{json .State}}'", Output: `{"Status":"exited","Running":false,"ExitCode":1}`},
		ssh.MockCommand{Match: "docker inspect", Output: "running"},
		ssh.MockCommand{Match: "curl -s -o /dev/null", Output: "200"},
		ssh.MockCommand{Match: "docker logs", Output: "boom"},
		ssh.MockCommand{Match: "docker stop", Output: ""},
		ssh.MockCommand{Match: "docker rm ", Output: ""},
		ssh.MockCommand{Match: "printf %s", Output: ""},
	)
	var buf bytes.Buffer
	d := NewDeployer(mock, &buf)
	err := d.Deploy(context.Background(), Config{
		App:       "myapp",
		Domain:    "myapp.com",
		Image:     "myapp:latest",
		Version:   "abc123",
		Processes: map[string]string{"web": "", "worker": "bad-command"},
		Health:    HealthConfig{Timeout: 5 * time.Second, Interval: 10 * time.Millisecond},
	})
	if err == nil || !strings.Contains(err.Error(), "worker myapp-worker-abc123") {
		t.Fatalf("expected the crashed worker to fail the deploy, got %v", err)
	}
	if _, ok := mock.Files["/deployments/myapp/state.json"]; ok {
		t.Error("a deploy with a dead worker must not commit state")
	}
}

// TestDeploy_PartialRunCorpseReconciled is the A14 regression: when docker
// creates the container but the run fails, the untracked corpse under the
// candidate name is removed (it is not running), so the next deploy's
// candidate does not collide.
func TestDeploy_PartialRunCorpseReconciled(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "mkdir -p /deployments/myapp", Output: ""},
		ssh.MockCommand{Match: "mkdir /deployments/myapp/.lock", Output: ""},
		ssh.MockCommand{Match: "if [ ! -e '/deployments/myapp/state.json' ]", Output: "absent"},
		ssh.MockCommand{Match: "if [ ! -e '/deployments/myapp/state' ]", Output: "absent"},
		ssh.MockCommand{Match: "ss -tln", Output: ssOutput},
		// The run fails after creating the container...
		ssh.MockCommand{Match: "docker run", Err: errBoom},
		// ...and the corpse under the candidate name is in Created state.
		ssh.MockCommand{Match: "docker inspect -f '{{.State.Status}}' 'myapp-web-abc123'", Output: "created"},
		ssh.MockCommand{Match: "docker rm -f", Output: ""},
		ssh.MockCommand{Match: "printf %s", Output: ""},
	)
	var buf bytes.Buffer
	d := NewDeployer(mock, &buf)
	err := d.Deploy(context.Background(), Config{
		App:     "myapp",
		Domain:  "myapp.com",
		Image:   "myapp:latest",
		Version: "abc123",
		Health:  HealthConfig{Timeout: 5 * time.Second, Interval: 10 * time.Millisecond},
	})
	if err == nil {
		t.Fatal("expected the deploy to fail")
	}
	sawCorpseRemoval := false
	for _, c := range mock.Calls {
		if strings.HasPrefix(c, "docker rm -f 'myapp-web-abc123'") {
			sawCorpseRemoval = true
		}
	}
	if !sawCorpseRemoval {
		t.Error("the failed run's created-but-untracked container must be reconciled")
	}
}

// TestDeploy_RunningNameConflictNeverRemoved: a RUNNING container under the
// candidate name is never removed by the partial-run reconciler (A14).
func TestDeploy_RunningNameConflictNeverRemoved(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "mkdir -p /deployments/myapp", Output: ""},
		ssh.MockCommand{Match: "mkdir /deployments/myapp/.lock", Output: ""},
		ssh.MockCommand{Match: "if [ ! -e '/deployments/myapp/state.json' ]", Output: "absent"},
		ssh.MockCommand{Match: "if [ ! -e '/deployments/myapp/state' ]", Output: "absent"},
		ssh.MockCommand{Match: "ss -tln", Output: ssOutput},
		ssh.MockCommand{Match: "docker run", Err: errBoom},
		ssh.MockCommand{Match: "docker inspect -f '{{.State.Status}}' 'myapp-web-abc123'", Output: "running"},
		ssh.MockCommand{Match: "docker rm -f", Output: ""},
		ssh.MockCommand{Match: "printf %s", Output: ""},
	)
	var buf bytes.Buffer
	d := NewDeployer(mock, &buf)
	_ = d.Deploy(context.Background(), Config{
		App:     "myapp",
		Domain:  "myapp.com",
		Image:   "myapp:latest",
		Version: "abc123",
		Health:  HealthConfig{Timeout: 5 * time.Second, Interval: 10 * time.Millisecond},
	})
	for _, c := range mock.Calls {
		if strings.HasPrefix(c, "docker rm -f 'myapp-web-abc123'") {
			t.Errorf("a running container under the candidate name must never be force-removed: %s", c)
		}
	}
}
