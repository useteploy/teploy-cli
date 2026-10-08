package caddy

import (
	"context"
	"strings"
	"testing"
)

func TestCampaignStampedPolicyAndWebhook(t *testing.T) {
	block := "# TEPLOY GENERATION 7\nexample.test {\n tls internal\n reverse_proxy app:3000\n}\n"
	policy, err := ExtractPolicy(block)
	if err != nil || !strings.Contains(policy.TLS, "internal") {
		t.Fatalf("policy %+v %v", policy, err)
	}
	cfg := &webhookRouteConfig{}
	got, err := injectWebhookFragment(block, "app", cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, "# TEPLOY GENERATION 7\nexample.test {\n") {
		t.Fatalf("metadata lost: %s", got)
	}
	if _, err = ExtractPolicy(got); err != nil {
		t.Fatal(err)
	}
}

func TestCampaignStampedMaintenanceOnOff(t *testing.T) {
	prior := "# TEPLOY BEGIN demo\n# TEPLOY GENERATION 7\nexample.test {\n tls internal\n reverse_proxy app:3000\n}\n# TEPLOY END demo\n"
	m, _ := caddyMutateMocks(prior)
	c := NewClient(m)
	if err := c.SetMaintenance(context.Background(), "demo", "example.test"); err != nil {
		t.Fatal(err)
	}
	b := string(m.Files[caddyfilePath])
	if !strings.Contains(b, "# TEPLOY GENERATION 7") {
		t.Fatalf("generation lost %s", b)
	}
	if err := c.RemoveMaintenance(context.Background(), "demo"); err != nil {
		t.Fatal(err)
	}
	b = string(m.Files[caddyfilePath])
	if !strings.Contains(b, "# TEPLOY GENERATION 7") || !strings.Contains(b, "reverse_proxy app:3000") {
		t.Fatalf("restore lost identity %s", b)
	}
}
