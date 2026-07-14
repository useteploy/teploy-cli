package openbao

import (
	"encoding/base64"
	"regexp"
	"strings"
	"testing"
)

func TestRenderServerConfig_Static(t *testing.T) {
	got := RenderServerConfig(ServerConfig{
		StoragePath: "/openbao/data",
		ListenAddr:  "0.0.0.0:8200",
		TLSDisable:  true,
		APIAddr:     "http://0.0.0.0:8200",
		Seal:        SealSpec{Type: SealStatic, KeyID: "abc-123"},
	})
	for _, want := range []string{
		`storage "file" {`,
		`path = "/openbao/data"`,
		`listener "tcp" {`,
		`tls_disable = true`,
		`seal "static" {`,
		`current_key_id = "abc-123"`,
		`current_key    = "env://BAO_SEAL_KEY"`,
		`disable_mlock = true`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestRenderServerConfig_KMSAndTransit(t *testing.T) {
	kms := RenderServerConfig(ServerConfig{Seal: SealSpec{Type: SealAWSKMS, KMSKeyID: "k1", KMSRegion: "us-east-1"}})
	if !strings.Contains(kms, `seal "awskms" {`) || !strings.Contains(kms, `kms_key_id = "k1"`) {
		t.Errorf("awskms seal not rendered:\n%s", kms)
	}
	tr := RenderServerConfig(ServerConfig{Seal: SealSpec{Type: SealTransit, TransitAddress: "http://u:8200", TransitKeyName: "unseal", TransitMount: "transit/"}})
	if !strings.Contains(tr, `seal "transit" {`) || !strings.Contains(tr, `key_name  = "unseal"`) {
		t.Errorf("transit seal not rendered:\n%s", tr)
	}
}

func TestRenderServerConfig_TLS(t *testing.T) {
	got := RenderServerConfig(ServerConfig{TLSDisable: false, TLSCertFile: "/c.pem", TLSKeyFile: "/k.pem", Seal: SealSpec{Type: SealStatic}})
	if strings.Contains(got, "tls_disable") {
		t.Errorf("TLS enabled config must not disable tls:\n%s", got)
	}
	if !strings.Contains(got, `tls_cert_file = "/c.pem"`) || !strings.Contains(got, `tls_key_file  = "/k.pem"`) {
		t.Errorf("tls cert/key missing:\n%s", got)
	}
}

func TestGenerateSealKey(t *testing.T) {
	k, err := GenerateSealKey()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.StdEncoding.DecodeString(k)
	if err != nil {
		t.Fatalf("seal key not valid base64: %v", err)
	}
	if len(raw) != 32 {
		t.Errorf("seal key must be 32 bytes (AES-256), got %d", len(raw))
	}
	if k2, _ := GenerateSealKey(); k2 == k {
		t.Error("seal keys must be random")
	}
}

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestGenerateKeyID(t *testing.T) {
	id, err := GenerateKeyID()
	if err != nil {
		t.Fatal(err)
	}
	if !uuidRe.MatchString(id) {
		t.Errorf("key id is not a v4 UUID: %q", id)
	}
}
