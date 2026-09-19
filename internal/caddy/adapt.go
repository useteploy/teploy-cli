// Caddy adapt-API integration (audits F48/F49).
//
// `caddy adapt --config - --adapter caddyfile` turns Caddyfile text into
// Caddy's own JSON config — the canonical structured route representation.
// Two consumers:
//
//   - The HARD pre-write gate is the SERVER's binary (Client.adaptCheck:
//     `docker exec -i caddy caddy adapt` over stdin) — the authoritative
//     validator, since it is the exact binary that will serve the config.
//   - The LOCAL binary, when one is in PATH (Adapter below), is an
//     advisory/debug surface and the cross-check oracle for the vendored
//     parser (routes_test). It is deliberately NOT allowed to refuse an
//     edit: its version and module set can differ from the server's, and a
//     stock local binary would reject legitimate caddy_extra directives
//     from custom server builds (rate_limit & co), breaking deploys that
//     work today. Binary drift makes a local hard gate a false-positive
//     machine, so the reload — run by the server's own caddy, with
//     rollback — remains the final authority, as it already was.
//
// The binary runs LOCALLY (adapt is a pure text transform; the Caddyfile
// content arrives over the executor) and is an optional dependency: without
// it, edits proceed on the vendored parser's guarantees plus the server-side
// gate and reload.

package caddy

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Adapter is a resolvable local caddy binary.
type Adapter struct {
	Path string
}

// ResolveAdapter finds a caddy binary in PATH ("" when there is none).
func ResolveAdapter() *Adapter {
	if path, err := exec.LookPath("caddy"); err == nil {
		return &Adapter{Path: path}
	}
	return nil
}

// AdaptCaddyfile adapts Caddyfile text to Caddy's JSON config via stdin,
// returning the adapted bytes. Any non-zero exit is an error carrying
// caddy's own stderr — including the line number it choked on.
func (a *Adapter) AdaptCaddyfile(ctx context.Context, content string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, a.Path, "adapt", "--config", "-", "--adapter", "caddyfile")
	cmd.Stdin = strings.NewReader(content)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("caddy adapt rejected the Caddyfile: %s", msg)
	}
	out := bytes.TrimSpace(stdout.Bytes())
	if len(out) == 0 {
		return nil, fmt.Errorf("caddy adapt produced no output")
	}
	return out, nil
}

// validateViaAdapt is the LOCAL advisory check: with a local caddy binary,
// content is adapted and any failure reported. Used by tooling and tests;
// never a deploy gate (see the package doc for the drift rationale).
func validateViaAdapt(ctx context.Context, content string) error {
	adapter := ResolveAdapter()
	if adapter == nil {
		return nil
	}
	_, err := adapter.AdaptCaddyfile(ctx, content)
	return err
}
