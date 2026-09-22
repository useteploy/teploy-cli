package autodeploy

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// The webhook admission ledger (C02): an append-only record of every
// delivery this listener durably admitted, so the 200 a provider receives
// is backed by bytes on disk that survive a crash immediately after the
// response. One JSON record per line at /deployments/<app>/
// .autodeploy-ledger.jsonl (the serve process runs on the server itself,
// like the dedup file next to it).
//
// Record kinds:
//
//	admitted   — a verified push to the watched branch was accepted
//	superseded — a pending (not yet running) admission was replaced by a
//	             newer delivery; superseded_by names the replacement
//	processed  — the deploy the admission caused ran to completion
//
// Folding the file yields the admitted-but-never-terminal records: on
// restart the serve process replays exactly those (newest per app wins),
// which is the correct webhook contract — unlike a UI admission write, the
// provider will NOT redeliver an event it was told was accepted.

// Admission record kinds (see the ledger commentary above).
const (
	AdmissionKindAdmitted   = "admitted"
	AdmissionKindSuperseded = "superseded"
	AdmissionKindProcessed  = "processed"
)

// AdmissionRecord is one line of the admission ledger. The identity fields
// (ID/Digest/App/Branch/Received) are carried on every record so a folded
// line is self-describing; transition kinds add their own fields.
type AdmissionRecord struct {
	Kind     string `json:"kind"`
	ID       string `json:"id"`
	Delivery string `json:"delivery,omitempty"` // provider delivery header, metadata only (A36)
	Digest   string `json:"digest"`             // hex sha256 of the AUTHENTICATED body
	App      string `json:"app"`
	Branch   string `json:"branch,omitempty"`
	// Commit is the commit the authenticated push named as the branch's new
	// head (payload after/checkout_sha; see PushCommit). Carried through
	// superseded/processed transitions so restart resume re-triggers the
	// delivery PINNED to its commit, not the moving tip. Empty when the
	// payload carried no usable commit (tip deploy).
	Commit       string    `json:"commit,omitempty"`
	Received     time.Time `json:"received"`
	SupersededBy string    `json:"superseded_by,omitempty"`
	At           time.Time `json:"at,omitempty"` // transition time (superseded/processed)
}

// NewAdmissionID mints a unique id for an admission record.
func NewAdmissionID() string {
	var id [16]byte
	if _, err := rand.Read(id[:]); err == nil {
		return hex.EncodeToString(id[:])
	}
	fallback := sha256.Sum256([]byte(time.Now().UTC().Format(time.RFC3339Nano)))
	return hex.EncodeToString(fallback[:16])
}

// ContentID is the dedup/ledger key for an authenticated body: the same
// digest the DeliveryDedup records, derived here so resume can seed dedup
// from ledger records without re-reading bodies.
func ContentID(body []byte) string {
	sum := sha256.Sum256(body)
	return "content:" + hex.EncodeToString(sum[:])
}

// ContentIDFromDigest is ContentID for a digest already computed (ledger
// records store the bare hex digest).
func ContentIDFromDigest(digestHex string) string {
	return "content:" + digestHex
}

// LedgerAppender durably appends one record: when Append returns nil, the
// record is on disk and fsynced — that is the admission durability point
// the webhook handler acks against. The interface is the test seam for
// ordering and failure injection.
type LedgerAppender interface {
	Append(rec AdmissionRecord) error
}

// FileLedger is the on-disk append-only ledger. Opens the file
// O_APPEND|O_CREATE (0600 — same privacy class as the dedup file) and
// fsyncs after every appended line.
type FileLedger struct {
	path string
	mu   chan struct{} // binary semaphore; keeps Append a mutex-free test target
	f    *os.File
}

// OpenLedger opens (or creates) the ledger at path for durable appends.
func OpenLedger(path string) (*FileLedger, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return nil, fmt.Errorf("opening webhook admission ledger %s: %w", path, err)
	}
	return &FileLedger{path: path, mu: make(chan struct{}, 1), f: f}, nil
}

// Append writes one record as a single line and fsyncs before returning.
// A non-nil error means the record MUST be treated as not durable.
func (l *FileLedger) Append(rec AdmissionRecord) error {
	line, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("encoding admission record: %w", err)
	}
	line = append(line, '\n')

	l.mu <- struct{}{}
	_, werr := l.f.Write(line)
	if werr == nil {
		werr = l.f.Sync()
	}
	<-l.mu
	if werr != nil {
		return fmt.Errorf("appending to webhook admission ledger %s: %w", l.path, werr)
	}
	return nil
}

// Close closes the underlying file.
func (l *FileLedger) Close() error {
	if l == nil || l.f == nil {
		return nil
	}
	return l.f.Close()
}

// ErrLedgerCorrupt flags a ledger line that cannot be parsed mid-file —
// the fold refuses to guess around unknown history.
var ErrLedgerCorrupt = errors.New("webhook admission ledger corrupt")

// ParseLedger parses raw ledger bytes into records. A torn FINAL line (no
// trailing newline — a crash mid-append, never fsynced, never acked) is
// ignored; any other unparseable line fails with ErrLedgerCorrupt.
func ParseLedger(data []byte) ([]AdmissionRecord, error) {
	if len(data) == 0 {
		return nil, nil
	}
	torn := len(data) > 0 && data[len(data)-1] != '\n'
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if torn {
		lines = lines[:len(lines)-1]
	}
	recs := make([]AdmissionRecord, 0, len(lines))
	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec AdmissionRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			return nil, fmt.Errorf("%w: %s line %d: %v", ErrLedgerCorrupt, "ledger", i+1, err)
		}
		switch rec.Kind {
		case AdmissionKindAdmitted, AdmissionKindSuperseded, AdmissionKindProcessed:
		default:
			return nil, fmt.Errorf("%w: unknown record kind %q at line %d", ErrLedgerCorrupt, rec.Kind, i+1)
		}
		recs = append(recs, rec)
	}
	return recs, nil
}

// FoldedAdmission is the folded state of the ledger: the records still
// admitted-and-unprocessed, in admission order.
type FoldedAdmission struct {
	Record AdmissionRecord
}

// FoldAdmissions folds parsed records into (a) the pending admissions —
// admitted, never superseded, never processed, in admission order — and
// (b) every admitted digest with its received time, so restart can reseed
// the replay dedup from the durable ledger even when the best-effort
// dedup file lost an entry.
func FoldAdmissions(recs []AdmissionRecord) (pending []AdmissionRecord, digests map[string]time.Time) {
	digests = make(map[string]time.Time)
	state := make(map[string]string) // id -> admitted|superseded|processed
	for _, rec := range recs {
		switch rec.Kind {
		case AdmissionKindAdmitted:
			state[rec.ID] = AdmissionKindAdmitted
			if _, ok := digests[rec.Digest]; !ok {
				digests[rec.Digest] = rec.Received
			}
		case AdmissionKindSuperseded, AdmissionKindProcessed:
			if prev, ok := state[rec.ID]; ok && prev == AdmissionKindAdmitted {
				state[rec.ID] = rec.Kind
			}
		}
	}
	for _, rec := range recs {
		if rec.Kind == AdmissionKindAdmitted && state[rec.ID] == AdmissionKindAdmitted {
			pending = append(pending, rec)
		}
	}
	return pending, digests
}

// NewestPending returns the newest pending admission for app from a folded
// pending list (the last admitted-unprocessed record for that app), or nil.
func NewestPending(pending []AdmissionRecord, app string) *AdmissionRecord {
	var newest *AdmissionRecord
	for i := range pending {
		if pending[i].App == app && (newest == nil || !pending[i].Received.Before(newest.Received)) {
			p := pending[i]
			newest = &p
		}
	}
	return newest
}

// SeedDedupFromLedger records recent admitted digests into a dedup set so
// a redelivery after a restart is rejected as the replay it is even when
// the best-effort dedup file never persisted the entry. Entries older than
// the delivery TTL are skipped (matching the dedup prune window).
func SeedDedupFromLedger(d *DeliveryDedup, digests map[string]time.Time, now time.Time) {
	var keys []string
	for k := range digests {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, digest := range keys {
		t := digests[digest]
		if now.Sub(t) > deliveryTTL {
			continue
		}
		d.RecordOnly(ContentIDFromDigest(digest))
	}
}
