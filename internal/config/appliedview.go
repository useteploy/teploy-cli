package config

import (
	"encoding/json"
	"fmt"
)

// AppliedManifestView is the read side of the redacted applied manifest
// NormalizeAndDigest writes into release state: the subset of recorded
// fields a consumer diffs planned config against (C05 plan/apply). It
// lives next to the writer so the shape has one home — a field dropped
// here while NormalizeAndDigest still writes it is a compile-adjacent
// test failure, not a silent plan blind spot. Values stay redacted:
// environment appears as key presence only, secrets never enter.
type AppliedManifestView struct {
	App            string
	DeploymentType string
	Domain         string
	IngressMode    string
	Container      *AppliedManifestContainer
	Accessories    map[string]AppliedManifestAccessory
}

// AppliedManifestContainer is the recorded container deployment shape.
// A nil pointer means the deployed release was static (no container
// section in the manifest).
type AppliedManifestContainer struct {
	Image     string
	Port      int
	Replicas  int
	Bind      string
	EnvKeys   []string
	EnvFiles  []string
	Volumes   map[string]string
	Memory    string
	CPU       string
	Publish   []string
	Processes []string
}

// AppliedManifestAccessory is the recorded accessory shape.
type AppliedManifestAccessory struct {
	Image   string
	Port    int
	EnvKeys []string
	Volumes map[string]string
	Publish []string
}

// ParseAppliedManifest decodes a manifest written by NormalizeAndDigest.
// Malformed JSON, a non-object root, or a container section that is not
// an object is an error — a plan that cannot read what was deployed must
// refuse, never guess (the ReadAttemptProvenance discipline).
func ParseAppliedManifest(data []byte) (*AppliedManifestView, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("applied manifest is empty")
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("parsing applied manifest: %w", err)
	}
	view := &AppliedManifestView{}
	decodeString(root, "app", &view.App)
	decodeString(root, "deployment_type", &view.DeploymentType)
	decodeString(root, "domain", &view.Domain)
	decodeString(root, "ingress_mode", &view.IngressMode)

	if raw, ok := root["container"]; ok && len(raw) > 0 && string(raw) != "null" {
		var c struct {
			Image     string            `json:"image"`
			Port      int               `json:"port"`
			Replicas  int               `json:"replicas"`
			Bind      string            `json:"bind"`
			EnvKeys   []string          `json:"env_keys"`
			EnvFiles  []string          `json:"env_files"`
			Volumes   map[string]string `json:"volumes"`
			Memory    string            `json:"memory"`
			CPU       string            `json:"cpu"`
			Publish   []string          `json:"publish"`
			Processes []string          `json:"processes"`
		}
		if err := json.Unmarshal(raw, &c); err != nil {
			return nil, fmt.Errorf("parsing applied manifest container section: %w", err)
		}
		view.Container = &AppliedManifestContainer{
			Image:     c.Image,
			Port:      c.Port,
			Replicas:  c.Replicas,
			Bind:      c.Bind,
			EnvKeys:   c.EnvKeys,
			EnvFiles:  c.EnvFiles,
			Volumes:   c.Volumes,
			Memory:    c.Memory,
			CPU:       c.CPU,
			Publish:   c.Publish,
			Processes: c.Processes,
		}
	}

	if raw, ok := root["accessories"]; ok && len(raw) > 0 && string(raw) != "null" {
		var accs map[string]struct {
			Image   string            `json:"image"`
			Port    int               `json:"port"`
			EnvKeys []string          `json:"env_keys"`
			Volumes map[string]string `json:"volumes"`
			Publish []string          `json:"publish"`
		}
		if err := json.Unmarshal(raw, &accs); err != nil {
			return nil, fmt.Errorf("parsing applied manifest accessories section: %w", err)
		}
		view.Accessories = make(map[string]AppliedManifestAccessory, len(accs))
		for name, a := range accs {
			view.Accessories[name] = AppliedManifestAccessory{
				Image:   a.Image,
				Port:    a.Port,
				EnvKeys: a.EnvKeys,
				Volumes: a.Volumes,
				Publish: a.Publish,
			}
		}
	}
	return view, nil
}

func decodeString(root map[string]json.RawMessage, key string, dst *string) {
	if raw, ok := root[key]; ok {
		_ = json.Unmarshal(raw, dst)
	}
}
