package template

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/config"
)

// corpusValues maps every catalog-declared variable to a deliberately
// YAML-hostile value — quotes, colon-space, hash, newlines, tabs, and
// bool/number lookalikes — so rendering the whole catalog exercises the
// scalar quoting rules against every placeholder position templates
// actually use (audit useteploy__templates-03; the per-value round-trip
// unit tests live in template_test.go).
var corpusValues = map[string]string{
	"domain":      "app.example.com",
	"db_password": "p\"ss: #x\ntrue\t007\\end",
	"var3":        "with 'single' and \"double\" quotes",
	"var4":        "$HOME `cmd` ~user",
	"var5":        "007",
	"var6":        "a: b # c",
}

// TestRenderAllCatalogTemplates renders every entry of the templates
// catalog (checkout of useteploy/templates pointed at by
// TEMPLATES_REPO_DIR) through the exact production path — variable
// substitution, secret generation, parse with the real Teploy config
// parser — and fails on any residual placeholder or invalid result.
// The templates repo's CI runs this against teploy-cli@main so catalog
// and parser cannot drift apart silently.
func TestRenderAllCatalogTemplates(t *testing.T) {
	root := os.Getenv("TEMPLATES_REPO_DIR")
	if root == "" {
		t.Skip("TEMPLATES_REPO_DIR not set")
	}

	raw, err := os.ReadFile(filepath.Join(root, "index.json"))
	if err != nil {
		t.Fatalf("read index.json: %v", err)
	}
	var index []Info
	if err := json.Unmarshal(raw, &index); err != nil {
		t.Fatalf("parse index.json: %v", err)
	}
	if len(index) == 0 {
		t.Fatal("empty catalog")
	}

	rendered := 0
	for _, entry := range index {
		t.Run(entry.Name, func(t *testing.T) {
			tpl, err := os.ReadFile(filepath.Join(root, entry.Name, "teploy.yml"))
			if err != nil {
				t.Fatalf("read teploy.yml: %v", err)
			}

			vars := map[string]string{}
			for _, name := range entry.Variables {
				v, ok := corpusValues[name]
				if !ok {
					v = corpusValues["db_password"]
				}
				vars[name] = v
			}

			content := substituteVariables(string(tpl), vars)
			content, generated := GenerateSecrets(content)

			if strings.Contains(content, "{{") {
				t.Errorf("residual placeholder after render:\n%s", content)
			}
			if strings.Contains(content, ": generate") {
				t.Errorf("unreplaced generate sentinel:\n%s", content)
			}
			if len(generated) == 0 && hasGenerateSentinel(string(tpl)) {
				t.Errorf("template declares generate sentinels but none were consumed")
			}

			if _, err := config.ParseAppBytes([]byte(content)); err != nil {
				t.Errorf("rendered template does not parse as a Teploy config: %v\n%s", err, content)
			}
			rendered++
		})
	}
	// Guard against a renamed env var silently skipping every entry.
	if rendered == 0 {
		t.Fatal("no templates were rendered")
	}
}

// hasGenerateSentinel reports whether any line ends in the exact
// "generate" sentinel shapes GenerateSecrets consumes (prose like
// "generates RSS feeds" in comments does not count).
func hasGenerateSentinel(content string) bool {
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasSuffix(trimmed, ": generate") || strings.HasSuffix(trimmed, ": \"generate\"") {
			return true
		}
	}
	return false
}
