package state

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/ssh"
)

func snapB64(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}

func canonicalFixture() string {
	return `{"schema_version":2,"deployment_type":"container","ingress_mode":"caddy","domain":"blog.example.com","current_hash":"v2","current_ports":[49153],"previous_hash":"v1","previous_ports":[49152]}`
}

func TestReadAppSnapshotsCanonicalAndLock(t *testing.T) {
	lockJSON := `{"type":"manual","user":"alice","ts":"2026-07-22T11:00:00Z"}`
	mock := ssh.NewMockExecutor("h",
		ssh.MockCommand{Match: "for app in", Output: strings.Join([]string{
			"A blog",
			"S 0 " + snapB64(canonicalFixture()),
			"G absent",
			"L " + snapB64(lockJSON),
			"M 1",
			"R 0",
		}, "\n")},
	)
	snaps, err := ReadAppSnapshots(context.Background(), mock, []string{"blog"})
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 1 || snaps[0].App != "blog" {
		t.Fatalf("snapshots = %#v", snaps)
	}
	snap := snaps[0]
	if snap.StateErr != nil || snap.State == nil || snap.State.Domain != "blog.example.com" || snap.State.CurrentHash != "v2" {
		t.Fatalf("state wrong: %#v err=%v", snap.State, snap.StateErr)
	}
	if snap.Lock == nil || snap.Lock.User != "alice" {
		t.Fatalf("lock wrong: %#v", snap.Lock)
	}
	if !snap.Maintenance || snap.HasReleases {
		t.Fatalf("flags wrong: maintenance=%v releases=%v", snap.Maintenance, snap.HasReleases)
	}
}

func TestReadAppSnapshotsLegacyFallback(t *testing.T) {
	mock := ssh.NewMockExecutor("h",
		ssh.MockCommand{Match: "for app in", Output: strings.Join([]string{
			"A old",
			"S absent",
			"G 0 " + snapB64("current_hash=l9\ncurrent_port=49160\ndomain=old.example.com\n"),
			"L ",
			"M 0",
			"R 1",
		}, "\n")},
	)
	snaps, err := ReadAppSnapshots(context.Background(), mock, []string{"old"})
	if err != nil {
		t.Fatal(err)
	}
	snap := snaps[0]
	if snap.StateErr != nil || snap.State == nil {
		t.Fatalf("legacy state not parsed: %#v err=%v", snap.State, snap.StateErr)
	}
	if snap.State.SchemaVersion != 1 || snap.State.CurrentHash != "l9" || snap.State.CurrentPorts[0] != 49160 {
		t.Fatalf("legacy parse wrong: %#v", snap.State)
	}
}

// F15 discipline through the bundle: an existing-but-empty canonical file
// is an error, never absence; an unreadable file keeps cat's rc; confirmed
// absence is silence.
func TestReadAppSnapshotsF15Discipline(t *testing.T) {
	mock := ssh.NewMockExecutor("h",
		ssh.MockCommand{Match: "for app in", Output: strings.Join([]string{
			"A empty",
			"S 0 ",
			"G absent",
			"L ",
			"M 0",
			"R 0",
			"A unreadable",
			"S 1 ",
			"G absent",
			"L ",
			"M 0",
			"R 0",
			"A clean",
			"S absent",
			"G absent",
			"L ",
			"M 0",
			"R 0",
		}, "\n")},
	)
	snaps, err := ReadAppSnapshots(context.Background(), mock, []string{"clean", "empty", "unreadable"})
	if err != nil {
		t.Fatal(err)
	}
	byApp := map[string]AppSnapshot{}
	for _, snap := range snaps {
		byApp[snap.App] = snap
	}
	if snap := byApp["clean"]; snap.StateErr != nil || snap.State != nil {
		t.Fatalf("confirmed absence must be silent: %#v err=%v", snap.State, snap.StateErr)
	}
	if snap := byApp["empty"]; snap.StateErr == nil || !strings.Contains(snap.StateErr.Error(), "exists but is empty") {
		t.Fatalf("empty canonical file must error: %v", snap.StateErr)
	}
	if snap := byApp["unreadable"]; snap.StateErr == nil || !strings.Contains(snap.StateErr.Error(), "cat exited 1") {
		t.Fatalf("unreadable canonical file must error: %v", snap.StateErr)
	}
}

// A truncated bundle (transport died mid-loop) keeps the apps it finished
// and marks the rest, instead of failing the whole read.
func TestReadAppSnapshotsTruncatedRecord(t *testing.T) {
	mock := ssh.NewMockExecutor("h",
		ssh.MockCommand{Match: "for app in", Output: strings.Join([]string{
			"A done",
			"S absent",
			"G absent",
			"L ",
			"M 0",
			"R 0",
			"A cut",
			"S 0 " + snapB64(canonicalFixture()),
		}, "\n")},
	)
	snaps, err := ReadAppSnapshots(context.Background(), mock, []string{"cut", "done", "never-started"})
	if err != nil {
		t.Fatal(err)
	}
	byApp := map[string]AppSnapshot{}
	for _, snap := range snaps {
		byApp[snap.App] = snap
	}
	if snap := byApp["done"]; snap.StateErr != nil {
		t.Fatalf("completed record contaminated: %v", snap.StateErr)
	}
	for _, app := range []string{"cut", "never-started"} {
		if snap := byApp[app]; snap.StateErr == nil || !strings.Contains(snap.StateErr.Error(), "truncated") {
			t.Fatalf("%s not marked truncated: %#v", app, snap)
		}
	}
}

func TestReadAppSnapshotsWholesaleFailures(t *testing.T) {
	transport := ssh.NewMockExecutor("h",
		ssh.MockCommand{Match: "for app in", Err: errors.New("connection reset")},
	)
	if _, err := ReadAppSnapshots(context.Background(), transport, []string{"blog"}); err == nil || !strings.Contains(err.Error(), "connection reset") {
		t.Fatalf("transport failure not surfaced: %v", err)
	}

	framing := ssh.NewMockExecutor("h",
		ssh.MockCommand{Match: "for app in", Output: "X what is this\n"},
	)
	if _, err := ReadAppSnapshots(context.Background(), framing, []string{"blog"}); err == nil || !strings.Contains(err.Error(), "framing") {
		t.Fatalf("framing failure not surfaced: %v", err)
	}

	empty, err := ReadAppSnapshots(context.Background(), nil, nil)
	if err != nil || empty != nil {
		t.Fatalf("empty request must be a no-op: %#v %v", empty, err)
	}
}

// The generated command must quote every app name and probe the exact
// paths Read/ReadLock use — the bundle is only a batching of those reads.
func TestSnapshotCommandShape(t *testing.T) {
	cmd := snapshotCommand([]string{"blog", "web-2"})
	for _, want := range []string{
		"for app in 'blog' 'web-2'",
		`d="/deployments/$app"`,
		`[ ! -e "$d/state.json" ]`,
		`[ ! -e "$d/state" ]`,
		`cat -- "$d/state.json"`,
		`"$d/.lock/info"`,
		`[ -f "$d/.maintenance-block" ]`,
		`[ -d "$d/releases" ]`,
		"base64",
		"tr -d '\\n'",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("snapshot command missing %q:\n%s", want, cmd)
		}
	}
}

// countingExecutor records Run calls and answers everything with empty
// output, so chunking can be asserted without per-chunk fixtures.
type countingExecutor struct {
	ssh.Executor
	calls int
}

func (c *countingExecutor) Run(ctx context.Context, cmd string) (string, error) {
	c.calls++
	return "", nil
}

func TestReadAppSnapshotsChunksAtHundred(t *testing.T) {
	apps := make([]string, 0, 205)
	for i := 0; i < 205; i++ {
		apps = append(apps, fmt.Sprintf("app-%03d", i))
	}
	exec := &countingExecutor{}
	snaps, err := ReadAppSnapshots(context.Background(), exec, apps)
	if err != nil {
		t.Fatal(err)
	}
	if exec.calls != 3 {
		t.Fatalf("205 apps took %d commands, want 3 (chunks of 100)", exec.calls)
	}
	if len(snaps) != 205 {
		t.Fatalf("snapshots = %d, want 205", len(snaps))
	}
	for _, snap := range snaps {
		if snap.StateErr == nil {
			t.Fatalf("empty-output chunk must mark apps truncated, got clean %#v", snap)
		}
	}
}
