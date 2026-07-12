package config

import "testing"

func TestCanaryCount(t *testing.T) {
	tests := []struct {
		spec    string
		total   int
		want    int
		wantErr bool
	}{
		{"", 5, 1, false},     // default = 1
		{"2", 5, 2, false},    // explicit count
		{"10", 5, 4, false},   // clamped to total-1 (whole fleet is not a canary)
		{"1", 2, 1, false},    // minimum viable fleet
		{"10%", 5, 1, false},  // percent rounds up, never zero
		{"50%", 4, 2, false},  // exact percent
		{"100%", 5, 4, false}, // clamped to total-1
		{"0", 5, 0, true},     // invalid count
		{"-1", 5, 0, true},    // invalid count
		{"abc", 5, 0, true},   // garbage
		{"0%", 5, 0, true},    // invalid percent
		{"101%", 5, 0, true},  // invalid percent
	}
	for _, tc := range tests {
		r := &RolloutConfig{Canary: tc.spec}
		got, err := r.CanaryCount(tc.total)
		if tc.wantErr != (err != nil) {
			t.Errorf("CanaryCount(%q, %d): err = %v, wantErr = %v", tc.spec, tc.total, err, tc.wantErr)
			continue
		}
		if !tc.wantErr && got != tc.want {
			t.Errorf("CanaryCount(%q, %d) = %d, want %d", tc.spec, tc.total, got, tc.want)
		}
	}
}

func TestRolloutOverlayMerge(t *testing.T) {
	base := &AppConfig{App: "x"}
	overlay := &AppConfig{Rollout: &RolloutConfig{Canary: "2", MaxFailures: 1}}
	mergeConfigs(base, overlay)
	if base.Rollout == nil || base.Rollout.Canary != "2" || base.Rollout.MaxFailures != 1 {
		t.Fatalf("overlay rollout not merged: %+v", base.Rollout)
	}
}
