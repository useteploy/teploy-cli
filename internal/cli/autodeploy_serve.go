package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"github.com/useteploy/teploy/internal/autodeploy"
	"github.com/useteploy/teploy/internal/build"
	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/docker"
	"github.com/useteploy/teploy/internal/env"
	"github.com/useteploy/teploy/internal/releasemeta"
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
		app       string
		branch    string
		port      int
		strictEnv bool
	)

	cmd := &cobra.Command{
		Use:    "serve",
		Short:  "Run the auto-deploy webhook listener (invoked by systemd — not for interactive use)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// The systemd unit cannot easily grow a flag retroactively;
			// the environment variable is the config surface for already
			// installed units (set TEPLOY_STRICT_ENV=1 in the unit file).
			if os.Getenv("TEPLOY_STRICT_ENV") == "1" {
				strictEnv = true
			}
			return runAutoDeployServe(app, branch, port, strictEnv)
		},
	}

	cmd.Flags().StringVar(&app, "app", "", "app name (required)")
	cmd.Flags().StringVar(&branch, "branch", "main", "branch to watch for pushes")
	cmd.Flags().IntVar(&port, "port", 9876, "port to listen on — 0.0.0.0, reachable from Caddy's docker bridge network; every request still requires a valid HMAC signature")
	cmd.Flags().BoolVar(&strictEnv, "strict-env", false, "strict env mode: fail the deploy when env: references an unset ${VAR} (also enabled by TEPLOY_STRICT_ENV=1)")
	cmd.MarkFlagRequired("app")

	return cmd
}

func runAutoDeployServe(app, branch string, port int, strictEnv bool) error {
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
	// The stored bytes are the HMAC key, used VERBATIM (audit T32): setup
	// rejects whitespace-wrapped secrets, so silently trimming here would
	// sign with different bytes than a hand-edited file actually contains
	// and turn a visible configuration mistake into an unexplained 401.
	if len(secretBytes) == 0 {
		// An empty secret authenticates nothing (any unsigned request would
		// compare equal) — fail closed at startup (audit F43).
		return fmt.Errorf("webhook secret at %s is empty — every request would be unauthenticated; re-run `teploy autodeploy setup`", autodeploy.SecretPath(app))
	}
	secret := string(secretBytes)
	if secret != strings.TrimSpace(secret) {
		return fmt.Errorf("webhook secret at %s has leading/trailing whitespace — the HMAC key is used verbatim, so verification would fail against a provider sending the trimmed value; fix the file (or re-run `teploy autodeploy setup`)", autodeploy.SecretPath(app))
	}

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

	// Dedup persistence is serialized AND atomic (audit A36): two
	// concurrent requests used to snapshot and os.WriteFile the same file
	// independently, so an older snapshot could overwrite a newer one,
	// overlapping writes could truncate, and every error was ignored.
	var dedupMu sync.Mutex
	handler := newWebhookHandler(webhookHandlerConfig{
		secret: secret,
		branch: branch,
		dedup:  dedup,
		logf:   logf,
		onDedupChanged: func() {
			dedupMu.Lock()
			defer dedupMu.Unlock()
			snap, err := dedup.Snapshot()
			if err != nil {
				logf("could not snapshot webhook dedup state: %v", err)
				return
			}
			tmp := dedupPath + ".tmp"
			if err := os.WriteFile(tmp, snap, 0600); err != nil {
				logf("could not persist webhook dedup state: %v", err)
				return
			}
			if err := os.Rename(tmp, dedupPath); err != nil {
				logf("could not publish webhook dedup state: %v", err)
			}
		},
		trigger: func(changedFiles []string, filesKnown bool) {
			// Deploy asynchronously so the webhook response isn't held
			// open for a potentially multi-minute build — matches
			// providers' expectation of a prompt response, without
			// needing the old bash listener's `nohup ... &` detached-
			// process trick.
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
				defer cancel()
				if err := triggerAutoDeploy(ctx, executor, app, branch, buildDir, out, changedFiles, filesKnown, strictEnv); err != nil {
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
	// Explicit limits (audit F43): the default server has no read/header/
	// write/idle timeouts, so slow-loris style connections could hold
	// sockets open indefinitely. Every request remains HMAC-verified.
	server := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    32 << 10,
	}
	return server.ListenAndServe()
}

// webhookHandlerConfig holds newWebhookHandler's dependencies, injected
// rather than closed over directly so the request-handling logic (HMAC
// verification, dedup, response codes) is unit-testable with
// httptest.NewRecorder without touching the filesystem or triggering a
// real deploy.
type webhookHandlerConfig struct {
	secret string
	// branch is the ref this listener watches; only push events for it may
	// trigger a deploy (audit F40). Empty accepts any push (tests).
	branch string
	dedup  *autodeploy.DeliveryDedup
	logf   func(format string, args ...any)
	// onDedupChanged is called after a new (non-replayed) delivery ID is
	// recorded, so the caller can persist the dedup snapshot. Optional.
	onDedupChanged func()
	// trigger is called exactly once per accepted, non-replayed webhook —
	// the actual deploy kickoff. Never called for a rejected or replayed
	// request. It receives the files the push touched and whether that set
	// is reliable (see autodeploy.ChangedFiles); the deploy step uses them
	// for monorepo path filtering.
	trigger func(changedFiles []string, filesKnown bool)
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

		// Cap body size with MaxBytesReader so an oversized body gets a
		// real 413 instead of a silently TRUNCATED read that then failed
		// HMAC verification (audit F43).
		r.Body = http.MaxBytesReader(w, r.Body, 10<<20)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
				return
			}
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

		// Replay protection is keyed on the AUTHENTICATED CONTENT, not the
		// unauthenticated delivery-ID header: a captured signed body could
		// be replayed under a fresh (or absent) delivery ID and bypass an
		// ID-only dedup (audit F41). Every legitimate provider retry
		// replays the SAME signed body, so content dedup covers them all;
		// the delivery ID is kept as log metadata only — a REUSED delivery
		// ID carrying different authenticated content used to suppress a
		// distinct event (audit A36).
		contentSum := sha256.Sum256(body)
		contentID := "content:" + hex.EncodeToString(contentSum[:])
		if cfg.dedup.SeenAndRecord(contentID) {
			w.WriteHeader(http.StatusOK)
			if cfg.logf != nil {
				cfg.logf("ignored replayed webhook content")
			}
			return
		}
		if cfg.onDedupChanged != nil {
			cfg.onDedupChanged()
		}

		// Bind the deploy to THIS event: only a push to the watched branch
		// may trigger it; pings, tags, other branches, and branch deletions
		// are acknowledged no-ops (audit F40).
		if !autodeploy.PushEvent(body, cfg.branch) {
			w.WriteHeader(http.StatusAccepted)
			if cfg.logf != nil {
				cfg.logf("accepted webhook (not a push to %s) — no deploy", cfg.branch)
			}
			return
		}

		w.WriteHeader(http.StatusOK)
		if cfg.logf != nil {
			if deliveryID != "" {
				cfg.logf("accepted webhook (delivery %s), triggering deploy", deliveryID)
			} else {
				cfg.logf("accepted webhook, triggering deploy")
			}
		}
		if cfg.trigger != nil {
			// Parse the changed-file set from the push body for monorepo
			// path filtering. filesKnown=false (unknown provider, truncated
			// or tag/ping payload) means the deploy step must not skip.
			changedFiles, filesKnown := autodeploy.ChangedFiles(body)
			cfg.trigger(changedFiles, filesKnown)
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
func triggerAutoDeploy(ctx context.Context, executor ssh.Executor, app, branch, buildDir string, out io.Writer, changedFiles []string, filesKnown, strictEnv bool) error {
	// The lock's parent must exist before it can be acquired — a server
	// whose app was never manually deployed has no /deployments/<app> yet.
	if err := state.EnsureAppDir(ctx, executor, app); err != nil {
		return fmt.Errorf("creating app directory: %w", err)
	}
	// Fenced acquisition (F16): the lease spans fetch → build → deploy, and
	// the deploy's effect sites verify the fence.
	lk, err := state.AcquireLockFenced(ctx, executor, app)
	if err != nil {
		return fmt.Errorf("acquiring deploy lock: %w", err)
	}
	defer state.ReleaseLockFenced(executor, lk, app)
	lk.StartRenewal(executor)

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

	// Local file references resolve against the CHECKOUT, not the resident
	// process's working directory (audit T31): the systemd unit has no
	// WorkingDirectory, so a relative tls.cert/key that worked in a manual
	// invocation resolved against "/" under the service and read the wrong
	// file (or nothing). Absolute paths are preserved as-is.
	if appCfg.TLS != nil && !appCfg.TLS.Internal {
		appCfg.TLS = resolveTLSFromRoot(appCfg.TLS, buildDir)
	}

	// Resolve env_files from the CHECKOUT, with the same single-pass ${VAR}
	// expansion rule manual deploys use (audit F59/F66): without this, the
	// same manifest received different container env depending on whether
	// the deploy was manual or webhook-triggered — encrypted-file secrets
	// simply went missing on the webhook path. strictEnv (F57/TCL-32)
	// makes unset variables fail loudly here too.
	if err := expandEnvTemplates(appCfg.Env, strictEnv); err != nil {
		return err
	}
	if len(appCfg.EnvFiles) > 0 {
		fileVars, err := env.LoadLocalEnvFiles(ctx, buildDir, appCfg.EnvFiles)
		if err != nil {
			return fmt.Errorf("resolving env_files from %s: %w", buildDir, err)
		}
		if appCfg.Env == nil {
			appCfg.Env = map[string]string{}
		}
		for k, v := range fileVars {
			if _, explicit := appCfg.Env[k]; !explicit {
				appCfg.Env[k] = v
			}
		}
	}

	// Monorepo path filter: if autodeploy.paths is set and we have a
	// reliable changed-file list, skip the deploy when nothing under those
	// paths changed. Fail open (deploy) whenever the file set is unknown —
	// we never want to silently skip a real change.
	if appCfg.Autodeploy != nil && len(appCfg.Autodeploy.Paths) > 0 {
		if !filesKnown {
			fmt.Fprintln(out, "autodeploy.paths set, but the push payload had no reliable file list — deploying to be safe.")
		} else if !autodeploy.PathMatches(appCfg.Autodeploy.Paths, changedFiles) {
			fmt.Fprintf(out, "Skipping deploy: no changed files match autodeploy.paths (%s).\n", strings.Join(appCfg.Autodeploy.Paths, ", "))
			return nil
		}
	}

	if appCfg.IsStatic() {
		return fmt.Errorf("type:static apps are not supported by `teploy autodeploy serve` yet — use a scheduled/manual deploy")
	}

	version, err := gitShortHashIn(buildDir)
	if err != nil {
		return fmt.Errorf("resolving version: %w", err)
	}
	if revision, revisionErr := gitRevisionIn(buildDir); revisionErr == nil {
		appCfg.SourceRevision = revision
	}

	var image string
	needsBuild := appCfg.Image == ""
	if needsBuild {
		buildMode, detectErr := build.DetectAt(buildDir, appCfg.Dockerfile)
		if detectErr != nil {
			return detectErr
		}
		fmt.Fprintf(out, "Building image on server (%s)...\n", buildMode)
		builder := build.NewBuilder(executor, out)
		image, err = builder.Build(ctx, build.BuildConfig{
			App:        app,
			Version:    version,
			Mode:       buildMode,
			BuildDir:   buildDir,
			Context:    appCfg.Context,
			Dockerfile: appCfg.Dockerfile,
			Platform:   appCfg.Platform,
		})
		if err != nil {
			return fmt.Errorf("building image: %w", err)
		}
	} else {
		image = appCfg.Image
		// Same image policy as manual deploys (digest-pinned cache reuse,
		// fresh pull for mutable tags, warned local fallback).
		if err := ensureImage(ctx, docker.NewClient(executor), image, out); err != nil {
			return err
		}
	}

	// The outer lock taken at the top of triggerAutoDeploy is still held —
	// route through the fenced entry point so Deploy doesn't deadlock on its
	// own second acquisition (audit F07), passing the fence handle (F16) and
	// the attempt that keys this deploy's env/TLS artifacts (F08).
	att := releasemeta.MustAttempt(app, version)
	return deployBuiltImageFenced(ctx, executor, appCfg, image, version, "localhost", false, needsBuild, lk, &att)
}

// resolveTLSFromRoot returns a COPY of tls with relative cert/key paths
// resolved against root (audit T31) — the resident autodeploy process runs
// under systemd with no WorkingDirectory, so relative paths must never be
// interpreted against whatever cwd it inherited.
func resolveTLSFromRoot(tls *config.TLSConfig, root string) *config.TLSConfig {
	resolve := func(p string) string {
		if p == "" || filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(root, p)
	}
	out := *tls
	out.Cert = resolve(out.Cert)
	out.Key = resolve(out.Key)
	return &out
}
