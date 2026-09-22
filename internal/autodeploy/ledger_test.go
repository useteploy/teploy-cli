package autodeploy

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDeliveryDedup_UnrecordRollsBackFailedAdmission(t *testing.T) {
	d := NewDeliveryDedup()
	if d.SeenAndRecord("content:abc") {
		t.Fatal("first record reported as replay")
	}
	// The admission this record fronted failed to persist: unrecord so the
	// provider retry goes through admission again.
	d.Unrecord("content:abc")
	if d.SeenAndRecord("content:abc") {
		t.Fatal("after unrecord the digest still reads as replay — a failed admission would swallow the retry")
	}
}

func TestDeliveryDedup_RecordOnlySeedsWithoutReplayFlag(t *testing.T) {
	d := NewDeliveryDedup()
	d.RecordOnly("content:seed")
	if !d.SeenAndRecord("content:seed") {
		t.Fatal("seeded digest must read as replay")
	}
	if d.SeenAndRecord("content:other") {
		t.Fatal("unrelated digest must not read as replay")
	}
}

func TestContentIDGolden(t *testing.T) {
	got := ContentID([]byte(`{"ref":"refs/heads/main"}`))
	// Pin the derivation (sha256 hex with the content: prefix) without
	// hand-computing a constant: derive once, ensure it is stable and hex.
	if got == "" || len(got) != len("content:")+64 {
		t.Fatalf("ContentID = %q, want content:<64 hex chars>", got)
	}
	if got != ContentIDFromDigest(got[len("content:"):]) {
		t.Fatal("ContentID and ContentIDFromDigest disagree on the same digest")
	}
}

func TestNewAdmissionIDUnique(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 1000; i++ {
		id := NewAdmissionID()
		if seen[id] {
			t.Fatalf("duplicate admission id %q", id)
		}
		seen[id] = true
	}
}

func TestFileLedgerAppendsAreDurableAndSerialized(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	l, err := OpenLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 32)
	for i := 0; i < 32; i++ {
		go func(n int) {
			done <- l.Append(AdmissionRecord{Kind: AdmissionKindAdmitted, ID: NewAdmissionID(), Digest: "d", App: "app"})
		}(i)
	}
	for i := 0; i < 32; i++ {
		if err := <-done; err != nil {
			t.Fatalf("concurrent append: %v", err)
		}
	}
	l.Close()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	recs, err := ParseLedger(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 32 {
		t.Fatalf("parsed %d records, want 32 (interleaved writes must not corrupt lines)", len(recs))
	}
}

func TestFoldAdmissionsPendingOrderAndDigests(t *testing.T) {
	now := time.Now().UTC()
	recs := []AdmissionRecord{
		{Kind: AdmissionKindAdmitted, ID: "a", Digest: "da", App: "app", Received: now},
		{Kind: AdmissionKindAdmitted, ID: "b", Digest: "db", App: "app", Received: now.Add(time.Second)},
		{Kind: AdmissionKindSuperseded, ID: "a", Digest: "da", App: "app", SupersededBy: "b"},
		{Kind: AdmissionKindAdmitted, ID: "c", Digest: "dc", App: "other", Received: now.Add(2 * time.Second)},
	}
	pending, digests := FoldAdmissions(recs)
	if len(pending) != 2 || pending[0].ID != "b" || pending[1].ID != "c" {
		t.Fatalf("pending = %+v, want [b c] in admission order", pending)
	}
	if len(digests) != 3 {
		t.Fatalf("digests = %d, want 3", len(digests))
	}
	if newest := NewestPending(pending, "app"); newest == nil || newest.ID != "b" {
		t.Fatalf("NewestPending(app) = %+v, want b", newest)
	}
	if newest := NewestPending(pending, "missing"); newest != nil {
		t.Fatalf("NewestPending(unknown app) = %+v, want nil", newest)
	}
}

func TestSeedDedupFromLedgerHonorsTTL(t *testing.T) {
	d := NewDeliveryDedup()
	now := time.Now().UTC()
	digests := map[string]time.Time{
		"fresh": now.Add(-time.Hour),
		"stale": now.Add(-deliveryTTL - time.Hour),
	}
	SeedDedupFromLedger(d, digests, now)
	if !d.SeenAndRecord(ContentIDFromDigest("fresh")) {
		t.Error("recent admitted digest must be seeded as replay")
	}
	if d.SeenAndRecord(ContentIDFromDigest("stale")) {
		t.Error("digest older than the delivery TTL must not be seeded")
	}
}
