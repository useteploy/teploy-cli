package openbao

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/useteploy/teploy/internal/ssh"
	"net/http"
	"net/http/httptest"
	"testing"
)

func auditCursorMock(marker, log string) *ssh.MockExecutor {
	return ssh.NewMockExecutor("test", ssh.MockCommand{Match: "mkdir /deployments/demo/.lock", Output: ""}, ssh.MockCommand{Match: "rm -rf", Output: ""}, ssh.MockCommand{Match: "if test -f /deployments/demo/accessories/openbao/.audit-shipped", Output: marker}, ssh.MockCommand{Match: "docker exec", Output: log})
}
func auditResponse(path string) string {
	return fmt.Sprintf(`{"type":"response","auth":{"display_name":"demo","policy_results":{"allowed":true}},"request":{"operation":"read","mount_type":"kv","path":%q}}`, path)
}

func TestAuditCursorCountsConsumedLinesOnEmissionFailure(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 2 {
			w.WriteHeader(500)
		} else {
			w.WriteHeader(201)
		}
	}))
	defer server.Close()
	log := `{"type":"request"}` + "\n" + auditResponse("A") + "\n" + `{"type":"request"}` + "\n" + auditResponse("B") + "\n"
	mock := auditCursorMock("{}", log)
	n, err := NewClient(mock, &bytes.Buffer{}).ShipAudit(context.Background(), "demo", "openbao", server.URL, "", "default")
	if err == nil || n != 1 {
		t.Fatalf("shipment failure: %d %v", n, err)
	}
	var cursor struct {
		Lines  int    `json:"lines"`
		Prefix string `json:"prefix"`
	}
	if err := json.Unmarshal(mock.Files["/deployments/demo/accessories/openbao/.audit-shipped"], &cursor); err != nil {
		t.Fatal(err)
	}
	if cursor.Lines != 3 || cursor.Prefix == "" {
		t.Fatalf("event count confused with consumed lines: %+v", cursor)
	}
}
func TestAuditCursorDetectsSameLengthRotation(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(201) }))
	defer server.Close()
	first := auditCursorMock("{}", auditResponse("A")+"\n")
	if n, err := NewClient(first, &bytes.Buffer{}).ShipAudit(context.Background(), "demo", "openbao", server.URL, "", "default"); err != nil || n != 1 {
		t.Fatalf("first: %d %v", n, err)
	}
	next := auditCursorMock(string(first.Files["/deployments/demo/accessories/openbao/.audit-shipped"]), auditResponse("B")+"\n")
	if n, err := NewClient(next, &bytes.Buffer{}).ShipAudit(context.Background(), "demo", "openbao", server.URL, "", "default"); err != nil || n != 1 {
		t.Fatalf("rotation stalled: %d %v", n, err)
	}
	if calls != 2 {
		t.Fatalf("rotation delivered %d", calls)
	}
}
