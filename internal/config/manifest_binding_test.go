package config

import (
	"encoding/json"
	"testing"
)

func TestExecutionBindingIncludesCommandsAndPolicy(t *testing.T) {
	base := AppConfig{App: "demo", Processes: map[string]string{"web": "server --safe"}, Hooks: HooksConfig{PreDeploy: "check safe"}, CaddyExtra: "header X-Mode safe", Env: map[string]string{"TOKEN": "dummy-one"}}
	first, err := ExecutionBindingDigest(&base, "image:v1")
	if err != nil {
		t.Fatal(err)
	}
	changes := []func(*AppConfig){
		func(c *AppConfig) { c.Processes["web"] = "server --unsafe" },
		func(c *AppConfig) { c.Hooks.PreDeploy = "check unsafe" },
		func(c *AppConfig) { c.CaddyExtra = "header X-Mode unsafe" },
		func(c *AppConfig) { c.Build = []string{"echo unsafe"} },
		func(c *AppConfig) { c.Headers = map[string]string{"X-Policy": "changed"} },
		func(c *AppConfig) {
			c.Accessories = map[string]AccessoryConfig{"db": {CommandArgs: []string{"redis-server", "--appendonly", "yes"}}}
		},
	}
	for i, change := range changes {
		data, _ := json.Marshal(base)
		var cfg AppConfig
		json.Unmarshal(data, &cfg)
		change(&cfg)
		digest, err := ExecutionBindingDigest(&cfg, "image:v1")
		if err != nil {
			t.Fatal(err)
		}
		if digest == first {
			t.Fatalf("change %d not bound", i)
		}
	}
	base.Env["TOKEN"] = "dummy-two"
	same, _ := ExecutionBindingDigest(&base, "image:v1")
	if same != first {
		t.Fatal("environment value policy unexpectedly changed")
	}
	if base.Env["TOKEN"] != "dummy-two" {
		t.Fatal("digest mutated config")
	}
}
