# Open audit items

Unresolved findings for this repository from the ChatGPT-led audit series.
Pass 1-5 (2026-09-09 through 2026-09-11, register: teploy-neutron-lullmail
expanded audit) closed fully below. Pass 6 (2026-09-17, 78 findings F01-F78,
pinned at 7d62778) is recorded beneath it: every P0/P1 contained defect is
fixed; what remains open is the deferred architectural tail (the two
upstream/owner items closed 2026-09-17 — rulesets live, actions pinned).
Round 2 (2026-09-17, 60 findings TCL-01..TCL-60,
pinned at 1a8ea32) is recorded at the bottom: 24 findings closed with
contained fixes (several narrowing pass-6 deferrals), the rest deferred —
almost all of them the same architectural tail pass 6 already carries, now
with the round-2 evidence folded in.

Open items: pass-6 deferred tail (29 sub-items across 24 findings), plus
the round-2 residual tail itemized in that section. The 2 upstream/owner
items are closed (below). The two upstream items received from
teploy-dash's 2026-09-17 pass are closed below.

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
| F64 | P1 | fixed | 6a7d642 + live rulesets — release pipeline runs CI as a required verify job; org layer closed 2026-09-17: ruleset 23638983 (main: required check `test`, strict up-to-date, no force-push/deletion) + 23638984 (v* tags: creation/update/deletion/non-FF blocked, maintainer-only bypass) |
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
- F63 — Supply-chain pinning. Workflow action pins landed 2026-09-17
  (reviewed release SHAs, see upstream section); trivy is not referenced
  by any workflow in this repo, so no CI image pin applies. Remaining
  open: installer digests (tracked with TCL-54).
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

## Pass 6 — upstream / owner (closed 2026-09-17)

- F64 — CLOSED: repository rulesets live on github.com/useteploy/teploy-cli
  (api.github.com/repos/useteploy/teploy-cli/rulesets). 23638983
  "main-protection" (branch, active, refs/heads/main): required status
  check `test` (the CI workflow's job, confirmed as the reported context
  on main) with strict up-to-date policy; deletion and non-fast-forward
  blocked; no bypass actors. 23638984 "vtag-protection" (tag, active,
  refs/tags/v*): creation, update, deletion, and non-fast-forward all
  blocked; sole bypass actor im-tyler (always). A v-tag can no longer be
  pushed, moved, or deleted except by the maintainer — the
  any-v-tag-republishes-latest hazard is closed at the org layer too.
- F63 — CLOSED for workflows: every `uses:` ref pinned to the tag's
  reviewed release commit — actions/checkout v4.4.0
  (11d5960a326750d5838078e36cf38b85af677262), actions/setup-go v5.6.0
  (40f1582b2485089dde7abd97c1529aa768e1baff),
  goreleaser/goreleaser-action v6.4.0
  (e435ccd777264be153ace6237001ef4d979d3a7a) — resolved via
  git/refs/tags and dereferenced/verified against the tagged commits. No
  trivy reference exists in this repo's workflows (the README trivy gate
  is teploy's server-side scan, not CI), so there is no scanner image to
  digest-pin. Installer-digest pinning remains with TCL-54.

## Upstream from teploy-dash (2026-09-17 pass) — received and fixed

teploy-dash's own 2026-09-17 audit (its register, pass 6 at 46902d1)
recorded two defects owned by THIS repo (its UPSTREAM-1 from its A23,
its UPSTREAM-2 from its A36). Both fixed here; its register was not
edited from this side.

- UPSTREAM-1 (dash A23) — secrets in argv: `env set` values, template
  `--var` values, and KV values traveled in the teploy argv, visible in
  the host process list; dash cannot fix that alone (it redacts known
  values and already pipes the registry password via stdin). FIXED in
  cb7c0fc: a stdin secret-input contract — `env set KEY --stdin` and
  `kv set KEY --stdin` read the value verbatim (kv never echoes it
  back), `template deploy/install --var-stdin` reads a JSON object that
  overrides `--var`. Reads bounded at 1 MiB, NUL rejected, argv forms
  unchanged. Dash can capability-detect the flags before switching.
- UPSTREAM-2 (dash A36) — atomic server rename/update: dash's rename was
  remove+add across two CLI processes — non-atomic, and it lost
  tags/vpn_ip, which `server add` cannot set (dash now rejects
  destination collisions and restores on failure, but the metadata loss
  was CLI-owned). FIXED in 72c57f9: `server rename` moves the entire
  record in one commit (typed ErrServerExists on collision, verified
  no-op on same name), `server update` changes only passed flags (empty
  value clears, tags never touched), and every servers.yml mutation now
  commits via sibling-temp + fsync + rename instead of an in-place
  truncating write.

Gates at the closing commits: `go vet ./...` clean; `go test ./... -race`
all packages ok. No push performed.

## Round 2 (2026-09-17, TCL-01..TCL-60, pinned at 1a8ea32) — record

Report reviewed finding-by-finding against the source; no false positives
were found, but several pass-6 closure claims were genuinely incomplete at
the sink (TCL-01 vs F07, TCL-02 vs F03/F06, TCL-20 vs F47, TCL-22 vs F44,
TCL-21 vs F45, TCL-29 vs F23/F69). Contained fixes landed in 18 commits;
everything else defers onto the standing pass-6 architectural tail (the
rationale there still holds) or onto the new items below.

### Round 2 — fixed (contained)

| ID | P | Where |
|---|---|-------|
| TCL-01 | High | dde2989 — Deploy acquired its own mkdir lock and then DeployLocked acquired it again (non-reentrant: every normal deploy failed "already in progress"; the stateless mock let repeated mkdirs succeed, which is why CI stayed green). DeployLocked no longer locks; Deploy/autodeploy ensure the app dir exists before acquisition |
| TCL-02 | High | dde2989 — same-version cleanup re-inventoried by version label AFTER the replacement started (same label → the sweep removed the just-deployed generation). Predecessors are snapshotted after the renames but before any new container starts; cleanup touches only that snapshot (F06 removed-worker property preserved) |
| TCL-06 | High | 2877955 — rollback reads state and resolves its target under the app lock, not before it |
| TCL-07 | Med | 2877955 — host-ingress displacement captures only RUNNING containers of the authoritative current version; stopped history is no longer recorded as displaced and resurrected on failure |
| TCL-11 | High | 74a0509 — Restart quotes every inspect-derived argument (workdir, user, network, aliases, -p binding, restart policy) at the shell boundary |
| TCL-16 | High | 8f39d41 — health URL built with JoinHostPort (IPv6 bracketed), host validated fail-closed (IP/localhost), path request-URI shaped, one quoted curl --url arg, --globoff, per-attempt deadlines; checkTCP validates its host |
| TCL-18 | Med | c36ffd9 — deploy Config.validate enforces container-port/replica bounds, host-ingress single replica, non-negative stop timeout; config 'port' range-checked at parse |
| TCL-19 | Med | dde2989 (partial) — predecessor cleanup stop/remove failures are reported, not silently dropped. Structured degraded outcome + LogEntry.Image population: still open |
| TCL-20 | High | 5bf5594 — loadBalancerBlock renders the validated configured health path (was hardcoded health_uri /up — F47's fix never reached the rendering sink) |
| TCL-21 | High | 5bf5594 — both delivery digests required non-empty (equal-empty no longer reads as delivered); a post-reload verification failure restores the previous Caddyfile so disk matches what the container serves |
| TCL-22 | High | 5bf5594 — an unterminated managed marker fails closed instead of trimming through EOF |
| TCL-23 | High | 5bf5594 — the legacy lb-<app> block is migrated only when its own site address proves it serves this app's hosts; a real app named lb-<app> is never touched |
| TCL-25 | High | 5bf5594 (partial) — repeated maintenance-on no longer overwrites the stashed original route. Generation-scoped maintenance + security-envelope preservation: still F48 |
| TCL-26 | High | ea4ce31 — no more recursive chown of /deployments; ownership is set non-recursively on the two control-plane directories and checked. App-data ownership is an invariant |
| TCL-27 | High | ea4ce31 (partial) — the pre-recreate detailed mount inventory fails closed (inspect error and JSON parse error both abort); the stub Caddyfile is written only on confirmed absence. Compensated migration transaction: deferred (F67 tail) |
| TCL-29 | High | 9c86f4d + 5fcd08f — secret Get/Remove/Rotate/List/ensureKey and accessory credential/.env reads classify absence by an always-exit-0 absent/present framing; transport failures are errors, never ErrNotFound; decrypt captures stdout/stderr separately so age warnings cannot contaminate plaintext |
| TCL-30 | High | ac2ffea — mergeSecretVaultRefs allocates and returns the merged map; vault-only apps (nil secrets map) no longer panic |
| TCL-35 | High | c36ffd9 — accessory volume keys validated as identifiers, container destinations must be absolute |
| TCL-38 | Med | 4888719 — static tree hash v3 emits a typed record per entry including directory PATHS (v2 counted directories only; differently-named empty dirs collided) |
| TCL-42 | High | fd2ec15 (partial) — redis backup/restore refuse AOF-enabled instances; the pre-stop recovery copy is mandatory when a dump exists. Same-second LASTSAVE ambiguity and persistence-status-acknowledged proof: open |
| TCL-43 | High | fd2ec15 — recovery-incomplete restore failures retain their artifacts (typed marker; callers' run cleanup no longer deletes what the error calls "kept"); the fully-recovered branch states honestly that staging was discarded |
| TCL-46 | Med | fd2ec15 — ValidateSchedule rejects descending/wildcard ranges; SetSchedule reads the crontab with its exit status checked and only the canonical no-crontab message starts from empty. Host-wide flock: still F39 |
| TCL-52 | Med | 9772dad + 9895ba9 — RsyncTarget re-derives the host from the parsed endpoint (:22 stripped, IPv6 bracketed exactly once); rsync -e commands are quoted element-wise (ExternalSSHCommand) |
| TCL-53 | High | 4550b35 — streamImage owns an explicit pipe, starts the consumer first, closes parent ends, waits concurrently, cancels+reaps the peer on either failure, and reaps on Start failure |
| TCL-55 | Med | 1a9c70a (partial) — TCP dial bounded (15s) even with an unbounded context; explicit --key read/parse/passphrase errors surface instead of falling through to default identities. Session-open bounding (needs a dedicated connection per session) and TOFU enrollment serialization: deferred |
| TCL-56 | High | 5bf45a0 (partial) — non-purge removal keeps secrets/ and .env beside volumes/ and accessories/, and the deletion is checked. Locked/journaled retirement and purge coverage of named volumes: deferred |
| TCL-58 | Med | 1a9c70a (partial) — the update sanity run is context-bounded and must report the downloaded release's version; the permission-denied hint no longer points at the temp file the defer deletes. Downgrade policy, signature/provenance, Windows replacement: F62 tail |

### Round 2 — deferred (architectural / product), with rationale

Standing pass-6 entries still cover these; the round-2 evidence is folded
into each rather than duplicated as new work items.

- TCL-03 — F04 (generation-scoped identities / RouteSwitch handoff) + F21
  (publish recreate strategy). Contained pieces already landed: candidate
  name dedup (F03), publish+replicas rejected at validation.
- TCL-04 — F08 (attempt-scoped immutable artifacts under one lease).
- TCL-05 — F16 (fencing/renewal). Containment added this round: the caddy
  lock release is detached/bounded so cancellation cannot strand it.
- TCL-08 — F05/F35 tail (durable journal). Contained pieces landed this
  round: cleanup failure reporting, caddy verify-failure compensation.
- TCL-09 — F14/F13 (immutable per-release execution record).
- TCL-10 — ingress-transition transaction (caddy→host/external leaves the
  old route behind). Needs F04's generation handoff; a plain "reject
  ingress changes" would break the documented host-migration flow. Folded
  into F04.
- TCL-12 — F22, narrowed: docker-run channels COULD lift env to --env-file
  (Restart -e from inspect is now the registered contained follow-up);
  docker exec channels (BAO_TOKEN, MYSQL_PWD) remain blocked on
  container-side file plumbing shared with the engine images.
- TCL-13 — F20 (full RecreateSpec preservation; needs F14).
- TCL-14 — needs the recorded primary-port contract (F14 metadata);
  multi-port ambiguity is real but not fixable contained without it.
- TCL-15 — port allocation redesign (Docker-ephemeral publish + inspect).
  With F14.
- TCL-17 — F47 tail (explicit HTTP/TCP/auto probe modes; the 404/3xx TCP
  fallback is documented deliberate compat).
- TCL-24 — F49 tail (foreign-block adoption by brace counting; parser/
  adapt-API based adoption is the fix).
- TCL-28 — F50 (split the public static tree from /deployments).
- TCL-31 — env-encoder unification across accessory/seal writers. The
  app env writer validates records (F73); accessory credential values are
  generated (no newlines possible) — contained follow-up, registered.
- TCL-32 — strict ${VAR} resolution (fail on unset). Product decision:
  would break deploys that currently rely on empty expansion; needs an
  explicit opt-in syntax. Owner decision.
- TCL-33 — F24 (resumable OpenBao Setup; mandatory persistence landed).
- TCL-34 — OpenBao agent readiness gate + token-sink isolation (new
  lifecycle surface; shares F24's step-journal design).
- TCL-36 — F69 tail (credential rotation workflow, GID/nonzero-limit
  drift detection).
- TCL-37 — F05/F37 tail (readiness-gated accessory upgrade with verified
  recovery).
- TCL-39 — static route-policy restore needs F13/F48; renderer input
  hardening (header-name grammar, fallback charset) registered as the
  contained follow-up inside that item.
- TCL-40 — restore under the app lock + writer quiescence (F37-adjacent;
  the orchestrated quiesce/cutover boundary is new lifecycle surface).
- TCL-41 — F37 (engine-specific consistency contract; verify-backup
  exists as the correctness gate today).
- TCL-44 — F37/host-helper (constrained extractor, entry policy, bounds).
- TCL-45 — scheduled/manual backup unification (one engine + artifact
  schema + private workspaces + protected credentials). Architectural.
- TCL-47 — engine auth adapters + S3 session tokens. Medium; with F37.
- TCL-48 — F42 (durable webhook queue).
- TCL-49 — F40 (pin webhook builds to the event's commit).
- TCL-50 — F60 (complete-plan fingerprint vs display digest).
- TCL-51 — F57 (presence-aware overlay semantics).
- TCL-54 — platform parity per builder (nixpacks --platform), DetectAt
  stat distinction, pinned installer. Medium; registered with F63's
  supply-chain work.
- TCL-57 — F62 (bounded extraction).
- TCL-59 — F65 (stateful fakes + integration matrix). This round's new
  tests are behavioral where feasible (real shell for the crontab logic,
  real subprocesses for the transfer stall, filesystem-backed fake for
  the maintenance stash) — the prefix-response mock remains the gap.
- TCL-60 — F63/F64 owner items (pinned actions/digests, branch rulesets).
  Closed 2026-09-17: workflow action pins landed; rulesets 23638983 +
  23638984 live (pass-6 upstream section).

Gates at the closing commits: `go vet ./...` clean; `go test ./... -race`
all packages ok. No push performed.
