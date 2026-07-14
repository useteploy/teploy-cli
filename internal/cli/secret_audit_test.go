package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGenerateSecretAuditService(t *testing.T) {
	s := generateSecretAuditService("myapp")
	for _, want := range []string{
		"Type=oneshot",
		"Requires=docker.service",
		"secret audit ship --local --app myapp",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("service missing %q:\n%s", want, s)
		}
	}
}

func TestGenerateSecretAuditTimer(t *testing.T) {
	s := generateSecretAuditTimer("myapp", 300)
	for _, want := range []string{
		"OnUnitActiveSec=300s",
		"WantedBy=timers.target",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("timer missing %q:\n%s", want, s)
		}
	}
}

func TestReadSecretAuditConf(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "conf")
	os.WriteFile(p, []byte("endpoint=https://o.example.com\ntoken=abc\nsite=default\naccessory=openbao\n"), 0600)
	conf, err := readSecretAuditConf(p)
	if err != nil {
		t.Fatal(err)
	}
	if conf["endpoint"] != "https://o.example.com" || conf["token"] != "abc" || conf["accessory"] != "openbao" {
		t.Errorf("unexpected conf: %v", conf)
	}
	// accessory defaults when absent.
	os.WriteFile(p, []byte("endpoint=x\n"), 0600)
	conf, _ = readSecretAuditConf(p)
	if conf["accessory"] != "openbao" {
		t.Errorf("accessory should default to openbao, got %q", conf["accessory"])
	}
}
