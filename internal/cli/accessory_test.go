package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestQuoteExecArgs_PreservesMultiWordArgBoundary reproduces a real failure
// found live: `teploy accessory exec db -- psql -U postgres -c 'SELECT 1'`
// (the command's own --help example) reported "extra command-line argument
// \"1\" ignored" because the joined command string got re-split on
// whitespace by the remote `sh -c` in docker.Client.ExecStream, turning the
// single argv entry "SELECT 1" into two words.
//
// Verifies against a real /bin/sh, not just a string comparison, so it
// catches the actual double-shell-layering bug (ExecStream wraps the
// command in its own ssh.ShellQuote before handing it to `sh -c`) rather
// than just checking quoteExecArgs' output looks plausible.
func TestQuoteExecArgs_PreservesMultiWordArgBoundary(t *testing.T) {
	dumper := writeArgvDumper(t)
	args := []string{dumper, "-U", "postgres", "-c", "SELECT 1"}
	command := quoteExecArgs(args)

	// Mirrors docker.Client.ExecStream exactly: `sh -c %s` with %s =
	// ssh.ShellQuote(command) — the layer that silently re-split the
	// naive strings.Join(args, " ") version of this command.
	wrapped := "sh -c " + shQuoteForTest(command)
	out, err := exec.Command("/bin/sh", "-c", wrapped).CombinedOutput()
	if err != nil {
		t.Fatalf("running wrapped command: %v (output: %s)", err, out)
	}

	got := string(out)
	if !strings.Contains(got, "ARG<SELECT 1>") {
		t.Errorf("expected \"SELECT 1\" to survive as one argument, got:\n%s", got)
	}
	if strings.Contains(got, "ARG<SELECT>") || strings.Contains(got, "ARG<1>") {
		t.Errorf("argument boundary was lost — \"SELECT 1\" got re-split into separate words:\n%s", got)
	}
}

// TestQuoteExecArgs_SingleWordArgsUnaffected verifies the common case — a
// command whose args have no whitespace — passes through quoteExecArgs
// unaltered: two single-word args stay two args, neither split nor merged.
//
// It runs against the controlled argv-dumper, not a real binary. The prior
// version exec'd `redis-cli PING` and keyed off "not found" in the error,
// which made it environment-dependent and flaky: on a machine where
// redis-cli IS installed but no Redis is running, the binary runs and exits
// non-zero with "Connection refused" — no "not found" — and the test
// failed for a reason unrelated to argument quoting.
func TestQuoteExecArgs_SingleWordArgsUnaffected(t *testing.T) {
	dumper := writeArgvDumper(t)
	command := quoteExecArgs([]string{dumper, "PING"})

	wrapped := "sh -c " + shQuoteForTest(command)
	out, err := exec.Command("/bin/sh", "-c", wrapped).CombinedOutput()
	if err != nil {
		t.Fatalf("running wrapped command: %v (output: %s)", err, out)
	}

	got := string(out)
	// The single-word arg survives as exactly one argument, verbatim.
	if !strings.Contains(got, "ARG<PING>") {
		t.Errorf("expected PING to survive as one argument, got:\n%s", got)
	}
	// No spurious splitting, empty args, or quote characters leaking through.
	if strings.Contains(got, "ARG<>") || strings.Contains(got, "ARG<'PING'>") {
		t.Errorf("single-word arg was altered by quoting:\n%s", got)
	}
}

// writeArgvDumper writes a tiny shell script that prints each of its
// arguments on its own line, wrapped in ARG<...>, so tests can inspect
// exactly how many words a reconstructed command line was split into.
func writeArgvDumper(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "argv-dump.sh")
	script := "#!/bin/sh\nfor a; do printf 'ARG<%s>\\n' \"$a\"; done\n"
	if err := os.WriteFile(path, []byte(script), 0755); err != nil {
		t.Fatalf("writing argv dumper: %v", err)
	}
	return path
}

// shQuoteForTest mirrors ssh.ShellQuote — duplicated here rather than
// imported to keep this test's simulation of ExecStream's wrapping
// self-contained and obviously correct on inspection.
func shQuoteForTest(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}
