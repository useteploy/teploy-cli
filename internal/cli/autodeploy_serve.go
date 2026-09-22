package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	// through, not a hard failure; the admission ledger below reseeds it).
	dedupPath := fmt.Sprintf("/deployments/%s/.autodeploy-dedup.json", app)
	dedupData, _ := os.ReadFile(dedupPath)
	dedup := autodeploy.LoadDeliveryDedup(dedupData)

	executor := ssh.NewLocalExecutor()
	buildDir := autodeploy.BuildDir(app)

	logf := func(format string, args ...any) {
		fmt.Fprintf(out, "%s "+format+"\n", append([]any{time.Now().UTC().Format(time.RFC3339)}, args...)...)
	}

	// The admission ledger (C02): every 200 this process sends is backed by
	// an fsynced record here, and restarts replay admitted-but-never-
	// processed deliveries from it.
	ledgerPath := fmt.Sprintf("/deployments/%s/.autodeploy-ledger.jsonl", app)
	ledger, err := autodeploy.OpenLedger(ledgerPath)
	if err != nil {
		return err
	}
	defer ledger.Close()

	// Bounded queueing: one worker, one newest-wins pending slot. The
	// deploy itself runs in the worker — never a goroutine per delivery.
	queue := newAdmissionQueue(ledger, func(changedFiles []string, filesKnown bool, commit string) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		if err := triggerAutoDeploy(ctx, executor, app, branch, buildDir, commit, out, changedFiles, filesKnown, strictEnv); err != nil {
			logf("deploy failed: %v", err)
		} else {
			logf("deploy complete")
		}
	}, logf)

	if err := resumeAdmissions(ledgerPath, app, ledger, queue, dedup, logf); err != nil {
		return fmt.Errorf("resuming webhook admissions: %w", err)
	}

	// Dedup persistence is serialized AND atomic (audit A36): two
	// concurrent requests used to snapshot and os.WriteFile the same file
	// independently, so an older snapshot could overwrite a newer one,
	// overlapping writes could truncate, and every error was ignored.
	// It stays best-effort: the admission LEDGER is the durable record;
	// a lost dedup snapshot degrades to one redundant deploy of the
	// branch tip, never a lost admission.
	var dedupMu sync.Mutex
	handler := newWebhookHandler(webhookHandlerConfig{
		secret: secret,
		branch: branch,
		app:    app,
		dedup:  dedup,
		ledger: ledger,
		queue:  queue,
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
// verification, dedup, admission durability, response codes) is
// unit-testable with httptest without touching the filesystem or
// triggering a real deploy.
type webhookHandlerConfig struct {
	secret string
	// branch is the ref this listener watches; only push events for it may
	// trigger a deploy (audit F40). Empty accepts any push (tests).
	branch string
	// app names the admissions written to the ledger (one serve process
	// per app).
	app   string
	dedup *autodeploy.DeliveryDedup
	logf  func(format string, args ...any)
	// ledger is the durable admission record (C02): the handler acks 200
	// only after the fsynced append of the admission succeeds.
	ledger autodeploy.LedgerAppender
	// queue holds the bounded one-running-plus-one-pending deploy slot.
	queue *admissionQueue
	// onDedupChanged is called after a delivery is DURABLY admitted, so
	// the caller can persist the dedup snapshot. Best-effort. Optional.
	onDedupChanged func()
}

// newWebhookHandler returns the HTTP handler for the webhook endpoint:
// verifies the request (GitHub HMAC or GitLab token, whichever header is
// present), rejects invalid or replayed deliveries, durably admits pushes
// to the watched branch (200 only after the admission record is fsynced —
// C02), and enqueues them on the bounded queue. A persistence failure is
// a 503 + Retry-After: never ack what isn't durable.
func newWebhookHandler(cfg webhookHandlerConfig) http.HandlerFunc {
	writeJSON := func(w http.ResponseWriter, code int, v any) {
		body, err := json.Marshal(v)
		if err != nil {
			w.WriteHeader(code)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		w.Write(body)
	}
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
		digest := hex.EncodeToString(contentSum[:])
		contentID := autodeploy.ContentIDFromDigest(digest)
		if cfg.dedup.SeenAndRecord(contentID) {
			writeJSON(w, http.StatusOK, admissionResponse{Status: "duplicate"})
			if cfg.logf != nil {
				cfg.logf("ignored replayed webhook content")
			}
			return
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

		// Parse the changed-file set from the push body for monorepo path
		// filtering. filesKnown=false (unknown provider, truncated or
		// tag/ping payload) means the deploy step must not skip.
		changedFiles, filesKnown := autodeploy.ChangedFiles(body)

		// Bind the deploy to the AUTHENTICATED COMMIT (C02): the payload's
		// after/checkout_sha names the exact commit this event built; the
		// fetch pins the checkout to it instead of the moving tip.
		commit := autodeploy.PushCommit(body)

		// DURABLE ADMISSION (C02): the record is fsynced BEFORE the 200.
		// A crash immediately after the response still leaves the admitted
		// delivery discoverable by restart resume.
		rec := autodeploy.AdmissionRecord{
			Kind:     autodeploy.AdmissionKindAdmitted,
			ID:       autodeploy.NewAdmissionID(),
			Delivery: deliveryID,
			Digest:   digest,
			App:      cfg.app,
			Branch:   cfg.branch,
			Commit:   commit,
			Received: time.Now().UTC(),
		}
		if err := cfg.ledger.Append(rec); err != nil {
			// Not durable → never ack. Un-record the dedup entry so the
			// provider's retry of the SAME signed body goes through
			// admission again instead of being swallowed as a replay of
			// something that was never admitted.
			cfg.dedup.Unrecord(contentID)
			if cfg.logf != nil {
				cfg.logf("admission not durable, rejecting (provider should retry): %v", err)
			}
			w.Header().Set("Retry-After", "5")
			writeJSON(w, http.StatusServiceUnavailable, admissionResponse{Status: "error", Error: "admission could not be made durable"})
			return
		}
		if cfg.onDedupChanged != nil {
			cfg.onDedupChanged()
		}

		disposition := cfg.queue.admit(rec, changedFiles, filesKnown)
		writeJSON(w, http.StatusOK, admissionResponse{Status: "admitted", Disposition: string(disposition)})
		if cfg.logf != nil {
			if deliveryID != "" {
				cfg.logf("accepted webhook (delivery %s), admission %s %s", deliveryID, rec.ID, disposition)
			} else {
				cfg.logf("accepted webhook, admission %s %s", rec.ID, disposition)
			}
		}
	}
}

// admissionResponse is the small JSON body on admission replies so the
// provider (and tests) can tell what happened: running (deploy starting
// now), queued (one deploy running, this one is the pending newest),
// superseded (replaced an older pending delivery), duplicate (replay).
type admissionResponse struct {
	Status      string `json:"status"`
	Disposition string `json:"disposition,omitempty"`
	Error       string `json:"error,omitempty"`
}

// admissionDisposition reports what the bounded queue did with a durably
// admitted delivery.
type admissionDisposition string

const (
	dispositionRunning    admissionDisposition = "running"
	dispositionQueued     admissionDisposition = "queued"
	dispositionSuperseded admissionDisposition = "superseded"
)

// queuedAdmission is PENDING WORK AS A RECORD, never a blocked goroutine:
// the single worker picks it up when the running deploy finishes. The
// authenticated commit rides on the record (rec.Commit), so a resumed
// admission stays pinned to its delivery's commit across a restart.
type queuedAdmission struct {
	rec          autodeploy.AdmissionRecord
	changedFiles []string
	filesKnown   bool
}

// admissionQueue is the bounded webhook deploy queue (C02): at most ONE
// running deploy plus ONE pending slot per app, newest-wins. A delivery
// arriving while both are busy SUPERSEDES the queued one (the running
// deploy is never cancelled mid-flight — cancellation propagation is
// deliberately out of scope; the newest deploy runs next instead).
type admissionQueue struct {
	mu         sync.Mutex
	workerLive bool
	pending    *queuedAdmission

	ledger autodeploy.LedgerAppender
	run    func(changedFiles []string, filesKnown bool, commit string)
	logf   func(format string, args ...any)
}

func newAdmissionQueue(ledger autodeploy.LedgerAppender, run func(changedFiles []string, filesKnown bool, commit string), logf func(format string, args ...any)) *admissionQueue {
	return &admissionQueue{ledger: ledger, run: run, logf: logf}
}

// admit places a durably admitted delivery on the queue and reports the
// disposition. Called from the request goroutine after the ledger append.
func (q *admissionQueue) admit(rec autodeploy.AdmissionRecord, changedFiles []string, filesKnown bool) admissionDisposition {
	q.mu.Lock()
	defer q.mu.Unlock()
	item := &queuedAdmission{rec: rec, changedFiles: changedFiles, filesKnown: filesKnown}
	switch {
	case q.workerLive && q.pending != nil:
		old := q.pending
		q.pending = item
		q.markSupersededLocked(old.rec, rec.ID)
		return dispositionSuperseded
	case q.workerLive:
		q.pending = item
		return dispositionQueued
	default:
		q.pending = item
		q.workerLive = true
		go q.worker()
		return dispositionRunning
	}
}

// worker is the ONLY deploy runner: one goroutine at a time, draining the
// pending slot. It exits when the queue is empty; the next admit restarts
// it — so rapid deliveries during a long deploy never spawn per-delivery
// goroutines.
func (q *admissionQueue) worker() {
	for {
		q.mu.Lock()
		item := q.pending
		if item == nil {
			q.workerLive = false
			q.mu.Unlock()
			return
		}
		q.pending = nil
		q.mu.Unlock()

		if q.run != nil {
			q.run(item.changedFiles, item.filesKnown, item.rec.Commit)
		}
		q.markProcessed(item.rec)
	}
}

// markSupersededLocked records the newest-wins replacement in the ledger.
// Best-effort: a failed mark leaves both records pending, and resume's
// newest-per-app rule still picks the newer one — the event is not lost.
func (q *admissionQueue) markSupersededLocked(old autodeploy.AdmissionRecord, byID string) {
	err := q.ledger.Append(autodeploy.AdmissionRecord{
		Kind:         autodeploy.AdmissionKindSuperseded,
		ID:           old.ID,
		Delivery:     old.Delivery,
		Digest:       old.Digest,
		App:          old.App,
		Branch:       old.Branch,
		Commit:       old.Commit,
		Received:     old.Received,
		SupersededBy: byID,
		At:           time.Now().UTC(),
	})
	if err != nil && q.logf != nil {
		q.logf("could not mark admission %s superseded: %v (resume still picks the newest)", old.ID, err)
	}
}

// markProcessed records completion in the ledger so restart resume never
// re-triggers a finished deploy. Best-effort: a failed mark can replay ONE
// deploy of the branch tip on resume — idempotent at the engine (fetch +
// same-version redeploy), never a lost event.
func (q *admissionQueue) markProcessed(rec autodeploy.AdmissionRecord) {
	err := q.ledger.Append(autodeploy.AdmissionRecord{
		Kind:     autodeploy.AdmissionKindProcessed,
		ID:       rec.ID,
		Delivery: rec.Delivery,
		Digest:   rec.Digest,
		App:      rec.App,
		Branch:   rec.Branch,
		Commit:   rec.Commit,
		Received: rec.Received,
		At:       time.Now().UTC(),
	})
	if err != nil && q.logf != nil {
		q.logf("could not mark admission %s processed: %v (resume may replay this deploy once)", rec.ID, err)
	}
}

// snapshot exposes the bounded state for tests and diagnostics.
func (q *admissionQueue) snapshot() (workerLive bool, pending *queuedAdmission) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.workerLive, q.pending
}

// resumeAdmissions replays admitted-but-never-processed deliveries from
// the ledger after a restart (C02): newest per app wins, older pendings
// are marked superseded, and the recent admitted digests reseed the replay
// dedup (the dedup file is best-effort). This is the correct webhook
// contract — the provider will not redeliver an event it was told was
// accepted, so replaying admitted-not-processed work IS the job (unlike a
// UI admission write, where replay is the bug).
func resumeAdmissions(ledgerPath, app string, ledger autodeploy.LedgerAppender, q *admissionQueue, dedup *autodeploy.DeliveryDedup, logf func(format string, args ...any)) error {
	data, err := os.ReadFile(ledgerPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("reading webhook admission ledger: %w", err)
	}
	recs, err := autodeploy.ParseLedger(data)
	if err != nil {
		return err
	}
	pending, digests := autodeploy.FoldAdmissions(recs)
	autodeploy.SeedDedupFromLedger(dedup, digests, time.Now().UTC())
	newest := autodeploy.NewestPending(pending, app)
	if newest == nil {
		return nil
	}
	for _, p := range pending {
		if p.App != app || p.ID == newest.ID {
			continue
		}
		if err := ledger.Append(autodeploy.AdmissionRecord{
			Kind:         autodeploy.AdmissionKindSuperseded,
			ID:           p.ID,
			Delivery:     p.Delivery,
			Digest:       p.Digest,
			App:          p.App,
			Branch:       p.Branch,
			Commit:       p.Commit,
			Received:     p.Received,
			SupersededBy: newest.ID,
			At:           time.Now().UTC(),
		}); err != nil && logf != nil {
			logf("could not mark stale admission %s superseded during resume: %v", p.ID, err)
		}
	}
	if logf != nil {
		logf("resuming admitted-but-unprocessed webhook delivery %s (received %s; changed-file list unrecoverable post-crash — deploying fail-open)", newest.ID, newest.Received.Format(time.RFC3339))
	}
	q.admit(*newest, nil, false)
	return nil
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
//
// commit pins the build to the commit the webhook delivery authenticated
// (payload after/checkout_sha — C02); empty deploys the branch tip (the
// scheduled-redeploy path, which has no event to pin to).
func triggerAutoDeploy(ctx context.Context, executor ssh.Executor, app, branch, buildDir, commit string, out io.Writer, changedFiles []string, filesKnown, strictEnv bool) error {
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
	if err := fetchCheckout(ctx, executor, buildDir, branch, commit, out); err != nil {
		return err
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

// fetchCheckout advances buildDir's origin and resets the worktree to the
// commit the delivery authenticated, or to the branch tip when no commit is
// known (C02: a delivery for commit A never silently builds whatever the
// moving branch points at by fetch time). The two modes are stated in the
// deploy output so the operator can see which ran.
//
// Pinning strategy: fetch the branch (brings the tip and its reachable
// history), then best-effort fetch the commit SHA itself — on servers that
// allow SHA fetches (GitHub, GitLab) this also pulls commits no longer
// reachable from the tip after a force-push — then VERIFY the commit is
// present as a commit object before resetting to it. When the commit cannot
// be brought in at all (force-pushed away and garbage-collected, or the
// server rejects SHA fetches), the deploy fails loudly naming both the
// authenticated commit and where the branch is now; it never silently
// falls back to the tip.
func fetchCheckout(ctx context.Context, executor ssh.Executor, buildDir, branch, commit string, out io.Writer) error {
	cd := "cd " + ssh.ShellQuote(buildDir) + " && "
	if _, err := executor.Run(ctx, cd+"git fetch origin "+ssh.ShellQuote(branch)); err != nil {
		return fmt.Errorf("fetching %s (is %s a valid git checkout with a fetchable 'origin' remote? this must exist before the first webhook-triggered deploy — see `teploy deploy`'s server-build mode, or clone it manually): %w",
			branch, buildDir, err)
	}
	if commit == "" {
		fmt.Fprintf(out, "Deploying tip of %s\n", branch)
		if _, err := executor.Run(ctx, cd+"git reset --hard "+ssh.ShellQuote("origin/"+branch)); err != nil {
			return fmt.Errorf("resetting %s to origin/%s: %w", buildDir, branch, err)
		}
		return nil
	}

	fmt.Fprintf(out, "Deploying %s from delivery (branch %s)\n", commit, branch)
	// Best-effort direct fetch of the authenticated commit: failure is
	// fine (many servers refuse SHA fetches) — the branch fetch above
	// already brought everything reachable from the tip, and existence
	// is verified before the reset either way.
	_, _ = executor.Run(ctx, cd+"git fetch origin "+ssh.ShellQuote(commit))
	if _, err := executor.Run(ctx, cd+"git cat-file -e "+ssh.ShellQuote(commit+"^{commit}")); err != nil {
		tip, tipErr := executor.Run(ctx, cd+"git rev-parse "+ssh.ShellQuote("origin/"+branch))
		tip = strings.TrimSpace(tip)
		if tipErr != nil || tip == "" {
			tip = "<unknown>"
		}
		return fmt.Errorf("the delivery's authenticated commit %s cannot be fetched from origin (branch %s is now at %s — the commit was force-pushed away or removed); refusing to deploy the moved tip instead. Re-push the commit or trigger a fresh deploy: %w",
			commit, branch, tip, err)
	}
	if _, err := executor.Run(ctx, cd+"git reset --hard "+ssh.ShellQuote(commit)); err != nil {
		return fmt.Errorf("resetting %s to authenticated commit %s: %w", buildDir, commit, err)
	}
	return nil
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
