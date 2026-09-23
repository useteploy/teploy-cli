package cli

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/ssh"
)

// TestParseDockerAuths_RealConfigStructure reproduces the shape of a real
// ~/.docker/config.json (confirmed live against an actual server) and
// verifies both registries and their usernames are extracted correctly —
// the previous implementation scanned lines for a bare ":" outside braces,
// which had no real notion of JSON structure and would have broken on
// entries with extra fields (email, identitytoken) or credHelpers blocks.
func TestParseDockerAuths_RealConfigStructure(t *testing.T) {
	configJSON := `{
		"auths": {
			"ghcr.io": {"auth": "bXl1c2VyOm15dG9rZW4xMjM="},
			"registry.example.com": {"auth": "YW5vdGhlcnVzZXI6c2VjcmV0", "email": "a@b.com"}
		},
		"credHelpers": {
			"gcr.io": "gcloud"
		}
	}`

	entries, err := parseDockerAuths(configJSON)
	if err != nil {
		t.Fatalf("parseDockerAuths: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d: %v", len(entries), entries)
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].Server < entries[j].Server })
	if entries[0].Server != "ghcr.io" || entries[0].Username != "myuser" {
		t.Errorf("entry 0 = %+v, want ghcr.io/myuser", entries[0])
	}
	if entries[1].Server != "registry.example.com" || entries[1].Username != "anotheruser" {
		t.Errorf("entry 1 = %+v, want registry.example.com/anotheruser", entries[1])
	}
}

func TestParseDockerAuths_EmptyConfig(t *testing.T) {
	entries, err := parseDockerAuths("{}")
	if err != nil {
		t.Fatalf("parseDockerAuths: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected 0 entries for empty config, got %d", len(entries))
	}
}

// TestParseDockerAuths_ProducesValidJSON reproduces a real bug found live:
// runRegistryList's --json output was human-readable text, not JSON, even
// though --json is a documented global flag every other list-style command
// honors — teploy-dash calls exactly `teploy registry list --json` and
// passes stdout through verbatim as an HTTP response body. This confirms
// the entries this function returns round-trip through the exact encoding
// used there (json.NewEncoder).
func TestParseDockerAuths_ProducesValidJSON(t *testing.T) {
	entries, err := parseDockerAuths(`{"auths":{"ghcr.io":{"auth":"dTpw"}}}`)
	if err != nil {
		t.Fatalf("parseDockerAuths: %v", err)
	}
	out, err := json.Marshal(entries)
	if err != nil {
		t.Fatalf("marshaling entries: %v", err)
	}
	var roundTrip []RegistryEntry
	if err := json.Unmarshal(out, &roundTrip); err != nil {
		t.Fatalf("entries didn't round-trip as valid JSON: %v (got: %s)", err, out)
	}
	if len(roundTrip) != 1 || roundTrip[0].Server != "ghcr.io" {
		t.Errorf("round-tripped entries = %+v", roundTrip)
	}
}

// TestRegistryListFromServer_ReadFailureIsNotEmptyState pins the C08
// acceptance for reads: a config.json that EXISTS but cannot be read is
// an error, never an empty list that reports "No registries
// configured"; confirmed absence of the file is the legitimate empty
// state.
func TestRegistryListFromServer_ReadFailureIsNotEmptyState(t *testing.T) {
	ctx := context.Background()

	t.Run("absent file is the empty state", func(t *testing.T) {
		mock := ssh.NewMockExecutor("h",
			ssh.MockCommand{Match: "if [ ! -f ~/.docker/config.json ]", Output: "absent"})
		entries, err := registryListFromServer(ctx, mock)
		if err != nil || len(entries) != 0 {
			t.Fatalf("absent config = %v, %v; want empty, nil", entries, err)
		}
	})

	t.Run("read failure errors", func(t *testing.T) {
		mock := ssh.NewMockExecutor("h",
			ssh.MockCommand{Match: "if [ ! -f ~/.docker/config.json ]", Err: errors.New("exit status 1: cat: /root/.docker/config.json: Permission denied")})
		if _, err := registryListFromServer(ctx, mock); err == nil {
			t.Fatal("an unreadable config.json must error, not read as no-registries")
		}
	})

	t.Run("present config parses", func(t *testing.T) {
		mock := ssh.NewMockExecutor("h",
			ssh.MockCommand{Match: "if [ ! -f ~/.docker/config.json ]", Output: `{"auths":{"ghcr.io":{"auth":"dTpw"}}}`})
		entries, err := registryListFromServer(ctx, mock)
		if err != nil || len(entries) != 1 || entries[0].Server != "ghcr.io" {
			t.Fatalf("present config = %v, %v", entries, err)
		}
	})

	t.Run("framed read touches the server once", func(t *testing.T) {
		mock := ssh.NewMockExecutor("h",
			ssh.MockCommand{Match: "if [ ! -f ~/.docker/config.json ]", Output: "absent"})
		if _, err := registryListFromServer(ctx, mock); err != nil {
			t.Fatal(err)
		}
		if len(mock.Calls) != 1 || !strings.HasPrefix(mock.Calls[0], "if [ ! -f ~/.docker/config.json ]") {
			t.Fatalf("unexpected command shape: %v", mock.Calls)
		}
	})
}
