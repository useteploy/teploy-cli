# Changelog

All notable changes to teploy are documented here. Format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

### Added
- `keep_versions: N` — auto-prune older app versions on deploy. The current and immediately-previous versions are always protected; older versions beyond `N` are removed (containers + images). Container-deploy only — static deploys use `keep_releases`. Default `0` = keep everything (legacy behavior).
- `healthcheck.<proc>.disable` — per-process override that passes `--no-healthcheck` to `docker run`. Useful for worker containers that share an image with a web container and would otherwise inherit a useless HTTP healthcheck.
- `ingress: external` — opt out of Caddy entirely. The container still joins the `teploy` Docker network with its app-name alias, but Teploy doesn't write or reload the Caddyfile. For users fronting the app with Cloudflare Tunnel, Tailscale Funnel, nginx, AWS ALB, or any other external ingress.

### Fixed
- `.goreleaser.yml`: migrated `archives.format` and `format_overrides.format` to the new `formats: [tar.gz|zip]` list syntax (goreleaser 2.x deprecation cleanup).

### Docs
- README Config section now documents `tls`, `keep_versions`, `healthcheck`, and `ingress` (previously undocumented).
- CLAUDE.md updated for repo state (project structure reflects current packages; stale design-doc references removed).

## [0.1.5] - 2026-05-29

### Added
- Per-app custom TLS certificate via `tls: { cert, key }`. Cert + key are local file paths, uploaded to the server on deploy and referenced from the generated Caddy site block. Required when the public hostname is proxied (Cloudflare proxy, Cloudflare Tunnel) so ACME challenges can't reach the origin — terminate TLS with a Cloudflare Origin Certificate instead. The cert survives every deploy (unlike a hand-edited Caddyfile, which the authoritative model overwrites).
- `teploy setup` now recreates legacy single-file-Caddyfile-mount containers, not just `--resume` ones.

### Fixed
- Stale Caddyfile bind-mount bug. `setup.go` previously mounted the Caddyfile as a *single file*; `caddy.go` writes it atomically (`mv`), which swaps the inode. Docker pins single-file mounts to the original inode, so the container never saw route updates and `caddy reload` reloaded stale config. Caddyfile is now mounted via its parent directory (`/deployments/caddy:/etc/caddy`), preserving updates after atomic rewrites. Affects every server using the prior mount model — recreate Caddy with `teploy setup` to pick up the fix.

### Docs
- README URLs updated for `useteploy/teploy` → `useteploy/teploy-cli` repo rename. GitHub auto-redirects, but explicit references now point at the canonical name.

## [0.1.4] - 2026-05-28

### Added
- Caddyfile is now the single source of truth. Admin-API route changes are mirrored back to the Caddyfile so they survive `caddy reload`.

### Fixed
- Published container ports bound to `127.0.0.1` instead of `0.0.0.0` — closes a direct-from-internet exposure vector when the server's firewall is permissive.
- Health checks tolerate HTTP 3xx responses (apps that redirect from `/` to `/login` are no longer marked unhealthy).
- Asset bridging now runs as root and surfaces failures up the deploy pipeline. Fixes Next.js / Rails 404s on `/assets/*` after deploys to non-root-`USER` images.

## [0.1.3] - 2026-05-10

### Added
- `type: static` deploy mode — rsync + symlink + Caddyfile, no Docker. For static sites (Astro, Hugo, plain HTML).
- Scheduled auto-deploy via cron (`autodeploy:` block in teploy.yml).
- `-d` / `--destination` flag for accessory subcommands (consistent with deploy).
- Ad-hoc deploy flags: `--app`, `--image`, `--domain` for scripting and dashboard use without a teploy.yml.
- Per-server replicas (different `replicas:` per server in fleet).
- Template install (deploy directly from a registry or git URL).
- Server hardening — firewall + SSH config + auto-updates wired into `teploy setup`.
- Password bootstrap for fresh servers.
- VPN integration in setup (Tailscale + WireGuard).
- Comma-separated `domain:` field for apex + www served from one block.

### Fixed
- Encrypted secrets actually injected into container env (was silently no-op).
- Foreign volume mount detection prevents data loss on swap (deploy aborts with `--migrate-volumes` hint when an existing container has a different host path).
- Caddy route persistence survives reloads (precursor to the v0.1.4 source-of-truth fix).
- Rollback port bug — used the wrong port when the previous container's `ContainerPort` had changed.
- Rollback missing domain in embedded UI.

## [0.1.2] - 2026-03-13

### Added
- Scoop bucket for Windows installs (`scoop install teploy`).

### Docs
- README features table, tool comparison, and badges.

## [0.1.1] - 2026-03-13

Patch release of initial drop.

## [0.1.0] - 2026-03-13

Initial release.
