package cli

import "testing"

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
