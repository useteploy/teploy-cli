package cli

import (
	"testing"

	"github.com/useteploy/teploy/internal/config"
)

// TestDeployConfigFromAppPropagatesContainerPort is the positive
// config→deploy seam check for Compose port preservation (C05): the
// application port declared in the imported config must arrive in the
// deploy engine's ContainerPort unchanged. This hop is what stayed
// unobservable while the Compose importer dropped the port entirely.
func TestDeployConfigFromAppPropagatesContainerPort(t *testing.T) {
	appCfg := &config.AppConfig{App: "app", Domain: "app.example.com", Port: 3000}
	got := deployConfigFromApp(appCfg, "example/web:v1", "v1", nil, nil, "", "", false, nil, "")
	if got.ContainerPort != 3000 {
		t.Errorf("deploy.Config.ContainerPort = %d, want 3000 (the imported application port)", got.ContainerPort)
	}
	if got.Image != "example/web:v1" {
		t.Errorf("deploy.Config.Image = %q, want example/web:v1", got.Image)
	}

	// A config with no declared port (a teploy.yml app relying on the
	// default) must flow through as 0 — normalization to 80 belongs to
	// the deploy engine, not this mapping.
	appCfg.Port = 0
	got = deployConfigFromApp(appCfg, "example/web:v1", "v1", nil, nil, "", "", false, nil, "")
	if got.ContainerPort != 0 {
		t.Errorf("deploy.Config.ContainerPort = %d, want 0 (deploy normalizes the default)", got.ContainerPort)
	}
}
