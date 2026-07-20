package cli

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/autodeploy"
)

func githubSign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// TestWebhookHandler_ValidSignatureTriggersDeployOnce is the direct
// regression test for the old autodeploy's worst bug: the generated bash
// listener verified the signature correctly but never called anything
// resembling a real deploy — it only built an image and stopped. This
// confirms a valid, well-formed webhook actually calls trigger exactly
// once.
func TestWebhookHandler_ValidSignatureTriggersDeployOnce(t *testing.T) {
	secret := "s3cret"
	body := []byte(`{"ref":"refs/heads/main"}`)
	triggerCount := 0

	handler := newWebhookHandler(webhookHandlerConfig{
		secret:  secret,
		dedup:   autodeploy.NewDeliveryDedup(),
		trigger: func(_ []string, _ bool) { triggerCount++ },
	})

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(body)))
	req.Header.Set("X-Hub-Signature-256", githubSign(secret, body))
	req.Header.Set("X-GitHub-Delivery", "delivery-1")
	rec := httptest.NewRecorder()

	handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if triggerCount != 1 {
		t.Errorf("trigger called %d times, want 1", triggerCount)
	}
}

func TestWebhookHandler_InvalidSignatureNeverTriggers(t *testing.T) {
	triggered := false
	handler := newWebhookHandler(webhookHandlerConfig{
		secret:  "s3cret",
		dedup:   autodeploy.NewDeliveryDedup(),
		trigger: func(_ []string, _ bool) { triggered = true },
	})

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"ref":"refs/heads/main"}`))
	req.Header.Set("X-Hub-Signature-256", "sha256=0000000000000000000000000000000000000000000000000000000000000000")
	rec := httptest.NewRecorder()

	handler(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if triggered {
		t.Error("an invalid signature must never trigger a deploy")
	}
}

func TestWebhookHandler_NoSignatureNeverTriggers(t *testing.T) {
	triggered := false
	handler := newWebhookHandler(webhookHandlerConfig{
		secret:  "s3cret",
		dedup:   autodeploy.NewDeliveryDedup(),
		trigger: func(_ []string, _ bool) { triggered = true },
	})

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()

	handler(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if triggered {
		t.Error("a request with no signature header must never trigger a deploy")
	}
}

// TestWebhookHandler_ReplayedDeliveryIgnored is the regression test for
// the missing replay protection in the old bash listener: a captured
// valid payload+signature could be replayed indefinitely to re-trigger
// deploys.
func TestWebhookHandler_ReplayedDeliveryIgnored(t *testing.T) {
	secret := "s3cret"
	body := []byte(`{"ref":"refs/heads/main"}`)
	triggerCount := 0
	dedup := autodeploy.NewDeliveryDedup()

	handler := newWebhookHandler(webhookHandlerConfig{
		secret:  secret,
		dedup:   dedup,
		trigger: func(_ []string, _ bool) { triggerCount++ },
	})

	makeReq := func() *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(body)))
		req.Header.Set("X-Hub-Signature-256", githubSign(secret, body))
		req.Header.Set("X-GitHub-Delivery", "delivery-1")
		return req
	}

	handler(httptest.NewRecorder(), makeReq())
	rec2 := httptest.NewRecorder()
	handler(rec2, makeReq())

	if triggerCount != 1 {
		t.Errorf("trigger called %d times across a replayed delivery, want 1", triggerCount)
	}
	// A replay is a no-op, not a rejection — provider shouldn't retry harder.
	if rec2.Code != http.StatusOK {
		t.Errorf("replayed delivery status = %d, want 200 (no-op, not a failure)", rec2.Code)
	}
}

func TestWebhookHandler_GitLabToken(t *testing.T) {
	secret := "s3cret"
	triggerCount := 0
	handler := newWebhookHandler(webhookHandlerConfig{
		secret:  secret,
		dedup:   autodeploy.NewDeliveryDedup(),
		trigger: func(_ []string, _ bool) { triggerCount++ },
	})

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"ref":"refs/heads/main"}`))
	req.Header.Set("X-Gitlab-Token", secret)
	rec := httptest.NewRecorder()

	handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if triggerCount != 1 {
		t.Errorf("trigger called %d times, want 1", triggerCount)
	}
}

func TestWebhookHandler_GitLabWrongToken(t *testing.T) {
	triggered := false
	handler := newWebhookHandler(webhookHandlerConfig{
		secret:  "s3cret",
		dedup:   autodeploy.NewDeliveryDedup(),
		trigger: func(_ []string, _ bool) { triggered = true },
	})

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`))
	req.Header.Set("X-Gitlab-Token", "wrong")
	rec := httptest.NewRecorder()

	handler(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if triggered {
		t.Error("wrong GitLab token must never trigger a deploy")
	}
}

func TestWebhookHandler_RejectsNonPOST(t *testing.T) {
	triggered := false
	handler := newWebhookHandler(webhookHandlerConfig{
		secret:  "s3cret",
		dedup:   autodeploy.NewDeliveryDedup(),
		trigger: func(_ []string, _ bool) { triggered = true },
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()

	handler(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
	if triggered {
		t.Error("a GET request must never trigger a deploy")
	}
}

func TestWebhookHandler_OnDedupChangedCalledOnNewDelivery(t *testing.T) {
	secret := "s3cret"
	body := []byte(`{}`)
	changedCount := 0

	handler := newWebhookHandler(webhookHandlerConfig{
		secret:         secret,
		dedup:          autodeploy.NewDeliveryDedup(),
		trigger:        func(_ []string, _ bool) {},
		onDedupChanged: func() { changedCount++ },
	})

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(body)))
	req.Header.Set("X-Hub-Signature-256", githubSign(secret, body))
	req.Header.Set("X-GitHub-Delivery", "delivery-1")

	handler(httptest.NewRecorder(), req)
	if changedCount != 1 {
		t.Errorf("onDedupChanged called %d times, want 1", changedCount)
	}
}
