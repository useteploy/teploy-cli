package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The engine keeps its token in a directory only root can enter. A user
// with passwordless sudo cannot see into it, so the token command must
// test for the file under sudo too. This runs the real command in a local
// shell, with the token's directory shut to this user and a stand-in sudo
// that opens it only for the command it runs.
func TestSandboxTokenIsReadThroughSudoFromARootOnlyDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX shell")
	}
	if os.Geteuid() == 0 {
		t.Skip("root reads the directory without sudo; the sudo path cannot be shown as root")
	}
	root := t.TempDir()
	engineDir := filepath.Join(root, "engine")
	if err := os.Mkdir(engineDir, 0o700); err != nil {
		t.Fatal(err)
	}
	tokenFile := filepath.Join(engineDir, "token")
	if err := os.WriteFile(tokenFile, []byte("sekret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(engineDir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(engineDir, 0o700) })

	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	sudoLog := filepath.Join(root, "sudo.log")
	fakeSudo := "#!/bin/sh\n" +
		"[ \"$1\" = -n ] || { echo 'sudo: a password is required' >&2; exit 1; }\nshift\n" +
		"echo used >> '" + sudoLog + "'\n" +
		"chmod 700 '" + engineDir + "'\n\"$@\"\nrc=$?\nchmod 000 '" + engineDir + "'\nexit $rc\n"
	if err := os.WriteFile(filepath.Join(bin, "sudo"), []byte(fakeSudo), 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(file string) (string, error) {
		cmd := exec.Command("sh", "-c", sandboxTokenCommand(file))
		cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
		out, err := cmd.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}

	got, err := run(tokenFile)
	if err != nil || got != "sekret" {
		t.Fatalf("token read = %q, %v; want the token through sudo", got, err)
	}
	if b, _ := os.ReadFile(sudoLog); !strings.Contains(string(b), "used") {
		t.Fatal("the read did not go through sudo")
	}

	got, err = run(filepath.Join(engineDir, "missing"))
	if err != nil || got != sandboxTokenMarker {
		t.Fatalf("missing token = %q, %v; want the absent marker", got, err)
	}
}
