# Teploy contracts corpus — MANIFEST

The machine-interface fixture corpus (X02 S2; ADR `_internal/
X02_RESOURCE_CONTRACT_ADR_2026-09-22.md` §4, adopted by
DELEGATED_DECISIONS_2026-09-23 D15). teploy-cli owns the corpus because it
produces the envelopes and sits at the bottom of the stack with no
Neutron/Nucleus dependency and a public mirror.

## Revision table

| Corpus rev | Emitting CLI | Machine Interface | Notes |
|---|---|---|---|
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
