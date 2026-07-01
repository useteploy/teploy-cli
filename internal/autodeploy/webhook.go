package autodeploy

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"time"
)

// VerifyGitHubSignature checks a GitHub webhook's X-Hub-Signature-256
// header ("sha256=<hex>") against an HMAC-SHA256 of body computed with
// secret, using a constant-time comparison (hmac.Equal) so response timing
// can't leak information about the correct signature — the previous bash
// listener compared with a plain `[ "$SIG" != "$EXPECTED" ]`, not
// constant-time.
func VerifyGitHubSignature(secret string, body []byte, header string) bool {
	const prefix = "sha256="
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	got, err := hex.DecodeString(strings.TrimPrefix(header, prefix))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	want := mac.Sum(nil)
	return hmac.Equal(got, want)
}

// VerifyGitLabToken checks a GitLab webhook's X-Gitlab-Token header, which
// (unlike GitHub) is the raw shared secret sent directly rather than an
// HMAC — constant-time compare via subtle.ConstantTimeCompare so response
// timing can't leak the correct secret.
func VerifyGitLabToken(secret, header string) bool {
	if header == "" || secret == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(secret), []byte(header)) == 1
}

// deliveryTTL bounds how long a delivery ID is remembered for replay
// rejection, and how far back DeliveryDedup.prune sweeps.
const deliveryTTL = 24 * time.Hour

// DeliveryDedup rejects a webhook delivery ID (GitHub's X-GitHub-Delivery,
// GitLab has no equivalent so it's synthesized by the caller) that's
// already been seen, so a captured valid webhook payload+signature can't
// be replayed to re-trigger deploys indefinitely — the previous bash
// listener had no replay protection at all. Entries older than deliveryTTL
// are pruned on every check so the backing store stays bounded regardless
// of traffic volume.
//
// Not safe for concurrent use across processes — fine here since `teploy
// autodeploy serve` is a single resident process per app.
type DeliveryDedup struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

// NewDeliveryDedup creates an empty in-memory dedup set. Callers that want
// it to survive a process restart should call LoadDeliveryDedup instead.
func NewDeliveryDedup() *DeliveryDedup {
	return &DeliveryDedup{seen: make(map[string]time.Time)}
}

// SeenAndRecord reports whether id has already been recorded (a replay),
// and if not, records it. Also prunes entries older than deliveryTTL.
func (d *DeliveryDedup) SeenAndRecord(id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()

	now := time.Now()
	for k, t := range d.seen {
		if now.Sub(t) > deliveryTTL {
			delete(d.seen, k)
		}
	}

	if _, ok := d.seen[id]; ok {
		return true
	}
	d.seen[id] = now
	return false
}

// Snapshot returns a JSON-serializable copy for persisting across process
// restarts (see LoadDeliveryDedup).
func (d *DeliveryDedup) Snapshot() ([]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return json.Marshal(d.seen)
}

// LoadDeliveryDedup restores a DeliveryDedup from a Snapshot. A nil or
// empty/invalid data argument yields an empty set rather than an error —
// losing dedup history across a restart degrades to "briefly more
// permissive," not a hard failure that should block the webhook listener
// from starting.
func LoadDeliveryDedup(data []byte) *DeliveryDedup {
	d := NewDeliveryDedup()
	if len(data) == 0 {
		return d
	}
	_ = json.Unmarshal(data, &d.seen)
	if d.seen == nil {
		d.seen = make(map[string]time.Time)
	}
	return d
}
