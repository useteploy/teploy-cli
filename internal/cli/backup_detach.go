package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"github.com/useteploy/teploy/internal/backup"
	"github.com/useteploy/teploy/internal/events"
	"github.com/useteploy/teploy/internal/ssh"
)

// `backup run --all --detach`: the same `backup run --all` a cron line runs, started
// on the server so that it outlives this connection. Its flock, heartbeat, events and
// status are that run's own; this only starts it and looks once, briefly, at what it
// did.

// Detached run states.
const (
	DetachStarted        = "started"
	DetachAlreadyRunning = "already_running"
	DetachFinished       = "finished"
	DetachFailed         = "failed"
)

// backupDetachDTO is `backup run --all --detach --json` (backup-setup-v1).
type backupDetachDTO struct {
	MachineInterface int       `json:"machine_interface"`
	Host             string    `json:"host"`
	RequestedAt      time.Time `json:"requested_at"`
	// Status: started (still running when this looked), already_running (another run
	// holds the lock; this one stood down), finished (it ended within the first
	// seconds; see result), failed (it ended without a result; see error).
	Status string `json:"status"`
	PID    int    `json:"pid,omitempty"`
	// Log is the run's progress on the server.
	Log    string                 `json:"log,omitempty"`
	Result *backup.FleetRunResult `json:"result,omitempty"`
	Error  string                 `json:"error,omitempty"`
}

const (
	detachMarker     = "# teploy-backup-detach"
	detachResultMark = "TEPLOY-DETACH-RESULT"
	detachLogMark    = "TEPLOY-DETACH-LOG"
)

// detachWait is how many seconds the start script watches the run: long enough for
// one that finds the lock held, or no policy, to say so.
const detachWait = 3

// detachScript starts cmd in its own session with no descriptor on this connection,
// so ending the connection neither signals it nor waits for it. The exit status goes
// through a file: a shell cannot ask about a child in another session reliably.
func detachScript(cmd string) string {
	return detachMarker + `
umask 077
[ -n "${HOME:-}" ] || { echo "this user has no home directory for the run's log" >&2; exit 3; }
d="$HOME/.config/teploy/backup-runs"
mkdir -p "$d" || exit 3
find "$d" -type f -mtime +14 -exec rm -f {} + 2>/dev/null
b=$(mktemp "$d/run.XXXXXXXX") || exit 3
W='b=$1; shift; "$@" >"$b.json" 2>"$b.log"; echo $? >"$b.rc.t"; mv -f "$b.rc.t" "$b.rc"'
if command -v setsid >/dev/null 2>&1; then
  setsid nohup sh -c "$W" sh "$b" ` + cmd + ` </dev/null >/dev/null 2>&1 &
else
  nohup sh -c "$W" sh "$b" ` + cmd + ` </dev/null >/dev/null 2>&1 &
fi
pid=$!
i=0
while [ "$i" -lt ` + strconv.Itoa(detachWait) + ` ] && [ ! -e "$b.rc" ]; do sleep 1; i=$((i+1)); done
if [ ! -e "$b.rc" ]; then printf 'started %s %s\n' "$pid" "$b"; exit 0; fi
printf 'finished %s %s\n' "$pid" "$b"
echo ` + detachResultMark + `
cat "$b.json"
echo ` + detachLogMark + `
tail -n 5 "$b.log"`
}

// parseDetachReply reads the start script's report into the DTO, and returns the base
// path of the run's files.
func parseDetachReply(out string, dto *backupDetachDTO) (string, error) {
	head, rest, _ := strings.Cut(out, "\n")
	// The path comes last: a home directory may have spaces in it.
	f := strings.SplitN(head, " ", 3)
	if len(f) < 3 || (f[0] != "started" && f[0] != "finished") {
		return "", fmt.Errorf("starting the run: unrecognized reply from the server")
	}
	pid, err := strconv.Atoi(f[1])
	if err != nil {
		return "", fmt.Errorf("starting the run: unrecognized reply from the server")
	}
	base := f[2]
	dto.PID, dto.Log = pid, base+".log"
	if f[0] == "started" {
		dto.Status = DetachStarted
		return base, nil
	}
	_, rest, _ = strings.Cut(rest, detachResultMark+"\n")
	body, logTail, _ := strings.Cut(rest, detachLogMark)
	var env backupRunEnvelope
	if err := json.Unmarshal([]byte(body), &env); err != nil || env.Status == "" {
		dto.Status = DetachFailed
		dto.Error = "the run ended without a result"
		if reason := lastNonEmptyLine(logTail); reason != "" {
			dto.Error += ": " + reason
		}
		return base, nil
	}
	if env.Status == "skipped" {
		dto.Status = DetachAlreadyRunning
		return base, nil
	}
	dto.Status = DetachFinished
	dto.Result = &env.FleetRunResult
	return base, nil
}

func lastNonEmptyLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return oneLine(lines[len(lines)-1])
}

// fleetBinarySupportsAll is whether bin speaks `backup run --all`.
func fleetBinarySupportsAll(ctx context.Context, exec ssh.Executor, bin string) bool {
	return !ssh.RunDetailed(ctx, exec, ssh.ShellQuote(bin)+" backup run --help 2>/dev/null | grep -q -- --all").Failed()
}

// startDetachedRun starts the run and records that it did. It never upgrades the
// server's binary: one too old to run this is a refusal naming the consented path.
func startDetachedRun(ctx context.Context, exec ssh.Executor, ignoreSpace bool, actor events.Actor, now time.Time) (backupDetachDTO, error) {
	dto := backupDetachDTO{MachineInterface: MachineInterface, Host: exec.Host(), RequestedAt: now}
	bin := scheduledTeployPath()
	if !fleetBinarySupportsAll(ctx, exec, bin) {
		return dto, fmt.Errorf("%s on this server is missing or does not support `backup run --all`, so nothing was started: upgrade it with `teploy server backup-preflight --host %s --upgrade-binary`", bin, exec.Host())
	}
	q := ssh.ShellQuote
	cmd := q(bin) + " backup run --all --json"
	if ignoreSpace {
		cmd += " --ignore-space-check"
	}
	if actor.Client != "" {
		who := actor.Client
		if actor.Label != "" {
			who += ":" + actor.Label
		}
		cmd += " --actor " + q(who)
	}
	if actor.Op != "" {
		cmd += " --actor-op " + q(actor.Op)
	}
	res := ssh.RunDetailed(ctx, exec, "sh -c "+q(detachScript(cmd)))
	if res.Failed() {
		reason := oneLine(res.ExitErrorText())
		if reason == "" {
			reason = fmt.Sprintf("the command exited with status %d and gave no reason", res.ExitCode)
		}
		err := fmt.Errorf("starting the run: %s", reason)
		events.Record(ctx, exec, events.Event{App: backup.FleetApp, Kind: events.KindBackupDetach, Outcome: events.Failed, Detail: events.ErrDetail(map[string]any{}, err)})
		return dto, err
	}
	base, err := parseDetachReply(string(res.Stdout), &dto)
	if err != nil {
		return dto, err
	}
	detail := map[string]any{"status": dto.Status, "pid": dto.PID, "log": dto.Log}
	outcome := events.Succeeded
	switch dto.Status {
	case DetachAlreadyRunning:
		// Nothing of this one is worth keeping.
		_, _ = exec.Run(ctx, "rm -f -- "+q(base)+" "+q(base+".json")+" "+q(base+".log")+" "+q(base+".rc"))
		dto.PID, dto.Log = 0, ""
		detail = map[string]any{"status": dto.Status}
		outcome = events.Refused
	case DetachFailed:
		outcome = events.Failed
		detail["error"] = dto.Error
	}
	events.Record(ctx, exec, events.Event{App: backup.FleetApp, Kind: events.KindBackupDetach, Outcome: outcome, Detail: detail})
	return dto, nil
}

// detachExit is the command's exit status: an already-running run is an answer, not a
// failure, and one that has finished exits as `backup run --all` would have.
func detachExit(dto backupDetachDTO) error {
	switch dto.Status {
	case DetachFailed:
		return errors.New(dto.Error)
	case DetachFinished:
		return fleetFailed(*dto.Result)
	}
	return nil
}

func runBackupRunDetach(flags *Flags, ignoreSpace bool) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	host, user, key, err := configResolveServerForTarget(flags)
	if err != nil {
		return err
	}
	executor, err := ssh.Connect(ctx, ssh.ConnectConfig{Host: host, User: user, KeyPath: key})
	if err != nil {
		return err
	}
	defer executor.Close()
	sctx, scancel := context.WithTimeout(ctx, 2*time.Minute)
	defer scancel()
	dto, err := startDetachedRun(sctx, executor, ignoreSpace, resolveActor(flags, cliBuildVersion), time.Now().UTC())
	if err != nil {
		return err
	}
	if flags.JSON {
		if encErr := json.NewEncoder(os.Stdout).Encode(dto); encErr != nil {
			return errors.Join(detachExit(dto), encErr)
		}
		return detachExit(dto)
	}
	switch dto.Status {
	case DetachStarted:
		fmt.Printf("Backup run started on %s (pid %d). Progress: %s. Results reach `teploy backup status` and `teploy events` as a normal run's do.\n", dto.Host, dto.PID, dto.Log)
	case DetachAlreadyRunning:
		fmt.Printf("A backup run is already running on %s; this one stood down.\n", dto.Host)
	case DetachFinished:
		fmt.Printf("The run ended straight away: %s\n", dto.Result.Status)
	}
	return detachExit(dto)
}
