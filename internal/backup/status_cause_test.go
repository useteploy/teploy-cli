package backup

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy/internal/events"
	"github.com/useteploy/teploy/internal/ssh"
)

// Not reachable always says why: this is what a consumer shows after "cannot reach".
func TestFleetStatusSaysWhyTheTargetWasNotReached(t *testing.T) {
	base := []ssh.MockCommand{
		{Match: "if [ -d /etc/teploy ]", Output: "absent\n"},
		{Match: "for p in /etc/teploy/backup-target.json", Output: "path /etc/teploy/backup-target.json\n" + `{"v":"1","bucket":"b","region":"r"}`},
		{Match: "crontab -l", Output: ""},
		{Match: "ids=$(docker ps -aq", Output: ""},
		{Match: "for d in /deployments/*/volumes", Output: ""},
		{Match: "for d in '/var/tmp'/teploy-backup.", Output: ""},
		{Match: "python3 -c", Output: ""},
	}
	now := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		extra ssh.MockCommand
		want  string
	}{
		{"no aws", ssh.MockCommand{Match: "command -v aws >/dev/null", Output: "age\n"}, "aws CLI is not installed"},
		{"could not check", ssh.MockCommand{Match: "command -v aws >/dev/null", Err: errors.New("ssh: connection reset")}, "could not check"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, err := ReadFleetStatus(context.Background(), ssh.NewMockExecutor("h", append(base, tc.extra)...), now)
			if err != nil {
				t.Fatal(err)
			}
			if st.Target.Reachable || !strings.Contains(st.Target.Error, tc.want) {
				t.Fatalf("%+v", st.Target)
			}
		})
	}
	// aws ran and said nothing: still a reason.
	cmds := append(append([]ssh.MockCommand{}, base...),
		ssh.MockCommand{Match: "command -v aws >/dev/null", Output: "aws aws-cli/2.30.0\n"},
		ssh.MockCommand{Match: "aws s3 ls", Err: errors.New("exit status 2")},
	)
	mock := ssh.NewMockExecutor("h", cmds...)
	st, err := ReadFleetStatus(context.Background(), mock, now)
	if err != nil {
		t.Fatal(err)
	}
	if st.Target.Reachable || strings.HasSuffix(st.Target.Error, ": ") || st.Target.Error == "" {
		t.Fatalf("%+v", st.Target)
	}
}

// A detached run is `backup run --all`: what makes its events count for coverage is
// policy_managed, and it is what a policy entry requires.
func TestPolicyManagedRunCountsForCoverage(t *testing.T) {
	at := time.Date(2026, 11, 6, 1, 0, 0, 0, time.UTC)
	ev := func(managed bool) events.Event {
		d := map[string]any{"accessory": "db", "kind": KindAccessory}
		if managed {
			d["policy_managed"] = true
		}
		return events.Event{TS: at, Kind: events.KindBackupRun, Outcome: events.Succeeded, Detail: d}
	}
	e := StatusEntry{Schedule: "policy"}
	fillEntryEvents(&e, []events.Event{ev(false)}, "db")
	if e.LastSuccess != nil {
		t.Fatal("a manual run must not count")
	}
	fillEntryEvents(&e, []events.Event{ev(true)}, "db")
	if e.LastSuccess == nil || !e.LastSuccess.Equal(at) {
		t.Fatalf("%+v", e)
	}
}
