# Release receipts (R01)

Every release — and every local verification of one — leaves a receipt:
the recorded inputs and the checksums that prove a clean machine can
recreate the build. This file holds the PATTERN (template below) and
receipts from local verification runs. A receipt from an actual release
is added by the owner at tag time; nothing in this directory cuts a
release, tags, or publishes.

## How to produce a receipt

```
make release-record   # builds the goreleaser matrix, writes
                      # release/expectations/checksums-<version>-<go>.txt
make release-verify   # rebuilds from a clean checkout and diffs against
                      # the recorded expectation (inputs must match)
make release-smoke    # builds the image (Dockerfile, scratch) from the
                      # verified binary and runs version + doctor in it
```

`release-record` prints the receipt block; paste it below under a new
heading. Commit the expectation file with it — the receipt and the
expectation are one artifact split across two files.

## Template

```markdown
### <version label> — <date> — <RELEASE | LOCAL VERIFICATION, NOT A RELEASE>

- commit        : <full sha> (git describe: <describe>)
- go toolchain  : <go version> (go.mod: <go directive>)
- build env     : CGO_ENABLED=0, GOFLAGS unset
- ldflags       : -s -w -X main.version=<version label>
- matrix        : linux/darwin/windows × amd64/arm64 (goreleaser parity)
- checksums     : release/expectations/checksums-<version>-<go>.txt
- verify        : make release-verify → REPRODUCIBLE (this machine, <os>/<arch>)
- image smoke   : make release-smoke → PASS (linux/<arch>, scratch image;
                  version exit 0; doctor emits the 9-check machine envelope,
                  exit 1 = documented failing-diagnosis semantics)
- goreleaser    : <release only: tag, goreleaser version, checksums.txt digest>
```

---

### 0.0.0-localverify-11d0709 — 2026-09-23 — LOCAL VERIFICATION, NOT A RELEASE

First receipt, from the R01 lane's local run (worktree branch
`c09-x02f-r01-cli`, off `d9b652d`). Recorded at source commit `11d0709`
(the R01 tooling commit, clean tree — the expectation file itself lands
in the immediately following commit, which is why `release-verify`
compares via source identity, not strict HEAD equality).

- commit        : 11d0709fbe19b642739809c614d1ff64a0812e41 (git describe: v0.1.37-46-g11d0709)
- go toolchain  : go1.26.6 (go.mod: 1.26.0)
- build env     : CGO_ENABLED=0, GOFLAGS unset
- ldflags       : -s -w -X main.version=0.0.0-localverify-11d0709
- matrix        : linux/darwin/windows × amd64/arm64 (goreleaser parity)
- checksums     : release/expectations/checksums-0.0.0-localverify-11d0709-go1-26-6.txt
  - bf185800ea9b731849cd0a4c61940586bb4fdfc5875636966abe01aad4a34d8e  teploy_darwin_amd64
  - 553044b9f5d81fc99fb7ebd2369f0435a41daff3abbfe3f27e0fc192024653f8  teploy_darwin_arm64
  - bacf79c2f18585676e64ebbc06a409304fdb91fb72b9c386ae0b51b4adbcc259  teploy_linux_amd64
  - 33446a81515d1d7c4026b5de04aa56e7216389f6833ac56d13c35a25a41fa208  teploy_linux_arm64
  - 7d0255f55e8f6e74e9c85acff6d57b634b323dd5ea84caeb2ce86da966362fc9  teploy_windows_amd64
  - 13d24237797c3484a40388fa21af4834c1affa3ba16beb2a139960764c23de56  teploy_windows_arm64
- verify        : `./scripts/release-verify.sh verify 0.0.0-localverify-11d0709`
  → REPRODUCIBLE (all 6 binaries bit-identical to the recording; clean
  tree, Darwin/arm64 host)
- image smoke   : `./scripts/release-verify.sh smoke` → PASS —
  linux/arm64 scratch image from the verified binary; `teploy version`
  exit 0; `teploy doctor --json` exit 1 with the full 9-check machine
  envelope (machine_interface 2), which is the documented
  failing-diagnosis semantics in a bare container, not a crash
- goreleaser    : n/a (no tag, no publish — owner-controlled)

Scope note: checksums cover the raw matrix binaries, the reproducible
unit; goreleaser's published `checksums.txt` covers archives (which
embed mtimes) and is recorded at release time. Bit-identical
reproduction is toolchain-scoped: the expectation file pins the go
version, and `release-verify` refuses to compare (exit 1, INPUTS
DIFFER) rather than report a meaningless mismatch across toolchains.
