package cli

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"

	"github.com/spf13/cobra"
	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/openbao"
	"github.com/useteploy/teploy/internal/ssh"
)

func secretAuditConfPath(app string) string {
	return fmt.Sprintf("/deployments/%s/.secret-audit.conf", app)
}
func secretAuditServiceName(app string) string { return "teploy-secret-audit-" + app + ".service" }
func secretAuditTimerName(app string) string   { return "teploy-secret-audit-" + app + ".timer" }

// newSecretAuditCmd groups the audit-trail operations: ship (one-shot forward),
// and enable/disable (a systemd timer that streams continuously).
func newSecretAuditCmd(flags *Flags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "audit",
		Short: "Forward OpenBao secret-access audit into the observe tamper-evident trail",
	}
	cmd.AddCommand(newSecretAuditShipCmd(flags))
	cmd.AddCommand(newSecretAuditEnableCmd(flags))
	cmd.AddCommand(newSecretAuditDisableCmd(flags))
	return cmd
}

func newSecretAuditShipCmd(flags *Flags) *cobra.Command {
	var accessory, app, endpoint, token, site string
	var local bool
	cmd := &cobra.Command{
		Use:   "ship",
		Short: "Forward new OpenBao audit entries to observe (idempotent)",
		Long: `Reads new OpenBao audit-log entries and forwards each secret/database access
into the teploy-observe tamper-evident audit trail. Idempotent — tracks how many
entries were already shipped. --local runs on the host (used by the timer),
reading observe settings from the host config written by 'secret audit enable'.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
			defer cancel()

			if local {
				// Timer path: run on the box with a local executor, reading the
				// observe settings from the host config file.
				if app == "" {
					return fmt.Errorf("--app is required with --local")
				}
				conf, err := readSecretAuditConf(secretAuditConfPath(app))
				if err != nil {
					return err
				}
				exec := ssh.NewLocalExecutor()
				n, err := openbao.NewClient(exec, os.Stdout).ShipAudit(ctx, app, conf["accessory"],
					conf["endpoint"], conf["token"], conf["site"])
				if err != nil {
					return err
				}
				fmt.Printf("Shipped %d audit event(s) to observe\n", n)
				return nil
			}

			// Interactive path: read teploy.yml + connect over SSH. Flags override.
			appCfg, err := config.LoadApp(".")
			if err != nil {
				return err
			}
			executor, err := connectForApp(ctx, flags, appCfg)
			if err != nil {
				return err
			}
			defer executor.Close()
			ep, tk, st := endpoint, token, site
			if ep == "" {
				ep = appCfg.Audit.Endpoint
			}
			if tk == "" {
				tk = appCfg.Audit.Token
			}
			if st == "" {
				st = appCfg.Audit.Site
			}
			n, err := openbao.NewClient(executor, os.Stdout).ShipAudit(ctx, appCfg.App, resolveVaultAccessory(accessory), ep, tk, st)
			if err != nil {
				return err
			}
			fmt.Printf("Shipped %d audit event(s) to observe\n", n)
			return nil
		},
	}
	cmd.Flags().StringVar(&accessory, "accessory", "openbao", "OpenBao accessory name")
	cmd.Flags().BoolVar(&local, "local", false, "run on the host (used by the timer)")
	cmd.Flags().StringVar(&app, "app", "", "app name (required with --local)")
	cmd.Flags().StringVar(&endpoint, "observe-endpoint", "", "observe endpoint (overrides teploy.yml)")
	cmd.Flags().StringVar(&token, "observe-token", "", "observe token (overrides teploy.yml)")
	cmd.Flags().StringVar(&site, "observe-site", "", "observe site (overrides teploy.yml)")
	return cmd
}

func newSecretAuditEnableCmd(flags *Flags) *cobra.Command {
	var accessory string
	var interval int
	var noBinary bool
	cmd := &cobra.Command{
		Use:   "enable",
		Short: "Install a timer that continuously streams secret-access audit to observe",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSecretAuditEnable(flags, resolveVaultAccessory(accessory), interval, noBinary)
		},
	}
	cmd.Flags().StringVar(&accessory, "accessory", "openbao", "OpenBao accessory name")
	cmd.Flags().IntVar(&interval, "interval", 300, "seconds between ships")
	cmd.Flags().BoolVar(&noBinary, "no-binary", false, "assume the teploy binary is already on the host")
	return cmd
}

func runSecretAuditEnable(flags *Flags, accessory string, interval int, noBinary bool) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	appCfg, executor, err := resolveApp(ctx, flags, "")
	if err != nil {
		return err
	}
	defer executor.Close()
	if appCfg.Audit.Endpoint == "" {
		return fmt.Errorf("no observe endpoint — set audit.endpoint (and token) in teploy.yml first")
	}
	if interval < 30 {
		interval = 300
	}

	// 1. Host config (0600) with the observe settings the local ship reads.
	conf := fmt.Sprintf("endpoint=%s\ntoken=%s\nsite=%s\naccessory=%s\n",
		appCfg.Audit.Endpoint, appCfg.Audit.Token, appCfg.Audit.Site, accessory)
	if err := executor.Upload(ctx, strings.NewReader(conf), secretAuditConfPath(appCfg.App), "0600"); err != nil {
		return fmt.Errorf("writing audit config: %w", err)
	}

	// 2. Ensure the teploy binary is on the host.
	if noBinary {
		fmt.Printf("Skipping binary install — assuming teploy is at %s.\n", healBinaryPath)
	} else {
		fmt.Println("Installing teploy binary on the host...")
		if _, err := deployTeployBinaryToServer(ctx, executor, healBinaryPath); err != nil {
			return fmt.Errorf("installing teploy binary (use --no-binary if present): %w", err)
		}
	}

	// 3. Install + start the timer.
	sudo := sudoPrefixFor(ctx, executor)
	if err := installSystemdUnit(ctx, executor, sudo, secretAuditServiceName(appCfg.App), generateSecretAuditService(appCfg.App)); err != nil {
		return err
	}
	if err := installSystemdUnit(ctx, executor, sudo, secretAuditTimerName(appCfg.App), generateSecretAuditTimer(appCfg.App, interval)); err != nil {
		return err
	}
	if _, err := executor.Run(ctx, sudo+"systemctl daemon-reload"); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w", err)
	}
	if _, err := executor.Run(ctx, sudo+"systemctl enable --now "+secretAuditTimerName(appCfg.App)); err != nil {
		return fmt.Errorf("enabling audit timer: %w", err)
	}
	fmt.Printf("Audit streaming enabled for %s: every %ds -> %s.\n", appCfg.App, interval, appCfg.Audit.Endpoint)
	return nil
}

func newSecretAuditDisableCmd(flags *Flags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "disable",
		Short: "Remove the audit-streaming timer",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
			defer cancel()
			appCfg, executor, err := resolveApp(ctx, flags, "")
			if err != nil {
				return err
			}
			defer executor.Close()
			sudo := sudoPrefixFor(ctx, executor)
			timer := secretAuditTimerName(appCfg.App)
			executor.Run(ctx, fmt.Sprintf("%ssystemctl disable --now %s 2>/dev/null", sudo, timer))
			executor.Run(ctx, fmt.Sprintf("%srm -f /etc/systemd/system/%s /etc/systemd/system/%s", sudo, timer, secretAuditServiceName(appCfg.App)))
			executor.Run(ctx, sudo+"systemctl daemon-reload")
			fmt.Printf("Audit streaming disabled for %s.\n", appCfg.App)
			return nil
		},
	}
	return cmd
}

func generateSecretAuditService(app string) string {
	return fmt.Sprintf(`[Unit]
Description=Teploy secret audit shipper for %s
After=docker.service
Requires=docker.service

[Service]
Type=oneshot
ExecStart=%s secret audit ship --local --app %s
`, app, healBinaryPath, app)
}

func generateSecretAuditTimer(app string, intervalSec int) string {
	return fmt.Sprintf(`[Unit]
Description=Teploy secret audit shipper timer for %s

[Timer]
OnBootSec=%ds
OnUnitActiveSec=%ds
AccuracySec=15s

[Install]
WantedBy=timers.target
`, app, intervalSec, intervalSec)
}

// readSecretAuditConf parses the key=value host config written by enable.
func readSecretAuditConf(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("reading audit config (run 'teploy secret audit enable'): %w", err)
	}
	defer f.Close()
	conf := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), "="); ok {
			conf[k] = v
		}
	}
	if conf["accessory"] == "" {
		conf["accessory"] = "openbao"
	}
	return conf, nil
}

