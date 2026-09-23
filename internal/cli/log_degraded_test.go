package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy/internal/state"
)

// TestWriteLogEntries_DegradedIsDistinctFromCleanAndFailed is the C01-5
// consumer regression: every consumer that filters LogEntry on Success
// must be able to tell a DEGRADED success (traffic switched, predecessor
// retirement incomplete) from a clean one. `teploy log` rendering is the
// in-repo consumer; the modeled rollback-selection filter demonstrates
// the fleet decision the finding describes — a rollback keyed on
// Success alone targets the degraded host as "cleanly on the new
// generation", while Success && !Degraded excludes it for attention.
func TestWriteLogEntries_DegradedIsDistinctFromCleanAndFailed(t *testing.T) {
	entries := []state.LogEntry{
		{Timestamp: time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC), App: "app", Type: "deploy", Hash: "aaa111", Success: true, DurationMs: 1000},
		{Timestamp: time.Date(2026, 9, 22, 12, 1, 0, 0, time.UTC), App: "app", Type: "deploy", Hash: "bbb222", Success: true, Degraded: true, DegradedReason: "stop app-web-aaa111: connection refused", DurationMs: 1200},
		{Timestamp: time.Date(2026, 9, 22, 12, 2, 0, 0, time.UTC), App: "app", Type: "deploy", Hash: "ccc333", Success: false, DurationMs: 300},
	}

	var out bytes.Buffer
	if err := writeLogEntries(&out, entries, false, ""); err != nil {
		t.Fatalf("writeLogEntries: %v", err)
	}
	text := out.String()

	for _, line := range strings.Split(text, "\n") {
		switch {
		case strings.Contains(line, "bbb222"):
			if !strings.Contains(line, "DEGRADED") {
				t.Errorf("degraded entry must render status DEGRADED, got line: %s", line)
			}
			if strings.Contains(line, " FAILED") {
				t.Errorf("degraded is not a failure (traffic switched), got line: %s", line)
			}
			if !strings.Contains(line, "app-web-aaa111") {
				t.Errorf("degraded entry must surface the reason, got line: %s", line)
			}
		case strings.Contains(line, "aaa111"):
			if !strings.Contains(line, " ok ") {
				t.Errorf("clean entry must render status ok, got line: %s", line)
			}
		case strings.Contains(line, "ccc333"):
			if !strings.Contains(line, "FAILED") {
				t.Errorf("failed entry must render status FAILED, got line: %s", line)
			}
		}
	}

	// The distinction a log-keyed consumer needs: the degraded host is a
	// success (it serves the new generation and is compensatable like one)
	// but NOT a clean success (part of the superseded generation remains).
	rolledBackTargets := 0
	cleanHosts := 0
	for _, e := range entries {
		if e.Success {
			rolledBackTargets++
		}
		if e.Success && !e.Degraded {
			cleanHosts++
		}
	}
	if rolledBackTargets != 2 || cleanHosts != 1 {
		t.Errorf("success-filtering consumers must see the degraded host: %d successes (want 2), %d clean (want 1)", rolledBackTargets, cleanHosts)
	}

	// JSON (the dash/machine surface) must carry the field.
	var jsonOut bytes.Buffer
	if err := writeLogEntries(&jsonOut, entries, true, ""); err != nil {
		t.Fatalf("writeLogEntries json: %v", err)
	}
	var decoded []map[string]any
	if err := json.Unmarshal(jsonOut.Bytes(), &decoded); err != nil {
		t.Fatalf("invalid log JSON: %v", err)
	}
	if len(decoded) != 3 {
		t.Fatalf("want 3 decoded entries, got %d", len(decoded))
	}
	if decoded[1]["degraded"] != true {
		t.Errorf("degraded entry must carry degraded=true in JSON, got %v", decoded[1]["degraded"])
	}
	if decoded[0]["degraded"] != nil {
		t.Errorf("clean entry must not carry a degraded key (omitempty), got %v", decoded[0]["degraded"])
	}
}
