package config

import (
	"testing"
	"strings"
)

// TCL-35: accessory volume keys become path segments under the accessory
// data directory and reach mkdir/ownership sites — they follow the same
// identifier grammar as top-level volumes, and container destinations must
// be absolute.
func TestValidate_RejectsUnsafeAccessoryVolumes(t *testing.T) {
	base := func() *AppConfig {
		return &AppConfig{App: "myapp", Domain: "myapp.com",
			Accessories: map[string]AccessoryConfig{"db": {Image: "postgres:16"}}}
	}
	for key, dest := range map[string]string{
		"../escape": "/data",
		"has space": "/data",
		"ok":        "relative/path",
		"ok2":       "/data\ninject",
	} {
		cfg := base()
		cfg.Accessories["db"] = AccessoryConfig{Image: "postgres:16", Volumes: map[string]string{key: dest}}
		if err := cfg.validate(); err == nil {
			t.Errorf("accepted accessory volume %q -> %q", key, dest)
		}
	}
	cfg := base()
	cfg.Accessories["db"] = AccessoryConfig{Image: "postgres:16", Volumes: map[string]string{"pgdata": "/var/lib/postgresql/data"}}
	if err := cfg.validate(); err != nil {
		t.Errorf("valid accessory volume rejected: %v", err)
	}
}

// TestParsePublishSpec_grammar pins the supported publish grammar (T23):
// valid single-port forms parse; ranges, out-of-range ports, malformed
// IPv6, and unknown protocols fail.
func TestParsePublishSpec_grammar(t *testing.T) {
	valid := map[string]PublishSpec{
		"3000":                        {ContainerPort: 3000},
		"3000/udp":                    {ContainerPort: 3000, Proto: "udp"},
		"9100:9000":                   {HostPort: 9100, ContainerPort: 9000},
		"127.0.0.1:9100:9000":         {Bind: "127.0.0.1", HostPort: 9100, ContainerPort: 9000},
		"[::1]:9100:9000":             {Bind: "::1", HostPort: 9100, ContainerPort: 9000},
		"[::1]::9000":                 {Bind: "::1", ContainerPort: 9000},
		"0.0.0.0:51820:51820/udp":     {Bind: "0.0.0.0", HostPort: 51820, ContainerPort: 51820, Proto: "udp"},
	}
	for in, want := range valid {
		got, err := ParsePublishSpec(in)
		if err != nil {
			t.Errorf("ParsePublishSpec(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParsePublishSpec(%q) = %+v, want %+v", in, got, want)
		}
	}
	for _, bad := range []string{
		"",
		"8000-8010:8000",
		"70000:80",
		"0:80",
		"[::1:80",
		"not-an-ip:80:80",
		"80/http",
		":::80",
		"a:80",
	} {
		if _, err := ParsePublishSpec(bad); err == nil {
			t.Errorf("ParsePublishSpec(%q) accepted an unsupported spec", bad)
		}
	}
}

// TestValidatePublishEntries_rejectsDuplicateBindings: the same host port
// bound again — explicitly or via a wildcard that covers it — is a
// guaranteed docker start failure mid-deploy (T23).
func TestValidatePublishEntries_rejectsDuplicateBindings(t *testing.T) {
	if err := ValidatePublishEntries([]string{"0.0.0.0:9100:9000", "9100:9001"}); err == nil {
		t.Error("duplicate host binding accepted")
	}
	if err := ValidatePublishEntries([]string{"127.0.0.1:9100:9000", "0.0.0.0:9100:9001"}); err == nil {
		t.Error("wildcard + specific binding of the same host port accepted (the wildcard covers the specific address)")
	}
	if err := ValidatePublishEntries([]string{"127.0.0.1:9100:9000", "[::1]:9100:9001"}); err != nil {
		t.Errorf("same port on distinct specific binds is fine: %v", err)
	}
	if err := ValidatePublishEntries([]string{"3000", "3000/udp", "[::1]::3000"}); err != nil {
		t.Errorf("ephemeral and distinct-proto bindings are fine: %v", err)
	}
}

// TestAccessValidate_StructuralBcrypt is the T53 regression: a bcrypt
// PREFIX with a truncated salt/digest used to pass validation and break the
// Caddy reload at deploy time.
func TestAccessValidate_StructuralBcrypt(t *testing.T) {
	complete := "$2a$10$" + strings.Repeat("a", 53)
	if err := (AccessConfig{BasicAuth: map[string]string{"alice": complete}}).validate(); err != nil {
		t.Fatalf("complete bcrypt hash rejected: %v", err)
	}
	for _, bad := range []string{"$2a$10$short", "password", "$2a$10$" + strings.Repeat("a", 52)} {
		if err := (AccessConfig{BasicAuth: map[string]string{"alice": bad}}).validate(); err == nil {
			t.Errorf("malformed bcrypt value %q accepted", bad)
		}
	}
	if err := (AccessConfig{BasicAuth: map[string]string{"bad user": complete}}).validate(); err == nil {
		t.Error("username with a space accepted")
	}
}

// TestAccessValidate_ForwardAuthFields: the verify URI must be
// request-path-shaped and copied headers must be HTTP tokens (T53).
func TestAccessValidate_ForwardAuthFields(t *testing.T) {
	good := &ForwardAuthConfig{URL: "authelia:9091", URI: "/api/verify", CopyHeaders: []string{"Remote-User", "Remote-Groups"}}
	if err := (AccessConfig{ForwardAuth: good}).validate(); err != nil {
		t.Fatalf("valid forward_auth rejected: %v", err)
	}
	badURI := &ForwardAuthConfig{URL: "authelia:9091", URI: "https://evil.example/verify"}
	if err := (AccessConfig{ForwardAuth: badURI}).validate(); err == nil {
		t.Error("absolute verify URI accepted")
	}
	badHeader := &ForwardAuthConfig{URL: "authelia:9091", CopyHeaders: []string{"Remote User"}}
	if err := (AccessConfig{ForwardAuth: badHeader}).validate(); err == nil {
		t.Error("header name with a space accepted")
	}
}
