package cli

// X02 S2 contracts corpus generator (contracts/ skeleton + first goldens).
// Regenerates contracts/fixtures/ from the REAL encoders and fails on any
// diff against the committed corpus - the TestPreviewIDGolden discipline
// generalized to the machine interface. Set TEPLOY_UPDATE_CONTRACTS=1 to
// rewrite the corpus after a deliberate contract change, then commit the
// diff together with the code that caused it and bump MANIFEST.md.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy/internal/backup"
	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/confighome"
	"github.com/useteploy/teploy/internal/events"
	"github.com/useteploy/teploy/internal/preview"
	"github.com/useteploy/teploy/internal/releasemeta"
	"github.com/useteploy/teploy/internal/ssh"
)

const contractsDir = "../../contracts"

func writeFixture(t *testing.T, path string, v any) {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		t.Fatalf("encode %s: %v", path, err)
	}
	full := filepath.Join(contractsDir, "fixtures", path)
	if os.Getenv("TEPLOY_UPDATE_CONTRACTS") == "1" {
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", full, err)
		}
		if err := os.WriteFile(full, buf.Bytes(), 0o644); err != nil {
			t.Fatalf("write %s: %v", full, err)
		}
		return
	}
	want, err := os.ReadFile(full)
	if err != nil {
		t.Fatalf("fixture %s missing (run with TEPLOY_UPDATE_CONTRACTS=1 to seed): %v", path, err)
	}
	if !bytes.Equal(bytes.TrimRight(want, "\n"), bytes.TrimRight(buf.Bytes(), "\n")) {
		t.Errorf("fixture %s drifted from the committed corpus - regenerate deliberately (TEPLOY_UPDATE_CONTRACTS=1), commit the diff WITH the code change, and bump contracts/MANIFEST.md", path)
	}
}

// TestContractsVersionHandshakeGolden drives the REAL writeVersion encoder.
func TestContractsVersionHandshakeGolden(t *testing.T) {
	var buf bytes.Buffer
	if err := writeVersion(&buf, "v0.0.0-contracts", true); err != nil {
		t.Fatalf("writeVersion: %v", err)
	}
	var v any
	if err := json.Unmarshal(buf.Bytes(), &v); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	writeFixture(t, "version-handshake/valid/mi2.json", v)
}

// TestContractsServerListEnvelopeGolden drives the REAL writeServerList
// encoder (the same path `server list --json` runs) for the valid fixture,
// and the pre-reshape bare-map encoder for the legacy class. MI 2 minted
// by this reshape: the bare map-of-servers root is gone on the wire.
func TestContractsServerListEnvelopeGolden(t *testing.T) {
	servers := map[string]config.Server{
		"prod": {
			ID:    "srv-0123456789abcdef",
			Host:  "192.0.2.10",
			User:  "deploy",
			Role:  "app",
			Tags:  map[string]string{"region": "us-east"},
			VpnIP: "100.64.0.7",
		},
		"staging": {Host: "192.0.2.20"}, // id-less legacy entry inside the envelope
	}
	var buf bytes.Buffer
	if err := writeServerList(&buf, servers, true, time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("writeServerList: %v", err)
	}
	var v any
	if err := json.Unmarshal(buf.Bytes(), &v); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	writeFixture(t, "server-list-envelope/valid/mi2.json", v)

	// Legacy: the bare map-of-servers root a pre-MI-2 CLI emitted — the
	// S1 exclusion, now a first-class legacy class. Same map encoder that
	// era used (json tags unchanged on config.Server).
	writeFixture(t, "server-list-envelope/legacy/bare-map.json", servers)
}

// TestContractsAppListEnvelopeGolden emits an appListDTO with one
// representative app through the same json tags the command marshals.
// Offline stand-in: the DTO values are constructed, the ENCODER is real.
func TestContractsAppListEnvelopeGolden(t *testing.T) {
	ts := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	writeFixture(t, "app-list-envelope/valid/mi2.json", appListDTO{
		MachineInterface: MachineInterface,
		Host:             "srv.example.com",
		Errors:           []machineError{},
		Apps: []appStatusDTO{{
			App: "myapp", Domain: "myapp.example.com", Type: "container",
			Ingress: "caddy", CurrentRelease: releaseStatusDTO{Version: "3", Ports: []int{3000}},
			PreviousRelease: releaseStatusDTO{Version: "2", Ports: []int{3000}},
			Containers:      []containerDTO{{ID: "9f31c02", Name: "myapp-web-3", Image: "nginx:1.27", State: "running", Status: "Up 4 minutes", CreatedAt: "2026-09-23T11:55:00Z", Process: "web", Version: "3"}},
			Processes:       []processDTO{},
			Lock:            nil, ObservedAt: ts, Errors: []machineError{},
		}},
		ObservedAt: ts,
	})

	// Legacy: pre-MI envelope (no machine_interface field) - the shape a
	// v0.1.36-or-older CLI emitted. Consumers must treat missing-MI as
	// legacy, not as MI 0.
	var legacy map[string]any
	raw, err := json.Marshal(appListDTO{
		Host: "srv.example.com", Apps: []appStatusDTO{}, ObservedAt: ts, Errors: []machineError{},
	})
	if err != nil {
		t.Fatalf("marshal legacy: %v", err)
	}
	if err := json.Unmarshal(raw, &legacy); err != nil {
		t.Fatalf("unmarshal legacy: %v", err)
	}
	delete(legacy, "machine_interface")
	writeFixture(t, "app-list-envelope/legacy/pre-mi.json", legacy)
}

// TestContractsServerStatusEnvelopeGolden drives the REAL
// collectServerStatus encoder (the same collection path `server status
// --json` runs) through a mock SSH executor — the DTO values are
// synthetic, the encoder and every parse stage (memory, disks, docker
// inventory, Caddy routes) are the real ones. The wire shape was
// verified against a live `server status --json` capture before this
// fixture was pinned (see MANIFEST rev 5). Two valid classes: a full
// healthy observation, and a partial one with the Caddy probe failing —
// the class a real deployment without a caddy container produces. The
// legacy fixture is the pre-MI shape (machine_interface absent), which a
// 42243e2-era CLI emitted.
func TestContractsServerStatusEnvelopeGolden(t *testing.T) {
	observedAt := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	container := `{"ID":"9f31c02","Names":"myapp-web-3","Image":"example/myapp:3","State":"running","Status":"Up 4 minutes","CreatedAt":"2026-09-23 11:55:00 +0000 UTC","Labels":"teploy.app=myapp,teploy.process=web,teploy.version=3"}`
	image := `{"ID":"sha256:1a2b3c4d5e6f","Repository":"example/myapp","Tag":"3","Size":"25MB","CreatedAt":"2026-09-23 11:50:00 +0000 UTC"}`
	caddy := `{"servers":{"srv0":{"routes":[{"@id":"myapp","match":[{"host":["myapp.example.com"]}],"handle":[{"handler":"subroute","routes":[{"handle":[{"handler":"reverse_proxy","upstreams":[{"dial":"myapp-web-3:3000"}]}]}]}]}]}}}`
	full := ssh.NewMockExecutor("192.0.2.10",
		ssh.MockCommand{Match: "cat /proc/uptime", Output: "3600.50 1200.00"},
		ssh.MockCommand{Match: "cat /proc/loadavg", Output: "0.10 0.20 0.30 1/100 1"},
		ssh.MockCommand{Match: "cat /proc/meminfo", Output: "MemTotal: 1000 kB\nMemAvailable: 400 kB\n"},
		ssh.MockCommand{Match: "df -B1 -P", Output: "Filesystem 1-blocks Used Available Capacity Mounted on\n/dev/vda1 1000 250 750 25% /\n/dev/vdb1 2000 500 1500 26% /srv\n"},
		ssh.MockCommand{Match: "docker version", Output: "29.0.0"},
		ssh.MockCommand{Match: "docker ps --all", Output: container},
		ssh.MockCommand{Match: "docker image ls", Output: image},
		ssh.MockCommand{Match: "docker exec caddy", Output: caddy},
	)
	got := collectServerStatus(context.Background(), full, "prod", observedAt)
	if len(got.Errors) != 0 {
		t.Fatalf("full observation reported errors: %#v", got.Errors)
	}
	fullStatus := got
	writeFixture(t, "server-status-envelope/valid/full.json", got)

	partial := ssh.NewMockExecutor("192.0.2.20",
		ssh.MockCommand{Match: "cat /proc/uptime", Output: "86400.00 86400.00"},
		ssh.MockCommand{Match: "cat /proc/loadavg", Output: "0.00 0.01 0.05 1/100 1"},
		ssh.MockCommand{Match: "cat /proc/meminfo", Output: "MemTotal: 500 kB\nMemAvailable: 250 kB\n"},
		ssh.MockCommand{Match: "df -B1 -P", Output: "Filesystem 1-blocks Used Available Capacity Mounted on\n/dev/vda1 500 100 400 20% /\n"},
		ssh.MockCommand{Match: "docker version", Output: "29.0.0"},
		ssh.MockCommand{Match: "docker ps --all", Output: ""},
		ssh.MockCommand{Match: "docker image ls", Output: ""},
		ssh.MockCommand{Match: "docker exec caddy", Err: errors.New("Error response from daemon: No such container: caddy")},
	)
	got = collectServerStatus(context.Background(), partial, "staging", observedAt)
	if len(got.Errors) != 1 || got.Errors[0].Scope != "caddy.routes" {
		t.Fatalf("partial observation missing its caddy error: %#v", got.Errors)
	}
	writeFixture(t, "server-status-envelope/valid/partial-caddy-unavailable.json", got)

	// Legacy: the pre-MI envelope (no machine_interface field) a CLI
	// between 42243e2 and dda4911 emitted. Same delete-from-map approach
	// as the app-list legacy class.
	raw, err := json.Marshal(fullStatus)
	if err != nil {
		t.Fatalf("marshal full status: %v", err)
	}
	var legacy map[string]any
	if err := json.Unmarshal(raw, &legacy); err != nil {
		t.Fatalf("unmarshal legacy: %v", err)
	}
	delete(legacy, "machine_interface")
	writeFixture(t, "server-status-envelope/legacy/pre-mi.json", legacy)
}

// TestContractsErrorEnvelopeGolden pins the two wired error classes.
func TestContractsErrorEnvelopeGolden(t *testing.T) {
	writeFixture(t, "error-envelope/valid/config-invalid.json", machineErrorEnvelope{
		MachineInterface: MachineInterface, Code: "config-invalid",
		Message: "invalid teploy configuration", Detail: "teploy.yml: services.0.name: required",
	})
	writeFixture(t, "error-envelope/valid/internal.json", machineErrorEnvelope{
		MachineInterface: MachineInterface, Code: "internal",
		Message: "command failed", Detail: "dial tcp: connection refused",
	})
	// Invalid: a code outside the taxonomy (schema enum must reject).
	writeFixture(t, "error-envelope/invalid/unknown-code.json", machineErrorEnvelope{
		MachineInterface: MachineInterface, Code: "kaboom", Message: "x",
	})
}

// TestContractsReleaseRecordGolden pins the releasemeta Record shape.
func TestContractsReleaseRecordGolden(t *testing.T) {
	ts := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	writeFixture(t, "release-record/valid/container.json", releasemeta.Record{
		SchemaVersion: 1, App: "myapp", Hash: "abc1234.deadb17ecafef00d", CreatedAt: ts,
		DeploymentType: "container", IngressMode: "caddy", Domain: "myapp.example.com",
		ImageRef: "nginx:1.27", ImageDigest: "sha256:0000", ManifestSHA256: "sha256:beef",
	})
}

// TestContractsAttemptNameGolden pins the attempt-name grammar class via
// representative strings (the schema pattern is the contract; these are the
// examples a consumer tests against).
func TestContractsAttemptNameGolden(t *testing.T) {
	writeFixture(t, "attempt-name/valid/examples.json", []string{
		"abc1234.deadb17ecafef00d",
		"9f31c02.0123456789abcdef",
		"ABC1234.deadb17ecafef00d",
		"v1.2.3.deadb17ecafef00d",
		"release_1.deadb17ecafef00d",
	})
	writeFixture(t, "attempt-name/invalid/examples.json", []string{
		"deadb17ecafef00d",         // missing the hash half
		"abc1234.DeadB17eCafef00d", // uppercase hex half
		"abc1234.deadb17ecafef00",  // 15 hex chars
		"../escape.attempt0000000", // path characters
	})
}

// TestContractsPlanRecordGolden pins the C05 plan-record shape through
// the REAL record construction: plan id computed by computePlanID,
// marshaled with the PlanRecord's own tags (the same encoder savePlanFile
// uses). Two representative records: a build plan (image
// unresolved-awaiting-build, bound by build inputs) and a prebuilt
// digest-pinned plan (resolved-by-digest) — the known-vs-unresolved
// classification a consumer reads before trusting an apply.
func TestContractsPlanRecordGolden(t *testing.T) {
	buildPlan := &PlanRecord{
		SchemaVersion: PlanRecordSchemaVersion,
		App:           "myapp",
		Server:        "srv.example.com",
		User:          "root",
		ServerName:    "prod",
		TargetVersion: "abc1234",
		VersionKnown:  true,
		ConfigDigest:  "3f2a9c11d8e4b7065a1c9f0e2b8d7a64c5e3f1b9a0d8c7e6f5a4b3c2d1e0f9a8",
		Image: PlanImageIdentity{
			NeedsBuild:         true,
			Resolution:         imageUnresolvedAwaitingBuild,
			ContextPath:        ".",
			ContextFingerprint: "c0ffee11aa22bb33",
			Dockerfile:         "Dockerfile",
			DockerfileSHA256:   "deadbeef11",
			Platform:           "linux/amd64",
		},
		TargetState: PlanTargetState{Deployed: true, Generation: 4, CurrentHash: "old1234", ManifestSHA256: "aa11bb22"},
		Effects: PlanEffects{
			Containers: []planChange{{Action: "create", Name: "myapp-web-abc1234", Detail: "web container"}},
			Routing:    []planEffect{{Action: "change", Name: "domain", From: "old.example.com", To: "new.example.com", Detail: "routes served by this deployment"}},
		},
		Unresolved: []string{"image unresolved — built at deploy time; the plan binds the build inputs (context . fingerprint c0ffee11aa22bb..., Dockerfile Dockerfile sha deadbeef11...)"},
	}
	buildPlan.PlanID = computePlanID(buildPlan)
	writeFixture(t, "plan-record/valid/build.json", buildPlan)

	digest := "sha256:" + strings.Repeat("ab", 32)
	digestPlan := &PlanRecord{
		SchemaVersion: PlanRecordSchemaVersion,
		App:           "myapp",
		Server:        "srv.example.com",
		ServerName:    "prod",
		TargetVersion: "sha256-aaaaaaaaaaaa",
		VersionKnown:  true,
		ConfigDigest:  "7d1e0f9a8b7c6d5e4f3a2b1c0d9e8f7a6b5c4d3e2f1a0b9c8d7e6f5a4b3c2d10",
		Image: PlanImageIdentity{
			Ref:        "registry.example.com/myapp@" + digest,
			Resolution: imageResolvedByDigest,
			Digest:     digest,
		},
		TargetState: PlanTargetState{Deployed: false},
		Effects:     PlanEffects{Containers: []planChange{{Action: "create", Name: "myapp-web-sha256-aaaaaaaaaaaa", Detail: "web container"}}},
	}
	digestPlan.PlanID = computePlanID(digestPlan)
	writeFixture(t, "plan-record/valid/prebuilt-digest.json", digestPlan)

	// Invalid: a record whose plan id does not recompute from its
	// identity — loadPlanFile refuses it (the schema's pattern cannot
	// see inside the hash, so this fixture pins the REFUSAL, not just
	// the shape).
	tampered := *digestPlan
	tampered.ConfigDigest = "0000000000000000000000000000000000000000000000000000000000000000"
	writeFixture(t, "plan-record/invalid/tampered-id.json", tampered)
}

// TestContractsPreviewStateListRowGolden drives the REAL `preview list
// --json` row encoder (previewListRows over preview.State) for a default
// and a tailnet-mode canonical preview (corpus rev 6). The row is wrapped
// with the artifact's era classification keys (era, app) — the wire row
// itself carries neither. The hand-authored identity fixtures (canonical,
// legacy, ambiguous) are unchanged.
func TestContractsPreviewStateListRowGolden(t *testing.T) {
	created := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	base := preview.State{
		ID:        preview.PreviewID("myapp", "feature/login"),
		Branch:    "feature/login",
		Repo:      "github.com/example/myapp",
		Route:     "myapp-preview-p-08e81639",
		Port:      49200,
		Container: "myapp-preview-p-08e81639-abc1234",
		Image:     "myapp-build-abc1234",
		CreatedAt: created,
		ExpiresAt: created.Add(72 * time.Hour),
	}
	def := base
	def.Domain = "preview-feature-login-08e81639.myapp.com"
	tailnet := base
	tailnet.Domain = "preview-feature-login-08e81639.100-64-1-2.sslip.io"
	tailnet.BaseDomain = "100-64-1-2.sslip.io"
	tailnet.HTTPOnly = true
	tailnet.AllowIPs = []string{"100.64.0.0/10"}

	for name, s := range map[string]preview.State{
		"preview-state/valid/canonical-list-row.json":         def,
		"preview-state/valid/canonical-list-row-tailnet.json": tailnet,
	} {
		data, err := json.Marshal(previewListRows([]preview.State{s})[0])
		if err != nil {
			t.Fatalf("marshal %s: %v", name, err)
		}
		var row map[string]any
		if err := json.Unmarshal(data, &row); err != nil {
			t.Fatalf("unmarshal %s: %v", name, err)
		}
		row["era"] = "canonical"
		row["app"] = "myapp"
		writeFixture(t, name, row)
	}
}

func TestRuntimeIdentifierSchemaParity(t *testing.T) {
	for _, hash := range []string{"v1", "v1.2.3", "1750000000", "abcdef12", "release_1", "ABC1234", strings.Repeat("a", 128)} {
		attempt, err := releasemeta.NewAttempt("demo", hash)
		if err != nil {
			t.Fatal(err)
		}
		for _, tt := range []struct {
			schema, value string
			nested        bool
		}{{"attempt-name", attempt.Name(), false}, {"release-record", hash, true}} {
			data, err := os.ReadFile(filepath.Join(contractsDir, "schema", tt.schema+".schema.json"))
			if err != nil {
				t.Fatal(err)
			}
			var schema map[string]any
			if err := json.Unmarshal(data, &schema); err != nil {
				t.Fatal(err)
			}
			pattern, _ := schema["pattern"].(string)
			if tt.nested {
				pattern = schema["properties"].(map[string]any)["hash"].(map[string]any)["pattern"].(string)
			}
			matched, err := regexp.MatchString(pattern, tt.value)
			if err != nil || !matched {
				t.Fatalf("%s rejects runtime %s: %v", tt.schema, tt.value, err)
			}
		}
	}
}

// TestContractsEventsEnvelopeGolden drives the REAL `teploy events --json`
// encoder with one started/terminal deploy pair and a key-names-only env
// event (rev 10).
func TestContractsEventsEnvelopeGolden(t *testing.T) {
	ts := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	actor := events.Actor{Client: "dash", Label: "alice", CLIVersion: "v0.0.0-contracts", Op: "op_0123456789abcdef"}
	evs := []events.Event{
		{V: 1, ID: "ev_000000000000000000000001", Seq: 1, TS: ts, App: "blog", Kind: events.KindDeploy, Outcome: events.Started, Release: "abc1234", Actor: actor, Detail: map[string]any{"image": "example/blog:abc1234"}},
		{V: 1, ID: "ev_000000000000000000000002", Seq: 2, TS: ts.Add(40 * time.Second), App: "blog", Kind: events.KindDeploy, Outcome: events.Succeeded, Release: "abc1234", Actor: actor, Detail: map[string]any{"image": "example/blog:abc1234", "duration_ms": 40000}},
		{V: 1, ID: "ev_000000000000000000000003", Seq: 3, TS: ts.Add(time.Minute), App: "blog", Kind: events.KindEnvSet, Outcome: events.Succeeded, Actor: events.Actor{Client: "cli", Label: "tyler@laptop", CLIVersion: "v0.0.0-contracts"}, Detail: map[string]any{"keys": []string{"LOG_LEVEL"}}},
	}
	var buf bytes.Buffer
	if err := writeEventsJSON(&buf, "192.0.2.10", "blog", evs); err != nil {
		t.Fatal(err)
	}
	var v any
	if err := json.Unmarshal(buf.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, "events-envelope/valid/deploy-and-env.json", v)
}

// TestContractsConfigHomeGolden pins the config-home envelopes (rev 11):
// `config get --json` for a Teploy-home app whose config moved since the
// deploy, `config home --json` for a folder app, `config history --json`,
// an app list with an undeployed Teploy-home app, and a release record
// deployed from Teploy config.
func TestContractsConfigHomeGolden(t *testing.T) {
	ts := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	head := "8f2c1d0e9b7a6f5e4d3c2b1a0f9e8d7c6b5a4f3e"
	applied := "1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d"
	writeFixture(t, "config-envelope/valid/teploy-home.json", configDTO{
		MachineInterface: MachineInterface, Host: "192.0.2.10", App: "blog", Home: "teploy",
		Commit: head, AppliedCommit: applied,
		Content: "app: blog\ndomain: blog.example.com\nimage: example/blog:1.4.0\nenv:\n  LOG_LEVEL: info\n  DB_PASSWORD: secret:DB_PASSWORD\n",
	})
	writeFixture(t, "config-envelope/valid/folder-home.json", configDTO{
		MachineInterface: MachineInterface, Host: "192.0.2.10", App: "legacy", Home: "folder",
	})
	writeFixture(t, "config-history-envelope/valid/two-commits.json", configHistoryDTO{
		MachineInterface: MachineInterface, Host: "192.0.2.10", App: "blog", AppliedCommit: applied,
		Commits: []confighome.Commit{
			{OID: head, Author: "dash:alice", Email: "dash@teploy", Date: ts, Message: "config set image"},
			{OID: applied, Author: "cli:tyler@laptop", Email: "cli@teploy", Date: ts.Add(-time.Hour), Message: "config home teploy (from teploy.yml)"},
		},
	})
	writeFixture(t, "app-list-envelope/valid/mi2-undeployed.json", appListDTO{
		MachineInterface: MachineInterface, Host: "srv.example.com", Apps: []appStatusDTO{}, ObservedAt: ts, Errors: []machineError{},
		Undeployed: []undeployedAppDTO{{App: "blog", Domain: "blog.example.com", Generation: 1, UpdatedAt: ts}},
	})
	writeFixture(t, "release-record/valid/container-teploy-home.json", releasemeta.Record{
		SchemaVersion: 1, App: "blog", Hash: "1.4.0", CreatedAt: ts,
		DeploymentType: "container", IngressMode: "caddy", Domain: "blog.example.com",
		ImageRef: "example/blog:1.4.0", ImageDigest: "sha256:0000", ManifestSHA256: "sha256:beef",
		ConfigCommit: head, Actor: "dash:alice",
	})
	writeFixture(t, "release-record/valid/container-folder-source.json", releasemeta.Record{
		SchemaVersion: 1, App: "legacy", Hash: "abc1234", CreatedAt: ts,
		DeploymentType: "container", IngressMode: "caddy", Domain: "legacy.example.com",
		ImageRef: "example/legacy:abc1234", ImageDigest: "sha256:0000", ManifestSHA256: "sha256:beef",
		SourceSHA256: "3f0a4c6e9d1b2a5f7e8c0d4b6a9f1e3c5b7d9a0e2f4c6b8d0a1c3e5f7b9d1a3c", Actor: "cli:tyler@laptop",
	})
}

// TestContractsConfigMergeGolden pins `config merge --json` (rev 13).
func TestContractsConfigMergeGolden(t *testing.T) {
	writeFixture(t, "config-merge-envelope/valid/one-conflict.json", configMergeDTO{
		MachineInterface: MachineInterface, Host: "192.0.2.10", App: "blog",
		Base:    "1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d",
		Current: "8f2c1d0e9b7a6f5e4d3c2b1a0f9e8d7c6b5a4f3e",
		Merged:  "app: blog\ndomain: blog.example.com\nenv:\n  LOG_LEVEL: debug\nimage: example/blog:1.5.0\n",
		Conflicts: []configConflict{
			{Key: "env.LOG_LEVEL", Mine: "debug", Current: "warn"},
		},
	})
}

// TestContractsBackupTargetGolden pins `server backup-target show --json`
// (rev 14, backup-target-v1). Never secret material: only whether
// credentials are stored.
func TestContractsBackupTargetGolden(t *testing.T) {
	writeFixture(t, "backup-target-envelope/valid/configured.json", backupTargetDTO{
		MachineInterface: MachineInterface, Host: "192.0.2.10",
		Configured:     true,
		Bucket:         "teploy-backups",
		Region:         "us-east-1",
		Endpoint:       "http://127.0.0.1:9100",
		Prefix:         "prod",
		HasCredentials: true,
		Path:           "/etc/teploy/backup-target.json",
	})
	writeFixture(t, "backup-target-envelope/valid/unconfigured.json", backupTargetDTO{
		MachineInterface: MachineInterface, Host: "192.0.2.10",
	})
}

// TestContractsBackupJSONGolden pins the backup-json-v1 envelopes (rev
// 14): `backup list --json` (parsed rows across volumes, accessories and
// teploy data, plus schedules discovered from teploy cron markers),
// `backup create --json` and `backup restore --json` (the quiesced
// restore receipt).
func TestContractsBackupJSONGolden(t *testing.T) {
	createdAt := "2026-10-20T03:17:04Z"
	writeFixture(t, "backup-list-envelope/valid/app.json", backupListDTO{
		MachineInterface: MachineInterface, Host: "192.0.2.10", App: "blog",
		Target: &backupTargetSummary{Bucket: "teploy-backups", Region: "us-east-1", HasCredentials: true},
		Backups: []backup.ListedBackup{
			{ID: "20261020-031704-1f2e3d4c5b6a7988", Kind: "volumes", SizeBytes: 1048576, CreatedAt: createdAt, Key: "s3://teploy-backups/blog/volumes/20261020-031704-1f2e3d4c5b6a7988.tar.gz", Restorable: true},
			{ID: "20261020-031704-1f2e3d4c5b6a7988", Kind: "teploy", SizeBytes: 4096, CreatedAt: createdAt, Key: "s3://teploy-backups/blog/teploy/20261020-031704-1f2e3d4c5b6a7988.tar.gz", Restorable: true},
			{ID: "20261019-030001", Kind: "accessory", Accessory: "db", SizeBytes: 2097152, CreatedAt: "2026-10-19T03:00:01Z", Key: "s3://teploy-backups/blog/accessories/db/20261019-030001.sql.gz", Restorable: true},
		},
		Schedules: []backupScheduleDTO{
			{Kind: "volumes", Cron: "30 3 * * *", KeepLast: 7, Form: "run"},
			{Kind: "accessory", Accessory: "db", Cron: "15 2 * * *", KeepLast: 5, MaxAgeDays: 30, Form: "run"},
		},
	})
	writeFixture(t, "backup-list-envelope/valid/legacy-schedule.json", backupListDTO{
		MachineInterface: MachineInterface, Host: "192.0.2.10", App: "legacy",
		Backups:   []backup.ListedBackup{},
		Schedules: []backupScheduleDTO{{Kind: "volumes", Cron: "0 3 * * *", KeepLast: 7, Form: "legacy"}},
	})
	writeFixture(t, "backup-create-envelope/valid/volumes.json", backupCreateDTO{
		MachineInterface: MachineInterface, Host: "192.0.2.10", App: "blog",
		ID: "20261020-031704-1f2e3d4c5b6a7988", Kind: "volumes", SizeBytes: 1048576, CreatedAt: createdAt,
		Key:       "s3://teploy-backups/blog/volumes/20261020-031704-1f2e3d4c5b6a7988.tar.gz",
		TeployKey: "s3://teploy-backups/blog/teploy/20261020-031704-1f2e3d4c5b6a7988.tar.gz", Pruned: 1,
	})
	writeFixture(t, "backup-create-envelope/valid/accessory.json", backupCreateDTO{
		MachineInterface: MachineInterface, Host: "192.0.2.10", App: "blog",
		ID: "20261020-031704-1f2e3d4c5b6a7988", Kind: "accessory", Accessory: "db", SizeBytes: 2097152, CreatedAt: createdAt,
		Key: "s3://teploy-backups/blog/accessories/db/20261020-031704-1f2e3d4c5b6a7988.sql.gz",
	})
	writeFixture(t, "backup-restore-envelope/valid/ok.json", backupRestoreDTO{
		MachineInterface: MachineInterface, Host: "192.0.2.10", App: "blog",
		ID: "20261019-030001", Kind: "volumes", OK: true,
		Stopped: []string{"blog-web-1f2e3d4", "blog-worker-1f2e3d4"},
		Started: []string{"blog-web-1f2e3d4", "blog-worker-1f2e3d4"},
		Health:  "healthy",
	})
	writeFixture(t, "backup-restore-envelope/valid/unhealthy.json", backupRestoreDTO{
		MachineInterface: MachineInterface, Host: "192.0.2.10", App: "blog",
		ID: "20261019-030001", Kind: "accessory", Accessory: "db", OK: false,
		Stopped:     []string{"blog-web-1f2e3d4"},
		Started:     []string{"blog-web-1f2e3d4"},
		Health:      "unhealthy",
		RecoveryDir: "/deployments/blog/volumes.restore-old.a1b2c3",
		Error:       "health check after restore failed: ... — pre-restore data kept in /deployments/blog/volumes.restore-old.a1b2c3",
	})
}
