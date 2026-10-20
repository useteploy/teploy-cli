package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy/internal/releasemeta"
	"github.com/useteploy/teploy/internal/ssh"
	"github.com/useteploy/teploy/internal/state"
)

// metaListOutput frames records the way the server-side listing prints
// them (see releasemeta.listScript).
func metaListOutput(t *testing.T, recs ...releasemeta.Record) string {
	t.Helper()
	var sb strings.Builder
	for i := range recs {
		data, err := json.Marshal(recs[i])
		if err != nil {
			t.Fatal(err)
		}
		sb.WriteString("\n__TEPLOY_META__" + recs[i].Hash + ".json\n" + string(data) + "\n")
	}
	return sb.String()
}

func containersOutput(version, image, process string) string {
	return `{"ID":"a","Names":"x","Image":"` + image + `","State":"running","Status":"up","CreatedAt":"2026-10-20 12:00:00 +0000 UTC","Labels":{"teploy.version":"` + version + `","teploy.process":"` + process + `"}}`
}

func TestReleasemetaList(t *testing.T) {
	ts := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	newer := releasemeta.Record{SchemaVersion: 1, App: "blog", Hash: "new1", CreatedAt: ts, DeploymentType: "container", ImageRef: "nginx:2"}
	older := releasemeta.Record{SchemaVersion: 1, App: "blog", Hash: "old1", CreatedAt: ts.Add(-time.Hour), DeploymentType: "container", ImageRef: "nginx:1"}
	broken := releasemeta.Record{SchemaVersion: 99, App: "blog", Hash: "bad1", CreatedAt: ts.Add(-2 * time.Hour), DeploymentType: "container"}
	foreign := releasemeta.Record{SchemaVersion: 1, App: "other", Hash: "oth1", CreatedAt: ts.Add(-3 * time.Hour), DeploymentType: "container"}

	var out strings.Builder
	out.WriteString(metaListOutput(t, broken, newer, older, foreign))
	// A malformed entry: identity says one thing, bytes say another.
	out.WriteString("\n__TEPLOY_META__zzz.json\n{not json}\n")

	mock := ssh.NewMockExecutor("h",
		ssh.MockCommand{Match: "d='/deployments/blog/meta'", Output: out.String()},
	)
	recs, warns, err := releasemeta.List(context.Background(), mock, "blog")
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 || recs[0].Hash != "new1" || recs[1].Hash != "old1" {
		t.Fatalf("records = %+v, want newest-first new1, old1", recs)
	}
	if len(warns) != 3 {
		t.Fatalf("warns = %v, want the unparsable, wrong-schema and foreign records skipped", warns)
	}

	// A missing directory is an empty list, not an error.
	mock = ssh.NewMockExecutor("h", ssh.MockCommand{Match: "d='/deployments/blog/meta'", Output: ""})
	if recs, warns, err = releasemeta.List(context.Background(), mock, "blog"); err != nil || len(recs) != 0 || len(warns) != 0 {
		t.Fatalf("empty store: recs=%v warns=%v err=%v", recs, warns, err)
	}

	// The transport trims the leading newline: the first marker can start
	// the output with no blank line before it. Both records must parse.
	trimmed := strings.TrimLeft(out.String(), "\n")
	mock = ssh.NewMockExecutor("h", ssh.MockCommand{Match: "d='/deployments/blog/meta'", Output: trimmed})
	if recs, warns, err = releasemeta.List(context.Background(), mock, "blog"); err != nil || len(recs) != 2 || len(warns) != 3 {
		t.Fatalf("trimmed-leading-newline output: recs=%d warns=%v err=%v", len(recs), warns, err)
	}
	if recs[0].Hash != "new1" || recs[1].Hash != "old1" {
		t.Fatalf("records = %+v, want newest-first new1, old1", recs)
	}
}

func TestContainerReleaseRows(t *testing.T) {
	ts := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	current := releasemeta.Record{SchemaVersion: 1, App: "blog", Hash: "cur1", CreatedAt: ts, DeploymentType: "container", ImageRef: "nginx:2", ImageDigest: "sha256:cur", ConfigCommit: "abc", Actor: "dash:alice"}
	prev := releasemeta.Record{SchemaVersion: 1, App: "blog", Hash: "prv1", CreatedAt: ts.Add(-time.Hour), DeploymentType: "container", ImageRef: "nginx:1", ImageDigest: "sha256:prv"}
	pruned := releasemeta.Record{SchemaVersion: 1, App: "blog", Hash: "pru1", CreatedAt: ts.Add(-2 * time.Hour), DeploymentType: "container", ImageRef: "nginx:0"}

	metaOut := metaListOutput(t, pruned, current, prev)
	containers := strings.Join([]string{
		containersOutput("cur1", "nginx:2", "web"),
		containersOutput("prv1", "nginx:1", "web"),
		containersOutput("prv1", "nginx:1", "worker"),
		containersOutput("orphan1", "nginx:9", "web"),
	}, "\n")
	mock := ssh.NewMockExecutor("h",
		ssh.MockCommand{Match: "d='/deployments/blog/meta'", Output: metaOut},
		ssh.MockCommand{Match: "docker ps --all --filter label=teploy.app='blog'", Output: containers},
	)
	st := &state.AppState{CurrentHash: "cur1", DeploymentType: "container"}
	rows, err := containerReleaseRows(context.Background(), mock, "blog", st)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 {
		t.Fatalf("rows = %+v, want 4 (3 records + 1 unrecorded)", rows)
	}
	byRelease := map[string]releaseRowDTO{}
	for _, r := range rows {
		byRelease[r.Release] = r
	}
	if r := byRelease["cur1"]; !r.Current || r.RollbackAvailable || r.Reason != "current release" || r.Image != "nginx:2" || r.ConfigCommit != "abc" || r.Actor != "dash:alice" || r.ImageDigest != "sha256:cur" {
		t.Fatalf("current row = %+v", r)
	}
	if r := byRelease["prv1"]; r.Current || !r.RollbackAvailable || r.Reason != "" {
		t.Fatalf("previous row = %+v", r)
	}
	if r := byRelease["pru1"]; r.RollbackAvailable || !strings.Contains(r.Reason, "keep_versions") {
		t.Fatalf("pruned row = %+v", r)
	}
	if r := byRelease["orphan1"]; r.Current || !r.RollbackAvailable || r.Image != "nginx:9" {
		t.Fatalf("unrecorded retained version must be listed and rollback-able: %+v", r)
	}
	if rows[0].Release != "cur1" || rows[1].Release != "prv1" {
		t.Fatalf("rows must be newest first: %+v", rows)
	}
}

func TestReleasesJSONEnvelope(t *testing.T) {
	ts := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	rows := []releaseRowDTO{
		{Release: "cur1", DeployedAt: ts, Image: "nginx:2", Current: true, Reason: "current release"},
		{Release: "prv1", DeployedAt: ts.Add(-time.Hour), Image: "nginx:1", RollbackAvailable: true},
	}
	var raw strings.Builder
	if err := json.NewEncoder(&raw).Encode(releasesDTO{
		MachineInterface: MachineInterface, Host: "192.0.2.10", App: "blog",
		DeploymentType: "container", Current: "cur1", Releases: rows,
	}); err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(raw.String()), &doc); err != nil {
		t.Fatal(err)
	}
	if doc["machine_interface"] != float64(MachineInterface) || doc["deployment_type"] != "container" || doc["current"] != "cur1" {
		t.Fatalf("envelope = %v", doc)
	}
	rels, ok := doc["releases"].([]any)
	if !ok || len(rels) != 2 {
		t.Fatalf("releases = %v", doc["releases"])
	}
	first, _ := rels[0].(map[string]any)
	if first["release"] != "cur1" || first["current"] != true || first["rollback_available"] != false || first["reason"] != "current release" {
		t.Fatalf("first row = %v", first)
	}
}

func TestWriteContainerReleaseTable(t *testing.T) {
	var buf strings.Builder
	if err := writeContainerReleaseTable(&buf, []releaseRowDTO{
		{Release: "cur1", Current: true},
		{Release: "prv1", RollbackAvailable: true},
		{Release: "pru1", Reason: "no containers retained for this version (keep_versions retention)"},
	}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"* cur1", "current", "prv1", "rollback available", "pru1", "keep_versions"} {
		if !strings.Contains(out, want) {
			t.Errorf("table missing %q:\n%s", want, out)
		}
	}
	buf.Reset()
	if err := writeContainerReleaseTable(&buf, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "No releases yet") {
		t.Fatalf("empty table: %q", buf.String())
	}
}
