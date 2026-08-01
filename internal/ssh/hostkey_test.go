package ssh

import (
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/ssh"
)

func testPublicKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating test key: %v", err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatalf("wrapping test key: %v", err)
	}
	return sshPub
}

type fakeAddr struct{}

func (fakeAddr) Network() string { return "tcp" }
func (fakeAddr) String() string  { return "127.0.0.1:22" }

// CLI-003: a $HOME that can't be resolved used to fall through to
// ssh.InsecureIgnoreHostKey(), silently disabling host-key verification.
func TestDefaultHostKeyCallback_MissingHomeFailsClosed(t *testing.T) {
	t.Setenv("HOME", "")
	// os.UserHomeDir reads $HOME directly on Unix; an empty value makes it error.
	_, err := defaultHostKeyCallback()
	if err == nil {
		t.Fatal("expected an error when $HOME cannot be resolved, got nil (silently accepting any host key)")
	}
}

// CLI-004: a known_hosts append failure used to `return nil`, silently
// accepting the presented key without recording it — every later connection
// then looks like a first connection, so TOFU never detects a key change.
func TestAcceptNewHostKeyCallback_WriteFailureFailsClosed(t *testing.T) {
	// Make the known_hosts parent path unwritable by placing it under a
	// regular file instead of a directory — MkdirAll then fails with ENOTDIR,
	// portable across CI without needing chmod/root tricks.
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0644); err != nil {
		t.Fatal(err)
	}
	knownHostsPath := filepath.Join(blocker, ".ssh", "known_hosts")

	callback := acceptNewHostKeyCallback(knownHostsPath)
	err := callback("example.com:22", fakeAddr{}, testPublicKey(t))
	if err == nil {
		t.Fatal("expected an error when known_hosts can't be written, got nil (silently accepted the key without recording it)")
	}
}

// Sanity check the success path still works: an unknown key against a fresh,
// writable known_hosts is accepted and actually recorded.
func TestAcceptNewHostKeyCallback_WriteSuccessRecordsKey(t *testing.T) {
	dir := t.TempDir()
	knownHostsPath := filepath.Join(dir, ".ssh", "known_hosts")

	callback := acceptNewHostKeyCallback(knownHostsPath)
	if err := callback("example.com:22", fakeAddr{}, testPublicKey(t)); err != nil {
		t.Fatalf("expected the first-time key to be accepted, got: %v", err)
	}
	data, err := os.ReadFile(knownHostsPath)
	if err != nil {
		t.Fatalf("expected known_hosts to be created: %v", err)
	}
	if len(data) == 0 {
		t.Error("expected the accepted key to be recorded in known_hosts, file is empty")
	}
}

var _ = net.Addr(fakeAddr{}) // compile-time interface check
