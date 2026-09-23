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
with the round-2 evidence folded in. Round 4 (2026-09-19, 63 findings
T01-T63, pinned at c30e4b3, record at the bottom) closed 35 findings with
contained fixes; its residual tail is the standing architectural items with
round-4 evidence folded in, plus the new T28/T40/T59 deferrals.

Open items: the pass-6 deferred tail minus the F14 family (resolved
2026-09-18) and the F08/F16/F48/F49/F57 family (resolved 2026-09-18,
bottom section: attempt-scoped artifacts, fenced locks, structured Caddy
routes, strict-env — folding in round-2's TCL-04/TCL-05/TCL-24/TCL-32/
TCL-51), the round-2 residual tail itemized in that section, and the
dependent designs F04 (and its TCL-10/TCL-15 dependents) that stay
deferred with their annotations — plus the round-3 deferrals recorded at
the bottom (mostly the same architectural tail, with round-3 evidence
folded in) and the round-4 deferrals recorded at the bottom (standing tail
+ round-4 evidence, plus the new T28/T40/T59 items). The 2 upstream/owner
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
  handoff redesign spans deploy, rollback, and Caddy together. Unblocked
  by F14 (the per-release record can now key generation-scoped identities
  and attempt artifacts), design remains.
- F08 — Attempt-scoped immutable artifacts (build dirs, env files, TLS)
  under one lease. The F07 lock split serializes container mutation; full
  artifact generation needs F04's generation IDs. F14's record exists as
  the keying surface; the lease/generation design remains.
- F13 — RESOLVED 2026-09-18 with F14 (see the F14-family section at the
  bottom): state-only rollback restores the complete target-release spec,
  including static serving config.
- F14 — RESOLVED 2026-09-18 (see the F14-family section at the bottom):
  per-release immutable metadata store keyed target+release at
  /deployments/<app>/meta/<hash>.json, with live-container backfill so
  existing installs converge.
- F16 — Owner-token fenced locks with renewal (age-based breaking
  retained: TTLs are documented and manual locks never expire; a fencing
  redesign can strand apps mid-incident if it ships wrong).
- F17 — Commands as argv arrays end-to-end (Cmd stays a deliberate
  operator-authored shell string; validated sources are quoted at the
  sinks).
- F20 — RESOLVED 2026-09-18 with F14 (see the F14-family section at the
  bottom): full RecreateSpec recreation preserving every inspect field
  the docker CLI can represent.
- F21 — RESOLVED 2026-09-18 with F14 (see the F14-family section at the
  bottom): publish takes the explicit recreate strategy on deploy and
  rollback.
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
- F40 — Pinning webhook builds to the payload's exact commit: LANDED
  2026-09-22 (see the C02 commit-pinned builds slice at the bottom —
  fetch + verify + reset to payload.after/checkout_sha, loud refusal when
  the commit is unfetchable). The standing F42/A37 remainder (queue
  durability beyond the ledger, listener scope) is unchanged.
- F42 — Durable webhook queue with per-app workers (in-proc model is
  bounded by the per-app lock + content dedup).
- F43 — Listener scoped to a private address/Unix socket reachable only
  by the Caddy bridge.
- F45 — Exact managed-route snapshot/restore transaction (needs F48's
  structured route representation — LANDED 2026-09-18, see the family
  section; the transaction design itself remains open and can now be
  built on ParseSites/ExtractPolicy).
- F47 — RESOLVED 2026-09-22 (see the C03 readiness-modes slice at the
  bottom): explicit `health.mode: http | tcp | auto` with config-grammar
  validation, pre-gate surfacing, record/receipt forwarding, and the
  auto fallback named as documented compat.
- F48 — RESOLVED 2026-09-18 (see the F16/F08/F48/F49/F57 family section
  at the bottom): maintenance preserves the site's TLS directive and
  access gate, extracted from the parsed current block; plus a pre-write
  adapt gate run by the SERVER's own caddy binary.
- F49 — RESOLVED 2026-09-18 (see the F16/F08/F48/F49/F57 family section
  at the bottom): parser-based foreign-block adoption with structured
  partial matches (the foreign block keeps its remaining hosts and
  directives); unparseable Caddyfiles abort the edit pre-write.
- F50 — Splitting the public static tree from /deployments (filesystem
  layout migration across live servers; deliberate ops project).
- F56 — Explicit pull policy (the warned local fallback is a deliberate
  out-of-band image story; changing it silently breaks offline deploys).
- F57 — RESOLVED 2026-09-18 as an opt-in (see the F16/F08/F48/F49/F57
  family section at the bottom): --strict-env makes an explicitly-empty
  map/list in a destination overlay CLEAR the base field. The default
  stays presence-blind by the register's compat decision; promoting
  strict to the default (if ever) is the remaining owner decision.
- F60 — Protected full execution spec per release (public redacted
  snapshot keeps its current role). Largely delivered 2026-09-18 by F14's
  per-release record (full execution spec, 0600, server-side, embedded
  RecreateSpec); if the intent included a reader-facing access contract
  (CLI/dash surfacing of the record), that design remains.
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
  name dedup (F03), publish+replicas rejected at validation. F21 RESOLVED
  2026-09-18 (F14-family section); the F04 remainder is unblocked by F14,
  design remains.
- TCL-04 — RESOLVED 2026-09-18 with F08 (family section at the bottom).
- TCL-05 — RESOLVED 2026-09-18 with F16 (family section at the bottom);
  the caddy-lock containment this round added is subsumed by the fenced
  app lock (the caddy file lock stays short-lived and unfenced by
  design).
- TCL-08 — F05/F35 tail (durable journal). Contained pieces landed this
  round: cleanup failure reporting, caddy verify-failure compensation.
- TCL-09 — RESOLVED 2026-09-18: F14/F13 landed (immutable per-release
  execution record; state-only rollback restores from it — F14-family
  section below).
- TCL-10 — ingress-transition transaction (caddy→host/external leaves the
  old route behind). Needs F04's generation handoff; a plain "reject
  ingress changes" would break the documented host-migration flow. Folded
  into F04. Unblocked by F14, design remains.
- TCL-12 — F22, narrowed: docker-run channels COULD lift env to --env-file
  (Restart -e from inspect is now the registered contained follow-up);
  docker exec channels (BAO_TOKEN, MYSQL_PWD) remain blocked on
  container-side file plumbing shared with the engine images.
- TCL-13 — RESOLVED 2026-09-18: F20 landed (full RecreateSpec preservation
  — F14-family section below).
- TCL-14 — RESOLVED 2026-09-18: the recorded primary-port contract landed
  (ports with primary designation in the F14 record; health checks and
  Caddy targets resolve through it — F14-family section below).
- TCL-15 — port allocation redesign (Docker-ephemeral publish + inspect).
  Unblocked by F14 (the record now carries the resolved port allocation
  per release), design remains.
- TCL-17 — RESOLVED 2026-09-22 with F47 (C03 readiness-modes slice at the
  bottom): the 404/3xx TCP fallback is now the NAMED `auto` compat mode,
  selectable and surfaced, no longer an undocumented default.
- TCL-24 — RESOLVED 2026-09-18 with F49 (family section at the bottom):
  adoption is parser-based; brace counting is gone.
- TCL-28 — F50 (split the public static tree from /deployments).
- TCL-31 — env-encoder unification across accessory/seal writers. The
  app env writer validates records (F73); accessory credential values are
  generated (no newlines possible) — contained follow-up, registered.
- TCL-32 — RESOLVED 2026-09-18 (family section at the bottom): the opt-in
  landed as --strict-env / serve --strict-env / TEPLOY_STRICT_ENV=1;
  default behavior unchanged per this entry's product decision.
- TCL-33 — F24 (resumable OpenBao Setup; mandatory persistence landed).
- TCL-34 — OpenBao agent readiness gate + token-sink isolation (new
  lifecycle surface; shares F24's step-journal design).
- TCL-36 — F69 tail (credential rotation workflow, GID/nonzero-limit
  drift detection).
- TCL-37 — F05/F37 tail (readiness-gated accessory upgrade with verified
  recovery).
- TCL-39 — static route-policy restore needs F13/F48; F13 landed 2026-09-18
  (recorded serving config restored) and F48 landed 2026-09-18 (structured
  route representation — ExtractPolicy — is now available for the
  policy-layer preservation). The orchestrated restore design and the
  renderer input hardening (header-name grammar, fallback charset) remain
  the open follow-ups inside this item.
- TCL-40 — restore under the app lock + writer quiescence (F37-adjacent;
  the orchestrated quiesce/cutover boundary is new lifecycle surface).
- TCL-41 — F37 (engine-specific consistency contract; verify-backup
  exists as the correctness gate today).
- TCL-44 — F37/host-helper (constrained extractor, entry policy, bounds).
- TCL-45 — scheduled/manual backup unification (one engine + artifact
  schema + private workspaces + protected credentials). Architectural.
- TCL-47 — engine auth adapters + S3 session tokens. Medium; with F37.
- TCL-48 — F42 (durable webhook queue).
- TCL-49 — F40 (pin webhook builds to the event commit): LANDED
  2026-09-22 (C02 commit-pinned builds slice, bottom).
- TCL-50 — F60 (complete-plan fingerprint vs display digest).
- TCL-51 — RESOLVED 2026-09-18 with F57's opt-in (family section at the
  bottom).
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

## F14 family (2026-09-18) — resolved

The per-release metadata architecture landed in three commits, closing
F14 and everything the register had blocked on it.

- **F14** (`97e6d03`) — `internal/releasemeta`: one immutable JSON record
  per (app, release id) at `/deployments/<app>/meta/<hash>.json`, atomic
  write at 0600, keyed per-target like state.json/pins (the server a
  command talks to IS the target). Records the full resolved spec: image
  ref + digest, env-file references, volumes, labels, ports with the
  TCL-14 primary designation and fixed/ephemeral flags, replicas/
  processes/cmd, resource limits, stop timeout, bind, health gate, Caddy
  edge config (TLS/extra/cache/firewall/access), static serving config,
  and the primary web container's full RecreateSpec. Writers: deploy paths
  only (a same-version redeploy is the one allowed rewrite); readers never
  mutate. Migration: a confirmed-missing record is backfilled from the
  live containers on first use ("release-0", flagged Backfilled) —
  existing installs converge without a redeploy; backfill records only
  what inspect can prove and leaves env-file refs/health/caddy empty for
  the legacy fallbacks.
- **F20** (`b3e2ed1`) — `internal/docker` RecreateSpec: InspectRecreate
  captures the complete docker-run-representable config in one inspect;
  RenderRecreateArgs is a pure renderer; Restart = Inspect + Recreate with
  the same signature. Fields previously dropped on every recreate: log
  rotation, entrypoint overrides, stop timeout/signal, extra hosts,
  sysctls, tmpfs, capabilities, security opts, read-only/privileged,
  secondary networks. Multi-element entrypoints that differ from the
  image's own fail closed (the CLI cannot represent argv there) instead of
  being silently joined.
- **F13 / F21 / TCL-14** (`7314da7`) — rollback and publish recreate
  restore from the record, not from reverse-engineered current state:
  recorded health gate, domain, TLS/caddy_extra/cache/firewall/access,
  ingress mode; recorded primary container port drives health probes
  (HostPortFor) and Caddy upstreams; publish apps take the explicit
  recreate strategy on deploy (displace current web first, restore on
  failure — the host-ingress contract) and rollback (free the fixed ports
  first, stop avoiding them). `teploy rollback --app <static-app>` is
  complete via StaticDeployer.RollbackStateOnly; no record means an
  explicit redeploy-first request, never a guess.

Degradation posture (deliberate): the store is an overlay, not a new
dependency — an unreadable record or failed backfill warns and falls back
to the historical inspect-driven path; only state-only static rollback
(which has nothing to fall back to) fails closed. Record-write failures
warn after the live commit rather than aborting a routed deploy.

Still deferred, now annotated: F04/TCL-10/TCL-15 are unblocked by F14
(designs remain — generation handoff, ingress-transition transaction,
Docker-ephemeral port allocation); F08 gains F14 as its keying surface;
F60's protected-spec-per-release is largely delivered by the record (a
reader-facing access contract, if wanted, is the remaining design);
TCL-39's F13 half landed, F48 remains. F48/F49 (Caddy adapt-API route
representation), F16 (lock fencing), F57 (overlay semantics) stay
deferred as before — F14 does not unblock them.

Gates at the closing commits: `go vet ./...` clean; `go test ./... -race`
all packages ok. No push performed.

## F16 / F08 / F48 / F49 / F57 family (2026-09-18) — resolved

Four commits closing the architecture items the F14 keying surface
unblocked, plus the strict-env owner decision. Gates at the closing
commits: `go vet ./...` clean; `go test ./... -race` all packages ok. No
push performed.

- **F16 + TCL-05** (`3381947`) — `internal/state/lock.go`: every auto
  lock carries a unique owner token (the fencing token) and is renewed in
  the background every staleLockTTL/3; staleness is measured from the
  last renewal, so a live-but-slow deploy is never falsely broken — the
  stranding hazard the register warned about — while a dead holder still
  self-heals after the historical 30-minute window. Effect sites verify
  the fence before every effectful phase (deploy/rollback/static), and
  the atomic state commit (`state.WriteFenced`) renames under the guard:
  a broken holder's late write is refused with ErrFenceLost, never
  applied. Recovery paths (restoring displaced containers, route
  rollback, cleanup of one's own partial effects) are deliberately
  UNFENCED — refusing to clean up is how a fencing design strands an app
  mid-incident. The owner token doubles as the fencing token; a separate
  monotonic counter adds nothing in this topology (the .lock dir on the
  target is the single authority — refusal is exactly "does it still
  name us"). MockExecutor evaluates the guard against its recorded file
  state, so fence tests prove a refused effect never executes.
- **F08 + TCL-04** (`fd93d7a`) — `internal/releasemeta/attempt.go`:
  every deploy attempt mints (app, hash, random id) and writes its
  artifacts immutable in the releasemeta namespace — build context at
  meta/att/<hash>.<id>/build (rsync --link-dest against the previous
  attempt restores incremental transfer and hardlink-shares unchanged
  files), env file at meta/att/<hash>.<id>/env (the F14 record's
  EnvFiles now names bytes no later attempt can overwrite), TLS at
  /deployments/caddy/tls/att/<hash>.<id>/ (kept under the caddy tls dir
  — the one mount every custom-TLS server provably has; container path
  /etc/caddy/tls/att/…). The terminal deploy path acquires the fenced
  lease BEFORE artifact generation, so attempts serialize at the source;
  `teploy build` (lockless by design) builds into its own attempt dir
  and can no longer interleave with a deploy's rsync. PruneAttempts
  protects current + previous + pinned releases and fails closed on
  unparsable entries (F78 parity). Rollback/LB TLS uploads keep the
  legacy shared paths (nil attempt): pre-F14 records still reference
  them and recorded releases override from the record.
- **F48 + F49 + TCL-24** (`25f8118`) — `internal/caddy/routes.go` +
  `adapt.go`: a vendored structural Caddyfile parser (site blocks with
  verbatim bodies, global options, snippets, comments, quoted strings,
  multi-line backtick literals, heredocs; loud errors on unbalanced
  braces and top-level import). F49: foreign-block adoption is decided
  on the parsed structure — whole-block when all hosts are adopted,
  STRUCTURED PARTIAL when not (the foreign block keeps its remaining
  hosts and its directives; the duplicate-site-address reload failure is
  gone), managed regions never adopted, unparseable files abort the edit
  pre-write. F48: SetMaintenance extracts the current block's tls
  directive and basic_auth/forward_auth spans (ExtractPolicy) and
  carries them into the maintenance block — no more silent TLS/auth
  downgrade for the duration. The hard pre-write adapt gate runs the
  SERVER's binary (docker exec -i caddy caddy adapt over stdin); a LOCAL
  caddy is advisory only (version/module drift makes a local hard gate a
  false-positive machine — found live with rate_limit under stock
  caddy). Cross-checked against real caddy v2.10.2: parser host
  extraction agrees with adapt's JSON on every fixture class, and the
  F48/F49 outputs adapt cleanly (PATH-gated test; CI has no binary and
  skips — the stub-binary tests cover the local-adapt plumbing).
- **F57 + TCL-32 + TCL-51** (`4bb6a79`) — one opt-in flag, default off:
  `--strict-env` (persistent) fails the deploy listing every unset
  ${VAR} in teploy.yml's env: (terminal path, autodeploy serve flag, or
  TEPLOY_STRICT_ENV=1 for already-installed units) and makes an
  explicitly-empty map/list in a destination overlay CLEAR the base
  field (`env: {}` / `publish: []` / TOML `publish = []`). Default
  behavior byte-identical — the compat decision the register recorded.

F04 dependency annotations (stay-out honored; recorded for its design
pass): F08's attempt ids provide the artifact-side generation token F04
wanted, and F08's early-lease restructure (`deployAppConfig` acquiring
the fence before artifact generation) plus `DeployFenced`'s lock-handle
parameter are the seam a generation handoff grows from. F45 can now
build on ParseSites/ExtractPolicy. TCL-15's port allocation remains
independent (the F14 record carries the resolved allocation).

## Round 3 (2026-09-19, A01-A52, pinned at 0faf201) — record

Report reviewed finding-by-finding against the source at the pinned HEAD
(the register's closure claims were NOT taken as proof — several findings
genuinely landed as new defects on top of the F08/F16 work, and several
restated the standing architectural tail). 28 findings closed with
contained fixes across 8 commits; the remaining 24 defer onto the
standing pass-6/round-2 tail (evidence folded in) or onto the new items
noted below. No false positives found; A21 and A17 were scoped to their
legacy-fallback/boundary remainders (the record-driven TCL-14 paths
already fixed the primary behavior).

### Round 3 — fixed (contained)

| ID | Sev | Where |
|---|-----|-------|
| A01 | High | 4537b89 — attempt TLS paths are app-scoped (/deployments/caddy/tls/att/<app>/<hash>.<id>/); PruneAttempts sweeps only the pruning app's artifact and TLS roots; the legacy flat TLS root is never swept (a flat entry's owner cannot be proven — the flat sweep deleted OTHER apps' live certs whenever hashes differed) |
| A02 | High | 4537b89 — the attempt-prune protection window covers every release that still has containers plus current/previous/pins (keep_versions retention holds releases whose records/routes reference attempt-scoped TLS and env); inventory failure skips the prune |
| A03 | High | 4537b89 — an unreadable pin file SKIPS attempt pruning entirely (version-prune parity) instead of pruning with current+previous only |
| A04 | High | dd8a788 — ReleaseLockFenced runs the release under the holdership guard: after a takeover the stale holder's rm -rf is refused (fence-lost = success, the successor keeps its lock); nil handles keep the admin/unfenced release |
| A06 | High | dd8a788 — WriteFenced and renewal stage to unique owner-scoped siblings (state.json.tmp-<owner>-<nonce>), so a stale holder cannot clobber the successor's staging and ride its guarded rename into authority; renewal stages in the app dir, never recreating a removed .lock |
| A08 | High | 89625d3 — the same-version path refuses to force-remove a RUNNING _replaced container (the failed prior attempt's renamed SERVING workload) and aborts on unclassified rename failures with the source still present |
| A10 | High | 89625d3 — abortStateCommit's fixed-port branch restores the Caddy route for caddy+publish apps (it used to return with Caddy pointing at the removed candidate names); on route-restore failure the candidates are restarted rather than routing to nothing |
| A11 | High | 89625d3 — every abortStateCommit compensation runs on a detached bounded recovery context (a cancelled-context commit failure used to skip the stops/restarts via the dead ctx) |
| A13 | Med | 89625d3 — restoreDisplacedAndStarted itemizes every failed stop/remove/restart in output and error; 'restored' is false when ANY displaced restart fails (was: true whenever zero candidates had started) |
| A14 | Med | 2faab70 — a failed docker run reconciles the candidate name (created-but-unstarted corpse removed so the next deploy cannot collide; a RUNNING container under the name is never touched) |
| A15 | High | 2faab70 — asset bridging builds the attempt's private tree (meta/att/<hash>.<id>/assets, seeded from the previous attempt with a real cp -a), extracts via docker create + docker cp (no image ENTRYPOINT runs), clones the volumes map before adding the mount — the shared live tree is never mutated pre-commit |
| A17 | Med | 2faab70 — Config.validate checks identity grammar (app/version/process), the ingress enum, and rejects publish+replicas>1; releasemeta.Path validates the app grammar; SplitHostPort rejects ports outside 1..65535; WriteFenced/ReleaseLockFenced verify the lease belongs to the app |
| A18 | Med | 2faab70 — ContainerPort==0 normalizes to 80 once at the top of DeployFenced and drives host ports, upstreams, diagnosis, and every create (no ':0' upstream) |
| A19 | Med | 784c955 — the primary -p binding is built by publishBinding (IP-validated, JoinHostPort-bracketed, port-ranged) and quoted — '::1:49152:80' concatenation is gone |
| A21 | Med | 784c955 — HostPort/InternalPort refuse containers with multiple DISTINCT ports instead of taking the first field (the release record's TCL-14 primary remains the authority); HostBindIP reports '' on mixed binds |
| A23 | Med | 2faab70 — workers must still be running (not exited/dead/restarting/unhealthy) one second after the detached run or the deploy fails with full cleanup; unreadable state inspects degrade to a warning |
| A25 | Med | 784c955 — PruneVersions counts a version pruned only when every container removal succeeded and returns the joined failures; the caller reports partial cleanup |
| A26 | Med | 89625d3 — logDeploy populates LogEntry.Image (closes TCL-19's open half) |
| A27 | High | 3b6025d — LocalExecutor.Upload is atomic (private sibling, chmod+fsync before publication, rename replacing a leaf symlink itself); the resident autodeploy path no longer runs on the un-hardened writer |
| A28 | High | 3b6025d — local commands run in their own process group with Cancel SIGKILLing the group and WaitDelay bounding the wait (descendants keeping pipes open used to block CombinedOutput indefinitely); non-unix fallback is explicit about the weaker guarantee |
| A32 | Med | 3b6025d — PublicKeyBytes derives the provisioning key from the requested private identity and verifies an existing .pub; PublicKeyPath no longer falls through to unrelated defaults for an explicit key; setup uses the derived key |
| A36 | Med | d2e2d76 — dedup persistence is serialized + atomic + error-reporting; the delivery-ID header is log metadata only (reused ID with different authenticated content no longer suppresses a distinct event) |
| A39 | High | 8bd4b70 — the .env commit is a set -eu script with a MANDATORY old-file recovery copy and both files staged as private same-filesystem siblings secured at 0600 before publication |
| A40 | High | 8bd4b70 — the redis restore AOF gate is a Go-level preflight requiring a proven 'appendonly no' reply (auth/transport/empty/unexpected all refuse before any stop or copy); the backup script's gate is strict under set -eu |
| A41 | High | 8bd4b70 — the redis restore snapshots the original dump AFTER the stop (the shutdown save is included), the copy is mandatory, and a failed final docker start invokes the same restore_original compensation as a failed install |
| A44 | Med | 8bd4b70 — backup ids carry a random 16-hex suffix (same-second S3 key collisions gone); ValidateDate accepts legacy and new ids with a real-date check; ordering preserved by the timestamp prefix |
| A46 | Med | 8bd4b70 — cron fields must be unsigned decimals; SetSchedule validates the schedule and rejects line breaks/NUL in command and marker at the sink |
| A51 | Med | d2e2d76 — PrefixWriter is mutex-protected with a 64 KiB fragment cap and surfaces write/flush failures; fleet slot acquisition is ctx-aware with a post-acquire re-check |
| A52 | Med | 2faab70 — DeployFenced resolves the immutable image ID once and creates every web/worker (and extracts assets) from it; the requested ref stays the recorded provenance; resolution failure warns and falls back |

Gates at the closing commits: `go vet ./...` clean; `go test ./... -race`
all packages ok. No push performed.

### Round 3 — deferred (standing tail, with round-3 evidence folded in)

- A05 — F16's remainder: a permanent server-side flock serialization of
  every lock transition and guarded effect (two contenders can still both
  read a stale owner; `grep owner; effect` is atomic per command but the
  multi-command phases are not). The landed owner-token fencing refuses
  stale effects; the full protocol is the redesign the register defers.
- A07 — F04/F05/F35: unfenced compensation is deliberate (register);
  generation/operation-scoped cleanup identities and the durable journal
  are the deferred design. A10/A11/A13's honest reporting now covers the
  contained half.
- A09 — F04 generation identity: same-version redeploys still rewrite the
  (app, hash) record — the documented immutability exception. Attempt ids
  (F08) are the keying surface for the generation-keyed store.
- A12 — F45 remainder: restorePreviousRoute still reconstructs from cfg +
  live inspect (now failing closed on ambiguous ports via A21); the exact
  receipt/compare-and-swap restore design remains open on
  ParseSites/ExtractPolicy.
- A16 — F04 external-ingress handoff (candidates reachable via the stable
  alias before readiness).
- A20 — TCL-15 port allocation redesign.
- A22 — RESOLVED 2026-09-22 (C03 readiness-modes slice at the bottom):
  F47/TCL-17 explicit probe modes landed; the 404/3xx TCP fallback stays
  as `auto`, the documented compat mode.
- A24 — F17 standing: Cmd remains a deliberate operator-authored shell
  string at the docker-run sink.
- A29 — TCL-55 session-open bounding (needs a dedicated connection per
  session).
- A30 — NEW deferral: unified structured executor output (CommandResult
  with separated stdout/stderr, truncation flags). Cross-cutting contract
  change over every caller; local/remote Run semantics documented as-is.
- A31 — TCL-55 TOFU enrollment serialization (cross-process known_hosts
  lock); the stale-snapshot window is narrower than the fixed
  fail-open-on-parse-error that F25 closed.
- A33 — F22: backup credentials in host-visible command text (AWS env
  assignments, MYSQL_PWD via docker exec -e); needs the container-side
  credential-file plumbing shared with the engine images.
- A34 — F42 durable webhook queue (ack-before-durable-job remains; A36
  closed the dedup-race half).
- A35 — F40 webhook build pinning to the event commit: LANDED 2026-09-22
  (C02 commit-pinned builds slice, bottom).
- A37 — F43 listener scope + operational bounds (graceful shutdown,
  bounded admission) — the durable queue (A34) is the prerequisite for
  honest shutdown semantics.
- A38 — TCL-40 restore under the app lease + writer quiescence.
- A42 — F37 per-engine validated cutover (SQL/Mongo in-place restore).
- A43 — TCL-44 constrained extractor/host helper (staging extraction
  still runs the host tar).
- A45 — F39 crontab edit under a host-side flock.
- A47 — F62/TCL-57 bounded update extraction.
- A48 — F62 update selection policy (downgrade on string inequality).
- A49 — F63/TCL-54 owner item: goreleaser `version: latest` and
  aquasec/trivy:latest need reviewed pins (real digests/versions the
  report deliberately does not invent); folded into the supply-chain
  owner entry with the installer-digest work.
- A50 — F65 real-filesystem/Docker integration matrix (this round's new
  tests remain mock-level; PrefixWriter/cancellation tests are behavioral
  with real processes).

## Round 4 (2026-09-19, T01-T63, pinned at c30e4b3) — record

Report reviewed finding-by-finding against the source at the pinned HEAD
(the register's closure claims were NOT taken as proof — several findings
landed as genuine defects on top of the newest surfaces, and several
restated the standing architectural tail). 35 findings closed with
contained fixes across 12 commits; the remaining 28 defer onto the standing
pass-6/round-2/round-3 tail (round-4 evidence folded in) or onto the new
items noted below. No false positives found; T38 was confirmed against the
post-A41 script (the pre-stop existence FLAG was the residual defect, not
the copy ordering); T07 confirmed A13's flag accounting never set restored
on success.

### Round 4 — fixed (contained)

| ID | Sev | Where |
|---|-----|-------|
| T02 | High | 41192f4 — the ambiguous-failure fallback of ReleaseLockFenced is a shell-level CONDITIONAL: the lock is removed only when its info still names the releasing owner (or is already gone); a successor's lock is never deleted by the old unconditional detached release |
| T06 | High | 34d6dc9 — every rollback route-phase failure (upstream-port inspection, SetRoute, SetLoadBalancerHealth) unwinds through the same cleanup as start/health failures: stop the uncommitted target, restore the displaced fixed-port workload |
| T07 | Med | 34d6dc9 — restoreDisplacedAndStarted sets restored on SUCCESSFUL restarts (an all-restored recovery no longer reports "no container is serving"); partial cleanup failures are joined into the returned error |
| T08 | High | c9260a7 — pin/unpin run under the app's fenced lock (the same one deploys/prunes hold) with release-id grammar validation at the command boundary |
| T10 | Med | 67c097b — the asset-bridge seed selector is mtime-ordered and skips attempts with no assets directory (the lexicographically-greatest pick could select an env-only attempt and silently seed nothing) |
| T11 | Med | 67c097b — asset_keep_days cleanup runs on the LIVE attempt-scoped tree (it had been a no-op since F08) and PruneAttempts bounds attempts per retained hash to the two newest |
| T12 | High | a83ff14 — InspectRecreate captures docker's EFFECTIVE top-level mounts: anonymous volumes (Dockerfile VOLUME) are preserved BY NAME, and an effective mount the CLI cannot represent fails the inspect instead of silently dropping storage; --mount values are CSV-encoded |
| T13 | Med | a83ff14 — the recreation renderer brackets IPv6 binds via net.JoinHostPort with validated ports/protocol ('::1:49152:80' concatenation is gone) |
| T15 | High | 1b267ec — ListContainers requests Labels as a JSON object via a custom --format; the comma-joined display string (whose values could forge reserved teploy.* labels) is no longer parsed for lifecycle decisions |
| T17 | Med | 1b267ec (half) — ImageExists distinguishes a proven "no such image" from daemon/permission failures (the old framing turned a broken daemon into a convincing cache miss). The deploy-path resolve-warns-and-fall-back stays deliberate (A52) |
| T19 | High | a83ff14 (recreation half, the TCL-12 registered follow-up) — resolved env rides a private 0600 on-target --env-file instead of -e argv; the docker-exec AWS/MySQL channels stay deferred (A33) |
| T20 | Med | 555581b (half) — health probes pass curl --noproxy '*' so ambient proxy configuration cannot hijack host-local readiness. The explicit HTTP/TCP mode redesign stays deferred (A22) |
| T21 | Med | 34d6dc9 — worker verification treats a persistently unreadable inspect as a deploy FAILURE after bounded retries (reversing A23's degrade-to-warning: unknown is not readiness) |
| T23 | Med | 9694108 — publish entries are parsed against a documented narrow grammar ([ip:]host:container[/proto], single ports, bracketed IPv6) at BOTH boundaries, duplicate host bindings (incl. wildcard-vs-specific) are rejected pre-mutation, and host-ingress conflicts with the fixed port fail at validation |
| T26 | High | 82d0ed3 — the webhook route is PERSISTED in the Caddyfile inside the app's managed site block from a per-app descriptor; every managed render (deploy/rollback/maintenance) re-applies it under the same lock + adapt gate + reload/verify transaction — the runtime admin-API injection could be erased by the very deploy it triggered |
| T27 | Med | 82d0ed3 — the route honors the configured listener port (9876 was hardcoded) and matches every configured domain (the comma list was one JSON host value) |
| T29 | High | 82d0ed3 — autodeploy Schedule/Unschedule read the crontab status-checked (only the canonical no-crontab message starts from empty) and the crontab -r fallback is gone |
| T30 | Med | 82d0ed3 — autodeploy Remove aggregates every step failure into an "incomplete" error; Status reports transport failures as errors, never as "inactive" |
| T31 | Med | 82d0ed3 — the resident path resolves relative TLS cert/key paths against the fetched checkout (the systemd unit has no WorkingDirectory) |
| T32 | Low | 82d0ed3 — the webhook secret is stored and HMAC-verified verbatim: setup rejects whitespace-wrapped secrets, serve refuses (with the reason) instead of trimming the key |
| T37 | High | 353dc93 — restore_original is defined and the baseline capture compensated AFTER the stop: a failed docker cp under set -e used to exit with Redis stopped and no restart attempted (behavioral tests drive the script under a real bash with a stub docker) |
| T38 | High | 353dc93 — the baseline is captured against the STOPPED container (docker cp), distinguishing "no such file" from every other failure — the old pre-stop existence flag missed the final RDB a graceful shutdown writes when none existed |
| T41 | High | a602edc — secret List runs a bare status-checked find and sorts in Go (the old find|sort pipeline without pipefail reported a failed listing as "no secrets"); listed names are grammar-validated |
| T45 | High | a602edc — every atomic publication renames with mv -fT (remote Upload, UploadAtomic, secret Set): a plain mv into a destination symlinked to a directory silently nested the file and left the destination unchanged |
| T46 | Med | a602edc (local half) — LocalExecutor.Upload fsyncs the containing directory after the rename. The remote-shell durability contract (fsync + parent sync over SSH) stays deferred |
| T48 | High | 555581b (narrowing A47) — update extraction is bounded and single-binary: declared sizes checked before reading, limited reads, non-regular/duplicate entries refused, entry count capped |
| T49 | Med | 555581b (half) — the updater derives its context from the Cobra command. The selection policy (downgrade/prerelease ordering, --allow-downgrade) stays deferred (A48) |
| T51 | High | 353dc93 — .teployignore EXTENDS the always-protected defaults (.env/.env.*/.git/node_modules); an unreadable ignore file is an error, never a silent defaults-only transfer |
| T53 | Med | 9694108 — basic_auth requires a COMPLETE structural bcrypt hash, forward_auth's verify URI must be request-path-shaped, copy_headers must be HTTP tokens, and the upstream URL rejects control characters (closes the TCL-39 renderer-input follow-up) |
| T56 | Med | 67c097b — releasemeta.Read validates the record's embedded App/Hash against the requested key; a copied or corrupted-but-valid record can no longer drive effects at a different release's spec |
| T57 | High | 5401f87 — a failed load-balancer update after a fully successful fleet wave is a nonzero exit ("backends deployed but load-balancer activation failed") |
| T58 | High | 5401f87 (half) — both fleet rollback waves run on bounded detached recovery contexts (a Ctrl-C no longer cancels the recovery itself into a no-op). The generation-identity half (compensating only the recorded predecessor) defers with the T04 family |
| T61 | Low | 555581b — CI and the release workflow are read-only by default; contents:write is granted only to the publishing job |
| T62 | High | 5401f87 (half) — maintenance on/off takes the app's fenced deploy lock, the --app path verifies the authoritative server ingress mode, and the stash is read/created/deleted inside the Caddyfile mutation transaction. The versioned-desired-state redesign stays deferred |
| T63 | Med | 34d6dc9 — the name-derived cleanup fallback retries the container inventory first (removed workers are invisible to name-derived retirement) and reports every fallback stop/remove failure |

### Round 4 — deferred (standing tail, with round-4 evidence folded in)

- T01 — A05's remainder (the grep-based guard is check-then-act at the
  multi-command phase granularity; the fenced single-command guards refuse
  stale effects but two contenders can still both read a stale owner). The
  permanent server-side serialization transaction is the redesign A05
  defers; T02's conditional fallback removed the worst unguarded deletion.
- T03 — the shared Caddy lock stays short-lived, ownerless, and unfenced BY
  DESIGN (the register's TCL-05 note); every Caddyfile edit now runs under
  the adapt gate + delivery verification, and the app-level fence covers
  deploy effects. The full target-side lock redesign folds into A05.
- T04 — A07/F04: operation-scoped receipts (attempt labels, container-ID
  receipts, generation comparison before compensation). T58's detached
  contexts and T06/T62's transactional cleanups cover the contained halves.
- T05 — A12's remainder: restorePreviousRoute still reconstructs the
  predecessor block from cfg + live inspect (now via A21's ambiguity-refusing
  port reads); the exact-block receipt/compare-and-swap restore design
  remains open on ParseSites/ExtractPolicy.
- T09 — A09: same-version redeploys rewrite the (app, hash) record (the
  documented immutability exception) and the record is written after the
  live commit (the deliberate degradation posture); attempt-keyed
  generation records remain the F04-adjacent design.
- T14 — F20's remainder: candidate-before-destructive recreate and the
  fields the docker CLI cannot round-trip (health checks beyond NONE,
  restart retries, DNS/devices/ulimits). T12/T13/T19 removed the silent
  DATA-loss halves (anonymous volumes, IPv6, env argv).
- T16 — A20/TCL-15: host-port preselection is not a reservation (ss-based
  allocation + Docker as final authority).
- T18 — A24/F17: Cmd stays a deliberate operator-authored shell string at
  the docker-run sink.
- T22 — A16: stable aliases expose external-ingress candidates before
  readiness (generation-scoped aliases need F04's handoff).
- T24 — A34: durable webhook job queue (ack-before-durable-job remains;
  A36's content dedup + T26's persisted routing cover the routing halves).
- T25 — A35: webhook builds fetched the watched branch HEAD, not the
  authenticated payload commit. LANDED 2026-09-22 (C02 commit-pinned
  builds slice, bottom).
- T28 — NEW deferral: the scheduled-redeploy cron script is a separate
  forked deployment engine (no lock, health gate, route/state/metadata
  commit). Unifying it behind the real deploy engine is the fix; whether
  to fail closed on `autodeploy schedule` until then is an owner product
  decision (it disables a shipped feature). T29's strict crontab handling
  removed the destructive halves around it.
- T33 — A37: listener scope, bounded admission, graceful shutdown with
  recoverable jobs (the durable queue is the prerequisite).
- T34 — A38/TCL-40: restore under the app lease + writer quiescence.
- T35 — A42: per-engine transactional consistency (SQL/Mongo staging +
  controlled cutover).
- T36 — A43/TCL-44: constrained extractor for restore archives (the host
  tar runs in a private staging tree today).
- T39 — TCL-42's open half: same-second LASTSAVE ambiguity and
  persistence-path discovery (dir/dbfilename assumptions).
- T40 — NEW deferral: a versioned whole-app disaster-recovery bundle
  (release records, secret stores + age identity, TLS references) is a
  product decision; today's archives are data-only by design.
- T42 — A30: errno-aware confirmed-missing reads (test -e folds EACCES
  into absence); needs the structured executor result.
- T43 — A30: local/remote executor output semantics (stdout/stderr split,
  no trimming) — the cross-cutting CommandResult contract.
- T44 — A29: session-open cancellation needs a per-command transport.
- T47 — A31: cross-process TOFU serialization of first-use host-key
  acceptance.
- T50 — A49 + the nixpacks curl|bash installer: reviewed pins/digests are
  owner items the register does not invent; the installer now joins them.
- T52 — TCL-54: nixpacks --platform parity and DetectAt's stat
  distinction.
- T54 — F57's owner decision: presence-aware overlay semantics beyond the
  strict-env opt-in.
- T55 — TCL-50/F60: the redacted manifest digest is not a complete-plan
  identity.
- T59 — NEW deferral: static publication hashes the mutable source before
  transfer and trusts an existing short-hash directory (snapshot +
  content-manifest verification design; concurrent source mutation is the
  precondition).
- T60 — A50: the fencing mock models a stronger atomicity guarantee than
  the real shell (this round's behavioral tests — the redis script under a
  real bash — are the pattern the integration matrix wants more of).

Gates at the closing commits: `go vet ./...` clean; `go test ./... -race`
all packages ok. No push performed. (Environmental note: Apple's Xcode 27
update landed mid-session and required license re-acceptance for
/usr/bin/git; the closing gates ran against the standalone Command Line
Tools git on PATH.)

## Product programme slices (2026-09-21) — Compose import contracts

Two C05 findings from the product evaluation's strengthened contract
probes (`_internal/evals/2026-09-21/`), fixed as bounded slices with
evidence. Base revision `8486355`; changes left uncommitted for review.

- **Compose port preservation** — `mapCompose` used the web service's
  ports only for candidacy and silently discarded them, so
  `ports: ['8080:3000']` imported with `Port=0` (deployed as `:80`,
  health check probing the wrong port). Ports now resolve through
  `composeAppPort` over the same narrow grammar as `ParsePublishSpec`
  (short strings or bare numbers; ranges and long-form objects refused
  naming the service; multiple distinct container ports refused as
  ambiguous; non-TCP entries preserved verbatim into `publish`). The
  Compose host-side binding is deliberately not preserved — teploy
  allocates host ports and routes via Caddy. The config→deploy hop was
  made observable by extracting `deployConfigFromApp` (both entry
  points now share one literal, covered by the strengthened
  `TestDeployConfigCopiesEveryMatchingAppConfigField` wiring guard,
  which previously saw only deploy.go's copy).
- **Independent-build refusal** — a service built from a different
  context than web's with no image was flattened into a same-image
  process (`jobs: build ./jobs` ran web's image under the jobs
  command — wrong code, right command, success reported). The import
  now refuses, deterministically naming every offending service and
  its build context, with the remediation (same context / prebuilt
  image / teploy.yml). Full multi-image build identity remains C05.

Gates: `go vet ./...` clean; `go test ./... -race` all packages ok;
strengthened probes — port and worker contracts PASS, both controls
PASS, the preview branch-identity probe remains a known failure (C06,
separate task). Mutation checks in a scratch copy: removing the port
assignment, substituting the host port, breaking the seam mapping, and
restoring the lossy flatten each fail the new regressions for the
intended reason. Real Docker port behavior remains a later journey
gate (J05); no remote deployment was performed. The preview collision
and full Compose breadth stay open under the product programme
(`_internal/TEPLOY_PRODUCT_EXCELLENCE_PROGRAMME_2026-09-21.md` C05/C06).

- **Compose field contract (preserve / translate / reject)** — the
  C05 defect class removed for the highest-impact fields: the importer
  decoded with non-strict yaml.Unmarshal, so unknown Compose keys were
  SILENTLY IGNORED — a file using healthcheck, networks, secrets,
  configs, profiles, deploy.resources or env_file imported
  "successfully" while dropping those semantics. The pass is bounded to
  the inventoried fields; the classification table is declared as the
  grammar in `TestLoadCompose_FieldClassificationInventory`
  (compose_test.go) and on the LoadCompose/composeAppPort doc comments.

  | Compose field | Decision |
  |---|---|
  | healthcheck | TRANSLATE (web): exec-form `["CMD","curl"/"wget",...,"http://localhost:<app-port>/<path>"]` → `health.path` + `health.interval_seconds`; `disable: true` / `test: ["NONE"]` → `healthcheck.web.disable` (--no-healthcheck). Workers: only the disabling forms translate; other tests rejected (no per-process HTTP gate). Accessories: ignored — inert (teploy supervises via `--restart always` + running-state, never queries docker health). timeout/retries/start_period deliberately NOT translated: compose timeout is per-probe, teploy `health.timeout_seconds` is the TOTAL gate window (translating would break slow starters); retries/start_period subsumed by that window. CMD-SHELL/string forms, non-HTTP probes, wrong port, https, non-localhost hosts, query strings, sub-second intervals: rejected naming the service. |
  | networks | REJECT except exact no-op (`[default]`, `{default: {}}`) — teploy runs every container on its own managed network |
  | restart | TOLERATE `always`/`unless-stopped` (teploy runs app containers `--restart unless-stopped`, docker.go; accessories `--restart always`; the `always` delta is only after manual stop + daemon restart, which teploy's lifecycle owns); `no`/`on-failure`/other REJECTED (crash-semantics change) |
  | env_file | REJECT (opaque file reference with compose-specific interpolation the importer cannot resolve; teploy `env_files` is a deliberate teploy.yml opt-in); empty tolerated |
  | secrets / configs | REJECT (no model); empty tolerated |
  | profiles | non-default-profile services SKIPPED entirely — `docker compose up` without `--profile` does not deploy them, so importing them would deploy something compose would not |
  | extends | REJECT (inheritance not losslessly resolvable) |
  | deploy | only no-op defaults tolerated (`{}`, `replicas: 1`, `mode: replicated`); resources/replicas≠1/global REJECTED |
  | labels | IGNORE (container metadata; teploy manages its own teploy.* labels) |
  | depends_on | TOLERATED deliberately: parsed, unused — teploy ensures every accessory is RUNNING before any app container starts (cli/deploy.go "Ensure accessories are running" step, cli/singledeploy.go), honoring the common ordering by construction; the delta (condition: service_healthy readiness gates not waited for) is documented in the table |
  | container_name | REJECT (teploy owns naming, {app}-{process}-{version}) |
  | hostname | REJECT (identity, no home) |
  | working_dir / entrypoint | REJECT (no model home — bake into the image) |
  | privileged / cap_add | false/empty tolerated; true/non-empty REJECTED (security-relevant; teploy runs unprivileged containers) |

  Accessories get the same field treatment as web (a network or
  privileged on postgres is refused exactly like one on web). Evidence:
  42 new subtests; TDD red recorded (every reject/translate case
  "imported successfully" against the old importer — the silent-ignore
  defect demonstrated live), then green after the fix; mutation check —
  making the healthcheck translation write nothing fails
  `TestLoadCompose_TranslatesHealthcheck` and the inventory row with
  `health.path = "", want /healthz` for the intended reason (reverted).
  All refusals are import-time (LoadCompose is pure; errors propagate
  through LoadApp fail-closed at deploy).

  Remaining open under C05: full Compose breadth (fields outside the
  inventory are still silently ignored — KnownFields-style strictness
  over the whole Compose schema), multi-image build identity,
  plan/apply. Base revision `566e291`; changes left uncommitted for
  review.

## Programme slice (2026-09-21) — C01 crash-recovery state table

Workstream C01 (P0): the implementation handoff's crash-recovery design
obligations landed as a bounded DESIGN+CODE slice. Base revision `f2e8c19`;
changes left uncommitted for review. No deploy code path was modified.

**Landed:**

- **The transition table as tested code** — `internal/deploy/recovery`:
  the eight lifecycle states (admitted → prepared → candidates-running →
  readiness-passed → traffic-switched → authoritative-state-committed →
  predecessor-retired → terminal-receipt-persisted), the transition
  lattice with per-transition durable-evidence citations (exact container
  names/labels, Caddy marker-block + reload + delivery receipts, fenced
  state.json rename, releasemeta record refs, attempt dirs), and
  `Decide(from, observation)` — a pure, total crash-window disposition
  function (RETRY / INSPECT / COMPENSATE / MANUAL) over tri-state
  evidence. Tested exhaustively: every state × every 3^10 evidence
  combination with cross-cutting safety invariants, plus canonical
  per-window dispositions and the handoff's named conflicts (candidate
  running with route never switched → INSPECT; predecessor already
  retired under uncommitted traffic → MANUAL; unknown container names →
  MANUAL). The exhaustive invariants caught two real rule-ordering
  defects during development (record/target mismatch upgrading the
  proven-dark window from MANUAL to INSPECT; unreadable side evidence
  overriding proven-dark) — the ordering is now R0-R7 with the
  proven-dark MANUAL ahead of both.
- **ADR** — `docs/C01_RECOVERY_STATE_TABLE.md`: mermaid lattice,
  disposition rules, the mapping onto the existing fenced-lock /
  releasemeta / attempt machinery (what already agrees), the multi-host
  rule (sequence of recorded outcomes + per-generation compensation; no
  global atomic commit), the lock-ordering rule (per-host app fences +
  short shared-proxy commit lock; never hold one host's lock waiting on
  another — current code complies), and the findings below.
- **Fault-prototype harness** —
  `internal/deploy/recovery/harness_integration_test.go`
  (`//go:build integration`, the repo's first integration-tagged test):
  drives a real SSH+Docker host (TEPLOY_FAULT_HOST/USER/KEY; skips with
  a clear message when unset) through the handoff's decisive scenarios —
  (a) a nohup'd docker effect landing after owner death and a genuine
  stale-break acquisition by a new owner (asserts the reconciliation
  decision differs from the quiescence assumption), (b) candidate
  running with no state/record (asserts INSPECT, never invented
  success), (c) a stale holder's guarded effect AND fenced state commit
  refused by the EXISTING fence machinery (ErrFenceLost, nothing on
  disk). Prints a scenario × observed × decision × correctness table.
  NOT executed in this slice (no fixture host available): it compiles
  (`go vet -tags integration` clean), its decision logic is the
  exhaustively-tested unit code, and it skips cleanly.

**Disagreements found between current code dispositions and the table**
(full detail with file:line in the ADR; these feed C01's implementation
slices):

- C01-1 lock acquisition is treated as quiescence — nothing reconciles
  the dead holder's in-flight effects after a stale break
  (state.go:479-516; deploy.go:414-453 handles only the same-version
  rename case).
- C01-2 pre-commit effects are check-then-act (`lk.Check` separate from
  the effect: deploy.go:570, 661, 712) — only WriteFenced composes
  guard+effect; a broken holder's candidate/route effects can land inside
  the new owner's window (A05/T01's consequence, now stated as a
  disposition).
- C01-3 the shared Caddy lock is ownerless/unfenced (caddy.go:684-697) —
  conflicting-route evidence (→ INSPECT) has no producer or consumer
  today.
- C01-4 no durable readiness receipt exists — ReadinessPassed is
  unobservable post-crash and collapses into CandidatesRunning's INSPECT
  (health.go; deploy.go:646-658).
- C01-5 the terminal receipt logs Success:true even when predecessor
  retirement partially failed (deploy.go:976-989 warnings +
  deploy.go:934 success log; LogEntry has no degraded field) — a
  fleet-rollback decision keyed on the log would skip a host still
  running the superseded generation.
- C01-6 record-write failure degrades silently; the table's promised
  RETRY-convergence has no reconciler (recordRelease warns,
  deploy.go:1230-1232; backfill fires only from rollback/recreate).
- C01-7 compensation reconstructs the predecessor route from config +
  live inspect instead of the recorded receipt (deploy.go:1097-1137;
  A12/T05 — the table requires undo-to-known-predecessor).
- C01-8 same-version running `_replaced` defers to the operator (MANUAL)
  where the table says INSPECT→compensable (deploy.go:438-451; needs
  F04/A09 generation identities — deliberate A08 containment, recorded
  as disagreement not defect).
- C01-9 candidate names are version-keyed, not attempt-keyed
  (docker.go:82-117) — two attempts of one hash are not attributable by
  evidence; F08 attempt ids are the existing keying surface.
- C01-10 the predecessor snapshot is in-memory only (deploy.go:464-473)
  — retirement re-derives correctly but loses the removed-worker
  capture on crash; the journal slice should persist it.

**Open:** harness execution against a real fixture host (next slice);
the ten findings above as C01 implementation work. Gates: `go vet ./...`
clean; `go vet -tags integration ./internal/deploy/recovery` clean;
`go test ./... -race` all packages ok (integration-tagged code excluded
by default); `go test -tags integration …TestFaultHarness` skips cleanly
with env unset; gofmt clean.

### C01 spike — executed against a real target (2026-09-21, later)

The fault harness ran against a real SSH+Docker fixture (colima VM,
Linux aarch64, Docker server 29.5.2, /deployments provisioned): scenario
(a) late effect after owner death — new owner acquired the lock, the
dead owner's container landed AFTER acquisition, decision = MANUAL
("lock acquisition proves quiescence" would have said RETRY); (b) side
effect without receipt — running candidate, no state/route/record,
decision = INSPECT, never invented success; (c) stale holder's late
write — the EXISTING fence machinery refused both the guarded effect
and the fenced state commit (ErrFenceLost). Result table preserved in
the session receipt. The design spike's executable-proof obligation is
met; the C01-1..C01-10 disagreement implementations remain open.

## Programme slice (2026-09-22) — C06 preview canonical identity

The product evaluation's branch-identity probe
(`_internal/evals/2026-09-21/probe_cli_contracts.py`,
TestMarketEvalPreviewBranchIdentityIsDistinct) demonstrated the C06 defect
live: `previewStatePath("market-eval", "feature/login")` ==
`previewStatePath(..., "feature-login")` because SanitizeBranch strips both
`/` and `-` to the same slug — and the slug was the IDENTIFIER everywhere:
state file, container name/process, Caddy route key, and DNS label. Two
branches whose slugs collide silently shared (or fought over) one preview:
the second deploy destroyed the first's container and overwrote its record
and route. Base revision `22ae801`; changes left uncommitted for review.

**Canonical ID design** (`internal/preview/preview.go`):

- `PreviewID(app, branch)` = `<app>-p-<8hex>`, 8hex = first 8 hex chars of
  `sha256(app + NUL + full branch ref)`. The app IS the canonical repo
  identity as teploy knows it (all server state is namespaced by it; one
  app = one repo's deployment identity). The git remote URL is recorded
  per-record as provenance but deliberately NOT hashed into the ID: remote
  URLs change on repo renames and protocol switches, which would silently
  orphan every existing preview. `previewIDHex` is pinned by
  TestPreviewIDGolden so the scheme cannot drift unnoticed.
- Sanitized slugs are DISPLAY names only: they remain the human-readable
  prefix of the preview subdomain, which now carries the ID suffix for
  uniqueness — `preview-<slug≤46>-<8hex>.<domain>` (whole DNS label ≤ 63).
  Two colliding branches therefore get distinct hostnames; without this,
  coexistence would still break at the Caddy site block (one hostname, one
  site). The slug never keys state, containers, or routes for new
  resources; its remaining uses are the read-only legacy lookup and the
  legacy-era route-key fallback.
- `State` records the full identity going forward: `id` (canonical),
  `branch` (full, unsanitized — always was), `repo` (trivially normalized
  origin remote: scheme/credentials stripped, `.git` dropped, scp-form
  rewritten — `normalizeRepoURL`, internal/cli/preview.go), and `route`
  (the Caddy route key / network alias this preview's artifacts live
  under, making records self-describing instead of re-derived).

**Identifier paths migrated** — state file `previewStatePath`
(preview.go:150), container name/process + network alias (Deploy, was
`<app>-preview-<slug>-<ver>`, now `<app>-preview-p-<hex>-<ver>`), Caddy
route key (SetRoute/RemoveRoute via `routeApp`/`previewRouteKey`),
preview domain (`previewDomain`), prune/cleanup enumeration (Prune/Destroy
resolve through the same keys), and the CLI create path records repo
identity (runPreviewDeploy → DeployConfig.Repo). Destroy-before-recreate
lifecycle behavior is unchanged this slice (separate recorded C06 item).

**Legacy contract** (`resolveRecord`, preview.go):

| Situation | Behavior |
|---|---|
| Legacy slug-keyed record, stored full Branch == requested (repo agrees when both record one) | ADOPT: Deploy migrates it under the canonical key with data preserved (full Branch, ID, Repo added), then tears down the artifacts the record itself names (stored Container, slug-era route key — `Route` empty marks the era); Destroy/Prune tear down those artifacts directly and remove the legacy file |
| Legacy record at the shared slug names a DIFFERENT branch (the collision), or repo mismatch when both record one | `*AmbiguousPreviewError` naming stored branch, requested branch, record path and remediation (`teploy preview destroy <stored-branch>` or manual rename/remove). NOTHING mutated — no container stop, no file removal, no route edit; verified by asserting the call log and file state stay empty/intact |
| Canonical record exists AND a mismatched legacy file sits at the shared slug | The legacy file belongs to the OTHER colliding branch: left untouched, does not block the operation (deploying/destroying this branch proceeds) |
| Canonical record exists AND a matching legacy duplicate exists | Interrupted-migration leftover of THIS branch (full-Branch match proves it): stale duplicate removed, canonical wins |
| Legacy record, unrelated slug | Keeps working untouched (TestPrune_OnlyDestroysExpired's fixtures are legacy records; TestLegacyOtherBranchNotBlocked covers cross-branch coexistence) |

Destroy/Prune adopt on full-Branch match alone (they carry no repo
identity); repo participates wherever it is known (Deploy). A
present-but-unparseable record fails closed naming the path rather than
being treated as absent.

**Evidence** — TDD: probe verified RED before the work
(TestMarketEvalPreviewBranchIdentityIsDistinct:
"feature/login" and "feature-login" → same record path), GREEN after;
probe suite fully green (compose contracts unchanged). New tests cover the
handoff's list, not just hash strings: coexistence (distinct state paths,
containers, routes, domains; neither deploy stops the other's container),
update-one-leaves-other, destroy-one-leaves-other, expire/prune-one
(modern + legacy fixtures), legacy adoption on Deploy (legacy file
migrated away, canonical record carries full Branch/ID/Repo/Route, legacy
container + slug route torn down), legacy collision ambiguity on Deploy
AND Destroy (record byte-identical after, zero mutation calls), repo
mismatch ambiguity, other-branch-not-blocked (including that branch's own
destroy still finding its legacy record), List over mixed-era records.
Mutation checks: constant ID suffix → identity/coexistence/golden tests
fail (colliding paths/domains/processes); removing the adoption
Branch-match → all three ambiguity tests fail (adopted instead of
refusing). Both reverted; gates after revert: `go vet ./...` clean,
`go test ./... -race` all packages ok, gofmt clean on touched files.

**Remaining C06 scope after this and the 2026-09-22 lifecycle slice (see
the bottom section)** — preview-profile config propagation through a
preview (image build args, env surface per preview), network/secret
isolation between previews, and the enforcement TIMER (pruning now CAN be
cron'd via `teploy preview prune`; nothing schedules it server-side) and
any Dash-side changes. The destroy-before-recreate lifecycle and the
deploy-piggyback-only TTL enforcement recorded above were closed by the
2026-09-22 lifecycle slice at the bottom.

## Product programme slice (2026-09-22) — C02: scheduled redeploys run through the engine

The scheduled-redeploy cron script reconstructed the container from
docker inspect and did its own stop/rm/run: no lock, no fence, no
health gate, no release record, no rollback, and a stop-to-start
downtime window — a second, weaker deploy path next to the engine
(C02's defect class; the script's own comment deferred this to "a v2").
The script now performs only the cheap digest pre-check (no-op when
unchanged) and, when the digest moved, invokes the on-server teploy
binary's new `autodeploy redeploy` — the exact triggerAutoDeploy path
(fenced lock, fetch, config load, env resolution, health-gated deploy)
the webhook listener uses. `teploy autodeploy schedule` gained
--branch, uploads the server binary, and verifies it supports
`redeploy` before installing anything (actionable error until a
release carries it — v0.1.35 does NOT; first release with it must
precede rescheduling). Existing installed scripts keep the old
behavior until `schedule` is re-run. Webhook admission durability,
cancel/supersede policy and Dash/CI trigger convergence remain recorded
C02 scope.

## Product programme slice (2026-09-22, later) — C02: webhook admission durability

The WEBHOOK trigger's admission contract (C02: "durable before
acknowledgment, bound to the authenticated commit, bounded queueing,
deduplication, cancel/supersede policy"). Base revision `01ec45c`; changes
left uncommitted for review. Closes the A34/T24 durable-webhook-queue
defect and the bounded-admission half of A37/T33 for this trigger path.

**Recon (what was wrong, file:line at base):** the handler verified HMAC,
checked in-memory dedup (persisted best-effort), and sent 200 with the
trigger merely STARTED — `internal/cli/autodeploy_serve.go:293` acked
before anything durable existed; the dedup tmp+rename at :134-141 was
synchronous but errors were swallowed (a 200 could go out with nothing on
disk). Worse than pileup: the per-delivery goroutine (:149) called
`triggerAutoDeploy` → `AcquireLockFenced` (:332), which does NOT block —
`acquireAutoLock` (state.go:479-516) returns "deploy is already in
progress" immediately, so **every delivery arriving during a running
deploy was acked 200 and then silently dropped** (unbounded short-lived
goroutine spawn, zero queueing). The dedup file records
`map["content:"+sha256(body)]time.Time` — body digest only, delivery ID
is log metadata (A36). A serve restart lost every acked-but-unprocessed
admission (no record existed).

**Landed:**

- **Admission ledger** — `internal/autodeploy/ledger.go`: append-only
  JSONL at `/deployments/<app>/.autodeploy-ledger.jsonl` (0600, next to
  the dedup file), one record per line (admitted / superseded / processed
  carrying id + provider delivery id + authenticated body digest + app +
  branch + received-at), `FileLedger.Append` = single write + fsync (the
  sibling+fsync+rename discipline applies to whole-file replacement; an
  append-only log durably appends). `ParseLedger` ignores a torn FINAL
  line (crash mid-append = never fsynced = never acked) and fails closed
  on mid-file corruption or unknown kinds; `FoldAdmissions` folds to
  pending + digest map; `NewestPending` picks newest per app.
- **Ack after fsync** — the handler appends the admission record and only
  then writes 200 `{"status":"admitted","disposition":…}`; a persistence
  failure rolls the dedup entry back (`DeliveryDedup.Unrecord` — the
  provider's retry of the same signed body re-runs admission instead of
  being swallowed as a replay of something never admitted) and answers
  503 + Retry-After: never ack what isn't durable. Replays answer 200
  `{"status":"duplicate"}`. The dedup snapshot persist
  (`onDedupChanged`) moved to AFTER durable admission, so a dedup entry
  on disk always corresponds to a ledger admission (a ping/tag persisting
  dedup it never admitted was the subtle loss window; non-push acks are
  now memory-only — their post-restart replay is a harmless no-op ack).
- **Bounded queue with supersede** — `admissionQueue`
  (autodeploy_serve.go): pending work is a RECORD, never a blocked
  goroutine. One worker (the only deploy runner — replaces the
  fire-and-forget goroutine), one newest-wins pending slot: idle →
  `running`; worker busy + slot empty → `queued`; slot filled → the older
  pending is marked superseded in the ledger (by-id) and replaced
  (`superseded` disposition). The RUNNING deploy is never cancelled
  mid-flight (cancellation propagation is deliberately out of scope; the
  newest deploy runs next instead). Same-digest redelivery while queued =
  dedup path (`duplicate`).
- **Restart resume** — `resumeAdmissions` on serve start: fold the
  ledger, reseed the replay dedup from recent admitted digests (the
  ledger backstops the best-effort dedup file across the delivery TTL),
  mark older pendings superseded, admit the newest per app with the
  changed-file list unrecoverable → filesKnown=false (deploy fail-open,
  the documented monorepo rule). Processed entries never re-trigger;
  duplicate delivery ids / digests collapse via newest-wins.

**Evidence** — TDD red against the base handler (both recorded failing):
10 distinct-body deliveries → 10 trigger calls (want ≤2), and the
admission response carried no disposition. Green after the rework.
Mutation check: moving the 200 ahead of the ledger append fails
`TestAdmission_AckWaitsForFsync` ("response written (status 200) before
the admission fsync completed"); reverted. New coverage: ack-after-fsync
ordering (gated ledger + response-flagging writer), persistence-failure
→ 503 + nothing admitted + retry-after-recovery admitted, supersede (A
marked superseded-by-B in the ledger, only B runs after the running
deploy), 10-delivery pileup collapse (1 running / 1 queued / 8
superseded; exactly 2 deploy invocations), crash-resume (exactly one
re-trigger; processed never re-triggers; newest-wins over duplicate
delivery ids; dedup reseeded), FileLedger concurrency/durability,
torn-tail/corruption parsing, fold/newest/seed units. Gates: `go vet
./...` clean; `go test ./... -race` all 25 packages ok; gofmt clean on
touched files (deploy.go/secret_audit.go/update_test.go were unformatted
at base — left alone).

**Remaining C02 scope (explicitly NOT in that slice):**

- **Commit-pinned builds — LANDED 2026-09-22** as its own slice (see the
  C02 commit-pinned builds section at the bottom): the fetch now resets to
  the authenticated `payload.after`/`checkout_sha` commit, and an
  unfetchable commit fails loudly naming both commits. Closes F40/A35/T25
  and the T25 half of that section's recon.
- **Dash/CI trigger convergence** — teploy-dash and CI-triggered deploys
  do not go through this admission path; converging them onto the ledger
  + queue (or the engine trigger generally) is cross-repo work.
- **Cancellation propagation** — a supersede never interrupts a running
  deploy; the newest runs next. Mid-deploy cancel is a deliberate
  non-goal here (the register's stranding posture) and stays open with
  the graceful-shutdown/bounded-admission remainder of A37/T33 (listener
  scope, signal-time drain of the worker).

## Finding from live use (2026-09-22) — SSH host-key algorithm coverage reads as "key mismatch"

Reported from teploy-ship's S14 trusted-copy provisioning (third wave,
85f44bf receipt). A known_hosts carrying only the host's ed25519 line makes
every connection fail with `ssh: handshake failed: knownhosts: key mismatch`
when the negotiated connection presents a different algorithm (this host
also has rsa + ecdsa host keys). The operator-facing failure names neither
the algorithm presented nor the algorithms on file, so it reads as a MITM
alarm rather than the coverage gap it is — the natural first reaction
(re-scan with `-t ed25519`, per most docs) is exactly what produces the
state.

Status: CLOSED 2026-09-22, fixed in `078f610`: both host-key callback
paths (default and accept-new) wrap a knownhosts mismatch with the host,
the presented key type, the on-file key types and the remediation —
`host key mismatch for <host>: server presented ssh-rsa, known_hosts has
no matching entry (has ssh-ed25519) — scan all algorithms (ssh-keyscan
without -t), not just one`. Failing closed is unchanged; the enriched
error still unwraps to `*knownhosts.KeyError`
(TestHostKeyMismatchNamesAlgorithms). Found while provisioning the ship
delivery worker; worked around by scanning all algorithms.

## Programme slice (2026-09-22, later still) — C02: commit-pinned builds

Closes the F40/TCL-49/A35/T25 family and the remaining-scope bullet of the
admission-durability slice above. Base revision `b7e030d`; changes left
uncommitted for review.

**Design:**

- `autodeploy.PushCommit(body)` (internal/autodeploy/paths.go) extracts the
  commit the authenticated push names as the branch's new head — GitLab's
  `checkout_sha` preferred, else `after` (GitHub/Gitea/Forgejo). Returns ""
  for non-push shapes, tag refs (whose "after" is the tag object), explicit
  deletions, the all-zero deletion marker, and malformed hashes (40/64
  lowercase hex required): "" means "deploy the tip and say so", never
  "pin to garbage".
- `fetchCheckout` (internal/cli/autodeploy_serve.go, extracted from
  triggerAutoDeploy) — tip mode renders the historical fetch+reset unchanged
  and states "Deploying tip of <branch>"; commit mode fetches the branch,
  best-effort fetches the SHA itself (pulls force-pushed-away commits on
  servers that allow SHA fetches — GitHub/GitLab do; failure fine),
  VERIFIES presence with `git cat-file -e '<sha>^{commit}'`, then
  `git reset --hard '<sha>'` — exact args asserted in tests. An unfetchable
  commit FAILS LOUDLY naming the authenticated commit, the branch, and the
  branch's current tip (`git rev-parse origin/<branch>`), and never resets
  the worktree — never a silent tip fallback. Output states
  "Deploying <commit> from delivery (branch <b>)".
- Threading: the handler parses the commit once per delivery and binds it
  to the durable admission (new `AdmissionRecord.Commit`, carried through
  superseded/processed transitions), the bounded queue's single worker
  passes it to the trigger, and restart resume re-triggers PINNED to the
  ledger-recorded commit. `autodeploy redeploy` (the scheduled path) passes
  "" — tip mode with the explicit tip output; `triggerAutoDeploy` grew the
  commit parameter (empty = tip).

**Evidence** — TDD: behavioral red first after a pure-behavior-preserving
extraction of fetchCheckout (commit-pinned test failed "not implemented
yet"; unfetchable test failed with no error; handler-threading test failed
with record/trigger commit ""). Ping/tag no-op coverage
(TestWebhookHandler_OnlyWatchedBranchPushes) verified unchanged. New
coverage: PushCommit provider shapes (GitHub/GitLab/deletion/zeros/
malformed/64-hex), exact fetch/verify/reset command forms + ordering, tip
mode never running pin commands, unfetchable → error naming both commits +
no reset, handler→ledger→trigger commit threading, no-commit → tip,
resume-carries-commit. Mutation checks (in-place, reverted):
removing the cat-file guard fails the commit-pinned and unfetchable tests
for the intended reason; severing PushCommit's pinning fails the handler
threading test and 3 PushCommit subtests. Gates after revert:
`go vet ./...` clean; `go test ./... -race` all 25 packages ok; gofmt
clean on touched files; contract probes 5/5 PASS.

**Remaining C02 scope:** Dash/CI trigger convergence (cross-repo);
cancellation propagation (supersede never interrupts a running deploy —
deliberate non-goal with the A37/T33 graceful-shutdown remainder). The
ancestry-policy question (should a tip AHEAD of the authenticated commit
ever deploy it? today: yes, the authenticated commit always wins — that is
what "bound to the authenticated commit" means) is settled by the
programme text, not by config.

## Programme slice (2026-09-22, later still) — C06: preview lifecycle

Closes the two recorded C06 lifecycle defects. Base revision `b7e030d`;
changes left uncommitted for review.

**Blue/green previews (defect: "Preview destroys old preview before
starting new"):**

- Candidate container name/alias/process carry the version
  (`<app>-preview-p-<hex>-<version>`; RunConfig.Name set explicitly), so
  each generation has its OWN docker network alias — the stable route can
  point at exactly one generation (a shared alias round-robins between
  predecessor and candidate the moment both run; SetRoute's doc contract
  asks for a specific container name as upstream, which preview now
  honors).
- Update order: allocate port → (same-version: `docker rename` the
  running predecessor aside `<name>-replaced`, the engine's pattern) →
  start candidate → inspect internal port → READINESS GATE → SetRoute
  under the STABLE key `<app>-preview-p-<hex>` with the CANDIDATE as
  upstream → rewrite the record → only then stop+remove the predecessor
  (+ remove a legacy-era route key when the adopted record had one; the
  canonical key is repointed, not removed). Canonical ID, state path,
  route key, and domain are unchanged across updates.
- Readiness gate mirrors internal/deploy/health.go's probe shape scoped to
  preview's own executor (no engine import): curl against the candidate's
  localhost-published port — 200 ready, 404/3xx → TCP-connect fallback,
  bounded by 30s/1s defaults (test-shortened knobs). Main deploys gate the
  same way; previews failing a dead image loudly is the consistent
  posture.
- On candidate failure (start, port inspect, health, route): stop+remove
  the CANDIDATE, rename a renamed-aside predecessor back under its
  recorded name, leave the predecessor running/routed/recorded, and the
  error names the failed candidate. Same-version updates are the engine's
  compromise: the alias is shared for the brief window between candidate
  start and predecessor retirement (recorded below as residual).

**TTL enforcement (defect: "Preview expiration cleanup occurs when another
preview deploy is invoked"):**

- `Manager.Prune` is the one shared prune core (per-preview outcome
  lines; failures warn and continue); `Manager.PruneAll` enumerates
  `/deployments/*/previews` across ALL apps and runs that core per app —
  canonical and legacy eras alike (enumeration is over files; Destroy
  adopts legacy records by full-Branch match as before). Idempotent by
  construction; touches nothing outside the previews directories and the
  artifacts records name.
- `teploy preview prune` now runs PruneAll (help text says so; connects
  via the cwd teploy.yml's server — the file identifies the target, the
  prune is not app-scoped). The deploy piggyback calls the SAME
  `Manager.Prune` core for its own app. The TTL field is the record's
  absolute `ExpiresAt`, default 72h documented on DeployConfig.TTL and
  State.ExpiresAt, applied on create AND refreshed on update.

**Evidence** — TDD: behavioral red recorded before implementation
(ordering test showed stop=5 < switch=12 < run=17 — the defect live in
the call log; failed-candidate deployed "successfully" with no gate;
same-version had no rename-aside; PruneAll stub pruned 0). New coverage:
blue/green ordering via mock call-log indexes (run < reload < stop, rm
after stop, record names the candidate under the stable route key),
failed candidate (predecessor container/route/record byte-identical,
candidate cleaned up, error names candidate + health), same-version
rename-aside + retirement after the healthy candidate holds the name
(no reload expected — a byte-identical Caddyfile block is a deliberate
no-op skip in caddy.mutate), prune exactness across two apps and both
eras + non-preview state.json untouched + idempotent second run (zero
docker commands), TTL default = CreatedAt+72h. All pre-existing preview
tests (coexistence, adoption, ambiguity, prune fixtures) pass unchanged.
Mutation checks (in-place, reverted): moving retirement ahead of the
route switch fails the ordering test with stop-before-switch; making
Prune skip ID-less records fails the era test (pruned 1, want 2) AND the
pre-existing legacy-fixture prune test. Gates: `go vet ./...` clean;
`go test ./... -race` all 25 packages ok; gofmt clean on touched files;
contract probes 5/5 PASS.

**Remaining C06 scope (residual):** preview-profile config propagation
(per-preview env/build-arg surface), network/secret isolation between
previews (candidates share the `teploy` network today, like all app
containers), the enforcement TIMER (nothing server-side schedules
pruning — `preview prune` is cron-able but teploy ships no daemon, by
design), the same-version shared-alias window above, and Dash-side
changes.

## Programme slice (2026-09-22, latest) — C01 implementation: attempt journal + honest degraded outcome

Three contained C01 findings landed as their own coherent changes (the
spec is docs/C01_RECOVERY_STATE_TABLE.md's findings list; the decision
function internal/deploy/recovery.Decide is UNCHANGED — its exhaustive
tests pass untouched; this slice produces the EVIDENCE its inputs model).
Base revision `c0efd26`; changes left uncommitted for review. New file
`internal/deploy/journal.go` is the attempt journal: receipts persisted
into the F08 attempt namespace (`meta/att/<hash>.<id>/`, write-once,
0600 atomic, identity-validated on read — T56 parity).

- **C01-10 — durable predecessor snapshot.** `predecessors.json`
  (exact container IDs/names/labels + the predecessor release identity +
  same-version flag) is persisted at the RENAME PHASE — after the 6b
  listing, before the recreate displacement or any new container starts
  (test pins the write's call index below the first `docker stop` and
  `docker run`). On the recovery paths the instruction names
  (`restoreDisplacedAndStarted`, `abortStateCommit`), when the in-memory
  displaced list is absent, `displacedFromSnapshot` reads the receipt and
  restores exactly the recorded web containers that are no longer running
  (blue/green predecessors and same-version `_replaced` renames inspect
  as running and are skipped by construction). abortStateCommit takes the
  attempt as a parameter for this. TDD red: both tests failed "no
  predecessor snapshot persisted" before journal.go existed. Mutation:
  removing the write fails both tests for that reason (reverted). The
  write-then-crash-then-recover test drives a NEW executor seeded with
  only the crashed attempt's file state and asserts the exact recorded
  name is recreated while a stopped same-release WORKER the name-derived
  fallback would touch is never inspected.
- **C01-4 — durable readiness receipt.** `readiness.json` (exact
  candidate container IDs from docker run + names, per-replica probe
  host/port/path, outcome, timestamp) is written EXACTLY when the health
  gate passes and BEFORE the traffic switch begins (test pins the
  ordering: after the last probe, before the Caddyfile transaction's
  first command; a health-failing deploy leaves no receipt). The
  Decide-side wiring is the evidence derivation `attemptReadinessState`
  (receipt present → recovery.ReadinessPassed; confirmed absent →
  CandidatesRunning — the ADR's collapse, un-collapsed) and
  `candidateAttribution` (running candidate-shaped containers are
  PROVABLY the crashed attempt's only on receipt ID/name match → Present;
  without a receipt, or with IDs that provably belong to another attempt
  of the same release, → Unknown — R4's never-auto-decide class). Tests
  assert through recovery.Decide: the crash-after-readiness world
  (traffic switched, uncommitted, predecessor serving) is COMPENSATE with
  the receipt and INSPECT without. Mutations: writing the receipt before
  the gate fails both ordering tests; receipt-independent attribution
  fails the Decide distinction ("without the receipt the same world must
  INSPECT, got COMPENSATE") — both reverted. In-tree consumers are the
  derivation helpers; the recovery OWNER that reads them on lock
  acquisition is C01-1's slice (recorded).
- **C01-5 — honest degraded outcome.** `state.LogEntry` gains
  `Degraded` + `DegradedReason` (omitempty — old entries parse
  unchanged). Step-14 retirement collects its incompleteness
  (stopPredecessorSnapshot now returns what escaped — stop/remove
  failures, fence-loss interruptions, skipped cleanup, name-fallback
  errors) and `logDeploy(ctx, cfg, true, reason, start)` records
  Success=true AND Degraded=true: traffic IS switched (not a deploy
  failure) but the outcome is not clean success. The Success-filtering
  consumer in-repo, `teploy log` rendering (internal/cli/log.go), shows
  DEGRADED + reason, and --json carries the field for dash/machine
  readers; the consumer test also models the fleet-rollback selector
  (Success alone targets the degraded host as clean; Success && !Degraded
  separates it). TDD red: tests failed to compile against the field-less
  LogEntry; mutation: emptying the degraded population at the success
  call site fails "DegradedReason must name the escaped container"
  (reverted — note this check ran before an accidental `git checkout`
  required re-applying the same edits; the re-applied code is identical
  and all tests re-ran green).

Gates: `go vet ./...` clean; `go vet -tags integration
./internal/deploy/recovery` clean; `go test ./... -race` all 25 packages
ok; recovery exhaustive suite green and byte-identical semantics; gofmt
clean on touched files; contract probes 5/5 PASS. No push performed.

**Residual C01 list (explicit):** C01-1 lock acquisition is treated as
quiescence — the replacement owner must run Decide over observed
evidence after a stale break (the locking-protocol redesign). C01-2
pre-commit effects are check-then-act, not guarded — a broken holder's
candidate/route effects can land inside the new owner's window. C01-3
the shared Caddy lock is ownerless/unfenced — conflicting-route evidence
has no producer/consumer. C01-8 same-version running `_replaced` stays
MANUAL — deliberate A08 containment; automating the INSPECT→adopt
continuation needs generation-scoped identities. C01-9 candidate names
are version-keyed, not attempt-keyed — two attempts of one hash are not
attributable by evidence (F04/A09). C01-6 (record-write convergence has
no reconciler) and C01-7 (compensation reconstructs the predecessor
route instead of using a receipt) also remain, with their register items
(A12/T05 standing for C01-7).

## Programme slice (2026-09-22, latest) — C01 implementation: record-repair debt + receipt-driven route compensation

The two remaining contained C01 findings landed as bounded slices (spec:
docs/C01_RECOVERY_STATE_TABLE.md findings 6 and 7; recovery.Decide and its
exhaustive suite untouched). Base revision `cce0726` (v0.1.37); changes
left uncommitted for review.

- **C01-6 — record-write failure now converges.** A releasemeta record
  write that fails after the live commit still never fails the deploy
  (deliberate degradation — record failure must not roll back live
  traffic), but the debt is now DURABLE and visible:
  `internal/deploy/repairdebt.go` persists a marker at
  `/deployments/<app>/repair-debt.json` (atomic 0600, identity-validated
  on read, T56 parity) naming app, release, attempt, what failed, when,
  and the failed write/repair attempt count. The NEXT deploy repairs it
  BEFORE its own work (DeployFenced step 1b, under the app lock): if the
  record already exists (a rollback/backfill converged it meanwhile) the
  marker is just cleared; otherwise the record is rebuilt from the live
  containers via releasemeta.Backfill — the table's transition-7
  convergence — and the marker cleared on success (reported in output);
  a repeated failure keeps the marker with an incremented count and says
  so (never a deploy failure — the debt describes the previous deploy).
  A re-failing record write of the SAME release bumps the existing
  marker instead of resetting its history. `teploy status` (writeStatus,
  extracted from runStatus for testability) reports outstanding debt in
  text and JSON (`repair_debt`), an unreadable marker is reported rather
  than hidden, and no marker means zero output noise.
- **C01-7 — route compensation uses the recorded receipt.**
  `restorePreviousRoute` (deploy.go, both abortStateCommit call sites)
  now renders the previous route from the predecessor release's F14
  RECORD — domain, replica upstream names, the recorded primary
  container port (TCL-14), TLS/caddy_extra/cache/firewall/access, and
  the LB health path — consulting zero live inspect (the record is
  authoritative for what teploy switched away FROM; compensating from
  cfg+inspect compensates to the wrong block exactly when config
  drifted). Reconstruct-from-inspection survives only as the documented
  fallback for legacy installs (no record), unreadable records, or
  records with no designated primary port — and every fallback is
  announced in the output ("restoring the previous route from live
  inspection"). rollback's restoreRollbackRoute (the failed-ROLLBACK
  compensation over running containers) is NOT this path and stays with
  the A12/T05 register item, as does the exact-block compare-and-swap
  design on ParseSites/ExtractPolicy.

Evidence — TDD red first per finding: C01-6's tests failed at compile
(RepairDebt absent) and behaviorally after stubbing (marker never
written; next deploy never repaired; count never bumped); C01-7's four
tests failed against the inspect-driven base for the finding's own
reasons (route rendered from inspect against a record, silent fallback,
a SUCCEEDING disagreeing inspect winning 8080-over-3000). New coverage:
marker content/order (app, release, attempt id, reason, count, timing),
next-deploy repair + clear + report ordering (repair precedes
"Deploying"), persistent-failure count bump, no-marker silence, status
text+JSON+unreadable, receipt rendering (exact hosts/upstream/TLS/health
path from the record, `_replaced` same-version naming, multi-replica
upstreams, zero NetworkSettings inspects), loud legacy fallback.
Mutation checks (in-place, reverted): removing the DeployFenced repair
call fails the repair test ("must rebuild the failed record") and the
count test ("got 1"); swapping restorePreviousRoute precedence to
inspect-first fails all three receipt tests (route from inspect,
disagreeing inspect wins, fallback message fires on the record path).
Gates after revert: `go vet ./...` clean; `go test ./... -race -count=1`
all 25 packages ok; gofmt clean on touched files; contract probes 5/5
PASS. No push performed.

**Residual C01 list (explicit, updated):** C01-1 lock acquisition is
treated as quiescence — the replacement owner must run Decide over
observed evidence after a stale break (ADR: the locking-protocol
redesign). C01-2 pre-commit effects are check-then-act, not guarded — a
broken holder's candidate/route effects can land inside the new owner's
window (ADR: guarded single-command effects, WriteFenced's shape
generalized). C01-3 the shared Caddy lock is ownerless/unfenced —
conflicting-route evidence has no producer/consumer (ADR: fenced
short-lived proxy-commit lock or an owner-tagged equivalent). C01-8
same-version running `_replaced` stays MANUAL — deliberate A08
containment; the INSPECT→adopt continuation needs F04 generation
identities (ADR: attempt-keyed container identities). C01-9 candidate
names are version-keyed, not attempt-keyed — two attempts of one hash are
not attributable by evidence (ADR: F08 attempt ids as the keying
surface for candidate names). C01-6 and C01-7 are LANDED (this slice).

## Programme slice (2026-09-22, latest) — C03: explicit readiness probe modes

Closes the F47/TCL-17/A22 standing deferral — the first bounded C03 slice
(P0: "Support HTTP, TCP, container and operator-defined readiness with
clear defaults and deadlines; retain `auto` only as an explicit
compatibility mode"). Base revision `a914631`; changes left uncommitted
for review. Drain/graceful-stop semantics, the LB health-path rendering
(5bf5594), and Caddy are untouched (next slices / explicit stay-out).

**Design:**

- **Grammar** — `health.mode: http | tcp | auto` in teploy.yml/TOML
  (`config.AppHealthConfig.Mode`). Empty/absent = `auto`, the compat
  default: HTTP GET first, exactly a 404/3xx falls back to the TCP dial —
  the verbatim historical behavior (preserved in `checkHealth`, now
  NAMED). `http` is status-based only (200 = ready; 404/3xx fails the
  attempt, no fallback). `tcp` dials the published port and never speaks
  HTTP. Unknown mode is rejected at config load AND at the shared
  execution-plan validator (`deploy.Config.validate` — direct
  construction via fleet/preview/autodeploy bypasses parsing, TCL-18
  parity). Field agreement: `mode: tcp` with a `path` set is REJECTED at
  config load (decision: reject, not warn — a path nothing fetches is a
  config that lies about what the gate does); `http`/`auto` without a
  path keep the `/health` default. A mode-only destination overlay
  replaces the whole health block (F57 semantics extended: Mode joined
  the presence detection); the normalized manifest carries
  `mode` (defaulted to auto) for drift identity.
- **Dispatch** — `probeOnce` (internal/deploy/health.go) switches on the
  normalized mode per attempt; `httpStatus` (extracted from the old
  monolithic attempt) returns the observed code so `checkHealth`'s
  fallback condition is byte-identical to before (an interim refactor
  that dialed on ANY non-200 was caught and corrected during the slice —
  auto must stay exactly today's behavior). `HealthCheckPublic` /
  `HealthCheckAt` (on-demand `teploy health`) keep the auto default.
- **Surfacing** — deploy (step 9) and rollback (step 3) print the gate
  BEFORE it runs: `Readiness: HTTP GET /healthz (30s deadline)` /
  `Readiness: TCP :3000 (30s)` / `Readiness: auto — HTTP then TCP
  fallback (compat, 30s deadline)` (tcp names the first replica's port;
  failures name replica + port as before).
- **Deadline verified** — `health.Timeout` was already a TOTAL deadline:
  `healthCheck` wraps the context in `WithTimeout`, the retry loop
  selects on ctx.Done, each HTTP attempt is curl-bounded
  (--connect-timeout 2 / --max-time 5), and the remote executor cancels
  the session at deadline (SIGTERM + close, RunStream). There was NO
  unbounded retry loop to bound; a regression test now pins it (a
  never-responding probe — an executor whose commands hang until context
  death — must fail within deadline + slack, not hang).
- **Forwarding** — the F14 release record (`releasemeta.Health.Mode`) and
  the C01-4 readiness receipt (`readinessProbe.Mode`) carry the effective
  mode; `applyRecordToRollback` overlays it (a modeless legacy record
  leaves the config's mode — compat). Rollback probes the way the target
  release was actually gated.

**Evidence** — TDD: red level 1 recorded as compile failure (Mode field
nowhere existed), red level 2 after plumbing-only (fields + passthrough,
no behavior): dispatch tests failed with mode ignored (http-mode deploy
passed via the TCP fallback; tcp-mode healthCheck timed out on an
unregistered curl; auto-explicit never dialed), unknown mode and tcp+path
were accepted, the surfaced lines were absent, the record carried no
mode. Green after the implementation. Mutation checks (in-place,
reverted, gates re-run green after each): (1) dispatch removed — always
http — fails TestHealthCheck_TCPMode* (curl issued / dial never run),
TestHealthCheck_AutoModeExplicitFallsBack (no dial), and the tcp-mode
DEPLOY test (gate times out); (2) readinessSummary collapsed to the http
line fails the tcp/auto surfaced-line tests; (3) removing the
`context.WithTimeout` total deadline hangs the never-responding-probe
test to the test-binary timeout. Gates: `go vet ./...` clean;
`go test ./... -race -count=1` all 25 packages ok; gofmt clean on every
touched file (pre-existing base strays in cli/deploy.go's fleet-rollback
region, deploy_test.go, f14_wiring_test.go, plan_a_test.go,
config/app_test.go left alone, consistent with the C02 posture); contract
probes 5/5 PASS. No push performed.

**C03 remainder (explicit):** request drain + graceful stop (stop_timeout
wiring, SIGTERM→SIGKILL ladder — deliberately this slice's stay-out), the
liveness-vs-readiness distinction (post-switch continuous probing; today
only the container HEALTHCHECK directive approximates it), multi-host
partial-wave readiness states (canary-wave aggregate gating beyond the
existing success/fail rollback), and WebSocket/SSE/long-request drain
verification at the traffic switch. Registered interactions to decide in
those slices: `mode: tcp` × the Caddy LB active health check (the LB
block's HTTP path probe would mark a non-HTTP upstream down — LB
rendering is 5bf5594's fixed surface, untouched here), and preview's
readiness gate (internal/preview) which mirrors the auto shape and has
no mode surface of its own.

## Programme slice (2026-09-22, latest) — C04: build provenance + plan/receipt equality

First bounded C04 slice (base revision `6a1142d`; changes left uncommitted
for review). Contract addressed: "Resolve Git revision, build context,
Dockerfile, platform and immutable image digest BEFORE execution... image
digest and effective configuration shown in plan equal the deployed
receipt; response-loss retries do not build a different source; changed
mutable tags behave according to selected policy." Commit pinning itself
was P0-done in C02 (webhook builds reset to the authenticated commit;
this slice records what every path resolved).

**Design:**

- `releasemeta.Provenance` (new internal/releasemeta/provenance.go) is the
  plan-time record: revision (full HEAD sha via the SourceRevision
  threading both trigger paths already had), a Dirty flag, build context
  path + context fingerprint, Dockerfile identity (path + content sha),
  target platform, requested image ref, the immutable digest resolved
  BEFORE execution, a DigestPinned-vs-mutable flag, and the
  effective-config (manifest) digest. Persisted as `provenance.json` in
  the F08 attempt namespace (`meta/att/<hash>.<id>/`, write-once, atomic
  0600, identity-validated on read — the journal discipline) by the shared
  post-build orchestration: `deployBuiltImageFenced` (manual + ad-hoc +
  autodeploy, new `sourceRoot` param: "." vs the fetched checkout) and
  `singleServerDeployer.deployApp` (multi-server/scale) — all three
  engine entry paths.
- Recon finding: the build package did NOT already compute a context
  fingerprint — the only tree-hash machinery was the static deployer's
  unexported `hashDir` (static-only semantics, symlink-rejecting). Added
  `build.ContextFingerprint(dir, excludes)` with hashDir's v3 typed/
  length-prefixed record encoding (F51/TCL-38 discipline) but
  symlink-INCLUSIVE (hashed by target: rsync -a preserves links into the
  context, so a link is build input), excludes applied (DefaultIgnore +
  .teployignore — the fingerprint describes the synced tree). Also
  extracted `build.EffectiveLocalPlatform` from localBuildDockerfile's
  inline rule (behavior-preserving) so the record names the platform the
  local build actually targets.
- Plan/receipt equality: `Deployer.DeployFenced` prints a plan block
  BEFORE any effect (image digest — not just tag, with pinned/mutable
  stated; revision, flagging a dirty worktree as "building uncommitted
  changes"; context fingerprint; Dockerfile identity; platform; manifest
  digest). The F14 record gains `ManifestSHA256` + embedded `Provenance`,
  and `recordRelease` returns it. A closing verification (step 17, after
  the live commit) asserts record digest == plan digest and reports
  equality explicitly; a mismatch is a loud warning + the C01-6
  repair-debt marker (next deploy reconciles) — never a failed live
  deploy. `plannedImageDigest` applies ONE like-for-like rule on both
  sides: a digest-pinned ref is identified by its manifest digest,
  everything else by docker's resolved content ID (`ImageDigestFromRef`,
  now exported) — otherwise a pinned ref's plan (manifest digest) and
  record (image ID) could never agree by construction.
- Retry stability verified + pinned: the attempt machinery gives each
  invocation a fresh write-once namespace, so a retry lands BESIDE the
  first receipt (test); source stability is resolution purity —
  `resolveDeployProvenance` is a pure function of (config, tree, image),
  no time- or attempt-dependent fields (they are stamped at write) —
  pinned by a DeepEqual double-resolution test; webhook retries
  additionally re-pin to the ledger commit (C02, unchanged).

**Evidence** — TDD red first: all four new test files failed to compile
against the absent machinery (undefined Provenance/WriteAttemptProvenance/
ContextFingerprint/Config.Provenance...). New coverage: provenance
round-trip + write-time foreign-identity refusal + read-time identity
mismatch refusal + absent-is-nil-nil; distinct immutable receipts for two
attempts of one release; fingerprint determinism/sensitivity (content at
constant size, rename, empty-dir structure, symlink retarget) +
exclude-honoring; resolution field capture for build/prebuilt/mutable-tag
paths; dirty-worktree flag; retry stability; plan output (digest,
revision, "building uncommitted changes", fingerprint, manifest digest —
and printed before the first container starts); record embedding
provenance + manifest digest; mismatch → loud warning naming both digests
+ repair-debt marker + deploy still succeeds; provenance/deploy identity
mismatch refused pre-effect; no plan digest → no false alarm. Mutation
checks (in-place, all reverted): equality check disabled → mismatch test
fails; provenance fields dropped from the plan print → plan test fails on
the missing fingerprint; dirty suffix severed → "building uncommitted
changes" assertion fails; record stops embedding provenance → record test
fails; fingerprint made content-blind (digest zeroed, size kept, against
a same-size content edit) → sensitivity test fails. Gates after revert:
`go vet ./...` clean; `go test ./... -race -count=1` all 25 packages ok;
gofmt clean on every touched hunk (cli/deploy.go's pre-existing
fleet-rollback region stray left alone, consistent with the C02/C03
posture); contract probes 5/5 PASS. No push performed.

**C04 remainder (explicit):** registry authentication provenance (recording
WHICH credential identity pulled/built — nothing today names the docker
config/secret used); scan/attestation separation (trivy's gate currently
FAILS the deploy on scan error — the contract wants scan failures distinct
from build failures, and attestation is unmodelled); the ARM64/AMD64
packaging matrix (cross-platform build verification on supported targets —
`platform` is now RECORDED everywhere but not matrix-tested); offline
fallback as an explicit pull POLICY (today's behavior — digest-pinned
cache reuse, mutable always-pull, warned local fallback — is now recorded
as provenance facts, not yet a selectable policy); build records for
`teploy build` outside deploys; cache diagnostics; secret-safe build-input
attestation. Changed-mutable-tag POLICY (beyond recording pinned-vs-
mutable + the mismatch warning) lands with the offline/pull-policy slice.

## Programme slice (2026-09-23) — X02 S1: versioned machine interface + S3-lite error envelope

First X02 slice (ADR
`../_internal/X02_RESOURCE_CONTRACT_ADR_2026-09-22.md` §2.1-2.3, adopted
by `../_internal/DELEGATED_DECISIONS_2026-09-23.md` decisions 1/4/7/8/9 —
D8 single-integer MI, D9 capability advertisement, D10 exit codes
unchanged). Base revision `6faefc4`; changes left uncommitted for review.

**Landed:**

- **Machine-interface version 1** (`internal/cli/machineinterface.go`):
  `MachineInterface = 1` at the root of `version --json` (new
  `{"version","machine_interface","capabilities"}` envelope), `app list
  --json` (appListDTO), and `server status --json` (serverStatusDTO) —
  additive fields; dash's Go decoders ignore unknown fields, verified
  against dash's actual decode sites. Versioning rules on the constant's
  doc comment: additive changes never bump; removal/rename/type or
  semantic change bumps. **Verified exclusion:** `server list --json`
  emits a bare map-of-servers root (dash decodes
  `map[string]{host,user}` at server.go:1641) — there is no envelope
  object to carry the field additively, and injecting a
  `machine_interface` KEY would materialize as a phantom server in
  dash's fleet; reshaping it is a non-additive change recorded as S2
  follow-up (needs a coordinated dash decode change).
- **Capability registry** (15 stable tokens, the doc-comment block in
  machineinterface.go IS the registry): `env-set-stdin`, `kv-set-stdin`,
  `template-var-stdin` (cb7c0fc), `server-rename`, `server-update`
  (72c57f9), `autodeploy-redeploy`, `health-modes` (C03),
  `provenance-records` (C04), `readiness-receipts` (C01-4),
  `preview-canonical-id`, `preview-blue-green` (C06), `repair-debt`
  (C01-6), `error-envelope` (this slice), `app-list-machine`,
  `server-status-machine`. Every token names a LANDED contract;
  `server-list-ids` deliberately absent (S4 not landed).
  TestCapabilityTokenRegistry pins the exact sorted set — rename or
  removal fails it.
- **S3-lite structured error envelope** (`internal/cli/errevelope.go`):
  on any command failure under `--json`, one document
  `{"machine_interface","code","message","detail"}` on STDERR (stdout
  stays the data channel); without `--json` the historical plain-text
  stderr line is byte-identical. Code taxonomy v1 defined as the closed
  registry: config-invalid, target-unreachable, unsupported, conflict,
  uncertain-outcome, degraded, internal (unknown → internal,
  forward-safe). **Wired classes:** config-load failures
  (`config.ErrInvalidConfig` sentinel — a no-text-change wrapper so
  errors.Is classifies while every message stays verbatim — wrapped at
  all LoadApp/LoadAppWithDestination/Compose-propagation returns) and
  deploy admission refusals (the dash-hit ad-hoc path's pre-effect
  validations, `--version` grammar, no-server, tag-filter parse —
  `errDeployAdmission` marker, same no-text-change discipline); both
  classify config-invalid. `Execute` reports through
  `reportExecutionError` before the unchanged `os.Exit(1)`; drift's exit
  2 extracted into `driftExitCode` so the 0/1/2 semantics are pinned in
  code (2 only with `--exit-code` AND drift found).

**Evidence** — TDD red level 1 recorded (all new test symbols undefined
at compile: MachineInterface, writeVersion, reportExecutionError,
ErrInvalidConfig, errDeployAdmission, driftExitCode), green after
implementation. New coverage: version JSON exact shape (3 keys, MI 1,
capabilities verbatim) + human output unchanged + cobra end-to-end;
registry completeness (golden list + per-constant membership +
uniqueness/sortedness); app list + server status envelopes carry
machine_interface; config-load envelope through a REAL failing `deploy
--json` (code, stable message, detail naming teploy.yml); admission
envelope through a real invalid `--app` ad-hoc deploy; envelope ABSENT
without --json (plain error text, no leak); unclassified → internal; exit
semantics pinned. Binary smoke: exact envelope JSON on stderr, exit 1,
human path unchanged. Mutation checks (in-place, all reverted):
suppressing the envelope under --json (`if false && jsonMode`) fails both
wired-class tests for the intended reason; renaming a token value fails
the registry test; removing a token from the advertised list fails it
(count + membership). Gates after revert: `go vet ./...` clean;
`go test ./... -race -count=1` all packages ok; gofmt clean on every
touched hunk (deploy.go's pre-existing fleet-rollback stray left alone,
consistent with the C02-C04 posture); contract probes 5/5 PASS. No push
performed.

**S2 + error-site migration list (recorded follow-ups):**

- `server list --json` reshape to an envelope root (coordinated dash
  decode change — the one non-additive MI bump candidate).
- target-unreachable: the ssh.Connect failure returns across commands
  (app list/server status "connecting to", deploy step 6).
- conflict: `preview.AmbiguousPreviewError` (typed and ready — one
  errors.As), `config.ErrServerExists`/`ErrServerNotFound` (dash
  currently matches message text; the envelope gives it a stable code).
- uncertain-outcome / degraded: the C01 journal outcomes (recovery
  dispositions), the T57 "backends deployed but load-balancer activation
  failed" class, LogEntry Degraded rendering.
- unsupported: version-skew refusals (e.g. autodeploy schedule's
  server-binary-lacks-redeploy error).
- Generalizing per-command envelopes for the remaining --json verbs
  (health/log/drift/stats/plan/validate/registry/template/accessory
  lists) is S2's `contracts/` skeleton work, not error-site migration.

## Programme slice (2026-09-23) — C09: `teploy doctor`

First bounded C09 slice (base revision `dda4911`; changes left
committed-free for review, per instruction). Contract addressed:
"`doctor` should diagnose local toolchain, SSH, Docker, registry, proxy,
disk and compatibility without causing deployment. Human progress goes to
the appropriate diagnostic stream; versioned JSON/events and stable exit
codes serve automation."

**Landed** (`internal/cli/doctor.go`, `teploy doctor [--json]
[--server <name>]`):

- **Nine stable checks** (fixed order, pinned by test): `git` (local
  PATH probe — missing git is a WARN, not a fail: git-less boxes deploy
  prebuilt images fine), `config` (the same `config.LoadApp` loader
  deploy uses, so the C05 Compose field contracts, C03 health-mode
  grammar, publish specs and overlay rules surface verbatim in detail;
  ErrNoConfig → fail with a `teploy init` remediation), `ssh` (the
  EXISTING connect path — `ssh.Connect` errors carry the key/auth hints
  and the 078f610 known_hosts algorithm naming, so the doctor detail
  names the presented/on-file algorithms verbatim), `docker` (daemon
  reachability via `docker version --format '{{.Server.Version}}'`),
  `disk` (root-filesystem headroom from `df -B1 -P /`: fail < 2 GiB,
  warn < 10 GiB or ≥ 85% used), `registry` (`docker manifest inspect` of
  the configured ref — a pure registry query that touches no local image
  state, unlike a pull; auth class DISTINGUISHED from unreachable from
  missing, each with its own remediation; build-from-source apps are
  ok-skips), `caddy` (admin API probe inside the caddy container — the
  same command `server status` uses; host/external ingress are ok-skips
  by design), `compatibility` (local version vs the server's
  `/deployments/.bin/teploy` if present — absent is ok (optional
  infrastructure), skew is a warn naming both versions), and
  `repair-debt` (the C01-6 marker via `deploy.ReadRepairDebt` —
  outstanding debt is a warn naming release+attempts; an UNREADABLE
  marker is a visible warn, never hidden).
- **No deployment effects**: every remote command is read-only
  (`docker version`, `docker manifest inspect`, the caddy admin wget,
  `df`, the server binary's `version`, the framed repair-debt read).
  Skip semantics are two-class and deliberate: skipped-because-not-
  applicable (host/external ingress, build app, no server binary, no
  app identity) = ok; skipped-because-input-unavailable (SSH down,
  config unreadable) = fail with the reason. Tests assert the mock's
  ENTIRE call log against a read-only allowlist on both the all-healthy
  and the every-remote-check-failing runs, plus that no files are ever
  uploaded.
- **Output contract**: human table on stdout (check/result/detail rows,
  indented `fix:` remediation lines, closing summary); `--json` emits
  the MI-1 envelope `{machine_interface, checks:[{name, result,
  detail, remediation}], summary:{ok, warn, fail}}` — all four check
  keys ALWAYS present (no omitempty: a stable shape means consumers
  never probe for optional keys), `result` closed to ok|warn|fail,
  summary counts machine-checked against the checks array. Exit codes:
  0 with no fail (warnings included), 1 with any fail, and never 2 —
  that stays `drift --exit-code`'s CI signal (X02 D10); documented in
  the command's help text and README. The report is the successful
  OUTPUT of the command — a failing diagnosis never renders the error
  envelope, stdout stays the data channel.
- **Capability token**: `doctor-diagnostics` added to the MI registry
  (additive, no MI bump); the golden list in
  TestCapabilityTokenRegistry updated to force the addition to stay
  deliberate.

**Evidence** — TDD red level 1 recorded (all new symbols undefined at
compile), green after implementation. New coverage (doctor_test.go, 17
test functions / 30+ subtests): all-healthy run (9 ok, stable order,
read-only call log), exact JSON shape (3 top-level keys, 4 check keys,
enum-closed results, summary cross-check), human table + remediation
lines + summary, per-check pass/fail/warn paths (config grammar from
teploy.yml AND Compose, git missing, ssh unreachable with the
known_hosts algorithm diagnostics carried through, no target, docker
daemon down, disk fail/warn/ok thresholds + parser robustness, registry
auth/missing/unreachable classes + classifier, caddy ok/fail/host/
external skips, compat agree/skew/absent/unreadable, repair-debt
absent/present/unreadable/no-app), the no-effects assertion on a
maximally failing run, exit-code semantics, and a cobra-wired end-to-
end all-OK run. Mutation checks (in-place, all reverted, gates re-run
green after each): a failing check reported ok (docker failure branch
forced to ok) fails TestDoctorDockerCheck and the human-table summary;
doctorExitCode forced to 0 fails all three exit-code assertions;
collapsing the registry auth class into unreachable fails the
auth-distinguished remediation and the classifier. Binary smoke: real
`teploy doctor` / `doctor --json` in an empty directory — table and
envelope as specified, exit 1, no stderr envelope, token advertised by
`version --json`.

Gates: `go vet ./...` clean; `go test ./... -race -count=1` all 25
packages ok; gofmt clean on touched files (pre-existing strays in
deploy.go/secret_audit.go/update_test.go left alone, consistent with
the C02-C04 posture); contract probes 5/5 PASS. No push performed.

**C09 remainder (explicit):** per-command next-recovery-action strings
(doctor's remediation field covers the diagnostic surface; every OTHER
failure path still renders free-text errors — the S2 error-envelope
migration is the machinery, this is the content), shell completion
(cobra completion for the command tree, including doctor's --server
values from servers.yml), config examples executability (README/
docs config snippets that cannot load under the current grammar —
a docs-vs-loader drift sweep), and the doctor surface itself has
natural follow-ons recorded here rather than hidden: multi-server
fleet diagnosis (doctor currently diagnoses ONE resolved target),
DNS/health-path diagnostics, and machine-event streaming (the
"versioned JSON/events" contract's events half — doctor emits one
versioned JSON document per run, not a stream).

## X02 S7 acceptance sweep — 2026-09-23

`scripts/x02-acceptance-sweep.sh` (this repo's executable harness, ADR §6
S7). Legs and evidence (exit 0, all PASS, non-vacuous — each pattern is
verified to match >=1 test before running):

| Leg | Package | Tests | Result |
|---|---|---|---|
| rename | ./internal/config | 5 (rename/update/re-add preserve id; legacy stays id-less; mint shape) | PASS |
| duplicate-identity | ./internal/preview | 2 (preview ID golden; branch identity distinct) | PASS |
| release-identity | ./internal/releasemeta | 2 (absent/present/unreadable; round-trip + path validation) | PASS |
| repeated-request | ./internal/cli | 3 (version/release/attempt-name corpus goldens) | PASS |
| response-loss | ./internal/deploy | 4 (Decide Compensate-vs-Inspect; attribution; predecessor snapshots) | PASS |
| rollback | ./internal/deploy | 4 (state-commit failure restores old workload/route; rollback from recorded spec; fixed-port displacement) | PASS |

The harness fails on any leg failing OR matching no tests (a vacuous pass
is a broken pin). Re-run and paste fresh output here on any contract
change.

## Programme slice (2026-09-23) — C01-1: replacement-owner reconciliation on acquisition

Closes the C01-1 disagreement (docs/C01_RECOVERY_STATE_TABLE.md finding 1):
lock acquisition used to be treated as quiescence — `acquireAutoLock` broke
a stale lock and the deploy proceeded with no observation of the dead
holder's leftover world. Base revision `0c1fe5d`.

**Design:**

- **Takeover signal** — `state.acquireAutoLock` now reports whether the
  acquisition broke a stale auto/heal lock; `state.Lock.TookOver()` exposes
  it (nil lock: false). Fresh acquisitions are unchanged.
- **Productionized observer** — `deploy.Observe` (internal/deploy/
  reconcile.go) is the fault harness's evidence collector as production
  code: docker label inventory, state.json, the managed Caddyfile, the
  per-release record → recovery.Observation, exact names, read failures map
  to Unknown (the never-auto-decide grade). The decision stays the pure
  table's (recovery.Decide); the observer imports the effectful packages,
  not the reverse.
- **Reconciliation gate** — DeployFenced step 1c: when TookOver, run
  `ReconcileAfterTakeover` BEFORE the deploy's first effect. RETRY is the
  only proceed disposition (surfaced to the operator); INSPECT gets ONE
  bounded re-observation (R4's transient-read reconcile trigger) then
  refuses; COMPENSATE/MANUAL refuse immediately. Every refusal carries the
  observed evidence classes and the inspect commands. Deliberately NOT an
  auto-compensator: compensation automation is the F04-keyed recovery-owner
  continuation; refusing with evidence is the safe subset the table
  permits. The reconciliation precedes the first docker run, so a refusal
  has nothing to undo.

**Evidence** — mock tests (internal/deploy/reconcile_test.go): takeover
with a foreign running workload refuses MANUAL with zero `docker run`/state
commits; clean-world takeover proceeds (a crash must not make the app
undeployable); same-version disagreement (state.json names the deploying
release) refuses INSPECT per R6; running-candidate-without-receipts INSPECT;
traffic-on-uncommitted-generation COMPENSATE; persistent unreadable stays
INSPECT; a transient inventory failure recovers via the single
re-observation; Observe classification pinned (_replaced rename = serving
predecessor, stopped = restorable, corpse ≠ running candidate, unreadable =
Unknown). Fixture-verified for real (internal/deploy/
reconcile_integration_test.go, colima docker 29.5.2): owner A's nohup'd
delayed candidate lands after owner B's genuine stale-break acquisition;
TookOver reports true; the production reconciler refuses MANUAL — the
quiescence assumption's RETRY is proven dead in production shape. The
pre-existing fault harness passes unchanged against the same fixture.
Gates: build/vet clean; `go test ./... -count=1` all packages ok (one
pre-existing load-sensitive timing test, cli TestAdmission_NoGoroutinePileup,
flaked once under full-suite parallel load and passes repeatedly in
isolation and in two follow-up full runs — not touched by this slice).

**Residual C01 list (updated):** C01-2/3 remain (guarded pre-commit
effects, fenced shared Caddy lock); C01-8/C01-9 unchanged (F04-keyed).
