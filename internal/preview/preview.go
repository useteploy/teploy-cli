package preview

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/useteploy/teploy/internal/caddy"
	"github.com/useteploy/teploy/internal/docker"
	"github.com/useteploy/teploy/internal/ssh"
)

const deploymentsDir = "/deployments"

// State tracks a preview deployment on the server.
type State struct {
	// ID is the canonical preview identifier (<app>-p-<8hex>, see
	// PreviewID). Empty on records written before the canonical-ID
	// migration (legacy slug-keyed records).
	ID string `json:"id,omitempty"`
	// Branch is the FULL, unsanitized branch name. Records are keyed by
	// the canonical ID, not by this value's sanitized form.
	Branch string `json:"branch"`
	// Repo is the trivially normalized origin remote URL of the checkout
	// that deployed this preview ("" when unresolvable). Provenance and
	// legacy disambiguation only — deliberately not part of the ID, which
	// keys on the app (the repo's stable deployment identity) instead.
	Repo string `json:"repo,omitempty"`
	// Route is the Caddy route key / docker network alias this preview's
	// artifacts live under. Empty on legacy records, whose artifacts were
	// keyed by the sanitized branch slug.
	Route     string    `json:"route,omitempty"`
	Domain    string    `json:"domain"`
	Port      int       `json:"port"`
	Container string    `json:"container"`
	Image     string    `json:"image"`
	CreatedAt time.Time `json:"created_at"`
	// ExpiresAt is the record's absolute TTL deadline: written on every
	// Deploy as creation/update time + the configured TTL (the documented
	// default is 72h — DeployConfig.TTL). Enforcement reads this field;
	// `teploy preview prune` (and the deploy piggyback) destroy records
	// whose deadline has passed.
	ExpiresAt time.Time `json:"expires_at"`
}

// DeployConfig holds parameters for creating a preview.
type DeployConfig struct {
	App     string
	Domain  string // base domain (e.g., myapp.com)
	Branch  string
	Image   string
	Version string
	EnvFile string
	Env     map[string]string
	Volumes map[string]string
	// TTL is how long the preview lives; 0 means the documented default of
	// 72h. Applied on every Deploy (create AND update — an update refreshes
	// the deadline), recorded as the absolute State.ExpiresAt.
	TTL time.Duration
	// Repo is the normalized repo identity recorded in the preview record
	// (see State.Repo). Empty is allowed: the repo is provenance, not part
	// of the preview ID.
	Repo string
}

// Manager handles preview environment lifecycle.
type Manager struct {
	exec   ssh.Executor
	docker *docker.Client
	caddy  *caddy.Client
	out    io.Writer
	// healthTimeout/healthInterval bound the candidate readiness gate
	// (blue/green switch). Defaults set by NewManager; unexported knobs so
	// tests can shorten them.
	healthTimeout  time.Duration
	healthInterval time.Duration
}

// NewManager creates a preview manager.
func NewManager(exec ssh.Executor, out io.Writer) *Manager {
	return &Manager{
		exec:           exec,
		docker:         docker.NewClient(exec),
		caddy:          caddy.NewClient(exec),
		out:            out,
		healthTimeout:  30 * time.Second,
		healthInterval: time.Second,
	}
}

var nonAlphanumeric = regexp.MustCompile(`[^a-z0-9-]`)

// SanitizeBranch cleans a branch name for use in DNS labels. This is a
// DISPLAY derivation only: distinct branches can sanitize to the same slug
// (feature/login and feature-login both become feature-login), so it must
// never key state, containers, or routes — that is PreviewID's job.
func SanitizeBranch(branch string) string {
	s := strings.ToLower(branch)
	s = strings.ReplaceAll(s, "/", "-")
	s = strings.ReplaceAll(s, "_", "-")
	s = nonAlphanumeric.ReplaceAllString(s, "")
	// Remove leading/trailing hyphens.
	s = strings.Trim(s, "-")
	// Truncate to 63 chars (DNS label limit).
	if len(s) > 63 {
		s = s[:63]
	}
	s = strings.TrimRight(s, "-")
	if s == "" {
		s = "preview"
	}
	return s
}

// previewIDHex returns the collision-resistant identity suffix for a
// preview: the first 8 hex chars of sha256(app + NUL + full branch ref).
// The app is the canonical repo identity as teploy knows it — every piece
// of server state is namespaced by the app, so one app is one repo's
// deployment identity. The git remote URL is recorded per-record as
// provenance but deliberately NOT hashed into the ID: remote URLs change
// on repo renames and protocol switches, which would silently orphan
// existing previews, while the app name is stable.
func previewIDHex(app, branch string) string {
	sum := sha256.Sum256([]byte(app + "\x00" + branch))
	return hex.EncodeToString(sum[:4])
}

// PreviewID returns the canonical preview identifier: <app>-p-<8hex>,
// derived from the app (canonical repo identity) plus the full branch ref.
// Distinct branches — even ones whose sanitized slugs collide — always get
// distinct IDs, and therefore distinct state files, containers, routes,
// and domains.
func PreviewID(app, branch string) string {
	return app + "-p-" + previewIDHex(app, branch)
}

// previewDomain returns the per-preview subdomain. The sanitized branch is
// the human-readable display part; the canonical ID suffix guarantees
// uniqueness, so branches that sanitize identically (feature/login vs
// feature-login) get distinct hostnames instead of fighting over one
// Caddy site block. The slug is bounded so the whole DNS label stays
// within 63 characters: "preview-" (8) + slug + "-" + 8hex.
func previewDomain(app, branch, baseDomain string) string {
	slug := SanitizeBranch(branch)
	if max := 63 - len("preview-") - 1 - len(previewIDHex(app, branch)); len(slug) > max {
		slug = strings.TrimRight(slug[:max], "-")
	}
	return fmt.Sprintf("preview-%s-%s.%s", slug, previewIDHex(app, branch), baseDomain)
}

func previewDir(app string) string {
	return fmt.Sprintf("%s/%s/previews", deploymentsDir, app)
}

// previewStatePath is the canonical state-file path, keyed by PreviewID.
func previewStatePath(app, branch string) string {
	return fmt.Sprintf("%s/%s.json", previewDir(app), PreviewID(app, branch))
}

// legacyPreviewStatePath is the pre-canonical-ID state-file path, keyed by
// the sanitized branch slug. Read-only: used to find and adopt (or refuse)
// records written by older teploy versions. Never written.
func legacyPreviewStatePath(app, branch string) string {
	return fmt.Sprintf("%s/%s.json", previewDir(app), SanitizeBranch(branch))
}

// previewRouteKey derives the Caddy route key (and docker network alias)
// under which a record's artifacts live. Modern records carry the key in
// State.Route; records without one predate the field and their artifacts
// were keyed by the sanitized branch slug.
func previewRouteKey(app string, s *State) string {
	if s.Route != "" {
		return s.Route
	}
	return app + "-preview-" + SanitizeBranch(s.Branch)
}

// AmbiguousPreviewError reports a legacy slug-keyed preview record whose
// identity cannot be established for the requested branch: the file is
// keyed by the sanitized slug two or more branches share, and its stored
// full Branch (or repo, when both sides record one) does not match the
// request. The record is NEVER mutated or deleted in this case — the
// operator must disambiguate explicitly.
type AmbiguousPreviewError struct {
	App             string
	Path            string
	StoredBranch    string
	RequestedBranch string
	StoredRepo      string
	RequestedRepo   string
}

func (e *AmbiguousPreviewError) Error() string {
	detail := fmt.Sprintf("stored branch %q does not match requested branch %q", e.StoredBranch, e.RequestedBranch)
	if e.StoredRepo != "" || e.RequestedRepo != "" {
		detail += fmt.Sprintf(" (stored repo %q vs requested repo %q)", e.StoredRepo, e.RequestedRepo)
	}
	return fmt.Sprintf(
		"ambiguous legacy preview record for app %q at %s: %s — the sanitized slug is shared by multiple branches, so identity cannot be established automatically. "+
			"Destroy the recorded preview explicitly with its own branch (`teploy preview destroy %s`), or inspect and remove/rename the state file on the server. Nothing was changed.",
		e.App, e.Path, detail, e.StoredBranch,
	)
}

// readRecord reads and parses the preview record at path. A confirmed
// absent file returns (nil, nil); a present-but-unparseable record is an
// error naming the path (identity cannot be established — fail closed
// rather than guessing or silently adopting).
func (m *Manager) readRecord(ctx context.Context, path string) (*State, error) {
	content, err := m.exec.Run(ctx, "cat "+path)
	if err != nil || strings.TrimSpace(content) == "" {
		return nil, nil
	}
	var s State
	if err := json.Unmarshal([]byte(strings.TrimSpace(content)), &s); err != nil {
		return nil, fmt.Errorf("reading preview record %s: %w", path, err)
	}
	return &s, nil
}

// resolveRecord locates the preview record for (app, branch) across the
// canonical-ID key and the legacy slug key, and returns it together with
// the path it was read from (nil, "" when no record exists).
//
// Legacy contract: a slug-keyed record is only touched when its stored
// full Branch matches the requested branch exactly (and its recorded Repo
// agrees when both sides have one). A match found at the legacy key is
// returned as-is with its path — callers decide what adoption means for
// their operation; Deploy migrates it to the canonical key, Destroy tears
// down the artifacts it actually names. A mismatch is an
// *AmbiguousPreviewError and nothing is mutated. When both keys hold
// records, the canonical one wins; a legacy duplicate whose stored Branch
// matches this branch is a stale leftover of an interrupted migration and
// is removed, but a legacy record for a DIFFERENT colliding branch belongs
// to that branch and is left in place untouched.
func (m *Manager) resolveRecord(ctx context.Context, app, branch, repo string) (*State, string, error) {
	canonPath := previewStatePath(app, branch)
	canon, err := m.readRecord(ctx, canonPath)
	if err != nil {
		return nil, "", err
	}

	legacyPath := legacyPreviewStatePath(app, branch)
	legacy, err := m.readRecord(ctx, legacyPath)
	if err != nil {
		return nil, "", err
	}
	if legacy == nil {
		return canon, canonPath, nil
	}

	// Only when the canonical key holds nothing can the legacy record
	// become this branch's: then its identity must be established exactly.
	// When the canonical key already holds this branch's record, a legacy
	// file under the shared slug that names a DIFFERENT branch belongs to
	// that branch's own (legacy) preview and must not block or color this
	// operation at all.
	if canon == nil {
		if legacy.Branch != branch || (legacy.Repo != "" && repo != "" && legacy.Repo != repo) {
			return nil, "", &AmbiguousPreviewError{
				App:             app,
				Path:            legacyPath,
				StoredBranch:    legacy.Branch,
				RequestedBranch: branch,
				StoredRepo:      legacy.Repo,
				RequestedRepo:   repo,
			}
		}
		return legacy, legacyPath, nil
	}
	// Both keys hold records. A legacy file whose stored Branch matches
	// this branch is a stale duplicate of the canonical one (interrupted
	// migration) — the full-Branch match established it is this branch's
	// own, so remove it. Any other legacy file stays untouched above.
	if legacy.Branch == branch {
		m.exec.Run(ctx, "rm -f -- "+legacyPath)
	}
	return canon, canonPath, nil
}

// writeRecord persists a preview record at path.
func (m *Manager) writeRecord(ctx context.Context, s *State, path string) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := m.exec.Upload(ctx, strings.NewReader(string(data)), path, "0644"); err != nil {
		return err
	}
	return nil
}

// Deploy creates or updates a preview environment for the given branch.
//
// Updates are blue/green (C06): the candidate starts under a
// version-suffixed container name and network alias, passes a readiness
// gate, and only then takes over the preview's stable Caddy route key —
// the predecessor is stopped and removed AFTER the switch. A candidate
// that fails its gate (or the route switch) leaves the predecessor
// running, routed, and recorded; only the failed candidate is cleaned up.
// The canonical ID, state path, route key, and domain are stable across
// updates — only the upstream container moves.
func (m *Manager) Deploy(ctx context.Context, cfg DeployConfig) error {
	if cfg.TTL == 0 {
		cfg.TTL = 72 * time.Hour
	}

	// Resolve any legacy record BEFORE mutating anything: a slug-keyed
	// record that belongs to a different branch must stop the deploy with
	// an ambiguous-resource error, never be silently overwritten; one that
	// unambiguously belongs to this branch is adopted under the canonical
	// key first, so the rewrite below replaces one record instead of
	// orphaning the old key.
	existing, existingPath, err := m.resolveRecord(ctx, cfg.App, cfg.Branch, cfg.Repo)
	if err != nil {
		return err
	}
	if existing != nil && existingPath == legacyPreviewStatePath(cfg.App, cfg.Branch) {
		adopted := *existing
		adopted.ID = PreviewID(cfg.App, cfg.Branch)
		if adopted.Repo == "" {
			adopted.Repo = cfg.Repo
		}
		// The record's live artifacts predate the migration (Route empty,
		// slug-keyed) — keep them described exactly as they are so the
		// retirement below tears down what is actually running.
		if err := m.writeRecord(ctx, &adopted, previewStatePath(cfg.App, cfg.Branch)); err != nil {
			return fmt.Errorf("migrating legacy preview record: %w", err)
		}
		m.exec.Run(ctx, "rm -f -- "+existingPath)
	}

	idHex := previewIDHex(cfg.App, cfg.Branch)
	domain := previewDomain(cfg.App, cfg.Branch, cfg.Domain)
	// The process (container name component AND network alias) carries the
	// version: each candidate gets its own alias, so the stable route can
	// point at exactly one generation — a shared alias would round-robin
	// between predecessor and candidate the moment both run.
	process := "preview-p-" + idHex + "-" + cfg.Version
	routeApp := cfg.App + "-preview-p-" + idHex
	containerName := cfg.App + "-" + process

	fmt.Fprintf(m.out, "Deploying preview for branch %q...\n", cfg.Branch)
	fmt.Fprintf(m.out, "  Domain: %s\n", domain)

	// Ensure preview directory exists.
	if _, err := m.exec.Run(ctx, "mkdir -p "+previewDir(cfg.App)); err != nil {
		return fmt.Errorf("creating preview directory: %w", err)
	}

	// Allocate port.
	port, err := m.docker.FindAvailablePort(ctx)
	if err != nil {
		return fmt.Errorf("allocating port: %w", err)
	}
	fmt.Fprintf(m.out, "  Port: %d\n", port)

	// Same-version update: the running predecessor holds the candidate's
	// exact name. Rename it aside (it keeps serving under the shared
	// alias until the switch — the main engine's _replaced pattern).
	predecessorContainer := ""
	predecessorRoute := ""
	renamedAside := false
	if existing != nil {
		predecessorContainer = existing.Container
		predecessorRoute = previewRouteKey(cfg.App, existing)
		if predecessorContainer == containerName {
			if _, err := m.exec.Run(ctx, fmt.Sprintf("docker rename %s %s",
				ssh.ShellQuote(predecessorContainer), ssh.ShellQuote(predecessorContainer+"-replaced"))); err != nil {
				return fmt.Errorf("renaming the running preview %s aside for the same-version update (a leftover -replaced container may need `docker rm` first): %w", predecessorContainer, err)
			}
			renamedAside = true
			predecessorContainer = predecessorContainer + "-replaced"
		}
	}

	// abortCandidate tears down the failed candidate and puts a renamed
	// predecessor back under its recorded name. The predecessor is never
	// touched beyond that — it keeps serving.
	abortCandidate := func(reason error, format string, args ...any) error {
		m.docker.Stop(ctx, containerName, 5)
		m.docker.Remove(ctx, containerName)
		if renamedAside {
			m.exec.Run(ctx, fmt.Sprintf("docker rename %s %s",
				ssh.ShellQuote(predecessorContainer), ssh.ShellQuote(containerName)))
		}
		if reason != nil {
			return fmt.Errorf(format+": %w", append(args, reason)...)
		}
		return fmt.Errorf(format, args...)
	}

	// Start the candidate.
	var envFiles []string
	if cfg.EnvFile != "" {
		envFiles = []string{cfg.EnvFile}
	}
	_, err = m.docker.Run(ctx, docker.RunConfig{
		App:      cfg.App,
		Process:  process,
		Version:  cfg.Version,
		Name:     containerName,
		Image:    cfg.Image,
		Port:     port,
		EnvFiles: envFiles,
		Env:      cfg.Env,
		Volumes:  cfg.Volumes,
	})
	if err != nil {
		return abortCandidate(err, "starting preview container %s", containerName)
	}

	// Caddy dials the upstream over the docker network, so it needs the
	// container's INTERNAL port, not the host-published port (which is
	// what `port` is). Passing the host port made Caddy dial a port the
	// container isn't listening on inside the network, so every preview
	// route 502'd.
	internalPort, err := m.docker.InternalPort(ctx, containerName)
	if err != nil {
		return abortCandidate(err, "inspecting preview container %s port", containerName)
	}

	// Readiness gate: traffic only switches to a candidate that answers.
	// Mirrors the deploy engine's probe (HTTP 200 on /health, 404/3xx
	// falling back to a TCP check) against the candidate's
	// localhost-published port.
	if err := m.waitReady(ctx, port); err != nil {
		return abortCandidate(err, "preview candidate %s failed its health check — the previous preview is still serving", containerName)
	}

	// Switch the preview domain's route to the candidate. The route KEY
	// (and with it the canonical identity) is stable; only the upstream
	// container moves. Preview subdomains use Caddy automatic HTTPS (no
	// custom cert).
	if err := m.caddy.SetRoute(ctx, routeApp, domain, containerName, internalPort, caddy.TLS{}, "", nil, caddy.Firewall{}, caddy.Access{}); err != nil {
		return abortCandidate(err, "setting preview route — the previous preview is still serving")
	}

	// Write state.
	now := time.Now().UTC()
	state := State{
		ID:        PreviewID(cfg.App, cfg.Branch),
		Branch:    cfg.Branch,
		Repo:      cfg.Repo,
		Route:     routeApp,
		Domain:    domain,
		Port:      port,
		Container: containerName,
		Image:     cfg.Image,
		CreatedAt: now,
		ExpiresAt: now.Add(cfg.TTL),
	}
	if err := m.writeRecord(ctx, &state, previewStatePath(cfg.App, cfg.Branch)); err != nil {
		return fmt.Errorf("writing preview state: %w", err)
	}

	// Retire the predecessor — strictly AFTER the route serves the
	// candidate (blue/green: this ordering is the fix; stopping first was
	// the downtime window).
	if predecessorContainer != "" {
		m.docker.Stop(ctx, predecessorContainer, 5)
		m.docker.Remove(ctx, predecessorContainer)
	}
	// A legacy-era predecessor lived under a different route key; that key
	// must go. The canonical key was just repointed, so it stays.
	if predecessorRoute != "" && predecessorRoute != routeApp {
		m.caddy.RemoveRoute(ctx, predecessorRoute)
	}

	fmt.Fprintf(m.out, "  Preview deployed: https://%s\n", domain)
	fmt.Fprintf(m.out, "  Expires: %s\n", state.ExpiresAt.Format(time.RFC3339))
	return nil
}

// waitReady polls the candidate's localhost-published port until it
// answers, bounded by the manager's health timeout. The probe mirrors the
// deploy engine's readiness check: HTTP 200 on /health is ready; 404 or a
// redirect means the app is listening but has no /health route, and a TCP
// connect counts as ready; anything else retries until the deadline.
func (m *Manager) waitReady(ctx context.Context, port int) error {
	deadlineCtx, cancel := context.WithTimeout(ctx, m.healthTimeout)
	defer cancel()
	for {
		if m.probeOnce(deadlineCtx, port) {
			return nil
		}
		select {
		case <-deadlineCtx.Done():
			return fmt.Errorf("no response on localhost:%d within %s", port, m.healthTimeout)
		case <-time.After(m.healthInterval):
			// retry
		}
	}
}

// probeOnce performs one health probe attempt against the preview
// candidate (same curl discipline as internal/deploy's checkHealth: one
// quoted --url argument, globoff, no proxy, bounded per-attempt timeouts).
func (m *Manager) probeOnce(ctx context.Context, port int) bool {
	if port < 1 || port > 65535 {
		return false
	}
	target := "http://" + net.JoinHostPort("localhost", strconv.Itoa(port)) + "/health"
	out, err := m.exec.Run(ctx, fmt.Sprintf(
		"curl -s -o /dev/null --noproxy '*' --globoff --connect-timeout 2 --max-time 5 -w '%%{http_code}' --url %s",
		ssh.ShellQuote(target)))
	if err == nil {
		switch code := strings.TrimSpace(out); {
		case code == "200":
			return true
		case code == "404" || strings.HasPrefix(code, "3"):
			return m.probeTCP(ctx, port)
		}
	}
	return false
}

// probeTCP reports whether a TCP connection to localhost:port succeeds —
// the listening-but-no-/health fallback.
func (m *Manager) probeTCP(ctx context.Context, port int) bool {
	if port < 1 || port > 65535 {
		return false
	}
	_, err := m.exec.Run(ctx, fmt.Sprintf("bash -c '</dev/tcp/localhost/%d' 2>/dev/null", port))
	return err == nil
}

// List returns all active previews for the app. Records from both the
// canonical-ID keys and legacy slug keys are listed; legacy records are
// returned unmodified (readers never mutate).
func (m *Manager) List(ctx context.Context, app string) ([]State, error) {
	dir := previewDir(app)
	out, err := m.exec.Run(ctx, fmt.Sprintf("ls %s/*.json 2>/dev/null", dir))
	if err != nil || strings.TrimSpace(out) == "" {
		return nil, nil
	}

	var previews []State
	for _, path := range strings.Split(strings.TrimSpace(out), "\n") {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		content, err := m.exec.Run(ctx, "cat "+path)
		if err != nil {
			continue
		}
		var s State
		if err := json.Unmarshal([]byte(strings.TrimSpace(content)), &s); err != nil {
			continue
		}
		previews = append(previews, s)
	}
	return previews, nil
}

// Destroy tears down a preview environment.
func (m *Manager) Destroy(ctx context.Context, app, branch string) error {
	s, path, err := m.resolveRecord(ctx, app, branch, "")
	if err != nil {
		return err
	}
	if s == nil {
		return nil // no preview to destroy
	}

	// Stop and remove container.
	m.docker.Stop(ctx, s.Container, 5)
	m.docker.Remove(ctx, s.Container)

	// Remove Caddy route (keyed by the record's own era).
	m.caddy.RemoveRoute(ctx, previewRouteKey(app, s))

	// Remove state file.
	m.exec.Run(ctx, "rm -f -- "+path)

	fmt.Fprintf(m.out, "Destroyed preview for branch %q\n", branch)
	return nil
}

// Prune removes a single app's expired previews, reporting the outcome of
// each one. This is the shared prune core: the deploy piggyback and the
// standalone all-apps prune (PruneAll) both run exactly this code path.
func (m *Manager) Prune(ctx context.Context, app string) (int, error) {
	previews, err := m.List(ctx, app)
	if err != nil {
		return 0, err
	}

	now := time.Now().UTC()
	pruned := 0
	for _, p := range previews {
		if !now.After(p.ExpiresAt) {
			continue
		}
		if err := m.Destroy(ctx, app, p.Branch); err != nil {
			fmt.Fprintf(m.out, "Warning: failed to prune preview %s: %v\n", p.Branch, err)
			continue
		}
		fmt.Fprintf(m.out, "Pruned expired preview %q (%s)\n", p.Branch, p.Domain)
		pruned++
	}
	return pruned, nil
}

// PruneAll removes expired previews across EVERY app on the target server,
// enumerating each app's preview records (canonical and legacy eras alike
// — List reads whatever files exist under the app's previews directory).
// This is what the standalone `teploy preview prune` runs, so TTL
// enforcement no longer depends on someone deploying a new preview of the
// same app: the command can be cron'd. Idempotent by construction — a
// pruned record is gone, so a second run finds nothing expired. Never
// touches anything outside /deployments/<app>/previews and the artifacts
// the records themselves name.
func (m *Manager) PruneAll(ctx context.Context) (int, error) {
	out, err := m.exec.Run(ctx, "ls -d /deployments/*/previews 2>/dev/null")
	if err != nil && strings.TrimSpace(out) == "" {
		// No preview directories at all — nothing to prune.
		return 0, nil
	}

	var apps []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		rest, ok := strings.CutPrefix(line, deploymentsDir+"/")
		if !ok {
			continue
		}
		app, ok := strings.CutSuffix(rest, "/previews")
		if ok && app != "" && !strings.Contains(app, "/") {
			apps = append(apps, app)
		}
	}

	total := 0
	var errs []error
	for _, app := range apps {
		n, err := m.Prune(ctx, app)
		if err != nil {
			errs = append(errs, fmt.Errorf("app %s: %w", app, err))
		}
		total += n
	}
	return total, errors.Join(errs...)
}
