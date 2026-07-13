package config

import (
	"strings"
	"testing"
)

func TestLoadApp_FirewallParses(t *testing.T) {
	cfg, err := writeAndLoad(t, `app: myapp
domain: app.example.com
firewall:
  allow_ips:
    - 10.0.0.0/8
    - 1.2.3.4
  deny_ips:
    - 9.9.9.9
  block_user_agents:
    - badbot
  max_body_size: 10MB
`)
	if err != nil {
		t.Fatalf("valid firewall should load: %v", err)
	}
	if len(cfg.Firewall.AllowIPs) != 2 || cfg.Firewall.MaxBodySize != "10MB" {
		t.Errorf("firewall not parsed: %+v", cfg.Firewall)
	}
	if cfg.Firewall.IsZero() {
		t.Error("firewall should not be zero")
	}
}

func TestLoadApp_FirewallBadIPRejected(t *testing.T) {
	_, err := writeAndLoad(t, `app: myapp
domain: app.example.com
firewall:
  deny_ips:
    - not-an-ip
`)
	if err == nil || !strings.Contains(err.Error(), "valid IP") {
		t.Fatalf("expected invalid-IP error, got: %v", err)
	}
}

func TestLoadApp_FirewallBadBodySizeRejected(t *testing.T) {
	_, err := writeAndLoad(t, `app: myapp
domain: app.example.com
firewall:
  max_body_size: 10 potatoes
`)
	if err == nil || !strings.Contains(err.Error(), "valid size") {
		t.Fatalf("expected invalid-size error, got: %v", err)
	}
}

func TestLoadApp_FirewallUserAgentInjectionRejected(t *testing.T) {
	_, err := writeAndLoad(t, `app: myapp
domain: app.example.com
firewall:
  block_user_agents:
    - "bad}\nexample.com {"
`)
	if err == nil || !strings.Contains(err.Error(), "not allowed in a matcher") {
		t.Fatalf("expected UA injection to be rejected, got: %v", err)
	}
}

func TestLoadApp_FirewallOnStaticRejected(t *testing.T) {
	_, err := writeAndLoad(t, `app: myapp
domain: app.example.com
type: static
source: dist
firewall:
  deny_ips:
    - 9.9.9.9
`)
	if err == nil || !strings.Contains(err.Error(), "type:static") {
		t.Fatalf("expected firewall-on-static rejection, got: %v", err)
	}
}

func TestLoadApp_FirewallOnExternalIngressRejected(t *testing.T) {
	_, err := writeAndLoad(t, `app: myapp
domain: app.example.com
ingress: external
firewall:
  deny_ips:
    - 9.9.9.9
`)
	if err == nil || !strings.Contains(err.Error(), "requires Caddy ingress") {
		t.Fatalf("expected firewall-on-external rejection, got: %v", err)
	}
}
