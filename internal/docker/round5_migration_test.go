package docker

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRound5ActualManualRecoveryScriptStopsAllAndRefusesCopyAfterFailure(t *testing.T) {
	for _, fail := range []bool{true, false} {
		t.Run(map[bool]string{true: "failed-stop", false: "all-stopped"}[fail], func(t *testing.T) {
			root := t.TempDir()
			source := filepath.Join(root, "source's data")
			dest := filepath.Join(root, "new's data")
			os.Mkdir(source, 0700)
			os.WriteFile(filepath.Join(source, "record"), []byte("original"), 0600)
			ids := strings.Repeat("a", 64) + "\n" + strings.Repeat("b", 64)
			log := filepath.Join(root, "stop.log")
			script := `#!/bin/sh
case "$1" in
 ps) if test -f "$FIXTURE_LOG" && test "$(wc -l < "$FIXTURE_LOG" | tr -d ' ')" = 2; then exit 0; fi; printf '%s\n' "$FIXTURE_IDS";;
 stop) printf '%s\n' "$3" >> "$FIXTURE_LOG"; if test "$FAIL_STOP" = yes; then exit 1; fi;;
 *) exit 99;;
esac
`
			os.WriteFile(filepath.Join(root, "docker"), []byte(script), 0700)
			command := exec.Command("sh", "-c", manualMigrationScript("demo", []VolumeMismatch{{ExistingSource: source, ExpectedSource: dest}}))
			failMode := "no"
			if fail {
				failMode = "yes"
			}
			command.Env = append(os.Environ(), "PATH="+root+":"+os.Getenv("PATH"), "FIXTURE_LOG="+log, "FIXTURE_IDS="+ids, "FAIL_STOP="+failMode)
			output, err := command.CombinedOutput()
			if fail {
				if err == nil {
					t.Fatal("failed stop succeeded")
				}
				if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
					t.Fatal("copy ran after failed stop")
				}
			} else {
				if err != nil {
					t.Fatal(err, string(output))
				}
				data, err := os.ReadFile(filepath.Join(dest, "record"))
				if err != nil || string(data) != "original" {
					t.Fatal("quoted checked copy failed", err)
				}
				stops, _ := os.ReadFile(log)
				if strings.Count(string(stops), "\n") != 2 {
					t.Fatal("not all writers stopped")
				}
			}
		})
	}
}
