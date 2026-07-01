package docker

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/ssh"
)

func TestContainerName(t *testing.T) {
	name := ContainerName("myapp", "web", "6ef8a6a8")
	if name != "myapp-web-6ef8a6a8" {
		t.Fatalf("expected myapp-web-6ef8a6a8, got %s", name)
	}
}

func TestClient_HostPort(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "docker inspect -f", Output: "49153 "},
	)
	client := NewClient(mock)

	port, err := client.HostPort(context.Background(), "myapp-web-abc123")
	if err != nil {
		t.Fatalf("HostPort: %v", err)
	}
	if port != 49153 {
		t.Errorf("got %d, want 49153", port)
	}
}

func TestClient_HostPort_NoBindings(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "docker inspect -f", Output: ""},
	)
	client := NewClient(mock)

	if _, err := client.HostPort(context.Background(), "myapp-worker-abc123"); err == nil {
		t.Error("expected an error for a container with no host-mapped ports")
	}
}

func TestClient_Run(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "docker run", Output: "abc123def456"},
	)
	client := NewClient(mock)

	id, err := client.Run(context.Background(), RunConfig{
		App:     "myapp",
		Process: "web",
		Version: "abc123",
		Image:   "nginx:latest",
		Port:    49152,
	})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if id != "abc123def456" {
		t.Fatalf("expected abc123def456, got %s", id)
	}

	cmd := mock.Calls[0]
	for _, want := range []string{
		"docker run --detach",
		"--restart unless-stopped",
		"--name 'myapp-web-abc123'",
		"--network teploy",
		"--network-alias 'myapp'",
		"--label 'teploy.app=myapp'",
		"--label 'teploy.process=web'",
		"--label 'teploy.version=abc123'",
		"-p 127.0.0.1:49152:80",
		"-e PORT=80",
		"--log-opt max-size=10m",
		"'nginx:latest'",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("command missing %q\ngot: %s", want, cmd)
		}
	}
}

func TestClient_Run_Worker(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "docker run", Output: "worker123"},
	)
	client := NewClient(mock)

	_, err := client.Run(context.Background(), RunConfig{
		App:     "myapp",
		Process: "worker",
		Version: "abc123",
		Image:   "myapp:abc123",
		Cmd:     "npm run worker",
	})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	cmd := mock.Calls[0]

	// Worker gets app-process alias, not just app name.
	if !strings.Contains(cmd, "--network-alias 'myapp-worker'") {
		t.Errorf("worker should get alias myapp-worker\ngot: %s", cmd)
	}

	// No port publishing when Port is zero.
	if strings.Contains(cmd, "-p ") {
		t.Error("worker without Port should not have port publishing")
	}

	// Command override appended after image.
	if !strings.HasSuffix(strings.TrimSpace(cmd), "npm run worker") {
		t.Errorf("command should end with cmd override\ngot: %s", cmd)
	}
}

func TestClient_Run_NoHealthcheck(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "docker run", Output: "nohealth123"},
	)
	client := NewClient(mock)

	_, err := client.Run(context.Background(), RunConfig{
		App:           "myapp",
		Process:       "worker",
		Version:       "abc123",
		Image:         "myapp:abc123",
		Cmd:           "npm run worker",
		NoHealthcheck: true,
	})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	cmd := mock.Calls[0]
	if !strings.Contains(cmd, "--no-healthcheck") {
		t.Errorf("NoHealthcheck=true should add --no-healthcheck to command\ngot: %s", cmd)
	}

	// The flag must precede the image so docker treats it as a run flag,
	// not a command argument to the container.
	imageIdx := strings.Index(cmd, "myapp:abc123")
	flagIdx := strings.Index(cmd, "--no-healthcheck")
	if flagIdx < 0 || flagIdx > imageIdx {
		t.Errorf("--no-healthcheck must appear before the image\ngot: %s", cmd)
	}
}

func TestClient_Run_HealthcheckDefault(t *testing.T) {
	// When NoHealthcheck is false (zero value), --no-healthcheck must not
	// be added. This is the default for all existing Teploy users.
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "docker run", Output: "default123"},
	)
	client := NewClient(mock)

	_, err := client.Run(context.Background(), RunConfig{
		App:     "myapp",
		Process: "web",
		Version: "abc123",
		Image:   "myapp:abc123",
		Port:    49152,
	})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	if strings.Contains(mock.Calls[0], "--no-healthcheck") {
		t.Errorf("NoHealthcheck=false should not add --no-healthcheck\ngot: %s", mock.Calls[0])
	}
}

func TestClient_Run_WithOptions(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "docker run", Output: "opts123"},
	)
	client := NewClient(mock)

	_, err := client.Run(context.Background(), RunConfig{
		App:      "myapp",
		Process:  "web",
		Version:  "abc123",
		Image:    "myapp:abc123",
		Port:     49152,
		EnvFiles: []string{"/deployments/myapp/.env"},
		Env:      map[string]string{"NODE_ENV": "production", "APP_KEY": "secret"},
		Volumes:  map[string]string{"/deployments/myapp/volumes/data": "/app/data"},
		Memory:   "512m",
		CPU:      "1.0",
	})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	cmd := mock.Calls[0]
	for _, want := range []string{
		"--env-file '/deployments/myapp/.env'",
		"-e 'APP_KEY=secret'",
		"-e 'NODE_ENV=production'",
		"-v '/deployments/myapp/volumes/data:/app/data'",
		"--memory '512m'",
		"--cpus '1.0'",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("command missing %q\ngot: %s", want, cmd)
		}
	}

	// Verify env vars are sorted (APP_KEY before NODE_ENV).
	appKeyIdx := strings.Index(cmd, "APP_KEY")
	nodeEnvIdx := strings.Index(cmd, "NODE_ENV")
	if appKeyIdx > nodeEnvIdx {
		t.Error("env vars should be sorted alphabetically")
	}
}

func TestClient_Run_Validation(t *testing.T) {
	client := NewClient(ssh.NewMockExecutor("1.2.3.4"))

	_, err := client.Run(context.Background(), RunConfig{})
	if err == nil {
		t.Fatal("expected error for empty config")
	}

	_, err = client.Run(context.Background(), RunConfig{App: "myapp"})
	if err == nil {
		t.Fatal("expected error for partial config")
	}
}

func TestClient_Stop(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "docker stop", Output: "myapp-web-abc123"},
	)
	client := NewClient(mock)

	if err := client.Stop(context.Background(), "myapp-web-abc123", 10); err != nil {
		t.Fatalf("Stop failed: %v", err)
	}

	if mock.Calls[0] != "docker stop -t 10 'myapp-web-abc123'" {
		t.Fatalf("unexpected command: %s", mock.Calls[0])
	}
}

func TestClient_Remove(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "docker rm", Output: "myapp-web-abc123"},
	)
	client := NewClient(mock)

	if err := client.Remove(context.Background(), "myapp-web-abc123"); err != nil {
		t.Fatalf("Remove failed: %v", err)
	}

	if mock.Calls[0] != "docker rm 'myapp-web-abc123'" {
		t.Fatalf("unexpected command: %s", mock.Calls[0])
	}
}

func TestClient_ListContainers(t *testing.T) {
	jsonOutput := `{"ID":"abc123","Names":"myapp-web-6ef8a6a8","Image":"nginx:latest","State":"running","Status":"Up 2 hours"}
{"ID":"def456","Names":"myapp-worker-6ef8a6a8","Image":"nginx:latest","State":"exited","Status":"Exited (0) 5m ago"}`

	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "docker ps", Output: jsonOutput},
	)
	client := NewClient(mock)

	containers, err := client.ListContainers(context.Background(), "myapp")
	if err != nil {
		t.Fatalf("ListContainers failed: %v", err)
	}
	if len(containers) != 2 {
		t.Fatalf("expected 2 containers, got %d", len(containers))
	}
	if containers[0].Name != "myapp-web-6ef8a6a8" {
		t.Fatalf("expected myapp-web-6ef8a6a8, got %s", containers[0].Name)
	}
	if containers[0].State != "running" {
		t.Fatalf("expected running, got %s", containers[0].State)
	}
	if containers[1].State != "exited" {
		t.Fatalf("expected exited, got %s", containers[1].State)
	}
}

func TestClient_ListContainers_Empty(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "docker ps", Output: ""},
	)
	client := NewClient(mock)

	containers, err := client.ListContainers(context.Background(), "myapp")
	if err != nil {
		t.Fatalf("ListContainers failed: %v", err)
	}
	if containers != nil {
		t.Fatalf("expected nil for empty result, got %v", containers)
	}
}

func TestClient_EnsureNetwork(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "docker network", Output: "teploy"},
	)
	client := NewClient(mock)

	if err := client.EnsureNetwork(context.Background()); err != nil {
		t.Fatalf("EnsureNetwork failed: %v", err)
	}

	cmd := mock.Calls[0]
	if !strings.Contains(cmd, "docker network inspect teploy") {
		t.Errorf("expected network inspect, got: %s", cmd)
	}
	if !strings.Contains(cmd, "docker network create teploy") {
		t.Errorf("expected network create fallback, got: %s", cmd)
	}
}

func TestClient_FindAvailablePort(t *testing.T) {
	ssOutput := `State  Recv-Q Send-Q  Local Address:Port  Peer Address:Port
LISTEN 0      128          0.0.0.0:22         0.0.0.0:*
LISTEN 0      128          0.0.0.0:80         0.0.0.0:*
LISTEN 0      128        127.0.0.1:49152      0.0.0.0:*`

	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "ss", Output: ssOutput},
	)
	client := NewClient(mock)

	port, err := client.FindAvailablePort(context.Background())
	if err != nil {
		t.Fatalf("FindAvailablePort failed: %v", err)
	}
	// 49152 is in use, so the first available is 49153.
	if port != 49153 {
		t.Fatalf("expected 49153, got %d", port)
	}
}

func TestClient_FindAvailablePort_FirstFree(t *testing.T) {
	ssOutput := `State  Recv-Q Send-Q  Local Address:Port  Peer Address:Port
LISTEN 0      128          0.0.0.0:22         0.0.0.0:*`

	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "ss", Output: ssOutput},
	)
	client := NewClient(mock)

	port, err := client.FindAvailablePort(context.Background())
	if err != nil {
		t.Fatalf("FindAvailablePort failed: %v", err)
	}
	if port != 49152 {
		t.Fatalf("expected 49152, got %d", port)
	}
}

func TestParseListeningPorts(t *testing.T) {
	output := `State  Recv-Q Send-Q  Local Address:Port  Peer Address:Port Process
LISTEN 0      128          0.0.0.0:22         0.0.0.0:*
LISTEN 0      128          0.0.0.0:80         0.0.0.0:*
LISTEN 0      128             [::]:443           [::]:*
LISTEN 0      128        127.0.0.1:49152      0.0.0.0:*`

	ports := parseListeningPorts(output)

	for _, p := range []int{22, 80, 443, 49152} {
		if !ports[p] {
			t.Errorf("expected port %d to be in use", p)
		}
	}
	if ports[8080] {
		t.Error("port 8080 should not be in use")
	}
}

func TestParseContainers(t *testing.T) {
	output := `{"ID":"abc123","Names":"myapp-web-6ef8a6a8","Image":"nginx:latest","State":"running","Status":"Up 2 hours"}`

	containers, err := ParseContainers(output)
	if err != nil {
		t.Fatalf("parseContainers failed: %v", err)
	}
	if len(containers) != 1 {
		t.Fatalf("expected 1 container, got %d", len(containers))
	}

	c := containers[0]
	if c.ID != "abc123" {
		t.Errorf("expected ID abc123, got %s", c.ID)
	}
	if c.Name != "myapp-web-6ef8a6a8" {
		t.Errorf("expected name myapp-web-6ef8a6a8, got %s", c.Name)
	}
	if c.Image != "nginx:latest" {
		t.Errorf("expected image nginx:latest, got %s", c.Image)
	}
	if c.State != "running" {
		t.Errorf("expected state running, got %s", c.State)
	}
}

func TestParseContainers_Invalid(t *testing.T) {
	_, err := ParseContainers("not json at all")
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

// TestEndToEnd_StartListStop exercises the core Step 2 acceptance flow:
// start a container, verify it's running, stop it.
func TestEndToEnd_StartListStop(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "docker network", Output: "teploy"},
		ssh.MockCommand{Match: "docker run", Output: "abc123def456"},
		ssh.MockCommand{Match: "docker ps", Output: `{"ID":"abc123def456","Names":"myapp-web-abc123","Image":"nginx:latest","State":"running","Status":"Up 5 seconds"}`},
		ssh.MockCommand{Match: "docker stop", Output: "myapp-web-abc123"},
		ssh.MockCommand{Match: "docker rm", Output: "myapp-web-abc123"},
	)
	client := NewClient(mock)
	ctx := context.Background()

	// 1. Ensure network exists.
	if err := client.EnsureNetwork(ctx); err != nil {
		t.Fatalf("EnsureNetwork: %v", err)
	}

	// 2. Start container.
	id, err := client.Run(ctx, RunConfig{
		App:     "myapp",
		Process: "web",
		Version: "abc123",
		Image:   "nginx:latest",
		Port:    49152,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if id != "abc123def456" {
		t.Fatalf("expected id abc123def456, got %s", id)
	}

	// 3. List and verify running.
	containers, err := client.ListContainers(ctx, "myapp")
	if err != nil {
		t.Fatalf("ListContainers: %v", err)
	}
	if len(containers) != 1 {
		t.Fatalf("expected 1 container, got %d", len(containers))
	}
	if containers[0].State != "running" {
		t.Fatalf("expected running, got %s", containers[0].State)
	}

	// 4. Stop and remove.
	if err := client.Stop(ctx, "myapp-web-abc123", 10); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := client.Remove(ctx, "myapp-web-abc123"); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	// Verify all 5 SSH calls were made.
	if len(mock.Calls) != 5 {
		t.Fatalf("expected 5 calls, got %d: %v", len(mock.Calls), mock.Calls)
	}
}

func TestClient_Exec(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "docker exec", Output: "migration complete"},
	)
	client := NewClient(mock)

	output, err := client.Exec(context.Background(), "myapp-web-abc123", "npm run migrate")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if output != "migration complete" {
		t.Errorf("expected 'migration complete', got %s", output)
	}

	cmd := mock.Calls[0]
	if !strings.Contains(cmd, "docker exec 'myapp-web-abc123'") {
		t.Errorf("expected docker exec with container name: %s", cmd)
	}
	if !strings.Contains(cmd, "sh -c") {
		t.Errorf("expected sh -c in exec command: %s", cmd)
	}
}

func TestClient_Exec_Failure(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "docker exec", Output: "error: relation does not exist", Err: fmt.Errorf("exit status 1")},
	)
	client := NewClient(mock)

	output, err := client.Exec(context.Background(), "myapp-web-abc123", "npm run migrate")
	if err == nil {
		t.Fatal("expected error")
	}
	if output != "error: relation does not exist" {
		t.Errorf("expected error output, got %s", output)
	}
}

func TestRunningContainer(t *testing.T) {
	// Two web containers: an old stopped one and the current running one,
	// plus a worker. RunningContainer("web") must return the running web.
	psOutput := `{"ID":"old","Names":"myapp-web-v1","Image":"myapp:v1","State":"exited","Status":"Exited","Labels":"teploy.app=myapp,teploy.process=web,teploy.version=v1"}
{"ID":"cur","Names":"myapp-web-v2","Image":"myapp:v2","State":"running","Status":"Up 1m","Labels":"teploy.app=myapp,teploy.process=web,teploy.version=v2"}
{"ID":"wk","Names":"myapp-worker-v2","Image":"myapp:v2","State":"running","Status":"Up 1m","Labels":"teploy.app=myapp,teploy.process=worker,teploy.version=v2"}`
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "docker ps --all --filter label=teploy.app='myapp'", Output: psOutput},
	)
	client := NewClient(mock)

	name, err := client.RunningContainer(context.Background(), "myapp", "web")
	if err != nil {
		t.Fatalf("RunningContainer: %v", err)
	}
	if name != "myapp-web-v2" {
		t.Errorf("expected running web myapp-web-v2, got %q", name)
	}

	worker, err := client.RunningContainer(context.Background(), "myapp", "worker")
	if err != nil || worker != "myapp-worker-v2" {
		t.Errorf("expected myapp-worker-v2, got %q (err %v)", worker, err)
	}
}

func TestRunningContainer_NoneRunning(t *testing.T) {
	psOutput := `{"ID":"old","Names":"myapp-web-v1","Image":"myapp:v1","State":"exited","Status":"Exited","Labels":"teploy.app=myapp,teploy.process=web,teploy.version=v1"}`
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "docker ps --all --filter label=teploy.app='myapp'", Output: psOutput},
	)
	client := NewClient(mock)
	if _, err := client.RunningContainer(context.Background(), "myapp", "web"); err == nil {
		t.Error("expected error when no running web container exists")
	}
}

func TestExecStream(t *testing.T) {
	// Verify the exec command is single-quoted (name + command) and output streams.
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "docker exec 'myapp-web-v2' sh -c 'bin/rails db:migrate'", Output: "migrated\n"},
	)
	client := NewClient(mock)

	var out strings.Builder
	if err := client.ExecStream(context.Background(), "myapp-web-v2", "bin/rails db:migrate", &out, &out); err != nil {
		t.Fatalf("ExecStream: %v", err)
	}
	if out.String() != "migrated\n" {
		t.Errorf("expected streamed output 'migrated', got %q", out.String())
	}
}

func TestExecStream_PropagatesError(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "docker exec 'c' sh -c 'false'", Output: "", Err: fmt.Errorf("exit status 1")},
	)
	client := NewClient(mock)
	var out strings.Builder
	if err := client.ExecStream(context.Background(), "c", "false", &out, &out); err == nil {
		t.Error("expected ExecStream to propagate the command's non-zero exit")
	}
}
