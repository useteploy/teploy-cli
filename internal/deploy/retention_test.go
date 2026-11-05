package deploy

// The default keep_versions notice period (decision D3): an app that never
// set keep_versions is pruned to DefaultKeepVersions only after a recorded
// 7-day notice, so `server audit` and the deploy output can warn first.

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy/internal/docker"
	"github.com/useteploy/teploy/internal/ssh"
)

func pruneDueExecutor(containers string, notice *string) *ssh.MockExecutor {
	m := ssh.NewMockExecutor("192.0.2.10",
		ssh.MockCommand{Match: "docker ps --all --filter label=teploy.app='myapp'", Output: containers},
	)
	if notice != nil {
		m.Files["/deployments/myapp/.prune-notice"] = []byte(*notice)
	}
	return m
}

func TestDefaultPruneDue(t *testing.T) {
	threeVersions := strings.Join([]string{
		`{"ID":"c1","Names":"myapp-web-v3","Image":"myapp:v3","State":"running","Status":"Up","CreatedAt":"2026-05-28 21:30:00 -0700 PDT","Labels":"teploy.app=myapp,teploy.version=v3"}`,
		`{"ID":"c2","Names":"myapp-web-v2","Image":"myapp:v2","State":"exited","Status":"Up","CreatedAt":"2026-05-28 21:00:00 -0700 PDT","Labels":"teploy.app=myapp,teploy.version=v2"}`,
		`{"ID":"c3","Names":"myapp-web-v1","Image":"myapp:v1","State":"exited","Status":"Up","CreatedAt":"2026-05-28 20:00:00 -0700 PDT","Labels":"teploy.app=myapp,teploy.version=v1"}`,
	}, "\n")

	t.Run("nothing beyond keep never starts a notice", func(t *testing.T) {
		var out bytes.Buffer
		m := pruneDueExecutor(threeVersions, nil)
		d := &Deployer{exec: m, docker: docker.NewClient(m), out: &out}
		if d.defaultPruneDue(context.Background(), "myapp", 5, nil) {
			t.Fatal("prune due with nothing beyond keep")
		}
		if _, uploaded := m.Files["/deployments/myapp/.prune-notice"]; uploaded {
			t.Error("a notice was recorded with nothing to prune")
		}
	})

	t.Run("the first finding starts the notice and waits", func(t *testing.T) {
		var out bytes.Buffer
		m := pruneDueExecutor(threeVersions, nil)
		d := &Deployer{exec: m, docker: docker.NewClient(m), out: &out}
		if d.defaultPruneDue(context.Background(), "myapp", 1, []string{"v3", "v2"}) {
			t.Fatal("prune due on the first sighting")
		}
		recorded := string(m.Files["/deployments/myapp/.prune-notice"])
		if !strings.Contains(recorded, time.Now().UTC().Format("2006-01-02")) {
			t.Errorf("notice = %q, want today's timestamp", recorded)
		}
		if !strings.Contains(out.String(), "remove them automatically") && !strings.Contains(out.String(), "remove them") {
			t.Errorf("deploy output must warn before the first automatic prune: %q", out.String())
		}
		if !strings.Contains(out.String(), "set keep_versions") {
			t.Errorf("the warning must say how to opt out: %q", out.String())
		}
	})

	t.Run("a notice inside the window waits and names the date", func(t *testing.T) {
		var out bytes.Buffer
		threeDaysAgo := time.Now().UTC().Add(-3 * 24 * time.Hour).Format(time.RFC3339)
		m := pruneDueExecutor(threeVersions, &threeDaysAgo)
		d := &Deployer{exec: m, docker: docker.NewClient(m), out: &out}
		if d.defaultPruneDue(context.Background(), "myapp", 1, []string{"v3", "v2"}) {
			t.Fatal("prune due three days into a seven-day notice")
		}
		if !strings.Contains(out.String(), time.Now().UTC().Add(4*24*time.Hour).Format("2006-01-02")) {
			t.Errorf("the warning should name when pruning starts: %q", out.String())
		}
	})

	t.Run("an elapsed notice allows the prune", func(t *testing.T) {
		var out bytes.Buffer
		eightDaysAgo := time.Now().UTC().Add(-8 * 24 * time.Hour).Format(time.RFC3339)
		m := pruneDueExecutor(threeVersions, &eightDaysAgo)
		d := &Deployer{exec: m, docker: docker.NewClient(m), out: &out}
		if !d.defaultPruneDue(context.Background(), "myapp", 1, []string{"v3", "v2"}) {
			t.Fatal("prune still refused after the notice elapsed")
		}
	})

	t.Run("an unreadable notice never prunes", func(t *testing.T) {
		var out bytes.Buffer
		m := ssh.NewMockExecutor("192.0.2.10",
			ssh.MockCommand{Match: "docker ps --all --filter label=teploy.app='myapp'", Output: threeVersions},
			ssh.MockCommand{Match: "if [ ! -e '/deployments/myapp/.prune-notice'", Err: errors.New("stale file handle")},
		)
		d := &Deployer{exec: m, docker: docker.NewClient(m), out: &out}
		if d.defaultPruneDue(context.Background(), "myapp", 1, []string{"v3", "v2"}) {
			t.Fatal("a notice that cannot be read must not authorize a prune")
		}
		if !strings.Contains(out.String(), "skipped") {
			t.Errorf("the refusal must be visible: %q", out.String())
		}
	})
}

func TestParsePruneNotice(t *testing.T) {
	if ts, ok := ParsePruneNotice([]byte("2026-11-01T00:00:00Z\n")); !ok || ts.UTC().Format(time.DateOnly) != "2026-11-01" {
		t.Errorf("ParsePruneNotice = %v, %v", ts, ok)
	}
	if _, ok := ParsePruneNotice([]byte("yesterday")); ok {
		t.Error("a non-timestamp notice must not parse")
	}
}
