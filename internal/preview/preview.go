package preview

import (
	"context"
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
	Branch    string    `json:"branch"`
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

// SanitizeBranch cleans a branch name for use in DNS labels.
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

// previewDomain returns the subdomain for a preview: preview-{branch}.{domain}
func previewDomain(branch, baseDomain string) string {
	return fmt.Sprintf("preview-%s.%s", SanitizeBranch(branch), baseDomain)
}

func previewDir(app string) string {
	return fmt.Sprintf("%s/%s/previews", deploymentsDir, app)
}

func previewStatePath(app, branch string) string {
	return fmt.Sprintf("%s/%s.json", previewDir(app), SanitizeBranch(branch))
}

// Deploy creates or updates a preview environment for the given branch.
func (m *Manager) Deploy(ctx context.Context, cfg DeployConfig) error {
	if cfg.TTL == 0 {
		cfg.TTL = 72 * time.Hour
	}

	sanitized := SanitizeBranch(cfg.Branch)
	domain := previewDomain(cfg.Branch, cfg.Domain)
	containerName := fmt.Sprintf("%s-preview-%s-%s", cfg.App, sanitized, cfg.Version)

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
		Process:  "preview-" + sanitized,
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

	// Set Caddy route for preview domain. The preview container gets a
	// dedicated network alias (cfg.App + "-preview-" + sanitized) via
	// docker.RunConfig.Process, which is what we dial here.
	routeApp := cfg.App + "-preview-" + sanitized
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
	if err := m.caddy.SetRoute(ctx, routeApp, domain, routeApp, internalPort, caddy.TLS{}, ""); err != nil {
		// Clean up container on route failure.
		m.docker.Stop(ctx, containerName, 5)
		m.docker.Remove(ctx, containerName)
		return fmt.Errorf("setting preview route: %w", err)
	}

	// Write state.
	now := time.Now().UTC()
	state := State{
		Branch:    cfg.Branch,
		Domain:    domain,
		Port:      port,
		Container: containerName,
		Image:     cfg.Image,
		CreatedAt: now,
		ExpiresAt: now.Add(cfg.TTL),
	}

	data, _ := json.MarshalIndent(state, "", "  ")
	statePath := previewStatePath(cfg.App, cfg.Branch)
	if err := m.exec.Upload(ctx, strings.NewReader(string(data)), statePath, "0644"); err != nil {
		return fmt.Errorf("writing preview state: %w", err)
	}

	fmt.Fprintf(m.out, "  Preview deployed: https://%s\n", domain)
	fmt.Fprintf(m.out, "  Expires: %s\n", state.ExpiresAt.Format(time.RFC3339))
	return nil
}

// List returns all active previews for the app.
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
	statePath := previewStatePath(app, branch)
	content, err := m.exec.Run(ctx, "cat "+statePath+" 2>/dev/null")
	if err != nil || strings.TrimSpace(content) == "" {
		return nil // no preview to destroy
	}

	var s State
	if err := json.Unmarshal([]byte(strings.TrimSpace(content)), &s); err != nil {
		return nil
	}

	// Stop and remove container.
	m.docker.Stop(ctx, s.Container, 5)
	m.docker.Remove(ctx, s.Container)

	// Remove Caddy route.
	sanitized := SanitizeBranch(branch)
	routeApp := app + "-preview-" + sanitized
	m.caddy.RemoveRoute(ctx, routeApp)

	// Remove state file.
	m.exec.Run(ctx, "rm -f "+statePath)

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
