package cli

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/useteploy/teploy/internal/autodeploy"
	"github.com/useteploy/teploy/internal/config"
)

func githubSign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// memLedger is an in-memory LedgerAppender for handler tests: records every
// append, optionally fails.
type memLedger struct {
	mu      sync.Mutex
	records []autodeploy.AdmissionRecord
	fail    error
}

func (m *memLedger) Append(rec autodeploy.AdmissionRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return m.fail
	}
	m.records = append(m.records, rec)
	return nil
}

func (m *memLedger) snapshot() []autodeploy.AdmissionRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]autodeploy.AdmissionRecord, len(m.records))
	copy(out, m.records)
	return out
}

func (m *memLedger) byKind(kind string) []autodeploy.AdmissionRecord {
	var out []autodeploy.AdmissionRecord
	for _, rec := range m.snapshot() {
		if rec.Kind == kind {
			out = append(out, rec)
		}
	}
	return out
}

// countingRun records deploy invocations. Each entry signals started
// (the deploy was entered — even if it then blocks) and each completion
// signals done.
type countingRun struct {
	mu      sync.Mutex
	calls   []time.Time
	started chan struct{}
	done    chan struct{}
	block   chan struct{} // non-nil: each call blocks until closed
}

func newCountingRun() *countingRun {
	return &countingRun{started: make(chan struct{}, 64), done: make(chan struct{}, 64)}
}

func (c *countingRun) blocking(block chan struct{}) *countingRun {
	c.block = block
	return c
}

func (c *countingRun) run(_ []string, _ bool, _ string) {
	c.mu.Lock()
	c.calls = append(c.calls, time.Now())
	block := c.block
	c.mu.Unlock()
	c.started <- struct{}{}
	if block != nil {
		<-block
	}
	c.done <- struct{}{}
}

func (c *countingRun) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.calls)
}

func (c *countingRun) waitCall(t *testing.T) {
	t.Helper()
	select {
	case <-c.started:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a deploy invocation")
	}
}

func (c *countingRun) waitIdle(t *testing.T, q *admissionQueue) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		workerLive, pending := q.snapshot()
		if !workerLive && pending == nil {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("queue did not drain to idle within timeout")
}

// newAdmissionStack wires a handler + queue + ledger with an injectable
// deploy runner, the shape runAutoDeployServe uses.
func newAdmissionStack(secret, branch, app string, run func([]string, bool, string)) (http.HandlerFunc, *memLedger, *admissionQueue) {
	ledger := &memLedger{}
	queue := newAdmissionQueue(ledger, run, func(string, ...any) {})
	handler := newWebhookHandler(webhookHandlerConfig{
		secret: secret,
		branch: branch,
		app:    app,
		dedup:  autodeploy.NewDeliveryDedup(),
		ledger: ledger,
		queue:  queue,
		logf:   func(string, ...any) {},
	})
	return handler, ledger, queue
}

func postSigned(t *testing.T, handler http.HandlerFunc, secret, delivery, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", githubSign(secret, []byte(body)))
	if delivery != "" {
		req.Header.Set("X-GitHub-Delivery", delivery)
	}
	rec := httptest.NewRecorder()
	handler(rec, req)
	return rec
}

// TestWebhookHandler_ValidSignatureTriggersDeployOnce is the direct
// regression test for the old autodeploy's worst bug: the generated bash
// listener verified the signature correctly but never called anything
// resembling a real deploy — it only built an image and stopped. This
// confirms a valid, well-formed webhook is durably admitted and runs the
// deploy exactly once.
func TestWebhookHandler_ValidSignatureTriggersDeployOnce(t *testing.T) {
	run := newCountingRun()
	handler, ledger, queue := newAdmissionStack("s3cret", "", "myapp", run.run)

	rec := postSigned(t, handler, "s3cret", "delivery-1", `{"ref":"refs/heads/main"}`)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	run.waitCall(t)
	run.waitIdle(t, queue)
	if run.count() != 1 {
		t.Errorf("deploy ran %d times, want 1", run.count())
	}
	if got := len(ledger.byKind(autodeploy.AdmissionKindAdmitted)); got != 1 {
		t.Errorf("admitted records = %d, want 1", got)
	}
	if got := len(ledger.byKind(autodeploy.AdmissionKindProcessed)); got != 1 {
		t.Errorf("processed records = %d, want 1", got)
	}
}

func TestWebhookHandler_InvalidSignatureNeverTriggers(t *testing.T) {
	run := newCountingRun()
	handler, _, _ := newAdmissionStack("s3cret", "", "myapp", run.run)

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"ref":"refs/heads/main"}`))
	req.Header.Set("X-Hub-Signature-256", "sha256=0000000000000000000000000000000000000000000000000000000000000000")
	rec := httptest.NewRecorder()

	handler(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if run.count() != 0 {
		t.Error("an invalid signature must never trigger a deploy")
	}
}

func TestWebhookHandler_NoSignatureNeverTriggers(t *testing.T) {
	run := newCountingRun()
	handler, _, _ := newAdmissionStack("s3cret", "", "myapp", run.run)

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()

	handler(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if run.count() != 0 {
		t.Error("a request with no signature header must never trigger a deploy")
	}
}

// TestWebhookHandler_ReplayedDeliveryIgnored is the regression test for
// the missing replay protection in the old bash listener: a captured
// valid payload+signature could be replayed indefinitely to re-trigger
// deploys.
func TestWebhookHandler_ReplayedDeliveryIgnored(t *testing.T) {
	run := newCountingRun()
	handler, _, queue := newAdmissionStack("s3cret", "", "myapp", run.run)

	rec1 := postSigned(t, handler, "s3cret", "delivery-1", `{"ref":"refs/heads/main"}`)
	rec2 := postSigned(t, handler, "s3cret", "delivery-1", `{"ref":"refs/heads/main"}`)

	run.waitCall(t)
	run.waitIdle(t, queue)
	if run.count() != 1 {
		t.Errorf("deploy ran %d times across a replayed delivery, want 1", run.count())
	}
	// A replay is a no-op, not a rejection — provider shouldn't retry harder.
	if rec2.Code != http.StatusOK {
		t.Errorf("replayed delivery status = %d, want 200 (no-op, not a failure)", rec2.Code)
	}
	if !strings.Contains(rec2.Body.String(), `"duplicate"`) {
		t.Errorf("replayed delivery body = %q, want duplicate status", rec2.Body.String())
	}
	if rec1.Code != http.StatusOK {
		t.Errorf("first delivery status = %d, want 200", rec1.Code)
	}
}

func TestWebhookHandler_GitLabToken(t *testing.T) {
	run := newCountingRun()
	handler, _, queue := newAdmissionStack("s3cret", "", "myapp", run.run)

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"ref":"refs/heads/main"}`))
	req.Header.Set("X-Gitlab-Token", "s3cret")
	rec := httptest.NewRecorder()

	handler(rec, req)
	run.waitCall(t)
	run.waitIdle(t, queue)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if run.count() != 1 {
		t.Errorf("deploy ran %d times, want 1", run.count())
	}
}

func TestWebhookHandler_GitLabWrongToken(t *testing.T) {
	run := newCountingRun()
	handler, _, _ := newAdmissionStack("s3cret", "", "myapp", run.run)

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`))
	req.Header.Set("X-Gitlab-Token", "wrong")
	rec := httptest.NewRecorder()

	handler(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if run.count() != 0 {
		t.Error("wrong GitLab token must never trigger a deploy")
	}
}

func TestWebhookHandler_RejectsNonPOST(t *testing.T) {
	run := newCountingRun()
	handler, _, _ := newAdmissionStack("s3cret", "", "myapp", run.run)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()

	handler(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
	if run.count() != 0 {
		t.Error("a GET request must never trigger a deploy")
	}
}

// TestWebhookHandler_OnDedupChangedCalledAfterDurableAdmission pins the
// C02 ordering: the dedup snapshot persist fires only after the admission
// is durable, so a dedup entry on disk always corresponds to a ledger
// admission. Non-push events (pings, other branches) are acknowledged
// without persisting dedup — their replay after a restart is a harmless
// no-op ack, never a lost deploy.
func TestWebhookHandler_OnDedupChangedCalledAfterDurableAdmission(t *testing.T) {
	run := newCountingRun()
	ledger := &memLedger{}
	queue := newAdmissionQueue(ledger, run.run, func(string, ...any) {})
	changedCount := 0
	handler := newWebhookHandler(webhookHandlerConfig{
		secret:         "s3cret",
		branch:         "main",
		app:            "myapp",
		dedup:          autodeploy.NewDeliveryDedup(),
		ledger:         ledger,
		queue:          queue,
		logf:           func(string, ...any) {},
		onDedupChanged: func() { changedCount++ },
	})

	postSigned(t, handler, "s3cret", "delivery-1", `{"ref":"refs/heads/main","after":"a"}`)
	if changedCount != 1 {
		t.Errorf("onDedupChanged called %d times after durable admission, want 1", changedCount)
	}
	if got := len(ledger.byKind(autodeploy.AdmissionKindAdmitted)); got != 1 {
		t.Errorf("admitted = %d before onDedupChanged fired, want 1 (persist follows durability)", got)
	}

	// A ping is acknowledged but never persisted to dedup (no admission).
	changedCount = 0
	postSigned(t, handler, "s3cret", "delivery-2", `{}`)
	if changedCount != 0 {
		t.Errorf("onDedupChanged fired %d times for a ping, want 0", changedCount)
	}
	run.waitIdle(t, queue)
}

// audit F40: only a push to the WATCHED branch may trigger a deploy. A ping,
// a tag push, a push to another branch, and a branch deletion are
// acknowledged no-ops — each used to deploy the watched branch's current
// state with an unrelated changed-file list.
func TestWebhookHandler_OnlyWatchedBranchPushes(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"watched branch", `{"ref":"refs/heads/main","commits":[{"added":["a"]}]}`, true},
		{"other branch", `{"ref":"refs/heads/develop","commits":[{"added":["a"]}]}`, false},
		{"tag push", `{"ref":"refs/tags/v1.0.0"}`, false},
		{"ping", `{}`, false},
		{"branch deletion", `{"ref":"refs/heads/main","deleted":true}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			run := newCountingRun()
			handler, ledger, queue := newAdmissionStack("s3cret", "main", "myapp", run.run)
			postSigned(t, handler, "s3cret", "", tc.body)
			if tc.want {
				run.waitCall(t)
			}
			run.waitIdle(t, queue)
			if got := run.count() > 0; got != tc.want {
				t.Errorf("deploy ran = %v, want %v", got, tc.want)
			}
			if got := len(ledger.byKind(autodeploy.AdmissionKindAdmitted)) > 0; got != tc.want {
				t.Errorf("admitted = %v, want %v", got, tc.want)
			}
		})
	}
}

// audit F41: the delivery-ID header is unauthenticated, so replaying a
// captured signed body under a FRESH delivery ID used to bypass the dedup.
// Dedup must be keyed on the authenticated content digest.
func TestWebhookHandler_ContentReplayRejected(t *testing.T) {
	run := newCountingRun()
	handler, _, queue := newAdmissionStack("s3cret", "main", "myapp", run.run)
	body := `{"ref":"refs/heads/main"}`

	if rec := postSigned(t, handler, "s3cret", "delivery-1", body); rec.Code != http.StatusOK {
		t.Fatalf("first delivery status = %d, want 200", rec.Code)
	}
	run.waitCall(t)
	// Same signed body, different delivery ID → replay, no second deploy.
	if rec := postSigned(t, handler, "s3cret", "delivery-2", body); rec.Code != http.StatusOK {
		t.Errorf("content replay should be a 200 no-op, got %d", rec.Code)
	}
	// Same signed body, NO delivery header at all → still a replay.
	if rec := postSigned(t, handler, "s3cret", "", body); rec.Code != http.StatusOK {
		t.Errorf("headerless content replay should be a 200 no-op, got %d", rec.Code)
	}
	run.waitIdle(t, queue)
	if run.count() != 1 {
		t.Errorf("deploy ran %d times for one unique signed body, want 1", run.count())
	}
}

// TestWebhookHandler_ReusedDeliveryIDDifferentContentNotSuppressed is the
// A36 regression: the (unauthenticated) delivery-ID header must never
// suppress DISTINCT authenticated content — a provider reusing an ID with
// a different signed body is a new event, and only content dedup decides
// replays.
func TestWebhookHandler_ReusedDeliveryIDDifferentContentNotSuppressed(t *testing.T) {
	run := newCountingRun()
	handler, _, queue := newAdmissionStack("s3cret", "", "myapp", run.run)

	postSigned(t, handler, "s3cret", "same-delivery-id", `{"ref":"refs/heads/main","after":"aaaa"}`)
	run.waitCall(t)
	postSigned(t, handler, "s3cret", "same-delivery-id", `{"ref":"refs/heads/main","after":"bbbb"}`)
	run.waitCall(t)
	run.waitIdle(t, queue)
	if run.count() != 2 {
		t.Errorf("distinct authenticated content under a reused delivery ID must both deploy, got %d", run.count())
	}
	// The SAME content replays to a no-op regardless of the header.
	postSigned(t, handler, "s3cret", "same-delivery-id", `{"ref":"refs/heads/main","after":"aaaa"}`)
	run.waitIdle(t, queue)
	if run.count() != 2 {
		t.Errorf("replayed content must be a no-op, got %d", run.count())
	}
}

// TestResolveTLSFromRoot is the T31 regression: the resident autodeploy
// process runs under systemd without a WorkingDirectory, so relative TLS
// paths must resolve against the checkout, and absolute paths must pass
// through untouched.
func TestResolveTLSFromRoot(t *testing.T) {
	in := &config.TLSConfig{Cert: "certs/app.crt", Key: "/etc/absolute.key"}
	out := resolveTLSFromRoot(in, "/deployments/myapp/build")
	if out.Cert != "/deployments/myapp/build/certs/app.crt" {
		t.Errorf("relative cert not resolved against the checkout: %q", out.Cert)
	}
	if out.Key != "/etc/absolute.key" {
		t.Errorf("absolute key must pass through: %q", out.Key)
	}
	if in.Cert != "certs/app.crt" {
		t.Errorf("input TLSConfig mutated: %+v", in)
	}
}

// TestWebhookHandler_ThreadsCommitToTrigger (C02): the commit the payload
// authenticates must reach BOTH the durable admission record and the deploy
// trigger — the delivery is bound to its commit end to end.
func TestWebhookHandler_ThreadsCommitToTrigger(t *testing.T) {
	var gotCommit string
	var gotMu sync.Mutex
	run := func(_ []string, _ bool, commit string) {
		gotMu.Lock()
		gotCommit = commit
		gotMu.Unlock()
	}
	handler, ledger, queue := newAdmissionStack("s3cret", "main", "myapp", run)

	const sha = "0123456789abcdef0123456789abcdef01234567"
	postSigned(t, handler, "s3cret", "delivery-1", `{"ref":"refs/heads/main","after":"`+sha+`"}`)
	waitQueueIdle(t, queue)

	admitted := ledger.byKind(autodeploy.AdmissionKindAdmitted)
	if len(admitted) != 1 {
		t.Fatalf("admitted records = %d, want 1", len(admitted))
	}
	if admitted[0].Commit != sha {
		t.Errorf("admission record commit = %q, want %q", admitted[0].Commit, sha)
	}
	gotMu.Lock()
	defer gotMu.Unlock()
	if gotCommit != sha {
		t.Errorf("deploy trigger received commit %q, want %q", gotCommit, sha)
	}
}

// A delivery with NO usable commit still deploys — pinned to nothing (tip),
// stated as such to the trigger.
func TestWebhookHandler_NoCommitMeansTip(t *testing.T) {
	var gotCommit string
	var gotMu sync.Mutex
	run := func(_ []string, _ bool, commit string) {
		gotMu.Lock()
		gotCommit = commit
		gotMu.Unlock()
	}
	handler, _, queue := newAdmissionStack("s3cret", "main", "myapp", run)

	postSigned(t, handler, "s3cret", "delivery-1", `{"ref":"refs/heads/main"}`)
	waitQueueIdle(t, queue)

	gotMu.Lock()
	defer gotMu.Unlock()
	if gotCommit != "" {
		t.Errorf("payload without a commit pinned trigger to %q, want empty (tip)", gotCommit)
	}
}

// waitQueueIdle blocks until the admission queue has no worker and no
// pending slot (for tests whose run func is not a countingRun).
func waitQueueIdle(t *testing.T, q *admissionQueue) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		workerLive, pending := q.snapshot()
		if !workerLive && pending == nil {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("queue did not drain to idle within timeout")
}
