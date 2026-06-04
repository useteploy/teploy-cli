# Changelog

All notable changes to teploy are documented here. Format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

### Removed
- The embedded `teploy ui` web dashboard (`internal/ui/`) has been removed. It was an unauthenticated, localhost HTTP/WebSocket server that duplicated a strict subset of **teploy-dash** — the dedicated dashboard product (separate repo, real auth, monitoring, its own releases). Maintaining a second, weaker dashboard inside a CLI whose identity is "single binary, no management server" was a standing security surface (no auth/CSRF/Origin checks) and a maintenance/drift tax. The CLI is now a pure deploy engine; use teploy-dash for a dashboard (it runs as a single binary too, including locally).

### Added
- `teploy app exec [--process web] -- <cmd>` — run a one-off command inside the app's running container (resolved by the `teploy.version` label): database migrations, seeds, rake/`manage.py` tasks, etc. Runs in the existing container via its shell, streams output, and exits non-zero if the command fails. Works from an app directory or with `--app`+`--host`. `teploy exec` remains server-level (raw SSH); this is the container-level counterpart.
- `teploy accessory exec <name> -- <cmd>` — the same for an accessory container, e.g. `accessory exec db -- psql -U postgres -c 'SELECT 1'` or `accessory exec cache -- redis-cli INFO`. (An interactive REPL variant — `console`/`db` — was considered but deferred; it requires PTY handling that can't be unit-tested cleanly. Non-interactive queries are covered by this command.)
- `--app <name>` flag on `status`, `stats`, `log`, `logs`, `lock`, `unlock`, `maintenance on/off`, `start`, `stop`, `restart`, `env set/get/list/unset`, and `accessory list/stop/start/logs`. With `--app` + `--host`, these commands act on a deployed app by reading server-side state instead of requiring a `teploy.yml` in the working directory — the same model `rollback --app` already used. This is what lets teploy-dash (and any automation without an app checkout) drive these commands for arbitrary apps. A shared `resolveApp` helper unifies the cwd-`teploy.yml` path and the `--app`+`--host` path. `maintenance` keeps its Caddy-ingress guard only on the cwd path (server state doesn't record ingress mode). `accessory upgrade/backup/restore` stay cwd-bound — they need the accessory's image config from `teploy.yml`, which server state doesn't carry.
- `keep_versions: N` — auto-prune older app versions on deploy. The current and immediately-previous versions are always protected; older versions beyond `N` are removed (containers + images). Container-deploy only — static deploys use `keep_releases`. Default `0` = keep everything (legacy behavior).
- `healthcheck.<proc>.disable` — per-process override that passes `--no-healthcheck` to `docker run`. Useful for worker containers that share an image with a web container and would otherwise inherit a useless HTTP healthcheck.
- `ingress: external` — opt out of Caddy entirely. The container still joins the `teploy` Docker network with its app-name alias, but Teploy doesn't write or reload the Caddyfile. For users fronting the app with Cloudflare Tunnel, Tailscale Funnel, nginx, AWS ALB, or any other external ingress.

### Security
- `secret set` previously interpolated the secret value into `echo %q | age …`, which uses double quotes — so the remote shell still expanded `$`, backticks and backslashes, silently corrupting values and *executing* a value like `$(...)` as the SSH user. Secrets are now passed via `printf '%s'` with single-quoting (no expansion). The same `%q`/double-quote pattern in `docker.Exec` is fixed too, and `docker run` now single-quotes every interpolated value (name, image, env values, volumes, labels) so a value with a space or shell metacharacter can't break or inject into the command. (The command override is intentionally left raw — it's an operator-authored argv.)
- SSH host-key verification no longer fails open. When `~/.ssh/known_hosts` is absent (the common fresh-box / CI case) teploy previously accepted any host key with no record — no MITM protection ever. It now falls back to trust-on-first-use: the key is recorded on first connect and a mismatch errors thereafter (use `--accept-new` or clear the entry after a deliberate re-provision).
- Auto-deploy webhooks are now authenticated. The listener never actually extracted the signature header, so with a secret every request was rejected and *without* one any POST to `0.0.0.0:9876` triggered a deploy. The listener now captures and verifies the `X-Hub-Signature-256` HMAC over the raw body and binds `127.0.0.1`; `autodeploy setup` requires a secret (the CLI generates and prints one when `--secret` is omitted).

### Fixed
- Multi-replica deploys (`replicas: N > 1`) could never succeed — every replica was handed the same host port (the allocator only consulted currently-listening sockets, and no container was started yet), so the second `docker run -p` collided. The deploy loop now excludes ports already claimed in the same pass.
- `teploy rollback` dropped `domain` and the replica port arrays from the persisted state on swap, so a subsequent rollback failed with "no domain in state" and multi-replica apps orphaned replicas 2..N on the next deploy. The swap now carries `domain` through and mirrors `current_ports`/`previous_ports`.
- `rollback`, `start`, and `restart` matched containers by a `-<version>` name suffix, which silently skipped every replica web container (named `<app>-web-<version>-1`). They now match on the `teploy.version` label that every container carries.
- `env set` wrote values needing quoting with Go `%q`, but `docker run --env-file` reads values literally — so the container received the surrounding quotes/escapes, and `env list` (which unquotes) disagreed with the running container. Values are now written verbatim; newline-containing values are rejected (the env-file format can't represent them).
- `maintenance off` could delete an app's Caddy route entirely. It ignored the error from reading the stashed pre-maintenance block, so any transient read failure rendered an empty block and removed the route, taking the domain offline. It now no-ops on a missing stash, aborts without touching the route on a read failure, and deletes the stash only after the reload succeeds.
- `teploy update` was permanently broken: it pointed at the wrong GitHub repo (`teploy/teploy`) and expected a bare per-platform binary, while releases ship `teploy_{os}_{arch}.tar.gz` archives. It now uses `useteploy/teploy`, downloads the archive + `checksums.txt`, verifies the SHA-256 before installing, and extracts the binary.
- Preview environments routed Caddy to the host-published port instead of the container's internal port, so every preview 502'd. Rollback already inspected the internal port; preview now does the same.
- Redis accessory backups were unrestorable — `AccessoryRestore` had no redis case, so it looked for a `.tar.gz` while the backup is stored as `.rdb.gz`. Added a redis restore path (stop → copy `dump.rdb` → start so it loads the snapshot).
- `env set`/`unset` could wipe the env file on a transient SSH failure: `readEnv` ran `cat … 2>/dev/null` and treated any error as "empty", so a read that failed mid-operation returned an empty map and the subsequent write overwrote the real file with nothing. Reads now use a `test -f` guard so a genuine transport error propagates and the write is aborted; a missing file still yields an empty map.
- `teploy server add` (and the config layer) silently dropped a server's `tags` and cleared its `vpn_ip`/`role`/`user` when re-adding an existing entry — `AddServer` replaced the whole record. Tags drive per-host env injection, so losing them broke deploys. AddServer now preserves `tags` and keeps any optional field not being changed.
- Domains are now validated at the Caddy sink (`parseDomains`), rejecting entries with characters that would break out of a Caddyfile site address (whitespace, newline, `{` `}` `#` `"` `\`). Defense-in-depth beyond config-time validation; a denylist, so legitimate wildcard / `host:port` addresses still work.
- Scheduled backups: the cron dedup matched the whole command as a `grep` *regex*, but backup commands are full of regex metacharacters (`. * $ ( ) /`), so re-scheduling could leave duplicate cron entries. Managed lines now carry a stable `# teploy-backup:<app>` marker and dedup with `grep -vF`.
- `mergeConfigs` didn't carry `replicas` from a destination overlay (`teploy.<dest>.yml`), so `-d prod` couldn't change the replica count.
- `PruneVersions` compared Docker `CreatedAt` timestamps as strings (lexicographic, wrong across timezones/DST); it now parses them to `time.Time` with a deterministic tie-break and protects any container it can't date.
- `RunStream` could let the command goroutine write to the caller's stdout/stderr after returning on context cancellation (a data race); it now waits for the command to finish first.
- `AppendLog` and the autodeploy Caddy-route setup no longer stage through fixed `/tmp` paths that concurrent runs would clobber — the log line is appended in a single atomic command (base64 + `>>`, sub-PIPE_BUF) and the webhook route is piped to `curl -d @-` over stdin.
- `.goreleaser.yml`: migrated `archives.format` and `format_overrides.format` to the new `formats: [tar.gz|zip]` list syntax (goreleaser 2.x deprecation cleanup).
- `teploy setup` hung during `fail2ban` install on fresh Debian/Ubuntu VMs because `apt-get`'s debconf step prompted for input. All `apt-get` calls (ufw, fail2ban, unattended-upgrades, sudo, age) now pass `DEBIAN_FRONTEND=noninteractive` to suppress prompts.
- SSH auth-failure errors now suggest concrete next steps (`--user <name>`, `--key <path>`, `--password`) instead of surfacing the raw `crypto/ssh` `unable to authenticate, attempted methods [...]` message. Root SSH is disabled on most modern distros, so the default `root` user fails silently for first-time users — the new message points to that fix.
- `teploy rollback` previously called `docker start` on the stopped previous-version container. On Docker 29.5+ this silently fails to re-publish `HostConfig.PortBindings` (and detaches the container from custom networks) if another container had taken + released the host port in the interim — a common situation when rolling back after deploying a neighboring app that reused the port. Rollback now uses a new `docker.Client.Restart` path that inspects the stopped container, force-removes it, and `docker run`s a fresh one with the same image, network mode + aliases, port bindings, env, bind mounts, named-volume + tmpfs mounts, command, working dir, user, labels, memory + CPU limits, restart policy, and `--no-healthcheck NONE` marker. End-to-end verified on Docker 29.5.2 (single-process + multi-process apps, with and without `ingress: external`).
- Config: `tls:` combined with `ingress: external` is now rejected at validation. With external ingress, the user's CF Tunnel / nginx / ALB handles TLS termination — the cert + key would be uploaded to the server but never wired into a Caddy block. Silent no-op caught upfront rather than silently wasting upload bandwidth.

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
