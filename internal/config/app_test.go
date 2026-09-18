package config

import "testing"

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
