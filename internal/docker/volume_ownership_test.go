package docker

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/ssh"
)

func volumePermissions(uid, gid uint32, mode string) config.VolumeOwnership {
	return config.VolumeOwnership{UID: &uid, GID: &gid, Mode: mode}
}

func TestVolumeOwnershipCommandIsBounded(t *testing.T) {
	m := ssh.NewMockExecutor("host", ssh.MockCommand{Match: "if [", Output: ""}, ssh.MockCommand{Match: "teploy_volume_actor", Output: ""})
	if err := NewClient(m).ProvisionManagedVolume(context.Background(), "observe", "", "observe-state", volumePermissions(10001, 10001, "0700")); err != nil {
		t.Fatal(err)
	}
	command := m.Calls[0]
	for _, bad := range []string{"chown -R", "chmod -R", "docker run", "/etc/"} {
		if strings.Contains(command, bad) {
			t.Fatalf("unsafe ownership command: %s", bad)
		}
	}
	if !strings.Contains(command, "sudo -n") || !strings.Contains(command, "populated volume ownership mismatch") {
		t.Fatal("missing privilege/refusal contract")
	}
	m.Calls = nil
	if err := NewClient(m).ProvisionManagedVolume(context.Background(), "observe", "", "/tmp/escape", volumePermissions(10001, 10001, "0700")); err == nil || len(m.Calls) > 0 {
		t.Fatal("host path ownership admitted")
	}
}

func TestVolumeOwnershipRealDirectoryLifecycle(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("descriptor fixture requires Linux directory descriptors")
	}
	root := t.TempDir()
	p := volumePermissions(uint32(os.Getuid()), uint32(os.Getgid()), "0700")
	// Relocate only the trusted starting descriptor: t.TempDir lives under a
	// world-writable ancestor which production correctly refuses. This fixture
	// exercises the descriptor core, not production ancestor admission.
	fixture := func(name string, permission config.VolumeOwnership) string {
		python := strings.Replace(managedDirectoryPython, "fd = os.open('/', flags)", "fd = os.open("+fmt.Sprintf("%q", root)+", flags)", 1)
		return "python3 -c " + ssh.ShellQuote(python) + " " + ssh.ShellQuote(name) + " provision " + fmt.Sprintf("%d %d %s", *permission.UID, *permission.GID, permission.Mode)
	}
	script := fixture("data", p)
	run := func(script string) error {
		cmd := exec.Command("sh", "-c", script)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Logf("script refusal: %s", out)
		}
		return err
	}
	if err := run(script); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "data")
	info, err := os.Stat(dir)
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("fresh mode: %v %v", info, err)
	}
	data := filepath.Join(dir, "retained")
	if err := os.WriteFile(data, []byte("existing user data"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := run(script); err != nil {
		t.Fatalf("idempotent populated matching directory: %v", err)
	}
	mismatch := fixture("data", volumePermissions(uint32(os.Getuid()), uint32(os.Getgid()), "0750"))
	if run(mismatch) == nil {
		t.Fatal("populated mismatch changed")
	}
	info, _ = os.Stat(data)
	if info.Mode().Perm() != 0640 {
		t.Fatal("descendant permissions changed")
	}
	contents, _ := os.ReadFile(data)
	if string(contents) != "existing user data" {
		t.Fatal("descendant bytes changed")
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if run(fixture("link", p)) == nil {
		t.Fatal("symlink volume accepted")
	}
	parentLink := filepath.Join(root, "parent")
	if err := os.Symlink(outside, parentLink); err != nil {
		t.Fatal(err)
	}
	if run(fixture("parent/data", p)) == nil {
		t.Fatal("symlink parent accepted")
	}
	if _, err := os.Stat(filepath.Join(outside, "data")); !os.IsNotExist(err) {
		t.Fatal("arbitrary host path touched")
	}
}
