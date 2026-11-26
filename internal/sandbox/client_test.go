package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testToken = "01TESTTOKEN0123456789ABCDEFGHJK01TESTTOKEN0123456789AB"
	testID    = "01JCV5ZK3Q7X9B2D4E6F8A0B1C"
	otherID   = "01JCV5ZK3Q7X9B2D4E6F8A0B1D"
)

// A run as the engine writes it (the daemon's own field names), running.
const runningRun = `{"id":"` + testID + `","image":"alpine:3.20","network":"none","user":"ship","createdAt":"2026-11-25T16:00:00Z","expiresAt":"2026-11-25T16:30:00Z","state":"running","onExpire":"destroy","usage":{"runningSec":12}}`

// A stopped run: the fields that only a stopped one carries, and the
// sampled usage a running one has once the engine has sampled it.
const stoppedRun = `{"id":"` + testID + `","image":"alpine:3.20","network":"allowlist","user":"root","createdAt":"2026-11-25T16:00:00Z","expiresAt":"2026-11-25T16:30:00Z","state":"stopped","onExpire":"destroy","labels":{"team":"x"},"forkedFrom":"` + otherID + `","usage":{"runningSec":12,"cpuSeconds":1.5,"rxBytes":10,"txBytes":20,"diskBytes":30,"sampledAt":"2026-11-25T16:00:12Z"},"stoppedAt":"2026-11-25T16:05:00Z","retainUntil":"2026-12-09T16:05:00Z"}`

type call struct {
	method, path, rawPath string
	auth, contentType     string
	body                  []byte
}

// recorder keeps what reached the engine, so a test can say what was sent,
// how often, and with which headers.
type recorder struct {
	mu    sync.Mutex
	calls []call
}

func (r *recorder) wrap(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		// rawPath is the path as it travelled: URL.Path is already decoded.
		r.calls = append(r.calls, call{method: req.Method, path: req.URL.Path, rawPath: req.URL.EscapedPath(), auth: req.Header.Get("Authorization"), contentType: req.Header.Get("Content-Type"), body: body})
		r.mu.Unlock()
		next(w, req)
	}
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func (r *recorder) at(t *testing.T, i int) call {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if i >= len(r.calls) {
		t.Fatalf("call %d never reached the engine (%d did)", i, len(r.calls))
	}
	return r.calls[i]
}

func newTestClient(t *testing.T, rec *recorder, next http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(rec.wrap(next))
	t.Cleanup(server.Close)
	client, err := New(server.Listener.Addr().String(), testToken, nil)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// problem writes the engine's error shape.
func problem(w http.ResponseWriter, status int, kind, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"type":"https://neutron.dev/errors/%s","title":"%s","status":%d,"detail":%q}`, kind, http.StatusText(status), status, detail)
}

// sseFrame writes one frame exactly as the engine does: the event, one
// `data:` line per line of the chunk, a blank line, then a flush.
func sseFrame(w http.ResponseWriter, event, data string) {
	fmt.Fprintf(w, "event: %s\n", event)
	for _, line := range strings.Split(data, "\n") {
		fmt.Fprintf(w, "data: %s\n", line)
	}
	io.WriteString(w, "\n")
	w.(http.Flusher).Flush()
}

func startStream(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
}

// guarded answers every route but /health with 401 unless the token is right.
func guarded(w http.ResponseWriter, req *http.Request) {
	if req.URL.Path != "/health" && req.Header.Get("Authorization") != "Bearer "+testToken {
		problem(w, http.StatusUnauthorized, "unauthorized", "Missing or invalid bearer token.")
	}
}

func TestHealthNeedsNoToken(t *testing.T) {
	rec := &recorder{}
	client := newTestClient(t, rec, func(w http.ResponseWriter, req *http.Request) {
		if auth := req.Header.Get("Authorization"); auth != "" {
			t.Errorf("GET /health carried %q; the one route without a token", auth)
		}
		io.WriteString(w, `{"status":"ok","version":"0.1.0"}`)
	})
	health, err := client.Health(context.Background())
	if err != nil || health.Status != "ok" || health.Version != "0.1.0" {
		t.Fatalf("Health = %+v, %v", health, err)
	}
}

// Every route but /health needs the token, and a wrong one is a 401 problem
// on each of them.
func TestEveryOtherRouteNeedsTheToken(t *testing.T) {
	rec := &recorder{}
	client := newTestClient(t, rec, guarded)
	wrong, err := New(client.Addr(), "not-the-token", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	check := func(what string, err error) {
		t.Helper()
		if !IsUnauthorized(err) {
			t.Errorf("%s with a wrong token: %v, want a 401 problem", what, err)
		}
	}
	_, err = wrong.Capacity(ctx)
	check("Capacity", err)
	_, err = wrong.List(ctx)
	check("List", err)
	_, err = wrong.Create(ctx, CreateRequest{Image: "alpine"})
	check("Create", err)
	_, err = wrong.Find(ctx, testID)
	check("Find", err)
	_, err = wrong.Stop(ctx, testID)
	check("Stop", err)
	_, err = wrong.Resume(ctx, testID, ResumeRequest{})
	check("Resume", err)
	_, err = wrong.Fork(ctx, testID, ForkRequest{})
	check("Fork", err)
	check("Destroy", wrong.Destroy(ctx, testID))
	_, err = wrong.Exec(ctx, testID, ExecRequest{Cmd: "true"}, func(Stream, string) {})
	check("Exec", err)
	check("PutFile", wrong.PutFile(ctx, testID, "a.txt", []byte("a")))
	_, err = wrong.GetFile(ctx, testID, "a.txt")
	check("GetFile", err)

	// The token reached every one of them as a bearer, and nowhere else.
	for i := 0; i < rec.count(); i++ {
		if got := rec.at(t, i).auth; got != "Bearer not-the-token" {
			t.Errorf("request %d carried %q", i, got)
		}
		if strings.Contains(string(rec.at(t, i).body), "not-the-token") {
			t.Errorf("request %d put the token in its body", i)
		}
	}
	// A problem never carries the token either.
	var apiErr *APIError
	if !errors.As(err, &apiErr) || strings.Contains(apiErr.Error(), "not-the-token") {
		t.Errorf("the 401 problem names the token: %v", err)
	}
}

func TestCapacityReadsTheDriver(t *testing.T) {
	client := newTestClient(t, &recorder{}, func(w http.ResponseWriter, req *http.Request) {
		guarded(w, req)
		if req.URL.Path != "/v1/capacity" {
			http.NotFound(w, req)
			return
		}
		io.WriteString(w, `{"server":"sandbox-1","driver":"docker","runtime":"default","nestedVirt":false,"cpus":4,"memoryMb":7936,"runs":{"running":1,"stopped":0},"allocated":{"cpus":1,"memoryMb":1024},"warmPool":{"sizes":[]},"access":{"enabled":true},"disk":{"kind":"docker-data-dir"}}`)
	})
	capacity, err := client.Capacity(context.Background())
	if err != nil || capacity.Driver != "docker" || capacity.Server != "sandbox-1" {
		t.Fatalf("Capacity = %+v, %v", capacity, err)
	}
}

func TestCreateSendsTheWireBodysFieldNames(t *testing.T) {
	rec := &recorder{}
	client := newTestClient(t, rec, func(w http.ResponseWriter, req *http.Request) {
		guarded(w, req)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"server":"sandbox-1",`+strings.TrimPrefix(runningRun, "{"))
	})
	ctx := context.Background()
	_, err := client.Create(ctx, CreateRequest{
		Image: "alpine:3.20", Env: map[string]string{"A": "b"}, TTLSec: 600, Network: "allowlist",
		Limits: &Limits{MemoryMB: 512, CPUs: 1.5, Pids: 64}, User: "root", Labels: map[string]string{"team": "x"}, OnExpire: "stop",
	})
	if err != nil {
		t.Fatal(err)
	}
	sent := rec.at(t, 0)
	if sent.method != http.MethodPost || sent.path != "/v1/runs" || sent.contentType != "application/json" {
		t.Fatalf("request %+v", sent)
		want := `{"image":"alpine:3.20","env":{"A":"b"},"ttlSec":600,"network":"allowlist","limits":{"memoryMb":512,"cpus":1.5,"pids":64},"user":"root","labels":{"team":"x"},"onExpire":"stop"}`
		if string(sent.body) != want {
			t.Fatalf("body %s, want %s", sent.body, want)
		}
	}

	// What is not given is left out, so the engine's own defaults apply.
	run, err := client.Create(ctx, CreateRequest{Image: "alpine:3.20"})
	if err != nil {
		t.Fatal(err)
	}
	if body := string(rec.at(t, 1).body); body != `{"image":"alpine:3.20"}` {
		t.Errorf("a minimal create sent %s", body)
	}
	if run.ID != testID || run.Server != "sandbox-1" {
		t.Errorf("Create returned %+v", run)
	}
}

// The optional fields are nil when the engine leaves them out, and what the
// engine said when it does not: absent is not zero.
func TestRunKeepsAbsentFieldsAbsent(t *testing.T) {
	client := newTestClient(t, &recorder{}, func(w http.ResponseWriter, req *http.Request) {
		guarded(w, req)
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"server":"sandbox-1",`+strings.TrimPrefix(runningRun, "{"))
	})
	run, err := client.Create(context.Background(), CreateRequest{Image: "alpine:3.20"})
	if err != nil {
		t.Fatal(err)
	}
	if run.Labels != nil || run.StoppedAt != nil || run.RetainUntil != nil || run.ForkedFrom != "" || run.Lease != nil {
		t.Errorf("a running run carries stopped-run fields: %+v", run)
	}
	if run.Usage.RunningSec != 12 || run.Usage.CPUSeconds != nil || run.Usage.RxBytes != nil || run.Usage.TxBytes != nil || run.Usage.DiskBytes != nil || run.Usage.SampledAt != nil {
		t.Errorf("unsampled usage read as numbers: %+v", run.Usage)
	}
	if run.CreatedAt.Year() != 2026 || !run.ExpiresAt.Equal(run.CreatedAt.Add(30*time.Minute)) {
		t.Errorf("times read wrong: %+v", run)
	}

	client = newTestClient(t, &recorder{}, func(w http.ResponseWriter, req *http.Request) {
		guarded(w, req)
		io.WriteString(w, `{"server":"sandbox-1","runs":[`+stoppedRun+`]}`)
	})
	found, err := client.Find(context.Background(), testID)
	if err != nil {
		t.Fatal(err)
	}
	if found.Server != "" || found.State != "stopped" || found.Labels["team"] != "x" || found.ForkedFrom != otherID {
		t.Errorf("stopped run read wrong: %+v", found)
	}
	if found.StoppedAt == nil || found.RetainUntil == nil || found.RetainUntil.Sub(*found.StoppedAt) != 14*24*time.Hour {
		t.Errorf("the stopped run's times: %+v %+v", found.StoppedAt, found.RetainUntil)
	}
	if *found.Usage.CPUSeconds != 1.5 || *found.Usage.RxBytes != 10 || *found.Usage.TxBytes != 20 || *found.Usage.DiskBytes != 30 || found.Usage.SampledAt == nil {
		t.Errorf("sampled usage: %+v", found.Usage)
	}
}

func TestListAndFind(t *testing.T) {
	rec := &recorder{}
	client := newTestClient(t, rec, func(w http.ResponseWriter, req *http.Request) {
		guarded(w, req)
		io.WriteString(w, `{"server":"sandbox-1","runs":[`+runningRun+`,`+stoppedRun+`]}`)
	})
	runs, err := client.List(context.Background())
	if err != nil || len(runs) != 2 || runs[0].ID != testID || runs[1].State != "stopped" {
		t.Fatalf("List = %+v, %v", runs, err)
	}
	if rec.at(t, 0).method != http.MethodGet || rec.at(t, 0).path != "/v1/runs" {
		t.Fatalf("List sent %+v", rec.at(t, 0))
	}
	if _, err := client.Find(context.Background(), otherID); !IsNotFound(err) {
		t.Errorf("a run that is not there: %v, want a 404 problem", err)
	}
	// An id is accepted in either case.
	if _, err := client.Find(context.Background(), strings.ToLower(testID)); err != nil {
		t.Errorf("a lower-case id: %v", err)
	}
	if got := rec.at(t, 2).path; got != "/v1/runs" {
		t.Errorf("Find read %s", got)
	}
}

func TestLifecycleRoutes(t *testing.T) {
	rec := &recorder{}
	client := newTestClient(t, rec, func(w http.ResponseWriter, req *http.Request) {
		guarded(w, req)
		switch req.URL.Path + " " + req.Method {
		case "/v1/runs/" + testID + "/stop POST", "/v1/runs/" + testID + "/resume POST":
			io.WriteString(w, `{"server":"sandbox-1",`+strings.TrimPrefix(stoppedRun, "{"))
		case "/v1/runs/" + testID + "/fork POST":
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"server":"sandbox-1",`+strings.TrimPrefix(runningRun, "{"))
		case "/v1/runs/" + testID + " DELETE":
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected %s %s", req.Method, req.URL.Path)
			http.NotFound(w, req)
		}
	})
	ctx := context.Background()
	if run, err := client.Stop(ctx, testID); err != nil || run.State != "stopped" {
		t.Errorf("Stop = %+v, %v", run, err)
	}
	if run, err := client.Resume(ctx, testID, ResumeRequest{TTLSec: 900}); err != nil || run.ID != testID {
		t.Errorf("Resume = %+v, %v", run, err)
	}
	if run, err := client.Fork(ctx, testID, ForkRequest{TTLSec: 300, Labels: map[string]string{"a": "b"}}); err != nil || run.ID != testID {
		t.Errorf("Fork = %+v, %v", run, err)
	}
	if err := client.Destroy(ctx, testID); err != nil {
		t.Errorf("Destroy: %v", err)
	}
	// A lower-case id travels upper-case: the id is a ULID.
	if err := client.Destroy(ctx, strings.ToLower(testID)); err != nil {
		t.Errorf("Destroy with a lower-case id: %v", err)
	}
	want := []struct{ method, path, body string }{
		{http.MethodPost, "/v1/runs/" + testID + "/stop", "{}"},
		{http.MethodPost, "/v1/runs/" + testID + "/resume", `{"ttlSec":900}`},
		{http.MethodPost, "/v1/runs/" + testID + "/fork", `{"ttlSec":300,"labels":{"a":"b"}}`},
		{http.MethodDelete, "/v1/runs/" + testID, ""},
		{http.MethodDelete, "/v1/runs/" + testID, ""},
	}
	for i, w := range want {
		got := rec.at(t, i)
		if got.method != w.method || got.path != w.path || string(got.body) != w.body {
			t.Errorf("request %d = %s %s %q, want %s %s %q", i, got.method, got.path, got.body, w.method, w.path, w.body)
		}
	}
}

// A refusal is the engine's problem: status and kind are the contract, the
// detail is its sentence. Only a refusal has a known outcome; a 500 does
// not ("mutations only after GET shows they did not happen").
func TestRefusalsAreProblems(t *testing.T) {
	cases := []struct {
		status, kind, detail string
		notFound, conflict   bool
		known                bool
	}{
		{"404", "not-found", "no such run", true, false, true},
		{"409", "conflict", "the run is stopped", false, true, true},
		{"400", "bad-request", "labels: at most 16", false, false, true},
		{"503", "at-capacity", "the daemon is full", false, false, true},
		{"408", "request-timeout", "slow", false, false, false},
		{"500", "internal", "docker: broke", false, false, false},
	}
	for _, c := range cases {
		status, _ := strconv.Atoi(c.status)
		client := newTestClient(t, &recorder{}, func(w http.ResponseWriter, req *http.Request) {
			problem(w, status, c.kind, c.detail)
		})
		_, err := client.Create(context.Background(), CreateRequest{Image: "alpine"})
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.Status != status || apiErr.Kind != c.kind || apiErr.Detail != c.detail {
			t.Errorf("%s: got %v, want a %s %s problem", c.status, err, c.status, c.kind)
			continue
		}
		if IsNotFound(err) != c.notFound || IsConflict(err) != c.conflict {
			t.Errorf("%s: the predicates disagree with the problem: %v", c.status, err)
		}
		if OutcomeKnown(err) != c.known {
			t.Errorf("%s: OutcomeKnown = %v, want %v", c.status, !c.known, c.known)
		}
		if want := fmt.Sprintf("the sandbox engine answered %d %s: %s", status, http.StatusText(status), c.detail); apiErr.Error() != want {
			t.Errorf("%s: Error() = %q, want %q", c.status, apiErr.Error(), want)
		}
	}
}

// An answer that is not a problem (a proxy, another service on the port)
// is neither parsed as one nor given a known outcome.
func TestAnswerThatIsNoProblem(t *testing.T) {
	client := newTestClient(t, &recorder{}, func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		io.WriteString(w, "<html>\n  <body>Bad gateway</body>\n</html>")
	})
	_, err := client.Create(context.Background(), CreateRequest{Image: "alpine"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusBadGateway || apiErr.Kind != "unknown" || apiErr.Detail != "<html> <body>Bad gateway</body> </html>" {
		t.Fatalf("got %+v, %v", apiErr, err)
	}
	if OutcomeKnown(err) {
		t.Error("a reply that is not the engine's problem was given a known outcome")
	}
}

// A create whose answer is lost is not sent again, and its outcome is
// unknown: the engine may have made the run.
func TestCreateIsSentOnceWhenItsAnswerIsLost(t *testing.T) {
	rec := &recorder{}
	client := newTestClient(t, rec, func(w http.ResponseWriter, req *http.Request) {
		io.ReadAll(req.Body)
		// Take the connection and hang up without a word.
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijacking: %v", err)
			return
		}
		conn.Close()
	})
	_, err := client.Create(context.Background(), CreateRequest{Image: "alpine"})
	if err == nil {
		t.Fatal("a lost answer read as success")
	}
	if got := rec.count(); got != 1 {
		t.Fatalf("the create reached the engine %d times; a create is never sent twice", got)
	}
	if OutcomeKnown(err) {
		t.Error("a create whose answer was lost was given a known outcome")
	}
	// The error names what was asked, never what would authorize it.
	if strings.Contains(err.Error(), testToken) {
		t.Errorf("the error names the token: %v", err)
	}
}

func TestOutcomeKnown(t *testing.T) {
	if !OutcomeKnown(nil) || !OutcomeKnown(notSent(errors.New("bad id"))) {
		t.Error("a request that was not sent, and no failure at all, have a known outcome")
	}
	if OutcomeKnown(fmt.Errorf("POST /v1/runs: %w", context.DeadlineExceeded)) || OutcomeKnown(context.Canceled) {
		t.Error("a wait that ran out, or a cancel, has an unknown outcome")
	}
	if OutcomeKnown(errors.New("Post \"http://127.0.0.1:7439/v1/runs\": EOF")) {
		t.Error("no answer has an unknown outcome")
	}
}

// An id is put in a URL path, so it is checked before it travels: nothing
// that is not a ULID is sent anywhere.
func TestIDsAreCheckedBeforeTheyTravel(t *testing.T) {
	for _, id := range []string{"", "abc", "../v1/runs", "01JCV5ZK3Q7X9B2D4E6F8A0B1", testID + "/exec", testID + "?x=1", "01JCV5ZK3Q7X9B2D4E6F8A0BI", "not-an-id"} {
		rec := &recorder{}
		client := newTestClient(t, rec, guarded)
		if err := client.Destroy(context.Background(), id); !NotSent(err) {
			t.Errorf("Destroy(%q): %v, want a request that is not sent", id, err)
		}
		if _, err := client.Exec(context.Background(), id, ExecRequest{Cmd: "true"}, func(Stream, string) {}); !NotSent(err) {
			t.Errorf("Exec(%q): %v, want a request that is not sent", id, err)
		}
		if rec.count() != 0 {
			t.Errorf("%q reached the engine %d times", id, rec.count())
		}
	}
}

func TestExecStreamsInOrder(t *testing.T) {
	rec := &recorder{}
	client := newTestClient(t, rec, func(w http.ResponseWriter, req *http.Request) {
		guarded(w, req)
		if req.URL.Path != "/v1/runs/"+testID+"/exec" {
			http.NotFound(w, req)
			return
		}
		startStream(w)
		sseFrame(w, "stdout", "hello\nworld")                        // one chunk, split over two data: lines
		fmt.Fprint(w, ": keep-alive\n\n")                            // a comment
		sseFrame(w, "metric", "ignored")                             // an event this client does not know
		sseFrame(w, "stderr", "oops\r")                              // a carriage return, sent raw
		sseFrame(w, "stdout", "")                                    // an empty chunk
		fmt.Fprint(w, "event: stdout\ndata: a\ndata: \ndata: b\n\n") // a chunk that is "a\n\nb"
		sseFrame(w, "exit", `{"exitCode":3,"timedOut":false}`)
	})
	var got []string
	result, err := client.Exec(context.Background(), testID, ExecRequest{Cmd: "sh -c 'echo hello'"}, func(stream Stream, chunk string) {
		got = append(got, string(stream)+"="+strings.ReplaceAll(chunk, "\n", "⏎"))
	})
	if err != nil || result.ExitCode != 3 || result.TimedOut {
		t.Fatalf("Exec = %+v, %v", result, err)
	}
	want := []string{"stdout=hello⏎world", "stderr=oops\r", "stdout=", "stdout=a⏎⏎b"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("chunks %q, want %q", got, want)
	}
	if sent := rec.at(t, 0); sent.method != http.MethodPost || sent.contentType != "application/json" {
		t.Errorf("Exec sent %+v", sent)
	}
}

func TestExecSendsTheWireBodysFieldNames(t *testing.T) {
	rec := &recorder{}
	client := newTestClient(t, rec, func(w http.ResponseWriter, req *http.Request) {
		startStream(w)
		sseFrame(w, "exit", `{"exitCode":124,"timedOut":true}`)
	})
	result, err := client.Exec(context.Background(), testID, ExecRequest{Cmd: "sleep 9", Cwd: "src", TimeoutSec: 7, Env: map[string]string{"A": "b"}}, func(Stream, string) {})
	if err != nil || result.ExitCode != 124 || !result.TimedOut {
		t.Fatalf("Exec = %+v, %v", result, err)
	}
	if body := string(rec.at(t, 0).body); body != `{"cmd":"sleep 9","cwd":"src","timeoutSec":7,"env":{"A":"b"}}` {
		t.Errorf("Exec sent %s", body)
	}
	// A sandbox that could not run the command says so with -1.
	result, err = client.Exec(context.Background(), testID, ExecRequest{Cmd: "x"}, func(Stream, string) {})
	if err != nil || result.ExitCode != 124 {
		t.Errorf("second Exec = %+v, %v", result, err)
	}
	if body := string(rec.at(t, 1).body); body != `{"cmd":"x"}` {
		t.Errorf("a minimal exec sent %s", body)
	}
	if _, err := client.Exec(context.Background(), testID, ExecRequest{Cmd: "  "}, func(Stream, string) {}); !NotSent(err) {
		t.Errorf("an empty command: %v, want a request that is not sent", err)
	}
}

// A refusal before the stream is a problem, and nothing ran.
func TestExecRefusedBeforeTheStream(t *testing.T) {
	client := newTestClient(t, &recorder{}, func(w http.ResponseWriter, req *http.Request) {
		problem(w, http.StatusConflict, "conflict", "run "+testID+" is stopped; resume it first")
	})
	called := false
	_, err := client.Exec(context.Background(), testID, ExecRequest{Cmd: "true"}, func(Stream, string) { called = true })
	if !IsConflict(err) {
		t.Fatalf("got %v, want a 409 problem", err)
	}
	if called {
		t.Error("output was passed on for an exec that never started")
	}
}

// The stream is the only word on how a command ended. One that dies before
// the exit frame leaves the outcome unknown, and says so with its own error
// rather than an exit code.
func TestExecStreamThatEndsWithoutAnExit(t *testing.T) {
	cases := map[string]struct {
		write func(w http.ResponseWriter)
		want  error
	}{
		"dies after output":    {func(w http.ResponseWriter) { startStream(w); sseFrame(w, "stdout", "half") }, ErrStreamEnded},
		"ends before anything": {func(w http.ResponseWriter) { startStream(w) }, ErrStreamEnded},
		"malformed exit":       {func(w http.ResponseWriter) { startStream(w); sseFrame(w, "exit", `{"exitCode":`) }, ErrBadExit},
		"not an event stream": {func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, "<h1>login</h1>")
		}, nil},
	}
	for name, c := range cases {
		client := newTestClient(t, &recorder{}, func(w http.ResponseWriter, req *http.Request) { c.write(w) })
		result, err := client.Exec(context.Background(), testID, ExecRequest{Cmd: "true"}, func(Stream, string) {})
		if err == nil {
			t.Errorf("%s: read as a command that exited with %d", name, result.ExitCode)
			continue
		}
		if c.want != nil && !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
		if result.ExitCode != 0 || result.TimedOut {
			t.Errorf("%s: a stream that gave no exit reported %+v", name, result)
		}
	}
}

// started says when a 200 arrived, and only then. A caller that reports on
// a command must tell an exec the engine refused (nothing ran) from one
// whose stream it took and then lost, and from one that was never answered.
func TestExecStartedIsSaidOnceA200Arrived(t *testing.T) {
	cases := map[string]struct {
		write   func(w http.ResponseWriter)
		started bool // a 200 arrived
		known   bool // OutcomeKnown: nothing ran, or the exit arrived
	}{
		"refused before the stream": {func(w http.ResponseWriter) {
			problem(w, http.StatusConflict, "conflict", "run "+testID+" is stopped")
		}, false, true},
		"ran to its exit": {func(w http.ResponseWriter) {
			startStream(w)
			sseFrame(w, "exit", `{"exitCode":3,"timedOut":false}`)
		}, true, true},
		"dies after the 200": {func(w http.ResponseWriter) {
			startStream(w)
			sseFrame(w, "stdout", "half")
		}, true, false},
		"a 200 that is not a stream": {func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, "<h1>login</h1>")
		}, true, false},
		"no answer": {func(w http.ResponseWriter) {
			// Take the connection and hang up without a word.
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
		}, false, false},
	}
	for name, c := range cases {
		client := newTestClient(t, &recorder{}, func(w http.ResponseWriter, req *http.Request) { c.write(w) })
		started := false
		_, err := client.ExecStream(context.Background(), testID, ExecRequest{Cmd: "true"}, func(Stream, string) {}, func() { started = true })
		if started != c.started {
			t.Errorf("%s: started = %v, want %v", name, started, c.started)
		}
		if OutcomeKnown(err) != c.known {
			t.Errorf("%s: OutcomeKnown = %v, want %v (%v)", name, !c.known, c.known, err)
		}
	}
	// A request that did not leave never starts anything.
	client := newTestClient(t, &recorder{}, guarded)
	started := false
	if _, err := client.ExecStream(context.Background(), "not-an-id", ExecRequest{Cmd: "true"}, func(Stream, string) {}, func() { started = true }); !NotSent(err) || started {
		t.Errorf("an id that is not one: started = %v, err = %v", started, err)
	}
	// Exec is ExecStream without wanting to know.
	if _, err := client.Exec(context.Background(), "not-an-id", ExecRequest{Cmd: "true"}, func(Stream, string) {}); !NotSent(err) {
		t.Errorf("Exec: %v", err)
	}
}

// The last frame may lack the blank line that ends it: the exit still counts.
func TestExecExitWithoutItsClosingBlankLine(t *testing.T) {
	client := newTestClient(t, &recorder{}, func(w http.ResponseWriter, req *http.Request) {
		startStream(w)
		fmt.Fprint(w, "event: stdout\ndata: last line without a newline")
		w.(http.Flusher).Flush()
	})
	result, err := client.Exec(context.Background(), testID, ExecRequest{Cmd: "true"}, func(Stream, string) {})
	if !errors.Is(err, ErrStreamEnded) {
		t.Fatalf("output with no exit frame: %v, %d", err, result.ExitCode)
	}
}

// The engine answers an exec with its first output, so a command that is
// silent is waited for through its whole timeout plus the grace, and no
// longer. The timeout is whole seconds, so a negative grace is what makes
// the wait short enough to test.
func TestExecWaitsForASilentCommandOnlySoLong(t *testing.T) {
	saved := execHeaderGrace
	t.Cleanup(func() { execHeaderGrace = saved })
	silent := func(delay time.Duration) http.HandlerFunc {
		return func(w http.ResponseWriter, req *http.Request) {
			time.Sleep(delay)
			startStream(w)
			sseFrame(w, "exit", `{"exitCode":0,"timedOut":false}`)
		}
	}
	ctx := context.Background()

	execHeaderGrace = -900 * time.Millisecond // the wait is a tenth of a second
	client := newTestClient(t, &recorder{}, silent(600*time.Millisecond))
	called := false
	start := time.Now()
	_, err := client.Exec(ctx, testID, ExecRequest{Cmd: "sleep 9", TimeoutSec: 1}, func(Stream, string) { called = true })
	if err == nil || called {
		t.Errorf("a silent engine was waited on to the end: %v (output passed on: %v)", err, called)
	}
	if elapsed := time.Since(start); elapsed > 550*time.Millisecond {
		t.Errorf("the wait lasted %s; it is the command's timeout plus the grace", elapsed)
	}

	execHeaderGrace = 150 * time.Millisecond
	client = newTestClient(t, &recorder{}, silent(50*time.Millisecond))
	if result, err := client.Exec(ctx, testID, ExecRequest{Cmd: "sleep 9", TimeoutSec: 1}, func(Stream, string) {}); err != nil || result.ExitCode != 0 {
		t.Errorf("a command silent for less than its timeout: %+v, %v", result, err)
	}
}

// A request cut off by its caller comes back as the context's error.
func TestExecEndsWithItsContext(t *testing.T) {
	client := newTestClient(t, &recorder{}, func(w http.ResponseWriter, req *http.Request) {
		startStream(w)
		sseFrame(w, "stdout", "started")
		<-req.Context().Done() // a command that never ends
	})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err := client.Exec(ctx, testID, ExecRequest{Cmd: "sleep 1000"}, func(Stream, string) {})
	if err == nil || ctx.Err() == nil {
		t.Fatalf("an exec outlived its context: %v", err)
	}
}

func TestPutAndGetFile(t *testing.T) {
	content := []byte("package main\n\x00\x01\xff")
	rec := &recorder{}
	client := newTestClient(t, rec, func(w http.ResponseWriter, req *http.Request) {
		guarded(w, req)
		if req.Method == http.MethodPut {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(content)
	})
	ctx := context.Background()
	// A leading slash names the same file: the path is /work's.
	if err := client.PutFile(ctx, testID, "/src/a b/ünïcode.go", content); err != nil {
		t.Fatal(err)
	}
	sent := rec.at(t, 0)
	const escaped = "/v1/runs/" + testID + "/files/src/a%20b/%C3%BCn%C3%AFcode.go"
	if sent.rawPath != escaped || sent.path != "/v1/runs/"+testID+"/files/src/a b/ünïcode.go" {
		t.Errorf("PUT went to %s (escaped %s), want %s", sent.path, sent.rawPath, escaped)
	}
	if sent.contentType != "application/octet-stream" || string(sent.body) != string(content) {
		t.Errorf("PUT sent %q %q", sent.contentType, sent.body)
	}
	got, err := client.GetFile(ctx, testID, "src/a b/ünïcode.go")
	if err != nil || string(got) != string(content) {
		t.Fatalf("GetFile = %q, %v", got, err)
	}
	if rec.at(t, 1).method != http.MethodGet || rec.at(t, 1).rawPath != escaped {
		t.Errorf("GET went to %s %s", rec.at(t, 1).method, rec.at(t, 1).rawPath)
	}

	// An empty file is no body at all.
	if err := client.PutFile(ctx, testID, "empty.txt", nil); err != nil {
		t.Fatal(err)
	}
	if len(rec.at(t, 2).body) != 0 {
		t.Errorf("an empty file sent %q", rec.at(t, 2).body)
	}
}

func TestFilesRefusedHere(t *testing.T) {
	for _, path := range []string{"", "/", "..", "a/../b", "a//b", "./a", "a/./b"} {
		rec := &recorder{}
		client := newTestClient(t, rec, guarded)
		if err := client.PutFile(context.Background(), testID, path, []byte("x")); !NotSent(err) {
			t.Errorf("PutFile(%q): %v, want a request that is not sent", path, err)
		}
		if _, err := client.GetFile(context.Background(), testID, path); !NotSent(err) {
			t.Errorf("GetFile(%q): %v, want a request that is not sent", path, err)
		}
		if rec.count() != 0 {
			t.Errorf("%q reached the engine", path)
		}
	}
	rec := &recorder{}
	client := newTestClient(t, rec, guarded)
	big := make([]byte, MaxFileBytes+1)
	if err := client.PutFile(context.Background(), testID, "big.bin", big); !NotSent(err) {
		t.Errorf("an oversized file: %v, want a request that is not sent", err)
	}
	if rec.count() != 0 {
		t.Errorf("an oversized file reached the engine")
	}
}

func TestAMissingFileIsNotFound(t *testing.T) {
	client := newTestClient(t, &recorder{}, func(w http.ResponseWriter, req *http.Request) {
		problem(w, http.StatusNotFound, "not-found", "No such file: nope.txt")
	})
	_, err := client.GetFile(context.Background(), testID, "nope.txt")
	if !IsNotFound(err) {
		t.Fatalf("got %v, want a 404 problem", err)
	}
}

// The engine never redirects. Were one to arrive, following it would hand
// the token to whatever sits at the other address.
func TestClientNeverFollowsARedirect(t *testing.T) {
	other := &recorder{}
	decoy := httptest.NewServer(other.wrap(func(w http.ResponseWriter, req *http.Request) { io.WriteString(w, `{}`) }))
	t.Cleanup(decoy.Close)
	client := newTestClient(t, &recorder{}, func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, decoy.URL+"/v1/runs", http.StatusFound)
	})
	_, err := client.Create(context.Background(), CreateRequest{Image: "alpine"})
	if err == nil {
		t.Fatal("a redirect read as success")
	}
	if other.count() != 0 {
		t.Fatalf("the redirect was followed: %d requests reached the other address, the token with them", other.count())
	}
}

// The connection goes where the dial function sends it: a proxy in the
// environment is ignored, or a request would leave the tunnel for the
// proxy's address. (net/http skips a proxy for a loopback host by itself,
// so the engine is named by a host that is not one.)
func TestClientIgnoresTheEnvironmentsProxy(t *testing.T) {
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy", "ALL_PROXY", "all_proxy"} {
		t.Setenv(name, "http://127.0.0.1:9") // nothing listens there
	}
	var got []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// A proxied request carries an absolute URL; a direct one does not.
		got = append(got, fmt.Sprintf("host=%s absolute=%v", req.Host, req.URL.IsAbs()))
		io.WriteString(w, `{"status":"ok","version":"0.1.0"}`)
	}))
	t.Cleanup(server.Close)
	var dialed []string
	client, err := New("engine.internal:7439", testToken, func(ctx context.Context, network, addr string) (net.Conn, error) {
		dialed = append(dialed, addr)
		return net.Dial(network, server.Listener.Addr().String())
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Health(context.Background()); err != nil {
		t.Fatalf("the request did not reach the engine: %v", err)
	}
	if strings.Join(dialed, ",") != "engine.internal:7439" {
		t.Errorf("the dial function was asked for %q, want the engine's address and nothing else", dialed)
	}
	if want := "host=engine.internal:7439 absolute=false"; strings.Join(got, "|") != want {
		t.Errorf("the engine saw %q, want %q", got, want)
	}
}

// An answer past the bound is an error, never a truncated document.
func TestAnswerPastTheBoundIsAnError(t *testing.T) {
	client := newTestClient(t, &recorder{}, func(w http.ResponseWriter, req *http.Request) {
		io.WriteString(w, `{"runs":["`+strings.Repeat("x", maxJSONBytes)+`"]}`)
	})
	if _, err := client.List(context.Background()); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("an oversized answer read as a list: %v", err)
	}
}

// A dial that fails comes back as an error that says what was asked. This
// is the tunnel's refused connection (ssh.ForwardRefused reads it).
func TestDialFailure(t *testing.T) {
	refused := errors.New("ssh: rejected: connect failed (\"open failed\")")
	client, err := New("127.0.0.1:7439", testToken, func(context.Context, string, string) (net.Conn, error) { return nil, refused })
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Health(context.Background())
	if !errors.Is(err, refused) || !strings.Contains(err.Error(), "GET /health") {
		t.Fatalf("got %v, want the dial's error under the request that was asked", err)
	}
	if OutcomeKnown(err) {
		t.Error("a request that could not connect was given a known outcome for a create; for all this client knows it never left")
	}
}

func TestParseAddr(t *testing.T) {
	for in, want := range map[string]string{
		"127.0.0.1:7439":        "http://127.0.0.1:7439",
		":7439":                 "http://127.0.0.1:7439",
		" 127.0.0.1:7439 ":      "http://127.0.0.1:7439",
		"10.0.0.5:7439":         "http://10.0.0.5:7439",
		"sandbox.internal:7439": "http://sandbox.internal:7439",
		"[::1]:7439":            "http://[::1]:7439",
		"[fd00::5]:7439":        "http://[fd00::5]:7439",
	} {
		if addr, base, err := ParseAddr(in); err != nil || addr != strings.TrimPrefix(want, "http://") || base != want {
			t.Errorf("ParseAddr(%q) = %q, %q, %v; want %q", in, addr, base, err, want)
		}
	}
	for _, in := range []string{"", "   ", "7439", "127.0.0.1", "127.0.0.1:0", "127.0.0.1:70000", "127.0.0.1:http", "127.0.0.1:7439/path", "http://127.0.0.1:7439", "127.0.0.1:7439?x=1", "host:7439#frag", "127.0.0.1:7439:1"} {
		if _, _, err := ParseAddr(in); err == nil {
			t.Errorf("ParseAddr(%q) was accepted", in)
		}
	}
	if _, err := New("127.0.0.1:7439", "  ", nil); err == nil {
		t.Error("an empty token was accepted")
	}
}
