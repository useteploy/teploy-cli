package ssh

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	gossh "golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func TestKnownHostKeyAlgorithmsOffersOnlyKeysOnFile(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := gossh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "known_hosts")
	line := knownhosts.Line([]string{knownhosts.Normalize("203.0.113.7:22")}, key)
	if err := os.WriteFile(path, []byte(line+"\n"), 0600); err != nil {
		t.Fatal(err)
	}

	if got, want := knownHostKeyAlgorithms(path, "203.0.113.7:22"), []string{gossh.KeyAlgoED25519}; !reflect.DeepEqual(got, want) {
		t.Fatalf("known host: got %v, want %v", got, want)
	}
	if got := knownHostKeyAlgorithms(path, "198.51.100.1:22"); got != nil {
		t.Fatalf("unknown host: got %v, want nil (Go defaults)", got)
	}
	if got := knownHostKeyAlgorithms(filepath.Join(t.TempDir(), "missing"), "203.0.113.7:22"); got != nil {
		t.Fatalf("missing file: got %v, want nil", got)
	}
}
