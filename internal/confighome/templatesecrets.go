package confighome

import (
	"bytes"
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// ExtractSecrets takes a rendered template and the values that must never
// enter the config repository (generated passwords, --var-stdin input) and
// moves them out (spec §2.3): an app env:/env_literal: value containing one
// is removed from the config and returned in app (to `teploy secret set`,
// which the age store injects into the app); an accessory env: value
// becomes `secret:KEY` and its value is returned in app too (accessories
// resolve secret: refs from the same store). A secret anywhere else in the
// file is refused — it would be committed.
func ExtractSecrets(content []byte, secrets []string) ([]byte, map[string]string, error) {
	var vals []string
	for _, s := range secrets {
		if s != "" {
			vals = append(vals, s)
		}
	}
	if len(vals) == 0 {
		return content, nil, nil
	}
	contains := func(v string) bool {
		for _, s := range vals {
			if strings.Contains(v, s) {
				return true
			}
		}
		return false
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(content, &doc); err != nil {
		return nil, nil, err
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, nil, fmt.Errorf("template must be a YAML mapping")
	}
	moved := map[string]string{}
	var problems []string
	put := func(key, value string) {
		if prev, ok := moved[key]; ok && prev != value {
			problems = append(problems, fmt.Sprintf("%s is used for two different secret values", key))
		}
		moved[key] = value
	}
	child := func(m *yaml.Node, key string) *yaml.Node {
		for i := 0; i+1 < len(m.Content); i += 2 {
			if m.Content[i].Value == key {
				return m.Content[i+1]
			}
		}
		return nil
	}
	// App env: remove the key, store the value.
	root := doc.Content[0]
	for _, field := range []string{"env", "env_literal"} {
		env := child(root, field)
		if env == nil || env.Kind != yaml.MappingNode {
			continue
		}
		var kept []*yaml.Node
		for i := 0; i+1 < len(env.Content); i += 2 {
			k, v := env.Content[i], env.Content[i+1]
			if v.Kind == yaml.ScalarNode && contains(v.Value) {
				put(k.Value, v.Value)
				continue
			}
			kept = append(kept, k, v)
		}
		env.Content = kept
	}
	// Accessory env: the value becomes a secret: reference under env:
	// (env_literal is never resolved, so a secret there moves to env:).
	if accs := child(root, "accessories"); accs != nil && accs.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(accs.Content); i += 2 {
			acc := accs.Content[i+1]
			if acc.Kind != yaml.MappingNode {
				continue
			}
			env := child(acc, "env")
			if env != nil && env.Kind == yaml.MappingNode {
				for j := 0; j+1 < len(env.Content); j += 2 {
					k, v := env.Content[j], env.Content[j+1]
					if v.Kind == yaml.ScalarNode && contains(v.Value) {
						put(k.Value, v.Value)
						v.Value, v.Tag, v.Style = "secret:"+k.Value, "!!str", 0
					}
				}
			}
			lit := child(acc, "env_literal")
			if lit == nil || lit.Kind != yaml.MappingNode {
				continue
			}
			var kept []*yaml.Node
			for j := 0; j+1 < len(lit.Content); j += 2 {
				k, v := lit.Content[j], lit.Content[j+1]
				if v.Kind != yaml.ScalarNode || !contains(v.Value) {
					kept = append(kept, k, v)
					continue
				}
				put(k.Value, v.Value)
				if env == nil || env.Kind != yaml.MappingNode {
					env = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
					acc.Content = append(acc.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "env"}, env)
				}
				env.Content = append(env.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: k.Value}, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "secret:" + k.Value})
			}
			lit.Content = kept
		}
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, nil, err
	}
	if contains(buf.String()) {
		problems = append(problems, "the template puts a generated or supplied secret outside env: (it would be stored in the config history)")
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return nil, nil, fmt.Errorf("cannot install this template into Teploy home:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return buf.Bytes(), moved, nil
}
