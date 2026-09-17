package cli

import (
	"strings"
	"testing"
)

// TestParseEnvSetArgs_Argv pins the legacy argv form: KEY=value pairs,
// several at once, and the explicit error for a pair without "=".
func TestParseEnvSetArgs_Argv(t *testing.T) {
	pairs, err := parseEnvSetArgs(strings.NewReader(""), []string{"A=1", "B=two"}, false)
	if err != nil {
		t.Fatalf("argv form: %v", err)
	}
	if pairs["A"] != "1" || pairs["B"] != "two" || len(pairs) != 2 {
		t.Fatalf("pairs = %v", pairs)
	}

	if _, err := parseEnvSetArgs(strings.NewReader(""), []string{"NOEQUALS"}, false); err == nil {
		t.Fatal("argument without '=' should be rejected")
	}
}

// TestParseEnvSetArgs_Stdin is the UPSTREAM-1 contract: with --stdin the
// single argument is the bare KEY and the value comes from stdin verbatim,
// so the secret never appears in the process's argv.
func TestParseEnvSetArgs_Stdin(t *testing.T) {
	pairs, err := parseEnvSetArgs(strings.NewReader("s3cr3t-value"), []string{"API_KEY"}, true)
	if err != nil {
		t.Fatalf("stdin form: %v", err)
	}
	if len(pairs) != 1 || pairs["API_KEY"] != "s3cr3t-value" {
		t.Fatalf("pairs = %v, want {API_KEY: s3cr3t-value}", pairs)
	}

	// Multiline secrets survive verbatim.
	pairs, err = parseEnvSetArgs(strings.NewReader("line1\nline2"), []string{"CERT"}, true)
	if err != nil {
		t.Fatalf("multiline stdin: %v", err)
	}
	if pairs["CERT"] != "line1\nline2" {
		t.Fatalf("multiline value mangled: %q", pairs["CERT"])
	}

	// A KEY=value argument alongside --stdin is a caller bug, not an
	// implicit argv fallback — refuse it.
	if _, err := parseEnvSetArgs(strings.NewReader("v"), []string{"K=v"}, true); err == nil {
		t.Fatal("KEY=value with --stdin should be rejected")
	}

	// --stdin sets exactly one variable per invocation.
	if _, err := parseEnvSetArgs(strings.NewReader("v"), []string{"A", "B"}, true); err == nil {
		t.Fatal("two keys with --stdin should be rejected")
	}

	// The stdin reader enforces the value rules (NUL, size bound).
	if _, err := parseEnvSetArgs(strings.NewReader("a\x00b"), []string{"K"}, true); err == nil {
		t.Fatal("NUL value should be rejected")
	}
}
