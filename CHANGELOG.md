# Changelog

All notable changes to teploy are documented here. Format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

## [0.1.29] - 2026-08-24

### Fixed
- Accessories whose image runs as a non-root user (Nucleus runs as uid 10001)
  crash-looped on their FIRST start: docker created the missing bind-mount
  directory root-owned and the engine could not open it. The ownership
  reconcile that already covered upgrades now runs on every start — teploy
  creates the volume directories itself and chowns them to the image's user
  from inside a throwaway container, so it works for a non-root deploy user
  without sudo. Found by `teploy template install teploy-ship` on a fresh
  server.

## [0.1.28] - 2026-08-24

### Changed
- `teploy template deploy|install` — `--domain` is now optional for
  `ingress: host` templates, which publish on `bind:port` and have no domain to
  give (teploy-ship is the first such template). The pairing is validated after
  render against the template's actual ingress mode, so the error names the
  rule instead of demanding a flag the template cannot use. New `--port` flag
  overrides the template's host port; on `template deploy` it also patches the
  written teploy.yml, because the file is what `teploy deploy` re-reads — an
  override that lived only in memory was silently ignored on the next step.

## [0.1.27] - 2026-08-20

### Added
- `teploy build` — build the app's image and stop there. No container is
  replaced, no route changes, no state is written; the single postcondition is
  that the image named on the last line exists on the server. `--json` prints
  `{image, version, built}` and nothing else on stdout.

  It closes a real hole. `teploy preview deploy` runs an image that must ALREADY
  exist at the current git hash — it falls back to `<app>-build-<hash>` and never
  builds one. The only thing that produced such an image was `teploy deploy`,
  which also replaces the running production containers, so "build this branch
  and look at it on a preview URL" was impossible without shipping the branch to
  production first. The command's own help said to do exactly that.
- `teploy preview deploy --image` — run a specific tag instead of re-deriving
  `<app>-build-<git hash>`. Without it, a build and the preview that consumes it
  agreed only because both read the same working directory, which fails silently
  the moment they run from different checkouts or at different commits.

## [0.1.26] - 2026-08-15

### Added
- `memory:` and `cpu:` limits for apps and each accessory, validated at parse
  time against docker's own syntax. The plumbing was complete and unreachable:
  `docker.RunConfig` carried the values and `deploy.go` passed them, but no yaml
  key ever set them, so no user could cap any container. Validating at parse
  time matters because docker rejects a malformed limit when the container
  *starts* — during a deploy that is after the old container is gone, so a typo
  like `8gb` was an outage. It is now a config error that names the offending
  accessory.

  This matters most for accessories. A storage engine's own memory budget is its
  accounting of its own allocations, so an engine that under-counts grows
  straight past it — one reached 30 GB RSS with a 16 GB budget set and the host
  OOM killer took an unrelated service down with it. Only the cgroup limit is
  enforced by the kernel. Limits are also fixed at container creation, so setting
  `memory:` on an already-running accessory warns and names the command that
  applies it rather than silently doing nothing.
- `teploy health --app <name> --host <server>` — the last read-only command that
  still required a `teploy.yml` in the working directory now reads server state
  like `status`, `stats` and `logs`, which is what lets teploy-dash drive it.

### Fixed
- Health checks probed `localhost` regardless of the `bind:` address. Docker
  publishes a container on exactly the address it is given, so an app with a
  specific bind is not reachable at localhost — the probe never connected,
  retried to the timeout, and failed a deploy whose container was perfectly
  healthy. Because `ingress: host` deploys by recreate, stopping the old
  container first, the failure was not a no-op: the deploy tore down the new
  container and left **nothing running**. Setting a bind address turned every
  deploy into a full outage.

  The same defect sat on the rollback and start/restart paths, where it is
  worse — a rollback that cannot health-check its target aborts, so the escape
  hatch failed exactly when it was needed. Fixed at all four call sites; the
  probe now resolves the address from the bind host, and the paths with no
  config in hand read it back from the running container.
- App-level `memory:`/`cpu:` never reached the container. `deploy.Config` is a
  separate struct the CLI populates field by field and nothing copied the two
  across, so both ends were correct, every unit test on either side passed, and
  a user setting `memory: 1g` got a container with no limit and no warning.
  Accessories were unaffected — they pass their config straight through, which
  is why verifying the accessory path live looked like proof the whole feature
  worked. The regression test guards the seam rather than either end: every
  `deploy.Config` field with an identically-named `AppConfig` field must
  actually be assigned.
- `cache:` was silently ignored for container apps. Only `StaticBlock` consumed
  `opts.Cache`, so a static-site deploy honoured the rules while every
  reverse-proxied and load-balanced app dropped them — `reverseProxyBlock` and
  `loadBalancerBlock` did not take a cache argument at all. The config parsed,
  validated and merged cleanly, and then produced nothing in the Caddyfile.

  The failure mode is the bad kind: no error, no warning, and a `teploy.yml` that
  reads as if caching is configured. Found on a real deploy that declared
  `"/assets/*": "public, max-age=31536000, immutable"` and was serving its HTML,
  stylesheet and JS with no `Cache-Control`, no `ETag` and no `Last-Modified` —
  so every navigation refetched the entire page. It presented as a slow app, and
  the app was not slow: it rendered in ~2 ms and spent the rest on the wire.

  Cache rules now render for all three block types.

- Cache rules are emitted in a stable order. The original loop ranged over a Go
  map, whose iteration order is randomised, so consecutive deploys that changed
  nothing still produced a different managed block. That made a Caddyfile diff
  useless for spotting genuine drift — which matters here, because a diff of that
  file is the tool for catching exactly this class of bug.

- Deploy and backup shell commands were interpolated unquoted, SSH host-key
  verification silently fell back to `InsecureIgnoreHostKey` on a missing
  `$HOME` or an unwritable `known_hosts`, and volume/accessory tar restores
  extracted straight into a live path. Commands are now consistently
  shell-quoted, both host-key paths fail closed, and a restore extracts to a
  staging directory and only promotes on success. `AcquireManualLock` no longer
  leaves an orphaned non-expiring lock when the metadata upload fails, and the
  documented install commands map `aarch64` -> `arm64` and verify the release
  SHA-256 against `checksums.txt` before extracting.

## [0.1.25] - 2026-07-27

### Fixed
- `teploy rollback` moved a `ingress: host` app to a random ephemeral port. The
  host port is fixed by config, so every version shares it and the current
  version's port always equals the target's — avoiding it made the recreate
  reallocate on *every* rollback rather than only on a genuine collision. A
  rollback that was supposed to restore service was what broke it. The fixed port
  is now preserved, and freed the way `deploy` already does it: the current web
  containers stop before the target starts, because two containers cannot bind
  one fixed port. That ordering was the other half of the bug — rollback used
  blue/green order and only appeared to work because the port was being
  reallocated. A failed health check now restores the containers it displaced,
  so a failed rollback leaves the app where it started rather than down.
- Redeploying a version that `rollback` left stopped failed with docker's raw
  "name is already in use". Rollback deliberately keeps superseded containers, so
  the ordinary roll-back-then-fix-forward sequence hit this every time. A stopped
  container holding the name is now removed and the run retried once; a running
  one is refused with a clear message, since that means the exact version is
  already live.

## [0.1.24] - 2026-07-26

### Added
- Webhook deliveries are now signed. Each carries
  `X-Teploy-Timestamp` and
  `X-Teploy-Signature: sha256=hex(HMAC-SHA256(secret, timestamp + "." + body))`,
  byte-identical to teploy-observe's and teploy-dash's scheme, so a receiver of
  all three writes one verifier. Signing the timestamp together with the body is
  what lets a receiver bound replay. Configure via `notifications.secret` or,
  preferably, `TEPLOY_WEBHOOK_SECRET` — `teploy.yml` is committed, and a signing
  secret in version control is not a secret. An unset secret sends unsigned,
  exactly as before.
- `backup schedule` reports whether its failure alert will be signed, so a
  verifying receiver rejecting an unsigned alert doesn't look like a wrong
  secret.

### Fixed
- `rollback` and `stop`/`start`/`restart` read only the legacy
  `notifications.webhook` key while `deploy`/`backup`/`preview` went through the
  multi-channel notifier. An install configured entirely with
  `notifications.channels` therefore received deploy events and silently never
  received a rollback — the one event you most want to hear about. All paths now
  use the same notifier, and event filters still apply, so this does not widen
  what existing channels receive.
- Scheduled-backup failure alerts were the one delivery path that could not be
  signed: the signature covers a timestamp, `date +%s` contains a `%`, and
  crontab treats `%` as a newline escape. The alert now lives in a script at
  `/deployments/<app>/backup-alert.sh` (mode 0700) that cron invokes, where `%`
  has no special meaning. Falls back to an unsigned delivery if `openssl` is
  absent, because a delivery that arrives and is rejected tells you something
  and silence tells you nothing.

## [0.1.23] - 2026-07-24

### Added
- `teploy drift --app <name> --host <server>`: drift detection without a
  `teploy.yml`. With no manifest to compare against, drift is derived from
  what the server itself records — containers of the deployed version that
  are no longer running, and containers of other versions still running.
  That covers manual stops and stale versions left behind, and it lets the
  dashboard report drift for apps it has no config for. JSON output carries
  `mode` (`manifest` or `state`) so consumers know which comparison ran;
  state mode also states its limitation, since replica-count drift is only
  visible to the manifest.

## [0.1.22] - 2026-07-23

### Added
- Machine-readable observation commands for automation and the dashboard:
  `teploy app list` (every app deployed on a server, with per-process
  container detail) and `teploy server status <server-or-host>` (resources,
  Docker inventory, and parsed Caddy routes). Both emit stable JSON under
  `--json`, contract-tested so consumers can pin to the shape.
- `teploy deploy [server]` accepts the server as a positional argument, so a
  delegating caller can target a server without a `teploy.yml` in scope.
- `--project-dir` global flag: run as if teploy had started in that
  directory, so a caller never depends on its own working directory.
- Canonical manifest model with revision semantics, the shared vocabulary
  for desired/applied/observed reconciliation.

### Changed
- Server-side state writes are atomic (write-temp then rename), so
  concurrent operations can never observe or leave a half-written state file.

## [0.1.21] - 2026-07-22

### Added
- `teploy remove` (alias `destroy`) — deploy's inverse, completing the app
  lifecycle. Stops and removes the app's containers, removes its Caddy route,
  and deletes its deploy state. Volumes, accessory data, and running accessory
  containers are preserved unless `--purge`. `--redirect <url>` leaves a
  permanent redirect for the app's domains as a plain unmanaged Caddy block
  that later teploy operations never touch. Idempotent; `--yes` and `--json`
  for automation; skips the route step on `ingress: host` servers.
- Zero-config first run: `teploy deploy` with no config offers the init flow
  inline on a TTY, writes the resulting `teploy.yml`, and continues deploying.
  Non-TTY behavior is unchanged (hard error, now with a `teploy init` hint).
- Deploy failure diagnosis: failed deploys now print a rule-based
  `Likely cause` + `Try:` under the container logs — OOM kills, missing env
  vars, database connection failures, wrong `port:` vs what the app actually
  listens on, 127.0.0.1-only binds, slow boots, disk-full, and bad
  entrypoints. Deterministic and local; no AI, no network calls.

- Release pinning: `teploy pin [version]` / `teploy unpin <version>` /
  `teploy pins`. A pinned version is never removed by `keep_versions`
  auto-pruning, so a known-good rollback target survives outside the
  retention window. Pins are stored server-side, so the CLI, dashboard, and
  auto-deploy all honor the same set.
- Monorepo path filtering for auto-deploy: an `autodeploy.paths` block in
  `teploy.yml` restricts which pushes redeploy an app — a push deploys only
  if it touched a matching file (trailing `/**` and `path.Match` globs).
  Declarative, and fail-open when a push payload has no reliable file list
  (never silently skips a real change).

### Changed
- `teploy init` no longer pre-fills an `app.example.com` domain. An empty
  domain now generates a valid `ingress: host` config (prompting for the
  port), and the server prompt lists known `servers.yml` names.

## [0.1.20] - 2026-07-15

### Added
- `teploy plan` / `teploy drift` / `teploy heal` — read-only deploy dry-run diff, live-vs-declared drift detection (`--exit-code` for CI), and bounded self-heal (host-side probe, restart-in-place with backoff, systemd timer, opt-in `heal.conf`). Web processes only; accessories excluded.
- `teploy kv` — shared Nucleus-backed KV store (`get`/`set --ttl`/`del`/`exists`/`incr --by`/`list <glob>`) for cross-app config and flags. Trusted-domain only: one global keyspace, prefixes are hygiene, not isolation.
- `rollout:` — staged multi-server deploys. A `canary: "N"` or `"P%"` wave deploys first and rolls back on failure without touching the rest of the fleet; `max_failures` sets a tolerance budget for the main wave, with a named-straggler exit (never a silent mixed-version fleet) when the budget is exceeded.
- `teploy secret` — full OpenBao integration under the existing secret command (`--provider local|openbao`): setup with auto-unseal (static-env, KMS, or Transit seals), per-app AppRole least-privilege policy, `put`/`get`/`list`, deploy-time `secret:name#field` env injection, dynamic DB credentials with an Agent-sidecar for auto-rotation, static-role rotation of an existing DB user, multi-node Raft HA (`--replicas`), and continuous audit streaming into Observe's tamper-evident trail (`secret audit enable/disable`). Replaces the earlier standalone `teploy vault` command — OpenBao is not HashiCorp Vault, and the naming now matches every other provider-abstracted command (`network --provider`, etc.).
- `teploy network grant/grants/revoke` — just-in-time mesh access: ephemeral, tagged, TTL-bound pre-auth keys for Tailscale or Headscale, no permanent credentials to leak or revoke by hand.
- `setup --harden` now also installs auditd (root/sudo execve plus writes under the Teploy-managed tree) and enables sudo I/O logging for `sudoreplay`-replayable sessions. The sudoers drop-in is visudo-validated before install so a malformed rule can't lock sudo on the box.
- `env_files:` — SOPS+age encrypted dotenv/YAML files merged into the container env at deploy time. File-based, at-rest secrets with no daemon required.
- `scan: true` — server-side Trivy vulnerability gate; blocks a deploy on fixable CRITICAL findings (`--ignore-unfixed` keeps unpatchable base-image CVEs from wedging every release).
- `firewall:` — per-app IP allow/deny lists, user-agent blocking, and a request-body size cap, rendered as Caddy directives that execute before the reverse proxy.
- `access:` — self-hosted inbound gate: `basic_auth` (bcrypt-hashed) or `forward_auth` delegation to an external identity proxy (Authelia, oauth2-proxy, any OIDC gateway).
- S3-compatible backup endpoints (MinIO, B2, R2) via `--endpoint`; accessory `command:` override (the missing primitive for MinIO/ntfy accessories) and `publish:` port mappings; backup retention (`teploy backup prune`, `--keep-last`/`--max-age-days`, also usable on the scheduled cron path); self-contained scheduled accessory backups (`--local --app`, no `teploy.yml` needed server-side); `teploy accessory verify-backup` restores the latest backup into a throwaway scratch container and proves it's actually usable (real restore + row/key counts, not just "the archive exists").
- `type: ntfy` notification channel; deploy and rollback events now emit to Observe's audit trail via a new `audit:` config block.
- `--role`/`--tag` deploy targeting; best-effort (non-fail-fast) fleet rollback so one server's rollback failure can't strand the rest of the fleet.

### Security
- The rsync channel used for static-site uploads now mirrors the control connection's host-key policy instead of unconditionally accepting new keys, closing a downgrade relative to the stricter default.
- `setup --harden`'s fail2ban no longer bans the operator's own SSH source — loopback, the Tailscale CGNAT range, and the live setup session's own IP are always exempted. Closes a real lockout seen in the wild (a management IP banned for 24h after two failed pubkey attempts).

### Fixed
- Image pull is now skipped when the image already exists on the server, unblocking locally-built or `docker load`-ed images with no registry.
- Backups and `verify-backup` no longer leave partial archives in `/tmp` after a failed dump or upload.
- Generic volume backups no longer fail on a live-file tar warning from a WAL rotating mid-read — the archive is a crash-consistent snapshot, and `verify-backup`'s scratch-container boot remains the actual correctness gate.

### Docs
- Resilience guide: N+1 topology, durable-state rules, and a human-confirmed dead-server rebuild runbook.
- CI/CD deploy recipe for Forgejo Actions and GitHub Actions, plus the no-secrets autodeploy-webhook alternative.
- OpenBao/secret feature docs (HA, seals, static-role rotation, audit streaming) and a Cloudflare caveat note for `firewall:`'s `remote_ip` matching.

## [0.1.19] - 2026-07-07

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
- VPN integration in setup (Tailscale / Headscale / Netbird).
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
