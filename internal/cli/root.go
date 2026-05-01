package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// Flags holds the global CLI flags, passed to subcommands explicitly.
type Flags struct {
	Host string
	User string
	Key  string
	JSON bool
}

func NewRootCmd(version string) *cobra.Command {
	flags := &Flags{}

	root := &cobra.Command{
		Use:   "teploy",
		Short: "Deploy apps to your servers",
		Long:  "A single binary that deploys Docker containers to any server with SSH access. No management server, no hosted dependencies.",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.PersistentFlags().StringVar(&flags.Host, "host", "", "server host (overrides servers.yml)")
	root.PersistentFlags().StringVar(&flags.User, "user", "", "SSH user (default: root)")
	root.PersistentFlags().StringVar(&flags.Key, "key", "", "path to SSH private key")
	root.PersistentFlags().BoolVar(&flags.JSON, "json", false, "output in JSON format")

	root.AddCommand(newDeployCmd(flags))
	root.AddCommand(newExecCmd(flags))
	root.AddCommand(newSetupCmd(flags))
	root.AddCommand(newAccessoryCmd(flags))
	root.AddCommand(newEnvCmd(flags))
	root.AddCommand(newStopCmd(flags))
	root.AddCommand(newStartCmd(flags))
	root.AddCommand(newRestartCmd(flags))
	root.AddCommand(newRollbackCmd(flags))
	root.AddCommand(newReleasesCmd(flags))
	root.AddCommand(newLogsCmd(flags))
	root.AddCommand(newLogCmd(flags))
	root.AddCommand(newStatusCmd(flags))
	root.AddCommand(newStatsCmd(flags))
	root.AddCommand(newHealthCmd(flags))
	root.AddCommand(newBackupCmd(flags))
	root.AddCommand(newLockCmd(flags))
	root.AddCommand(newUnlockCmd(flags))
	root.AddCommand(newInitCmd())
	root.AddCommand(newValidateCmd(flags))
	root.AddCommand(newRegistryCmd(flags))
	root.AddCommand(newSecretCmd(flags))
	root.AddCommand(newPreviewCmd(flags))
	root.AddCommand(newTemplateCmd(flags))
	root.AddCommand(newNetworkCmd(flags))
	root.AddCommand(newServerCmd(flags))
	root.AddCommand(newScaleCmd(flags))
	root.AddCommand(newAutoDeployCmd(flags))
	root.AddCommand(newMaintenanceCmd(flags))
	root.AddCommand(newUICmd())
	root.AddCommand(newUpdateCmd(version))
	root.AddCommand(newVersionCmd(version))

	return root
}

func Execute(version string) {
	root := NewRootCmd(version)
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
