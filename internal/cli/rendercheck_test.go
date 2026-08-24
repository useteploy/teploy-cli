package cli

import (
	"os"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/config"
	tmpl "github.com/useteploy/teploy/internal/template"
)

// Renders the REAL ship template file through the same path `template
// install` uses: secret generation + parse + override validation. Point
// SHIP_TEMPLATE_FILE at the checkout of useteploy/templates.
func TestRenderRealShipTemplate(t *testing.T) {
	path := os.Getenv("SHIP_TEMPLATE_FILE")
	if path == "" {
		t.Skip("SHIP_TEMPLATE_FILE not set")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	rendered, generated := tmpl.GenerateSecrets(string(raw))
	for _, key := range []string{"SHIP_WEB_TOKEN", "SHIP_WEBHOOK_SECRET", "SHIP_SESSION_SECRET"} {
		if _, ok := generated[key]; !ok {
			t.Errorf("expected %s to be generated", key)
		}
	}
	if strings.Contains(rendered, ": generate") {
		t.Error("unreplaced generate sentinel remains")
	}
	appCfg, err := config.ParseAppBytes([]byte(rendered))
	if err != nil {
		t.Fatalf("ParseAppBytes: %v", err)
	}
	if appCfg.Ingress != config.IngressHost || appCfg.Port != 7460 {
		t.Errorf("expected host ingress on 7460, got %q/%d", appCfg.Ingress, appCfg.Port)
	}
	if len(appCfg.Processes) != 2 {
		t.Errorf("expected web+worker processes, got %v", appCfg.Processes)
	}
	if _, ok := appCfg.Accessories["nucleus"]; !ok {
		t.Error("expected nucleus accessory")
	}
	if err := applyTemplateOverrides(appCfg, "", "mybox", 0); err != nil {
		t.Fatalf("host install without domain must validate: %v", err)
	}
}
