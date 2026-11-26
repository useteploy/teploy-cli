package cli

// sandbox_harness_test.go — what the `teploy sandbox` tests run against.
// The SSH connection is a mock executor (the commands the server answers)
// carrying a forwarded connection that goes to an engine running
// in-process. So what is exercised is the whole road the CLI builds: the
// token read over SSH, the tunnel, the bearer token, the wire contract, and
// what the CLI prints for each of the engine's answers.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	gossh "golang.org/x/crypto/ssh"

	"github.com/useteploy/teploy/internal/sandbox"
	"github.com/useteploy/teploy/internal/ssh"
)

const (
	sandboxTestToken = "01TESTTOKEN0123456789ABCDEFGHJK01TESTTOKEN0123456789AB"
	sandboxTestHost  = "prod"
	// sandboxTestIDBase is the first 24 characters of every run id the fake
	// engine hands out; the last two count.
	sandboxTestIDBase = "01JCV5ZK3Q7X9B2D4E6F8A0B"
)

func sandboxTestID(n int) string { return fmt.Sprintf("%s%02d", sandboxTestIDBase, n) }

// sandboxProblem writes the engine's error shape.
func sandboxProblem(w http.ResponseWriter, status int, kind, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"type":"https://neutron.dev/errors/%s","title":"%s","status":%d,"detail":%q}`, kind, http.StatusText(status), status, detail)
}

// fakeSandboxEngine is a teploy-sandbox daemon in-process: its routes and
// its answers in its own wire shape (the sandbox.Run tags are the daemon's
// field names), and the knobs a test turns to make it misbehave.
type fakeSandboxEngine struct {
	mu      sync.Mutex
	token   string
	driver  string
	version string
	now     func() time.Time

	runs  []sandbox.Run
	files map[string][]byte // "<id>/<path>" -> content

	created   []sandbox.CreateRequest
	forked    []sandbox.ForkRequest
	execs     []sandbox.ExecRequest
	stopped   []string
	resumed   []string
	destroyed []string

	// loseCreateAnswer makes a create that may have made its run hang up
	// without a word: the lost answer the contract says never to retry.
	loseCreateAnswer bool
	// loseExecAnswer is the same for an exec: the command may be running
	// when the connection goes.
	loseExecAnswer bool
	// execHook, when set, answers every exec itself.
	execHook func(w http.ResponseWriter, req sandbox.ExecRequest)
}

func newFakeSandboxEngine() *fakeSandboxEngine {
	return &fakeSandboxEngine{token: sandboxTestToken, driver: "docker", version: "0.1.0", files: map[string][]byte{}, now: time.Now}
}

// sandboxProbeLines is the probe's answer in the engine's own key=value.
func sandboxProbeLines(systemd, unit, managed bool, state, version string, process bool) string {
	yesno := func(b bool) string {
		if b {
			return "yes"
		}
		return "no"
	}
	present := func(b bool) string {
		if b {
			return "present"
		}
		return "absent"
	}
	if version == "" {
		version = "absent"
	}
	running := "none"
	if process {
		running = "running"
	}
	return strings.Join([]string{
		"systemd=" + present(systemd),
		"unit=" + present(unit),
		"managed=" + yesno(managed),
		"state=" + state,
		"version=" + version,
		"process=" + running,
		"dropins=none",
	}, "\n")
}

// installedSandboxCommands answers a server where `teploy sandbox host`
// installed the engine and it is running.
func installedSandboxCommands(version string) []ssh.MockCommand {
	return []ssh.MockCommand{
		{Match: "if command -v systemctl", Output: sandboxProbeLines(true, true, true, "active", version, true)},
		{Match: "f=" + ssh.ShellQuote(sandboxTokenFile), Output: sandboxTestToken},
	}
}

// tokenCommand answers the token read for a token file of the caller's naming.
func tokenCommand(file, output string) ssh.MockCommand {
	return ssh.MockCommand{Match: "f=" + ssh.ShellQuote(file), Output: output}
}

func (e *fakeSandboxEngine) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "version": e.version})
			return
		}
		e.mu.Lock()
		defer e.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+e.token {
			sandboxProblem(w, http.StatusUnauthorized, "unauthorized", "Missing or invalid bearer token.")
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/capacity":
			running, stopped := 0, 0
			for _, run := range e.runs {
				if run.State == "running" {
					running++
				} else {
					stopped++
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"server": "sandbox-1", "driver": e.driver, "runtime": "default",
				"cpus": 4, "memoryMb": 7936, "runs": map[string]int{"running": running, "stopped": stopped},
				"allocated": map[string]any{"cpus": 1, "memoryMb": 1024}})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/runs":
			_ = json.NewEncoder(w).Encode(map[string]any{"server": "sandbox-1", "runs": e.runs})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/runs":
			var req sandbox.CreateRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			e.created = append(e.created, req)
			run := e.newRun(req.Image)
			run.Network, run.Labels, run.OnExpire = req.Network, req.Labels, req.OnExpire
			if run.Network == "" {
				run.Network = "none"
			}
			if run.OnExpire == "" {
				run.OnExpire = "destroy"
			}
			if req.User == "root" {
				run.User = "root"
			}
			run.ExpiresAt = e.now().Add(time.Duration(req.TTLSec) * time.Second)
			if req.TTLSec == 0 {
				run.ExpiresAt = e.now().Add(30 * time.Minute)
			}
			e.runs = append(e.runs, run)
			if e.loseCreateAnswer {
				conn, _, err := w.(http.Hijacker).Hijack()
				if err == nil {
					conn.Close()
					return
				}
			}
			e.writeRun(w, http.StatusCreated, run)
		case strings.HasPrefix(r.URL.Path, "/v1/runs/"):
			e.runRoute(w, r, strings.TrimPrefix(r.URL.Path, "/v1/runs/"))
		default:
			sandboxProblem(w, http.StatusNotFound, "not-found", "no such route")
		}
	})
}

func (e *fakeSandboxEngine) newRun(image string) sandbox.Run {
	return sandbox.Run{ID: sandboxTestID(len(e.runs)), Image: image, User: "ship", State: "running", CreatedAt: e.now(), ExpiresAt: e.now().Add(30 * time.Minute), OnExpire: "destroy"}
}

// writeRun is the daemon's own: the run's JSON, with the server beside it.
func (e *fakeSandboxEngine) writeRun(w http.ResponseWriter, status int, run sandbox.Run) {
	raw, _ := json.Marshal(run)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	body["server"] = "sandbox-1"
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func (e *fakeSandboxEngine) runRoute(w http.ResponseWriter, r *http.Request, rest string) {
	id, action, _ := strings.Cut(rest, "/")
	at := -1
	for i, run := range e.runs {
		if run.ID == id {
			at = i
		}
	}
	if at < 0 {
		sandboxProblem(w, http.StatusNotFound, "not-found", "no such run: "+id)
		return
	}
	switch {
	case r.Method == http.MethodPost && action == "stop":
		if e.runs[at].State == "stopped" {
			sandboxProblem(w, http.StatusConflict, "conflict", "run "+id+" is already stopped")
			return
		}
		stopped, retain := e.now(), e.now().Add(14*24*time.Hour)
		e.runs[at].State, e.runs[at].StoppedAt, e.runs[at].RetainUntil = "stopped", &stopped, &retain
		e.stopped = append(e.stopped, id)
		e.writeRun(w, http.StatusOK, e.runs[at])
	case r.Method == http.MethodPost && action == "resume":
		if e.runs[at].State == "running" {
			sandboxProblem(w, http.StatusConflict, "conflict", "run "+id+" is already running")
			return
		}
		var req sandbox.ResumeRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		ttl := time.Duration(req.TTLSec) * time.Second
		if ttl == 0 {
			ttl = 30 * time.Minute
		}
		e.runs[at].State, e.runs[at].ExpiresAt = "running", e.now().Add(ttl)
		e.runs[at].StoppedAt, e.runs[at].RetainUntil = nil, nil
		e.resumed = append(e.resumed, id)
		e.writeRun(w, http.StatusOK, e.runs[at])
	case r.Method == http.MethodPost && action == "fork":
		var req sandbox.ForkRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		e.forked = append(e.forked, req)
		run := e.newRun(e.runs[at].Image)
		run.Network, run.User = e.runs[at].Network, e.runs[at].User
		if req.Labels == nil {
			run.Labels = e.runs[at].Labels
		} else {
			run.Labels = req.Labels
		}
		run.ForkedFrom = e.runs[at].ID
		e.runs = append(e.runs, run)
		e.writeRun(w, http.StatusCreated, run)
	case r.Method == http.MethodDelete && action == "":
		e.runs = append(e.runs[:at], e.runs[at+1:]...)
		e.destroyed = append(e.destroyed, id)
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPost && action == "exec":
		if e.runs[at].State == "stopped" {
			sandboxProblem(w, http.StatusConflict, "conflict", "run "+id+" is stopped; resume it first")
			return
		}
		var req sandbox.ExecRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.OutputEncoding != "" && req.OutputEncoding != "base64" {
			sandboxProblem(w, http.StatusBadRequest, "bad-request", "outputEncoding must be base64 or omitted.")
			return
		}
		e.execs = append(e.execs, req)
		if e.loseExecAnswer {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
				return
			}
		}
		// The stream is written outside the lock, as the daemon streams it.
		e.mu.Unlock()
		defer func() { e.mu.Lock() }()
		if e.execHook != nil {
			e.execHook(w, req)
			return
		}
		sandboxStreamLocalCommand(w, req)
	case strings.HasPrefix(action, "files/") && r.Method == http.MethodPut:
		content, _ := io.ReadAll(r.Body)
		e.files[id+"/"+strings.TrimPrefix(action, "files/")] = content
		w.WriteHeader(http.StatusNoContent)
	case strings.HasPrefix(action, "files/") && r.Method == http.MethodGet:
		content, ok := e.files[id+"/"+strings.TrimPrefix(action, "files/")]
		if !ok {
			sandboxProblem(w, http.StatusNotFound, "not-found", "No such file")
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(content)
	default:
		sandboxProblem(w, http.StatusNotFound, "not-found", "no such route")
	}
}

// sandboxStreamLocalCommand runs the command here, through sh -c as the
// daemon does, and frames its real output and exit as the daemon frames
// them: one frame a read, a `data:` line a line of it, exit last. Asked for
// base64, every chunk is one base64 line, as the daemon sends it.
func sandboxStreamLocalCommand(w http.ResponseWriter, req sandbox.ExecRequest) {
	cmd := exec.Command("sh", "-c", req.Cmd)
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	base64Mode := req.OutputEncoding == "base64"
	var mu sync.Mutex
	frame := func(event, data string) {
		mu.Lock()
		defer mu.Unlock()
		sandboxFrame(w, event, data, base64Mode)
	}
	var wg sync.WaitGroup
	for stream, event := range map[io.Reader]string{stdout: "stdout", stderr: "stderr"} {
		wg.Add(1)
		go func(reader io.Reader, event string) {
			defer wg.Done()
			buffer := make([]byte, 4096)
			for {
				n, err := reader.Read(buffer)
				if n > 0 {
					frame(event, string(buffer[:n]))
				}
				if err != nil {
					return
				}
			}
		}(stream, event)
	}
	wg.Wait()
	code := 0
	if err := cmd.Wait(); err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			code = exit.ExitCode()
		}
	}
	sandboxFrame(w, "exit", fmt.Sprintf(`{"exitCode":%d,"timedOut":false}`, code), false)
}

// sandboxFrame writes one frame as the daemon writes it: the event, one
// `data:` line a line of the chunk (in base64 a chunk is one line by
// construction), a blank line, then a flush. The exit frame is JSON in
// either encoding.
func sandboxFrame(w http.ResponseWriter, event, data string, base64Mode bool) {
	fmt.Fprintf(w, "event: %s\n", event)
	if base64Mode {
		fmt.Fprintf(w, "data: %s\n", base64.StdEncoding.EncodeToString([]byte(data)))
	} else {
		for _, line := range strings.Split(strings.TrimSuffix(data, "\n"), "\n") {
			fmt.Fprintf(w, "data: %s\n", line)
		}
		if strings.HasSuffix(data, "\n") {
			fmt.Fprint(w, "data: \n")
		}
	}
	fmt.Fprint(w, "\n")
	w.(http.Flusher).Flush()
}

// engineState reads or writes the fake engine's state under its lock: a
// request's goroutine writes it, the test's reads it.
func engineState[T any](e *fakeSandboxEngine, visit func() T) T {
	e.mu.Lock()
	defer e.mu.Unlock()
	return visit()
}

// withRun adds a run to the engine, running or stopped.
func withRun(h *sandboxHarness, state string) string {
	run := sandbox.Run{ID: sandboxTestID(len(h.engine.runs)), Image: "alpine:3.20", Network: "none", User: "ship", State: "running", CreatedAt: h.clock, ExpiresAt: h.clock.Add(30 * time.Minute), OnExpire: "destroy"}
	if state == "stopped" {
		stopped, retain := h.clock, h.clock.Add(14*24*time.Hour)
		run.State, run.StoppedAt, run.RetainUntil = "stopped", &stopped, &retain
	}
	return engineState(h.engine, func() string {
		h.engine.runs = append(h.engine.runs, run)
		return run.ID
	})
}

// sandboxSSE answers an exec with frames written as the engine writes
// them, in the encoding the exec asked for: output frames as the daemon
// frames that encoding, the exit frame always as its JSON.
func sandboxSSE(w http.ResponseWriter, req sandbox.ExecRequest, frames ...[2]string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	for _, frame := range frames {
		if frame[0] == "stdout" || frame[0] == "stderr" {
			sandboxFrame(w, frame[0], frame[1], req.OutputEncoding == "base64")
			continue
		}
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", frame[0], frame[1])
	}
}

// fakeSandboxLink is the SSH connection to a server in a sandbox test: the
// commands it answers come from a mock executor, and a forwarded connection
// goes to the test's engine — or is refused the way sshd refuses one.
type fakeSandboxLink struct {
	*ssh.MockExecutor
	engineAddr string
	// tunnel is "" for one that reaches the engine, "refused" when nothing
	// listens at the address, "prohibited" when sshd forbids forwarding.
	tunnel string
	user   string
	// beforeRun, when set, sees every command before it is answered: a test
	// makes the server change state as a command "runs".
	beforeRun func(cmd string)

	mu     sync.Mutex
	closed bool
	dials  int
}

func (l *fakeSandboxLink) Run(ctx context.Context, cmd string) (string, error) {
	if l.beforeRun != nil {
		l.beforeRun(cmd)
	}
	return l.MockExecutor.Run(ctx, cmd)
}

// setTunnel changes what a forwarded connection does from now on.
func (l *fakeSandboxLink) setTunnel(tunnel string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.tunnel = tunnel
}

func (l *fakeSandboxLink) ForwardDial(ctx context.Context, addr string) (net.Conn, error) {
	l.mu.Lock()
	l.dials++
	tunnel := l.tunnel
	l.mu.Unlock()
	switch tunnel {
	case "refused":
		return nil, &gossh.OpenChannelError{Reason: gossh.ConnectionFailed, Message: "connect failed"}
	case "prohibited":
		return nil, &gossh.OpenChannelError{Reason: gossh.Prohibited, Message: "administratively prohibited"}
	}
	return (&net.Dialer{}).DialContext(ctx, "tcp", l.engineAddr)
}

func (l *fakeSandboxLink) User() string {
	if l.user != "" {
		return l.user
	}
	return "root"
}

func (l *fakeSandboxLink) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closed = true
	return nil
}

func (l *fakeSandboxLink) isClosed() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closed
}

// callsWith are the commands the link answered that hold part.
func (l *fakeSandboxLink) callsWith(part string) []string {
	var found []string
	for _, call := range l.Calls {
		if strings.Contains(call, part) {
			found = append(found, call)
		}
	}
	return found
}

// sandboxHarness drives the sandbox commands with every side effect in
// memory: output, prompts, the connection, the clock.
type sandboxHarness struct {
	t           *testing.T
	engine      *fakeSandboxEngine
	link        *fakeSandboxLink
	out, errOut *bytes.Buffer
	flags       *Flags
	env         *sandboxEnv
	prompts     []string
	answer      bool
	connected   []string
	clock       time.Time
}

// newSandboxHarness starts an engine in-process and a link to a server
// that answers the given commands.
func newSandboxHarness(t *testing.T, commands ...ssh.MockCommand) *sandboxHarness {
	t.Helper()
	t.Setenv("HOME", t.TempDir()) // no servers.yml of the machine's own
	t.Setenv("TEPLOY_SSH_KEY", "")
	engine := newFakeSandboxEngine()
	server := httptest.NewServer(engine.handler())
	t.Cleanup(server.Close)
	link := &fakeSandboxLink{MockExecutor: ssh.NewMockExecutor(sandboxTestHost, commands...), engineAddr: server.Listener.Addr().String()}
	h := &sandboxHarness{
		t: t, engine: engine, link: link,
		out: &bytes.Buffer{}, errOut: &bytes.Buffer{}, flags: &Flags{Host: sandboxTestHost},
		clock: time.Date(2026, 11, 25, 16, 0, 0, 0, time.UTC),
	}
	engine.now = func() time.Time { return h.clock }
	h.env = &sandboxEnv{
		out:     h.out,
		errOut:  h.errOut,
		confirm: func(prompt string) bool { h.prompts = append(h.prompts, prompt); return h.answer },
		connect: func(ctx context.Context, host, user, key string) (ssh.Forwarder, error) {
			h.connected = append(h.connected, user+"@"+host)
			return link, nil
		},
		sleep: func(ctx context.Context, d time.Duration) error { h.clock = h.clock.Add(d); return ctx.Err() },
		fetch: func(ctx context.Context, url string) ([]byte, error) {
			return nil, fmt.Errorf("no network in a test: %s", url)
		},
		newRunLabel:  func() (string, error) { return "3f9a1c0b7d2e", nil },
		pollInterval: time.Second,
		startTimeout: 30 * time.Second,
		now:          func() time.Time { return h.clock },
	}
	return h
}

// nameServer gives the server its entry in servers.yml, so documents name
// a host rather than the raw word the command was given.
func (h *sandboxHarness) nameServer(name, host, user string) {
	h.t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		h.t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".teploy"), 0o755); err != nil {
		h.t.Fatal(err)
	}
	servers := "servers:\n  " + name + ":\n    host: " + host + "\n    user: " + user + "\n"
	if err := os.WriteFile(filepath.Join(home, ".teploy", "servers.yml"), []byte(servers), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

// installed is a harness for a server where `teploy sandbox host` installed
// the engine and it is running.
func newInstalledSandboxHarness(t *testing.T) *sandboxHarness {
	t.Helper()
	return newSandboxHarness(t, installedSandboxCommands("0.1.0")...)
}

func (h *sandboxHarness) run(args ...string) error {
	h.t.Helper()
	cmd := newSandboxCmdWithEnv(h.flags, h.env)
	cmd.SetArgs(args)
	cmd.SetOut(h.out)
	cmd.SetErr(h.errOut)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	return cmd.Execute()
}

// json runs with --json and the server named, and decodes stdout.
func (h *sandboxHarness) json(args ...string) (map[string]any, error) {
	h.t.Helper()
	h.flags.JSON = true
	h.flags.Host = sandboxTestHost
	defer func() { h.flags.JSON = false }()
	if err := h.run(args...); err != nil {
		return nil, err
	}
	return h.jsonDocument(), nil
}

// jsonDocument decodes stdout as one JSON document.
func (h *sandboxHarness) jsonDocument() map[string]any {
	h.t.Helper()
	var document map[string]any
	if err := json.Unmarshal(h.out.Bytes(), &document); err != nil {
		h.t.Fatalf("stdout is not one JSON document: %v\n%s", err, h.out.String())
	}
	return document
}

// createdJSON is a create body as the engine received it, in the wire's
// own field names.
func (e *fakeSandboxEngine) createdJSON(t *testing.T, n int) string {
	t.Helper()
	return engineState(e, func() string {
		raw, err := json.Marshal(e.created[n])
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	})
}

// execCount is how many execs the engine has run.
func (e *fakeSandboxEngine) execCount() int {
	return engineState(e, func() int { return len(e.execs) })
}

// setExecHook makes the engine answer every exec itself: with the request
// it was asked, so a test can answer in the encoding the CLI asked for.
func (e *fakeSandboxEngine) setExecHook(hook func(w http.ResponseWriter, req sandbox.ExecRequest)) {
	engineState(e, func() bool { e.execHook = hook; return true })
}

// file is what a sandbox holds at path.
func (e *fakeSandboxEngine) file(id, path string) ([]byte, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	content, ok := e.files[id+"/"+path]
	return content, ok
}
