package cli

import (
	"reflect"
	"testing"

	"github.com/useteploy/teploy/internal/config"
)

func TestParseTagFilters(t *testing.T) {
	m, err := parseTagFilters([]string{"region=eu", "tier=web"})
	if err != nil {
		t.Fatalf("parseTagFilters: %v", err)
	}
	if m["region"] != "eu" || m["tier"] != "web" {
		t.Fatalf("unexpected map: %v", m)
	}
	if _, err := parseTagFilters([]string{"noequals"}); err == nil {
		t.Fatal("expected error for invalid tag")
	}
	if m, _ := parseTagFilters(nil); m != nil {
		t.Fatal("nil input should give nil map (no filter)")
	}
}

func TestSelectServersByRoleTag(t *testing.T) {
	all := map[string]config.Server{
		"web-eu":  {Role: "app", Tags: map[string]string{"region": "eu"}},
		"web-us":  {Role: "", Tags: map[string]string{"region": "us"}}, // empty role defaults to "app"
		"lb-eu":   {Role: "lb", Tags: map[string]string{"region": "eu"}},
		"unknown": {Role: "app"},
	}
	names := []string{"web-eu", "web-us", "lb-eu", "missing"}

	tests := []struct {
		name string
		role string
		tags map[string]string
		want []string
	}{
		{"role app includes empty-role", "app", nil, []string{"web-eu", "web-us"}},
		{"role lb", "lb", nil, []string{"lb-eu"}},
		{"tag region=eu across roles", "", map[string]string{"region": "eu"}, []string{"web-eu", "lb-eu"}},
		{"role app + tag region=eu", "app", map[string]string{"region": "eu"}, []string{"web-eu"}},
		{"no match", "app", map[string]string{"region": "asia"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := selectServersByRoleTag(names, all, tt.role, tt.tags)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}
