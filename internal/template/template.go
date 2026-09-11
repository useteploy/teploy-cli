package template

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	// DefaultRepoURL points to the community template repository.
	//
	// Previously pointed at github.com/teploy/templates — that org never
	// existed (the real org is useteploy), so every `teploy template`
	// command 404'd unconditionally. Confirmed live. The real repo now
	// lives at github.com/useteploy/templates.
	DefaultRepoURL = "https://raw.githubusercontent.com/useteploy/templates/main"
	indexFile      = "index.json"

	// Size bounds for registry responses. The index and individual
	// templates are small documents; anything near a megabyte is a
	// misbehaving registry, not a catalog entry, and should fail before
	// deployment rather than as a mystery later.
	maxIndexSize    = 1 << 20
	maxTemplateSize = 1 << 20
)

// validTemplateName restricts template names to a single safe path
// component. The name is interpolated into the fetch URL, so without this
// a name like "../other" or "a/b" would walk the registry path, and a name
// with "?" or "#" could rewrite the query.
var validTemplateName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// placeholderRe matches a {{name}} variable reference left in rendered
// content.
var placeholderRe = regexp.MustCompile(`\{\{\s*([A-Za-z0-9_]+)\s*\}\}`)

// Info describes a template in the index.
type Info struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Accessories []string `json:"accessories,omitempty"`
	Variables   []string `json:"variables,omitempty"`
}

// Registry fetches and manages templates from the community repo.
type Registry struct {
	baseURL string
	client  *http.Client
}

// NewRegistry creates a template registry with the default repo URL.
func NewRegistry() *Registry {
	return &Registry{
		baseURL: DefaultRepoURL,
		client:  &http.Client{Timeout: 30 * time.Second},
	}
}

// SetBaseURL overrides the template repository URL (useful for testing).
func (r *Registry) SetBaseURL(url string) {
	r.baseURL = url
}

// readLimited reads at most limit bytes, failing clearly when the body is
// larger — an unbounded body would otherwise be buffered whole.
func readLimited(r io.Reader, limit int64, what string) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", what, err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s exceeds the %d MiB size limit", what, limit>>20)
	}
	return data, nil
}

// List fetches the template index and returns all available templates.
func (r *Registry) List(ctx context.Context) ([]Info, error) {
	url := r.baseURL + "/" + indexFile
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching template index: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("template index returned %d", resp.StatusCode)
	}

	body, err := readLimited(resp.Body, maxIndexSize, "template index")
	if err != nil {
		return nil, err
	}

	var index []Info
	if err := json.Unmarshal(body, &index); err != nil {
		return nil, fmt.Errorf("parsing template index: %w", err)
	}

	// A duplicate name makes `template info/deploy <name>` ambiguous —
	// reject the catalog rather than deploying the wrong entry.
	seen := make(map[string]bool, len(index))
	for _, t := range index {
		if seen[t.Name] {
			return nil, fmt.Errorf("template index contains duplicate entry %q", t.Name)
		}
		seen[t.Name] = true
	}
	return index, nil
}

// Fetch downloads a template and applies variable substitution. generated
// reports every "generate" sentinel that got replaced with a random value
// (key -> the value written in), keyed by the YAML key on that line (e.g.
// "POSTGRES_PASSWORD") — callers that deploy immediately (template install)
// need this to show the operator their credentials, since the rendered
// content isn't otherwise written anywhere retrievable. Found by
// inspection while writing new template content: without this, a
// "generate"d database password was used once to deploy and then
// permanently lost — the deployed database becomes unreachable by its own
// operator.
//
// When vars is non-nil the render is a deploy input, so Fetch also fails
// on unreplaced {{placeholders}} (missing variables) and on rendered
// content that is not valid YAML. With nil vars the raw template is
// returned as-is for preview (`template info`).
func (r *Registry) Fetch(ctx context.Context, name string, vars map[string]string) (content string, generated map[string]string, err error) {
	if !validTemplateName.MatchString(name) {
		return "", nil, fmt.Errorf("invalid template name %q", name)
	}

	url := r.baseURL + "/" + name + "/teploy.yml"
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "", nil, err
	}

	resp, err := r.client.Do(req)
	if err != nil {
		return "", nil, fmt.Errorf("fetching template %s: %w", name, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == 404 {
		return "", nil, fmt.Errorf("template %q not found", name)
	}
	if resp.StatusCode != 200 {
		return "", nil, fmt.Errorf("template fetch returned %d", resp.StatusCode)
	}

	body, err := readLimited(resp.Body, maxTemplateSize, "template")
	if err != nil {
		return "", nil, err
	}

	content = string(body)

	// Apply variable substitution.
	content = substituteVariables(content, vars)

	// Generate secrets.
	content, generated = GenerateSecrets(content)

	if vars != nil {
		if missing := findMissingVariables(content); len(missing) > 0 {
			return "", nil, fmt.Errorf("template %q requires variables not supplied: %s (pass --var name=value)", name, strings.Join(missing, ", "))
		}
		var doc yaml.Node
		if err := yaml.Unmarshal([]byte(content), &doc); err != nil {
			return "", nil, fmt.Errorf("rendered template %q is not valid YAML: %w", name, err)
		}
	}

	return content, generated, nil
}

// findMissingVariables returns the sorted, deduplicated names of all
// {{placeholders}} still present in content.
func findMissingVariables(content string) []string {
	var missing []string
	seen := make(map[string]bool)
	for _, m := range placeholderRe.FindAllStringSubmatch(content, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			missing = append(missing, m[1])
		}
	}
	sort.Strings(missing)
	return missing
}

// mapValueRe splits a mapping line into indent (with an optional sequence
// dash), key, separator, and raw value text. A quoted key alternative is
// listed first so keys containing colons match as keys.
var mapValueRe = regexp.MustCompile(`^(\s*(?:-\s+)?)(("[^"]*")|('[^']*')|([^:\s][^:]*)):(\s+)(.*)$`)

// seqItemRe matches a plain sequence item whose text is not itself a
// mapping (mapping items are handled by mapValueRe).
var seqItemRe = regexp.MustCompile(`^(\s*-\s+)(.+)$`)

// substituteVariables replaces {{name}} placeholders with the supplied
// values. Substituted text must survive as the exact same string after the
// rendered file is parsed: a value containing quotes, ": ", "#", a
// newline, or something YAML reads as a bool/null/number would silently
// corrupt or restructure the document when inserted raw, so scalar values
// are re-quoted as needed (see plainScalarSafe). Placeholders in comments,
// keys, flow collections, and block scalars keep the historical raw
// replacement — they hold no YAML scalar semantics.
func substituteVariables(content string, vars map[string]string) string {
	if len(vars) == 0 {
		return content
	}
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		if !strings.Contains(line, "{{") {
			continue
		}
		lines[i] = substituteLine(line, vars)
	}
	return strings.Join(lines, "\n")
}

func substituteLine(line string, vars map[string]string) string {
	if strings.HasPrefix(strings.TrimSpace(line), "#") {
		return replaceRaw(line, vars)
	}
	if m := mapValueRe.FindStringSubmatch(line); m != nil {
		prefix, key, sep, value := m[1], m[2], m[6], m[7]
		if !strings.Contains(value, "{{") && !strings.Contains(key, "{{") {
			return line
		}
		return prefix + replaceRaw(key, vars) + ":" + sep + substituteScalar(value, vars)
	}
	if m := seqItemRe.FindStringSubmatch(line); m != nil {
		return m[1] + substituteScalar(m[2], vars)
	}
	return replaceRaw(line, vars)
}

// doubleQuotedFullRe / singleQuotedFullRe split a leading quoted scalar
// into its interior and the remainder of the line (trailing comment etc).
var (
	doubleQuotedFullRe = regexp.MustCompile(`^"((?:[^"\\]|\\.)*)"(.*)$`)
	singleQuotedFullRe = regexp.MustCompile(`^'((?:[^']|'')*)'(.*)$`)
)

// substituteScalar renders the value text of a mapping or sequence line,
// keeping the result a single YAML scalar that reads back as the exact
// substituted string.
func substituteScalar(value string, vars map[string]string) string {
	if !strings.Contains(value, "{{") {
		return value
	}

	// Flow collections and block scalars keep raw replacement: quoting
	// rules there are positional, not scalar-wide. A value that begins
	// with a placeholder is a scalar being substituted, not a flow
	// collection — "{{var}}" starts with "{" and must not fall in here.
	if !strings.HasPrefix(value, "{{") {
		if strings.HasPrefix(value, "[") || strings.HasPrefix(value, "{") ||
			strings.HasPrefix(value, "|") || strings.HasPrefix(value, ">") {
			return replaceRaw(value, vars)
		}
	}

	// Quoted scalars: "#" and ": " are literal inside the quotes, so only
	// the inserted text is escaped; the template's own escapes stay
	// untouched (no double-escaping).
	if strings.HasPrefix(value, `"`) {
		if m := doubleQuotedFullRe.FindStringSubmatch(value); m != nil {
			return `"` + replaceEach(m[1], vars, escapeDoubleQuoted) + `"` + replaceRaw(m[2], vars)
		}
	}
	if strings.HasPrefix(value, `'`) {
		if m := singleQuotedFullRe.FindStringSubmatch(value); m != nil {
			inner, rest := m[1], m[2]
			// A single-quoted scalar cannot carry an embedded newline
			// (line breaks fold to spaces), so a value containing one
			// converts the whole scalar to double-quoted form.
			for k, v := range vars {
				if strings.ContainsAny(v, "\n\r") && strings.Contains(inner, "{{"+k+"}}") {
					s := replaceEach(strings.ReplaceAll(inner, "''", "'"), vars, func(v string) string { return v })
					return `"` + escapeDoubleQuoted(s) + `"` + replaceRaw(rest, vars)
				}
			}
			return `'` + replaceEach(inner, vars, func(v string) string { return strings.ReplaceAll(v, "'", "''") }) + `'` + replaceRaw(rest, vars)
		}
	}

	// Plain scalar: a trailing " #" starts a comment, which belongs to the
	// line rather than the value — substitute it raw and keep quoting
	// decisions on the value only. Plain scalars shed trailing whitespace
	// at parse time, so drop it rather than freeze it into a quoted value.
	scalar, comment := value, ""
	if i := strings.Index(value, " #"); i >= 0 {
		scalar, comment = value[:i], value[i:]
	}
	scalar = strings.TrimRight(scalar, " \t")

	if !strings.Contains(scalar, "{{") {
		return scalar + replaceRaw(comment, vars)
	}

	s := replaceRaw(scalar, vars)
	if plainScalarSafe(s) {
		return s + replaceRaw(comment, vars)
	}
	return `"` + escapeDoubleQuoted(s) + `"` + replaceRaw(comment, vars)
}

// replaceEach replaces every {{k}} with esc(vars[k]).
func replaceEach(s string, vars map[string]string, esc func(string) string) string {
	for k, v := range vars {
		s = strings.ReplaceAll(s, "{{"+k+"}}", esc(v))
	}
	return s
}

func replaceRaw(s string, vars map[string]string) string {
	return replaceEach(s, vars, func(v string) string { return v })
}

// escapeDoubleQuoted serializes s as the interior of a YAML double-quoted
// scalar.
func escapeDoubleQuoted(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&b, `\x%02x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}

// plainScalarSafe reports whether s can be written as an unquoted plain
// scalar and still read back as the exact same string: no YAML indicators,
// no comment/Mapping separators, no control characters, and nothing YAML
// would resolve to a bool/null/number/timestamp instead of a string
// (template variables are strings by contract — "true" and "007" must
// survive as strings).
func plainScalarSafe(s string) bool {
	if s == "" || strings.TrimSpace(s) != s {
		return false
	}
	if strings.ContainsAny(s, "\n\r\t") {
		return false
	}
	if strings.Contains(s, ": ") || strings.HasSuffix(s, ":") {
		return false
	}
	if strings.Contains(s, " #") {
		return false
	}
	if strings.ContainsAny(s[:1], "-?:,[]{}#&*!|>'\"%@`") {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return yamlScalarIsString(s)
}

// yamlScalarIsString reports whether s, as a plain scalar, resolves to a
// string tag under the same parser that later reads the rendered file.
func yamlScalarIsString(s string) bool {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(s), &doc); err != nil || len(doc.Content) == 0 {
		return false
	}
	return doc.Content[0].ShortTag() == "!!str"
}

// GenerateSecrets replaces "generate" env values with random 64-char hex
// strings, returning the rendered content and a key->generated-value map
// for every substitution made (see Fetch's doc comment for why callers
// need this).
//
// Map keys are the bare YAML key when that key is unique in the file (the
// historical shape: "POSTGRES_PASSWORD"). When the same key appears at
// more than one path — two accessories that each generate PASSWORD — bare
// keys would silently overwrite each other and one credential would drop
// out of the report entirely, so colliding entries are keyed by their full
// dotted path instead (e.g. "accessories.db.env.PASSWORD").
func GenerateSecrets(content string) (rendered string, generated map[string]string) {
	type secretEntry struct{ parent, key, secret string }
	var entries []secretEntry
	var lines []string

	var stack []string
	var stackIndent []int
	parent := func() string { return strings.Join(stack, ".") }

	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasSuffix(trimmed, ": generate") || strings.HasSuffix(trimmed, ": \"generate\"") {
			indent := line[:len(line)-len(strings.TrimLeft(line, " "))]
			key := strings.TrimSuffix(strings.TrimSuffix(trimmed, ": generate"), ": \"generate\"")
			secret := RandomHex(32)
			lines = append(lines, fmt.Sprintf("%s%s: %s", indent, key, secret))
			entries = append(entries, secretEntry{parent: parent(), key: key, secret: secret})
			continue
		}
		lines = append(lines, line)
		trackMappingPath(line, &stack, &stackIndent)
	}

	generated = make(map[string]string)
	counts := make(map[string]int)
	for _, e := range entries {
		counts[e.key]++
	}
	for _, e := range entries {
		if counts[e.key] == 1 || e.parent == "" {
			generated[e.key] = e.secret
		} else {
			generated[e.parent+"."+e.key] = e.secret
		}
	}
	return strings.Join(lines, "\n"), generated
}

// mappingKeyRe matches a mapping key line (with or without a value), for
// path tracking only.
var mappingKeyRe = regexp.MustCompile(`^(\s*)(-\s*)?(("[^"]*")|('[^']*')|([^:\s][^:]*)):(\s.*)?$`)

// trackMappingPath maintains the stack of enclosing mapping keys so a
// "generate" line can be attributed to its full path. Comment and blank
// lines are ignored; a key after a sequence dash nests at the dash's key
// column.
func trackMappingPath(line string, stack *[]string, stackIndent *[]int) {
	if line == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
		return
	}
	m := mappingKeyRe.FindStringSubmatch(line)
	if m == nil {
		return
	}
	indent := len(m[1])
	key := m[3]
	if m[2] != "" {
		indent += len(m[2])
	}
	for len(*stackIndent) > 0 && (*stackIndent)[len(*stackIndent)-1] >= indent {
		*stack = (*stack)[:len(*stack)-1]
		*stackIndent = (*stackIndent)[:len(*stackIndent)-1]
	}
	*stack = append(*stack, strings.Trim(key, `"'`))
	*stackIndent = append(*stackIndent, indent)
}

// RandomHex generates a random hex string of n bytes (2n chars).
func RandomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)
}
