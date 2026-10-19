package confighome

import (
	"bytes"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Merge3 merges two edits of one teploy.yml made from the same base, key
// by key: a key only one side changed takes that side's value; maps merge
// recursively (env:, volumes:, accessories.<name>.env:, ...); lists and
// scalars are atomic. Conflicts lists the dotted paths both sides changed
// differently; merged then holds "mine" for them, so the caller asks the
// operator only about those keys.
func Merge3(base, mine, theirs []byte) (merged []byte, conflicts []string, err error) {
	var b, m, t map[string]any
	for _, p := range []struct {
		raw []byte
		dst *map[string]any
		who string
	}{{base, &b, "base"}, {mine, &m, "your edit"}, {theirs, &t, "the current config"}} {
		if err := yaml.Unmarshal(p.raw, p.dst); err != nil {
			return nil, nil, fmt.Errorf("parsing %s: %w", p.who, err)
		}
		if *p.dst == nil {
			*p.dst = map[string]any{}
		}
	}
	out := merge3Map("", b, m, t, &conflicts)
	sort.Strings(conflicts)
	merged, err = marshalYAML(out)
	return merged, conflicts, err
}

// marshalYAML encodes with the 2-space indent teploy.yml files use.
func marshalYAML(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func merge3Map(path string, base, mine, theirs map[string]any, conflicts *[]string) map[string]any {
	keys := map[string]bool{}
	for _, mm := range []map[string]any{base, mine, theirs} {
		for k := range mm {
			keys[k] = true
		}
	}
	out := map[string]any{}
	for k := range keys {
		p := k
		if path != "" {
			p = path + "." + k
		}
		bv, inB := base[k]
		mv, inM := mine[k]
		tv, inT := theirs[k]
		bm, bIsMap := bv.(map[string]any)
		mm, mIsMap := mv.(map[string]any)
		tm, tIsMap := tv.(map[string]any)
		if mIsMap && tIsMap && (bIsMap || !inB) {
			if bm == nil {
				bm = map[string]any{}
			}
			out[k] = merge3Map(p, bm, mm, tm, conflicts)
			continue
		}
		mineChanged := inM != inB || !reflect.DeepEqual(mv, bv)
		theirsChanged := inT != inB || !reflect.DeepEqual(tv, bv)
		switch {
		case !theirsChanged:
			if inM {
				out[k] = mv
			}
		case !mineChanged:
			if inT {
				out[k] = tv
			}
		case inM == inT && reflect.DeepEqual(mv, tv):
			if inM {
				out[k] = mv
			}
		default:
			*conflicts = append(*conflicts, p)
			if inM {
				out[k] = mv
			}
		}
	}
	return out
}

// SetPath sets (or, with remove, deletes) a dotted key in a YAML document,
// used to apply the operator's choice for one conflicting key.
func SetPath(doc []byte, path string, value any, remove bool) ([]byte, error) {
	var root map[string]any
	if err := yaml.Unmarshal(doc, &root); err != nil {
		return nil, err
	}
	if root == nil {
		root = map[string]any{}
	}
	parts := strings.Split(path, ".")
	cur := root
	for _, part := range parts[:len(parts)-1] {
		next, ok := cur[part].(map[string]any)
		if !ok {
			next = map[string]any{}
			cur[part] = next
		}
		cur = next
	}
	last := parts[len(parts)-1]
	if remove {
		delete(cur, last)
	} else {
		cur[last] = value
	}
	return marshalYAML(root)
}

// GetPath reads a dotted key from a YAML document.
func GetPath(doc []byte, path string) (any, bool) {
	var root map[string]any
	if err := yaml.Unmarshal(doc, &root); err != nil {
		return nil, false
	}
	var cur any = root
	for _, part := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[part]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// Diff renders a unified line diff of a and b (labels name the sides).
// Empty when they are equal.
func Diff(aLabel, bLabel string, a, b []byte) string {
	al := splitLines(string(a))
	bl := splitLines(string(b))
	if strings.Join(al, "\n") == strings.Join(bl, "\n") {
		return ""
	}
	// LCS table: configs are small (bounded at 1 MiB, typically < 200 lines).
	n, m := len(al), len(bl)
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if al[i] == bl[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else if lcs[i+1][j] >= lcs[i][j+1] {
				lcs[i][j] = lcs[i+1][j]
			} else {
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "--- %s\n+++ %s\n", aLabel, bLabel)
	i, j := 0, 0
	for i < n || j < m {
		switch {
		case i < n && j < m && al[i] == bl[j]:
			sb.WriteString(" " + al[i] + "\n")
			i++
			j++
		case i < n && (j == m || lcs[i+1][j] >= lcs[i][j+1]):
			sb.WriteString("-" + al[i] + "\n")
			i++
		default:
			sb.WriteString("+" + bl[j] + "\n")
			j++
		}
	}
	return sb.String()
}

func splitLines(s string) []string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// EditPath sets a dotted key to value (YAML text), or removes it when
// value is nil, keeping the rest of the document — key order and
// comments — as the operator wrote it.
func EditPath(doc []byte, path string, value *string) ([]byte, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(doc, &root); err != nil {
		return nil, err
	}
	if root.Kind == 0 {
		root = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}
	}
	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 || root.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("teploy.yml is not a mapping")
	}
	var val *yaml.Node
	if value != nil {
		var v yaml.Node
		if err := yaml.Unmarshal([]byte(*value), &v); err != nil {
			return nil, fmt.Errorf("value for %s: %w", path, err)
		}
		if len(v.Content) == 0 {
			val = &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null"}
		} else {
			val = v.Content[0]
		}
		// Quotes only served to make the value a string; the encoder adds
		// them back wherever the plain form would read differently.
		if val.Kind == yaml.ScalarNode && val.Tag == "!!str" {
			val.Style = 0
		}
	}
	parts := strings.Split(path, ".")
	cur := root.Content[0]
	for i, part := range parts {
		if part == "" {
			return nil, fmt.Errorf("invalid key %q", path)
		}
		idx := -1
		for j := 0; j+1 < len(cur.Content); j += 2 {
			if cur.Content[j].Value == part {
				idx = j
				break
			}
		}
		if i == len(parts)-1 {
			switch {
			case val == nil && idx >= 0:
				cur.Content = append(cur.Content[:idx], cur.Content[idx+2:]...)
			case val != nil && idx >= 0:
				val.LineComment = cur.Content[idx+1].LineComment
				cur.Content[idx+1] = val
			case val != nil:
				cur.Content = append(cur.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: part}, val)
			}
			break
		}
		if idx < 0 {
			if val == nil {
				return doc, nil
			}
			child := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			cur.Content = append(cur.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: part}, child)
			cur = child
			continue
		}
		next := cur.Content[idx+1]
		if next.Kind != yaml.MappingNode {
			if val == nil {
				return doc, nil
			}
			if next.Tag != "!!null" {
				return nil, fmt.Errorf("%s is not a map", strings.Join(parts[:i+1], "."))
			}
			next.Kind, next.Tag, next.Value = yaml.MappingNode, "!!map", ""
		}
		cur = next
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&root); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ChangedKeys lists the dotted keys (top level, and one level into maps
// such as env:) whose values differ between two configs: names only, for
// the config.set event.
func ChangedKeys(prev, next []byte) []string {
	var a, b map[string]any
	_ = yaml.Unmarshal(prev, &a)
	_ = yaml.Unmarshal(next, &b)
	var out []string
	for k := range unionKeys(a, b) {
		av, bv := a[k], b[k]
		if reflect.DeepEqual(av, bv) {
			continue
		}
		am, aok := av.(map[string]any)
		bm, bok := bv.(map[string]any)
		if (aok || av == nil) && (bok || bv == nil) && (aok || bok) {
			for sub := range unionKeys(am, bm) {
				if !reflect.DeepEqual(am[sub], bm[sub]) {
					out = append(out, k+"."+sub)
				}
			}
			continue
		}
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func unionKeys(maps ...map[string]any) map[string]bool {
	keys := map[string]bool{}
	for _, m := range maps {
		for k := range m {
			keys[k] = true
		}
	}
	return keys
}
