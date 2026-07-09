package cli

import (
	"reflect"
	"testing"

	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/docker"
	"github.com/useteploy/teploy/internal/state"
)

// ctr builds a docker.Container for tests. process="" and role="" omit those
// labels. app is always "blog".
func ctr(name, cState, process, role string) docker.Container {
	labels := map[string]string{"teploy.app": "blog"}
	if process != "" {
		labels["teploy.process"] = process
	}
	if role != "" {
		labels["teploy.role"] = role
	}
	return docker.Container{Name: name, State: cState, Labels: labels}
}

// actionCounts tallies changes by action for assertions.
func actionCounts(changes []planChange) map[string]int {
	m := map[string]int{}
	for _, c := range changes {
		m[c.Action]++
	}
	return m
}

func TestPlanChanges(t *testing.T) {
	tests := []struct {
		name       string
		cfg        *config.AppConfig
		version    string
		current    *state.AppState
		containers []docker.Container
		wantCreate int
		wantStop   int
	}{
		{
			name:    "accessory (db) is not reported as stop",
			cfg:     &config.AppConfig{App: "blog"},
			version: "abc123",
			current: &state.AppState{CurrentHash: "abc123"},
			containers: []docker.Container{
				ctr("blog-web-abc123", "running", "web", ""),
				ctr("blog-db", "running", "", "accessory"),
			},
			wantCreate: 0, wantStop: 0, // web unchanged, db ignored
		},
		{
			name:    "preview container is not reported as stop",
			cfg:     &config.AppConfig{App: "blog"},
			version: "abc123",
			current: &state.AppState{CurrentHash: "abc123"},
			containers: []docker.Container{
				ctr("blog-web-abc123", "running", "web", ""),
				ctr("blog-preview-feat", "running", "preview-feat", ""),
			},
			wantCreate: 0, wantStop: 0,
		},
		{
			name:    "old version is stopped, new is created",
			cfg:     &config.AppConfig{App: "blog"},
			version: "new123",
			current: &state.AppState{CurrentHash: "old999"},
			containers: []docker.Container{
				ctr("blog-web-old999", "running", "web", ""),
			},
			wantCreate: 1, wantStop: 1,
		},
		{
			name:    "matching replicas: no change",
			cfg:     &config.AppConfig{App: "blog", Replicas: 2},
			version: "v1",
			current: &state.AppState{CurrentHash: "v1"},
			containers: []docker.Container{
				ctr("blog-web-v1-1", "running", "web", ""),
				ctr("blog-web-v1-2", "running", "web", ""),
			},
			wantCreate: 0, wantStop: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			changes, _ := planChanges(tt.cfg, tt.version, true, tt.current, tt.containers)
			c := actionCounts(changes)
			if c["create"] != tt.wantCreate {
				t.Errorf("create = %d, want %d (changes=%v)", c["create"], tt.wantCreate, changes)
			}
			if c["stop"] != tt.wantStop {
				t.Errorf("stop = %d, want %d (changes=%v)", c["stop"], tt.wantStop, changes)
			}
		})
	}
}

func names(dcs []desiredContainer) []string {
	out := make([]string, len(dcs))
	for i, d := range dcs {
		out[i] = d.Name
	}
	return out
}

func TestDesiredContainers(t *testing.T) {
	tests := []struct {
		name    string
		cfg     *config.AppConfig
		version string
		want    []string
	}{
		{
			name:    "web only, no processes, defaults to one web",
			cfg:     &config.AppConfig{App: "blog"},
			version: "abc123",
			want:    []string{"blog-web-abc123"},
		},
		{
			name:    "web replicas",
			cfg:     &config.AppConfig{App: "blog", Replicas: 3},
			version: "abc123",
			want: []string{
				"blog-web-abc123-1",
				"blog-web-abc123-2",
				"blog-web-abc123-3",
			},
		},
		{
			name: "web plus workers, one each, workers alphabetical after web",
			cfg: &config.AppConfig{
				App:       "api",
				Processes: map[string]string{"web": "", "worker": "rake jobs", "cron": "clockwork"},
			},
			version: "deadbee",
			want: []string{
				"api-web-deadbee",
				"api-cron-deadbee",
				"api-worker-deadbee",
			},
		},
		{
			name: "replicas apply to web only, workers stay single",
			cfg: &config.AppConfig{
				App:       "api",
				Replicas:  2,
				Processes: map[string]string{"web": "", "worker": "rake jobs"},
			},
			version: "v1",
			want: []string{
				"api-web-v1-1",
				"api-web-v1-2",
				"api-worker-v1",
			},
		},
		{
			name:    "zero replicas normalizes to one",
			cfg:     &config.AppConfig{App: "blog", Replicas: 0},
			version: "abc",
			want:    []string{"blog-web-abc"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := names(desiredContainers(tt.cfg, tt.version))
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("desiredContainers() = %v, want %v", got, tt.want)
			}
		})
	}
}
