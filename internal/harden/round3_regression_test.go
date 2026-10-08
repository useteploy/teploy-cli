package harden

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/ssh"
)

func TestRound3SSHWholeScriptExecutesAndRollsBack(t *testing.T) {
	for _, mode := range []string{"success", "validate-fails", "reload-fails"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			bin := filepath.Join(root, "bin")
			os.Mkdir(bin, 0700)
			config := filepath.Join(root, "sshd_config")
			original := []byte("# old config\n")
			os.WriteFile(config, original, 0600)
			policy := filepath.Join(root, "policy with ' quote")
			os.WriteFile(policy, []byte("PasswordAuthentication no\n"), 0600)
			os.WriteFile(filepath.Join(bin, "sshd"), []byte(`#!/bin/sh
printf 'sshd %s\n' "$*" >> "$R3_LOG"
if [ "$R3_MODE" = validate-fails ]; then exit 1; fi
if [ "$1" = -T ]; then printf 'permitrootlogin without-password\npasswordauthentication no\nkbdinteractiveauthentication no\npubkeyauthentication yes\n'; fi
`), 0700)
			os.WriteFile(filepath.Join(bin, "systemctl"), []byte(`#!/bin/sh
printf 'reload\n' >> "$R3_LOG"
if [ "$R3_MODE" = reload-fails ] && [ ! -e "$R3_ONCE" ]; then touch "$R3_ONCE"; exit 1; fi
`), 0700)
			script := strings.ReplaceAll(sshPolicyScript(policy), "/etc/ssh/sshd_config", config)
			command := exec.Command("sh", "-c", "sh -c "+ssh.ShellQuote(script))
			command.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "R3_MODE="+mode, "R3_LOG="+filepath.Join(root, "events"), "R3_ONCE="+filepath.Join(root, "once"))
			output, err := command.CombinedOutput()
			data, _ := os.ReadFile(config)
			events, _ := os.ReadFile(filepath.Join(root, "events"))
			if mode == "success" {
				if err != nil || !strings.Contains(string(data), "PasswordAuthentication no") || !strings.Contains(string(events), "reload") {
					t.Fatalf("whole script did not install/reload: %v %s %s", err, output, events)
				}
			} else {
				if err == nil || string(data) != string(original) {
					t.Fatalf("failure changed incumbent: %v %s", err, data)
				}
				// Count exact lines: the fixture paths embed the subtest
				// name, so a substring count of "reload" would count the
				// temp directory name too.
				reloads := 0
				for _, line := range strings.Split(string(events), "\n") {
					if line == "reload" {
						reloads++
					}
				}
				if mode == "reload-fails" && reloads != 2 {
					t.Fatalf("rollback did not reload old config (reloads=%d)", reloads)
				}
			}
		})
	}
}
