package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/spf13/cobra"
	"github.com/useteploy/teploy/internal/autodeploy"
	"github.com/useteploy/teploy/internal/build"
	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/docker"
	"github.com/useteploy/teploy/internal/ssh"
	"github.com/useteploy/teploy/internal/state"
)

// newAutoDeployServeCmd is the resident webhook listener `teploy autodeploy
// setup` installs as a systemd unit (see internal/autodeploy/autodeploy.go's
// Setup) — not meant to be run directly by a human. It replaces the
// previous bash+netcat listener entirely: HMAC verification, replay
// dedup, and the actual deploy all happen in this one Go process instead
// of a generated shell script that never called anything resembling a
// real deploy at all (see the autodeploy rebuild for what that cost).
func newAutoDeployServeCmd() *cobra.Command {
	var (
		app    string
		branch string
		port   int
	)

	cmd := &cobra.Command{
		Use:    "serve",
		Short:  "Run the auto-deploy webhook listener (invoked by systemd — not for interactive use)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAutoDeployServe(app, branch, port)
		},
	}

	cmd.Flags().StringVar(&app, "app", "", "app name (required)")
	cmd.Flags().StringVar(&branch, "branch", "main", "branch to watch for pushes")
	cmd.Flags().IntVar(&port, "port", 9876, "port to listen on — 0.0.0.0, reachable from Caddy's docker bridge network; every request still requires a valid HMAC signature")
	cmd.MarkFlagRequired("app")

	return cmd
}

func runAutoDeployServe(app, branch string, port int) error {
	if err := config.ValidateName(app); err != nil {
		return err
	}
	if err := autodeploy.ValidateBranch(branch); err != nil {
		return err
	}

	secretBytes, err := os.ReadFile(autodeploy.SecretPath(app))
	if err != nil {
		return fmt.Errorf("reading webhook secret from %s (run `teploy autodeploy setup` first): %w", autodeploy.SecretPath(app), err)
	}
	secret := string(secretBytes)

	logPath := fmt.Sprintf("/deployments/%s/autodeploy.log", app)
	logFile, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("opening log file %s: %w", logPath, err)
	}
	defer logFile.Close()
	out := io.MultiWriter(os.Stdout, logFile)

	// Restore delivery dedup state across restarts (best-effort — losing
	// this on a restart just means a very recent replay could briefly slip
	// through, not a hard failure).
	dedupPath := fmt.Sprintf("/deployments/%s/.autodeploy-dedup.json", app)
	dedupData, _ := os.ReadFile(dedupPath)
	dedup := autodeploy.LoadDeliveryDedup(dedupData)

	executor := ssh.NewLocalExecutor()
	buildDir := autodeploy.BuildDir(app)

	logf := func(format string, args ...any) {
		fmt.Fprintf(out, "%s "+format+"\n", append([]any{time.Now().UTC().Format(time.RFC3339)}, args...)...)
	}

	handler := newWebhookHandler(webhookHandlerConfig{
		secret: secret,
		dedup:  dedup,
		logf:   logf,
		onDedupChanged: func() {
			if snap, err := dedup.Snapshot(); err == nil {
				_ = os.WriteFile(dedupPath, snap, 0600)
			}
		},
		trigger: func() {
			// Deploy asynchronously so the webhook response isn't held
			// open for a potentially multi-minute build — matches
			// providers' expectation of a prompt response, without
			// needing the old bash listener's `nohup ... &` detached-
			// process trick.
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
				defer cancel()
				if err := triggerAutoDeploy(ctx, executor, app, branch, buildDir, out); err != nil {
					logf("deploy failed: %v", err)
				} else {
					logf("deploy complete")
				}
			}()
		},
	})

	mux := http.NewServeMux()
	mux.HandleFunc("/", handler)

	// Caddy runs as its own container on the "teploy" bridge network, a
	// separate network namespace from this host process — 127.0.0.1 would
	// only be reachable from other processes on the host itself, never
	// from inside a container. 0.0.0.0 exposes this beyond just Caddy (to
	// the docker bridge subnet, and to the LAN if the firewall doesn't
	// block 9876), but every request is HMAC-signature-verified regardless
	// of source, which is the actual security boundary here.
	addr := fmt.Sprintf("0.0.0.0:%d", port)
	logf("teploy autodeploy serve listening on %s for app %s (branch %s)", addr, app, branch)
	return http.ListenAndServe(addr, mux)
}

// webhookHandlerConfig holds newWebhookHandler's dependencies, injected
// rather than closed over directly so the request-handling logic (HMAC
// verification, dedup, response codes) is unit-testable with
// httptest.NewRecorder without touching the filesystem or triggering a
// real deploy.
type webhookHandlerConfig struct {
	secret string
	dedup  *autodeploy.DeliveryDedup
	logf   func(format string, args ...any)
	// onDedupChanged is called after a new (non-replayed) delivery ID is
	// recorded, so the caller can persist the dedup snapshot. Optional.
	onDedupChanged func()
	// trigger is called exactly once per accepted, non-replayed webhook —
	// the actual deploy kickoff. Never called for a rejected or replayed
	// request.
	trigger func()
}

// newWebhookHandler returns the HTTP handler for the webhook endpoint:
// verifies the request (GitHub HMAC or GitLab token, whichever header is
// present), rejects invalid or replayed deliveries, and calls cfg.trigger
// exactly once for anything else.
func newWebhookHandler(cfg webhookHandlerConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		// Cap body size — this is a webhook payload (a git push event),
		// not a file upload; nothing legitimate is anywhere near this size.
		body, err := io.ReadAll(io.LimitReader(r.Body, 10<<20))
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		// Accept either GitHub's HMAC-signed style or GitLab's shared-token
		// style, whichever header is present — see webhook.go.
		sigHeader := r.Header.Get("X-Hub-Signature-256")
		gitlabToken := r.Header.Get("X-Gitlab-Token")
		deliveryID := r.Header.Get("X-GitHub-Delivery")
		if deliveryID == "" {
			deliveryID = r.Header.Get("X-Gitlab-Event-UUID")
		}

		valid := false
		switch {
		case sigHeader != "":
			valid = autodeploy.VerifyGitHubSignature(cfg.secret, body, sigHeader)
		case gitlabToken != "":
			valid = autodeploy.VerifyGitLabToken(cfg.secret, gitlabToken)
		}
		if !valid {
			w.WriteHeader(http.StatusUnauthorized)
			if cfg.logf != nil {
				cfg.logf("rejected webhook: invalid or missing signature")
			}
			return
		}

		if deliveryID != "" && cfg.dedup.SeenAndRecord(deliveryID) {
			// 200, not an error status — this is a provider retry/replay
			// of a delivery we already handled, an intentional no-op, not
			// a failure the provider should retry harder on.
			w.WriteHeader(http.StatusOK)
			if cfg.logf != nil {
				cfg.logf("ignored replayed delivery %s", deliveryID)
			}
			return
		}
		if deliveryID != "" && cfg.onDedupChanged != nil {
			cfg.onDedupChanged()
		}

		w.WriteHeader(http.StatusOK)
		if cfg.logf != nil {
			cfg.logf("accepted webhook, triggering deploy")
		}
		if cfg.trigger != nil {
			cfg.trigger()
		}
	}
}

// triggerAutoDeploy fetches the watched branch, builds, and deploys —
// calling deployBuiltImage, the exact same post-build orchestration
// `teploy deploy` uses (see deploy.go), so this can never silently drift
// from what a manual deploy does the way the old generated bash script did
// (which never called anything resembling a real deploy at all).
//
// buildDir must already be a valid git checkout with a fetchable `origin`
// remote — `teploy autodeploy setup` clones it there once
// (ensureServerBuildCheckout, in autodeploy.go) as part of setup, using the
// operator's local git remote. That step is best-effort (a private repo
// needs credentials already configured for the server's user, or it's
// skipped with a warning), so this can still fail on a server that was
// never successfully cloned.
func triggerAutoDeploy(ctx context.Context, executor ssh.Executor, app, branch, buildDir string, out io.Writer) error {
	if err := state.AcquireLock(ctx, executor, app); err != nil {
		return fmt.Errorf("acquiring deploy lock: %w", err)
	}
	defer state.ReleaseLockDetached(executor, app)

	if _, err := executor.Run(ctx, "mkdir -p "+ssh.ShellQuote(buildDir)); err != nil {
		return fmt.Errorf("creating build directory: %w", err)
	}
	fetchCmd := fmt.Sprintf("cd %s && git fetch origin %s && git reset --hard origin/%s",
		ssh.ShellQuote(buildDir), ssh.ShellQuote(branch), ssh.ShellQuote(branch))
	if _, err := executor.Run(ctx, fetchCmd); err != nil {
		return fmt.Errorf("fetching %s (is %s a valid git checkout with a fetchable 'origin' remote? this must exist before the first webhook-triggered deploy — see `teploy deploy`'s server-build mode, or clone it manually): %w",
			branch, buildDir, err)
	}

	appCfg, err := config.LoadApp(buildDir)
	if err != nil {
		return fmt.Errorf("loading teploy.yml from %s: %w", buildDir, err)
	}
	if appCfg.App != app {
		return fmt.Errorf("teploy.yml in %s declares app %q, expected %q — refusing to deploy the wrong app", buildDir, appCfg.App, app)
	}
	if appCfg.IsStatic() {
		return fmt.Errorf("type:static apps are not supported by `teploy autodeploy serve` yet — use a scheduled/manual deploy")
	}

	version, err := gitShortHashIn(buildDir)
	if err != nil {
		return fmt.Errorf("resolving version: %w", err)
	}

	var image string
	needsBuild := appCfg.Image == ""
	if needsBuild {
		buildMode := build.Detect(buildDir)
		fmt.Fprintf(out, "Building image on server (%s)...\n", buildMode)
		builder := build.NewBuilder(executor, out)
		image, err = builder.Build(ctx, build.BuildConfig{
			App:      app,
			Version:  version,
			Mode:     buildMode,
			BuildDir: buildDir,
			Platform: appCfg.Platform,
		})
		if err != nil {
			return fmt.Errorf("building image: %w", err)
		}
	} else {
		image = appCfg.Image
		fmt.Fprintf(out, "Pulling image %s...\n", image)
		dk := docker.NewClient(executor)
		if err := dk.Pull(ctx, image); err != nil {
			return fmt.Errorf("pulling image: %w", err)
		}
	}

	return deployBuiltImage(ctx, executor, appCfg, image, version, "localhost", false, needsBuild)
}
