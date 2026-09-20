package caddy

import (
	"context"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/ssh"
)

func webhookTestMocks() *ssh.MockExecutor {
	return ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "mkdir /deployments/caddy/.lock", Output: ""},
		ssh.MockCommand{Match: "cat /deployments/caddy/Caddyfile", Output: "{\n\tadmin 0.0.0.0:2019\n}\n"},
		ssh.MockCommand{Match: "a=$(docker exec caddy md5sum", Output: "TEPLOY_CADDY_OK"},
		ssh.MockCommand{Match: "docker exec caddy caddy reload", Output: ""},
		ssh.MockCommand{Match: "rmdir /deployments/caddy/.lock", Output: ""},
	)
}

// TestWebhookRoute_SurvivesRedeploy is the T26 core regression: the webhook
// route used to exist only in Caddy's runtime API, so the next ordinary
// deploy's Caddyfile regeneration + reload erased it. The persisted
// fragment must survive every managed re-render.
func TestWebhookRoute_SurvivesRedeploy(t *testing.T) {
	ctx := context.Background()
	mock := webhookTestMocks()
	c := NewClient(mock)

	if err := c.SetWebhookRoute(ctx, "myapp", "myapp.com", 9876); err != nil {
		t.Fatalf("SetWebhookRoute: %v", err)
	}
	// No managed block yet: nothing to attach to; the descriptor persists.
	if strings.Contains(string(mock.Files["/deployments/caddy/Caddyfile"]), "teploy-webhook") {
		t.Fatal("fragment attached without a managed site block")
	}

	// First deploy renders the app block — the fragment attaches.
	if err := c.SetRoute(ctx, "myapp", "myapp.com", "myapp-web-v1", 3000, TLS{}, "", nil, Firewall{}, Access{}); err != nil {
		t.Fatalf("SetRoute: %v", err)
	}
	file := string(mock.Files["/deployments/caddy/Caddyfile"])
	if !strings.Contains(file, "path /teploy-webhook/myapp") || !strings.Contains(file, "host.docker.internal:9876") {
		t.Fatalf("webhook fragment not attached to the app block:\n%s", file)
	}

	// A REDEPLOY re-renders the block — the fragment must survive (T26).
	if err := c.SetRoute(ctx, "myapp", "myapp.com", "myapp-web-v2", 3000, TLS{}, "", nil, Firewall{}, Access{}); err != nil {
		t.Fatalf("SetRoute v2: %v", err)
	}
	file = string(mock.Files["/deployments/caddy/Caddyfile"])
	if !strings.Contains(file, "path /teploy-webhook/myapp") {
		t.Fatalf("webhook fragment erased by a redeploy:\n%s", file)
	}
	if !strings.Contains(file, "myapp-web-v2:3000") || strings.Contains(file, "myapp-web-v1") {
		t.Fatalf("route did not move to v2:\n%s", file)
	}
}

// TestWebhookRoute_PortAndDomainsHonored is the T27 regression: the runtime
// injector hardcoded port 9876 and inserted a comma-separated domain list
// as ONE host value.
func TestWebhookRoute_PortAndDomainsHonored(t *testing.T) {
	ctx := context.Background()
	mock := webhookTestMocks()
	c := NewClient(mock)
	if err := c.SetRoute(ctx, "myapp", "myapp.com, www.myapp.com", "myapp-web-v1", 3000, TLS{}, "", nil, Firewall{}, Access{}); err != nil {
		t.Fatalf("SetRoute: %v", err)
	}
	if err := c.SetWebhookRoute(ctx, "myapp", "myapp.com, www.myapp.com", 9911); err != nil {
		t.Fatalf("SetWebhookRoute: %v", err)
	}
	file := string(mock.Files["/deployments/caddy/Caddyfile"])
	if !strings.Contains(file, "host.docker.internal:9911") {
		t.Errorf("configured port 9911 not honored:\n%s", file)
	}
	if strings.Contains(file, "host.docker.internal:9876") {
		t.Errorf("hardcoded default port leaked:\n%s", file)
	}
	if !strings.Contains(file, "myapp.com, www.myapp.com {") {
		t.Errorf("multi-domain site address not rendered as before:\n%s", file)
	}
}

// TestWebhookRoute_RemovalStripsFragment: RemoveWebhookRoute deletes the
// descriptor and strips the fragment in one transaction.
func TestWebhookRoute_RemovalStripsFragment(t *testing.T) {
	ctx := context.Background()
	mock := webhookTestMocks()
	c := NewClient(mock)
	if err := c.SetRoute(ctx, "myapp", "myapp.com", "myapp-web-v1", 3000, TLS{}, "", nil, Firewall{}, Access{}); err != nil {
		t.Fatalf("SetRoute: %v", err)
	}
	if err := c.SetWebhookRoute(ctx, "myapp", "myapp.com", 9876); err != nil {
		t.Fatalf("SetWebhookRoute: %v", err)
	}
	if err := c.RemoveWebhookRoute(ctx, "myapp"); err != nil {
		t.Fatalf("RemoveWebhookRoute: %v", err)
	}
	file := string(mock.Files["/deployments/caddy/Caddyfile"])
	if strings.Contains(file, "teploy-webhook") {
		t.Errorf("fragment not stripped:\n%s", file)
	}
	if _, ok := mock.Files["/deployments/myapp/.webhook-route"]; ok {
		t.Error("descriptor not deleted")
	}
	if !strings.Contains(file, "myapp-web-v1:3000") {
		t.Errorf("the app's own route must survive webhook removal:\n%s", file)
	}
}
