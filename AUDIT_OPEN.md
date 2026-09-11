# Open audit items

Unresolved findings for this repository from the ChatGPT-led audit series
(2026-09-09 through 2026-09-11, passes 1-5; register:
teploy-neutron-lullmail expanded audit). Every P0/P1 finding has been fixed
and verified; the P2/P3 tail below is what remains.

Open items: 0

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
