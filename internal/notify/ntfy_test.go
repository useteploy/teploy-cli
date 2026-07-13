package notify

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSendNtfy(t *testing.T) {
	var gotBody, gotTitle, gotPriority string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		gotTitle = r.Header.Get("Title")
		gotPriority = r.Header.Get("Priority")
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
	if gotBody == "" || gotBody[0] == '{' {
		t.Errorf("ntfy body must be plain text, not JSON: %q", gotBody)
	}
}
