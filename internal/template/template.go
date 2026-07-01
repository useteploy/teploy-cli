package template

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
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
)

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

	var index []Info
	if err := json.NewDecoder(resp.Body).Decode(&index); err != nil {
		return nil, fmt.Errorf("parsing template index: %w", err)
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
func (r *Registry) Fetch(ctx context.Context, name string, vars map[string]string) (content string, generated map[string]string, err error) {
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

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", nil, fmt.Errorf("reading template: %w", err)
	}

	content = string(body)

	// Apply variable substitution.
	for k, v := range vars {
		content = strings.ReplaceAll(content, "{{"+k+"}}", v)
	}

	// Generate secrets.
	content, generated = GenerateSecrets(content)

	return content, generated, nil
}

// GenerateSecrets replaces "generate" env values with random 64-char hex
// strings, returning the rendered content and a key->generated-value map
// for every substitution made (see Fetch's doc comment for why callers
// need this).
func GenerateSecrets(content string) (rendered string, generated map[string]string) {
	generated = make(map[string]string)
	var lines []string
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasSuffix(trimmed, ": generate") || strings.HasSuffix(trimmed, ": \"generate\"") {
			indent := line[:len(line)-len(strings.TrimLeft(line, " "))]
			key := strings.TrimSuffix(strings.TrimSuffix(trimmed, ": generate"), ": \"generate\"")
			secret := RandomHex(32)
			lines = append(lines, fmt.Sprintf("%s%s: %s", indent, key, secret))
			generated[key] = secret
		} else {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n"), generated
}

// RandomHex generates a random hex string of n bytes (2n chars).
func RandomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)
}
