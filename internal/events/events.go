// Package events is the per-app history every client reads: one JSONL log
// per app at /deployments/.teploy/apps/<app>/events.jsonl, written only by
// the server-side helper below (server-authority spec §2.6).
//
// The helper holds an flock on events.lock for the whole append, assigns
// the server sequence number (strictly increasing per app, across
// rotations) and timestamp, rotates past 1 MiB keeping five files, and
// writes each line with a single O_APPEND write to a 0600 file. Clients
// never write the log directly, so a laptop CLI, the dash's bundled CLI,
// CI and the server listener all land in one ordered history.
//
// The log lives outside /deployments/<app>, so it survives `teploy
// remove` (including old CLIs' remove) and old CLIs that list apps by
// globbing /deployments/* skip it (dot directory).
//
// A failed append never fails the command that produced the event: callers
// warn and continue.
package events

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/useteploy/teploy/internal/ssh"
)

// Root is the server directory holding teploy's per-app server data.
const Root = "/deployments/.teploy"

// Outcomes. A "started" line without a terminal line and no live lock is
// rendered as unknown by readers.
const (
	Started   = "started"
	Succeeded = "succeeded"
	Failed    = "failed"
	Refused   = "refused"
)

// Actor says which client produced an event. Label is client-asserted
// (the dash keeps the authenticated identity in its own operation record,
// linked by Op).
type Actor struct {
	Client     string `json:"client"`
	Label      string `json:"label,omitempty"`
	CLIVersion string `json:"cli_version,omitempty"`
	Op         string `json:"op,omitempty"`
}

// ActorString renders an actor as client[:label], the flat form release
// records and audit lines carry.
func ActorString(a Actor) string {
	if a.Label == "" {
		return a.Client
	}
	return a.Client + ":" + a.Label
}

// Event is one line of the log.
type Event struct {
	V            int            `json:"v"`
	ID           string         `json:"id"`
	Seq          int64          `json:"seq"`
	TS           time.Time      `json:"ts"`
	App          string         `json:"app"`
	Kind         string         `json:"kind"`
	Outcome      string         `json:"outcome"`
	Release      string         `json:"release,omitempty"`
	Generation   uint64         `json:"generation,omitempty"`
	ConfigCommit string         `json:"config_commit,omitempty"`
	Actor        Actor          `json:"actor"`
	Detail       map[string]any `json:"detail,omitempty"`
}

// Kinds the CLI emits. Detail keys allowed per kind are listed in
// detailAllow; anything else is dropped before the line leaves the client.
const (
	KindDeploy         = "deploy"
	KindRollback       = "rollback"
	KindRestart        = "restart"
	KindStop           = "stop"
	KindStart          = "start"
	KindScale          = "scale"
	KindHeal           = "heal"
	KindEnvSet         = "env.set"
	KindEnvUnset       = "env.unset"
	KindSecretSet      = "secret.set"
	KindSecretUnset    = "secret.unset"
	KindConfigSet      = "config.set"
	KindConfigHome     = "config.home"
	KindRemove         = "remove"
	KindTriggerAdmit   = "trigger.admit"
	KindTriggerRefuse  = "trigger.refuse"
	KindPreviewDeploy  = "preview.deploy"
	KindPreviewDestroy = "preview.destroy"
	KindLock           = "lock"
	KindUnlock         = "unlock"
	KindMaintenanceOn  = "maintenance.on"
	KindMaintenanceOff = "maintenance.off"
	KindGateRepair     = "gate.repair"
	KindWatermarkReset = "watermark.reset"
	KindBackupRun      = "backup.run"
	KindBackupRestore  = "backup.restore"
	KindBackupTarget   = "backup_target.set"
)

// detailAllow is the per-kind allowlist of detail keys. Env and secret
// events carry key names only; nothing here may hold a value, a forge
// error body or a credential.
var detailAllow = map[string][]string{
	KindDeploy:         {"image", "previous", "error", "trigger", "duration_ms", "source_revision", "dirty", "unpushed"},
	KindRollback:       {"from", "to", "error", "duration_ms"},
	KindRestart:        {"process", "error"},
	KindStop:           {"process", "error"},
	KindStart:          {"process", "error"},
	KindScale:          {"process", "replicas", "error"},
	KindHeal:           {"container", "attempt", "error"},
	KindEnvSet:         {"keys"},
	KindEnvUnset:       {"keys"},
	KindSecretSet:      {"keys"},
	KindSecretUnset:    {"keys"},
	KindConfigSet:      {"privileged", "keys", "previous_commit"},
	KindConfigHome:     {"from", "to"},
	KindRemove:         {"purge", "redirect", "error"},
	KindTriggerAdmit:   {"ref", "commit", "delivery_id", "preview"},
	KindTriggerRefuse:  {"ref", "commit", "delivery_id", "reason"},
	KindPreviewDeploy:  {"preview", "branch", "error"},
	KindPreviewDestroy: {"preview", "error"},
	KindLock:           {"message"},
	KindUnlock:         {},
	KindMaintenanceOn:  {"error"},
	KindMaintenanceOff: {"error"},
	KindGateRepair:     {"case", "message"},
	KindWatermarkReset: {"ref"},
	KindBackupRun:      {"id", "kind", "accessory", "error"},
	KindBackupRestore:  {"id", "kind", "accessory", "error"},
	KindBackupTarget:   {"id", "kind", "accessory", "error"},
}

// maxErrorDetail bounds the "error" detail so a command's full stderr
// (which could echo anything) never lands in the log.
const maxErrorDetail = 300

// Sanitize drops detail keys the kind does not allow and bounds error text.
func Sanitize(ev *Event) {
	allowed := detailAllow[ev.Kind]
	if len(ev.Detail) == 0 {
		ev.Detail = nil
		return
	}
	out := map[string]any{}
	for _, k := range allowed {
		v, ok := ev.Detail[k]
		if !ok {
			continue
		}
		if k == "error" {
			s := fmt.Sprint(v)
			if len(s) > maxErrorDetail {
				s = s[:maxErrorDetail] + "..."
			}
			v = s
		}
		out[k] = v
	}
	if len(out) == 0 {
		out = nil
	}
	ev.Detail = out
}

// NewID returns a random event id (also the observe idempotency key).
func NewID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "ev_" + hex.EncodeToString(b[:])
}

// ErrOwnerMismatch is returned when /deployments/.teploy belongs to a
// different non-root user than the one connected.
var ErrOwnerMismatch = errors.New("teploy server data owned by another user")

// Append writes ev through the server helper and returns it with the
// server-assigned seq and ts. app must already be validated.
func Append(ctx context.Context, exec ssh.Executor, ev Event) (Event, error) {
	ev.V = 1
	if ev.ID == "" {
		ev.ID = NewID()
	}
	Sanitize(&ev)
	payload, err := json.Marshal(ev)
	if err != nil {
		return ev, err
	}
	res := ssh.RunInputDetailed(ctx, exec, "python3 -c "+ssh.ShellQuote(helper)+" append "+ssh.ShellQuote(ev.App), bytes.NewReader(payload))
	if res.Err != nil {
		return ev, res.Err
	}
	if res.ExitCode == 77 {
		return ev, fmt.Errorf("%w: %s; connect as that user or run `teploy server adopt-owner`", ErrOwnerMismatch, strings.TrimSpace(ownerFrom(res.Stderr)))
	}
	if res.ExitCode != 0 {
		return ev, fmt.Errorf("appending event: exit %d: %s", res.ExitCode, strings.TrimSpace(string(res.Stderr)))
	}
	var assigned struct {
		Seq int64     `json:"seq"`
		TS  time.Time `json:"ts"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(res.Stdout), &assigned); err != nil {
		return ev, fmt.Errorf("appending event: unreadable helper reply: %w", err)
	}
	ev.Seq, ev.TS = assigned.Seq, assigned.TS
	return ev, nil
}

func ownerFrom(stderr []byte) string {
	s := string(stderr)
	if i := strings.Index(s, "TEPLOY_OWNER_MISMATCH "); i >= 0 {
		return "this server's Teploy data is owned by " + strings.Fields(s[i+len("TEPLOY_OWNER_MISMATCH "):] + " ?")[0]
	}
	return s
}

// Read returns the app's events with seq > sinceSeq in seq order. An empty
// app reads every app on the server, ordered by (ts, app, seq).
func Read(ctx context.Context, exec ssh.Executor, app string, sinceSeq int64) ([]Event, error) {
	args := " read " + strconv.FormatInt(sinceSeq, 10)
	if app != "" {
		args += " " + ssh.ShellQuote(app)
	}
	res := ssh.RunDetailed(ctx, exec, "python3 -c "+ssh.ShellQuote(helper)+args)
	if res.Err != nil {
		return nil, res.Err
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("reading events: exit %d: %s", res.ExitCode, strings.TrimSpace(string(res.Stderr)))
	}
	var out []Event
	for _, line := range strings.Split(string(res.Stdout), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var ev Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue // a torn or foreign line is skipped, never fatal
		}
		out = append(out, ev)
	}
	if app == "" {
		sort.SliceStable(out, func(i, j int) bool {
			if !out[i].TS.Equal(out[j].TS) {
				return out[i].TS.Before(out[j].TS)
			}
			if out[i].App != out[j].App {
				return out[i].App < out[j].App
			}
			return out[i].Seq < out[j].Seq
		})
	}
	return out, nil
}

// helper is the single writer. Commands: `append <app>` (event JSON on
// stdin, prints {"seq","ts"}) and `read <since> [app]` (prints JSONL).
// Exit 77 = ownership mismatch.
const helper = `
import fcntl, json, os, pwd, sys, datetime
ROOT = os.environ.get("TEPLOY_EVENTS_ROOT_TEST") or "/deployments/.teploy"
MAX = int(os.environ.get("TEPLOY_EVENTS_MAX_TEST") or (1 << 20))
KEEP = 5

def owner_check():
    try:
        st = os.stat(ROOT)
    except FileNotFoundError:
        os.makedirs(ROOT, mode=0o700, exist_ok=True)
        os.chmod(ROOT, 0o700)
        return
    uid = os.getuid()
    if uid != 0 and st.st_uid != uid:
        try:
            name = pwd.getpwuid(st.st_uid).pw_name
        except KeyError:
            name = str(st.st_uid)
        sys.stderr.write("TEPLOY_OWNER_MISMATCH %s\n" % name)
        sys.exit(77)

def last_seq(d):
    for name in ["events.jsonl"] + ["events.jsonl.%d" % i for i in range(1, KEEP + 1)]:
        p = os.path.join(d, name)
        try:
            with open(p, "rb") as f:
                f.seek(0, 2)
                size = f.tell()
                f.seek(max(0, size - 65536))
                lines = [l for l in f.read().split(b"\n") if l.strip()]
        except FileNotFoundError:
            continue
        for l in reversed(lines):
            try:
                return int(json.loads(l)["seq"])
            except Exception:
                continue
    return 0

def rotate(d):
    for i in range(KEEP, 0, -1):
        src = os.path.join(d, "events.jsonl" + ("" if i == 1 else ".%d" % (i - 1)))
        dst = os.path.join(d, "events.jsonl.%d" % i)
        if os.path.exists(src):
            os.replace(src, dst)

def append(app):
    owner_check()
    apps = os.path.join(ROOT, "apps")
    os.makedirs(apps, mode=0o700, exist_ok=True)
    d = os.path.join(apps, app)
    os.makedirs(d, mode=0o700, exist_ok=True)
    ev = json.load(sys.stdin)
    lk = os.open(os.path.join(d, "events.lock"), os.O_RDWR | os.O_CREAT, 0o600)
    fcntl.flock(lk, fcntl.LOCK_EX)
    try:
        ev["seq"] = last_seq(d) + 1
        ev["ts"] = datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%S.%fZ")
        line = (json.dumps(ev, separators=(",", ":"), sort_keys=True) + "\n").encode()
        p = os.path.join(d, "events.jsonl")
        try:
            size = os.path.getsize(p)
        except FileNotFoundError:
            size = 0
        if size and size + len(line) > MAX:
            rotate(d)
        fd = os.open(p, os.O_WRONLY | os.O_APPEND | os.O_CREAT, 0o600)
        try:
            os.write(fd, line)
            os.fsync(fd)
        finally:
            os.close(fd)
        print(json.dumps({"seq": ev["seq"], "ts": ev["ts"]}))
    finally:
        fcntl.flock(lk, fcntl.LOCK_UN)
        os.close(lk)

def read(since, app):
    base = os.path.join(ROOT, "apps")
    apps = [app] if app else (sorted(os.listdir(base)) if os.path.isdir(base) else [])
    out = sys.stdout
    for a in apps:
        d = os.path.join(base, a)
        for name in ["events.jsonl.%d" % i for i in range(KEEP, 0, -1)] + ["events.jsonl"]:
            try:
                with open(os.path.join(d, name), "rb") as f:
                    for l in f:
                        try:
                            ev = json.loads(l)
                        except Exception:
                            continue
                        if int(ev.get("seq", 0)) > since:
                            out.write(json.dumps(ev, separators=(",", ":"), sort_keys=True) + "\n")
            except (FileNotFoundError, NotADirectoryError):
                continue

if sys.argv[1] == "append":
    append(sys.argv[2])
elif sys.argv[1] == "read":
    read(int(sys.argv[2]), sys.argv[3] if len(sys.argv) > 3 else "")
`

// processActor is the actor every event this process records carries,
// set once from the command line (--actor/--actor-op) before any command
// runs. One CLI invocation is one actor; the long-running listener sets
// its own.
var processActor = Actor{Client: "cli"}

// SetProcessActor sets the actor for events this process records.
func SetProcessActor(a Actor) { processActor = a }

// ProcessActor returns the actor set for this process.
func ProcessActor() Actor { return processActor }

// Warn receives the warning printed when an append fails.
var Warn io.Writer = os.Stderr

// Record appends ev with the process actor (unless ev carries one) and
// only warns on failure: history must never be why a command fails.
func Record(ctx context.Context, exec ssh.Executor, ev Event) Event {
	if exec == nil || ev.App == "" {
		return ev
	}
	if ev.Actor.Client == "" {
		ev.Actor = processActor
	}
	out, err := Append(ctx, exec, ev)
	if err != nil {
		fmt.Fprintf(Warn, "Warning: could not record the %s event for %s in the server history: %v\n", ev.Kind, ev.App, err)
	}
	return out
}

// Outcome maps a command error to the terminal outcome.
func Outcome(err error) string {
	if err != nil {
		return Failed
	}
	return Succeeded
}

// ErrDetail adds the bounded error text to detail when err is set.
func ErrDetail(detail map[string]any, err error) map[string]any {
	if err == nil {
		return detail
	}
	if detail == nil {
		detail = map[string]any{}
	}
	detail["error"] = err.Error()
	return detail
}
