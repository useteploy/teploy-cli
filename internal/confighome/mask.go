package confighome

import (
	"bytes"

	"gopkg.in/yaml.v3"

	"github.com/useteploy/teploy/internal/releasemeta"
)

// Masked replaces a detected secret value in a stored source config.
const Masked = "<redacted by teploy>"

// MaskSource returns a folder teploy.yml with every detected secret value
// masked (spec §2.5): env: and env_literal: values the release-record
// classifier would not keep (secret-looking names, credential URLs), for
// the app and its accessories, and the credential fields teploy knows.
// Comments and layout are kept. Best-effort, like the classifier.
func MaskSource(raw []byte) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return raw, nil
	}
	child := func(m *yaml.Node, key string) *yaml.Node {
		if m == nil || m.Kind != yaml.MappingNode {
			return nil
		}
		for i := 0; i+1 < len(m.Content); i += 2 {
			if m.Content[i].Value == key {
				return m.Content[i+1]
			}
		}
		return nil
	}
	maskEnv := func(env *yaml.Node, source string) {
		if env == nil || env.Kind != yaml.MappingNode {
			return
		}
		for i := 0; i+1 < len(env.Content); i += 2 {
			k, v := env.Content[i].Value, env.Content[i+1]
			if v.Kind != yaml.ScalarNode || v.Value == "" {
				continue
			}
			if keep, _ := releasemeta.ClassifyEnv(k, v.Value, map[string]string{k: source}, nil); !keep {
				v.Value, v.Tag, v.Style = Masked, "!!str", 0
			}
		}
	}
	maskScalar := func(n *yaml.Node) {
		if n != nil && n.Kind == yaml.ScalarNode && n.Value != "" {
			n.Value, n.Tag, n.Style = Masked, "!!str", 0
		}
	}
	root := doc.Content[0]
	maskEnv(child(root, "env"), releasemeta.EnvSourceConfig)
	maskEnv(child(root, "env_literal"), releasemeta.EnvSourceConfigLiteral)
	if accs := child(root, "accessories"); accs != nil && accs.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(accs.Content); i += 2 {
			maskEnv(child(accs.Content[i+1], "env"), releasemeta.EnvSourceConfig)
			maskEnv(child(accs.Content[i+1], "env_literal"), releasemeta.EnvSourceConfigLiteral)
		}
	}
	notif := child(root, "notifications")
	maskScalar(child(notif, "secret"))
	if chans := child(notif, "channels"); chans != nil && chans.Kind == yaml.SequenceNode {
		for _, ch := range chans.Content {
			maskScalar(child(ch, "secret"))
		}
	}
	maskScalar(child(child(root, "audit"), "token"))
	maskScalar(child(child(root, "network"), "auth_key"))
	maskScalar(child(child(root, "network"), "setup_key"))
	if access := child(root, "access"); access != nil {
		if ba := child(access, "basic_auth"); ba != nil && ba.Kind == yaml.MappingNode {
			for i := 1; i < len(ba.Content); i += 2 {
				maskScalar(ba.Content[i])
			}
		}
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
