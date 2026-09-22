package cli

// C02 webhook admission durability tests: ack-after-persist ordering,
// crash-between-admit-and-trigger resume, supersede, bounded queueing, and
// persistence-failure refusal.

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/useteploy/teploy/internal/autodeploy"
)

// recordingWriter flags the instant a response status/body write begins —
// the observable for "was the response written before the fsync?".
type recordingWriter struct {
	header    http.Header
	responded atomic.Bool
	code      atomic.Int32
}

func newRecordingWriter() *recordingWriter {
	return &recordingWriter{header: make(http.Header)}
}

func (w *recordingWriter) Header() http.Header { return w.header }

func (w *recordingWriter) WriteHeader(code int) {
	w.responded.Store(true)
	w.code.Store(int32(code))
}

func (w *recordingWriter) Write(p []byte) (int, error) {
	w.responded.Store(true)
	return len(p), nil
}

// gatedLedger is a LedgerAppender whose append passes through an explicit
// write → fsync gate: the "write" phase records the entry, then the append
// blocks in its "fsync" until released. Acks observed while the fsync is
// still blocked are acks-before-durability.
type gatedLedger struct {
	mu          sync.Mutex
	records     []autodeploy.AdmissionRecord
	writeDone   chan struct{}
	fsyncStart  chan struct{}
	fsyncDone   chan struct{}
	releaseOnce sync.Once
}

func newGatedLedger() *gatedLedger {
	return &gatedLedger{
		writeDone:  make(chan struct{}, 1),
		fsyncStart: make(chan struct{}, 1),
		fsyncDone:  make(chan struct{}, 1),
	}
}

func (g *gatedLedger) Append(rec autodeploy.AdmissionRecord) error {
	g.mu.Lock()
	g.records = append(g.records, rec)
	g.mu.Unlock()
	g.writeDone <- struct{}{}
	g.fsyncStart <- struct{}{} // entering fsync; blocking there until release
	<-g.fsyncDone
	return nil
}

func (g *gatedLedger) releaseFsync() {
	g.releaseOnce.Do(func() { close(g.fsyncDone) })
}

// TestAdmission_AckWaitsForFsync (C02 3a): the 200 must not be written
// before the admission ledger append (write + fsync) completes.
func TestAdmission_AckWaitsForFsync(t *testing.T) {
	run := newCountingRun()
	ledger := newGatedLedger()
	queue := newAdmissionQueue(ledger, run.run, func(string, ...any) {})
	handler := newWebhookHandler(webhookHandlerConfig{
		secret: "s3cret",
		branch: "main",
		app:    "myapp",
		dedup:  autodeploy.NewDeliveryDedup(),
		ledger: ledger,
		queue:  queue,
		logf:   func(string, ...any) {},
	})

	body := `{"ref":"refs/heads/main","after":"abc"}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", githubSign("s3cret", []byte(body)))
	w := newRecordingWriter()

	handlerDone := make(chan struct{})
	go func() {
		handler(w, req)
		close(handlerDone)
	}()

	select {
	case <-ledger.writeDone:
	case <-time.After(5 * time.Second):
		t.Fatal("ledger append never started")
	}
	select {
	case <-ledger.fsyncStart:
	case <-handlerDone:
		t.Fatal("handler finished without ever reaching the ledger fsync — admission was never durable")
	}

	// The fsync is blocked NOW. If the response has been written at this
	// point, the ack preceded durability.
	if w.responded.Load() {
		t.Fatalf("response written (status %d) before the admission fsync completed", w.code.Load())
	}

	ledger.releaseFsync()
	select {
	case <-handlerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not finish after fsync release")
	}
	if code := w.code.Load(); code != http.StatusOK {
		t.Errorf("status = %d, want 200", code)
	}
	if !strings.Contains(w.header.Get("Content-Type"), "application/json") {
		t.Errorf("content-type = %q, want application/json", w.header.Get("Content-Type"))
	}
}

// TestAdmission_PersistenceFailureRefused (C02 3e): when the durable
// append fails, the delivery is NOT acked — 503 + Retry-After, nothing
// admitted, and the dedup entry is rolled back so the provider's retry of
// the same signed body goes through admission again.
func TestAdmission_PersistenceFailureRefused(t *testing.T) {
	run := newCountingRun()
	ledger := &memLedger{fail: errors.New("disk full")}
	queue := newAdmissionQueue(ledger, run.run, func(string, ...any) {})
	dedup := autodeploy.NewDeliveryDedup()
	handler := newWebhookHandler(webhookHandlerConfig{
		secret: "s3cret",
		branch: "main",
		app:    "myapp",
		dedup:  dedup,
		ledger: ledger,
		queue:  queue,
		logf:   func(string, ...any) {},
	})

	rec := postSigned(t, handler, "s3cret", "delivery-1", `{"ref":"refs/heads/main","after":"abc"}`)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 (never ack what isn't durable)", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("503 must carry Retry-After so the provider retries")
	}
	workerLive, pending := queue.snapshot()
	if workerLive || pending != nil {
		t.Errorf("queue admitted work despite persistence failure (worker=%v pending=%v)", workerLive, pending != nil)
	}
	if got := len(ledger.snapshot()); got != 0 {
		t.Errorf("ledger records = %d, want 0", got)
	}

	// The dedup rollback is what makes the provider's retry work: the
	// same signed body must now be admitted normally once persistence
	// recovers.
	ledger.mu.Lock()
	ledger.fail = nil
	ledger.mu.Unlock()
	rec2 := postSigned(t, handler, "s3cret", "delivery-1", `{"ref":"refs/heads/main","after":"abc"}`)
	if rec2.Code != http.StatusOK {
		t.Errorf("retry after persistence failure: status = %d, want 200 admitted", rec2.Code)
	}
	run.waitCall(t)
	run.waitIdle(t, queue)
	if run.count() != 1 {
		t.Errorf("deploy ran %d times after recovery, want 1", run.count())
	}
}

// TestAdmission_Supersede (C02 3c): with a deploy running and delivery A
// queued, delivery B supersedes A — A is marked superseded in the ledger,
// B takes the slot, B's response says so, and when the running deploy
// finishes only B's trigger runs.
func TestAdmission_Supersede(t *testing.T) {
	block := make(chan struct{})
	run := newCountingRun().blocking(block)
	handler, ledger, queue := newAdmissionStack("s3cret", "main", "myapp", run.run)

	// W starts running (blocks in the deploy).
	if rec := postSigned(t, handler, "s3cret", "w", `{"ref":"refs/heads/main","after":"w"}`); rec.Code != http.StatusOK {
		t.Fatalf("running delivery status = %d", rec.Code)
	} else if !strings.Contains(rec.Body.String(), `"running"`) {
		t.Errorf("first delivery disposition = %q, want running", rec.Body.String())
	}
	run.waitCall(t) // W is inside the deploy now

	// A queues behind it.
	if rec := postSigned(t, handler, "s3cret", "a", `{"ref":"refs/heads/main","after":"a"}`); rec.Code != http.StatusOK {
		t.Fatalf("queued delivery status = %d", rec.Code)
	} else if !strings.Contains(rec.Body.String(), `"queued"`) {
		t.Errorf("second delivery disposition = %q, want queued", rec.Body.String())
	}

	// B supersedes A.
	if rec := postSigned(t, handler, "s3cret", "b", `{"ref":"refs/heads/main","after":"b"}`); rec.Code != http.StatusOK {
		t.Fatalf("superseding delivery status = %d", rec.Code)
	} else if !strings.Contains(rec.Body.String(), `"superseded"`) {
		t.Errorf("third delivery disposition = %q, want superseded", rec.Body.String())
	}

	// A is marked superseded in the ledger, by B.
	superseded := ledger.byKind(autodeploy.AdmissionKindSuperseded)
	if len(superseded) != 1 {
		t.Fatalf("superseded records = %d, want 1 (A)", len(superseded))
	}
	if superseded[0].Delivery != "a" {
		t.Errorf("superseded record names delivery %q, want a", superseded[0].Delivery)
	}
	bAdmitted := ledger.byKind(autodeploy.AdmissionKindAdmitted)
	var bRec *autodeploy.AdmissionRecord
	for i := range bAdmitted {
		if bAdmitted[i].Delivery == "b" {
			bRec = &bAdmitted[i]
		}
	}
	if bRec == nil || superseded[0].SupersededBy != bRec.ID {
		t.Errorf("A's superseded_by = %q, want B's admission id %v", superseded[0].SupersededBy, bRec)
	}

	// Let the running deploy finish: only B runs next (never A).
	close(block)
	run.waitCall(t) // B
	run.waitIdle(t, queue)
	if run.count() != 2 {
		t.Errorf("deploy ran %d times (W then B only), want 2", run.count())
	}
	if got := len(ledger.byKind(autodeploy.AdmissionKindProcessed)); got != 2 {
		t.Errorf("processed records = %d, want 2 (W and B)", got)
	}
}

// TestAdmission_NoGoroutinePileup (C02 3d, the red test that failed
// against the fire-and-forget handler): 10 rapid deliveries during one
// long running deploy collapse to one running + one newest pending. No
// per-delivery spawn: after the running deploy finishes, exactly one more
// deploy (the newest) runs.
func TestAdmission_NoGoroutinePileup(t *testing.T) {
	block := make(chan struct{})
	run := newCountingRun().blocking(block)
	handler, ledger, queue := newAdmissionStack("s3cret", "main", "myapp", run.run)

	running, queued, superseded := 0, 0, 0
	for i := 0; i < 10; i++ {
		body := `{"ref":"refs/heads/main","after":"c` + string(rune('0'+i)) + `"}`
		rec := postSigned(t, handler, "s3cret", "", body)
		if rec.Code != http.StatusOK {
			t.Fatalf("delivery %d status = %d, want 200", i, rec.Code)
		}
		switch {
		case strings.Contains(rec.Body.String(), `"running"`):
			running++
		case strings.Contains(rec.Body.String(), `"queued"`):
			queued++
		case strings.Contains(rec.Body.String(), `"superseded"`):
			superseded++
		}
	}

	if running != 1 || queued != 1 || superseded != 8 {
		t.Errorf("dispositions running=%d queued=%d superseded=%d, want 1/1/8", running, queued, superseded)
	}
	// Exactly one deploy is actually running; the queue holds exactly one.
	run.waitCall(t)
	if run.count() != 1 {
		t.Errorf("deploy invocations while busy = %d, want 1", run.count())
	}
	workerLive, pending := queue.snapshot()
	if !workerLive || pending == nil {
		t.Errorf("queue state worker=%v pending=%v, want running with one pending", workerLive, pending != nil)
	}
	if got := len(ledger.byKind(autodeploy.AdmissionKindSuperseded)); got != 8 {
		t.Errorf("superseded ledger records = %d, want 8", got)
	}

	close(block)
	run.waitCall(t) // the newest pending
	run.waitIdle(t, queue)
	if run.count() != 2 {
		t.Errorf("total deploy invocations = %d, want 2 (first + newest)", run.count())
	}
	if got := len(ledger.byKind(autodeploy.AdmissionKindProcessed)); got != 2 {
		t.Errorf("processed records = %d, want 2", got)
	}
}

// TestAdmission_ResumeAfterCrash (C02 3b): admitted-but-never-processed
// ledger entries trigger exactly one deploy on resume; processed entries
// never re-trigger; a duplicate delivery id never re-triggers; and the
// resume reseeds the dedup set from the ledger so redeliveries read as
// replays.
func TestAdmission_ResumeAfterCrash(t *testing.T) {
	dir := t.TempDir()
	ledgerPath := filepath.Join(dir, ".autodeploy-ledger.jsonl")
	fileLedger, err := autodeploy.OpenLedger(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Add(-time.Minute)
	mustAdmit := func(id, delivery, digest string, received time.Time) autodeploy.AdmissionRecord {
		rec := autodeploy.AdmissionRecord{
			Kind: autodeploy.AdmissionKindAdmitted, ID: id, Delivery: delivery,
			Digest: digest, App: "myapp", Branch: "main", Received: received,
		}
		if err := fileLedger.Append(rec); err != nil {
			t.Fatal(err)
		}
		return rec
	}

	// A: admitted and processed — must never re-trigger.
	a := mustAdmit("id-a", "del-a", "aaaa", base)
	if err := fileLedger.Append(autodeploy.AdmissionRecord{Kind: autodeploy.AdmissionKindProcessed, ID: a.ID, Digest: a.Digest, App: "myapp", Received: a.Received, At: base.Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
	// B: admitted, never processed — the crash window.
	b := mustAdmit("id-b", "del-b", "bbbb", base.Add(2*time.Second))
	_ = b

	run := newCountingRun()
	resumeLedger := &memLedger{}
	dedup := autodeploy.NewDeliveryDedup()
	queue := newAdmissionQueue(resumeLedger, run.run, func(string, ...any) {})
	if err := resumeAdmissions(ledgerPath, "myapp", resumeLedger, queue, dedup, func(string, ...any) {}); err != nil {
		t.Fatal(err)
	}

	run.waitCall(t)
	run.waitIdle(t, queue)
	if run.count() != 1 {
		t.Errorf("resume ran %d deploys, want exactly 1 (the admitted-unprocessed entry)", run.count())
	}
	// The resumed admission is marked processed in the resume ledger.
	if got := len(resumeLedger.byKind(autodeploy.AdmissionKindProcessed)); got != 1 {
		t.Errorf("resume processed marks = %d, want 1", got)
	}
	// Dedup reseeded from the ledger: redelivering B's digest reads as replay.
	if !dedup.SeenAndRecord(autodeploy.ContentIDFromDigest("bbbb")) {
		t.Error("resume did not reseed dedup with the admitted digest (redelivery would re-admit)")
	}
	if dedup.SeenAndRecord(autodeploy.ContentIDFromDigest("zzzz")) {
		t.Error("unrelated digest incorrectly seeded into dedup")
	}
}

// TestAdmission_ResumeNewestWinsAndDupesSkipped: several pending
// admissions (including a shared delivery id and a shared digest) collapse
// to ONE deploy of the newest entry.
func TestAdmission_ResumeNewestWinsAndDupesSkipped(t *testing.T) {
	dir := t.TempDir()
	ledgerPath := filepath.Join(dir, ".autodeploy-ledger.jsonl")
	fileLedger, err := autodeploy.OpenLedger(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Add(-time.Minute)
	admits := []struct {
		id, delivery, digest string
		offset               time.Duration
	}{
		{"id-old", "same-delivery", "old-digest", 0},
		{"id-mid", "same-delivery", "mid-digest", time.Second}, // reused delivery id
		{"id-new", "same-delivery", "new-digest", 2 * time.Second},
	}
	for _, a := range admits {
		if err := fileLedger.Append(autodeploy.AdmissionRecord{
			Kind: autodeploy.AdmissionKindAdmitted, ID: a.id, Delivery: a.delivery,
			Digest: a.digest, App: "myapp", Branch: "main", Received: base.Add(a.offset),
		}); err != nil {
			t.Fatal(err)
		}
	}

	run := newCountingRun()
	resumeLedger := &memLedger{}
	queue := newAdmissionQueue(resumeLedger, run.run, func(string, ...any) {})
	if err := resumeAdmissions(ledgerPath, "myapp", resumeLedger, queue, autodeploy.NewDeliveryDedup(), func(string, ...any) {}); err != nil {
		t.Fatal(err)
	}

	run.waitCall(t)
	run.waitIdle(t, queue)
	if run.count() != 1 {
		t.Errorf("resume ran %d deploys, want 1 (newest wins; duplicates skipped)", run.count())
	}
	// The older pendings were marked superseded by the newest.
	superseded := resumeLedger.byKind(autodeploy.AdmissionKindSuperseded)
	if len(superseded) != 2 {
		t.Fatalf("resume superseded marks = %d, want 2", len(superseded))
	}
	for _, s := range superseded {
		if s.SupersededBy != "id-new" {
			t.Errorf("superseded_by = %q, want id-new", s.SupersededBy)
		}
	}
}

// TestAdmission_ResumeEmptyAndMissing: no ledger file and an all-processed
// ledger are both quiet resumes.
func TestAdmission_ResumeEmptyAndMissing(t *testing.T) {
	run := newCountingRun()
	q := newAdmissionQueue(&memLedger{}, run.run, func(string, ...any) {})

	if err := resumeAdmissions(filepath.Join(t.TempDir(), "nope.jsonl"), "myapp", &memLedger{}, q, autodeploy.NewDeliveryDedup(), func(string, ...any) {}); err != nil {
		t.Fatalf("missing ledger must not error: %v", err)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.jsonl")
	fl, err := autodeploy.OpenLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := fl.Append(autodeploy.AdmissionRecord{Kind: autodeploy.AdmissionKindAdmitted, ID: "x", Digest: "d", App: "myapp", Received: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := fl.Append(autodeploy.AdmissionRecord{Kind: autodeploy.AdmissionKindProcessed, ID: "x", Digest: "d", App: "myapp", Received: time.Now().UTC(), At: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := resumeAdmissions(path, "myapp", &memLedger{}, q, autodeploy.NewDeliveryDedup(), func(string, ...any) {}); err != nil {
		t.Fatal(err)
	}
	run.waitIdle(t, q)
	if run.count() != 0 {
		t.Errorf("all-processed ledger re-triggered %d deploys, want 0", run.count())
	}
}

// TestAdmission_SameDigestWhileQueuedIsDuplicate: a redelivery of the
// exact same signed body while it sits in the pending slot is a duplicate
// (dedup path), not a second admission.
func TestAdmission_SameDigestWhileQueuedIsDuplicate(t *testing.T) {
	block := make(chan struct{})
	run := newCountingRun().blocking(block)
	handler, ledger, queue := newAdmissionStack("s3cret", "main", "myapp", run.run)

	postSigned(t, handler, "s3cret", "w", `{"ref":"refs/heads/main","after":"w"}`)
	run.waitCall(t)
	body := `{"ref":"refs/heads/main","after":"q"}`
	if rec := postSigned(t, handler, "s3cret", "q1", body); !strings.Contains(rec.Body.String(), `"queued"`) {
		t.Fatalf("first copy disposition = %q, want queued", rec.Body.String())
	}
	if rec := postSigned(t, handler, "s3cret", "q2", body); !strings.Contains(rec.Body.String(), `"duplicate"`) {
		t.Fatalf("redelivery while pending = %q, want duplicate", rec.Body.String())
	}

	close(block)
	run.waitCall(t)
	run.waitIdle(t, queue)
	if run.count() != 2 {
		t.Errorf("deploy ran %d times, want 2", run.count())
	}
	if got := len(ledger.byKind(autodeploy.AdmissionKindAdmitted)); got != 2 {
		t.Errorf("admissions = %d, want 2 (W and one Q — the redelivery is not admitted)", got)
	}
}

// TestAdmission_LedgerFileDurability: the real FileLedger round-trips
// through ParseLedger and FoldAdmissions, ignores a torn tail, and refuses
// mid-file corruption.
func TestAdmission_LedgerFileDurability(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.jsonl")
	fl, err := autodeploy.OpenLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	recs := []autodeploy.AdmissionRecord{
		{Kind: autodeploy.AdmissionKindAdmitted, ID: "1", Digest: "d1", App: "myapp", Received: now},
		{Kind: autodeploy.AdmissionKindSuperseded, ID: "1", Digest: "d1", App: "myapp", Received: now, SupersededBy: "2", At: now},
		{Kind: autodeploy.AdmissionKindAdmitted, ID: "2", Digest: "d2", App: "myapp", Received: now.Add(time.Second)},
		{Kind: autodeploy.AdmissionKindProcessed, ID: "2", Digest: "d2", App: "myapp", Received: now.Add(time.Second), At: now.Add(2 * time.Second)},
	}
	for _, r := range recs {
		if err := fl.Append(r); err != nil {
			t.Fatal(err)
		}
	}
	fl.Close()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := autodeploy.ParseLedger(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed) != 4 {
		t.Fatalf("parsed %d records, want 4", len(parsed))
	}
	pending, digests := autodeploy.FoldAdmissions(parsed)
	if len(pending) != 0 {
		t.Errorf("pending = %d, want 0 (1 superseded, 2 processed)", len(pending))
	}
	if len(digests) != 2 {
		t.Errorf("digests = %d, want 2", len(digests))
	}

	// Torn tail (crash mid-append, no trailing newline) is ignored.
	torn := append(data, []byte(`{"kind":"admitted","id":"3","dig`)...)
	parsed, err = autodeploy.ParseLedger(torn)
	if err != nil {
		t.Fatalf("torn tail must be ignored: %v", err)
	}
	if len(parsed) != 4 {
		t.Errorf("torn tail parsed %d records, want 4", len(parsed))
	}

	// Mid-file corruption fails closed.
	corrupt := []byte("{\"kind\":\"admitted\"\n{\"kind\":\"processed\"}\n")
	if _, err := autodeploy.ParseLedger(corrupt); !errors.Is(err, autodeploy.ErrLedgerCorrupt) {
		t.Errorf("mid-file corruption = %v, want ErrLedgerCorrupt", err)
	}

	// Unknown kind fails closed.
	unknown := []byte("{\"kind\":\"weird\"}\n")
	if _, err := autodeploy.ParseLedger(unknown); !errors.Is(err, autodeploy.ErrLedgerCorrupt) {
		t.Errorf("unknown kind = %v, want ErrLedgerCorrupt", err)
	}

	// Ledger file is private.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("ledger mode = %v, want 0600", info.Mode().Perm())
	}
}
