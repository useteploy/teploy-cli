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
