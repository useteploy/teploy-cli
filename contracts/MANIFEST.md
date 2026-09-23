# Teploy contracts corpus — MANIFEST

The machine-interface fixture corpus (X02 S2; ADR `_internal/
X02_RESOURCE_CONTRACT_ADR_2026-09-22.md` §4, adopted by
DELEGATED_DECISIONS_2026-09-23 D15). teploy-cli owns the corpus because it
produces the envelopes and sits at the bottom of the stack with no
Neutron/Nucleus dependency and a public mirror.

## Revision table

| Corpus rev | Emitting CLI | Machine Interface | Notes |
|---|---|---|---|
| 1 | post-v0.1.37 main (S2 skeleton) | 1 | First goldens: version handshake, app-list envelope (MI + pre-MI legacy), error envelope (config-invalid, internal, invalid-code), release-record, attempt-name grammar, preview-state eras. |
| 2 | post-v0.1.37 main (S6) | 1 | observation-envelope: schema corrected from the S2 draft shape to the ADR §2.4 canonical form (resource/collected_at/freshness tri-state/error/source/last_known) before any consumer existed; fixtures generated from teploy-dash's real constructors (fresh, stale, unknown-unreachable, unreachable-last-known). |
| 3 | post-v0.1.37 main (C05) | 1 | plan-record: the `teploy plan --out` / `teploy apply` binding record (build + prebuilt-digest valid fixtures, tampered-id invalid fixture); `plan-apply` capability token added to the version handshake (additive). |

## Artifact status

| Artifact | Schema | Fixtures | Producer |
|---|---|---|---|
| version-handshake | yes | valid (real `writeVersion` encoder) | teploy-cli |
| app-list-envelope | yes | valid (real DTO tags) + legacy pre-MI | teploy-cli |
| server-status-envelope | yes (appStatus root) | pending S2 tail (live `server status` capture) | teploy-cli |
| error-envelope | yes | valid x2 + invalid code | teploy-cli |
| release-record | yes | valid container | teploy-cli |
| attempt-name | yes (pattern) | valid + invalid examples | teploy-cli |
| preview-state | yes (canonical/legacy) | valid + legacy + ambiguous | teploy-cli |
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
