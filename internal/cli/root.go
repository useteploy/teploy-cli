package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/useteploy/teploy/internal/events"
	"github.com/useteploy/teploy/internal/ssh"
	"github.com/useteploy/teploy/internal/state"
	"github.com/useteploy/teploy/internal/trigger"
	"os"
	"os/user"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// Flags holds the global CLI flags, passed to subcommands explicitly.
type Flags struct {
	Host       string
	User       string
	Key        string
	ProjectDir string
	JSON       bool
	// StrictEnv is the opt-in strict mode (audits F57/TCL-32): unset
	// ${VAR} references in teploy.yml's env: fail the deploy with the
	// variable names listed, and a destination overlay that names a
	// map/list key with an empty value explicitly CLEARS the base's field
	// (`env: {}` means "no env"). Default off: deploys relying on
	// empty-expansion keep working.
	StrictEnv bool
	// Actor names the client recorded in the server event history
	// ("<client>[:<label>]", e.g. dash:alice); ActorOp links the event to
	// the client's own operation record. Both are client-asserted.
	Actor   string
	ActorOp string
	// Wait is how long a mutating command waits for another operation's
	// app lock before refusing (0 = refuse immediately, naming the holder).
	Wait time.Duration
	// ConfigUnprivileged (config --unprivileged) refuses config changes
	// that reach the host.
	ConfigUnprivileged bool
}

func NewRootCmd(version string) *cobra.Command {
	flags := &Flags{}

	root := &cobra.Command{
		Use:           "teploy",
		Short:         "Deploy apps to your servers",
		Long:          "A single binary that deploys Docker containers to any server with SSH access. No management server, no hosted dependencies.",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			actor := resolveActor(flags, version)
			events.SetProcessActor(actor)
			holder := events.ActorString(actor)
			if actor.Op != "" {
				holder += " operation " + actor.Op
			}
			state.SetLockHolder(holder)
			state.SetLockWait(flags.Wait)
			if flags.ProjectDir == "" {
				return nil
			}
			if err := os.Chdir(flags.ProjectDir); err != nil {
				failure := fmt.Errorf("changing to project directory %q: %w", flags.ProjectDir, err)
				if requested, _ := cmd.Flags().GetBool("trigger-stdin"); requested {
					result := trigger.Initial(trigger.Request{})
					result.ErrorCode = "admission_failed"
					if encodeErr := json.NewEncoder(cmd.OutOrStdout()).Encode(result); encodeErr != nil {
						return encodeErr
					}
				}
				return failure
			}
			return nil
		},
	}

	root.PersistentFlags().StringVar(&flags.Host, "host", "", "server host or servers.yml name (overrides the configured server)")
	root.PersistentFlags().StringVar(&flags.User, "user", "", "SSH user (default: root)")
	root.PersistentFlags().StringVar(&flags.Key, "key", "", "path to SSH private key")
	root.PersistentFlags().StringVar(&flags.ProjectDir, "project-dir", "", "run as if teploy was started in this directory")
	root.PersistentFlags().BoolVar(&flags.JSON, "json", false, "output in JSON format")
	root.PersistentFlags().StringVar(&flags.Actor, "actor", "", "client recorded in the server event history, as <client>[:<label>] (default cli:<user>@<host>, or $TEPLOY_ACTOR)")
	root.PersistentFlags().StringVar(&flags.ActorOp, "actor-op", "", "the calling client's operation id, recorded with each event")
	root.PersistentFlags().DurationVar(&flags.Wait, "wait", 0, "wait up to this long (e.g. 2m) for another operation holding the app's lock, instead of refusing")
	root.PersistentFlags().BoolVar(&flags.StrictEnv, "strict-env", false, "strict env/overlay mode: fail on unset ${VAR} in env:, and let an empty map/list in a destination overlay explicitly clear the base value")

	root.AddCommand(newDeployCmd(flags))
	root.AddCommand(newTriggerCmd(flags))
	root.AddCommand(newBuildCmd(flags))
	root.AddCommand(newExecCmd(flags))
	root.AddCommand(newAppCmd(flags))
	root.AddCommand(newEventsCmd(flags))
	root.AddCommand(newConfigCmd(flags))
	root.AddCommand(newSetupCmd(flags))
	root.AddCommand(newAccessoryCmd(flags))
	root.AddCommand(newKvCmd(flags))
	root.AddCommand(newEnvCmd(flags))
	root.AddCommand(newStopCmd(flags))
	root.AddCommand(newStartCmd(flags))
	root.AddCommand(newRestartCmd(flags))
	root.AddCommand(newRollbackCmd(flags))
	root.AddCommand(newRedeployCmd(flags))
	root.AddCommand(newRemoveCmd(flags))
	root.AddCommand(newReleasesCmd(flags))
	root.AddCommand(newPinCmd(flags))
	root.AddCommand(newUnpinCmd(flags))
	root.AddCommand(newPinsCmd(flags))
	root.AddCommand(newLogsCmd(flags))
	root.AddCommand(newLogCmd(flags))
	root.AddCommand(newStatusCmd(flags))
	root.AddCommand(newStatsCmd(flags))
	root.AddCommand(newHealthCmd(flags))
	root.AddCommand(newBackupCmd(flags))
	root.AddCommand(newDRCmd(flags, version))
	root.AddCommand(newLockCmd(flags))
	root.AddCommand(newUnlockCmd(flags))
	root.AddCommand(newInitCmd(flags))
	root.AddCommand(newValidateCmd(flags))
	root.AddCommand(newDoctorCmd(flags, version))
	root.AddCommand(newMetaCmd(flags))
	root.AddCommand(newPlanCmd(flags))
	root.AddCommand(newApplyCmd(flags))
	root.AddCommand(newDriftCmd(flags))
	root.AddCommand(newHealCmd(flags))
	root.AddCommand(newRegistryCmd(flags))
	root.AddCommand(newSecretCmd(flags))
	root.AddCommand(newPreviewCmd(flags))
	root.AddCommand(newTemplateCmd(flags))
	root.AddCommand(newNetworkCmd(flags))
	root.AddCommand(newServerCmd(flags))
	root.AddCommand(newScaleCmd(flags))
	root.AddCommand(newAutoDeployCmd(flags))
	root.AddCommand(newMaintenanceCmd(flags))
	root.AddCommand(newUpdateCmd(version))
	root.AddCommand(newVersionCmd(flags, version))

	return root
}

func Execute(version string) {
	root := NewRootCmd(version)
	if err := root.Execute(); err != nil {
		reportExecutionError(root, err, os.Stderr)
		os.Exit(1)
	}
}

// resolveActor builds the event actor: --actor, else $TEPLOY_ACTOR, else
// cli:<user>@<host>. Labels are bounded and stripped of control bytes so a
// client cannot smuggle structure into the log.
func resolveActor(flags *Flags, version string) events.Actor {
	raw := flags.Actor
	if raw == "" {
		raw = os.Getenv("TEPLOY_ACTOR")
	}
	client, label, _ := strings.Cut(raw, ":")
	if client == "" {
		client = "cli"
		label = localActorLabel()
	}
	return events.Actor{Client: cleanActorPart(client, 32), Label: cleanActorPart(label, 128), CLIVersion: version, Op: cleanActorPart(flags.ActorOp, 128)}
}

func localActorLabel() string {
	name := os.Getenv("USER")
	if u, err := user.Current(); err == nil && u.Username != "" {
		name = u.Username
	}
	host, _ := os.Hostname()
	if host == "" {
		return name
	}
	return name + "@" + host
}

func cleanActorPart(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, strings.TrimSpace(s))
	if len(s) > max {
		s = s[:max]
	}
	return s
}

// recordAppEvent appends a terminal event for a CLI-level command to the
// app's server history (warns, never fails the command).
func recordAppEvent(ctx context.Context, exec ssh.Executor, app, kind string, err error, detail map[string]any) {
	events.Record(ctx, exec, events.Event{App: app, Kind: kind, Outcome: events.Outcome(err), Detail: events.ErrDetail(detail, err)})
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
