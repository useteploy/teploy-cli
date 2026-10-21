# Changelog

All notable changes to teploy are documented here. Format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

## [0.1.47] - 2026-10-21

### Fixed

- `teploy remove` now deletes the server-built images (`<app>-build-<version>`)
  of the containers it removes. Before, they stayed forever: the deploy-time
  72-hour image prune skips images a kept container still uses, and remove
  took the containers away without the images. Images are taken from the
  app's own containers (never a name pattern), removal is never forced, and
  pulled images are left alone. `--json` reports them as `removed_images`.

## [0.1.46] - 2026-10-21

### Fixed

- Native repository proof for a CI checkout (`trigger` with a GitHub or
  GitLab repository ID) now compares host and path only. A checkout over
  `ssh://git@gitlab.lan:2222/g/r` matches the API's
  `http://gitlab.lan:8080/g/r` instead of being refused; the API authority
  stays pinned by `TEPLOY_TRIGGER_API_BASE`.

## [0.1.45] - 2026-10-20

### Upgrade notes

- `teploy setup` moves the age identity from `/deployments/.age-key` to
  `/etc/teploy/age-key` (0600), out of the Caddy-readable tree. It leaves
  only a public-key marker at the old path: older CLIs can still encrypt
  (`secret set`) but can no longer decrypt, so their deploys of apps with
  secrets fail. Upgrade every machine and CI image to 0.1.45 before
  running `setup` on a server. Existing secrets keep decrypting with this
  version.
- Backup cron lines that embed bucket, region or credentials keep working.
  `teploy server backup-target set` rewrites them to `teploy backup run`,
  and `teploy doctor` flags any line that still embeds credentials.
- Apps that build from `git:` need the server poll timer (`teploy trigger
  install-timer --host S`); `teploy doctor`'s `git-trigger` check reports
  a missing one.

### Added

- Teploy-home apps can build from a git repository: a `git:` block
  (`repository`, `branch`, `paths`, `poll`, `submodules`) in the config,
  `image:` becoming optional when it is set (`git-source-v1`; contracts
  corpus rev 16 adds `source-show-envelope`, `source-key-envelope` and
  `trigger-poll-envelope`). Works with GitHub, GitLab, Forgejo/Gitea,
  Cursor Origin and any git host over SSH or HTTPS. `context:` and
  `dockerfile:` are relative to the repository root; `build_secrets:`
  passes age-store values to the build as `docker build --secret`
  (values never enter the config, image layers, argv or logs).
- `teploy config source key --app X --host S [--type ssh|token]`
  generates an ed25519 deploy key on the server (public half printed)
  or stores an HTTPS token read from stdin. Credentials live
  age-encrypted outside `/deployments` (Caddy cannot read them) and are
  used via `GIT_SSH_COMMAND` / `GIT_ASKPASS` — never in URLs,
  `.git/config` or command lines. `config source host-key` pins the
  forge's SSH host key (`ssh-keyscan` on the server, fingerprint shown,
  optional `--expected-fingerprint` verification); fetches run with
  `StrictHostKeyChecking=yes` against it. `config source show --json`
  (no secrets) and `config source remove` round out the surface.
- `teploy trigger poll` is the one admitter for git sources: dedup by
  (app, ref, commit), a per-ref watermark with an ancestry check on the
  server-side mirror, one recorded refusal per force-push (refused until
  `teploy trigger reset --app X --ref refs/heads/<branch>`), changed
  paths computed on the server and filtered by `git.paths` (defaulting
  to `context:`), a failed commit retried at most twice, and the newest
  admitted head always deployed (`trigger-poll-v1`). `--all` polls every
  due app for the server timer; `--now --hint <sha>` is the webhook wake
  (the remote head wins over the hint). `teploy trigger retry --app X`
  re-arms a failed commit.
- `teploy trigger install-timer --host S` installs an idempotent systemd
  timer running `trigger poll --all` every minute (each app honors its
  own `git.poll` interval from admissions timestamps; nothing per-app in
  crontab); it is also offered after `config source key`. `teploy doctor`
  gains a `git-trigger` check: the timer is present and its resident
  binary supports `trigger poll` whenever any app has a `git:` source.
- Deploying a Teploy-home app with `git:` set (`teploy deploy --app X`)
  builds the branch head from the mirror under the app lock and records
  the commit as the release's `source_revision` (release provenance and
  the deploy event); a manual deploy honors the same force-push refusal
  and advances the watermark.
- New event kinds `trigger.admit`/`trigger.refuse` detail `reason`, and
  `source.connect` records credential lifecycle (type and fingerprint
  only).
- One backup target per server, read at run time (`backup-target-v1`;
  contracts corpus rev 15). `teploy server backup-target set --host S
  --bucket B [--region] [--endpoint] [--prefix]` stores it at
  `/etc/teploy/backup-target.json` (0600, outside `/deployments`;
  `~/.config/teploy/` fallback for a non-root deploy user, said so on
  output), with the S3 credentials read from stdin as two lines (access
  key, then secret key; prompted with echo off on a terminal) and stored
  age-encrypted with the server's own identity — used whether or not an
  endpoint is set, ambient AWS chain only when none are stored.
  `show [--json]` never emits secrets; `unset` removes the target.
  Setting a target rewrites teploy-generated backup cron lines (by marker)
  to the `backup run` form — hand-written commands under a teploy marker
  are left untouched and reported — and `teploy doctor` gains a
  `backup-cron` check flagging cron lines with embedded credentials
  (fail) or bucket flags (warn).
- `teploy backup create|list|restore|prune|schedule [--app X --host S]
  --json` are folderless and default to the stored target
  (`backup-json-v1`): `list --json` reports parsed rows
  `{id, kind: volumes|accessory|teploy, accessory?, size_bytes,
  created_at, key}` across the volumes, accessories and teploy prefixes
  plus schedules discovered from teploy cron markers (was: raw
  `aws s3 ls` lines); `create --json` reports the created backup's
  identity and size; `restore --json` reports the quiesced receipt. The
  legacy `--bucket`-style flags still work and override the stored
  target. `teploy dr` and `teploy accessory backup|restore|
  verify-backup` default to the stored target as well.
- `teploy backup run --app X [--accessory Y]` — the cron entrypoint, runs
  on the server like `autodeploy redeploy` and reads the target and the
  per-app retention at run time. Scheduled jobs are a single
  argument-free cron line; retention moves to a per-app store
  (`backup schedule` and `accessory backup --schedule` write it;
  `--max-age-days` is new on the latter). `backup unschedule` removes a
  schedule. Failure alerting moved from the cron line into `backup run`
  (same `/deployments/<app>/backup-alert.sh`, same signatures).
- Restore is now quiesced: under the app's fenced lock the app's
  containers (and a generic accessory's container, for accessory
  restores) are stopped, the backup is promoted with the existing
  two-phase recovery discipline, the containers are started again and
  health-checked. On failure — or an unhealthy app afterwards — the
  pre-restore recovery copy is kept and named. Non-interactive restores
  require `--yes`.
- Events `backup.run`, `backup.restore` and `backup_target.set` (detail
  allowlist: id, kind, accessory, error — never bucket or credential
  material; the server-level target event is fanned out to every app
  known on the server).

### Notes

- Static Teploy-home apps with `git:` remain refused (static deploys
  still need a code source); submodules are refused pending
  implementation; `file://` repositories are accepted as a privileged
  config change (same-server sources).

### Changed

- `backup list --json` now emits the versioned envelope above instead of
  a bare array of raw `aws s3 ls` lines. The old shape was never a
  contracts-corpus artifact and no consumer is known to parse it;
  consumers should gate on `backup-json-v1`.

### Deferred

- A `backup: {schedule, keep_last, max_age_days, accessories}` app-config
  key (schedule/retention from Teploy-home config) is deferred: making a
  config key authoritative over crontab entries hooks cron reconciliation
  into deploy, which does not fit this change. Per-app schedules remain
  managed with `backup schedule --app X --host S` (folderless), which
  stores both halves server-side.
- Folderless per-command reads (server-authority plan D2): `teploy secret
  set/get/list/rm/rotate --app X --host S` read exactly the server facts
  they need — no teploy.yml, no reconstructed config. The secrets provider
  comes from the app's Teploy config when it has one, else the folder
  config in the cwd when it is this app, else local; `--provider` still
  overrides. `secret set KEY --stdin` reads the value verbatim from stdin,
  never argv (`secret-stdin-v1`).
- `teploy releases --app X --host S [--json]` for container apps: release
  records (internal/releasemeta) newest first — release id, deployed_at,
  image (+digest), config commit, actor — each marked current or
  rollback-able (containers for that version still present per
  keep_versions retention) with the reason when not (`releases-json-v1`;
  contracts corpus rev 14 adds `releases-envelope`). Static apps keep the
  retained-directory listing; the folder-static `--json` array is
  unchanged.
- `teploy rollback [--to X] --json` emits the machine result
  `{app, server, from, to, ok}`; progress goes to stderr and failures keep
  the structured error envelope (`rollback-json-v1`; corpus adds
  `rollback-result`).
- `teploy redeploy [--app X --host S] [--json]`: a fresh deploy of the
  CURRENT release's recorded image with env re-resolved from `.env`,
  secrets and the recorded config, through the normal fenced deploy engine
  (same version → the existing same-version redeploy path; the deploy
  event carries `trigger=redeploy`). Teploy-home apps deploy from their
  server config; folderless folder-home apps deploy a config rebuilt from
  server records and only when it digest-matches the running release —
  anything unrecorded is an explicit error. Static apps are refused
  (`redeploy-v1`; corpus adds `redeploy-result`).
- CLI env guard: `env set`/`env unset` (folder and `--app`) refuse keys
  declared under `env:`/`env_literal:` by the app's Teploy-home config,
  pointing at `teploy config set` / dash Settings
  (`env-config-guard-v1`).
- The server's age identity moved out of `/deployments` (which Caddy
  mounts read-only into the edge container) to `/etc/teploy/age-key`
  (0600, directory 0700). Reads prefer the new path and fall back to the
  old one, so nothing breaks mid-migration; secret resolution, accessory
  secrets, DR bundles (`--include-age-key`) and restore installs all
  resolve through one helper. `teploy setup` migrates after verifying the
  copy decrypts, leaving a public-key marker (not the private key) at the
  old path so an older teploy keeps encrypting to the same identity;
  `teploy doctor` reports the location (`age-identity` check). Non-root
  deploy users that cannot write `/etc/teploy` keep using the old path.
## [0.1.44] - 2026-10-19

### Added

- `teploy config merge --base <commit> --file - [--json]` is read-only. It
  merges an edit made from an older commit with the current config, key by
  key, and returns the merged file plus the keys both sides changed
  (`config-merge-v1`; contracts corpus rev 13 adds `config-merge-envelope`).
  Editors such as teploy-dash resolve conflicts with it instead of
  re-implementing the merge.
- `teploy config ... --unprivileged` refuses any change that reaches the
  host: host-bind volumes, `caddy_extra`, published or host ports, and
  bind/ingress/network (`config-unprivileged-v1`). Dash passes it for users
  without the admin capability. Adopting a config rebuilt from the server
  (`--from-server`) is not a change and is allowed.
- `teploy deploy --app X --image T [--domain D] [--port P] --home teploy`
  creates a new app in Teploy home from those flags, then deploys it
  (`deploy-home-v1`). An existing folder app is refused; convert it with
  `config home teploy --from-server`.

### Changed

- Config files written by `config set`/`merge`/`edit` use 2-space
  indentation and drop quotes that only marked a value as a string.

## [0.1.43] - 2026-10-19

### Upgrade notes

- **Moving an app to Teploy home locks out older CLIs on purpose.** After
  `teploy config home teploy` (or `template install --home teploy`, or a
  dash-created Teploy-home app), teploy v0.1.42 and older refuse to deploy,
  roll back, start/stop/restart or plan that app ("unsupported state schema
  version 3"). `app list` shows it as an error row. Upgrade every laptop, CI
  image and server binary that touches the app first.
- Older CLIs never read state for `env set/unset`, `secret set`, `remove`,
  `autodeploy setup`, `heal enable`, `preview`, `accessory`, `backup` or
  `lock`, so those are not refused. An old `remove` followed by an old
  `deploy` re-creates the app from a folder. The next v0.1.43 command
  repairs the marker, warns, and records `gate.repair`; `teploy doctor`
  reports it.
- DR bundles created by this version are schema 2, and older CLIs refuse
  them. This version still reads schema-1 bundles.

### Added

- One server-side event history per app, shared by every client. Deploys,
  rollbacks (container and static), start/stop/restart, env set/unset,
  secret set/rm/rotate, lock/unlock, maintenance on/off, heal restarts,
  preview deploy/destroy and remove each append a line to
  `/deployments/.teploy/apps/<app>/events.jsonl` on the app's server, through
  one server helper that holds an flock, assigns a strictly increasing `seq`
  and the server timestamp, rotates past 1 MiB (five files kept) and writes
  0600. Lines carry the actor (`client`, `label`, CLI version, the client's
  operation id); env and secret events carry key names only. A failed append
  warns and never fails the command. The log lives outside the app directory,
  so it survives `remove`, and older CLIs never see it.
- `teploy events [--app <name>] [--since-seq N] [--json]` reads that history
  (`--host` alone lists every app on the server).
- Global `--actor <client>[:<label>]` and `--actor-op <id>` name the calling
  client in the history (default `cli:<user>@<host>`, or `$TEPLOY_ACTOR`);
  the observe audit forward uses the same `client:label`. Capability tokens
  `events-v1` and `actor-flag-v1`; contracts corpus rev 10 adds the
  `events-envelope` artifact.
- Config homes. `teploy config home teploy` moves an app's `teploy.yml` from
  its folder into a git repository on its server
  (`/deployments/.teploy/apps/<app>/config.git`, 0700, pushes refused). Any
  client can then edit it with `teploy config get|set|edit|diff|history|
  restore|pull|push|purge-history`. Every write is a compare-and-swap that
  the server builds under an flock and records as a `config.set` event, with
  `privileged: true` when the change reaches the host (host binds,
  `caddy_extra`, published ports, bind/ingress/network). A conflict exits 1,
  with code `conflict` and the current commit. `config edit` merges
  concurrent edits key by key and asks only about keys both sides changed.
  Configs are validated for portability: no `$VAR` in `env:` (`$$` is a
  literal `$`), no `env_files`, `server:`, local TLS or static build paths,
  and no secret-looking values.
- `teploy deploy --app <app> --host <server>` (or `teploy deploy <app>
  --host <server>` outside a folder) deploys a Teploy-home app from its
  server config under the app lock and records `config_commit` (and the
  actor) on the release. `--image T` commits `image: T` first, so existing
  CI one-liners keep working. Other ad-hoc overrides are refused.
- State schema v3 (v2 plus `tombstone`) marks Teploy-home apps. Older CLIs
  refuse it, which keeps them from deploying such an app from a stale
  folder. Folder deploys from this CLI are refused too, at admission and
  again in the engine at swap time. Never-deployed and removed Teploy-home
  apps are v3 tombstones: hidden from `app list`, shown with
  `--include-undeployed`. Under the lock the CLI repairs what an older CLI
  left behind and records a `gate.repair` event. An app marked v3 whose
  config.git is gone is refused as orphaned until `config home folder`.
- `teploy remove` keeps a Teploy-home app's config and history and writes a
  tombstone. `--purge` deletes them as well.
- `teploy config home teploy --from-server --app X --host H` and `teploy
  init --from-server X --host H`. Both rebuild an app's `teploy.yml` from
  what its server recorded (the applied manifest and the running release's
  record). The result is accepted only when its digest equals the release's
  `manifest_sha256`, so no mount, port or setting can be dropped. Otherwise
  they say what the server does not keep: secret or unrecorded env values,
  hooks, accessories, custom TLS files, access rules. `init --from-server`
  on a Teploy-home app writes a linked copy.
- `teploy template install --home teploy` installs a template as a
  Teploy-home app. Every generated or `--var-stdin` value is moved out
  before the first commit: app env keys go to `teploy secret`, and accessory
  env keys become `secret:KEY`. A template that puts such a value anywhere
  else is refused, so no password enters the config history.
- DR bundles are schema 2. They carry the app's config home and its server
  data (`/deployments/.teploy/apps/<app>`: config.git and events, taken
  under the config writer's lock) and accept schema-v3 state. Cutover
  installs the config, merges events by id, and sets the state marker for
  the resulting home. `--home=bundle|current` resolves a home mismatch with
  an existing target app. Older CLIs refuse schema-2 bundles.
  `teploy backup create` uploads the same server data next to the volume
  archive (`<app>/teploy/<timestamp>.tar.gz`).
- Global `--wait <duration>` waits for another operation's app lock instead
  of refusing. A refusal now names the holder ("locked by dash:alice
  operation op_42 since ...").
- Folder deploys store the deployed `teploy.yml`, with detected secret
  values masked, as `meta/<release>.source.yml` (0600). The release record
  carries its `source_sha256`.
- `teploy doctor` gains a `config-home` check. It lists the apps edited in
  Teploy, and the next command repairs apps an older CLI changed (warn).
  Orphaned apps (marked v3 with no config repository) fail.
- Capability tokens `config-home-v1` and `undeployed-list-v1`. Contracts
  corpus rev 11 adds `config-envelope` and `config-history-envelope`, the
  `undeployed` app-list array, and `config_commit`/`actor` on release
  records.

### Fixed

- `teploy dr cutover` onto a fresh host failed to take the app lock (the app
  directory did not exist yet). It also failed to publish the restored state
  ("manifest_sha256 does not match applied_manifest"), because the
  pretty-printed bundle manifest re-indented the embedded applied manifest.

- `teploy doctor`'s secret-exposure scan no longer aborts with "could not
  scan /deployments" for a non-root SSH user: directories it cannot read
  (database data and backup dirs owned by another uid) are skipped. Build
  contexts are always the deploy user's own, so nothing reportable is lost.
- The same scan no longer counts templates (`*.example`, `*.sample`,
  `*.template`) as secret-bearing files.

## [0.1.42] - 2026-10-19

### Security

- Release metadata no longer stores secret env values. Every record
  (`/deployments/<app>/meta/<release>.json`) used to embed the web
  container's full `docker inspect` env in `recreate.env` (and backfilled
  records also in `env`), secrets included, so backups, DR bundles and any
  copy of the file carried them in plaintext. Values are now kept only when
  their source is provably non-secret (plain `teploy.yml` `env:`/`env_literal`,
  the image's own ENV, `PORT`); every other key is recorded as
  `{key, source}` in `env_redacted`, with no value and no digest. Key-name
  and URL-credential checks back this up. Redaction happens in the single
  record writer, in DR bundle export and in DR restore, so pre-fix records
  never travel or come back.
- Attempt env files (`meta/att/<attempt>/env`, the plaintext resolved env
  passed to `docker run --env-file`) are removed as soon as the release is
  live; nothing reads them afterwards.
- New `teploy meta scrub --host <server> [--app <name>] [--apply]` rewrites
  records written by older versions and removes leftover attempt env files
  (older than 30 minutes). Dry run by default; output names keys and paths,
  never values; each app is processed under its deploy lock; idempotent.
- A recreate spec carrying redacted env is refused instead of starting a
  container with secrets missing. Rollback is unchanged: it recreates from
  docker's own record of the target container, so it uses the values that
  release was deployed with (not secrets rotated since). It now warns, naming
  the keys, when `.env` or the secret store changed after the target release
  was deployed, so a rotation is never silently undone.

### Fixed

- `teploy remove` takes the app's deploy lock, so it can no longer interleave
  with a deploy, heal pass or listener delivery, and it retires what could
  act on the app later: the autodeploy webhook listener unit, the
  scheduled-redeploy cron entry, and the heal and secret-audit timers
  (previously left running against a deleted app). Backup schedules are
  removed only with `--purge`, since a default remove keeps the data they
  back up. The JSON summary lists what was removed (`removed_automation`).
- `teploy app list` (human output) lists every readable app when one app's
  state is unreadable, then reports each problem with its scope and exits
  non-zero. It used to print only the first error and hide every app.
- `teploy setup` installs `git` and `python3` when missing (python3 is
  required by state writes; git by server-side checkouts), and
  `teploy doctor` gains a `server-tools` check (python3 missing fails, git
  missing warns).
- `teploy deploy` no longer aborts with `rsync failed: exit status 23` on
  macOS's system rsync (openrsync) when the build context contains a
  dangling symlink (e.g. a submodule link into a store absent from a fresh
  checkout). openrsync stats every `--files-from` entry through its link;
  such links are now left out of the transfer with a warning naming them
  (GNU rsync still transfers them as links).

## [0.1.41] - 2026-10-14

### Changed

- `app list` observes a whole server in a constant number of SSH round
  trips (one control-plane bundle per 100 apps, one `docker ps` for the
  host, at most one image-tag resolve) instead of 5–6 sequential execs per
  app. On a 41-app production server this took ~27s — longer than
  teploy-dash's 10s fleet probe budget, so the dashboard reported the box
  as unknown with "0 apps, never observed successfully" while every app was
  running. Measured after the change: ~2s, byte-identical output (same apps,
  releases, containers, locks; only live timestamps differ). Error scopes
  shift with the batching: an unreadable or corrupt state file is still that
  app's `state`-scope error, but a Docker failure is host-wide and now
  surfaces once at the top level (`containers` scope) instead of being
  duplicated into every app's errors.

## [0.1.40] - 2026-10-11

v0.1.39 was tagged from the wrong commit (`b3f9a11`, which predates the
v0.1.38 fix), so it shipped everything below **without** the secret-newline
fix and refused any multi-line secret again. v0.1.40 is the intended release:
everything below plus v0.1.38. Skip v0.1.39.

### Upgrade note

- **Managed volume directories now refuse unsafe ancestors.** Before a
  deploy touches `/deployments/<app>/volumes/...` or an accessory's data
  directory, every parent directory must be owned by root or the deploy user
  and not group/world writable. Servers whose `/deployments/<app>` was
  created group-writable (e.g. mode 775 under a 002 umask) stop at the first
  deploy. The error now names the directory and the fix
  (`sudo chmod go-w <dir>`, not recursive); it used to name neither.

### Fixed

- Secrets keep the v0.1.38 behaviour: one trailing newline is dropped before
  the container env file is written (lost in v0.1.39).

### Changed

- **Machine interface 2: `server list --json` emits the machine
  envelope.** The output is now
  `{"machine_interface":2,"servers":[{"name","id","host","user","role","tags","vpn_ip"}],"observed_at"}`
  — every per-server field the old bare map carried, with the map key
  promoted to a `name` field — where it previously emitted a bare
  map-of-servers at the root. Removing the bare map from the wire is a
  non-additive machine-interface change: the whole binary now reports
  `machine_interface: 2` in every `--json` envelope (MI 2 = MI 1 plus
  this reshape; no capability token changed). Machine consumers pinned
  to MI 1 must move to the envelope; teploy-dash ships a decoder for
  both shapes in the same release.

### Added

- **Tailnet preview mode.** `teploy preview deploy` takes
  `--base-domain <domain>` (hostname base instead of the app domain, e.g.
  `100-64-1-2.sslip.io`), `--http-only` (plain HTTP site, no ACME) and
  `--allow-ip <ip|cidr>` (repeatable; everything else gets 403). The mode
  is stored in the preview record (`base_domain`, `http_only`,
  `allow_ips`, all omitted for default previews) and inherited by later
  deploys of the branch unless overridden (`--http-only=false`,
  `--allow-ip ""`), so a blue/green update never silently re-enables
  HTTPS or drops the allowlist. `preview list --json` rows gain `url`
  (`http://` for HTTP-only, else `https://`); `domain` is unchanged. The
  `preview-exposure` capability token is advertised. Preview identity
  (`<app>-p-<hex8>`) and blue/green are unchanged; records without the
  new fields behave exactly as before.

- **Plan/apply with drift invalidation (C05).** `teploy plan` now renders
  the full effect set — routing (domain/ingress/port/publishes),
  environment keys, storage volumes, resource limits, accessories —
  alongside the container diff, and classifies image data honestly:
  resolved-by-digest, mutable-tag-resolved-at-plan-time, or
  unresolved-awaiting-build (a build plan binds its build inputs —
  context fingerprint + Dockerfile identity — instead of guessing).
  `teploy plan --out FILE` records a plan identity (effective-config
  digest, target version, server/app, deployed-state generation).
  `teploy apply FILE` re-derives every binding input and refuses,
  naming what drifted (config edit, overlay flip, moved git version,
  changed build inputs, or any deploy/rollback in between — the state
  generation), then executes through the normal deploy engine with the
  plan id stamped into the release's provenance receipt. A floating-tag
  plan (timestamp version) is refused outright — it can never be
  bound. Under `--json` a drift refusal classifies as the error
  envelope's `conflict` code; the `plan-apply` capability token is
  advertised.
- **Compose import: the web service's `environment:` and `volumes:` are
  now translated** into `env:`/`volumes:` (host binds keep their full
  path). Both were silently DROPPED — only accessories were parsed —
  so an imported stack deployed without any of the web service's
  environment or storage.

## [0.1.38] - 2026-10-08

### Fixed

- **Deploys no longer fail on secrets stored with a trailing newline.**
  Since v0.1.37 secrets decrypt exactly as stored, so values written by
  older releases (which kept one trailing line terminator) were rejected by
  the env-file writer with "env value for X spans multiple lines". When
  building the container `--env-file`, exactly one trailing `\n` or `\r\n`
  is now dropped from secret values only, with a warning naming the keys
  (never the values). Stored values, `secret get`, templates and kv are
  untouched; embedded newlines and values with two or more trailing
  newlines are still rejected, and `env:`/tag values are never trimmed.
  `teploy secret set KEY=value` also warns when a value ends in a newline
  (it still stores exactly what is passed). To clear the warning, re-set
  the secret: `teploy secret set KEY=value`.

## [0.1.37] - 2026-09-22

### Fixed

- **Webhook deploys build the authenticated commit.** A delivery now
  pins the build to the payload's exact commit (fetch-verify-reset);
  if that commit was force-pushed away the deploy fails loudly naming
  both commits instead of silently building the moving branch tip.
  The pin rides the durable admission ledger through supersede and
  crash-resume.
- **Preview updates no longer take the preview down.** A preview update
  now runs blue/green: the candidate starts under a version-suffixed
  name with its own network alias, passes a readiness gate, the route
  switches, and only then is the predecessor retired — a failed
  candidate leaves the old preview serving. `teploy preview prune`
  prunes expired previews across all apps (both record eras,
  idempotent, 72h default TTL) and is cron-able; the deploy-time prune
  uses the same core.
- **SSH host-key mismatches now name what was presented and what
  known_hosts holds** (key algorithms included), instead of a bare
  mismatch error (found via ship's delivery provisioning).

### Added

- **Crash-recovery evidence for deploys (C01 design obligations):**
  readiness receipts (exact candidate IDs + probe outcomes) and
  predecessor snapshots (exact container IDs) persist per attempt at
  the moment they become true, so recovery can distinguish
  compensable states from inspect-only ones and restore exactly what
  was displaced; the deploy log records DEGRADED outcomes (traffic
  switched but retirement partially failed) instead of clean success.

## [0.1.36] - 2026-09-22

### Fixed

- **Preview environments no longer collide across branch names.**
  Branches that sanitize to the same slug (`feature/login` and
  `feature-login`) used to share one preview record — the second
  deploy destroyed the first's state, container and route. Previews
  now carry canonical IDs (`<app>-p-<hex8>`, derived from the app and
  the FULL branch ref) through state files, container names, network
  aliases, Caddy routes and domains; the slug is only a display
  prefix. Existing slug-keyed records are adopted when their stored
  full branch matches; a genuine collision surfaces an explicit error
  naming both branches instead of guessing or deleting.
- **Webhook deliveries during a running deploy were acknowledged, then
  silently dropped.** The deploy lock returns immediately when already
  held — it does not queue — so every push arriving mid-deploy got a
  200 and then nothing. Admission is now durable before the
  acknowledgment (an fsync'd append-only ledger), with one bounded
  newest-wins pending slot per app (older pending deliveries are
  marked superseded, never piled up in goroutines), and a listener
  restart resumes admitted-but-unprocessed work exactly once.
  Persistence failure refuses with 503 + Retry-After instead of
  acknowledging something that is not durable.

### Changed

- **Scheduled redeploys run through the deploy engine.** The cron
  script no longer reconstructs the container from `docker inspect`
  with its own stop/rm/run (no lock, no health gate, no release
  record, no rollback, a stop-to-start downtime window). It keeps the
  cheap digest pre-check and, when the digest moved, invokes the new
  server-side `teploy autodeploy redeploy` — the same fenced,
  health-gated deploy path as the webhook listener. `schedule` gains
  `--branch`, installs the server binary, and verifies it supports the
  command. **Re-run `teploy autodeploy schedule` on existing apps to
  upgrade an installed script**; until then the old script keeps its
  previous behavior.

## [0.1.35] - 2026-09-22

### Fixed

- **Compose import no longer loses the application port.** `ports:
  ['8080:3000']` means container port 3000 bound to host 8080; the
  importer previously used ports only to pick the web service and
  discarded them, so the app deployed as `:80`, failed its health check
  and rolled back. The container port now imports as the application
  port (with bare ports, IP-prefixed bindings and IPv6 forms supported;
  non-TCP entries preserved into `publish`), and unsupported grammar —
  ranges, long-form port objects, multiple distinct container ports,
  UDP-only services — is refused naming the service and the reason
  instead of being silently reinterpreted.
- **Compose import refuses services it cannot faithfully deploy.**
  Previously a service built from a different context than the app's
  (`jobs: build ./jobs` next to `web: build ./web`) was silently
  flattened into a process of the app's image: the wrong code ran under
  the right command and the import reported success. It is now refused
  naming the service, its build context and the remediation (shared
  context, prebuilt image, or teploy.yml). Same-build workers are
  unaffected. This is a deliberate behavior change: files that imported
  "successfully" while deploying something other than what they declared
  now fail fast at import time.
- **Compose fields are classified instead of silently ignored.** The
  importer's non-strict YAML parse accepted files using `healthcheck`,
  `networks`, `secrets`, `configs`, `profiles`, `deploy`, `env_file`,
  `entrypoint` and security options while dropping their semantics.
  Now: service healthchecks translate to teploy's `health:` block,
  no-op values are tolerated, non-default profiles skip the service
  (matching `docker compose up` semantics), metadata is ignored with
  reasons, and everything with semantics teploy cannot preserve is
  rejected naming the service and field. Another deliberate behavior
  change in the same spirit as above.

### Added

- **Crash-recovery state table for the deploy lifecycle** (design spike
  for the transaction work): an eight-state lattice with an exhaustive,
  property-tested disposition function (retry / inspect / compensate /
  manual) over crash evidence, an ADR mapping it onto the existing
  fenced-lock and release-record machinery, and an integration-tagged
  fault harness (`go test -tags integration ./internal/deploy/recovery`
  with `TEPLOY_FAULT_*` env) that drives a real SSH+Docker host through
  late-effect-after-owner-death, side-effect-without-receipt and
  stale-holder scenarios. No deploy behavior changed in this release by
  this table; it is the specification the recovery work implements
  against.

## [0.1.34] - 2026-09-11

### Fixed

- Template rendering is YAML-safe with path-keyed generated secrets and
  a bounded, validated registry fetch; the templates corpus test renders
  the whole catalog.
- Homebrew formulas carry a version test block.
- Deterministic Compose import (sorted web-service selection) and
  exec-form command quoting that survives the container's `sh -c`
  re-parse.
- Registry-port image references parse correctly in backup/restore, and
  `.env` is archived/restored at its app-level location.
- Scheduled backups: only the archive just created is uploaded,
  `--endpoint` applies to every aws call, `%` escapes survive crond, and
  accessory restore uses a per-invocation temp directory; MySQL
  dump/restore passes the root password via `MYSQL_PWD` instead of
  argv.

## [0.1.33] - 2026-09-01

### Fixed

- **`deploy` served a stale image whenever one was already on the server.**
  `ensureImage` skipped `docker pull` any time the image existed in the host's
  local cache, which treated a mutable tag exactly like an immutable one.
  Observed live: an app whose `teploy.yml` named a registry image with no tag
  (so `:latest`) had a `:latest` already on the host; CI pushed a fresh
  `:latest` for five days and teploy never pulled one of them. Every deploy
  started a container named after the newer commit, passed its health check and
  reported success while production served the build from five days earlier.
  Nothing could catch it from the outside — the container name is a label teploy
  writes, so `docker ps` agreed, and `teploy drift` compares live state to deploy
  state by name, so it agreed too. A later manual `docker pull` reported
  "Downloaded newer image".

  Now only a **digest-pinned** reference (`repo@sha256:...`) is served from the
  local cache, because it is content-addressed and cannot move. Every tag, and a
  bare repo, is pulled on every deploy. A tag that looks like a git sha is still
  mutable by convention only, so it is not special-cased. The cost is a manifest
  check, not a re-download.

  The out-of-band case the skip was added for — an image built or `docker
  load`ed on the server that exists in no registry — still works: when the pull
  fails and a copy is present, the deploy falls back to it and says so
  (`WARNING: pull failed ...` / `Falling back to the local copy ... it may be
  older than the registry`), so "pulled fresh" can never be mistaken for
  "registry unreachable, using what was already here". A failed pull with
  nothing cached is still an error. Only `ensureImage` had this shape; the
  other pull sites (`singledeploy`, `autodeploy serve`, `accessory upgrade`)
  already pulled unconditionally.

- `accessory backup --schedule` generated a cron line calling a bare `teploy`
  while installing the binary to `/usr/local/bin`, which is not on cron's
  `PATH=/usr/bin:/bin`. The job was listed by `crontab -l`, cron was running,
  and it had never executed once in 48 days. The generated line carries no
  output redirect, so the failure went nowhere. The cron entry now uses the
  absolute path.

- `accessory verify-backup` had no `--local`, so it could not be pointed at the
  backups it exists to prove — the nightly jobs run as a local cron job writing
  to a MinIO on `127.0.0.1`, where there is no host for the SSH path to reach.

- `accessory verify-backup` staged both the downloaded archive and its extract
  in `/tmp`, a tmpfs on most distributions and therefore sized by RAM. A 28 GB
  dataset on a host with a 15 GB `/tmp` failed with `tar: Wrote only 2560 of
  10240 bytes`, which reads as a corrupt archive and was not. Staging moved to
  the disk-backed `/var/tmp`; `TEPLOY_VERIFY_TMPDIR` overrides it.

### Added

- `teploy secret rm KEY [KEY...]` — delete secrets from the local age store.
  `secret` had get/list/put/rotate/set/setup/status/audit/db and no way to
  remove anything, so a key that had to go away (a revoked vendor credential, a
  retired integration) could only be overwritten with a dummy value and stayed
  injected into every container. Reports each key as removed or as never set;
  a key that is not set is not an error, so re-running a revocation is safe.
  Local store only — `--provider openbao` is refused with a clear message
  rather than silently removing nothing, since versioned-KV delete/destroy is
  not the local store's unlink.

## [0.1.32] - 2026-08-31

### Fixed

- `deploy` labelled the deploy with the local git HEAD even when deploying a
  prebuilt image, so the container name, `teploy log` and the operator's screen
  all named a commit whose code was not running. Observed as "Deployed version
  fe82bf0" while running the image tagged `462d7a7`. The version now comes from
  the image when one is given; `--version` still wins over everything.

  This was never specific to the `--image` flag — a `teploy.yml` pinning a
  prebuilt image hit the identical problem with no flag passed.

  **Behaviour change:** with an image that carries no usable version (untagged,
  or `:latest`), `deploy` now labels with a timestamp rather than the git hash.
  A git hash there asserts a commit that may not be what `:latest` resolves to;
  a timestamp asserts nothing except uniqueness, which is the honest answer when
  the reference genuinely does not identify a build. `:latest` is refused
  outright as a version: reused, it gives every deploy the same container name
  and leaves `CurrentHash == PreviousHash`, disabling rollback.

- `plan` resolved the target version by the old rule, so it predicted container
  names `deploy` would not create. It now uses the same resolution.

### Added

- `LogEntry.image`, so `teploy log` records the image a deploy actually ran.
  The version alone could not be checked against anything, and the log is
  precisely where a rollback target gets chosen. Omitted for entries with no
  image (heal, lifecycle, static); old log lines parse unchanged.

## [0.1.31] - 2026-08-31

### Added

- `teploy lock status` — report the current deploy lock without touching it.
  Locks were writable from both ends (`deploy` takes an auto lock, `lock` takes
  a manual one, `unlock` releases) but readable from neither: nothing could ask
  whether one was held. So "a deploy is running", "a human froze deploys" and
  "a deploy crashed and left its lock behind" were indistinguishable from the
  outside — the last of which self-heals on the next deploy and needs no
  intervention at all, while the middle one needs a person.

  Read-only: it never creates, refreshes or releases a lock, not even the app
  directory (`teploy lock` calls `EnsureAppDir` first; this doesn't). Staleness
  comes from the same TTLs `AcquireLock` breaks locks by — 30 minutes for auto,
  2 minutes for heal, never for manual — via a new exported
  `state.LockInfo.IsStale`, so the report can't drift from the behaviour it
  describes. Exits 0 whether or not a lock is held; a held lock is a state to
  report, not a command failure. `--json` emits
  `{app, server, locked, type, user, message, since, stale}` with `locked` and
  `stale` always present, which is what lets CI ask "is a deploy running?"
  without parsing prose or shelling out to `pgrep`. Supports `--app`/`--host`
  like `lock` and `unlock`, so it works with no `teploy.yml` on disk.

## [0.1.30] - 2026-08-24

### Fixed
- `teploy remove` — a crash-looping process (`restarting`) is now stopped
  before removal instead of aborting the whole remove with docker's "container
  is restarting" error.
- `teploy remove --purge` — accessory data owned by the engine's uid (Nucleus:
  10001) could not be deleted by a non-root deploy user; the purge now falls
  back to an in-container `rm` as root, matching how the directories were
  handed over on start.

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
