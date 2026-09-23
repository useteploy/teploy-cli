#!/bin/sh
# teploy target-side critical-section helper (C01-1 mechanism).
#
# Invoked over SSH by the CLI (internal/targetguard). Holds an OS-exclusive
# flock for the duration of one protected effect — the property the mkdir
# lock lacks: process death releases it, so a killed helper can never leave
# a frozen app. Under the lock it fences on the committed generation: a
# client whose plan was prepared against an older generation is refused
# before its effect runs, so an abandoned owner's stale rollback can never
# stop a newer generation.
#
# argv: <app> <expected_generation> -- <command...>
#
# PROTOCOL: the helper always exits 0 and reports its outcome as the FIRST
# line of stdout (the ssh executor abstraction does not preserve exit
# codes, so the outcome rides the output stream):
#   GUARD_OK                    effect committed
#   GUARD_BUSY                  lock held and not acquired (retryable)
#   GUARD_FENCED <c> <e>        committed generation c > expected e (stale plan)
#   GUARD_UNFIT                 no flock(1) — fails closed, NEVER lock-free
#   GUARD_BADGEN                generation sidecar unreadable
#   GUARD_EFFECT_FAILED <n>     the effect exited n
# The effect's own combined output follows on subsequent lines.
#
# LOCK SHAPE: `flock FILE -c BODY` — the util-linux `flock FD` form (lock
# an already-open fd, hold it past the flock process's exit) is NOT
# portable: busybox releases on child exit, which testing on alpine
# proved as silent non-serialization (ABAB interleaving). The FILE form
# runs the whole verify+effect body as flock's child, which both
# implementations hold for the body's lifetime.
#
# The committed generation lives in the .generation sidecar (a plain
# integer, written atomically by the state commit; absent = 0).

set -u

APP="$1"
EXPECTED="$2"
shift 2
if [ "${1:-}" = "--" ]; then shift; fi

ROOT="${TEPLOY_DEPLOYMENTS_ROOT:-/deployments}"
APPDIR="$ROOT/$APP"
LOCKFILE="$APPDIR/.lock/guard"
mkdir -p "$APPDIR/.lock"

command -v flock >/dev/null 2>&1 || {
    echo "GUARD_UNFIT"
    exit 0
}

# Lock-primitive self-test (once per app dir, marker under the lock): a
# target whose flock does not actually SERIALIZE must fail closed, never
# run effects under a fictitious lock. Observed in the wild: busybox
# flock's FILE-cmd form acquires instantly against a held lock in some
# environments (alpine under podman, 2026-09-23) while its FD form blocks
# correctly — util-linux is sound. The self-test pins the property we
# depend on instead of the tool's name.
if [ ! -f "$LOCKFILE.selftest-ok" ]; then
    flock "$LOCKFILE" -c "sleep 1" &
    HOLDER=$!
    sleep 0.2
    if flock -n "$LOCKFILE" -c "true" 2>/dev/null; then
        kill "$HOLDER" 2>/dev/null
        wait "$HOLDER" 2>/dev/null
        echo "GUARD_UNFIT"
        exit 0
    fi
    wait "$HOLDER"
    touch "$LOCKFILE.selftest-ok"
fi

GUARD_APPDIR="$APPDIR" GUARD_EXPECTED="$EXPECTED" GUARD_EFFECT="$*" \
flock "$LOCKFILE" -c '
    APPDIR="$GUARD_APPDIR"; EXPECTED="$GUARD_EXPECTED"
    if [ -f "$APPDIR/.generation" ]; then
        COMMITTED=$(cat "$APPDIR/.generation" 2>/dev/null)
        case "$COMMITTED" in
            ""|*[!0-9]*)
                echo "GUARD_BADGEN"
                exit 0
                ;;
        esac
        if [ "$COMMITTED" -gt "$EXPECTED" ]; then
            echo "GUARD_FENCED $COMMITTED $EXPECTED"
            exit 0
        fi
    fi
    OUT=$(mktemp) || { echo "GUARD_BUSY"; exit 0; }
    if sh -c "$GUARD_EFFECT" >"$OUT" 2>&1; then
        echo "GUARD_OK"
    else
        echo "GUARD_EFFECT_FAILED $?"
    fi
    cat "$OUT"
    rm -f "$OUT"
' || echo "GUARD_BUSY"
