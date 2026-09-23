package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/ssh"
)

// TestStatus_ShowsOutstandingRepairDebt is the C01-6 operator-surface
// regression: `teploy status` reports the app's outstanding record repair
// debt (release, attempt count, remediation), in text and JSON.
func TestStatus_ShowsOutstandingRepairDebt(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "docker ps", Output: ""},
	)
	debt := map[string]any{
		"schema_version":  1,
		"app":             "myapp",
		"release":         "new456",
		"attempt":         "new456.0123456789abcdef",
		"reason":          "uploading temporary file: boom",
		"attempts":        2,
		"first_failed_at": time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC).Format(time.RFC3339Nano),
		"last_failed_at":  time.Date(2026, 9, 22, 11, 0, 0, 0, time.UTC).Format(time.RFC3339Nano),
	}
	debtJSON, _ := json.Marshal(debt)
	mock.Files["/deployments/myapp/repair-debt.json"] = debtJSON

	var out bytes.Buffer
	if err := writeStatus(context.Background(), &Flags{}, &config.AppConfig{App: "myapp"}, mock, &out); err != nil {
		t.Fatalf("writeStatus: %v", err)
	}
	text := out.String()
	for _, want := range []string{"Repair debt", "new456", "2 attempt"} {
		if !strings.Contains(text, want) {
			t.Errorf("status must report the debt (missing %q), got:\n%s", want, text)
		}
	}

	// JSON surface (dash/machine readers) carries the structured marker.
	var jsonOut bytes.Buffer
	if err := writeStatus(context.Background(), &Flags{JSON: true}, &config.AppConfig{App: "myapp"}, mock, &jsonOut); err != nil {
		t.Fatalf("writeStatus json: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(jsonOut.Bytes(), &decoded); err != nil {
		t.Fatalf("invalid status JSON: %v", err)
	}
	debtField, ok := decoded["repair_debt"].(map[string]any)
	if !ok {
		t.Fatalf("status JSON must carry repair_debt, got %v", decoded["repair_debt"])
	}
	if debtField["release"] != "new456" {
		t.Errorf("repair_debt.release = %v, want new456", debtField["release"])
	}
}

// TestStatus_NoDebtMarkerNoNoise pins the quiet side: an app with no repair
// debt gets no debt output (text or JSON).
func TestStatus_NoDebtMarkerNoNoise(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "docker ps", Output: ""},
	)
	var out bytes.Buffer
	if err := writeStatus(context.Background(), &Flags{}, &config.AppConfig{App: "myapp"}, mock, &out); err != nil {
		t.Fatalf("writeStatus: %v", err)
	}
	if strings.Contains(strings.ToLower(out.String()), "repair") {
		t.Errorf("no debt means no repair output noise, got:\n%s", out.String())
	}

	var jsonOut bytes.Buffer
	if err := writeStatus(context.Background(), &Flags{JSON: true}, &config.AppConfig{App: "myapp"}, mock, &jsonOut); err != nil {
		t.Fatalf("writeStatus json: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(jsonOut.Bytes(), &decoded); err != nil {
		t.Fatalf("invalid status JSON: %v", err)
	}
	if decoded["repair_debt"] != nil {
		t.Errorf("repair_debt must be null without a marker, got %v", decoded["repair_debt"])
	}
}
