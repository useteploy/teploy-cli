# teploy-cli — known bugs

Append-only log of confirmed defects in this repo that are **not fixed yet**.
Same rules as `_internal/UPSTREAM_BUGS.md`: never rewrite an entry — a later
finding is appended to it as a dated `**Update YYYY-MM-DD:**` line. Delete an
entry only by moving it to a Closed section with the fixing sha.

Fix ownership lives in `AGENTS.md`: defects reported by teploy-dash or
teploy-ship that trace into this binary are ours, and a workaround landed up
there without an entry here is how the same bug gets paid for twice. When a
`BUGS.md` entry is fixed, say so in the fixing commit message so the two can be
found from each other.

```
## YYYY-MM-DD — short title
- **Area:** cli | config | caddy | dns | deploy | other
- **Symptom:** what was observed
- **Suspected component:** file/function, with line references as of the report date
- **Reproduction:** minimal steps or case
- **Workaround (if any):** what callers can do until it is fixed
- **Reporter:** agent/session identifier
- **Status:** open | fixed (<sha>)
```

---

<!-- Append new entries below this line. Newest first. -->

_No open entries._

---

## Closed

## 2026-09-08 — comma-separated domains fail DNS validation as one literal hostname

- **Area:** cli (validate, deploy) + dns
- **Symptom:** any `teploy.yml` with a multi-host `domain:` list
  (e.g. `domain: "dreamlucidgroup.com, www.dreamlucidgroup.com"`) gets
  `DNS: could not resolve domain a.com, www.a.com: no such host` — the whole
  comma-joined string is looked up as if it were one hostname. In `validate`
  this is a warning; in `deploy` it **aborts the deploy**, even when every
  listed host resolves correctly.
- **Suspected component:** two call sites pass `appCfg.Domain` to
  `dns.Validate` without splitting it first:
  - `internal/cli/validate.go:107` — `dns.Validate(appCfg.Domain, host, nil)`
  - `internal/cli/deploy.go:441` — same call, fatal on error
  (`dns.Validate` itself is fine — it just does `net.LookupHost` on what it is
  given.) The rest of the CLI already splits correctly:
  `nonPublicDomainWarnings` (`validate.go:146`) splits on `,`, and
  `splitDomains` (`remove.go:120`) is the trimmed per-host parser.
- **Reproduction:** `domain: "example.com, www.example.com"` where both hosts
  resolve; run `teploy validate` (false-positive DNS warning) or
  `teploy deploy` (aborts). Observed in the wild deploying `hardware-site`
  (dreamlucidgroup.com + www) to deploy-ovh on 2026-09-08; both hosts
  verified via `dig` to point at the server before bypassing.
- **Workaround:** `--skip-dns-check` after verifying each host's DNS manually.
  No code workaround; both hosts must otherwise be dropped to single-host form.
- **Fix sketch:** route both call sites through `splitDomains` (hoist it out of
  `remove.go` if the package layout prefers) and validate each host
  individually, reporting the failing host by name. Add a unit test with a
  two-host domain string next to the existing `nonPublicDomainWarnings` and
  `splitDomains` tests.
- **Reporter:** hardware-site session, 2026-09-08.
- **Status:** fixed (65f67df, released in v0.1.51)
- **Update 2026-11-14:** fixed in `65f67df` ("fix(dns): check each host, …"):
  `validate`, `deploy` and the folderless deploy path all call
  `validateDomainDNS` (`internal/cli/dnscheck.go`), which splits the domain on
  `,`, trims each host and validates it alone; `dns.Validate` has no other
  caller. Moved to Closed; the entry was never moved when the fix landed.
