package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// verifyAsReceiver is written the way a receiving service must write it —
// independently of sign.go, recomputing the HMAC from the raw body and the
// timestamp header. If this and signRequest ever disagree, one of them is
// wrong, which is exactly what these tests are for.
func verifyAsReceiver(secret string, body []byte, tsHeader, sigHeader string) bool {
	const prefix = "sha256="
	if !strings.HasPrefix(sigHeader, prefix) || tsHeader == "" {
		return false
	}
	got, err := hex.DecodeString(strings.TrimPrefix(sigHeader, prefix))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(tsHeader))
	mac.Write([]byte("."))
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

// captured is what a receiver sees: the exact bytes plus the two headers.
type captured struct {
	body []byte
	ts   string
	sig  string
}

func captureServer(t *testing.T, into *captured) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading body: %v", err)
		}
		into.body = body
		into.ts = r.Header.Get(timestampHeader)
		into.sig = r.Header.Get(signatureHeader)
		w.WriteHeader(http.StatusOK)
	}))
}

func TestSend_UnsignedWhenNoSecret(t *testing.T) {
	var got captured
	srv := captureServer(t, &got)
	defer srv.Close()

	n := NewNotifier(srv.URL, "")
	if err := n.Send(context.Background(), Payload{App: "app", Type: "deploy", Success: true}); err != nil {
		t.Fatalf("send: %v", err)
	}
	// An install with no secret configured must keep delivering exactly what it
	// always delivered: a receiver that rejects unknown headers stays working.
	if got.sig != "" || got.ts != "" {
		t.Errorf("expected no signature headers without a secret, got sig=%q ts=%q", got.sig, got.ts)
	}
}

func TestSend_SignatureVerifies(t *testing.T) {
	var got captured
	srv := captureServer(t, &got)
	defer srv.Close()

	const secret = "s3cr3t"
	n := NewNotifier(srv.URL, secret)
	if err := n.Send(context.Background(), Payload{App: "akiroo-lite", Type: "deploy", Success: true}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if got.sig == "" {
		t.Fatal("expected a signature header")
	}
	if !verifyAsReceiver(secret, got.body, got.ts, got.sig) {
		t.Error("signature did not verify against the received body")
	}
}

func TestSend_SignatureRejectsWrongSecret(t *testing.T) {
	var got captured
	srv := captureServer(t, &got)
	defer srv.Close()

	n := NewNotifier(srv.URL, "right")
	if err := n.Send(context.Background(), Payload{App: "app", Type: "deploy"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if verifyAsReceiver("wrong", got.body, got.ts, got.sig) {
		t.Error("signature verified under the wrong secret")
	}
}

func TestSend_SignatureCoversBody(t *testing.T) {
	var got captured
	srv := captureServer(t, &got)
	defer srv.Close()

	const secret = "s3cr3t"
	n := NewNotifier(srv.URL, secret)
	if err := n.Send(context.Background(), Payload{App: "app", Type: "deploy", Success: false}); err != nil {
		t.Fatalf("send: %v", err)
	}
	tampered := append([]byte{}, got.body...)
	tampered[0] ^= 0xff
	if verifyAsReceiver(secret, tampered, got.ts, got.sig) {
		t.Error("signature verified over a tampered body")
	}
}

func TestSend_SignatureCoversTimestamp(t *testing.T) {
	var got captured
	srv := captureServer(t, &got)
	defer srv.Close()

	const secret = "s3cr3t"
	n := NewNotifier(srv.URL, secret)
	if err := n.Send(context.Background(), Payload{App: "app", Type: "deploy"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	// Binding the timestamp into the MAC is what makes a replay window
	// enforceable: an attacker cannot move a captured delivery forward in time.
	if verifyAsReceiver(secret, got.body, "1", got.sig) {
		t.Error("signature verified with a substituted timestamp")
	}
}

func TestSend_TimestampIsCurrentUnixSeconds(t *testing.T) {
	var got captured
	srv := captureServer(t, &got)
	defer srv.Close()

	n := NewNotifier(srv.URL, "s3cr3t")
	if err := n.Send(context.Background(), Payload{App: "app", Type: "deploy"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	ts, err := strconv.ParseInt(got.ts, 10, 64)
	if err != nil {
		t.Fatalf("timestamp header %q is not unix seconds: %v", got.ts, err)
	}
	if skew := time.Since(time.Unix(ts, 0)); skew < 0 || skew > time.Minute {
		t.Errorf("timestamp skew %v outside the acceptable window", skew)
	}
}

func TestMultiNotifier_SignsWebhookChannel(t *testing.T) {
	var got captured
	srv := captureServer(t, &got)
	defer srv.Close()

	const secret = "chan-secret"
	n := NewMultiNotifier([]Channel{{Type: "webhook", URL: srv.URL, Secret: secret}})
	if errs := n.Send(context.Background(), Payload{App: "app", Type: "deploy"}); len(errs) > 0 {
		t.Fatalf("send: %v", errs)
	}
	if !verifyAsReceiver(secret, got.body, got.ts, got.sig) {
		t.Error("per-channel secret did not produce a verifiable signature")
	}
}

func TestMultiNotifier_DoesNotSignSlackOrNtfy(t *testing.T) {
	// Slack and ntfy define their own request shapes and never verify these
	// headers, so sending them would be a promise nobody checks.
	for _, typ := range []string{"slack", "ntfy"} {
		t.Run(typ, func(t *testing.T) {
			var got captured
			srv := captureServer(t, &got)
			defer srv.Close()

			n := NewMultiNotifier([]Channel{{Type: typ, URL: srv.URL, Secret: "unused"}})
			if errs := n.Send(context.Background(), Payload{App: "app", Type: "deploy"}); len(errs) > 0 {
				t.Fatalf("send: %v", errs)
			}
			if got.sig != "" || got.ts != "" {
				t.Errorf("%s delivery carried signature headers (sig=%q ts=%q)", typ, got.sig, got.ts)
			}
		})
	}
}

// TestSignatureSchemeMatchesObserve pins the wire format against a fixed
// vector. teploy-observe signs identically (internal/platform/webhooks.go), and
// the whole point of that agreement is that a consumer of both products writes
// one verifier. A change here silently breaks every such receiver, so the
// expected value is spelled out rather than recomputed from signature().
func TestSignatureSchemeMatchesObserve(t *testing.T) {
	const (
		secret = "test-secret"
		ts     = "1700000000"
	)
	body := []byte(`{"app":"demo"}`)

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "." + string(body)))
	want := hex.EncodeToString(mac.Sum(nil))

	if got := hex.EncodeToString(signature(secret, ts, body)); got != want {
		t.Errorf("framing changed: signature() = %s, want HMAC over timestamp+\".\"+body = %s", got, want)
	}
}
