# Migrating to teploy (from Dokploy / Coolify / raw Compose)

Both platforms center on Compose files, and teploy can import that subset
of Compose it can preserve exactly. The contract (programme workstream
C05): **every supplied field is preserved, explicitly translated, or
rejected with a named, actionable error — never silently dropped.** The
classification table below is a summary; the executable authority is the
importer and its conformance tests (`internal/config/compose.go`,
`TestLoadCompose_FieldClassificationInventory` in `compose_test.go`).

## What a migration looks like

1. `teploy setup <server>` on a fresh host (Docker + Caddy + firewall).
2. Put your `docker-compose.yml` in the project directory (or run
   `teploy init`, which offers to import it).
3. `teploy validate` — this either imports or refuses, naming every
   problem field.
4. Fix refusals (below), set `server:` and `domain:`, deploy.
5. Cut traffic over (DNS or proxy) when the app is verified — both stacks
   can run side by side; nothing forces a destructive cutover.

## What converts

| Compose | Becomes |
|---|---|
| The single non-accessory service with ports | The app (`web` process); its container port from short-form `"host:container"` (host side deliberately not preserved — teploy allocates host ports and routes via Caddy) |
| `build:` on the web service | Build context for the image |
| `image:` on the web service | `image:` (no build) |
| Same-`build:` second service | A `processes:` worker running the same image with the service's `command:` |
| `postgres/redis/mysql/mariadb/mongo/clickhouse/meilisearch/elasticsearch/memcached/rabbitmq/nats` images | Accessories (known default ports) |
| Any other standalone-image service | An accessory |
| `environment:` (map or list) | `env:` verbatim (deploy-time `${VAR}` expansion applies) |
| `volumes:` on web or services | Named volumes (managed under `/deployments/<app>/volumes/`) or host binds (source starting `/`) |
| Web `healthcheck.test` exec-list HTTP probe | `health:` path/interval |
| `healthcheck: {disable: true}` / `test: ["NONE"]` | `healthcheck.<process>.disable: true` |
| `restart: always` / `unless-stopped` | Tolerated (teploy's own policies match) |
| `deploy:` with only `replicas: 1` / `mode: replicated` | Tolerated as the no-op default |
| `networks: [default]` (or absent) | Tolerated as the implicit default |
| Services under non-default `profiles:` | Skipped, deliberately — `docker compose up` without `--profile` would not deploy them either |
| `depends_on` | Parsed, not translated: teploy already starts every accessory before any app container. Readiness conditions (`service_healthy`) are **not** waited for |

## What refuses (named errors, before any effect)

- A second service with a **different build context**: "unsupported
  independent build in compose import: `<svc>` (build `"<ctx>"`) while
  `"web"` builds from ... — teploy runs one image per app and cannot
  preserve a separately built service; use the same build context as the
  app, a prebuilt image, or write teploy.yml".
- **Multiple port-publishing non-accessory services**: "ambiguous compose
  import: multiple non-accessory services publish ports (...)".
- Per-field refusals, one named error each: non-default `networks:`,
  `env_file`, `secrets`, `configs`, `extends`, non-default `deploy:`,
  `container_name`, `hostname`, `working_dir`, `entrypoint`,
  `privileged: true`, non-empty `cap_add`, other `restart:` policies,
  long-form/ranged/multi-port publishes, non-web healthchecks without a
  home. Each error names the field, why it cannot be preserved, and the
  alternative ("remove it or write teploy.yml").

What multi-image stacks should do instead: model each independently built
service as its own teploy app on the same server (they share the teploy
network and can address each other by app name), or prebuild images and
run them as accessories.

## Concept mapping from Dokploy/Coolify

| There | Here |
|---|---|
| Project / Application | One directory with `teploy.yml` (or an imported compose file) |
| Environment variables UI | `teploy env set KEY=value` (stored server-side) |
| Secrets | `teploy secret set` (age-encrypted at rest) or SOPS/age `env_files:` |
| Traefik + Let's Encrypt | Caddy, written and reloaded by teploy (ACME default; custom `tls:` supported) |
| Domains / routes | `domain:` per app; preview subdomains via `teploy preview` |
| Databases | `accessories:` (managed containers with `--restart always`) |
| Webhook auto-deploy | `teploy autodeploy setup` (+ `autodeploy.paths:` filters for monorepos) |
| Dashboard | [teploy-dash](https://github.com/useteploy/teploy-dash) (optional, read-state + delegate; the CLI stays the source of truth) |
| Backups | `teploy backup` (data-only) and `teploy dr` (whole-app bundles) |

## Reversible adoption

Nothing in a teploy migration touches the origin platform: state lives in
`/deployments/<app>/` on the host you point at, containers are plain
Docker containers with `teploy.*` labels, and
`teploy remove` retires an app's containers, proxy route, and deploy
state when you want it gone. Rolling back to Dokploy/Coolify means
pointing DNS at the old deployment — run both in parallel until the new
one has proven itself, then decommission the old.

## After importing

- `teploy doctor` — the `config` check re-runs the importer; `disk`,
  `docker`, `caddy` validate the target.
- `teploy plan` — read-only preview of the first deploy's container and
  routing changes.
- Data: migrate database contents with a dump/restore into the new
  accessory (`teploy accessory backup/restore` on the source platform's
  volume export), then cut over.
