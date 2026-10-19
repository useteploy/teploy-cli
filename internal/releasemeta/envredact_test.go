package releasemeta

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/docker"
	"github.com/useteploy/teploy/internal/ssh"
)

// Secret values used across these tests; none may ever appear in written
// bytes or reports.
var secretValues = []string{
	"age-store-value", "vault-value", "dotenv-value", "envfile-value",
	"interpolated-value", "named-but-config", "s3cr3t", "p@ss:word",
	"unknown-origin-value",
}

func TestClassifyEnv(t *testing.T) {
	prov := map[string]string{
		"AGE_KEY":     EnvSourceSecretStore,
		"VAULT_KEY":   EnvSourceVault,
		"DOTENV_KEY":  EnvSourcePersistedEnv,
		"FILE_KEY":    EnvSourceEnvFile,
		"INTERP":      EnvSourceInterpolated,
		"LOG_LEVEL":   EnvSourceConfig,
		"MODE":        EnvSourceConfigLiteral,
		"DB_PASSWORD": EnvSourceConfig,
		"DATABASE":    EnvSourceConfig,
		"NUCLEUS_URL": EnvSourceConfig,
	}
	image := map[string]string{"PATH": "/usr/bin", "NODE_ENV": "production"}
	cases := []struct {
		key, value string
		keep       bool
		source     string
	}{
		{"AGE_KEY", "age-store-value", false, EnvSourceSecretStore},
		{"VAULT_KEY", "vault-value", false, EnvSourceVault},
		{"DOTENV_KEY", "dotenv-value", false, EnvSourcePersistedEnv},
		{"FILE_KEY", "envfile-value", false, EnvSourceEnvFile},
		{"INTERP", "interpolated-value", false, EnvSourceInterpolated},
		{"LOG_LEVEL", "debug", true, EnvSourceConfig},
		{"MODE", "literal $value", true, EnvSourceConfigLiteral},
		{"DB_PASSWORD", "named-but-config", false, EnvSourceNamePattern},
		{"DATABASE", "postgres://app:s3cr3t@db:5432/app", false, EnvSourceCredentialURL},
		{"NUCLEUS_URL", "postgresql://u:p@ss:word@nucleus/x", false, EnvSourceCredentialURL},
		{"PATH", "/usr/bin", true, EnvSourceImage},
		{"NODE_ENV", "development", false, EnvSourceUnclassified}, // differs from image default
		{"PORT", "3000", true, EnvSourceRuntime},
		{"OBSERVE_JWT_SECRET", "unknown-origin-value", false, EnvSourceUnclassified},
	}
	for _, c := range cases {
		keep, src := ClassifyEnv(c.key, c.value, prov, image)
		if keep != c.keep || src != c.source {
			t.Errorf("%s: got keep=%v source=%s, want keep=%v source=%s", c.key, keep, src, c.keep, c.source)
		}
	}
	// No provenance and no image env: only PORT survives.
	if keep, _ := ClassifyEnv("LOG_LEVEL", "debug", nil, nil); keep {
		t.Error("value of unknown provenance must not be kept")
	}
}

func TestHasURLCredentials(t *testing.T) {
	for v, want := range map[string]bool{
		"postgres://u:pw@h/db":            true,
		"redis://:pw@h:6379":              true,
		"https://user@host/path":          false, // no password
		"mailto:someone@example.com":      false,
		"amqp://u:p%40ss@h/vhost":         true,
		"weird://u:p@ss/word@h":           true,
		"plain-value":                     false,
		"user:pw@host-without-scheme.com": false,
	} {
		if got := hasURLCredentials(v); got != want {
			t.Errorf("hasURLCredentials(%q) = %v, want %v", v, got, want)
		}
	}
}

func assertNoSecretBytes(t *testing.T, where string, data []byte) {
	t.Helper()
	for _, v := range secretValues {
		if strings.Contains(string(data), v) {
			t.Errorf("%s contains a secret value (%d bytes)", where, len(data))
		}
	}
}

// The writer never emits a secret value, whatever the source.
func TestWrite_NeverPersistsSecretValues(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4", ssh.MockCommand{Match: "mkdir -p /deployments/myapp/meta"})
	rec := &Record{
		App: "myapp", Hash: "v2",
		Env: map[string]string{"LOG_LEVEL": "debug", "AGE_KEY": "age-store-value"},
		Recreate: &docker.RecreateSpec{Name: "myapp-web-v2", Env: []string{
			"AGE_KEY=age-store-value", "VAULT_KEY=vault-value", "DOTENV_KEY=dotenv-value",
			"FILE_KEY=envfile-value", "INTERP=interpolated-value", "DB_PASSWORD=named-but-config",
			"DATABASE=postgres://app:s3cr3t@db/app", "UNKNOWN=unknown-origin-value",
			"LOG_LEVEL=debug", "PATH=/usr/bin", "PORT=3000",
		}},
		EnvProvenance: map[string]string{
			"AGE_KEY": EnvSourceSecretStore, "VAULT_KEY": EnvSourceVault, "DOTENV_KEY": EnvSourcePersistedEnv,
			"FILE_KEY": EnvSourceEnvFile, "INTERP": EnvSourceInterpolated, "DB_PASSWORD": EnvSourceConfig,
			"DATABASE": EnvSourceConfig, "LOG_LEVEL": EnvSourceConfig,
		},
		ImageEnv: map[string]string{"PATH": "/usr/bin"},
	}
	if err := Write(context.Background(), mock, rec); err != nil {
		t.Fatal(err)
	}
	raw := mock.Files["/deployments/myapp/meta/v2.json"]
	if raw == nil {
		t.Fatal("record not written")
	}
	assertNoSecretBytes(t, "written record", raw)

	var back Record
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	gotEnv := strings.Join(back.Recreate.Env, ",")
	if gotEnv != "LOG_LEVEL=debug,PATH=/usr/bin,PORT=3000" {
		t.Errorf("kept recreate env = %s", gotEnv)
	}
	want := map[string]string{
		"AGE_KEY": EnvSourceSecretStore, "VAULT_KEY": EnvSourceVault, "DOTENV_KEY": EnvSourcePersistedEnv,
		"FILE_KEY": EnvSourceEnvFile, "INTERP": EnvSourceInterpolated, "DB_PASSWORD": EnvSourceNamePattern,
		"DATABASE": EnvSourceCredentialURL, "UNKNOWN": EnvSourceUnclassified,
	}
	got := map[string]string{}
	for _, r := range back.Recreate.EnvRedacted {
		got[r.Key] = r.Source
	}
	for k, src := range want {
		if got[k] != src {
			t.Errorf("redacted %s source = %q, want %q", k, got[k], src)
		}
	}
	if back.Env["LOG_LEVEL"] != "debug" || back.Env["AGE_KEY"] != "" || !hasRedacted(back.EnvRedacted, "AGE_KEY") {
		t.Errorf("record env not redacted correctly: env=%v redacted=%+v", keysOf(back.Env), back.EnvRedacted)
	}
	if strings.Contains(string(raw), "env_provenance") || strings.Contains(string(raw), "image_env") {
		t.Error("classification inputs must never be persisted")
	}
}

// An old-shape record (plaintext recreate.env + env) is rewritten with every
// other field preserved, and the rewrite is idempotent.
func TestRedactRaw_PreservesOtherFieldsAndIsIdempotent(t *testing.T) {
	old := `{"schema_version":1,"app":"observe","hash":"v7","future_field":{"a":1},` +
		`"env":{"OBSERVE_ADMIN_PASSWORD":"s3cr3t","PORT":"8080"},` +
		`"recreate":{"name":"observe-web-v7","image_id":"sha256:abc","env":["NUCLEUS_PASSWORD=s3cr3t",` +
		`"OBSERVE_NUCLEUS_URL=postgres://observe:p@ss:word@nucleus/observe","PATH=/usr/bin","PORT=8080","LANG=C"],` +
		`"labels":{"teploy.app":"observe"}}}`
	clean, keys, changed, err := RedactRaw([]byte(old), map[string]string{"PATH": "/usr/bin"})
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	assertNoSecretBytes(t, "redacted record", clean)
	if strings.Join(keys, ",") != "LANG,NUCLEUS_PASSWORD,OBSERVE_ADMIN_PASSWORD,OBSERVE_NUCLEUS_URL" {
		t.Errorf("redacted keys = %v", keys)
	}
	var before, after map[string]any
	_ = json.Unmarshal([]byte(old), &before)
	_ = json.Unmarshal(clean, &after)
	for _, k := range []string{"schema_version", "app", "hash", "future_field"} {
		b, _ := json.Marshal(before[k])
		a, _ := json.Marshal(after[k])
		if string(a) != string(b) {
			t.Errorf("field %s changed: %s -> %s", k, b, a)
		}
	}
	rc := after["recreate"].(map[string]any)
	if rc["name"] != "observe-web-v7" || rc["labels"].(map[string]any)["teploy.app"] != "observe" {
		t.Errorf("recreate spec fields lost: %v", rc)
	}
	if env := rc["env"].([]any); len(env) != 2 || env[0] != "PATH=/usr/bin" || env[1] != "PORT=8080" {
		t.Errorf("kept recreate env = %v", env)
	}
	again, keys2, changed2, err := RedactRaw(clean, map[string]string{"PATH": "/usr/bin"})
	if err != nil || changed2 || len(keys2) != 0 || string(again) != string(clean) {
		t.Errorf("second pass must be a no-op: changed=%v keys=%v err=%v", changed2, keys2, err)
	}
}

func TestScrub_DryRunThenApplyThenIdempotent(t *testing.T) {
	const recPath = "/deployments/observe/meta/v7.json"
	const attPath = "/deployments/observe/meta/att/v7.abcd/env"
	old := `{"schema_version":1,"app":"observe","hash":"v7","recreate":{"name":"observe-web-v7","env":["OBSERVE_JWT_SECRET=s3cr3t","PORT=8080"]}}`
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "for f in /deployments/*/meta/*.json", Output: recPath + "\n"},
		ssh.MockCommand{Match: "for f in /deployments/*/meta/att/*/env", Output: attPath + "\n"},
		ssh.MockCommand{Match: "mkdir /deployments/observe/.lock"},
		ssh.MockCommand{Match: "rm -rf"},
	)
	mock.Files[recPath] = []byte(old)
	mock.Files[attPath] = []byte("OBSERVE_JWT_SECRET=s3cr3t\n")

	dry, err := Scrub(context.Background(), mock, nil, ScrubOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if string(mock.Files[recPath]) != old || mock.Files[attPath] == nil {
		t.Fatal("dry run must not change anything")
	}
	if len(dry.Records) != 1 || dry.Records[0].Rewritten || strings.Join(dry.Records[0].RedactedKeys, ",") != "OBSERVE_JWT_SECRET" {
		t.Errorf("dry run report = %+v", dry.Records)
	}
	report, _ := json.Marshal(dry)
	assertNoSecretBytes(t, "dry-run report", report)

	applied, err := Scrub(context.Background(), mock, nil, ScrubOptions{Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(applied.Records) != 1 || !applied.Records[0].Rewritten || applied.AttemptsRemoved != 1 {
		t.Errorf("apply report = %+v", applied)
	}
	assertNoSecretBytes(t, "scrubbed record", mock.Files[recPath])
	if _, still := mock.Files[attPath]; still {
		t.Error("attempt env file not removed")
	}
	if !strings.Contains(string(mock.Files[recPath]), `"PORT=8080"`) {
		t.Error("non-secret value lost")
	}

	second, err := Scrub(context.Background(), mock, nil, ScrubOptions{Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Records) != 0 {
		t.Errorf("second apply must find nothing: %+v", second.Records)
	}
}

func TestScrub_RejectsBadAppName(t *testing.T) {
	if _, err := Scrub(context.Background(), ssh.NewMockExecutor("h"), nil, ScrubOptions{App: "../etc"}); err == nil {
		t.Error("expected invalid app name to be refused")
	}
}

func TestAttemptEnvFiles(t *testing.T) {
	got := AttemptEnvFiles("myapp", []string{
		"/deployments/myapp/.env",
		"/deployments/myapp/meta/att/v1.ab12/env",
		"/deployments/other/meta/att/v1.ab12/env",
		"/deployments/myapp/meta/att/../../x/env",
		"/deployments/myapp/meta/att/v1.ab12/build/env",
	})
	if strings.Join(got, ",") != "/deployments/myapp/meta/att/v1.ab12/env" {
		t.Errorf("AttemptEnvFiles = %v", got)
	}
}
