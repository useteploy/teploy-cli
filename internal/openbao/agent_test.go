package openbao

import (
	"strings"
	"testing"
)

func TestRenderAgentConfig(t *testing.T) {
	got := RenderAgentConfig(AgentConfig{
		VaultAddr:    "http://myapp-openbao:8200",
		RoleIDPath:   "/agent/role_id",
		SecretIDPath: "/agent/secret_id",
		TokenSink:    "/vault/secrets/.vault-token",
		Templates:    []AgentTemplate{{Contents: "X={{ .Data.foo }}", Destination: "/vault/secrets/db.env"}},
	})
	for _, want := range []string{
		`address = "http://myapp-openbao:8200"`,
		`auto_auth {`,
		`method "approle" {`,
		`role_id_file_path                   = "/agent/role_id"`,
		`secret_id_file_path                 = "/agent/secret_id"`,
		`sink "file" {`,
		`path = "/vault/secrets/.vault-token"`,
		`template {`,
		`destination = "/vault/secrets/db.env"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("agent config missing %q in:\n%s", want, got)
		}
	}
}

func TestDBEnvTemplate(t *testing.T) {
	tmpl := DBEnvTemplate("myapp")
	if !strings.Contains(tmpl.Contents, `secret "database/creds/myapp-role"`) {
		t.Errorf("template should read the app's dynamic role: %s", tmpl.Contents)
	}
	if !strings.Contains(tmpl.Contents, "DB_USER={{ .Data.username }}") || !strings.Contains(tmpl.Contents, "DB_PASS={{ .Data.password }}") {
		t.Errorf("template should emit DB_USER/DB_PASS: %s", tmpl.Contents)
	}
	if tmpl.Destination != "/vault/secrets/db.env" {
		t.Errorf("unexpected destination: %s", tmpl.Destination)
	}
}

func TestAppReadPolicy(t *testing.T) {
	noDB := AppReadPolicy("myapp", false)
	if !strings.Contains(noDB, `path "secret/data/myapp/*" { capabilities = ["read"] }`) {
		t.Errorf("policy missing KV read: %s", noDB)
	}
	if strings.Contains(noDB, "database/creds") {
		t.Errorf("no-DB policy must not grant db creds: %s", noDB)
	}
	withDB := AppReadPolicy("myapp", true)
	if !strings.Contains(withDB, `path "database/creds/myapp-role" { capabilities = ["read"] }`) {
		t.Errorf("withDB policy must grant db creds read: %s", withDB)
	}
}

func TestSecretsVolumeAndMount(t *testing.T) {
	if SecretsVolume("myapp") != "myapp-vault-secrets" {
		t.Errorf("unexpected volume name: %s", SecretsVolume("myapp"))
	}
	if AgentMountPath != "/vault/secrets" {
		t.Errorf("unexpected mount path: %s", AgentMountPath)
	}
}
