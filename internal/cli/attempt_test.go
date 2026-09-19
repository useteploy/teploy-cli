package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/releasemeta"
	"github.com/useteploy/teploy/internal/ssh"
)

// TestUploadAppTLS_AttemptScoped proves F08's TLS artifact rule: a deploy's
// cert/key land in the attempt's own directory (never the shared legacy
// path), written atomically, and the returned paths are the CONTAINER-side
// ones the Caddyfile references.
func TestUploadAppTLS_AttemptScoped(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certPath, []byte("CERT"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte("KEY"), 0600); err != nil {
		t.Fatal(err)
	}
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "mkdir -p ", Output: ""},
		ssh.MockCommand{Match: "mv -f -- ", Output: ""},
	)
	att := releasemeta.MustAttempt("myapp", "abc123")
	cert, key, err := uploadAppTLS(context.Background(), mock, "myapp", &config.TLSConfig{Cert: certPath, Key: keyPath}, &att)
	if err != nil {
		t.Fatalf("uploadAppTLS: %v", err)
	}
	if want := "/etc/caddy/tls/att/myapp/" + att.Name() + "/myapp.crt"; cert != want {
		t.Errorf("cert container path: got %s want %s", cert, want)
	}
	if want := "/etc/caddy/tls/att/myapp/" + att.Name() + "/myapp.key"; key != want {
		t.Errorf("key container path: got %s want %s", key, want)
	}
	hostCert := att.TLSDir() + "/myapp.crt"
	data, ok := mock.Files[hostCert]
	if !ok || string(data) != "CERT" {
		t.Errorf("cert not uploaded to the attempt dir %s (found=%v)", hostCert, ok)
	}
	if _, legacy := mock.Files["/deployments/caddy/tls/myapp.crt"]; legacy {
		t.Error("attempt-scoped upload must not write the legacy shared cert path")
	}
	for _, c := range mock.Calls {
		if strings.HasPrefix(c, "UPLOAD:") && strings.Contains(c, ".key") && strings.Contains(c, "0600") == false {
			t.Errorf("key upload mode: %s", c)
		}
	}
}

// TestUploadAppTLS_LegacyFallbackForRollback: the rollback CLI passes a nil
// attempt (target hash unknown before the record is read); that path keeps
// the pre-F08 shared layout, which pre-F14 records still reference.
func TestUploadAppTLS_LegacyFallbackForRollback(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certPath, []byte("CERT"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte("KEY"), 0600); err != nil {
		t.Fatal(err)
	}
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "mkdir -p /deployments/caddy/tls", Output: ""},
		ssh.MockCommand{Match: "mv -f -- ", Output: ""},
	)
	cert, key, err := uploadAppTLS(context.Background(), mock, "myapp", &config.TLSConfig{Cert: certPath, Key: keyPath}, nil)
	if err != nil {
		t.Fatalf("uploadAppTLS legacy: %v", err)
	}
	if cert != "/etc/caddy/tls/myapp.crt" || key != "/etc/caddy/tls/myapp.key" {
		t.Errorf("legacy paths: got %s / %s", cert, key)
	}
}
