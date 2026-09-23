package ssh

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
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

// 2026-09-22 live finding (ship S14 trusted-copy provisioning): a
// known_hosts carrying only the host's ed25519 line made every connection
// presenting a different algorithm fail with a bare "knownhosts: key
// mismatch" — the error named neither the algorithm presented nor the ones
// on file, so an algorithm-coverage gap read as a MITM alarm. Both callback
// paths must name the host, the presented algorithm, the on-file algorithms
// and the scan-without--t remediation, while still failing closed (and
// remaining classifiable as *knownhosts.KeyError by callers).
func TestHostKeyMismatchNamesAlgorithms(t *testing.T) {
	dir := t.TempDir()
	sshDir := filepath.Join(dir, ".ssh")
	if err := os.MkdirAll(sshDir, 0700); err != nil {
		t.Fatal(err)
	}
	knownHostsPath := filepath.Join(sshDir, "known_hosts")

	enrolled := testPublicKey(t)
	line := knownhosts.Line([]string{knownhosts.Normalize("203.0.113.7:22")}, enrolled)
	if err := os.WriteFile(knownHostsPath, []byte(line+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(knownHostsPath)
	if err != nil {
		t.Fatal(err)
	}

	rsaPriv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating rsa test key: %v", err)
	}
	presented, err := ssh.NewPublicKey(&rsaPriv.PublicKey)
	if err != nil {
		t.Fatalf("wrapping rsa test key: %v", err)
	}

	assertEnriched := func(name string, err error) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s: expected a mismatch error, got nil", name)
		}
		for _, want := range []string{
			"203.0.113.7:22",
			"ssh-rsa",
			"ssh-ed25519",
			"ssh-keyscan without -t",
		} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: error does not name %q:\n%s", name, want, err)
			}
		}
		var keyErr *knownhosts.KeyError
		if !errors.As(err, &keyErr) {
			t.Errorf("%s: wrapped error no longer unwraps to *knownhosts.KeyError: %v", name, err)
		}
	}

	t.Setenv("HOME", dir)
	strict, err := defaultHostKeyCallback()
	if err != nil {
		t.Fatalf("defaultHostKeyCallback: %v", err)
	}
	assertEnriched("defaultHostKeyCallback", strict("203.0.113.7:22", fakeAddr{}, presented))

	acceptNew := acceptNewHostKeyCallback(knownHostsPath)
	assertEnriched("acceptNewHostKeyCallback", acceptNew("203.0.113.7:22", fakeAddr{}, presented))

	// Failing closed is unchanged: the mismatch must not enroll the presented
	// key (accept-new) or alter known_hosts in any way.
	after, err := os.ReadFile(knownHostsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("known_hosts was modified by a rejected host key — a mismatch must not enroll anything")
	}
}

// TestPublicKeyBytes_DerivesFromPrivateKey is the A32 regression: with an
// explicit identity and NO .pub file, the public key is DERIVED from the
// private key instead of falling through to an unrelated default; a
// mismatched .pub is an error, never a silent identity switch.
func TestPublicKeyBytes_DerivesFromPrivateKey(t *testing.T) {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen not available")
	}
	dir := t.TempDir()
	key := filepath.Join(dir, "id_test")
	if out, err := exec.Command("ssh-keygen", "-t", "ed25519", "-N", "", "-f", key).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v: %s", err, out)
	}

	derived, err := PublicKeyBytes(key)
	if err != nil {
		t.Fatalf("PublicKeyBytes without .pub: %v", err)
	}
	want, err := os.ReadFile(key + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	// MarshalAuthorizedKey omits the .pub's trailing comment — compare the
	// key type and material only.
	gotFields := strings.Fields(string(derived))
	wantFields := strings.Fields(string(want))
	if len(gotFields) < 2 || len(wantFields) < 2 || gotFields[0] != wantFields[0] || gotFields[1] != wantFields[1] {
		t.Errorf("derived key does not match the generated .pub:\n got %q\nwant %q", derived, want)
	}

	// A .pub that disagrees with the private key must be refused.
	other := filepath.Join(dir, "id_other")
	if out, err := exec.Command("ssh-keygen", "-t", "ed25519", "-N", "", "-f", other).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v: %s", err, out)
	}
	if err := os.Rename(other+".pub", key+".pub"); err != nil {
		t.Fatal(err)
	}
	if _, err := PublicKeyBytes(key); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("a mismatched .pub must be refused, got %v", err)
	}

	// PublicKeyPath with an explicit key never falls through to defaults.
	if err := os.Remove(key + ".pub"); err != nil {
		t.Fatal(err)
	}
	if _, err := PublicKeyPath(key); err == nil {
		t.Error("an explicit key without .pub must not select an unrelated default public key")
	}
}
