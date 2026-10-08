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
  - 9b41470e71554bb6a2899a5832ae20b67af06ec27aa6b3d31a99fa07a3706329  teploy_darwin_amd64
  - fa51620f2abbbdfa8d9db71c18470c327f1ce7be8065cac7fb5045c01c5eef22  teploy_darwin_arm64
  - 2e37ab081802d45638dd5a4d0e50f685c9ef27dd438eafcb078653539b2dcd37  teploy_linux_amd64
  - 1e22ca85320fdef40039a430248ac5b46366ba41904d33e449e34742f0896c73  teploy_linux_arm64
  - d0b2447f61520c97f4be25e9ef57cc9bc7ab8db253a3b5672a20413708e3cc5a  teploy_windows_amd64
  - 7108ded33e71efc125601d7bbf93bb1beb57d51e360dbe2b763bdc8314f89bd5  teploy_windows_arm64
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

2026-10-07 reconciliation: the human checksum transcription above was corrected to the expectation file after rebuilding all six binaries at exact source commit `11d0709fbe19b642739809c614d1ff64a0812e41` with Go 1.26.6 and verifying byte equality. Future recordings explicitly clear GOFLAGS, disable VCS build-info variation, and bind the actual compiler-source digest (including dirty source). Historical expectations retain their original recipe.
