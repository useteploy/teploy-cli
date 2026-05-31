package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"os/user"

	"github.com/spf13/cobra"
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
