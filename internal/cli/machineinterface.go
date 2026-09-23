package cli

import "sort"

// Machine-interface contract, version 2 (X02 S1/S2 — versioned resource
// and operation contracts, _internal/X02_RESOURCE_CONTRACT_ADR_2026-09-22.md
// §2.1-2.2, adopted by DELEGATED_DECISIONS_2026-09-23 D8/D9).
//
// MachineInterface is the version of teploy's machine-readable output
// contract. It rides at the root of every --json envelope a machine
// consumer parses — `version --json`, `app list --json`,
// `server status --json`, and `server list --json` — so a consumer
// (teploy-dash) can fail closed on an interface newer than the one it
// supports BEFORE submitting any mutation, instead of discovering the
// skew after the fact.
//
// Versioning rules (D8):
//   - additive changes (new fields, new capability tokens) do NOT bump;
//   - non-additive changes (field removal, rename, type or semantic
//     change, capability-token removal or redefinition) bump it.
//
// MI 1 is assigned to the envelope shapes as implemented at v0.1.37 plus
// the field itself — assigning it is the compatibility commitment X01
// lacked. MI 1's one recorded exclusion (`server list --json` emitted a
// bare map-of-servers root with no envelope object, so the field could
// not be added) is resolved by MI 2.
//
// MI 2 = MI 1 + the server-list envelope reshape, and NOTHING else:
// `server list --json` now emits {machine_interface, servers[],
// observed_at} with the per-server fields carried over (name, host,
// user, role, tags, vpn_ip, id), where it previously emitted the bare
// map-of-servers root. Removing the bare map from the wire is
// non-additive, and per D8 a non-additive change to ONE command's
// envelope bumps the interface for the whole binary — every envelope
// (version, app list, server status, server list, error) now reports
// machine_interface 2. No capability token was added, removed, or
// redefined; no other envelope shape changed.
const MachineInterface = 2

// Capability tokens advertised by `teploy version --json` (X02 §2.2).
// THIS BLOCK IS THE REGISTRY — the single source of the token set.
// Tokens are stable identifiers consumers code against: ADDING one is
// additive; REMOVING one or changing its meaning bumps
// MachineInterface. Every token must name a contract that has LANDED in
// this binary (TestCapabilityTokenRegistry pins the exact set).
const (
	// env set KEY --stdin reads the value verbatim from stdin; secrets
	// never travel in argv (cb7c0fc, dash UPSTREAM-1).
	CapEnvSetStdin = "env-set-stdin"
	// kv set KEY --stdin, the same stdin contract for KV values.
	CapKvSetStdin = "kv-set-stdin"
	// template deploy/install --var-stdin reads a JSON object that
	// overrides --var (cb7c0fc).
	CapTemplateVarStdin = "template-var-stdin"
	// server rename moves the whole record in one atomic commit,
	// preserving every field (72c57f9, dash UPSTREAM-2).
	CapServerRename = "server-rename"
	// server update changes only the flags passed, atomically (72c57f9).
	CapServerUpdate = "server-update"
	// autodeploy redeploy runs scheduled redeploys through the real
	// deploy engine (fenced lock, fetch, health gate) — no side engine.
	CapAutodeployRedeploy = "autodeploy-redeploy"
	// health.mode http | tcp | auto — explicit readiness probe modes,
	// validated at load, surfaced before the gate (C03).
	CapHealthModes = "health-modes"
	// attempt provenance.json: revision, context fingerprint, and the
	// immutable image digest resolved BEFORE execution (C04).
	CapProvenanceRecords = "provenance-records"
	// attempt readiness.json: durable readiness evidence written exactly
	// when the health gate passes (C01-4).
	CapReadinessReceipts = "readiness-receipts"
	// previews are keyed by <app>-p-<hex8> of sha256(app NUL branch) —
	// branch-distinct identity with legacy adoption/ambiguity contract
	// (C06).
	CapPreviewCanonicalID = "preview-canonical-id"
	// repair-debt.json: durable record-write failure debt, repaired by
	// the next deploy before its own work (C01-6).
	CapRepairDebt = "repair-debt"
	// preview updates are blue/green: candidate readiness-gated before
	// the route switch; predecessor retired only after (C06).
	CapPreviewBlueGreen = "preview-blue-green"
	// structured error envelope on stderr under --json (X02 §2.3).
	CapErrorEnvelope = "error-envelope"
	// `app list --json` emits the MI-1 machine envelope.
	CapAppListMachine = "app-list-machine"
	// `server status --json` emits the MI-1 machine envelope.
	CapServerStatusMachine = "server-status-machine"
	// `teploy doctor [--json] [--server]`: read-only diagnostics with
	// stable check names, ok/warn/fail results, remediations, and 0/1
	// exit semantics — 2 never (that stays drift's) (C09).
	CapDoctorDiagnostics = "doctor-diagnostics"
	// `teploy plan --out` / `teploy apply <file>`: plans carry a binding
	// identity (effective-config digest, target version, build inputs,
	// server/app, state generation) and apply refuses — naming what
	// drifted — when anything moved since the plan (C05). Applied
	// releases carry provenance.plan_id.
	CapPlanApply = "plan-apply"
)

// MachineCapabilities returns every capability token this build
// advertises, sorted; `version --json` emits it verbatim.
func MachineCapabilities() []string {
	tokens := []string{
		CapEnvSetStdin,
		CapKvSetStdin,
		CapTemplateVarStdin,
		CapServerRename,
		CapServerUpdate,
		CapAutodeployRedeploy,
		CapHealthModes,
		CapProvenanceRecords,
		CapReadinessReceipts,
		CapPreviewCanonicalID,
		CapRepairDebt,
		CapPreviewBlueGreen,
		CapErrorEnvelope,
		CapAppListMachine,
		CapServerStatusMachine,
		CapDoctorDiagnostics,
		CapPlanApply,
	}
	sort.Strings(tokens)
	return tokens
}
