package cli

import (
	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/docker"
	"testing"
)

func TestLogsRejectsSelectorBeforeResolution(t *testing.T) {
	if err := runLogs(&Flags{}, "demo", "web;touch /tmp/marker", 50, false); err == nil {
		t.Fatal("unsafe selector accepted")
	}
}
func TestFleetEndpointSeparatesManagementPort(t *testing.T) {
	for _, test := range []struct{ endpoint, want string }{{"192.0.2.10:2222", "192.0.2.10:8080"}, {"[2001:db8::1]:2222", "[2001:db8::1]:8080"}, {"2001:db8::1", "[2001:db8::1]:8080"}} {
		if got := fleetServiceAddress(test.endpoint, &config.AppConfig{Ingress: "host", Port: 8080}); got != test.want {
			t.Fatalf("%s => %s", test.endpoint, got)
		}
	}
}
func TestAccessoriesAndPreviewsAreNotDeployedWorkload(t *testing.T) {
	inventory := []docker.Container{{Labels: map[string]string{"teploy.role": "accessory", "teploy.version": "v1"}}, {Labels: map[string]string{"teploy.process": "preview-main", "teploy.version": "v1"}}}
	if hasDeployedWorkload(inventory) {
		t.Fatal("accessory-only inventory hides missing application")
	}
}
