package openbao

import "testing"

func TestAppPath(t *testing.T) {
	if got := appPath("myapp", "db"); got != "secret/myapp/db" {
		t.Errorf("appPath = %q", got)
	}
	if got := appPath("myapp", ""); got != "secret/myapp" {
		t.Errorf("appPath empty name = %q", got)
	}
}

func TestShellSingleQuote(t *testing.T) {
	cases := map[string]string{
		"plain":       "'plain'",
		"a b":         "'a b'",
		"it's":        `'it'\''s'`,
		"$(rm -rf /)": `'$(rm -rf /)'`, // command substitution neutralized inside single quotes
	}
	for in, want := range cases {
		if got := shellSingleQuote(in); got != want {
			t.Errorf("shellSingleQuote(%q) = %q, want %q", in, got, want)
		}
	}
}
