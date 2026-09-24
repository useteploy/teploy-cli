#!/usr/bin/env bash
# R01 release-reproducibility receipts for teploy-cli.
#
# Subcommands:
#   verify   build the exact goreleaser matrix locally, checksum every
#            binary, and diff against the recorded expectation for this
#            (version label, go toolchain). Exit 0 on match; exit 1 on
#            checksum mismatch or recorded-input mismatch (a different
#            go toolchain cannot reproduce the recorded bits — align or
#            re-record); exit 1 with NO RECORDED EXPECTATION when none
#            exists yet (run `record`). Honest results, never vacuous.
#   record   build the same matrix and write the expectation file
#            (release/expectations/) plus a receipt block for
#            release/RELEASE_RECEIPT.md. Recording is a deliberate act —
#            expectations are committed and reviewed like any contract.
#   smoke    build the linux binary for the local docker architecture,
#            build the container image (Dockerfile at repo root), and
#            run `version` + `doctor` inside it as the built-image
#            smoke. Prints SKIP (exit 0) when docker is unavailable.
#
# The matrix mirrors .goreleaser.yml exactly: CGO_ENABLED=0,
# -s -w -X main.version=<label>, linux/darwin/windows x amd64/arm64.
# Checksums cover the RAW BINARIES (goreleaser's checksums.txt covers
# archives; raw binaries are the reproducible unit — archives embed
# mtimes). Pass the version label as the second argument (or VERSION
# env) to verify/record against a specific expectation; the default
# label embeds HEAD's short sha and is what `record` uses for local
# runs.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$REPO_ROOT"

OS_ARCH_MATRIX="linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64"
DIST_DIR="$REPO_ROOT/dist-verify"
EXPECT_DIR="$REPO_ROOT/release/expectations"

VERSION="${2:-${VERSION:-}}"
if [ -z "$VERSION" ]; then
  SHORT_SHA="$(git rev-parse --short HEAD)"
  VERSION="0.0.0-localverify-${SHORT_SHA}"
fi
GO_VERSION="$(go env GOVERSION)"
GO_SLUG="$(echo "$GO_VERSION" | tr '.' '-')"
COMMIT="$(git rev-parse HEAD)"
GIT_DIRTY="$(git status --porcelain | wc -l | tr -d ' ')"
GIT_DESCRIBE="$(git describe --tags 2>/dev/null || echo untagged)"
EXPECT_FILE="$EXPECT_DIR/checksums-${VERSION}-${GO_SLUG}.txt"

record_inputs() {
  cat <<EOF
# teploy release-verify receipt
# version label : $VERSION
# commit        : $COMMIT
# git describe  : $GIT_DESCRIBE
# dirty files   : $GIT_DIRTY
# go toolchain  : $GO_VERSION (go.mod: $(awk '/^go /{print $2}' go.mod))
# build env     : CGO_ENABLED=0 GOFLAGS=
# ldflags       : -s -w -X main.version=$VERSION
# recorded      : $(date -u +%Y-%m-%dT%H:%M:%SZ) on $(uname -s)/$(uname -m)
EOF
}

build_matrix() {
  rm -rf "$DIST_DIR"
  mkdir -p "$DIST_DIR"
  for os_arch in $OS_ARCH_MATRIX; do
    GOOS="${os_arch%%/*}"; GOARCH="${os_arch##*/}"
    echo "building teploy_${GOOS}_${GOARCH}"
    CGO_ENABLED=0 GOOS="$GOOS" GOARCH="$GOARCH" \
      go build -trimpath -ldflags "-s -w -X main.version=$VERSION" \
      -o "$DIST_DIR/teploy_${GOOS}_${GOARCH}" ./cmd/teploy
  done
}

checksum_matrix() {
  # Basenames only — the receipt must diff identically on any machine
  # that checks out the repo at a different path.
  (cd "$DIST_DIR" && for f in teploy_*; do
    shasum -a 256 "$f"
  done)
}

case "${1:-verify}" in
verify)
  [ -d "$EXPECT_DIR" ] || { echo "NO RECORDED EXPECTATION (release/expectations/ absent) — run: make release-record"; exit 1; }
  # Re-derive the expectation file for the CURRENT version label unless
  # exactly one exists; VERSION was defaulted from HEAD, so a recorded
  # expectation for this commit resolves directly.
  if [ ! -f "$EXPECT_FILE" ]; then
    echo "NO RECORDED EXPECTATION for (version $VERSION, $GO_VERSION) — run: make release-record"
    ls "$EXPECT_DIR" 2>/dev/null | sed 's/^/  existing: /'
    exit 1
  fi
  build_matrix
  checksum_matrix >"$DIST_DIR/checksums.txt"
  # Inputs must match what was recorded, else the diff is meaningless.
  RECORDED_GO="$(sed -n 's/^# go toolchain  : \([a-z0-9.]*\).*/\1/p' "$EXPECT_FILE" | head -1)"
  RECORDED_COMMIT="$(sed -n 's/^# commit        : //p' "$EXPECT_FILE" | head -1)"
  if [ "$RECORDED_GO" != "$GO_VERSION" ]; then
    echo "INPUTS DIFFER: expectation recorded with $RECORDED_GO, building with $GO_VERSION."
    echo "Bit-identical reproduction is toolchain-scoped — align toolchains or re-record."
    exit 1
  fi
  if [ "$RECORDED_COMMIT" != "$COMMIT" ]; then
    # The expectation file itself lands in a commit AFTER the source it
    # records, so strict HEAD equality would make every recorded
    # expectation unverifiable. The honest test is source identity: if
    # nothing that feeds the compiler changed since the recorded
    # commit, the comparison is still meaningful.
    CHANGED="$(git diff --name-only "$RECORDED_COMMIT" "$COMMIT" -- cmd internal go.mod go.sum)"
    if [ -n "$CHANGED" ]; then
      echo "INPUTS DIFFER: build inputs changed since the recorded commit $RECORDED_COMMIT:"
      echo "$CHANGED"
      echo "Re-record at HEAD, or verify from the recorded commit (git checkout $RECORDED_COMMIT -- detached)."
      exit 1
    fi
    echo "note: HEAD is a source-identical descendant of the recorded commit (no cmd/ internal/ go.mod go.sum changes) — comparing"
  fi
  # Compare the checksum LINES (the receipt header records inputs, and
  # legitimately differs — e.g. dirty-file count — between record and
  # verify runs at the same commit).
  grep -v '^#' "$EXPECT_FILE" >"$DIST_DIR/expected-checksums.txt"
  if diff -u "$DIST_DIR/expected-checksums.txt" "$DIST_DIR/checksums.txt" >/dev/null; then
    echo "REPRODUCIBLE: all 6 matrix binaries match the recorded checksums for $VERSION ($GO_VERSION)"
    sed -n '2,8p' "$EXPECT_FILE" | sed 's/^# //'
  else
    echo "CHECKSUM MISMATCH against $EXPECT_FILE:"
    diff -u "$DIST_DIR/expected-checksums.txt" "$DIST_DIR/checksums.txt" || true
    exit 1
  fi
  ;;

record)
  mkdir -p "$EXPECT_DIR"
  build_matrix
  { record_inputs; checksum_matrix; } >"$EXPECT_FILE"
  echo "recorded $EXPECT_FILE"
  echo "receipt block for release/RELEASE_RECEIPT.md follows;"
  record_inputs | tail -n +2
  ;;

smoke)
  command -v docker >/dev/null 2>&1 || { echo "SKIP: docker not found on PATH"; exit 0; }
  docker version --format '{{.Server.Version}}' >/dev/null 2>&1 || { echo "SKIP: docker daemon unreachable"; exit 0; }
  DOCKER_ARCH="$(docker version --format '{{.Server.Arch}}')"
  case "$DOCKER_ARCH" in
    amd64|arm64) ;;
    *) echo "SKIP: unsupported docker architecture $DOCKER_ARCH"; exit 0 ;;
  esac
  rm -rf "$DIST_DIR"; mkdir -p "$DIST_DIR"
  echo "building linux/$DOCKER_ARCH smoke binary ($VERSION)"
  CGO_ENABLED=0 GOOS=linux GOARCH="$DOCKER_ARCH" \
    go build -trimpath -ldflags "-s -w -X main.version=$VERSION" \
    -o "$DIST_DIR/teploy_linux_$DOCKER_ARCH" ./cmd/teploy
  IMAGE="teploy:release-smoke-$VERSION"
  docker build -q -f "$REPO_ROOT/Dockerfile" --build-arg BINARY="dist-verify/teploy_linux_$DOCKER_ARCH" -t "$IMAGE" "$REPO_ROOT" >/dev/null
  echo "image built: $IMAGE (linux/$DOCKER_ARCH, scratch)"
  set +e
  VERSION_OUT="$(docker run --rm "$IMAGE" version 2>&1)"; V_RC=$?
  DOCTOR_OUT="$(docker run --rm "$IMAGE" doctor --json 2>&1)"; D_RC=$?
  set -e
  echo "smoke: teploy version  -> exit $V_RC: $VERSION_OUT"
  echo "smoke: teploy doctor --json -> exit $D_RC"
  [ $V_RC -eq 0 ] || { echo "FAIL: version smoke exited $V_RC"; docker rmi "$IMAGE" >/dev/null; exit 1; }
  echo "$DOCTOR_OUT" | python3 -c '
import json,sys
r=json.load(sys.stdin)
assert r["machine_interface"]>=1, "no machine_interface in doctor envelope"
names=[c["name"] for c in r["checks"]]
print("  doctor envelope ok: machine_interface", r["machine_interface"], "-", len(names), "checks:", ", ".join(names))
' || { echo "FAIL: doctor did not emit a valid machine envelope"; echo "$DOCTOR_OUT"; docker rmi "$IMAGE" >/dev/null; exit 1; }
  # doctor exit 1 = failing diagnosis (expected in a bare container);
  # a crash or missing envelope is the failure the smoke guards against.
  if [ $D_RC -ne 0 ] && [ $D_RC -ne 1 ]; then
    echo "FAIL: doctor smoke exited $D_RC (0/1 are the documented codes)"
    docker rmi "$IMAGE" >/dev/null; exit 1
  fi
  docker rmi "$IMAGE" >/dev/null
  echo "SMOKE PASS: version exits 0; doctor reports a valid machine envelope with documented exit semantics"
  ;;

*)
  echo "usage: release-verify.sh [verify|record|smoke] [version-label]" >&2
  echo "  verify [label]  rebuild + diff against the recorded expectation" >&2
  echo "  record [label]  build + write the expectation + print the receipt block" >&2
  echo "  smoke [label]   built-image smoke (version + doctor in scratch)" >&2
  exit 1
  ;;
esac
