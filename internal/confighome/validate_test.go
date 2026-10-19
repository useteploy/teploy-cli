package confighome

import (
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/config"
)

func TestLoadAcceptsPortableConfig(t *testing.T) {
	cfg, err := Load([]byte("app: blog\ndomain: blog.example.com\nimage: nginx:1.27\nenv:\n  LOG_LEVEL: info\n  PRICE: \"$$5\"\n  DB_PASSWORD: secret:DB_PASSWORD\n"), "blog")
	if err != nil {
		t.Fatal(err)
	}
	Unescape(cfg.Env)
	if cfg.Env["PRICE"] != "$5" {
		t.Fatalf("$$ must render as a literal $, got %q", cfg.Env["PRICE"])
	}
}

func TestLoadRefusesNonPortableConfig(t *testing.T) {
	raw := `app: blog
domain: blog.example.com
server: prod
env_files: [.env.prod]
env:
  URL: https://${HOST}/x
  API_TOKEN: abc123def456
tls:
  cert: ./cert.pem
  key: ./key.pem
`
	_, err := Load([]byte(raw), "blog")
	if err == nil {
		t.Fatal("want refusal")
	}
	for _, want := range []string{"env.URL references ${HOST}", "env_files", "server:/servers:", "tls.cert", "env.API_TOKEN looks like a secret", "image: is required"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal misses %q:\n%v", want, err)
		}
	}
}

func TestLoadRefusesLocalStaticBuildAndWrongApp(t *testing.T) {
	_, err := Load([]byte("app: site\ntype: static\ndomain: s.example.com\nsource: dist\nbuild: [\"npm run build\"]\n"), "other")
	if err == nil || !strings.Contains(err.Error(), "build_remote") || !strings.Contains(err.Error(), "source:") || !strings.Contains(err.Error(), `belongs to "other"`) {
		t.Fatalf("got %v", err)
	}
}

func TestPrivilegedChange(t *testing.T) {
	base := &config.AppConfig{App: "a", Image: "x", Volumes: map[string]string{"data": "/data"}}
	same := *base
	same.Image = "y"
	if PrivilegedChange(base, &same) {
		t.Fatal("an image bump is not privileged")
	}
	bind := *base
	bind.Volumes = map[string]string{"data": "/data", "/etc": "/host-etc"}
	if !PrivilegedChange(base, &bind) {
		t.Fatal("adding a host bind is privileged")
	}
	caddy := *base
	caddy.CaddyExtra = "file_server"
	if !PrivilegedChange(base, &caddy) {
		t.Fatal("caddy_extra is privileged")
	}
	pub := *base
	pub.Publish = []string{"0.0.0.0:22:22"}
	if !PrivilegedChange(base, &pub) {
		t.Fatal("publish is privileged")
	}
}

func TestLoadKeepsShellVarsOutsideEnv(t *testing.T) {
	raw := "app: blog\ndomain: blog.example.com\nimage: nginx:1\nprocesses:\n  web: sh -c 'exec server --root $HOME'\nhooks:\n  pre_deploy: echo $APP_VERSION\n"
	if _, err := Load([]byte(raw), "blog"); err != nil {
		t.Fatalf("$VAR outside env: is never expanded and must be accepted: %v", err)
	}
}

func TestMaskSourceHidesDetectedSecrets(t *testing.T) {
	raw := []byte("# blog\napp: blog\nserver: prod\nenv:\n  LOG_LEVEL: info\n  API_TOKEN: abc123\n  DATABASE_URL: postgres://u:pw@db/blog\naccessories:\n  db:\n    image: postgres:16\n    env:\n      POSTGRES_PASSWORD: hunter2\naudit:\n  token: t0k3n\n")
	out, err := MaskSource(raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, leaked := range []string{"abc123", "u:pw@", "hunter2", "t0k3n"} {
		if strings.Contains(string(out), leaked) {
			t.Fatalf("%q survived masking:\n%s", leaked, out)
		}
	}
	if !strings.Contains(string(out), "LOG_LEVEL: info") || !strings.HasPrefix(string(out), "# blog\n") {
		t.Fatalf("plain values or comments lost:\n%s", out)
	}
}
