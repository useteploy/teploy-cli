# Open audit items

Unresolved findings for this repository from the ChatGPT-led audit series.
Pass 1-5 (2026-09-09 through 2026-09-11, register: teploy-neutron-lullmail
expanded audit) closed fully below. Pass 6 (2026-09-17, 78 findings F01-F78,
pinned at 7d62778) is recorded beneath it: every P0/P1 contained defect is
fixed; what remains open is the deferred architectural tail and two
upstream/owner items.

Open items: 29 deferred sub-items across 24 findings + 2 upstream/owner.

## Resolved from this register

- useteploy__teploy-cli-01, -02, -03 — fixed 2026-09-11 in
  `fix(template)`: YAML-safe variable rendering, path-keyed duplicate
  generated secrets, bounded/validated registry fetch (d4fa5af).
- useteploy__teploy-cli-08, -09 — fixed 2026-09-11 in `fix(backup)`:
  registry-port image parsing, .env archived/restored at its app-level
  location with a recoverable pre-restore copy (b2f465d).
- useteploy__teploy-cli-14, -15 — fixed 2026-09-11 in `fix(config)`
  together with the compose-side image-name parsing twin of -08
  (976207b).

## Pass 6 (2026-09-17, F01-F78) — fixed

| ID | P | Status | Where |
|---|---|-------|-------|
| F01 | P0 | fixed (containment) | e84e634 — DecryptAll filters VAULT_ROOT_TOKEN/RECOVERY_KEYS/SEAL_KEY/SEAL_KEY_ID; separation of the management namespace into its own store is the deferred tail (below) |
| F02 | P0 | fixed | e6eba37 — move-aside and copy-in are separate phases with distinct recoveries; a partial move-aside moves saved entries back WITHOUT deleting still-live originals |
| F03 | P1 | fixed (narrow) | 36a79cf — same-version rename dedupes candidate names (replicas==1 double-processing deleted the live predecessor). Full generation-ID naming: deferred (F04) |
| F05 | P1 | fixed (partial) | 36a79cf + 47c033d — cleanup installed before first container start, detached bounded recovery context, displaced host-web restored on every early exit; accessory Upgrade propagates stop/remove failures. Durable operation journal for SIGKILL: deferred |
| F06 | P2 | fixed | 36a79cf — old-workload cleanup matches the teploy.version label across the container inventory, so removed workers are stopped |
| F07 | P1 | fixed | 36a79cf + 5a4c470 — Deploy acquires the lock around a new DeployLocked entry point; the webhook path (which locks before fetch) uses it |
| F09 | P1 | fixed | 5a4c470 — fleet deploy.Config carries Bind/Publish/Memory/CPU/CaddyExtra/Cache/Firewall/Access |
| F10 | P1 | fixed | 5a4c470 — --image/--version flow into the fleet path; prebuilt images version from the image ref; DetectAt + context/dockerfile honored; ensureImage parity |
| F11 | P1 | fixed | 36a79cf — Rollback runs under the app lock |
| F12 | P1 | fixed | 36a79cf — target inventory preflight before freeing the fixed port; displaced workload restored on every post-displacement failure; host-ingress state-commit branch actually restores and says what happened |
| F13 | P1 | fixed (partial) | 5a4c470 — --app validated, domain required only for Caddy ingress, ingress from stored state, static state-only rollback explicitly refused with direction. Target-release spec restoration: deferred (needs F14) |
| F15 | P1 | fixed | 24ad88e + 36a79cf — Read distinguishes confirmed-missing from every other failure; deploy/static/rollback/drift propagate it |
| F17 | P1 | fixed (partial) | 36a79cf + 47c033d — Restart binds quoted + immutable image ID; remote docker build / nixpacks args quoted. Command-as-array redesign: deferred |
| F18 | P1 | fixed (partial) | 5a4c470 + 36a79cf + e84e634 — --version/--to grammar-validated (CLI + library), static --to hex grammar, app names validated on state-only paths, secret keys grammar-checked |
| F19 | P1 | fixed | 36a79cf — Restart recreates from the container's immutable top-level image ID, not the mutable Config.Image tag |
| F22 | P1 | fixed (partial) | e84e634 + 2a04c41 + 9bf93f0 — secret Set streams plaintext via stdin, accessory env rides a 0600 --env-file, Upload is mode-safe. Remaining argv exposure (BAO_TOKEN / MYSQL_PWD in docker exec, Restart -e from inspect): deferred — docker exec has no --env-file |
| F23 | P1 | fixed | e84e634 — exact-byte reads (no TrimSpace), typed ErrNotFound, List error propagation, temp+rename Set; ensureSecret regenerates only on confirmed absence |
| F24 | P1 | fixed (partial) | e84e634 — recovery-keys/root-token persistence mandatory (was ignored), initialized fast path fails loudly when the stored root token is unavailable. Step-wise resumable Setup: deferred |
| F25 | P1 | fixed | 9bf93f0 — accept-new fails closed on unreadable known_hosts, revoked keys, and every mismatch; only a genuinely unknown host enrolls |
| F26 | P1 | fixed | 9bf93f0 — handshake bounded by deadline + context-close; RunStream/RunInput/Upload already had select-based cancellation |
| F27 | P1 | fixed | 9bf93f0 + 390f849 — all transfer channels (sync, streamImage, layer transfer, static rsync) enforce strict verification with accept-new mirroring the control connection |
| F28 | P1 | fixed | 9bf93f0 — Upload streams via stdin into a umask-077 mktemp sibling, applies mode before content, renames atomically |
| F29 | P2 | fixed | 9bf93f0 + 390f849 — bare IPv6 bracketed for dial; ExternalSSHArgs/RsyncTarget normalize ports/IPv6/keys across channels; static rsync passes the configured identity |
| F30 | P1 | fixed | 9bf93f0 — x/crypto v0.48.0 -> v0.57.0 (covers GO-2026-6355 + GO-2026-5017; both list NewClientConn, which this repo calls); go 1.26 + CI bumped; `govulncheck ./...`: 0 reachable |
| F31 | P1 | fixed | e6eba37 — MySQL/MariaDB restore assigns its s3Key/restorePath (was deterministically broken) + post-switch artifact-spec guard |
| F32 | P2 | fixed | e6eba37 — legacy .env probe tests the FILE deployments/<app>/.env, not the directory |
| F33 | P1 | fixed | e6eba37 — volume restore runs from one private mktemp workspace; no stale sibling env |
| F34 | P1 | fixed | e6eba37 — volume + accessory backup artifacts built under 0700 mktemp workspaces, cleaned on every exit |
| F35 | P1 | fixed (partial) | e6eba37 — recovery dir retained until the .env commit succeeds. Durable multi-phase journal: deferred |
| F36 | P1 | fixed | e6eba37 — redis backup fails closed on BGSAVE refusal/poll exhaustion/copy failure |
| F38 | P1 | fixed | e6eba37 — redis restore decompresses first, saves the current dump, restores + restarts the original on failure |
| F39 | P2 | fixed (partial) | e6eba37 — ValidateSchedule parses five fields with ranges/steps. Crontab edit under a host lock: deferred |
| F40 | P1 | fixed (partial) | 390f849 — only a push to the watched branch triggers; pings/tags/other branches/deletions are acknowledged no-ops. Pinning the build to payload.After: deferred |
| F41 | P2 | fixed | 390f849 — dedup keys on the HMAC-authenticated body digest; delivery ID is secondary |
| F43 | P2 | fixed (partial) | 390f849 — MaxBytesReader 413, empty-secret startup refusal, server timeouts + header cap. Private listener/Unix-socket scope: deferred (Caddy bridge topology) |
| F44 | P1 | fixed | 36a79cf — markers match as complete lines; nested/duplicate/unmatched markers refused; web vs web-staging collision closed |
| F45 | P1 | fixed (partial) | 36a79cf — delivery verification fails closed; reload rollback runs detached+bounded and surfaces restore failures. Exact managed-block snapshot/restore: deferred (F48) |
| F46 | P1 | fixed | 36a79cf — Caddyfile written via random sibling in /deployments/caddy + atomic rename (same filesystem) |
| F47 | P1 | fixed (partial) | 36a79cf + 5a4c470 — LB active checks probe the configured health path (default /health, was hardcoded /up); rollback forwards the health spec. Explicit HTTP/TCP probe modes: deferred |
| F51 | P1 | fixed | 36a79cf — v2 length-prefixed tree hash (v1's NUL framing was ambiguous); full digest authoritative |
| F52 | P1 | fixed | 36a79cf — hashDir rejects symlinks/special files before upload |
| F53 | P2 | fixed | 36a79cf — current symlink swapped via sibling + mv -Tf (ln -sfn unlinks first) |
| F54 | P2 | fixed | 36a79cf — prune protects current + previous and reports removal failures |
| F55 | P1 | fixed | 390f849 — skip-transfer requires identical immutable image IDs (docker image inspect), not tag existence |
| F57 | P1 | fixed (partial) | 2a04c41 — overlays merge Access/Firewall/Secret/Audit/Context/Dockerfile/whole Health. Presence-aware clearing (false/empty overrides): deferred |
| F58 | P2 | fixed | 2a04c41 — unreadable candidates error; single YAML document enforced; TOML unknown keys rejected like YAML |
| F59 | P1 | fixed | 5a4c470 — ${VAR} expansion applies exactly once to explicit YAML env templates, before literal file/secret values merge; serializer no longer expands; env keys + NUL validated. (Nuance: the serializer itself only ever expanded appEnv — the corruption path was runDeploy merging file values INTO appCfg.Env first.) |
| F60 | P2 | fixed (partial) | 2a04c41 — manifest records memory/cpu/publish/cache. Protected per-release full execution spec: deferred |
| F61 | P1 | fixed | 5a4c470 — replaceBinary via sibling-temp + chmod + sync + rename; ETXTBSY and truncated-binary states eliminated |
| F62 | P2 | fixed (partial) | 5a4c470 — downloads bounded at 256MB. Update-selection policy (prerelease/downgrade) + extraction bounds: deferred |
| F64 | P1 | fixed (partial) | 6a7d642 — release pipeline runs CI as a required verify job. Branch/tag rulesets: upstream/owner (below) |
| F66 | P1 | fixed | 5a4c470 — webhook path resolves env_files from the fetched checkout with the same single-pass expansion |
| F67 | P1 | fixed (partial) | 5a4c470 — failed Caddy inventory reads abort before destructive decisions; a stopped proxy is reported and started, not declared running. Adopted non-default /data//config sources during recreation: deferred |
| F68 | P1 | fixed | 5a4c470 — manual backup/restore read image+env from the running container; config fallback only when free of auto/secret: references |
| F69 | P1 | fixed (partial) | 2a04c41 — credential-store read failures propagate; auto-generation only on confirmed absence. Explicit rotation flow + running-config drift reporting: deferred |
| F70 | P2 | fixed | 2a04c41 — connection URLs built with net/url (reserved characters safe) |
| F71 | P2 | fixed | 2a04c41 — isImageType delegates to docker.ImageRepository (registry-port refs classified correctly) |
| F72 | P2 | fixed | 2a04c41 — command_args (argv form) with command/command_args mutual exclusion |
| F73 | P1 | fixed | 2a04c41 — per-app env staging + typed existing-.env reads + atomic whole-file rewrite |
| F74 | P2 | fixed (partial) | 2a04c41 — explicit uid:gid honored; UID-only changes owner alone (no invented group). Fresh-vs-existing recursive-chown policy: deferred |
| F75 | P2 | fixed | 5a4c470 — decryption subprocesses context-bound with WaitDelay |
| F76 | P2 | fixed (partial) | 5a4c470 — state errors propagate; all-containers-removed reports as drift. Static drift remains explicitly "not implemented" (honest no-op kept) |
| F77 | P3 | fixed | 6a7d642 — dead Node/Playwright UI scaffold removed (tested the removed `teploy ui`) |
| F78 | P1 | fixed | 24ad88e + 36a79cf — ReadPins typed absence, atomic pin writes, pruning fails closed when pins are unreadable |

No outright false positives in this pass. One imprecision (F59, noted above)
did not change the fix.

## Pass 6 — deferred (architectural / product-behavior)

One rationale covers each entry: the fix requires a cross-cutting redesign
or an explicit product decision rather than a contained correction, and
landing it hastily inside an audit sweep would risk the availability this
CLI exists to protect. Each has a contained mitigation in place where the
defect could corrupt data today.

- F04 — Generation-scoped container identities + a RouteSwitch handoff
  boundary (candidates unroutable before readiness, external-ingress
  policy). The contained F03 fix removed the deletion defect; the alias
  handoff redesign spans deploy, rollback, and Caddy together.
- F08 — Attempt-scoped immutable artifacts (build dirs, env files, TLS)
  under one lease. The F07 lock split serializes container mutation; full
  artifact generation needs F04's generation IDs.
- F13 — State-only rollback restoring a complete target-release spec
  (incl. static serving config). Requires F14's per-release metadata.
- F14 — Per-release immutable metadata store keyed by target ID (beyond
  the current one-level PreviousRelease). Schema + migration design.
- F16 — Owner-token fenced locks with renewal (age-based breaking
  retained: TTLs are documented and manual locks never expire; a fencing
  redesign can strand apps mid-incident if it ships wrong).
- F17 — Commands as argv arrays end-to-end (Cmd stays a deliberate
  operator-authored shell string; validated sources are quoted at the
  sinks).
- F20 — Full RecreateSpec/Engine-API recreation preserving every inspect
  field (immutable image ID + quoted binds landed; full spec needs F14).
- F21 — Explicit recreate strategy for `publish` (product decision: the
  current failure mode is a clean pre-mutation docker-run error, not an
  outage).
- F22 — Remaining argv exposure in `docker exec` channels (BAO_TOKEN,
  MYSQL_PWD, Restart -e): docker exec has no --env-file; needs
  container-side file plumbing shared with the engine images.
- F24 — Fully resumable step-wise OpenBao Setup (mandatory persistence
  landed; step journal is new surface).
- F35 — Durable multi-phase restore journal (retention-until-commit
  landed).
- F37 — Per-engine RestoreEngine (validate -> scratch -> verify -> cut
  over). `accessory verify-backup` already proves archives in scratch;
  the orchestrated boundary is new lifecycle surface.
- F39 — Crontab edit under a host-side flock.
- F40 — Pinning webhook builds to the payload's exact commit (fetch +
  ancestry policy).
- F42 — Durable webhook queue with per-app workers (in-proc model is
  bounded by the per-app lock + content dedup).
- F43 — Listener scoped to a private address/Unix socket reachable only
  by the Caddy bridge.
- F45 — Exact managed-route snapshot/restore transaction (needs F48).
- F47 — Explicit HTTP/TCP/auto probe modes (compat fallback is
  deliberate and documented).
- F48 — Maintenance mode preserving TLS/access layers (needs a structured
  route representation, i.e. Caddy's adapt API — string fragments cannot
  reconstruct policy faithfully).
- F49 — Parser-based foreign-block adoption plans (whole-block-only rule
  landed; partial matches now fail loudly at reload instead of deleting
  another host's routes).
- F50 — Splitting the public static tree from /deployments (filesystem
  layout migration across live servers; deliberate ops project).
- F56 — Explicit pull policy (the warned local fallback is a deliberate
  out-of-band image story; changing it silently breaks offline deploys).
- F57 — Presence-aware overlay semantics (explicit clearing of
  maps/lists, null handling) — schema design decision.
- F60 — Protected full execution spec per release (public redacted
  snapshot keeps its current role).
- F62 — Update selection policy (prereleases/downgrades) + extraction
  member bounds.
- F63 — Supply-chain pinning (Actions to reviewed SHAs, installer
  digests, scanner image pin). Needs maintained, reviewed pins — not
  fabricated ones; tracked with F64 as release-hygiene work.
- F65 — Broader real-filesystem/Docker integration test matrix. Every
  fixed finding above landed with a mock-level regression test; the
  full fault-injection matrix is ongoing infrastructure work.
- F67 — Preserving adopted non-default /data//config sources during
  Caddy recreation (foreign mounts are preserved; teploy-managed
  destinations are recreated from the current model).
- F69 — Engine-aware credential rotation + running-config drift
  reporting.
- F74 — Fresh-vs-existing recursive chown policy (explicit migration
  approval flow).

## Pass 6 — upstream / owner

- F64 — branch protection + tag rulesets for main: GitHub repository
  settings (api.github.com rulesets returned empty 2026-09-17). The
  repo-side gate (release `needs: verify`) landed in 6a7d642; enabling
  required checks/rulesets is an owner action on
  github.com/useteploy/teploy-cli.
- F63 — goreleaser/Action version pins and trivy image digests require
  maintained reviewed digests; same owner workflow as above.
