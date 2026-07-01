package accessories

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/ssh"
)

func TestEnsureRunning_New(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		// No stored credentials.
		ssh.MockCommand{Match: "cat /deployments/myapp/accessories/postgres/credentials", Err: fmt.Errorf("not found")},
		// Not running.
		ssh.MockCommand{Match: "docker inspect", Err: fmt.Errorf("not found")},
		// Create directory.
		ssh.MockCommand{Match: "mkdir -p /deployments/myapp/accessories/postgres", Output: ""},
		// Start container.
		ssh.MockCommand{Match: "docker run", Output: "abc123"},
	)

	var buf bytes.Buffer
	mgr := NewManager(mock, &buf)

	vars, err := mgr.EnsureRunning(context.Background(), "myapp", "postgres", config.AccessoryConfig{
		Image: "postgres:16",
		Port:  5432,
		Env: map[string]string{
			"POSTGRES_PASSWORD": "auto",
			"POSTGRES_DB":       "myapp",
		},
		Volumes: map[string]string{
			"data": "/var/lib/postgresql/data",
		},
	})
	if err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}

	// Verify docker run was called with correct flags.
	var runCmd string
	for _, call := range mock.Calls {
		if strings.HasPrefix(call, "docker run") {
			runCmd = call
		}
	}
	if runCmd == "" {
		t.Fatal("expected docker run command")
	}

	if !strings.Contains(runCmd, "--name 'myapp-postgres'") {
		t.Error("expected container name myapp-postgres")
	}
	if !strings.Contains(runCmd, "--network teploy") {
		t.Error("expected network teploy")
	}
	if !strings.Contains(runCmd, "--network-alias 'myapp-postgres'") {
		t.Error("expected network alias myapp-postgres")
	}
	if !strings.Contains(runCmd, "--restart always") {
		t.Error("expected restart always")
	}
	if !strings.Contains(runCmd, "teploy.role=accessory") {
		t.Error("expected accessory role label")
	}
	if !strings.Contains(runCmd, "teploy.accessory=postgres") {
		t.Error("expected accessory name label")
	}
	if !strings.Contains(runCmd, "-e 'POSTGRES_DB=myapp'") {
		t.Error("expected POSTGRES_DB env var")
	}
	if !strings.Contains(runCmd, "-v '/deployments/myapp/accessories/postgres/data:/var/lib/postgresql/data'") {
		t.Error("expected volume mount")
	}
	if !strings.Contains(runCmd, "postgres:16") {
		t.Error("expected image postgres:16")
	}

	// Verify credentials were written.
	credData := mock.Files["/deployments/myapp/accessories/postgres/credentials"]
	if credData == nil {
		t.Fatal("expected credentials file to be written")
	}
	credContent := string(credData)
	if !strings.Contains(credContent, "POSTGRES_PASSWORD=") {
		t.Error("expected POSTGRES_PASSWORD in credentials")
	}
	// Password should be non-empty hex string.
	for _, line := range strings.Split(credContent, "\n") {
		if strings.HasPrefix(line, "POSTGRES_PASSWORD=") {
			password := strings.TrimPrefix(line, "POSTGRES_PASSWORD=")
			if len(password) != 32 {
				t.Errorf("expected 32-char hex password, got %d chars: %s", len(password), password)
			}
		}
	}

	// Verify DATABASE_URL was returned.
	dbURL, ok := vars["DATABASE_URL"]
	if !ok {
		t.Fatal("expected DATABASE_URL in returned vars")
	}
	if !strings.HasPrefix(dbURL, "postgres://postgres:") {
		t.Errorf("expected postgres:// URL, got: %s", dbURL)
	}
	if !strings.Contains(dbURL, "@myapp-postgres:5432/myapp") {
		t.Errorf("expected @myapp-postgres:5432/myapp in URL, got: %s", dbURL)
	}

	// Verify output messages.
	output := buf.String()
	if !strings.Contains(output, "Starting myapp-postgres") {
		t.Error("expected starting message")
	}
	if !strings.Contains(output, "myapp-postgres started") {
		t.Error("expected started message")
	}
}

func TestEnsureRunning_AlreadyRunning(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		// Stored credentials exist (needed for connection string).
		ssh.MockCommand{Match: "cat /deployments/myapp/accessories/postgres/credentials", Output: "POSTGRES_PASSWORD=existingpass123\n"},
		// Already running.
		ssh.MockCommand{Match: "docker inspect", Output: "running"},
	)

	var buf bytes.Buffer
	mgr := NewManager(mock, &buf)

	vars, err := mgr.EnsureRunning(context.Background(), "myapp", "postgres", config.AccessoryConfig{
		Image: "postgres:16",
		Port:  5432,
		Env: map[string]string{
			"POSTGRES_PASSWORD": "auto",
			"POSTGRES_DB":       "myapp",
		},
	})
	if err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}

	// Should not have run docker run.
	for _, call := range mock.Calls {
		if strings.HasPrefix(call, "docker run") {
			t.Error("should not start container when already running")
		}
	}

	// Should still return connection env vars.
	if vars["DATABASE_URL"] != "postgres://postgres:existingpass123@myapp-postgres:5432/myapp" {
		t.Errorf("unexpected DATABASE_URL: %s", vars["DATABASE_URL"])
	}

	if !strings.Contains(buf.String(), "already running") {
		t.Error("expected 'already running' message")
	}
}

func TestEnsureRunning_StoredCredentials(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		// Stored credentials exist.
		ssh.MockCommand{Match: "cat /deployments/myapp/accessories/postgres/credentials", Output: "POSTGRES_PASSWORD=storedpass456\n"},
		// Not running.
		ssh.MockCommand{Match: "docker inspect", Err: fmt.Errorf("not found")},
		// Create directory.
		ssh.MockCommand{Match: "mkdir -p", Output: ""},
		// Start container.
		ssh.MockCommand{Match: "docker run", Output: "def456"},
	)

	var buf bytes.Buffer
	mgr := NewManager(mock, &buf)

	_, err := mgr.EnsureRunning(context.Background(), "myapp", "postgres", config.AccessoryConfig{
		Image: "postgres:16",
		Env: map[string]string{
			"POSTGRES_PASSWORD": "auto",
			"POSTGRES_DB":       "myapp",
		},
	})
	if err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}

	// Verify stored password was used (not a new one generated).
	var runCmd string
	for _, call := range mock.Calls {
		if strings.HasPrefix(call, "docker run") {
			runCmd = call
		}
	}
	if !strings.Contains(runCmd, "-e 'POSTGRES_PASSWORD=storedpass456'") {
		t.Error("expected stored password to be used")
	}

	// Credentials file should NOT be re-written (no new passwords generated).
	if _, ok := mock.Files["/deployments/myapp/accessories/postgres/credentials"]; ok {
		t.Error("should not re-write credentials when all auto values have stored values")
	}
}

func TestEnsureRunning_NoEnv(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		// Not running.
		ssh.MockCommand{Match: "docker inspect", Err: fmt.Errorf("not found")},
		// Create directory.
		ssh.MockCommand{Match: "mkdir -p", Output: ""},
		// Start container.
		ssh.MockCommand{Match: "docker run", Output: "ghi789"},
	)

	var buf bytes.Buffer
	mgr := NewManager(mock, &buf)

	vars, err := mgr.EnsureRunning(context.Background(), "myapp", "redis", config.AccessoryConfig{
		Image: "redis:7",
		Port:  6379,
	})
	if err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}

	if vars["REDIS_URL"] != "redis://myapp-redis:6379" {
		t.Errorf("unexpected REDIS_URL: %s", vars["REDIS_URL"])
	}
}

func TestList(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{
			Match:  "docker ps --all --filter label=teploy.app='myapp' --filter label=teploy.role=accessory",
			Output: `{"ID":"abc123","Names":"myapp-postgres","Image":"postgres:16","State":"running","Status":"Up 2 hours"}`,
		},
	)

	var buf bytes.Buffer
	mgr := NewManager(mock, &buf)

	containers, err := mgr.List(context.Background(), "myapp")
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	if len(containers) != 1 {
		t.Fatalf("expected 1 container, got %d", len(containers))
	}
	if containers[0].Name != "myapp-postgres" {
		t.Errorf("expected name myapp-postgres, got %s", containers[0].Name)
	}
	if containers[0].State != "running" {
		t.Errorf("expected state running, got %s", containers[0].State)
	}
}

func TestList_Empty(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "docker ps", Output: ""},
	)

	var buf bytes.Buffer
	mgr := NewManager(mock, &buf)

	containers, err := mgr.List(context.Background(), "myapp")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if containers != nil {
		t.Errorf("expected nil containers, got %v", containers)
	}
}

func TestStop(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "docker stop", Output: ""},
	)

	var buf bytes.Buffer
	mgr := NewManager(mock, &buf)

	if err := mgr.Stop(context.Background(), "myapp", "postgres"); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if len(mock.Calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(mock.Calls))
	}
	if !strings.Contains(mock.Calls[0], "docker stop -t 10 'myapp-postgres'") {
		t.Errorf("unexpected command: %s", mock.Calls[0])
	}
}

func TestStart(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "docker start", Output: ""},
	)

	var buf bytes.Buffer
	mgr := NewManager(mock, &buf)

	if err := mgr.Start(context.Background(), "myapp", "postgres"); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if len(mock.Calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(mock.Calls))
	}
	if mock.Calls[0] != "docker start 'myapp-postgres'" {
		t.Errorf("unexpected command: %s", mock.Calls[0])
	}
}

func TestLogs(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "docker logs", Output: "2024-01-01 LOG: database ready"},
	)

	var buf bytes.Buffer
	mgr := NewManager(mock, &buf)

	if err := mgr.Logs(context.Background(), "myapp", "postgres", 50); err != nil {
		t.Fatalf("Logs: %v", err)
	}

	if !strings.Contains(mock.Calls[0], "docker logs --tail 50 'myapp-postgres'") {
		t.Errorf("unexpected command: %s", mock.Calls[0])
	}
	if !strings.Contains(buf.String(), "database ready") {
		t.Error("expected log output")
	}
}

func TestUpgrade(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		// Pull new image.
		ssh.MockCommand{Match: "docker pull", Output: ""},
		// Stop old container.
		ssh.MockCommand{Match: "docker stop", Output: ""},
		// Remove old container.
		ssh.MockCommand{Match: "docker rm", Output: ""},
		// EnsureRunning: resolve env (no stored creds).
		ssh.MockCommand{Match: "cat /deployments/myapp/accessories/postgres/credentials", Err: fmt.Errorf("not found")},
		// EnsureRunning: not running (just removed).
		ssh.MockCommand{Match: "docker inspect", Err: fmt.Errorf("not found")},
		// EnsureRunning: create directory.
		ssh.MockCommand{Match: "mkdir -p", Output: ""},
		// EnsureRunning: start new container.
		ssh.MockCommand{Match: "docker run", Output: "new123"},
	)

	var buf bytes.Buffer
	mgr := NewManager(mock, &buf)

	err := mgr.Upgrade(context.Background(), "myapp", "postgres", "postgres:17", config.AccessoryConfig{
		Image: "postgres:16",
		Env: map[string]string{
			"POSTGRES_PASSWORD": "auto",
			"POSTGRES_DB":       "myapp",
		},
		Volumes: map[string]string{
			"data": "/var/lib/postgresql/data",
		},
	})
	if err != nil {
		t.Fatalf("Upgrade: %v", err)
	}

	// Verify pull was called with new image.
	if !strings.Contains(mock.Calls[0], "docker pull 'postgres:17'") {
		t.Errorf("expected pull of postgres:17, got: %s", mock.Calls[0])
	}

	// Verify new container uses new image.
	var runCmd string
	for _, call := range mock.Calls {
		if strings.HasPrefix(call, "docker run") {
			runCmd = call
		}
	}
	if !strings.Contains(runCmd, "postgres:17") {
		t.Errorf("expected new image in run command, got: %s", runCmd)
	}

	output := buf.String()
	if !strings.Contains(output, "Pulling postgres:17") {
		t.Error("expected pulling message")
	}
	if !strings.Contains(output, "Stopping myapp-postgres") {
		t.Error("expected stopping message")
	}
}

func TestConnectionEnvVars_Postgres(t *testing.T) {
	vars := connectionEnvVars("myapp", "postgres", "postgres:16", 5432, map[string]string{
		"POSTGRES_PASSWORD": "secret123",
		"POSTGRES_DB":       "mydb",
	})

	expected := "postgres://postgres:secret123@myapp-postgres:5432/mydb"
	if vars["DATABASE_URL"] != expected {
		t.Errorf("expected %s, got %s", expected, vars["DATABASE_URL"])
	}
}

func TestConnectionEnvVars_PostgresDefaults(t *testing.T) {
	vars := connectionEnvVars("myapp", "db", "postgres:16", 0, map[string]string{
		"POSTGRES_PASSWORD": "secret",
	})

	// Should default to port 5432, user postgres, db = app name.
	expected := "postgres://postgres:secret@myapp-db:5432/myapp"
	if vars["DATABASE_URL"] != expected {
		t.Errorf("expected %s, got %s", expected, vars["DATABASE_URL"])
	}
}

func TestConnectionEnvVars_MySQL(t *testing.T) {
	vars := connectionEnvVars("myapp", "mysql", "mysql:8", 3306, map[string]string{
		"MYSQL_ROOT_PASSWORD": "rootpass",
		"MYSQL_DATABASE":      "mydb",
	})

	expected := "mysql://root:rootpass@myapp-mysql:3306/mydb"
	if vars["DATABASE_URL"] != expected {
		t.Errorf("expected %s, got %s", expected, vars["DATABASE_URL"])
	}
}

func TestConnectionEnvVars_Redis(t *testing.T) {
	vars := connectionEnvVars("myapp", "redis", "redis:7", 6379, nil)

	expected := "redis://myapp-redis:6379"
	if vars["REDIS_URL"] != expected {
		t.Errorf("expected %s, got %s", expected, vars["REDIS_URL"])
	}
}

func TestConnectionEnvVars_Mongo(t *testing.T) {
	vars := connectionEnvVars("myapp", "mongo", "mongo:7", 27017, nil)

	expected := "mongodb://myapp-mongo:27017"
	if vars["MONGODB_URL"] != expected {
		t.Errorf("expected %s, got %s", expected, vars["MONGODB_URL"])
	}
}

func TestConnectionEnvVars_Unknown(t *testing.T) {
	vars := connectionEnvVars("myapp", "rabbitmq", "rabbitmq:3", 5672, nil)

	if len(vars) != 0 {
		t.Errorf("expected no vars for unknown image, got %v", vars)
	}
}

func TestInjectEnvVars_NewFile(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		// No existing .env.
		ssh.MockCommand{Match: "cat /deployments/myapp/.env", Err: fmt.Errorf("not found")},
	)

	var buf bytes.Buffer
	mgr := NewManager(mock, &buf)

	err := mgr.InjectEnvVars(context.Background(), "myapp", map[string]string{
		"DATABASE_URL": "postgres://localhost/myapp",
		"REDIS_URL":    "redis://localhost:6379",
	})
	if err != nil {
		t.Fatalf("InjectEnvVars: %v", err)
	}

	envData := mock.Files["/deployments/myapp/.env"]
	if envData == nil {
		t.Fatal("expected .env file to be created")
	}

	content := string(envData)
	if !strings.Contains(content, "DATABASE_URL=postgres://localhost/myapp") {
		t.Error("expected DATABASE_URL in .env")
	}
	if !strings.Contains(content, "REDIS_URL=redis://localhost:6379") {
		t.Error("expected REDIS_URL in .env")
	}
}

func TestInjectEnvVars_SkipExisting(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		// Existing .env with DATABASE_URL already set.
		ssh.MockCommand{Match: "cat /deployments/myapp/.env", Output: "DATABASE_URL=postgres://custom/mydb\n"},
		// Append new vars.
		ssh.MockCommand{Match: "cat /tmp/teploy_env_append", Output: ""},
	)

	var buf bytes.Buffer
	mgr := NewManager(mock, &buf)

	err := mgr.InjectEnvVars(context.Background(), "myapp", map[string]string{
		"DATABASE_URL": "postgres://auto-generated/myapp",
		"REDIS_URL":    "redis://localhost:6379",
	})
	if err != nil {
		t.Fatalf("InjectEnvVars: %v", err)
	}

	// Only REDIS_URL should be appended (DATABASE_URL already exists).
	appendData := mock.Files["/tmp/teploy_env_append"]
	if appendData == nil {
		t.Fatal("expected append file to be created")
	}

	content := string(appendData)
	if strings.Contains(content, "DATABASE_URL") {
		t.Error("should NOT overwrite existing DATABASE_URL")
	}
	if !strings.Contains(content, "REDIS_URL=redis://localhost:6379") {
		t.Error("expected REDIS_URL in append data")
	}
}

func TestInjectEnvVars_AllExist(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "cat /deployments/myapp/.env", Output: "DATABASE_URL=existing\nREDIS_URL=existing\n"},
	)

	var buf bytes.Buffer
	mgr := NewManager(mock, &buf)

	err := mgr.InjectEnvVars(context.Background(), "myapp", map[string]string{
		"DATABASE_URL": "new-value",
		"REDIS_URL":    "new-value",
	})
	if err != nil {
		t.Fatalf("InjectEnvVars: %v", err)
	}

	// No files should be written — all keys already exist.
	if len(mock.Files) != 0 {
		t.Errorf("expected no file writes, got %d", len(mock.Files))
	}
}

func TestContainerName(t *testing.T) {
	if got := ContainerName("myapp", "postgres"); got != "myapp-postgres" {
		t.Errorf("expected myapp-postgres, got %s", got)
	}
}

func TestIsImageType(t *testing.T) {
	tests := []struct {
		image, service string
		want           bool
	}{
		{"postgres:16", "postgres", true},
		{"postgres", "postgres", true},
		{"library/postgres:16", "postgres", true},
		{"redis:7", "redis", true},
		{"mysql:8", "mysql", true},
		{"mariadb:10", "mariadb", true},
		{"mongo:7", "mongo", true},
		{"custom-postgres:1", "postgres", false},
		{"nginx:latest", "postgres", false},
	}

	for _, tt := range tests {
		got := isImageType(tt.image, tt.service)
		if got != tt.want {
			t.Errorf("isImageType(%q, %q) = %v, want %v", tt.image, tt.service, got, tt.want)
		}
	}
}
