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
	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/deploy"
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
	return &cobra.Command{
		Use:   "health",
		Short: "Run health check on the running app",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runHealth(flags)
		},
	}
}

func runHealth(flags *Flags) error {
	appCfg, err := config.LoadApp(".")
	if err != nil {
		return err
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	executor, err := connectForApp(ctx, flags, appCfg)
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
	if err := deployer.HealthCheckPublic(ctx, current.CurrentPort); err != nil {
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
