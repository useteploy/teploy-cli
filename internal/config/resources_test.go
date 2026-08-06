package config

import (
	"strings"
	"testing"
)

// Docker rejects a malformed limit when the container starts — which on a
// deploy is after the old container is already gone. Catching it at parse time
// turns a typo into a config error instead of an outage.
func TestValidateResources_RejectsMalformedValues(t *testing.T) {
	bad := []struct{ memory, cpu string }{
		{memory: "8gb"},  // docker uses g, not gb
		{memory: "8 g"},  // no embedded space
		{memory: "lots"}, //
		{memory: "-1g"},  // negative
		{memory: "8g; rm -rf /"},
		{cpu: "two"},
		{cpu: "2 cores"},
		{cpu: "-1"},
	}
	for _, c := range bad {
		if err := validateResources("", c.memory, c.cpu); err == nil {
			t.Errorf("validateResources(memory=%q, cpu=%q) = nil, want error", c.memory, c.cpu)
		}
	}
}

func TestValidateResources_AcceptsDockerSyntax(t *testing.T) {
	good := []struct{ memory, cpu string }{
		{}, // both unset: unlimited, the default
		{memory: "512m"},
		{memory: "8g"},
		{memory: "1.5g"},
		{memory: "536870912"}, // plain byte count
		{memory: "2G"},        // case-insensitive
		{cpu: "0.5"},
		{cpu: "2"},
		{memory: "8g", cpu: "2"},
	}
	for _, c := range good {
		if err := validateResources("", c.memory, c.cpu); err != nil {
			t.Errorf("validateResources(memory=%q, cpu=%q) = %v, want nil", c.memory, c.cpu, err)
		}
	}
}

// The error has to name which accessory is wrong; "'memory' is invalid" is not
// actionable in a config with several of them.
func TestValidate_NamesTheOffendingAccessory(t *testing.T) {
	cfg := &AppConfig{
		App:    "myapp",
		Domain: "example.com",
		Accessories: map[string]AccessoryConfig{
			"redis":   {Image: "redis:7"},
			"nucleus": {Image: "nucleus:latest", Memory: "8gb"},
		},
	}
	err := cfg.validate()
	if err == nil {
		t.Fatal("expected an error for the malformed accessory memory value")
	}
	if got := err.Error(); !strings.Contains(got, "nucleus") || !strings.Contains(got, "8gb") {
		t.Errorf("error should name the accessory and the bad value, got: %s", got)
	}
}

func TestMergeAccessory_OverlayCanRetuneLimitsPerDestination(t *testing.T) {
	// The same accessory wants a different cap on a 4 GB test box than on a
	// 32 GB production host, so an overlay must be able to override it.
	base := AccessoryConfig{Image: "nucleus:v0.1.5", Memory: "8g", CPU: "4"}
	out := mergeAccessory(base, AccessoryConfig{Memory: "1g"})
	if out.Memory != "1g" {
		t.Errorf("overlay memory should win: got %q", out.Memory)
	}
	if out.CPU != "4" {
		t.Errorf("unset overlay cpu must not clear the base value: got %q", out.CPU)
	}
	if out.Image != "nucleus:v0.1.5" {
		t.Errorf("unrelated fields must survive the merge: got %q", out.Image)
	}
}
