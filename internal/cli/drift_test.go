package cli

import (
	"context"
	"testing"

	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/docker"
	"github.com/useteploy/teploy/internal/ssh"
	"github.com/useteploy/teploy/internal/state"
)

// psLine builds one `docker ps --format {{json .}}` line.
func psLine(name, state, labels string) string {
	return `{"ID":"` + name + `","Names":"` + name + `","Image":"img:latest","State":"` +
		state + `","Status":"Up","CreatedAt":"2026-01-01 00:00:00 +0000 UTC","Labels":"` + labels + `"}`
}

func TestDriftItems(t *testing.T) {
	tests := []struct {
		name       string
		cfg        *config.AppConfig
		current    *state.AppState
		containers []docker.Container
		wantItems  int
	}{
		{
			name:    "preview container is not unexpected drift",
			cfg:     &config.AppConfig{App: "blog"},
			current: &state.AppState{CurrentHash: "abc123"},
			containers: []docker.Container{
				ctr("blog-web-abc123", "running", "web", ""),
				ctr("blog-preview-feat", "running", "preview-feat", ""),
			},
			wantItems: 0,
		},
		{
			name:    "accessory is not unexpected drift",
			cfg:     &config.AppConfig{App: "blog"},
			current: &state.AppState{CurrentHash: "abc123"},
			containers: []docker.Container{
				ctr("blog-web-abc123", "running", "web", ""),
				ctr("blog-db", "running", "", "accessory"),
			},
			wantItems: 0,
		},
		{
			name:    "missing replica is drift",
			cfg:     &config.AppConfig{App: "blog", Replicas: 2},
			current: &state.AppState{CurrentHash: "abc123"},
			containers: []docker.Container{
				ctr("blog-web-abc123-1", "running", "web", ""),
			},
			wantItems: 1, // blog-web-abc123-2 missing
		},
		{
			name:    "old version running + declared missing = 2 items",
			cfg:     &config.AppConfig{App: "blog"},
			current: &state.AppState{CurrentHash: "abc123"},
			containers: []docker.Container{
				ctr("blog-web-old999", "running", "web", ""),
			},
			wantItems: 2, // abc123 missing + old999 unexpected
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := driftItems(tt.cfg, tt.current, tt.containers)
			if len(got) != tt.wantItems {
				t.Errorf("driftItems() = %d items, want %d (%v)", len(got), tt.wantItems, got)
			}
		})
	}
}

func TestReportDrift(t *testing.T) {
	const appLabels = "teploy.app=blog,teploy.process=web,teploy.version=abc123"
	const oldLabels = "teploy.app=blog,teploy.process=web,teploy.version=old999"
	const accLabels = "teploy.app=blog,teploy.role=accessory,teploy.accessory=db"

	tests := []struct {
		name      string
		stateOut  string
		psOut     string
		wantDrift bool
	}{
		{
			name:      "in sync — declared web running at deployed version",
			stateOut:  "current_hash=abc123\ncurrent_port=3000\n",
			psOut:     psLine("blog-web-abc123", "running", appLabels),
			wantDrift: false,
		},
		{
			name:      "missing — declared web not running",
			stateOut:  "current_hash=abc123\ncurrent_port=3000\n",
			psOut:     "",
			wantDrift: true,
		},
		{
			name:     "unexpected old version left running + declared missing",
			stateOut: "current_hash=abc123\ncurrent_port=3000\n",
			psOut:    psLine("blog-web-old999", "running", oldLabels),
			// blog-web-abc123 declared but not running (missing) AND
			// blog-web-old999 running but not declared (unexpected)
			wantDrift: true,
		},
		{
			name:      "accessory does not count as unexpected drift",
			stateOut:  "current_hash=abc123\ncurrent_port=3000\n",
			psOut:     psLine("blog-web-abc123", "running", appLabels) + "\n" + psLine("blog-db", "running", accLabels),
			wantDrift: false,
		},
		{
			name:      "not deployed — no drift",
			stateOut:  "",
			psOut:     "",
			wantDrift: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := ssh.NewMockExecutor("test-host",
				ssh.MockCommand{Match: "cat /deployments/blog/state", Output: tt.stateOut},
				ssh.MockCommand{Match: "docker ps", Output: tt.psOut},
			)
			appCfg := &config.AppConfig{App: "blog"}

			got, err := reportDrift(context.Background(), &Flags{JSON: true}, appCfg, mock)
			if err != nil {
				t.Fatalf("reportDrift() error: %v", err)
			}
			if got != tt.wantDrift {
				t.Errorf("reportDrift() drift = %v, want %v", got, tt.wantDrift)
			}
		})
	}
}
