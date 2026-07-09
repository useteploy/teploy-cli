package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/useteploy/teploy/internal/docker"
	"github.com/useteploy/teploy/internal/ssh"
	"github.com/useteploy/teploy/internal/state"
)

// healConfPath / healStatePath live under the app's /deployments dir, alongside
// state and .lock — host-readable, and (unlike teploy.yml, which is never
// shipped to the server) the source of truth for whether heal is enabled.
func healConfPath(app string) string  { return fmt.Sprintf("/deployments/%s/heal.conf", app) }
func healStatePath(app string) string { return fmt.Sprintf("/deployments/%s/.heal-state", app) }

// healBinaryPath is where the teploy binary lives on the host for the timer to
// invoke (same convention as teploy-sandbox).
const healBinaryPath = "/usr/local/bin/teploy"

func healServiceName(app string) string { return "teploy-heal-" + app + ".service" }
func healTimerName(app string) string   { return "teploy-heal-" + app + ".timer" }

// sudoPrefixFor returns "sudo " unless the SSH user is already root.
func sudoPrefixFor(ctx context.Context, exec ssh.Executor) string {
	out, err := exec.Run(ctx, "id -u")
	if err == nil && strings.TrimSpace(out) == "0" {
		return ""
	}
	return "sudo "
}

// generateHealService renders the oneshot unit the timer fires. It is NOT
// enabled directly (no [Install]); the timer owns its schedule.
func generateHealService(app, binPath string) string {
	return fmt.Sprintf(`[Unit]
Description=Teploy self-heal for %s
After=docker.service
Requires=docker.service

[Service]
Type=oneshot
ExecStart=%s heal run --app %s
`, app, binPath, app)
}

// generateHealTimer renders the timer that fires the heal service every
// intervalSec seconds.
func generateHealTimer(app string, intervalSec int) string {
	return fmt.Sprintf(`[Unit]
Description=Teploy self-heal timer for %s

[Timer]
OnBootSec=60
OnUnitActiveSec=%ds
AccuracySec=5s

[Install]
WantedBy=timers.target
`, app, intervalSec)
}

// installSystemdUnit stages a unit file in /tmp (SFTP can't write root-owned
// /etc/systemd/system) then moves it into place with sudo.
func installSystemdUnit(ctx context.Context, exec ssh.Executor, sudo, name, content string) error {
	staging := "/tmp/" + name
	if err := exec.Upload(ctx, strings.NewReader(content), staging, "0644"); err != nil {
		return fmt.Errorf("staging %s: %w", name, err)
	}
	if _, err := exec.Run(ctx, fmt.Sprintf("%smv %s /etc/systemd/system/%s", sudo, staging, name)); err != nil {
		return fmt.Errorf("installing %s: %w", name, err)
	}
	return nil
}

// HealConfig is the host-side opt-in manifest. Its presence means heal is
// enabled for the app; it lists which processes to watch and the backoff policy.
type HealConfig struct {
	Processes   []string `json:"processes"`       // opted-in process names (web is the only one with a probe surface)
	MaxAttempts int      `json:"max_attempts"`    // consecutive restarts before giving up + alerting
	BackoffSecs int      `json:"backoff_seconds"` // minimum seconds between restarts of the same container
	HealthPath  string   `json:"health_path"`     // probe path (default /health)
}

func (c HealConfig) withDefaults() HealConfig {
	if c.MaxAttempts == 0 {
		c.MaxAttempts = 5
	}
	if c.BackoffSecs == 0 {
		c.BackoffSecs = 60
	}
	if c.HealthPath == "" {
		c.HealthPath = "/health"
	}
	if len(c.Processes) == 0 {
		c.Processes = []string{"web"}
	}
	return c
}

// healContainerState tracks consecutive restart attempts + the last restart time
// for one container, so heal can back off instead of thrashing a container whose
// dependency is down.
type healContainerState struct {
	Attempts    int   `json:"attempts"`
	LastRestart int64 `json:"last_restart"` // unix seconds
	GaveUp      bool  `json:"gave_up"`      // alerted + stopped trying
}

func newHealCmd(flags *Flags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "heal",
		Short: "Restart unhealthy app containers in place (bounded self-heal)",
		Long: "Self-heal restarts a container that is running but failing its health " +
			"probe (crashes are already handled by Docker's restart policy). It is opt-in " +
			"per app, web-only, yields to any in-progress deploy, and backs off to avoid " +
			"thrashing. `enable` turns it on for an app; a systemd timer runs `heal run`.",
	}
	cmd.AddCommand(newHealEnableCmd(flags))
	cmd.AddCommand(newHealDisableCmd(flags))
	cmd.AddCommand(newHealRunCmd(flags))
	cmd.AddCommand(newHealStatusCmd(flags))
	return cmd
}

func newHealStatusCmd(flags *Flags) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show whether self-heal is enabled + recent heal events",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runHealStatus(flags)
		},
	}
}

func runHealStatus(flags *Flags) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	appCfg, executor, err := resolveApp(ctx, flags, "")
	if err != nil {
		return err
	}
	defer executor.Close()

	cfg, ok, err := readHealConf(ctx, executor, appCfg.App)
	if err != nil {
		return err
	}
	if !ok {
		fmt.Printf("Self-heal is DISABLED for %s.\n", appCfg.App)
		return nil
	}
	active, _ := executor.Run(ctx, "systemctl is-active "+healTimerName(appCfg.App)+" 2>/dev/null")
	fmt.Printf("Self-heal ENABLED for %s\n", appCfg.App)
	fmt.Printf("  processes:    %s\n", strings.Join(cfg.Processes, ","))
	fmt.Printf("  max-attempts: %d\n", cfg.MaxAttempts)
	fmt.Printf("  backoff:      %ds\n", cfg.BackoffSecs)
	fmt.Printf("  timer:        %s\n", strings.TrimSpace(active))

	events, _ := executor.Run(ctx, fmt.Sprintf(
		"grep '\"app\":\"%s\"' /deployments/teploy.log 2>/dev/null | grep heal_ | tail -5", appCfg.App))
	if strings.TrimSpace(events) != "" {
		fmt.Println("  recent heal events:")
		for _, line := range strings.Split(strings.TrimSpace(events), "\n") {
			fmt.Printf("    %s\n", line)
		}
	}
	return nil
}

func newHealEnableCmd(flags *Flags) *cobra.Command {
	var maxAttempts, backoff, interval int
	var healthPath string
	var noBinary bool
	cmd := &cobra.Command{
		Use:   "enable",
		Short: "Enable self-heal for the app (manifest + systemd timer on the host)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runHealEnable(flags, HealConfig{
				MaxAttempts: maxAttempts, BackoffSecs: backoff, HealthPath: healthPath,
			}, interval, noBinary)
		},
	}
	cmd.Flags().IntVar(&maxAttempts, "max-attempts", 0, "consecutive restarts before giving up + alerting (default 5)")
	cmd.Flags().IntVar(&backoff, "backoff", 0, "minimum seconds between restarts of the same container (default 60)")
	cmd.Flags().IntVar(&interval, "interval", 30, "seconds between heal passes (systemd timer)")
	cmd.Flags().StringVar(&healthPath, "health-path", "", "health probe path (default /health)")
	cmd.Flags().BoolVar(&noBinary, "no-binary", false, "skip downloading the teploy binary to the host (assume it's already at "+healBinaryPath+")")
	return cmd
}

func newHealDisableCmd(flags *Flags) *cobra.Command {
	return &cobra.Command{
		Use:   "disable",
		Short: "Disable self-heal for the app (removes the host-side heal manifest)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runHealDisable(flags)
		},
	}
}

// newHealRunCmd is the one-shot the systemd timer fires ON the host. It uses a
// local executor (runs docker/curl locally), so it needs the teploy binary on
// the server — shipped the same way `autodeploy serve` is.
func newHealRunCmd(flags *Flags) *cobra.Command {
	var app string
	cmd := &cobra.Command{
		Use:    "run",
		Short:  "Run one heal pass on the local host (invoked by the systemd timer)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if app == "" {
				return fmt.Errorf("--app is required")
			}
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
			defer cancel()
			exec := ssh.NewLocalExecutor()
			n, err := runHealPass(ctx, exec, app, os.Stdout)
			if err != nil {
				return err
			}
			if !flags.JSON {
				fmt.Printf("heal: %d container(s) restarted\n", n)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&app, "app", "", "app to heal (required)")
	return cmd
}

func runHealEnable(flags *Flags, cfg HealConfig, intervalSec int, noBinary bool) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	appCfg, executor, err := resolveApp(ctx, flags, "")
	if err != nil {
		return err
	}
	defer executor.Close()
	if intervalSec < 5 {
		intervalSec = 30
	}

	// 1. Write the host-side opt-in manifest (the source of truth heal reads).
	cfg = cfg.withDefaults()
	data, _ := json.MarshalIndent(cfg, "", "  ")
	if err := executor.Upload(ctx, bytes.NewReader(append(data, '\n')), healConfPath(appCfg.App), "0644"); err != nil {
		return fmt.Errorf("writing heal manifest: %w", err)
	}

	// 2. Ensure the teploy binary is on the host for the timer to invoke.
	if noBinary {
		fmt.Printf("Skipping binary install — assuming teploy is at %s.\n", healBinaryPath)
	} else {
		fmt.Println("Installing teploy binary on the host...")
		if _, err := deployTeployBinaryToServer(ctx, executor, healBinaryPath); err != nil {
			return fmt.Errorf("installing teploy binary (use --no-binary if it's already present): %w", err)
		}
	}

	// 3. Install + start the systemd timer.
	sudo := sudoPrefixFor(ctx, executor)
	if err := installSystemdUnit(ctx, executor, sudo, healServiceName(appCfg.App), generateHealService(appCfg.App, healBinaryPath)); err != nil {
		return err
	}
	if err := installSystemdUnit(ctx, executor, sudo, healTimerName(appCfg.App), generateHealTimer(appCfg.App, intervalSec)); err != nil {
		return err
	}
	if _, err := executor.Run(ctx, sudo+"systemctl daemon-reload"); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w", err)
	}
	if _, err := executor.Run(ctx, sudo+"systemctl enable --now "+healTimerName(appCfg.App)); err != nil {
		return fmt.Errorf("enabling heal timer: %w", err)
	}

	fmt.Printf("Self-heal enabled for %s: web containers, max-attempts=%d, backoff=%ds, every %ds.\n",
		appCfg.App, cfg.MaxAttempts, cfg.BackoffSecs, intervalSec)
	return nil
}

func runHealDisable(flags *Flags) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	appCfg, executor, err := resolveApp(ctx, flags, "")
	if err != nil {
		return err
	}
	defer executor.Close()

	sudo := sudoPrefixFor(ctx, executor)
	timer, svc := healTimerName(appCfg.App), healServiceName(appCfg.App)
	// Best-effort teardown of the timer/service, then remove the manifest.
	executor.Run(ctx, fmt.Sprintf("%ssystemctl disable --now %s 2>/dev/null", sudo, timer))
	executor.Run(ctx, fmt.Sprintf("%srm -f /etc/systemd/system/%s /etc/systemd/system/%s", sudo, timer, svc))
	executor.Run(ctx, sudo+"systemctl daemon-reload")
	if _, err := executor.Run(ctx, "rm -f "+ssh.ShellQuote(healConfPath(appCfg.App))); err != nil {
		return fmt.Errorf("removing heal manifest: %w", err)
	}
	fmt.Printf("Self-heal disabled for %s.\n", appCfg.App)
	return nil
}

// runHealPass performs one heal pass: probe each opted-in, running web container
// and restart-in-place any that fail, respecting the deploy lock and backoff.
// Returns the number of containers restarted. Pure orchestration over the
// executor so it is testable with a mock.
func runHealPass(ctx context.Context, exec ssh.Executor, app string, out io.Writer) (int, error) {
	cfg, ok, err := readHealConf(ctx, exec, app)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, nil // heal not enabled for this app — nothing to do
	}

	dk := docker.NewClient(exec)
	containers, err := dk.ListContainers(ctx, app)
	if err != nil {
		return 0, err
	}
	known := map[string]bool{}
	for _, p := range cfg.Processes {
		known[p] = true
	}

	hs := readHealState(ctx, exec, app)
	now := time.Now().Unix()
	restarted := 0

	for _, c := range containers {
		if c.State != "running" || c.Labels["teploy.role"] == "accessory" {
			continue
		}
		proc := c.Labels["teploy.process"]
		// Web-only: non-web processes publish no host port, so there's no probe
		// surface (and Docker's restart policy already covers their crashes).
		if proc != "web" || !known[proc] {
			continue
		}

		port, err := dk.HostPort(ctx, c.Name)
		if err != nil || port == 0 {
			continue // can't probe without a published port
		}
		if probeHealthy(ctx, exec, port, cfg.HealthPath) {
			delete(hs, c.Name) // recovered — reset attempt state
			continue
		}

		st := hs[c.Name]
		if st.GaveUp {
			continue
		}
		if st.LastRestart > 0 && now-st.LastRestart < int64(cfg.BackoffSecs) {
			continue // still within backoff window
		}
		if st.Attempts >= cfg.MaxAttempts {
			st.GaveUp = true
			hs[c.Name] = st
			state.AppendLog(ctx, exec, state.LogEntry{
				Timestamp: time.Now().UTC(), App: app, Type: "heal_giveup", Success: false,
				Message: fmt.Sprintf("%s unhealthy after %d restarts — giving up", c.Name, st.Attempts),
			})
			fmt.Fprintf(out, "heal: %s gave up after %d attempts\n", c.Name, st.Attempts)
			continue
		}

		// Acquire the heal lock — yields to any in-progress deploy, never breaks it.
		acquired, err := state.AcquireHealLock(ctx, exec, app)
		if err != nil {
			return restarted, err
		}
		if !acquired {
			continue // a deploy (or another heal) holds the lock — skip this pass
		}
		_, restartErr := exec.Run(ctx, "docker restart "+ssh.ShellQuote(c.Name))
		state.ReleaseLock(ctx, exec, app)

		st.Attempts++
		st.LastRestart = now
		hs[c.Name] = st
		success := restartErr == nil
		if success {
			restarted++
		}
		state.AppendLog(ctx, exec, state.LogEntry{
			Timestamp: time.Now().UTC(), App: app, Type: "heal_restart", Success: success,
			Message: fmt.Sprintf("%s failed health probe — restarted in place (attempt %d)", c.Name, st.Attempts),
		})
		fmt.Fprintf(out, "heal: restarted %s (attempt %d)\n", c.Name, st.Attempts)
	}

	writeHealState(ctx, exec, app, hs)
	return restarted, nil
}

// probeHealthy runs a single host-side health check, mirroring
// internal/deploy/health.go: 200 = healthy; 404/3xx falls back to a TCP check.
func probeHealthy(ctx context.Context, exec ssh.Executor, port int, path string) bool {
	out, err := exec.Run(ctx, fmt.Sprintf("curl -s -o /dev/null -w '%%{http_code}' http://localhost:%d%s", port, path))
	if err != nil {
		return false
	}
	code := strings.TrimSpace(out)
	if code == "200" {
		return true
	}
	if code == "404" || strings.HasPrefix(code, "3") {
		_, terr := exec.Run(ctx, fmt.Sprintf("bash -c '</dev/tcp/localhost/%d' 2>/dev/null", port))
		return terr == nil
	}
	return false
}

func readHealConf(ctx context.Context, exec ssh.Executor, app string) (HealConfig, bool, error) {
	out, err := exec.Run(ctx, "cat "+ssh.ShellQuote(healConfPath(app))+" 2>/dev/null")
	if err != nil || strings.TrimSpace(out) == "" {
		return HealConfig{}, false, nil
	}
	var cfg HealConfig
	if err := json.Unmarshal([]byte(out), &cfg); err != nil {
		return HealConfig{}, false, fmt.Errorf("parsing heal.conf for %s: %w", app, err)
	}
	return cfg.withDefaults(), true, nil
}

func readHealState(ctx context.Context, exec ssh.Executor, app string) map[string]healContainerState {
	hs := map[string]healContainerState{}
	out, err := exec.Run(ctx, "cat "+ssh.ShellQuote(healStatePath(app))+" 2>/dev/null")
	if err != nil || strings.TrimSpace(out) == "" {
		return hs
	}
	_ = json.Unmarshal([]byte(out), &hs)
	return hs
}

func writeHealState(ctx context.Context, exec ssh.Executor, app string, hs map[string]healContainerState) {
	data, _ := json.Marshal(hs)
	_ = exec.Upload(ctx, bytes.NewReader(data), healStatePath(app), "0644")
}
