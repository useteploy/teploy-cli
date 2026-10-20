# Teploy contracts corpus — MANIFEST

The machine-interface fixture corpus (X02 S2; ADR `_internal/
X02_RESOURCE_CONTRACT_ADR_2026-09-22.md` §4, adopted by
DELEGATED_DECISIONS_2026-09-23 D15). teploy-cli owns the corpus because it
produces the envelopes and sits at the bottom of the stack with no
Neutron/Nucleus dependency and a public mirror.

## Revision table

| Corpus rev | Emitting CLI | Machine Interface | Notes |
|---|---|---|---|
| 16 | main (server-authority spec P3 / plan D4, git sources) | 2 | Additive. New artifacts source-show-envelope (`config source show --json`: repository/branch/poll, public credential facts — type, deploy-key public half + fingerprint, token username, pinned host-key fingerprints — and the per-ref admission state), source-key-envelope (`config source key --json` for ssh/token and `host-key --json`) and trigger-poll-envelope (`trigger poll --json`: one result object per app — ref, head, admitted, reason vocabulary incl. force-push/retry-limit/not-due, deployed_release), all fixtures GENERATED from the real DTOs. version-handshake advertises `git-source-v1` (git: code source, per-app credentials outside /deployments, build_secrets) and `trigger-poll-v1` (the one admitter + reset/retry/install-timer) (regenerated mi2.json). No MI bump. |
| 15 | main (server-authority spec D3: backup target) | 2 | Additive. One backup target per server, stored outside /deployments with age-encrypted credentials and read at run time (`server backup-target set/show/unset`; `backup-target-v1`). New artifacts backup-target-envelope, backup-list-envelope, backup-create-envelope, backup-restore-envelope — fixtures GENERATED from the real DTO encoders (`backup list --json` now reports parsed rows {id, kind, accessory, size_bytes, created_at, key} plus schedules discovered from teploy cron markers; the old bare-array-of-raw-`aws s3 ls`-lines `backup list --json` shape is gone — it was never a corpus artifact and pinned no consumer). `backup create/restore --json` envelopes; restore quiesces the app under its fenced lock (backup-json-v1). version-handshake advertises `backup-target-v1` and `backup-json-v1` (regenerated mi2.json). No MI bump. |
| 14 | main (server-authority plan D2, folderless reads) | 2 | Additive. New artifacts releases-envelope (`teploy releases --app X --host S --json`: container release records newest first with current/rollback_available (+reason); static apps list retained releases — both via the envelope, the folder-static legacy bare array is unchanged), rollback-result (`teploy rollback [--to X] --json`: {app, server, from, to, ok}; failures keep the error envelope) and redeploy-result (`teploy redeploy [--json]`: {app, server, home, release, image, ok}). Fixtures GENERATED from the real DTOs. version-handshake advertises `secret-stdin-v1` (secret set KEY --stdin; folderless secret set/get/list/rm/rotate), `releases-json-v1`, `rollback-json-v1`, `redeploy-v1` (fresh deploy of the current release's recorded image, env re-resolved) and `env-config-guard-v1` (env set/unset refuse keys a Teploy-home config declares) (regenerated mi2.json). No MI bump. || 13 | main (server-authority spec P2 support) | 2 | Additive. New artifact config-merge-envelope (`teploy config merge --base <commit> --file - --json`: merged config + conflicting keys, read-only), fixture GENERATED from the real configMergeDTO. version-handshake advertises `config-merge-v1`, `config-unprivileged-v1` (`config ... --unprivileged` refuses host-reaching changes) and `deploy-home-v1` (`deploy --app X --image T --home teploy` creates a new Teploy-home app) (regenerated mi2.json). No MI bump. |
| 13 | main (server-authority spec P2 support) | 2 | Additive. New artifact config-merge-envelope (`teploy config merge --base <commit> --file - --json`: merged config + conflicting keys, read-only), fixture GENERATED from the real configMergeDTO. version-handshake advertises `config-merge-v1`, `config-unprivileged-v1` (`config ... --unprivileged` refuses host-reaching changes) and `deploy-home-v1` (`deploy --app X --image T --home teploy` creates a new Teploy-home app) (regenerated mi2.json). No MI bump. |
| 12 | main (server-authority spec P1 tail) | 2 | Additive. release-record gains optional `source_sha256` (folder-home deploys store the deployed teploy.yml, detected secrets masked, as meta/<hash>.source.yml 0600) with a new valid fixture. version-handshake advertises `lock-wait-v1` (global `--wait <duration>`; lock refusals name the holder) (regenerated mi2.json). `config-home-v1` now also covers `config home teploy --from-server`, `init --from-server` and `template install --home teploy`. DR bundles are schema 2 (config home + server data); they are not a wire artifact of this corpus. No MI bump. |
| 11 | main (server-authority spec P1, config home) | 2 | Additive. New artifacts config-envelope (`teploy config get|home|set --json`: home folder/teploy, config commit, applied_commit of the running release, content) and config-history-envelope (`config history --json`), fixtures GENERATED from the real DTOs (teploy-home with config changed since deploy, folder-home, two-commit history). app-list-envelope gains optional `undeployed[]` (`--include-undeployed`, v3 tombstones) with a new valid fixture; release-record gains optional `config_commit` and `actor` with a new valid fixture. version-handshake advertises `config-home-v1` and `undeployed-list-v1` (regenerated mi2.json). State schema v3 (v2 + `tombstone`) marks Teploy-home apps; it is server state, not a wire artifact, and older CLIs refuse it by design. No MI bump. |
| 10 | main (server-authority spec P1, events) | 2 | Additive. New artifact events-envelope: `teploy events --json` (per-app server event history, `/deployments/.teploy/apps/<app>/events.jsonl`), schema + one valid fixture GENERATED from the real `writeEventsJSON` encoder (started/succeeded deploy pair + key-names-only env.set). version-handshake advertises `events-v1` and `actor-flag-v1` (regenerated mi2.json). No MI bump. |
| 9 | campaign based on 9939eaf, trigger-fix-r5 (uncommitted source patch; exact source digest in private report) | 2 | Additive. version-handshake advertises `trigger-convergence-v1` and `preview-compare-destroy-v1`: the reserved `--trigger-stdin` interface is now implemented (bounded schema1 stdin request, target-resident durable operation receipts reconciled under the app lease before any preparation, truthful committed/not_committed/unknown results with final JSON on nonzero Cobra paths) and preview destruction compares admitted ownership/generation against authority re-read under the same lease (renewal between listing and destroy is refused without effects). preview-state records/list rows may carry optional `ownership_id`, `generation`, `updated_at`, `operation_key`, `execution_binding_digest`, `source_revision`, `image_digest` (all absent on legacy records). Regenerated fixture: version-handshake mi2.json (two new capability tokens). Consumer activation (Dash) remains gated on its own pinned contract acceptance. No MI bump. Schema corrected 2026-10-19: preview-state canonical `id` accepts the 128-bit (32 hex) form rev 8 introduced as well as pre-rev-8 8-hex records; the rev 8/9 list-row fixtures failed their own schema until then. |
| 8 | campaign based on 9939eaf (uncommitted source patch; exact source digest in private report) | 2 | Plan record schema 2; literal template environment; release/attempt identifier grammar parity; 128-bit preview ID with legacy lookup; additive managed `volume_ownership` for pre-start UID/GID/mode provisioning. |
| 7 | main (L4 cli-defects: ID-created container image resolution) | 2 | Additive. The container object in app-list-envelope and server-status-envelope gains optional `image_id` (full sha256) and `image_tags` (string array), emitted only for containers created by image ID (A52 creates web/worker containers from the immutable ID). For those, `image` now carries the first repo tag instead of the bare 12-hex short ID docker ps reports - restoring the pre-A52 meaning (Ship wave-9: ID-created containers never matched the artifact tag). Name-created containers and existing fixtures are unchanged (no fixture regenerated: the corpus fixtures use name-form images). No MI bump. |
| 6 | main (tailnet preview mode, DELEGATED_DECISIONS §10) | 2 | Additive. preview-state schema gains optional record/list-row fields on both eras (`domain`, `url`, `base_domain`, `http_only`, `allow_ips`) with the invariant url scheme = `http://` iff `http_only` (else `https://`); two valid fixtures GENERATED from the real `preview list --json` row encoder (`previewListRows`, `contracts_golden_test.go`): canonical-list-row (default mode, no exposure keys, https url) and canonical-list-row-tailnet (base_domain + http_only + allow_ips, http url), each wrapped with the artifact's `era`/`app` classification keys (the wire row carries neither). The hand-authored identity fixtures (canonical, legacy, ambiguous) are unchanged. version-handshake gains the `preview-exposure` capability token (additive). No MI bump. |
| 5 | main (X02 S2 tail: server-status fixtures + schema correction) | 2 | server-status-envelope fixtures landed (was "pending live capture"): valid x2 (full healthy observation, partial-caddy-unavailable — the class a target without a caddy container produces) + legacy pre-MI (machine_interface absent, the 42243e2-era shape). Encoder-derived: generated from the REAL `collectServerStatus` via a mock SSH executor (`contracts_golden_test.go`, TEPLOY_UPDATE_CONTRACTS) — synthetic values, real encoder and parse stages; the wire shape was verified against a live `server status --json` run before pinning. Defect fixed in the same commit: the schema had copied the appStatus root since its S2 draft (its own defect-fix commit 08cfb1b said so) and never described the actual serverStatusDTO wire format (server/host/uptime/load/memory/disks/docker/caddy) — rewritten to the real root with strict required-key coverage of the DTO's no-omitempty fields. Additive to consumers (a schema that matched nothing before now matches the wire); no MI bump. |
| 4 | main (X02 S2 tail: server-list reshape) | 2 | **The MI 2 bump** (D8 non-additive): `server list --json` now emits the envelope `{machine_interface, servers[], observed_at}` carrying the per-server fields unchanged (name + id/host/user/role/tags/vpn_ip); the pre-reshape bare map-of-servers root is GONE on the wire and is pinned as the artifact's legacy class. New artifact server-list-envelope (schema + valid + legacy fixtures); version-handshake schema maximum 1→2 and its valid fixture renamed mi1→mi2 (app-list valid likewise — both envelopes now report MI 2). Capability tokens unchanged. Coordinated consumer: teploy-dash decodes both shapes during the transition (MaxSupportedMachineInterface 2). |
| 3 (amended) | main (C05 plan-record corpus + defect fix) | 1 | C05 added the plan-record artifact + plan-apply token (see git history); amendment: server-status schema now carries its own $defs (its $refs never resolved), and app-list fixtures emit [] where the encoder emits [] (null fixtures failed schema + the real dash decode - found by dash's new contracts CI job, fixed here). |
| 1 | post-v0.1.37 main (S2 skeleton) | 1 | First goldens: version handshake, app-list envelope (MI + pre-MI legacy), error envelope (config-invalid, internal, invalid-code), release-record, attempt-name grammar, preview-state eras. |
| 2 | post-v0.1.37 main (S6) | 1 | observation-envelope: schema corrected from the S2 draft shape to the ADR §2.4 canonical form (resource/collected_at/freshness tri-state/error/source/last_known) before any consumer existed; fixtures generated from teploy-dash's real constructors (fresh, stale, unknown-unreachable, unreachable-last-known). |
| 3 | post-v0.1.37 main (C05) | 1 | plan-record: the `teploy plan --out` / `teploy apply` binding record (build + prebuilt-digest valid fixtures, tampered-id invalid fixture); `plan-apply` capability token added to the version handshake (additive). |

## Artifact status

| Artifact | Schema | Fixtures | Producer |
|---|---|---|---|
| version-handshake | yes | valid (real `writeVersion` encoder) | teploy-cli |
| app-list-envelope | yes | valid (real DTO tags) + legacy pre-MI | teploy-cli |
| server-list-envelope | yes (MI 2 reshape) | valid (real `writeServerList` encoder) + legacy bare-map | teploy-cli |
| server-status-envelope | yes (serverStatusDTO root, corrected rev 5) | valid x2 (full, partial-caddy-unavailable; real `collectServerStatus` encoder over mock executor) + legacy pre-MI | teploy-cli |
| error-envelope | yes | valid x2 + invalid code | teploy-cli |
| release-record | yes | valid container | teploy-cli |
| attempt-name | yes (pattern) | valid + invalid examples | teploy-cli |
| preview-state | yes (canonical/legacy; optional record/list-row fields rev 6) | valid (hand-authored identity + 2 generated list rows) + legacy + ambiguous | teploy-cli |
| observation-envelope | yes (§2.4 canonical, rev 2) | valid x4 (fresh, stale, unknown-unreachable, unreachable-last-known; dash encoder) | teploy-dash |
| plan-record | yes | valid x2 (build unresolved-awaiting-build, prebuilt resolved-by-digest) + invalid tampered-id | teploy-cli |
| operation-record | yes | pending S5/S6 (dash) | teploy-dash |
| events-envelope | yes (rev 10) | valid (real `writeEventsJSON` encoder) | teploy-cli |
| config-envelope | yes (rev 11) | valid x2 (teploy-home, folder-home; real configDTO) | teploy-cli |
| config-history-envelope | yes (rev 11) | valid (real configHistoryDTO) | teploy-cli |
| config-merge-envelope | yes (rev 13) | valid (real configMergeDTO) | teploy-cli |
| source-show-envelope | yes (rev 16) | valid x2 (connected, no-source; real sourceShowDTO) | teploy-cli |
| source-key-envelope | yes (rev 16) | valid x2 (ssh, token; real sourceKeyDTO) | teploy-cli |
| trigger-poll-envelope | yes (rev 16) | valid x3 (deployed, force-push-refusal, not-due; real gitsource.PollResult) | teploy-cli |
| backup-target-envelope | yes (rev 15) | valid x2 (configured, unconfigured; real backupTargetDTO) | teploy-cli |
| backup-list-envelope | yes (rev 15) | valid x2 (app, legacy-schedule; real backupListDTO) | teploy-cli |
| backup-create-envelope | yes (rev 15) | valid x2 (volumes, accessory; real backupCreateDTO) | teploy-cli |
| backup-restore-envelope | yes (rev 15) | valid x2 (ok, unhealthy; real backupRestoreDTO) | teploy-cli |
| releases-envelope | yes (rev 14) | valid x2 (container app, static app; real releasesDTO) | teploy-cli |
| rollback-result | yes (rev 14) | valid (real rollbackResult) | teploy-cli |
| redeploy-result | yes (rev 14) | valid x2 (teploy-home, folder-home; real redeployDTO) | teploy-cli |
## Rules

- Fixtures under `valid/` and `legacy/` are GENERATED from the real
  encoders where a CLI producer exists (`internal/cli/
  contracts_golden_test.go`, run with `TEPLOY_UPDATE_CONTRACTS=1` to
  rewrite). Hand-authored fixtures say so in this file. Never edit a
  generated fixture by hand.
- `invalid/` and `ambiguous/` fixtures MUST fail schema validation /
  adoption respectively — they pin refusals, not shapes.
- A corpus change lands in the SAME commit as the code that changed the
  contract, with this manifest's revision table bumped. Non-additive
  changes bump `machine_interface` (D8) and are coordinated with
  teploy-dash's decoder first.
- Legacy fixtures are first-class forever: an id-less server, a pre-MI
  envelope, a slug-keyed preview are states real deployments carry.

## Regeneration

```
cd teploy-cli
TEPLOY_UPDATE_CONTRACTS=1 go test ./internal/cli/ -run TestContracts
```

CI runs the same test WITHOUT the env var: any drift between the corpus
and the encoders fails the build.

## Known downgrade hazard (from the ADR §5 row 1)

An older CLI rewriting `~/.teploy/servers.yml` silently drops unknown
fields, so an `id` minted by a newer CLI can vanish on downgrade. The
file itself cannot enforce it; the mitigation is consumer-side (dash
treats id-vanished as ambiguous-legacy requiring explicit re-binding,
never auto-re-mint). Consumers MUST NOT treat a missing
`machine_interface` field as MI 0 — it means "pre-MI producer", the
legacy decode path.

## 2026-10-07 contract update

PlanRecord schema 2 requires re-planning records from schema 1. The private config binding includes execution commands, hook contents and routing values; environment values retain the established exclusion policy. Static plans bind the selected source and exact plan output exclusion. Release and attempt identifier schemas now match runtime release IDs (1–128 grammar-valid characters); attempts append a dot and 16 hex nonce. Templates emit literal `env_literal` maps for rendered environment values, preserving user passwords through deployment.
