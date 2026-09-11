# Open audit items

Unresolved findings for this repository from the ChatGPT-led audit series (2026-09-09 through 2026-09-11, passes 1-5; register: teploy-neutron-lullmail expanded audit). Every P0/P1 finding has been fixed and verified; the items below are the remaining P2/P3 tail plus one item needing validation. Fields are quoted from the audit register; line references point at the review commits listed per item where recorded.

Open items: 7 P2 (7 total)

## useteploy__teploy-cli-01 - P2 - Open

**Template values are inserted without YAML escaping**

- Kind: Confirmed from source
- Evidence: Fetch performs strings.ReplaceAll on raw YAML for each supplied variable. It does not distinguish a scalar value from YAML syntax. The sampled useteploy/templates database templates put {{db_password}} in unquoted scalar positions.
- Impact: A supplied value containing a colon followed by a space, a newline, or YAML-significant quoting can make the generated configuration invalid or change its structure. This is a rendering correctness issue; a remotely exploitable trust boundary was not established.
- Proposed fix: Use parsed YAML nodes and typed value substitution, or a rendering layer that correctly serializes complete scalar values. Specify how embedded placeholders in larger strings are handled; validate the rendered schema before deploying.
- Acceptance test: Render passwords containing quotes, colon-space, leading #, newlines, true, and leading zeros. Parse the result and assert the recovered string equals the exact input.
- Review commit: `2a5e56bd1cd9b82cda3ec80ad221c316ff795cb1` (last reviewed 2026-09-10)

## useteploy__teploy-cli-02 - P2 - Open

**Repeated generated-secret keys overwrite the returned credential record**

- Kind: Confirmed from source
- Evidence: GenerateSecrets generates a fresh secret for each matching line but stores results as generated[key] using only the bare YAML key. Two accessories with POSTGRES_PASSWORD: generate receive different secrets, while only the last appears in the returned map.
- Impact: The documented immediate-install workflow relies on this map to show all generated credentials. Earlier credentials can therefore be omitted from that return value, even though they were written into the rendered configuration.
- Proposed fix: Return credentials keyed by their complete configuration path, or as structured entries with accessory and environment-key identifiers. Use explicit named references when several services are intended to share one generated secret.
- Acceptance test: Include two nested accessory env sections with the same key name. Verify both exact generated values are retrievable with distinct identities. Preserve the existing distinct-key tests.
- Review commit: `2a5e56bd1cd9b82cda3ec80ad221c316ff795cb1` (last reviewed 2026-09-10)

## useteploy__teploy-cli-03 - P2 - Open improvement

**Validate and bound remotely fetched template content**

- Kind: Improvement
- Evidence: List decodes the response body directly, and Fetch uses io.ReadAll. A 30-second HTTP timeout exists, but no response-size limit or residual-placeholder/schema validation is present in this module.
- Impact: An unexpectedly large or malformed registry response can consume unnecessary memory or fail later in deployment with a less useful error. The default registry is trusted GitHub content; this is hardening, not proof of an attacker-controlled production endpoint.
- Proposed fix: Add explicit index/template byte limits, template-name validation, required-variable checks, rendered-schema validation, and an optional pinned registry revision recorded with the deployment.
- Acceptance test: Use httptest responses that exceed each size limit, omit required variables, contain malformed YAML, or return duplicate catalog entries; assert clear failures before deployment.
- Review commit: `2a5e56bd1cd9b82cda3ec80ad221c316ff795cb1` (last reviewed 2026-09-10)

## useteploy__teploy-cli-08 - P2 - Open

**A registry port makes database-image detection choose the wrong backup strategy**

- Kind: Source-confirmed
- Evidence: isDBType calls strings.Split(image, ":")[0] before taking the final slash component. For registry.example:5000/postgres:16 the retained text is registry.example, not postgres.
- Impact: A PostgreSQL/MySQL/Redis image hosted behind an explicit registry port falls into the generic filesystem backup/restore branch instead of the database-aware path. The helper mismatch was reproduced locally; resulting data consistency depends on the engine and workload.
- Proposed fix: Parse image references correctly, separating registry ports from the final tag/digest. Prefer an explicit accessory backup type over inference from image names, and validate inferred behavior before performing a restore.
- Acceptance test: Cover plain images, namespaced images, registry ports, digests, custom image aliases and supported database types. Assert selection of the intended backup and restore strategy.
- Review commit: `2a5e56bd1cd9b82cda3ec80ad221c316ff795cb1` (last reviewed 2026-09-10)

## useteploy__teploy-cli-09 - P2 - Open

**The volume backup and restore disagree about where .env belongs**

- Kind: Source-confirmed
- Evidence: BackupVolumes adds the absolute app .env path to a tar created relative to the volumes directory. RestoreVolumes extracts the archive and promotes the whole staging tree into the volumes directory. There is no special restoration of the app-level .env.
- Impact: Tar stores the .env under the stripped absolute-path suffix, so restore places it in a nested directory under volumes rather than beside volumes in the app directory. Configuration/credentials may remain stale while data is restored; the archive layout was reproduced with local GNU tar.
- Proposed fix: Define a versioned backup layout with separate volumes and configuration entries. Restore each to its intended location, preserve restrictive permissions, and make overwriting secrets an explicit recoverable operation.
- Acceptance test: Back up a synthetic app with distinct old/new .env contents, restore into a new temporary layout and assert that configuration and volume files land at their documented locations without nested host-path artifacts.
- Review commit: `2a5e56bd1cd9b82cda3ec80ad221c316ff795cb1` (last reviewed 2026-09-10)

## useteploy__teploy-cli-14 - P2 - Open

**Refuse ambiguous primary-service selection during Compose import**

- Kind: Source-supported defect/contract mismatch
- Evidence: mapCompose ranges over compose.Services (a Go map), selects the first service with any published ports and breaks. The known-accessory classification occurs only afterward for remaining services. A database exposing a port is therefore eligible as the web service.
- Impact: Importing the same multi-port Compose definition can select different primary images and misclassify the intended application. A PostgreSQL/Redis service with published ports can be selected as the web image. No deployment was performed; the diagnostic exercised the selection loop.
- Proposed fix: Make the primary service an explicit import choice when more than one candidate exists; provide a deterministic, documented unambiguous default and exclude known accessories from automatic web selection. Merely sorting map keys is insufficient to choose the right service.
- Acceptance test: Import definitions with both application and database ports in multiple insertion orders. Ambiguity must require a selection or fail clearly; explicit web selection must always produce the same app/accessory plan. A single unambiguous app must continue to import.
- Review commit: `2a5e56bd1cd9b82cda3ec80ad221c316ff795cb1` (last reviewed 2026-09-10)

## useteploy__teploy-cli-15 - P2 - Open

**Preserve exec-form command argument boundaries during Compose import**

- Kind: Source-supported defect/contract mismatch
- Evidence: parseCommand converts a list of command arguments to strings and joins them with an unquoted space. The resulting process command cannot distinguish arguments containing spaces, empty arguments, quotes or shell metacharacters from separate tokens.
- Impact: Valid Compose exec-form worker commands are changed during conversion. In the local example, sh -c with one complete printf script printed hello world as argv but failed after join and shell reparse. This validates representation loss and local shell behavior, not a full remote launcher.
- Proposed fix: Keep an argv representation throughout the process configuration and launcher. Where the output format necessarily requires a shell command, quote each argument with the platform-appropriate escaping, preserving empty arguments and preventing unintended expansion; do not reinterpret explicit string commands unnecessarily.
- Acceptance test: Round-trip command arrays containing a spaced script, an empty argument, quotes and a literal dollar sign. The executed argv/output must match the original exec-form command; string-form commands must retain their established behavior.
- Review commit: `2a5e56bd1cd9b82cda3ec80ad221c316ff795cb1` (last reviewed 2026-09-10)

