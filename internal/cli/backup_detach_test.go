package cli

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/useteploy/teploy/internal/backup"
	"github.com/useteploy/teploy/internal/events"
	"github.com/useteploy/teploy/internal/ssh"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBackupRunDetachFlagRefusals(t *testing.T) {
	flags := &Flags{Host: "h"}
	for _, args := range [][]string{{"--detach"}, {"--detach", "--app", "x"}, {"--all", "--detach", "--app", "x"}, {"--all", "--detach", "--accessory", "x"}} {
		cmd := newBackupRunCmd(flags)
		cmd.SetArgs(args)
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "--detach") {
			t.Fatalf("%v: %v", args, err)
		}
	}
	// --host is not optional: there is no server to start it on.
	cmd := newBackupRunCmd(&Flags{})
	cmd.SetArgs([]string{"--all", "--detach"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "--host") {
		t.Fatalf("%v", err)
	}
}

// Too old a binary is a refusal naming the consented way: never an implicit upgrade.
func TestDetachNeverUpgrades(t *testing.T) {
	mock := ssh.NewMockExecutor("h", ssh.MockCommand{Match: "'/usr/local/bin/teploy' backup run --help", Err: errors.New("exit status 1")})
	_, err := startDetachedRun(context.Background(), mock, false, events.Actor{}, setupNow)
	if err == nil || !strings.Contains(err.Error(), "--upgrade-binary") {
		t.Fatalf("%v", err)
	}
	if len(mock.Calls) != 1 {
		t.Fatalf("%v", mock.Calls)
	}
}

func detachMock(reply string, extra ...ssh.MockCommand) *ssh.MockExecutor {
	cmds := []ssh.MockCommand{
		{Match: "'/usr/local/bin/teploy' backup run --help", Output: ""},
		{Match: "sh -c '" + detachMarker, Output: reply},
		{Match: "rm -f -- ", Output: ""},
		{Match: "python3 -c", Output: `{"seq":1,"ts":"2026-01-01T00:00:00Z"}`},
	}
	return ssh.NewMockExecutor("192.0.2.10", append(cmds, extra...)...)
}

func TestDetachCommandIsTheCronRunAndIsRecorded(t *testing.T) {
	mock := detachMock("started 4242 /root/.config/teploy/backup-runs/run.abc\n")
	d, err := startDetachedRun(context.Background(), mock, true, events.Actor{Client: "dash", Label: "ada", Op: "op1"}, setupNow)
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != DetachStarted || d.PID != 4242 || d.Log != "/root/.config/teploy/backup-runs/run.abc.log" || d.Result != nil {
		t.Fatalf("%+v", d)
	}
	if detachExit(d) != nil {
		t.Fatal("started is not a failure")
	}
	var script string
	for _, c := range mock.Calls {
		if strings.Contains(c, detachMarker) {
			script = c
		}
	}
	// ShellQuote nests: the words are there, quoted twice.
	for _, want := range []string{"/usr/local/bin/teploy", " backup run --all --json --ignore-space-check", "--actor", "dash:ada", "--actor-op", "op1", "setsid nohup"} {
		if !strings.Contains(script, want) {
			t.Fatalf("no %q in %s", want, script)
		}
	}
	if len(mock.Inputs) != 1 || !strings.Contains(mock.Inputs[0], `"kind":"backup.detach"`) || !strings.Contains(mock.Inputs[0], `"outcome":"succeeded"`) || !strings.Contains(mock.Inputs[0], `"pid":4242`) {
		t.Fatalf("%v", mock.Inputs)
	}
}

func TestDetachAlreadyRunningIsNotAFailure(t *testing.T) {
	skipped, _ := json.Marshal(backupRunEnvelope{MachineInterface: MachineInterface, FleetRunResult: backup.FleetRunResult{Status: "skipped", StartedAt: setupNow, Targets: []backup.FleetTargetResult{}, Error: "another run holds the lock"}})
	mock := detachMock("finished 4243 /root/run.abc\n" + detachResultMark + "\n" + string(skipped) + "\n" + detachLogMark + "\nAnother backup run --all is running\n")
	d, err := startDetachedRun(context.Background(), mock, false, events.Actor{}, setupNow)
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != DetachAlreadyRunning || d.PID != 0 || d.Log != "" || detachExit(d) != nil {
		t.Fatalf("%+v", d)
	}
	if len(mock.Inputs) != 1 || !strings.Contains(mock.Inputs[0], `"outcome":"refused"`) {
		t.Fatalf("%v", mock.Inputs)
	}
	if !strings.Contains(strings.Join(mock.Calls, "\n"), "rm -f -- '/root/run.abc' '/root/run.abc.json'") {
		t.Fatalf("%v", mock.Calls)
	}
}

func TestDetachRunThatEndsAtOnce(t *testing.T) {
	failed, _ := json.Marshal(backupRunEnvelope{MachineInterface: MachineInterface, FleetRunResult: backup.FleetRunResult{Status: backup.RunFailed, StartedAt: setupNow, Targets: []backup.FleetTargetResult{}, Error: "no backup policy on this server"}})
	d, err := startDetachedRun(context.Background(), detachMock("finished 1 /r/run.a\n"+detachResultMark+"\n"+string(failed)+"\n"+detachLogMark+"\n"), false, events.Actor{}, setupNow)
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != DetachFinished || d.Result == nil || d.Result.Error == "" {
		t.Fatalf("%+v", d)
	}
	if e := detachExit(d); e == nil || !strings.Contains(e.Error(), "no backup policy") {
		t.Fatalf("%v", e)
	}
	// Ended with no result at all: the log's last line is the reason.
	mock := detachMock("finished 1 /r/run.a\n" + detachResultMark + "\n" + detachLogMark + "\nunknown flag: --json\n")
	d, err = startDetachedRun(context.Background(), mock, false, events.Actor{}, setupNow)
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != DetachFailed || !strings.Contains(d.Error, "unknown flag") || detachExit(d) == nil {
		t.Fatalf("%+v", d)
	}
	if !strings.Contains(mock.Inputs[0], `"outcome":"failed"`) {
		t.Fatalf("%v", mock.Inputs)
	}
	// Refused garbage rather than guessing.
	if _, err := startDetachedRun(context.Background(), detachMock("what\n"), false, events.Actor{}, setupNow); err == nil {
		t.Fatal("no error")
	}
}

// fakeServerBinary is a teploy that answers --help and runs body for backup run --all.
func fakeServerBinary(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "teploy")
	script := "#!/bin/sh\ncase \"$*\" in\n*--help*) echo '  --all  back up everything';;\n*)\n" + body + "\n;;\nesac\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEPLOY_SERVER_BINARY", path)
	home := t.TempDir()
	t.Setenv("HOME", home)
	return dir
}

// The real start script through a real shell: it returns while the run goes on, with
// nothing of the run attached to the connection.
func TestDetachScriptRunOutlivesTheStart(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "finished")
	fakeServerBinary(t, `sleep 6; echo "$*" > `+ssh.ShellQuote(marker))
	started := time.Now()
	d, err := startDetachedRun(context.Background(), localNoEvents{ssh.NewLocalExecutor()}, false, events.Actor{Client: "dash"}, setupNow)
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != DetachStarted || d.PID == 0 || !strings.HasSuffix(d.Log, ".log") {
		t.Fatalf("%+v", d)
	}
	if took := time.Since(started); took > 5500*time.Millisecond {
		t.Fatalf("waited for the run: %s", took)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("already finished")
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		if b, err := os.ReadFile(marker); err == nil {
			if !strings.Contains(string(b), "backup run --all --json") || !strings.Contains(string(b), "--actor dash") {
				t.Fatalf("%s", b)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the run did not outlive the start")
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func TestDetachScriptSeesTheStandDown(t *testing.T) {
	fakeServerBinary(t, `echo '{"machine_interface":2,"status":"skipped","started_at":"2026-11-06T00:00:00Z","duration_ms":0,"encrypted":false,"targets":[],"error":"another run holds the lock"}'; echo "Another backup run --all is running" >&2`)
	d, err := startDetachedRun(context.Background(), localNoEvents{ssh.NewLocalExecutor()}, false, events.Actor{}, setupNow)
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != DetachAlreadyRunning {
		t.Fatalf("%+v", d)
	}
	entries, err := os.ReadDir(filepath.Join(os.Getenv("HOME"), ".config/teploy/backup-runs"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("a stood-down run left %v", entries)
	}
}

func TestDetachScriptReportsARunThatDied(t *testing.T) {
	fakeServerBinary(t, `echo "boom: cannot start" >&2; exit 2`)
	d, err := startDetachedRun(context.Background(), localNoEvents{ssh.NewLocalExecutor()}, false, events.Actor{}, setupNow)
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != DetachFailed || !strings.Contains(d.Error, "boom: cannot start") {
		t.Fatalf("%+v", d)
	}
}

// localNoEvents runs commands locally and answers the event helper itself: the real
// one writes under /deployments.
type localNoEvents struct{ *ssh.LocalExecutor }

func (l localNoEvents) RunStream(ctx context.Context, cmd string, stdout, stderr io.Writer) error {
	if strings.HasPrefix(cmd, "python3 -c") {
		_, err := io.WriteString(stdout, `{"seq":1,"ts":"2026-01-01T00:00:00Z"}`)
		return err
	}
	return l.LocalExecutor.RunStream(ctx, cmd, stdout, stderr)
}

// What a run under `--detach` records for a target is what coverage reads.
func TestFleetTargetEventIsPolicyManaged(t *testing.T) {
	mock := ssh.NewMockExecutor("h", ssh.MockCommand{Match: "python3 -c", Output: `{"seq":1,"ts":"2026-01-01T00:00:00Z"}`})
	recordFleetTargetEvent(context.Background(), mock, backup.FleetTargetResult{Kind: backup.TargetAccessory, App: "blog", Accessory: "db", Status: backup.TargetOK})
	if len(mock.Inputs) != 1 || !strings.Contains(mock.Inputs[0], `"policy_managed":true`) {
		t.Fatalf("%v", mock.Inputs)
	}
}
