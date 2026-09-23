#!/usr/bin/env bash
# X02 S7 acceptance sweep — teploy-cli (ADR §6 S7).
# One executable harness running this repo's legs of the programme's
# acceptance line (:89): rename, duplicate identity, repeated request,
# response loss, rollback — target/history identity preserved end to end.
# Any leg failing (or matching no tests — vacuous pass is a broken pin)
# fails the sweep. Receipt: paste the output into AUDIT_OPEN when the
# contract changes.
set -u
cd "$(dirname "$0")/.."
fail=0
log=$(mktemp)

leg() {
  label="$1"; pkg="$2"; pattern="$3"
  printf '== %-24s %-28s ' "$label" "$pkg"
  listed=$(go test -list "$pattern" "$pkg" 2>/dev/null | grep -c '^Test')
  if [ "${listed:-0}" -eq 0 ]; then
    echo "FAIL (no tests matched: $pattern)"
    fail=1
    return
  fi
  if go test -count=1 "$pkg" -run "$pattern" >"$log" 2>&1; then
    echo "PASS ($listed test(s))"
  else
    echo "FAIL ($listed test(s), log: $log)"
    tail -5 "$log"
    fail=1
  fi
}

# rename: server identity survives rename/update/re-add; legacy stays id-less.
leg rename            ./internal/config     'TestRenameServer_PreservesID|TestUpdateServer_PreservesID|TestAddServer_ReAddPreservesID|TestAddServer_LegacyEntryStaysIDLess|TestAddServer_MintsIDForNewEntry'

# duplicate identity: preview IDs are repo+branch-keyed (distinct under
# collision); release records refuse foreign identity.
leg duplicate-identity ./internal/preview    'TestPreviewIDGolden|TestPreviewBranchIdentityIsDistinct'
leg release-identity   ./internal/releasemeta 'TestRead_AbsentVsPresentVsUnreadable|TestWrite_RoundTripAndPathValidation'

# repeated request: machine-interface goldens pin the envelope contract the
# idempotent paths emit (version handshake, release-record, attempt names).
leg repeated-request   ./internal/cli        'TestContractsVersionHandshakeGolden|TestContractsReleaseRecordGolden|TestContractsAttemptNameGolden'

# response loss: uncertain outcome resolves by evidence (Compensate vs
# Inspect), attribution is exact, crash recovery compensates recorded IDs.
leg response-loss      ./internal/deploy     'TestReadinessReceipt_DecideDistinguishesCompensateFromInspect|TestCandidateAttribution|TestPredecessorSnapshot'

# rollback: a failed state commit restores the old workload/route;
# rollbacks restore from the recorded spec and displace correctly.
leg rollback           ./internal/deploy     'TestDeploy_HostIngressStateCommitFailureRestoresOldWorkload|TestDeploy_StateCommitFailureRestoresRouteWithoutStoppingOldWorkload|TestRollback_RestoresFromRecordedSpec|TestRollback_FixedPortTargetDisplacesCurrent'

exit $fail
