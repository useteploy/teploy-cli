package notify

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"time"
)

// Webhook deliveries are signed so a receiver can tell a real delivery from
// anyone who learned the URL. Without this, the URL is the only credential and
// a webhook endpoint has to either trust every caller or sit behind a network
// boundary.
//
// The construction is deliberately identical to teploy-observe's
// (internal/platform/webhooks.go fireHTTP):
//
//	X-Teploy-Timestamp: <unix seconds>
//	X-Teploy-Signature: sha256=hex(HMAC-SHA256(secret, timestamp + "." + body))
//
// Signing the timestamp together with the body — rather than the body alone,
// GitHub-style — is what lets a receiver bound replay: a captured delivery is
// only valid inside whatever window the receiver chooses to accept, and the
// timestamp can't be edited without invalidating the signature. Keeping the
// scheme byte-identical to observe's means a consumer of both products writes
// one verifier and parameterizes the header name, instead of one verifier per
// product.
const (
	signatureHeader = "X-Teploy-Signature"
	timestampHeader = "X-Teploy-Timestamp"
)

// signRequest attaches the timestamp and signature headers for body. An empty
// secret is a no-op: signing is opt-in, and an unconfigured secret must leave
// existing unsigned consumers working rather than shipping a header they would
// reject.
func signRequest(req *http.Request, secret string, body []byte) {
	if secret == "" {
		return
	}
	ts := strconv.FormatInt(time.Now().UTC().Unix(), 10)
	req.Header.Set(timestampHeader, ts)
	req.Header.Set(signatureHeader, "sha256="+hex.EncodeToString(signature(secret, ts, body)))
}

// signature computes the raw HMAC over timestamp + "." + body. Split out so a
// test can recompute the expected value without reimplementing the framing —
// a test that hardcodes its own concatenation stops catching a change to it.
func signature(secret, timestamp string, body []byte) []byte {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(body)
	return mac.Sum(nil)
}
