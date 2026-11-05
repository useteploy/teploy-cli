package backup

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/ssh"
)

func TestParseSchedulerProbe(t *testing.T) {
	st, err := parseSchedulerProbe("daemon=no\nenabled=unknown\ncrontab=no\nteploy_entries=0\ncrond_teploy=teploy-wp\ncrond_other=replicate\n")
	if err != nil {
		t.Fatal(err)
	}
	if st.Daemon || st.Crontab || st.Enabled != "unknown" || !st.HasJobs() || st.CronDTeploy[0] != "teploy-wp" || st.CronDOther[0] != "replicate" {
		t.Fatalf("parsed %+v", st)
	}
	if _, err := parseSchedulerProbe("sh: 1: syntax error"); err == nil {
		t.Fatal("garbage output must not read as 'no daemon'")
	}
}

// `backup schedule` refuses on a host without a running cron daemon (and on
// an unreadable probe): the crontab line would be written and never run.
func TestRequireScheduler(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, out string
		err       error
		wantSub   string
	}{
		{name: "not installed", out: "daemon=no\nenabled=unknown\ncrontab=no\nteploy_entries=0\n", wantSub: "cron is not installed"},
		{name: "installed, stopped", out: "daemon=no\nenabled=disabled\ncrontab=yes\nteploy_entries=0\n", wantSub: "no cron daemon is running"},
		{name: "probe failed", err: errors.New("exit status 2: sh: syntax error"), wantSub: "refusing to install a schedule"},
		{name: "running", out: "daemon=yes\nenabled=enabled\ncrontab=yes\nteploy_entries=0\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mock := ssh.NewMockExecutor("h", ssh.MockCommand{Match: "sched_daemon=no", Output: tc.out, Err: tc.err})
			err := RequireScheduler(ctx, mock)
			if tc.wantSub == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("err = %v, want %q", err, tc.wantSub)
			}
			if tc.err == nil && !strings.Contains(err.Error(), "teploy setup") {
				t.Errorf("the refusal must carry the remedy: %v", err)
			}
		})
	}
}

// The probe is POSIX sh (Debian's /bin/sh is dash) and always reports a
// daemon line; on a host without /proc it reads as no daemon.
func TestSchedulerProbeRunsUnderSh(t *testing.T) {
	out, err := exec.Command("sh", "-c", SchedulerProbe).CombinedOutput()
	if err != nil {
		t.Fatalf("probe failed under sh: %v\n%s", err, out)
	}
	if _, err := parseSchedulerProbe(string(out)); err != nil {
		t.Fatal(err)
	}
}

// `backup list --json` on a cron-less host: no crontab binary means nothing is
// scheduled, not a failure. Any other failure still is one.
func TestReadCrontabWithoutCron(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.Symlink("/bin/sh", dir+"/sh"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	if lines, err := ReadCrontab(ctx, ssh.NewLocalExecutor()); err != nil || lines != nil {
		t.Fatalf("no crontab binary = %v, %v", lines, err)
	}
}
