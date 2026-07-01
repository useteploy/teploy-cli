package autodeploy

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"
)

func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestVerifyGitHubSignature_Valid(t *testing.T) {
	body := []byte(`{"ref":"refs/heads/main"}`)
	header := sign("mysecret", body)
	if !VerifyGitHubSignature("mysecret", body, header) {
		t.Error("expected valid signature to verify")
	}
}

func TestVerifyGitHubSignature_WrongSecret(t *testing.T) {
	body := []byte(`{"ref":"refs/heads/main"}`)
	header := sign("mysecret", body)
	if VerifyGitHubSignature("wrongsecret", body, header) {
		t.Error("expected wrong-secret signature to fail")
	}
}

func TestVerifyGitHubSignature_TamperedBody(t *testing.T) {
	header := sign("mysecret", []byte(`{"ref":"refs/heads/main"}`))
	// Same signature, different body — must fail.
	if VerifyGitHubSignature("mysecret", []byte(`{"ref":"refs/heads/evil"}`), header) {
		t.Error("expected tampered body to fail verification")
	}
}

func TestVerifyGitHubSignature_MissingPrefix(t *testing.T) {
	body := []byte("data")
	// Header without the "sha256=" prefix must be rejected, not parsed
	// leniently.
	rawHex := hex.EncodeToString([]byte("not-a-real-mac"))
	if VerifyGitHubSignature("secret", body, rawHex) {
		t.Error("expected header without sha256= prefix to be rejected")
	}
}

func TestVerifyGitHubSignature_MalformedHex(t *testing.T) {
	if VerifyGitHubSignature("secret", []byte("data"), "sha256=not-hex!!") {
		t.Error("expected malformed hex to be rejected, not panic or false-positive")
	}
}

func TestVerifyGitLabToken(t *testing.T) {
	if !VerifyGitLabToken("mytoken", "mytoken") {
		t.Error("expected matching token to verify")
	}
	if VerifyGitLabToken("mytoken", "wrongtoken") {
		t.Error("expected mismatched token to fail")
	}
	if VerifyGitLabToken("mytoken", "") {
		t.Error("expected empty header to fail")
	}
	if VerifyGitLabToken("", "anything") {
		t.Error("expected empty configured secret to fail closed, not match everything")
	}
}

// TestDeliveryDedup_RejectsReplay is the regression test for the missing
// replay protection in the old bash listener: a captured valid
// payload+signature could be replayed indefinitely to re-trigger deploys.
func TestDeliveryDedup_RejectsReplay(t *testing.T) {
	d := NewDeliveryDedup()
	if d.SeenAndRecord("abc-123") {
		t.Error("first sight of a delivery ID should not be flagged as a replay")
	}
	if !d.SeenAndRecord("abc-123") {
		t.Error("second sight of the same delivery ID must be flagged as a replay")
	}
}

func TestDeliveryDedup_DifferentIDsIndependent(t *testing.T) {
	d := NewDeliveryDedup()
	d.SeenAndRecord("id-1")
	if d.SeenAndRecord("id-2") {
		t.Error("a different delivery ID must not be treated as a replay of an unrelated one")
	}
}

func TestDeliveryDedup_SnapshotRoundTrip(t *testing.T) {
	d := NewDeliveryDedup()
	d.SeenAndRecord("id-1")
	d.SeenAndRecord("id-2")

	data, err := d.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	restored := LoadDeliveryDedup(data)
	if !restored.SeenAndRecord("id-1") {
		t.Error("restored dedup set should still remember id-1 as seen")
	}
	if restored.SeenAndRecord("id-3") {
		t.Error("restored dedup set should not treat a never-seen ID as a replay")
	}
}

func TestLoadDeliveryDedup_EmptyOrInvalidData(t *testing.T) {
	for _, data := range [][]byte{nil, {}, []byte("not json")} {
		d := LoadDeliveryDedup(data)
		if d.SeenAndRecord("id-1") {
			t.Errorf("LoadDeliveryDedup(%v) should start empty, not treat id-1 as already seen", data)
		}
	}
}

func TestDeliveryDedup_PrunesStaleEntries(t *testing.T) {
	d := NewDeliveryDedup()
	// Manually seed an entry older than deliveryTTL to test pruning without
	// waiting 24h in a test.
	d.mu.Lock()
	d.seen["stale-id"] = time.Now().Add(-deliveryTTL - time.Hour)
	d.mu.Unlock()

	// Recording a new ID triggers the prune sweep as a side effect.
	d.SeenAndRecord("fresh-id")

	d.mu.Lock()
	_, stillPresent := d.seen["stale-id"]
	d.mu.Unlock()
	if stillPresent {
		t.Error("expected stale entry to be pruned")
	}
}
