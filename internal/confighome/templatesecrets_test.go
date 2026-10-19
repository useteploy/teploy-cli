package confighome

import (
	"strings"
	"testing"
)

func TestExtractSecretsKeepsValuesOutOfTheConfig(t *testing.T) {
	rendered := []byte(`app: wiki
domain: wiki.example.com
image: wiki:2
env:
  DATABASE_URL: postgres://wiki:Gen3ratedPw@wiki-db:5432/wiki
  LOG_LEVEL: info
accessories:
  db:
    image: postgres:16
    env:
      POSTGRES_PASSWORD: Gen3ratedPw
      POSTGRES_USER: wiki
`)
	out, moved, err := ExtractSecrets(rendered, []string{"Gen3ratedPw"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "Gen3ratedPw") {
		t.Fatalf("secret left in the config:\n%s", out)
	}
	if moved["DATABASE_URL"] != "postgres://wiki:Gen3ratedPw@wiki-db:5432/wiki" || moved["POSTGRES_PASSWORD"] != "Gen3ratedPw" {
		t.Fatalf("moved = %v", moved)
	}
	if !strings.Contains(string(out), "POSTGRES_PASSWORD: secret:POSTGRES_PASSWORD") || strings.Contains(string(out), "DATABASE_URL") || !strings.Contains(string(out), "LOG_LEVEL: info") {
		t.Fatalf("config after extraction:\n%s", out)
	}
	if _, err := Load(out, "wiki"); err != nil {
		t.Fatalf("the cleaned template must be storable in Teploy: %v", err)
	}
}

func TestExtractSecretsRefusesSecretsOutsideEnv(t *testing.T) {
	_, _, err := ExtractSecrets([]byte("app: wiki\ndomain: w.example.com\nimage: wiki:2\nprocesses:\n  web: serve --password Gen3ratedPw\n"), []string{"Gen3ratedPw"})
	if err == nil || !strings.Contains(err.Error(), "outside env:") {
		t.Fatalf("got %v", err)
	}
}

// Rendered templates carry their literal values as env_literal (never
// expanded); a generated secret there must leave the config too.
func TestExtractSecretsFromRenderedLiteralEnv(t *testing.T) {
	rendered := []byte("app: pa\ndomain: pa.example.com\nimage: adminer:4\nenv_literal:\n  ADMINER_DEFAULT_SERVER: pa-db\naccessories:\n  db:\n    image: postgres:16\n    env_literal:\n      POSTGRES_PASSWORD: Gen3ratedPw\n      POSTGRES_DB: pa\n")
	out, moved, err := ExtractSecrets(rendered, []string{"Gen3ratedPw"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "Gen3ratedPw") || moved["POSTGRES_PASSWORD"] != "Gen3ratedPw" || !strings.Contains(string(out), "POSTGRES_PASSWORD: secret:POSTGRES_PASSWORD") || !strings.Contains(string(out), "POSTGRES_DB: pa") {
		t.Fatalf("moved=%v\n%s", moved, out)
	}
	if _, err := Load(out, "pa"); err != nil {
		t.Fatal(err)
	}
}
