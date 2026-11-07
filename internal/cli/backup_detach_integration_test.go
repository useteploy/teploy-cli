//go:build integration

// The detached run with the real binary and its real flock, and the preflight's real
// probes, on this machine as an unprivileged user: policy and target under a temp HOME,
// aws and docker as stubs (docker is what holds a run open while the lock is tested).
// Nothing outside the temp directories is touched. A host with no setsid gets a real
// one through perl, and dash is used where there is one.
//
//	go build -o /tmp/teploy-e2e ./cmd/teploy
//	TEPLOY_DETACH_E2E=/tmp/teploy-e2e go test -tags integration -run TestDetachE2E -v ./internal/cli
package cli

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy/internal/backup"
	"github.com/useteploy/teploy/internal/events"
	"github.com/useteploy/teploy/internal/ssh"
)

func writeE2E(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

func TestDetachE2E(t *testing.T) {
	bin := os.Getenv("TEPLOY_DETACH_E2E")
	if bin == "" {
		t.Skip("set TEPLOY_DETACH_E2E to a built teploy binary")
	}
	if _, err := os.Stat("/etc/teploy"); err == nil {
		t.Skip("this machine has /etc/teploy, which would shadow the temp policy")
	}
	home, stubs := t.TempDir(), t.TempDir()
	release := filepath.Join(home, "release-docker")
	writeE2E(t, filepath.Join(stubs, "aws"), "#!/bin/sh\nif [ \"$1\" = --version ]; then echo aws-cli/2.30.1 Python/3; exit 0; fi\nexit 1\n", 0o755)
	// The run's first docker call waits here, holding the run lock, until released.
	writeE2E(t, filepath.Join(stubs, "docker"), "#!/bin/sh\nwhile [ ! -e "+ssh.ShellQuote(release)+" ]; do sleep 1; done\nexit 1\n", 0o755)
	if _, err := exec.LookPath("setsid"); err != nil {
		writeE2E(t, filepath.Join(stubs, "setsid"), "#!/usr/bin/perl\nuse POSIX;\nPOSIX::setsid();\nexec @ARGV;\n", 0o755)
	}
	if _, err := os.Stat("/bin/dash"); err == nil {
		if err := os.Symlink("/bin/dash", filepath.Join(stubs, "sh")); err != nil {
			t.Fatal(err)
		}
	}
	policy := `{"v":"1","retention":{"keep_last":3},"volumes":false}`
	writeE2E(t, filepath.Join(home, ".config/teploy/backup-policy.json"), policy, 0o600)
	writeE2E(t, filepath.Join(home, ".config/teploy/backup-target.json"), `{"v":"1","bucket":"b","region":"r"}`, 0o600)
	t.Setenv("HOME", home)
	t.Setenv("PATH", stubs+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TEPLOY_SERVER_BINARY", bin)

	ctx := context.Background()
	local := localNoEvents{ssh.NewLocalExecutor()}
	now := time.Now().UTC()

	// The preflight's real probes.
	d := collectBackupPreflight(ctx, local, bin, "0.1.55", now)
	t.Logf("preflight: %+v", d)
	if !d.Reachable || d.Error != "" || d.Binary.FleetEngine == nil || !*d.Binary.FleetEngine || d.Binary.Version == "" {
		t.Fatalf("%+v", d)
	}
	if d.Tools.AWS.State != PreflightOK || d.Tools.AWS.Version != "2.30.1" || d.Tools.Python3.State != PreflightOK {
		t.Fatalf("%+v", d.Tools)
	}
	if d.Disk.FreeBytes == nil || *d.Disk.FreeBytes <= 0 || d.Target.State != PreflightConfigured || d.Policy.State != PreflightConfigured {
		t.Fatalf("%+v %+v %+v", d.Disk, d.Target, d.Policy)
	}
	// stat -c is GNU's: elsewhere the owner is unreadable, which must read as unknown,
	// and so must a crontab that could not be read.
	want := ManagedNo
	if runtime.GOOS != "linux" || d.Policy.CronInstalled == nil {
		want = PreflightUnknown
	}
	if d.Policy.ManagedElsewhere != want {
		t.Fatalf("%+v", d.Policy)
	}

	// The first run takes the lock and stays in docker. That this returned at all says
	// the run holds nothing of the connection: the executor waits for its pipes.
	first, err := startDetachedRun(ctx, local, false, events.Actor{Client: "dash", Label: "e2e"}, now)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("first: %+v", first)
	if first.Status != DetachStarted || first.PID == 0 {
		t.Fatalf("%+v", first)
	}
	pgid, err := exec.Command("ps", "-o", "pgid=", "-p", strconv.Itoa(first.PID)).Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(pgid)) != strconv.Itoa(first.PID) {
		t.Fatalf("not a session of its own: pgid %s, pid %d", pgid, first.PID)
	}

	// What a dropped connection sends, to the run's whole group.
	if err := exec.Command("kill", "-HUP", "-"+strconv.Itoa(first.PID)).Run(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	if err := exec.Command("kill", "-0", strconv.Itoa(first.PID)).Run(); err != nil {
		t.Fatal("the run did not survive a hangup")
	}

	// A second one finds the real flock held.
	second, err := startDetachedRun(ctx, local, false, events.Actor{Client: "dash", Label: "e2e"}, now)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("second: %+v", second)
	if second.Status != DetachAlreadyRunning || detachExit(second) != nil {
		t.Fatalf("%+v", second)
	}

	// Released, the first ends as a normal run does, with its result and exit status.
	writeE2E(t, release, "", 0o644)
	base := strings.TrimSuffix(first.Log, ".log")
	deadline := time.Now().Add(60 * time.Second)
	for {
		if _, err := os.Stat(base + ".rc"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			runlog, _ := os.ReadFile(first.Log)
			t.Fatalf("the run did not end: %s", runlog)
		}
		time.Sleep(time.Second)
	}
	body, err := os.ReadFile(base + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var env backupRunEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	if env.Status != backup.RunFailed || env.MachineInterface != MachineInterface {
		t.Fatalf("%s", body)
	}

	// The lock is free again: a third run is its own, and ends within the look.
	third, err := startDetachedRun(ctx, local, false, events.Actor{}, now)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("third: %+v", third)
	if third.Status != DetachFinished || third.Result == nil || detachExit(third) == nil {
		t.Fatalf("%+v", third)
	}
}
