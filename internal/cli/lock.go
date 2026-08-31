package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"os/user"

	"github.com/spf13/cobra"
	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/ssh"
	"github.com/useteploy/teploy/internal/state"
)

func newLockCmd(flags *Flags) *cobra.Command {
	var message string
	var appName string

	cmd := &cobra.Command{
		Use:   "lock",
		Short: "Freeze deploys for the app",
		Long:  "Place a manual deploy lock on the app. All deploys are blocked until 'teploy unlock' is run.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLock(flags, appName, message)
		},
	}

	cmd.Flags().StringVarP(&message, "message", "m", "", "reason for locking")
	cmd.Flags().StringVar(&appName, "app", "", "app name — act on server state instead of teploy.yml (requires --host)")
	cmd.AddCommand(newLockStatusCmd(flags))
	return cmd
}

func runLock(flags *Flags, appName, message string) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	appCfg, executor, err := resolveApp(ctx, flags, appName)
	if err != nil {
		return err
	}
	defer executor.Close()

	if err := state.EnsureAppDir(ctx, executor, appCfg.App); err != nil {
		return err
	}

	username := "unknown"
	if u, err := user.Current(); err == nil {
		username = u.Username
	}

	if err := state.AcquireManualLock(ctx, executor, appCfg.App, username, message); err != nil {
		return err
	}

	fmt.Printf("Locked %s", appCfg.App)
	if message != "" {
		fmt.Printf(": %s", message)
	}
	fmt.Println()
	return nil
}

func newLockStatusCmd(flags *Flags) *cobra.Command {
	var appName string
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Report the current deploy lock (read-only)",
		Long: "Report whether deploys are locked, without touching the lock — this never " +
			"places, refreshes or releases one.\n\n" +
			"It separates the three states that otherwise look identical from the outside: " +
			"an 'auto' lock (a deploy is running right now), a 'manual' lock (a human froze " +
			"deploys with 'teploy lock', and it will never expire), and a stale lock left " +
			"behind by a deploy that died before releasing it — the last of which the next " +
			"deploy breaks on its own.\n\n" +
			"Exits 0 whether or not a lock is held; a lock is a state to report, not a " +
			"failure. Use --json for the same answer in a stable machine-readable shape.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLockStatus(flags, appName, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&appName, "app", "", "app name — act on server state instead of teploy.yml (requires --host)")
	return cmd
}

// lockStatusDTO is the --json shape of `teploy lock status`, and the whole
// point of the command: a CI script asking "is a deploy in progress?" reads
// these fields instead of parsing prose or shelling out to pgrep. `locked` and
// `stale` are always emitted (never omitempty) so a consumer can branch on
// them without distinguishing false from absent.
type lockStatusDTO struct {
	App     string `json:"app"`
	Server  string `json:"server"`
	Locked  bool   `json:"locked"`
	Type    string `json:"type,omitempty"` // auto, manual, or heal
	User    string `json:"user,omitempty"`
	Message string `json:"message,omitempty"`
	Since   string `json:"since,omitempty"` // RFC3339, as recorded in the lock
	Stale   bool   `json:"stale"`
}

func runLockStatus(flags *Flags, appName string, out io.Writer) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	appCfg, executor, err := resolveApp(ctx, flags, appName)
	if err != nil {
		return err
	}
	defer executor.Close()

	return reportLockStatus(ctx, flags, appCfg, executor, out)
}

func reportLockStatus(ctx context.Context, flags *Flags, appCfg *config.AppConfig, executor ssh.Executor, out io.Writer) error {
	// Deliberately no state.EnsureAppDir here, unlike runLock: a status
	// command must not create anything on the server, not even a directory.
	// ReadLock is a bare `cat`, so this whole path is read-only.
	info, err := state.ReadLock(ctx, executor, appCfg.App)
	if err != nil {
		return err
	}
	return writeLockStatus(out, lockStatus(appCfg.App, executor.Host(), info), flags.JSON)
}

// lockStatus builds the report from its inputs (no I/O), so it is directly
// unit-testable. A nil info means no lock exists.
func lockStatus(app, server string, info *state.LockInfo) lockStatusDTO {
	status := lockStatusDTO{App: app, Server: server}
	if info == nil {
		return status
	}
	status.Locked = true
	status.Type = info.Type
	status.User = info.User
	status.Message = info.Message
	status.Since = info.TS
	status.Stale = info.IsStale()
	return status
}

func writeLockStatus(out io.Writer, status lockStatusDTO, jsonOutput bool) error {
	if jsonOutput {
		return json.NewEncoder(out).Encode(status)
	}

	if !status.Locked {
		fmt.Fprintf(out, "%s is not locked\n", status.App)
		return nil
	}

	fmt.Fprintf(out, "%s is locked on %s\n", status.App, status.Server)
	fmt.Fprintf(out, "  Type:    %s\n", lockTypeDetail(status.Type))
	if status.User != "" {
		fmt.Fprintf(out, "  Held by: %s\n", status.User)
	}
	if status.Message != "" {
		fmt.Fprintf(out, "  Message: %s\n", status.Message)
	}
	if status.Since != "" {
		fmt.Fprintf(out, "  Since:   %s\n", status.Since)
	}
	if status.Stale {
		fmt.Fprintln(out, "  Stale:   yes — whatever took this lock is presumed dead; the next deploy breaks it automatically")
	}
	return nil
}

// lockTypeDetail expands a LockInfo.Type into what it means for deploys.
func lockTypeDetail(lockType string) string {
	switch lockType {
	case "auto":
		return "auto (a deploy is in progress)"
	case "manual":
		return "manual (deploys frozen — release with 'teploy unlock')"
	case "heal":
		return "heal (a heal restart is in progress)"
	case "":
		return "unknown"
	default:
		return lockType
	}
}

func newUnlockCmd(flags *Flags) *cobra.Command {
	var appName string
	cmd := &cobra.Command{
		Use:   "unlock",
		Short: "Release deploy lock for the app",
		Long:  "Remove the manual deploy lock, allowing deploys to proceed.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runUnlock(flags, appName)
		},
	}
	cmd.Flags().StringVar(&appName, "app", "", "app name — act on server state instead of teploy.yml (requires --host)")
	return cmd
}

func runUnlock(flags *Flags, appName string) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	appCfg, executor, err := resolveApp(ctx, flags, appName)
	if err != nil {
		return err
	}
	defer executor.Close()

	// Check if locked first for user feedback.
	info, _ := state.ReadLock(ctx, executor, appCfg.App)
	if info == nil {
		fmt.Printf("%s is not locked\n", appCfg.App)
		return nil
	}

	state.ReleaseLock(ctx, executor, appCfg.App)
	fmt.Printf("Unlocked %s\n", appCfg.App)
	return nil
}
