package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadApp_NotificationChannelUnknownTypeRejected(t *testing.T) {
	dir := t.TempDir()
	content := `app: myapp
domain: myapp.com
notifications:
  channels:
    - type: discord
      url: https://discord.com/api/webhooks/xxx
`
	if err := os.WriteFile(filepath.Join(dir, "teploy.yml"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadApp(dir)
	if err == nil {
		t.Fatal("expected error for unrecognized notification channel type")
	}
	if !strings.Contains(err.Error(), "type") {
		t.Errorf("error should mention the type field, got: %v", err)
	}
}

func TestSigningSecret_ConfigWinsOverEnv(t *testing.T) {
	t.Setenv(WebhookSecretEnv, "from-env")
	n := NotificationsConfig{Secret: "from-config"}
	if got := n.SigningSecret(); got != "from-config" {
		t.Errorf("SigningSecret() = %q, want the configured value", got)
	}
}

func TestSigningSecret_FallsBackToEnv(t *testing.T) {
	t.Setenv(WebhookSecretEnv, "from-env")
	if got := (NotificationsConfig{}).SigningSecret(); got != "from-env" {
		t.Errorf("SigningSecret() = %q, want the env value", got)
	}
}

func TestSigningSecret_EmptyWhenNeitherSet(t *testing.T) {
	t.Setenv(WebhookSecretEnv, "")
	// Unsigned is the behavior of every install that has not opted in; a
	// non-empty default here would start signing deliveries nobody verifies.
	if got := (NotificationsConfig{}).SigningSecret(); got != "" {
		t.Errorf("SigningSecret() = %q, want empty", got)
	}
}

func TestChannelSigningSecret_ChannelOverridesThenInherits(t *testing.T) {
	t.Setenv(WebhookSecretEnv, "from-env")
	n := NotificationsConfig{Secret: "top-level"}

	own := NotificationChannelConfig{Type: "webhook", Secret: "channel-own"}
	if got := n.ChannelSigningSecret(own); got != "channel-own" {
		t.Errorf("ChannelSigningSecret(own) = %q, want the channel's own secret", got)
	}

	bare := NotificationChannelConfig{Type: "webhook"}
	if got := n.ChannelSigningSecret(bare); got != "top-level" {
		t.Errorf("ChannelSigningSecret(bare) = %q, want the top-level secret", got)
	}

	if got := (NotificationsConfig{}).ChannelSigningSecret(bare); got != "from-env" {
		t.Errorf("ChannelSigningSecret with no config = %q, want the env value", got)
	}
}

func TestLoadApp_NotificationSecretParsed(t *testing.T) {
	dir := t.TempDir()
	content := `app: myapp
domain: myapp.com
notifications:
  webhook: https://example.com/hook
  secret: yaml-secret
  channels:
    - type: webhook
      url: https://example.com/other
      secret: channel-secret
`
	if err := os.WriteFile(filepath.Join(dir, "teploy.yml"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadApp(dir)
	if err != nil {
		t.Fatalf("LoadApp: %v", err)
	}
	if cfg.Notifications.Secret != "yaml-secret" {
		t.Errorf("Notifications.Secret = %q, want yaml-secret", cfg.Notifications.Secret)
	}
	if len(cfg.Notifications.Channels) != 1 || cfg.Notifications.Channels[0].Secret != "channel-secret" {
		t.Errorf("channel secret not parsed: %+v", cfg.Notifications.Channels)
	}
}

func TestLoadApp_NotificationChannelSlackAccepted(t *testing.T) {
	dir := t.TempDir()
	content := `app: myapp
domain: myapp.com
notifications:
  channels:
    - type: slack
      url: https://hooks.slack.com/services/xxx
`
	if err := os.WriteFile(filepath.Join(dir, "teploy.yml"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadApp(dir)
	if err != nil {
		t.Fatalf("LoadApp: %v", err)
	}
	if len(cfg.Notifications.Channels) != 1 || cfg.Notifications.Channels[0].Type != "slack" {
		t.Errorf("Notifications.Channels = %+v, want one slack channel", cfg.Notifications.Channels)
	}
}
