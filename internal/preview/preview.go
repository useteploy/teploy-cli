package preview

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
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
	TTL     time.Duration // default 72h
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
}

// NewManager creates a preview manager.
func NewManager(exec ssh.Executor, out io.Writer) *Manager {
	return &Manager{
		exec:   exec,
		docker: docker.NewClient(exec),
		caddy:  caddy.NewClient(exec),
		out:    out,
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
		// destroy below tears down what is actually running.
		if err := m.writeRecord(ctx, &adopted, previewStatePath(cfg.App, cfg.Branch)); err != nil {
			return fmt.Errorf("migrating legacy preview record: %w", err)
		}
		m.exec.Run(ctx, "rm -f -- "+existingPath)
	}

	idHex := previewIDHex(cfg.App, cfg.Branch)
	domain := previewDomain(cfg.App, cfg.Branch, cfg.Domain)
	process := "preview-p-" + idHex
	routeApp := cfg.App + "-" + process
	containerName := fmt.Sprintf("%s-%s-%s", cfg.App, process, cfg.Version)

	fmt.Fprintf(m.out, "Deploying preview for branch %q...\n", cfg.Branch)
	fmt.Fprintf(m.out, "  Domain: %s\n", domain)

	// Ensure preview directory exists.
	if _, err := m.exec.Run(ctx, "mkdir -p "+previewDir(cfg.App)); err != nil {
		return fmt.Errorf("creating preview directory: %w", err)
	}

	// Destroy existing preview for this branch if it exists.
	m.Destroy(ctx, cfg.App, cfg.Branch)

	// Allocate port.
	port, err := m.docker.FindAvailablePort(ctx)
	if err != nil {
		return fmt.Errorf("allocating port: %w", err)
	}
	fmt.Fprintf(m.out, "  Port: %d\n", port)

	// Start container.
	var envFiles []string
	if cfg.EnvFile != "" {
		envFiles = []string{cfg.EnvFile}
	}
	_, err = m.docker.Run(ctx, docker.RunConfig{
		App:      cfg.App,
		Process:  process,
		Version:  cfg.Version,
		Image:    cfg.Image,
		Port:     port,
		EnvFiles: envFiles,
		Env:      cfg.Env,
		Volumes:  cfg.Volumes,
	})
	if err != nil {
		return fmt.Errorf("starting preview container: %w", err)
	}

	// Set Caddy route for the preview domain. The preview container gets a
	// dedicated network alias (cfg.App + "-" + process) via
	// docker.RunConfig.Process, which is what we dial here.
	// Caddy dials the upstream over the docker network, so it needs the
	// container's INTERNAL port, not the host-published port (which is what
	// `port` is). Passing the host port made Caddy dial a port the container
	// isn't listening on inside the network, so every preview route 502'd.
	internalPort, err := m.docker.InternalPort(ctx, containerName)
	if err != nil {
		m.docker.Stop(ctx, containerName, 5)
		m.docker.Remove(ctx, containerName)
		return fmt.Errorf("inspecting preview container port: %w", err)
	}
	// Preview subdomains use Caddy automatic HTTPS (no custom cert).
	if err := m.caddy.SetRoute(ctx, routeApp, domain, routeApp, internalPort, caddy.TLS{}, "", nil, caddy.Firewall{}, caddy.Access{}); err != nil {
		// Clean up container on route failure.
		m.docker.Stop(ctx, containerName, 5)
		m.docker.Remove(ctx, containerName)
		return fmt.Errorf("setting preview route: %w", err)
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

	fmt.Fprintf(m.out, "  Preview deployed: https://%s\n", domain)
	fmt.Fprintf(m.out, "  Expires: %s\n", state.ExpiresAt.Format(time.RFC3339))
	return nil
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

// Prune removes expired previews.
func (m *Manager) Prune(ctx context.Context, app string) (int, error) {
	previews, err := m.List(ctx, app)
	if err != nil {
		return 0, err
	}

	now := time.Now().UTC()
	pruned := 0
	for _, p := range previews {
		if now.After(p.ExpiresAt) {
			if err := m.Destroy(ctx, app, p.Branch); err != nil {
				fmt.Fprintf(m.out, "Warning: failed to prune preview %s: %v\n", p.Branch, err)
				continue
			}
			pruned++
		}
	}
	return pruned, nil
}
