package cli

import (
	"testing"

	"github.com/useteploy/teploy/internal/openbao"
)

func TestBuildSealSpec_Static(t *testing.T) {
	for _, s := range []string{"", "static"} {
		spec, env, err := buildSealSpec(s, sealFlags{})
		if err != nil {
			t.Fatalf("static seal %q: %v", s, err)
		}
		if spec.Type != openbao.SealStatic || env != nil {
			t.Errorf("static seal should have no env: %+v %v", spec, env)
		}
	}
}

func TestBuildSealSpec_AWSKMS(t *testing.T) {
	if _, _, err := buildSealSpec("awskms", sealFlags{kmsKeyID: "k"}); err == nil {
		t.Error("awskms without region should error")
	}
	spec, env, err := buildSealSpec("awskms", sealFlags{kmsKeyID: "k1", kmsRegion: "us-east-1", kmsAccessKey: "AK", kmsSecretKey: "SK"})
	if err != nil {
		t.Fatal(err)
	}
	if spec.Type != openbao.SealAWSKMS || spec.KMSKeyID != "k1" || spec.KMSRegion != "us-east-1" {
		t.Errorf("unexpected spec: %+v", spec)
	}
	// Secret creds must be env-only (never in the config/spec output).
	if env["AWS_ACCESS_KEY_ID"] != "AK" || env["AWS_SECRET_ACCESS_KEY"] != "SK" || env["AWS_REGION"] != "us-east-1" {
		t.Errorf("aws creds not in seal env: %v", env)
	}
	// Without explicit creds, only region is set (rely on IAM role).
	_, env2, _ := buildSealSpec("awskms", sealFlags{kmsKeyID: "k", kmsRegion: "r"})
	if _, ok := env2["AWS_ACCESS_KEY_ID"]; ok {
		t.Error("no access key should be set when relying on IAM role")
	}
}

func TestBuildSealSpec_Transit(t *testing.T) {
	if _, _, err := buildSealSpec("transit", sealFlags{transitAddr: "http://u:8200"}); err == nil {
		t.Error("transit without token/key should error")
	}
	spec, env, err := buildSealSpec("transit", sealFlags{transitAddr: "http://u:8200", transitToken: "tok", transitKey: "unseal", transitMount: "transit/"})
	if err != nil {
		t.Fatal(err)
	}
	if spec.Type != openbao.SealTransit || spec.TransitKeyName != "unseal" {
		t.Errorf("unexpected spec: %+v", spec)
	}
	// The token rides VAULT_TOKEN env (verified against OpenBao docs), not config.
	if env["VAULT_TOKEN"] != "tok" {
		t.Errorf("transit token must be in VAULT_TOKEN env: %v", env)
	}
}

func TestBuildSealSpec_Unknown(t *testing.T) {
	if _, _, err := buildSealSpec("bogus", sealFlags{}); err == nil {
		t.Error("unknown seal should error")
	}
}
