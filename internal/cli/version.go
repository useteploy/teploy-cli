package cli

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

// versionDTO is the `teploy version --json` envelope (X02 §2.1): the
// machine-interface version plus the capability registry, so one call
// replaces help-text scraping as the compatibility handshake.
type versionDTO struct {
	Version          string   `json:"version"`
	MachineInterface int      `json:"machine_interface"`
	Capabilities     []string `json:"capabilities"`
}

func newVersionCmd(flags *Flags, version string) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Show teploy version",
		Long: `Show teploy version.

With --json, emits the machine-interface handshake instead: the version,
the machine_interface number (fail closed if a consumer supports less),
and the capability tokens this build advertises. One call replaces
help-text scraping as the compatibility check.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return writeVersion(cmd.OutOrStdout(), version, flags.JSON)
		},
	}
}

func writeVersion(out io.Writer, version string, jsonOutput bool) error {
	if !jsonOutput {
		fmt.Fprintf(out, "teploy %s\n", version)
		return nil
	}
	return json.NewEncoder(out).Encode(versionDTO{
		Version:          version,
		MachineInterface: MachineInterface,
		Capabilities:     MachineCapabilities(),
	})
}
