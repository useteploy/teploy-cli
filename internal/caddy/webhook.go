// Persisted webhook routing (audit T26/T27).
//
// The webhook listener's route used to be injected through Caddy's admin
// API at runtime — a route that lived ONLY in the running process's
// memory. Every ordinary deploy regenerates the on-disk Caddyfile and
// reloads, which erased the webhook route (a webhook-triggered deploy could
// remove its own future trigger), and any Caddy restart lost it too.
//
// The route is now PERSISTED: a small per-app config file records the
// webhook (hosts, path, listener port), and the fragment is rendered INSIDE
// the app's managed site block — ahead of the terminal reverse_proxy, so
// POSTs to /teploy-webhook/<app> reach the listener without passing the
// app's own auth gate (the behavior the runtime route's global-first
// evaluation provided). Every managed-block render (deploy, rollback,
// maintenance) re-applies the fragment from the config file, under the same
// Caddyfile lock + adapt gate + reload/verify transaction as every other
// route edit.
package caddy

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/useteploy/teploy/internal/ssh"
)

// webhookConfigPath is the per-app persisted webhook route descriptor
// (/deployments/<app>/.webhook-route), 0600.
func webhookConfigPath(app string) string {
	return fmt.Sprintf("/deployments/%s/.webhook-route", app)
}

// webhookRouteConfig is the persisted shape. Hosts is the validated host
// list the route matches; Path is the request path; Port is the LOCAL
// listener port Caddy proxies to (the dial is always host.docker.internal
// — see the old runtime injector's note about network namespaces).
type webhookRouteConfig struct {
	Hosts []string `json:"hosts"`
	Path  string   `json:"path"`
	Port  int      `json:"port"`
}

// readServerFile frames a file read with a confirmed-absence distinction.
func readServerFile(ctx context.Context, exec ssh.Executor, path string) ([]byte, bool, error) {
	out, err := exec.Run(ctx, fmt.Sprintf(
		"if [ ! -e %s ]; then printf 'absent\\n'; else printf 'present\\n'; cat -- %s; fi",
		ssh.ShellQuote(path), ssh.ShellQuote(path)))
	if err != nil {
		return nil, false, fmt.Errorf("reading %s: %w", path, err)
	}
	out = strings.TrimRight(out, "\n")
	if out == "absent" || strings.HasPrefix(out, "absent\n") {
		return nil, false, nil
	}
	if !strings.HasPrefix(out, "present\n") {
		if out == "present" {
			return nil, true, nil
		}
		return nil, false, fmt.Errorf("reading %s: unrecognized output framing", path)
	}
	return []byte(strings.TrimPrefix(out, "present\n")), true, nil
}

// loadWebhookConfig reads the persisted webhook descriptor. Absent is
// (nil, nil); unreadable/corrupt is an error — a corrupt descriptor must
// abort the route edit, never silently drop the webhook route.
func loadWebhookConfig(ctx context.Context, exec ssh.Executor, app string) (*webhookRouteConfig, error) {
	data, present, err := readServerFile(ctx, exec, webhookConfigPath(app))
	if err != nil || !present {
		return nil, err
	}
	var cfg webhookRouteConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing the persisted webhook route for %s: %w", app, err)
	}
	if len(cfg.Hosts) == 0 || cfg.Path == "" || cfg.Port < 1 || cfg.Port > 65535 {
		return nil, fmt.Errorf("persisted webhook route for %s is incomplete (hosts/path/port)", app)
	}
	return &cfg, nil
}

// webhookFragment renders the managed webhook fragment for injection at the
// top of the app's site block. handle blocks evaluate before the site's
// terminal reverse_proxy and before its auth directives, so the webhook
// stays reachable with its own HMAC boundary exactly as the old
// global-first runtime route was.
func webhookFragment(app string, cfg *webhookRouteConfig) string {
	matcher := "teploy_hook_" + app
	var b strings.Builder
	b.WriteString(fmt.Sprintf("\t# TEPLOY WEBHOOK BEGIN %s\n", app))
	b.WriteString(fmt.Sprintf("\t@%s {\n\t\tmethod POST\n\t\tpath %s\n\t}\n", matcher, cfg.Path))
	b.WriteString(fmt.Sprintf("\thandle @%s {\n\t\treverse_proxy host.docker.internal:%s\n\t}\n", matcher, strconv.Itoa(cfg.Port)))
	b.WriteString(fmt.Sprintf("\t# TEPLOY WEBHOOK END %s\n", app))
	return b.String()
}

// stripWebhookFragment removes the app's webhook fragment from its managed
// block (idempotent; a fragment that never existed is a no-op).
func stripWebhookFragment(block, app string) string {
	begin := fmt.Sprintf("\t# TEPLOY WEBHOOK BEGIN %s", app)
	end := fmt.Sprintf("\t# TEPLOY WEBHOOK END %s", app)
	var out []string
	skipping := false
	for _, line := range strings.Split(block, "\n") {
		if strings.TrimSpace(line) == strings.TrimSpace(begin) {
			skipping = true
			continue
		}
		if skipping {
			if strings.TrimSpace(line) == strings.TrimSpace(end) {
				skipping = false
			}
			continue
		}
		out = append(out, line)
	}
	result := strings.Join(out, "\n")
	for strings.Contains(result, "\n\n\n") {
		result = strings.ReplaceAll(result, "\n\n\n", "\n\n")
	}
	return result
}

// injectWebhookFragment inserts the fragment directly after the site
// block's opening line (the address line). The block arrives without its
// TEPLOY markers (extractCaddyfileBlock's contract).
func injectWebhookFragment(block, app string, cfg *webhookRouteConfig) (string, error) {
	lines := strings.Split(block, "\n")
	if len(lines) < 2 || !strings.HasSuffix(strings.TrimSpace(lines[0]), "{") {
		return "", fmt.Errorf("cannot place the webhook route: %s's site block has no recognizable opening line", app)
	}
	fragment := webhookFragment(app, cfg)
	out := append([]string{lines[0], strings.TrimRight(fragment, "\n")}, lines[1:]...)
	return strings.Join(out, "\n"), nil
}

// applyWebhookToBlock strips any existing fragment from the app's block and
// re-injects it from the PERSISTED descriptor when one exists. Returns the
// updated whole-file content. Called inside the Caddyfile mutation lock.
func (c *Client) applyWebhookToBlock(ctx context.Context, app, content string) (string, error) {
	begin := fmt.Sprintf(markerBeginFmt, app)
	end := fmt.Sprintf(markerEndFmt, app)
	block := extractCaddyfileBlock(content, begin, end)
	if block == "" {
		// No managed app block (external ingress, not yet deployed):
		// nothing to attach to. The descriptor, if written, applies at the
		// first managed render.
		return content, nil
	}
	cfg, err := loadWebhookConfig(ctx, c.exec, app)
	if err != nil {
		return "", err
	}
	block = stripWebhookFragment(block, app)
	if cfg != nil {
		if block, err = injectWebhookFragment(block, app, cfg); err != nil {
			return "", err
		}
	}
	updated, err := removeCaddyfileBlock(content, begin, end)
	if err != nil {
		return "", err
	}
	wrapped := begin + "\n" + strings.TrimRight(block, "\n") + "\n" + end
	return strings.TrimRight(updated, "\n") + "\n\n" + wrapped + "\n", nil
}

// SetWebhookRoute persists the app's webhook route descriptor and applies
// it to the app's managed site block in one Caddyfile transaction. Reports
// whether the fragment is LIVE: false means the app has no managed block
// yet (not deployed / external ingress) — the descriptor is stored and the
// route attaches on the first deploy that renders one.
func (c *Client) SetWebhookRoute(ctx context.Context, app, domain string, port int) error {
	hosts, err := parseDomains(domain)
	if err != nil {
		return err
	}
	if len(hosts) == 0 {
		return fmt.Errorf("SetWebhookRoute: domain must be non-empty")
	}
	if port < 1 || port > 65535 {
		return fmt.Errorf("SetWebhookRoute: listener port must be in 1..65535 (got %d)", port)
	}
	cfg := webhookRouteConfig{Hosts: hosts, Path: "/teploy-webhook/" + app, Port: port}
	data, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	// Descriptor first, then the route edit reads it inside the mutation
	// lock — a crash between the two leaves a descriptor the next deploy
	// applies; the reverse could render a fragment with no source of truth.
	if err := ssh.UploadAtomic(ctx, c.exec, strings.NewReader(string(data)+"\n"), webhookConfigPath(app), "0600"); err != nil {
		return fmt.Errorf("persisting the webhook route descriptor: %w", err)
	}
	return c.mutate(ctx, func(prev string) (string, error) {
		return c.applyWebhookToBlock(ctx, app, prev)
	})
}

// HasManagedBlock reports whether the app currently has a managed site
// block (the precondition for a live webhook fragment).
func (c *Client) HasManagedBlock(ctx context.Context, app string) (bool, error) {
	prev, err := c.exec.Run(ctx, "cat "+caddyfilePath)
	if err != nil {
		return false, fmt.Errorf("reading caddyfile (did setup run?): %w", err)
	}
	return extractCaddyfileBlock(prev, fmt.Sprintf(markerBeginFmt, app), fmt.Sprintf(markerEndFmt, app)) != "", nil
}

// RemoveWebhookRoute deletes the persisted descriptor and strips the
// fragment from the app's block in one transaction. No-op when neither
// exists.
func (c *Client) RemoveWebhookRoute(ctx context.Context, app string) error {
	if _, err := c.exec.Run(ctx, "rm -f -- "+ssh.ShellQuote(webhookConfigPath(app))); err != nil {
		return fmt.Errorf("removing the webhook route descriptor: %w", err)
	}
	return c.mutate(ctx, func(prev string) (string, error) {
		return c.applyWebhookToBlock(ctx, app, prev)
	})
}
