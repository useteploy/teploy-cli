# Failure and recovery

What failure looks like from the outside (exit codes, the structured error
envelope), what `teploy doctor` diagnoses, what happens when a deploy is
interrupted mid-flight, and the disaster-recovery bundle family. The
behavioral contract behind the crash-recovery machinery is
[C01_RECOVERY_STATE_TABLE.md](C01_RECOVERY_STATE_TABLE.md); this page is
the operator-facing version.

## Exit codes and the error envelope

Exit codes are stable and minimal: `0` success, `1` command failure. `2`
is reserved as `teploy drift --exit-code`'s CI signal and is never used
by other commands (`doctor` fails with 1, never 2).

Under `--json`, a failed command additionally writes one JSON document to
**stderr** (stdout keeps only successful output):

```json
{"machine_interface":2,"code":"config-invalid","message":"invalid teploy configuration","detail":"invalid teploy.yml: 'domain' is required"}
```

The closed code taxonomy:

| Code | Meaning | Wired today |
|---|---|---|
| `config-invalid` | Config (teploy.yml/TOML/destination/Compose) failed to load or validate, or a deploy request was refused before any effect (admission) | Yes — config errors and deploy admission refusals |
| `conflict` | The request is coherent but the world moved under it (plan/apply drift refusal) | Yes |
| `internal` | Everything else, including not-yet-migrated failure sites | Yes (default) |
| `target-unreachable` | SSH/transport failure | Reserved — currently classifies as `internal` |
| `unsupported` | The verb needs a capability this binary lacks | Reserved |
| `uncertain-outcome` | The effect's fate is unknown pending reconciliation | Reserved |
| `degraded` | Success with a flag — traffic switched but the outcome is not clean | Reserved |

Reserved means the code exists in the schema and consumers must decode it,
but no error site classifies into it yet; treat an unknown code as
`internal` (forward-safe). Machine consumers: stderr is the error channel,
stdout is data, and the exit code stays the pass/fail signal.

A note on honest outcomes: a deploy whose traffic switched but whose
predecessor retirement partially failed is recorded in `teploy log` as
`DEGRADED` (with the reason), not as a clean `ok` — filter on status, not
just success.

## teploy doctor

Read-only diagnostics; a run that fails checks deploys nothing. Checks:

| Check | What it verifies |
|---|---|
| `git` | Local git present (warn only — prebuilt-image deploys do not need it) |
| `config` | teploy.yml/TOML or the Compose importer accepts the project (grammar errors arrive verbatim) |
| `ssh` | Key-auth connectivity to the app's server (or `--server`) |
| `docker` | Remote Docker reachable |
| `disk` | Root filesystem headroom (fail < 2 GiB free; warn < 10 GiB or > 85% used — deploys write layers, backups and attempt artifacts to `/`) |
| `registry` | The configured `image:` is reachable; auth failures distinguished from unreachable |
| `caddy` | Caddy admin API (Caddy ingress only; skipped-ok for host/external ingress) |
| `compatibility` | Local teploy version vs the server-side teploy binary when one exists (autodeploy installs one at `/deployments/.bin/teploy`) |
| `repair-debt` | Outstanding release-record repair debt (below) |

`--json` emits `{machine_interface, checks[{name,result,detail,remediation}], summary}`;
result is `ok|warn|fail` and every check always carries all four keys.
Failing checks each print a `fix:` line in human mode.

## Interrupted deploys

A deploy is a sequence of durable steps (attempt artifacts, containers,
readiness receipt, traffic switch, fenced state commit, predecessor
retirement, terminal record). The machinery around an owner that dies
mid-sequence:

- **Fenced per-app locks.** A new owner that takes over a stale lock does
  not assume quiescence: it observes the target's real evidence (running
  containers by exact name/label, receipts, authority state) and runs the
  recovery decision before its first effect. Safe-to-retry is the only
  disposition that proceeds automatically; anything ambiguous refuses
  with the observed evidence rather than guessing.
- **Dispositions** (what the decision function can order): RETRY
  (idempotent tail — record writes, predecessor retirement), INSPECT
  (effects landed without receipts — re-observe, never invent success),
  COMPENSATE (uncommitted traffic — undo via the recorded predecessor),
  MANUAL (unattributable workloads, unreadable authority, traffic on an
  uncommitted generation with the predecessor gone). MANUAL means the
  operator reconciles by hand; the refusal message names what was seen.
- **Repair debt.** If the post-commit release-record write fails, a
  `/deployments/<app>/repair-debt.json` marker is persisted; the next
  deploy repairs the record from live containers before its own work,
  then clears the marker. `teploy status` and `teploy doctor` report
  outstanding debt.
- **Predecessor snapshot persistence.** The predecessor container
  identities are journaled with the attempt
  (`meta/att/<hash>.<id>/predecessors.json`) before any new container
  starts, so compensation after a crash targets exactly the recorded
  containers rather than an inference.

What this means operationally: after a crashed or canceled deploy, the
command to run first is `teploy doctor`, then `teploy status`. If the
deploy refused with a recovery-evidence error, that is the MANUAL
disposition — read the named evidence, `teploy rollback` if a predecessor
is recorded, and only then redeploy. Never respond to an uncertain deploy
by blind re-deploying; the CLI will refuse where it cannot attribute, and
that refusal is the safety working.

Fleet note: a multi-server rollout is a sequence of per-host recorded
outcomes, not a global transaction — see the staged-rollout section of the
README for canary and failure-budget behavior.

## Self-heal (steady state)

`teploy heal enable` installs a systemd-timer probe that restarts an
unhealthy **web** container in place (bounded attempts/backoff) — for
"container up but failing", not for deploys. `teploy heal status` /
`teploy heal disable` manage it.

## DR bundles (teploy dr)

`teploy backup` is the data-only family (volume archives, single
accessories). `teploy dr` bundles the whole application: state, release
records, the applied manifest, secret references (or explicitly opted-in
encrypted material), routing identity, and consistency-labeled data
snapshots.

```bash
# create (S3 or a plain directory on the server)
teploy dr create --bucket my-bucket            # or --dir /srv/dr-bundles
teploy dr create --include-secrets             # opt in: age ciphertexts, resolved .env,
                                               # accessory credentials (never default)
teploy dr create --include-age-key             # + the key itself, so a fresh host can
                                               # decrypt (requires --include-secrets)
teploy dr create --stop-app                    # quiesced volume snapshots (app restarted after)

teploy dr list --dir ...                       # bundle ids
teploy dr show <id> --dir ...                  # manifest: snapshots + consistency,
                                               # secrets mode, routing, recovery plan

teploy dr restore <id> --dir ...               # isolated restore into /var/tmp/teploy-dr,
                                               # boots scratch engines + app container,
                                               # validates, writes an RPO/RTO receipt.
                                               # Nothing under /deployments is touched.
teploy dr cutover <id>                         # the explicit mutation: promote a
                                               # validated staged restore over the live app
                                               # (originals kept for two-phase recovery)
```

Verified against a scratch host (this branch): `create --dir`, `list`,
`show`, and `restore` — the restore receipt reported staging path, RPO/RTO,
per-check results (`app ... pass image=... running`), and the exact next
command (`teploy dr cutover <id>`). A restore that fails validation
exits non-zero with "nothing live was touched"; missing secret keys fail
before any mutation. After a cutover, run `teploy deploy` to bring the
app container and routing live from the restored state.

Snapshot consistency is labeled per snapshot: engine dumps are
engine-consistent; raw volume copies are crash-consistent unless the app
was stopped (`--stop-app`) or you assert a volume quiesced by hand
(`--quiesced-volume NAME`). The labels are recorded in the manifest —
read them before trusting a volume snapshot of a writing database.

For the topology-level version (N+1, state off-box, the dead-server
runbook), see [resilience.md](resilience.md).
