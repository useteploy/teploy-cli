# Resilience: surviving server loss

Teploy deliberately does **not** include a scheduler that moves workloads
between servers. Resilience comes from topology, not orchestration — this
guide is the supported pattern, and the recovery runbook for when a server
dies.

The one-sentence version: **redundancy beats recovery speed.** A fleet that
tolerates a dead server needs no automation to survive one; no automation
makes a single irreplaceable server safe.

## The two rules

**1. Stateless tier: run N+1 active.** Put your web/worker containers on one
more server than the load needs, all active behind the load balancer
(`teploy scale`, or a `servers:` list — Caddy health-checks upstreams and
routes around dead ones automatically). A server dying is then a capacity
event, not an outage: nothing needs to be "recovered" for traffic to keep
flowing. A cold standby you'd have to activate is not redundancy — it's
recovery work you've merely prepared.

**2. Stateful tier: state must outlive any one server.** The database and
uploaded files are the only things a dead server can actually take from you.
Pick one of, in order of preference:

- **Managed database** (RDS, Neon, Supabase, a provider Postgres) — server
  death can't touch it. The right answer for most teams.
- **Replicated self-hosted DB** — a streaming replica on a *different*
  server, promoted manually on failure. Teploy does not automate promotion
  (choosing a new primary is a leadership decision — automating it badly
  causes split-brain, which is worse than the outage).
- **At minimum: scheduled, verified backups off the box.**
  `teploy accessory backup <db> --bucket ... --schedule "0 3 * * *"` for the
  database, `teploy backup schedule` for volumes — to real S3, or a MinIO
  accessory **on a different server**. A backup target on the same server
  dies with it.

  Backups you haven't restored are hopes, not backups. Schedule
  `teploy accessory verify-backup` (or run it after any schema change): it
  restores the latest backup into a scratch container and proves it's
  usable, without touching the live accessory.

Object storage (user uploads) belongs in S3-compatible storage, not on the
app server's disk — then the app tier holds nothing worth recovering.

## What Teploy already does for you

| Failure | Covered by |
|---|---|
| Container crashes | Docker restart policy (`--restart unless-stopped`, always on) |
| Container hangs (up but failing probes) | `teploy heal enable` — probe + restart-in-place on a systemd timer |
| Bad deploy | Health-gated blue/green + automatic rollback; `rollout:` canary for fleets |
| One server of N dies | Caddy LB health checks route around it (N+1 rule above) |
| **The server with your only copy of state dies** | **Nothing. That's what this guide prevents.** |

## Runbook: replacing a dead server

Teploy's recovery model is **human-confirmed**: you decide the server is
dead (not partitioned, not rebooting), then rebuild is mechanical. Total
time is dominated by DNS, not Teploy.

```bash
# 0. Confirm it's actually dead (not a network partition):
#    provider console, ping from a second location, provider status page.
#    If the old server might still be running, STOP it at the provider —
#    two servers both believing they're live is split-brain.

# 1. Provision a replacement (any provider), note the IP.

# 2. Add it to servers.yml under the old name (or a new one):
teploy server add box-2 <new-ip> --user root

# 3. Provision it:
teploy setup box-2

# 4. Restore state FIRST (databases before the app that expects them):
teploy accessory start postgres
teploy accessory restore postgres <timestamp> --bucket my-backups
teploy backup restore <timestamp> --bucket my-backups   # volumes, if any

# 5. Deploy the app at the version the rest of the fleet runs:
teploy deploy box-2 --version <current-git-sha>

# 6. Cut over:
#    - Behind a Teploy-managed LB: nothing to do — health checks admit the
#      new upstream automatically.
#    - Direct DNS to the dead server: repoint the A record. Your recovery
#      time is your DNS TTL — keep it at 300s or less if a server IS your
#      ingress.

# 7. Verify:
teploy status && teploy health
teploy drift        # confirms live state matches teploy.yml
```

Practice this once on a throwaway VM before you need it. The runbook is
five commands; the difference between five minutes and a bad afternoon is
whether step 4 has a verified backup to restore.

## What Teploy will not do (by design)

- **Automatic failover / replica promotion** — leadership decisions under
  partial information cause split-brain. Human-confirm, then promote.
- **Moving workloads off a "suspected dead" server** — same reason. Teploy
  heals *in place* and recovers *by rebuild*, it never re-places.
- **Autoscaling** — capacity is a planning decision; N+1 covers failure.

If you need those, you need an orchestrator (Nomad, Kubernetes) and the
operational weight that comes with it. Teploy's bet is that most fleets
under ~100 nodes are better served by the two rules above.
