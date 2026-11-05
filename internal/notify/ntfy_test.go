package notify

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSendNtfy(t *testing.T) {
	var gotBody, gotTitle, gotPriority, gotTags string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		gotTitle = r.Header.Get("Title")
		gotPriority = r.Header.Get("Priority")
		gotTags = r.Header.Get("Tags")
	}))
	defer srv.Close()

	n := NewMultiNotifier([]Channel{{Type: "ntfy", URL: srv.URL + "/deploys"}})
	errs := n.Send(context.Background(), Payload{
		App: "myapp", Server: "box1", Type: "deploy", Success: false, Message: "health check failed",
	})
	if len(errs) != 0 {
		t.Fatalf("send errors: %v", errs)
	}
	if gotTitle != "teploy: myapp deploy" {
		t.Errorf("Title = %q", gotTitle)
	}
	if gotPriority != "high" {
		t.Errorf("failed deploys must be high priority, got %q", gotPriority)
	}
	// Plain word tags: ntfy turns emoji shortcodes (rotating_light,
	// white_check_mark) into emoji, and product output carries none.
	if gotTags != "teploy,deploy,failed" {
		t.Errorf("Tags = %q, want plain words teploy,deploy,failed", gotTags)
	}
	if gotBody == "" || gotBody[0] == '{' {
		t.Errorf("ntfy body must be plain text, not JSON: %q", gotBody)
	}
}

func TestNtfyTagsArePlainWords(t *testing.T) {
	for typ, want := range map[string]string{
		"deploy":     "teploy,deploy,succeeded",
		"Rollback":   "teploy,rollback,succeeded",
		"backup,x y": "teploy,backupxy,succeeded",
		"":           "teploy,succeeded",
	} {
		if got := ntfyTags(typ, "succeeded"); got != want {
			t.Errorf("ntfyTags(%q) = %q, want %q", typ, got, want)
		}
	}
}
