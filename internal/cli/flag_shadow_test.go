package cli

import (
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// A subcommand flag named like a global one hides it: `sandbox new` once
// had its own --user (the user inside the sandbox), so `--user deploy`
// never reached SSH and a non-root login could not create a sandbox.
// No sandbox command may define a local flag with a global flag's name.
// (Older commands do it on purpose: `server add --user` is the user the
// server is saved with, and several --json flags mean the global one.)
func TestNoSandboxCommandHidesAGlobalFlag(t *testing.T) {
	root := NewRootCmd("test")
	globals := map[string]bool{}
	root.PersistentFlags().VisitAll(func(f *pflag.Flag) { globals[f.Name] = true })
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		c.LocalNonPersistentFlags().VisitAll(func(f *pflag.Flag) {
			if globals[f.Name] {
				t.Errorf("%q defines --%s, which hides the global --%s", c.CommandPath(), f.Name, f.Name)
			}
		})
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	sandbox, _, err := root.Find([]string{"sandbox"})
	if err != nil || sandbox.Name() != "sandbox" {
		t.Fatalf("no sandbox command: %v", err)
	}
	walk(sandbox)
}
