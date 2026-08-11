package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"time"

	"github.com/spf13/cobra"
	"github.com/useteploy/teploy/internal/deploy"
	"github.com/useteploy/teploy/internal/docker"
	"github.com/useteploy/teploy/internal/state"
)

type healthDTO struct {
	App        string    `json:"app"`
	Host       string    `json:"host"`
	Port       int       `json:"port"`
	Healthy    bool      `json:"healthy"`
	Error      string    `json:"error"`
	ObservedAt time.Time `json:"observed_at"`
}

func newHealthCmd(flags *Flags) *cobra.Command {
	var appName string
	cmd := &cobra.Command{
		Use:   "health",
		Short: "Run health check on the running app",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runHealth(flags, appName)
		},
	}
	cmd.Flags().StringVar(&appName, "app", "", "app name — act on server state instead of teploy.yml (requires --host)")
	return cmd
}

func runHealth(flags *Flags, appName string) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	appCfg, executor, err := resolveApp(ctx, flags, appName)
	if err != nil {
		return err
	}
	defer executor.Close()

	current, err := state.Read(ctx, executor, appCfg.App)
	if err != nil || current == nil {
		statusErr := fmt.Errorf("no deploy state found for %s — deploy first", appCfg.App)
		if flags.JSON {
			if encodeErr := json.NewEncoder(os.Stdout).Encode(healthDTO{App: appCfg.App, Host: executor.Host(), Healthy: false, Error: statusErr.Error(), ObservedAt: time.Now().UTC()}); encodeErr != nil {
				return encodeErr
			}
		}
		return statusErr
	}

	if !flags.JSON {
		fmt.Printf("Running health check on %s (port %d)...\n", appCfg.App, current.CurrentPort)
	}

	deployerOut := io.Writer(os.Stdout)
	if flags.JSON {
		deployerOut = io.Discard
	}
	deployer := deploy.NewDeployer(executor, deployerOut)
	// Probe the address the container is actually published on. An app with a
	// `bind:` is not reachable at localhost, so a localhost-only probe reports
	// a perfectly healthy app as failed — the same trap that made every deploy
	// of a bound app an outage until the deployer learned to read the bind.
	if err := deployer.HealthCheckAt(ctx, current.CurrentPort, docker.ContainerName(appCfg.App, "web", current.CurrentHash)); err != nil {
		if flags.JSON {
			if encodeErr := json.NewEncoder(os.Stdout).Encode(healthDTO{App: appCfg.App, Host: executor.Host(), Port: current.CurrentPort, Healthy: false, Error: err.Error(), ObservedAt: time.Now().UTC()}); encodeErr != nil {
				return encodeErr
			}
		} else {
			fmt.Printf("Health check FAILED: %v\n", err)
		}
		return err
	}
	if flags.JSON {
		return json.NewEncoder(os.Stdout).Encode(healthDTO{App: appCfg.App, Host: executor.Host(), Port: current.CurrentPort, Healthy: true, ObservedAt: time.Now().UTC()})
	}

	fmt.Println("Health check passed")
	return nil
}
