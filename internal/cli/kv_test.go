package cli

import (
	"strings"
	"testing"
)

func TestKvQuote(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"flags/beta", "'flags/beta'"},
		{"it's", "'it''s'"},
		{"", "''"},
		{"a'b'c", "'a''b''c'"},
	}
	for _, tc := range tests {
		if got := kvQuote(tc.in); got != tc.want {
			t.Errorf("kvQuote(%q) = %s, want %s", tc.in, got, tc.want)
		}
	}
}

func TestShellSingleQuote(t *testing.T) {
	// The SQL payload must survive the container's `sh -c` layer intact,
	// including embedded single quotes from kvQuote.
	in := "SELECT KV_SET('it''s', 'v')"
	want := `'SELECT KV_SET('"'"'it'"'"''"'"'s'"'"', '"'"'v'"'"')'`
	if got := shellSingleQuote(in); got != want {
		t.Errorf("shellSingleQuote = %s, want %s", got, want)
	}
}

func TestKvFirstValue(t *testing.T) {
	tests := []struct {
		name, in, want string
		wantErr        bool
	}{
		// Column name varies between friendly aliases and raw expressions —
		// the extractor must not depend on it.
		{"friendly alias", `[{"kv_get":"on"}]`, "on", false},
		{"raw expression", `[{"KV_KEYS('flags/*')":"[\"a\",\"b\"]"}]`, `["a","b"]`, false},
		{"empty result", `[]`, "", false},
		{"warning line before JSON", "some warning\n[{\"kv_incr\":\"7\"}]", "7", false},
		{"garbage", "not json at all", "", true},
	}
	for _, tc := range tests {
		got, err := kvFirstValue(tc.in)
		if tc.wantErr != (err != nil) {
			t.Errorf("%s: err = %v, wantErr = %v", tc.name, err, tc.wantErr)
			continue
		}
		if !tc.wantErr && got != tc.want {
			t.Errorf("%s: value = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestKvTruthy(t *testing.T) {
	for _, v := range []string{"true", "t", "1", "on", "yes", " TRUE "} {
		if !kvTruthy(v) {
			t.Errorf("kvTruthy(%q) = false, want true", v)
		}
	}
	for _, v := range []string{"false", "f", "0", "", "off", "no", "NULL"} {
		if kvTruthy(v) {
			t.Errorf("kvTruthy(%q) = true, want false", v)
		}
	}
}

// TestParseKvSetArgs_Argv pins the legacy two-positional form.
func TestParseKvSetArgs_Argv(t *testing.T) {
	key, value, err := parseKvSetArgs(strings.NewReader(""), []string{"flags/beta", "on"}, false)
	if err != nil {
		t.Fatalf("argv form: %v", err)
	}
	if key != "flags/beta" || value != "on" {
		t.Fatalf("got %q/%q", key, value)
	}

	if _, _, err := parseKvSetArgs(strings.NewReader(""), []string{"flags/beta"}, false); err == nil {
		t.Fatal("key without value and without --stdin should be rejected")
	}
}

// TestParseKvSetArgs_Stdin is the UPSTREAM-1 contract: with --stdin the key
// is the only argument and the value comes from stdin verbatim, so the
// secret never appears in the process's argv.
func TestParseKvSetArgs_Stdin(t *testing.T) {
	key, value, err := parseKvSetArgs(strings.NewReader("t0ps3cr3t"), []string{"smtp/password"}, true)
	if err != nil {
		t.Fatalf("stdin form: %v", err)
	}
	if key != "smtp/password" || value != "t0ps3cr3t" {
		t.Fatalf("got %q/%q", key, value)
	}

	// A value positional alongside --stdin is a caller bug — refuse it
	// rather than racing an argv value against the pipe.
	if _, _, err := parseKvSetArgs(strings.NewReader("v"), []string{"k", "v"}, true); err == nil {
		t.Fatal("key + value with --stdin should be rejected")
	}

	if _, _, err := parseKvSetArgs(strings.NewReader("a\x00b"), []string{"k"}, true); err == nil {
		t.Fatal("NUL value should be rejected")
	}
}

func TestBuildKvSetSQL(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value string
		ttl   int64
		want  string
	}{
		{"plain", "flags/beta", "on", 0, "SELECT KV_SET('flags/beta', 'on')"},
		{"ttl", "flags/beta", "on", 300, "SELECT KV_SET('flags/beta', 'on', 300)"},
		{"negative ttl ignored", "flags/beta", "on", -1, "SELECT KV_SET('flags/beta', 'on')"},
		{"quote escaping", "it's", "va'lue\nmultiline", 0, "SELECT KV_SET('it''s', 'va''lue\nmultiline')"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := buildKvSetSQL(tc.key, tc.value, tc.ttl); got != tc.want {
				t.Errorf("buildKvSetSQL = %s, want %s", got, tc.want)
			}
		})
	}
}
