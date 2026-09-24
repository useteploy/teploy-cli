# Supported workloads

What `teploy deploy` accepts today, and what it refuses — with the refusal
behavior named. If something you need is in the refused column, the answer
is "not yet", not "silently degraded": every refusal below is a named,
actionable error issued before any container effect unless stated
otherwise.

## Matrix

| Workload | Status | Notes |
|---|---|---|
| Single-image container app (Dockerfile) | Supported | Default path. Build on the server (`rsync` context + `docker build`), or `build_local: true`. Nixpacks used when no Dockerfile is present (requires Nixpacks on the server or locally). |
| Single-image app from a registry (`image:`) | Supported | Private registries via `teploy registry login`. |
| Multi-process from one image (`processes:` web/worker/cron) | Supported | All processes run from the same image; `healthcheck:` per-process overrides for inherited probes. |
| Static site (`type: static`) | Supported | rsync to the server, served by the managed Caddy. Requires Caddy ingress; `ingress: host`/`external` and `tls:` are rejected for static. |
| Compose file as an importer (subset) | Supported (subset) | `docker-compose.yml` in the project dir imports when no `teploy.yml` exists. Every supplied field is preserved, translated, or rejected with a named error — see [migration.md](migration.md) for the classification summary. |
| Templates (`teploy template install`) | Supported | One-command deploys of reviewed community apps (Postgres+Adminer, WordPress, Immich, ...). Catalog: `teploy template list`. |
| Preview environments (`teploy preview`) | Supported | Branch slugs on `preview-<branch>.<domain>` against a pre-built image (`teploy build`). Requires Teploy-managed Caddy. Tailnet-only mode: `--base-domain <tailnet-ip>.sslip.io --http-only --allow-ip 100.64.0.0/10`. |
| Accessories (Postgres, Redis, MySQL, Mariaadb, Mongo, ClickHouse, Meilisearch, Elasticsearch, Memcached, RabbitMQ, NATS, or any standalone image) | Supported | Managed alongside the app with `--restart always`, volumes, ports, env. |
| Multi-image stacks (several independently built services) | Refused | One image per app is the model. A Compose file whose service builds from a different context than the web service refuses at config load: `unsupported independent build in compose import: ... — teploy runs one image per app and cannot preserve a separately built service`. Model as separate teploy apps, or prebuilt images. |
| Multiple web candidates in one Compose file | Refused | `ambiguous compose import: multiple non-accessory services publish ports (...)`. Remove ports from non-app services or write `teploy.yml`. |
| Host port ranges / long-form Compose ports | Refused | Named error from the port grammar; short-form `"host:container"` and non-TCP publishes are what convert. |
| Compose `networks:` (non-default), `env_file`, `secrets`, `configs`, `extends`, `deploy:` (non-default), `container_name`, `hostname`, `working_dir`, `entrypoint`, `privileged`, `cap_add`, `restart:` (other than always/unless-stopped) | Refused per field | One named, actionable error per supplied field whose meaning would be lost (`unsupported compose fields: ...`). Full table: [migration.md](migration.md). |
| Kubernetes-style scheduling, cross-host replica scheduling | Not supported, by design | No scheduler. Fleet semantics are per-host deploys + Caddy LB. See [resilience.md](resilience.md). |

## Ingress modes and their guarantees

| Mode | Deploy strategy | Downtime | Rollback |
|---|---|---|---|
| `ingress: caddy` (default) | Blue/green: new container starts, passes the readiness gate, then traffic switches; predecessor stops | None during the switch (drain via `drain_seconds`) | `teploy rollback` switches back to the predecessor version |
| `ingress: external` | Same container lifecycle; Teploy never touches the proxy (the container joins the teploy network with its app-name alias) | Whatever your proxy's cutover does | `teploy rollback` (container-level) |
| `ingress: host` | Recreate: stop old, start new, on a fixed host port | Seconds per deploy (a fixed port cannot be blue/green) | `teploy rollback` redeploys the previous version and health-gates it (there is no instant container switch — the prior container was removed) |

Static apps (`type: static`) are Caddy-served and use release directories,
not containers; `keep_releases` prunes them.

## Operational limits

Declared, not aspirational:

- **One app = one image.** Every process runs from the same image; there is
  no per-service build. This is the boundary the Compose importer enforces.
- **Single-writer deploys per app per host.** Deploys serialize behind a
  fenced per-app lock; a crashed holder's effects are reconciled by the next
  owner (see [failure-and-recovery.md](failure-and-recovery.md)).
- **Fixed host ports are single-replica.** `ingress: host` and `publish:`
  entries cannot be load-balanced across containers on one host;
  replicas require Caddy ingress.
- **Readiness is a gate, not a liveness system.** `health:` defines what
  "healthy" means before traffic switches; steady-state restart-in-place is
  `teploy heal enable` (bounded, systemd-timer driven).
- **The server needs `rsync` on PATH** for source sync (present on normal
  Debian/Ubuntu images; `teploy setup` installs it). Missing `rsync` fails
  the sync step with `rsync failed: exit status 127` before anything lands.
- **No scheduler.** Surviving server loss is a topology concern —
  [resilience.md](resilience.md) is the supported pattern and runbook.
- **Version identity comes from git** (short hash) or `--version`. A project
  that is not a git repository must deploy with `--version` — the failure
  message says so: `could not determine version from git ... (use --version
  flag)`.
