package cli

import (
	"strings"
	"testing"
)

func TestReadSecretValue(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"verbatim value", "s3cr3t", "s3cr3t", false},
		// The contract is machine-to-machine: no trailing-newline trim. A
		// human piping `echo secret` gets "secret\n" — that is documented;
		// dash sends exact bytes.
		{"trailing newline kept", "secret\n", "secret\n", false},
		{"multiline value", "line1\nline2\n", "line1\nline2\n", false},
		{"empty value", "", "", false},
		{"leading and interior spaces", "  padded  ", "  padded  ", false},
		{"NUL rejected", "bad\x00value", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := readSecretValue(strings.NewReader(tc.in))
			if tc.wantErr != (err != nil) {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if !tc.wantErr && got != tc.want {
				t.Errorf("value = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestReadSecretValue_SizeBound(t *testing.T) {
	// Exactly at the bound passes.
	ok := strings.Repeat("a", maxSecretStdinBytes)
	if got, err := readSecretValue(strings.NewReader(ok)); err != nil || got != ok {
		t.Fatalf("value at the bound: err=%v len=%d", err, len(got))
	}
	// One byte over is refused rather than silently read.
	over := strings.Repeat("a", maxSecretStdinBytes+1)
	if _, err := readSecretValue(strings.NewReader(over)); err == nil {
		t.Fatal("value over the bound should be rejected")
	}
}

func TestReadVarMapStdin(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    map[string]string
		wantErr bool
	}{
		{"object", `{"API_KEY":"abc","EMPTY":""}`, map[string]string{"API_KEY": "abc", "EMPTY": ""}, false},
		{"empty object", `{}`, map[string]string{}, false},
		{"empty input", ``, nil, true},
		{"not an object", `["API_KEY"]`, nil, true},
		{"non-string value", `{"N":5}`, nil, true},
		{"malformed json", `{"K":`, nil, true},
		{"NUL in value", "{\"K\":\"a\x00b\"}", nil, true},
		{"NUL in key", "{\"K\x002\":\"v\"}", nil, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := readVarMapStdin(strings.NewReader(tc.in))
			if tc.wantErr != (err != nil) {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("key %q: got %q, want %q", k, got[k], v)
				}
			}
		})
	}
}

func TestReadVarMapStdin_SizeBound(t *testing.T) {
	// A payload one byte over the bound must be rejected, not parsed.
	big := `{"K":"` + strings.Repeat("a", maxSecretStdinBytes) + `"}`
	if _, err := readVarMapStdin(strings.NewReader(big)); err == nil {
		t.Fatal("payload over the bound should be rejected")
	}
}
