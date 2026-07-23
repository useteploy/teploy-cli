package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/docker"
	teployenv "github.com/useteploy/teploy/internal/env"
	"github.com/useteploy/teploy/internal/state"
)

func TestMachineJSONListOutputs(t *testing.T) {
	t.Run("env", func(t *testing.T) {
		var out bytes.Buffer
		if err := writeEnvList(&out, nil, false, true); err != nil {
			t.Fatal(err)
		}
		assertJSONEqual(t, out.Bytes(), []any{})

		out.Reset()
		entries := []teployenv.Entry{{Key: "DATABASE_URL", Value: "secret"}}
		if err := writeEnvList(&out, entries, false, true); err != nil {
			t.Fatal(err)
		}
		assertJSONEqual(t, out.Bytes(), []any{
			map[string]any{"key": "DATABASE_URL", "value": "***"},
		})
	})

	t.Run("accessory", func(t *testing.T) {
		var out bytes.Buffer
		if err := writeAccessoryList(&out, nil, true); err != nil {
			t.Fatal(err)
		}
		assertJSONEqual(t, out.Bytes(), []any{})

		out.Reset()
		containers := []docker.Container{{
			ID: "abc", Name: "app-db", Image: "postgres:16", State: "running", Status: "Up",
		}}
		if err := writeAccessoryList(&out, containers, true); err != nil {
			t.Fatal(err)
		}
		assertJSONEqual(t, out.Bytes(), []any{map[string]any{
			"id": "abc", "name": "app-db", "image": "postgres:16", "state": "running",
			"status": "Up", "created_at": "",
		}})
	})

	t.Run("log", func(t *testing.T) {
		var out bytes.Buffer
		if err := writeLogEntries(&out, nil, true, "ignored"); err != nil {
			t.Fatal(err)
		}
		assertJSONEqual(t, out.Bytes(), []any{})

		out.Reset()
		entries := []state.LogEntry{{
			Timestamp: time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC),
			App:       "app",
			Type:      "deploy",
			Success:   true,
		}}
		if err := writeLogEntries(&out, entries, true, ""); err != nil {
			t.Fatal(err)
		}
		var decoded []map[string]any
		if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
			t.Fatalf("invalid log JSON: %v", err)
		}
		if len(decoded) != 1 || decoded[0]["app"] != "app" || decoded[0]["type"] != "deploy" {
			t.Fatalf("unexpected log JSON: %s", out.String())
		}
		if _, exists := decoded[0]["App"]; exists {
			t.Fatalf("log JSON contains unstable uppercase key: %s", out.String())
		}
	})

	t.Run("server", func(t *testing.T) {
		var out bytes.Buffer
		if err := writeServerList(&out, nil, true); err != nil {
			t.Fatal(err)
		}
		assertJSONEqual(t, out.Bytes(), map[string]any{})

		out.Reset()
		servers := map[string]config.Server{
			"prod": {Host: "192.0.2.10", User: "deploy", Role: "app"},
		}
		if err := writeServerList(&out, servers, true); err != nil {
			t.Fatal(err)
		}
		assertJSONEqual(t, out.Bytes(), map[string]any{
			"prod": map[string]any{"host": "192.0.2.10", "user": "deploy", "role": "app"},
		})
	})

	t.Run("stats", func(t *testing.T) {
		var out bytes.Buffer
		if err := writeContainerStats(&out, nil); err != nil {
			t.Fatal(err)
		}
		assertJSONEqual(t, out.Bytes(), []any{})

		raw := `{"Name":"app-web-v1","CPUPerc":"0.25%","MemUsage":"10MiB / 1GiB","MemPerc":"1.00%","NetIO":"1kB / 2kB","BlockIO":"0B / 0B"}`
		stats, err := parseContainerStats(raw)
		if err != nil {
			t.Fatal(err)
		}
		out.Reset()
		if err := writeContainerStats(&out, stats); err != nil {
			t.Fatal(err)
		}
		assertJSONEqual(t, out.Bytes(), []any{map[string]any{
			"name": "app-web-v1", "cpu_percent": "0.25%", "memory_usage": "10MiB / 1GiB",
			"memory_percent": "1.00%", "network_io": "1kB / 2kB", "block_io": "0B / 0B",
		}})
	})
}

func TestServerListJSONWithoutConfigFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var out bytes.Buffer
	if err := runServerList(&Flags{JSON: true}, &out); err != nil {
		t.Fatalf("runServerList: %v", err)
	}
	assertJSONEqual(t, out.Bytes(), map[string]any{})
}

func TestLogsCommandFollowModes(t *testing.T) {
	if got := logsCommand("app-web-v1", 50, true); got != "docker logs -f --tail 50 app-web-v1" {
		t.Fatalf("default logs command = %q", got)
	}
	got := logsCommand("app-web-v1", 100, false)
	if got != "docker logs --tail 100 app-web-v1" {
		t.Fatalf("bounded logs command = %q", got)
	}
	if strings.Contains(got, " -f") {
		t.Fatalf("bounded logs command still follows: %q", got)
	}
}

func assertJSONEqual(t *testing.T, raw []byte, want any) {
	t.Helper()
	var got any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("invalid JSON %q: %v", raw, err)
	}
	wantRaw, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var normalizedWant any
	if err := json.Unmarshal(wantRaw, &normalizedWant); err != nil {
		t.Fatal(err)
	}
	if !jsonValuesEqual(got, normalizedWant) {
		t.Fatalf("JSON = %s, want %s", raw, wantRaw)
	}
}

func jsonValuesEqual(a, b any) bool {
	aRaw, _ := json.Marshal(a)
	bRaw, _ := json.Marshal(b)
	return bytes.Equal(aRaw, bRaw)
}
