package backup

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/useteploy/teploy/internal/ssh"
)

// Scheduler presence. Backup and trigger schedules are crontab lines; a
// server with no running cron daemon never runs any of them, silently.
// deploy-ovh had teploy-style backup jobs in /etc/cron.d for 6.5 months
// with cron never installed (2026-11-03).

// SchedulerStatus is what the scheduler probe found on a server.
type SchedulerStatus struct {
	// Daemon reports a cron or crond process running.
	Daemon bool
	// Enabled is the systemd unit state (enabled, disabled, masked, ...)
	// or "unknown" when there is no systemd or no cron unit.
	Enabled string
	// Crontab reports the crontab binary.
	Crontab bool
	// CrontabEntries counts the user's crontab schedule lines;
	// TeployEntries those that are teploy's (backup markers, the teploy
	// binary, a /deployments script such as the trigger-poll redeploy).
	CrontabEntries, TeployEntries int
	// CronDTeploy and CronDOther list /etc/cron.d files that do / do not
	// mention teploy (package jobs that ship a systemd-timer twin are
	// left out: they run without cron).
	CronDTeploy, CronDOther []string
}

// HasJobs reports whether anything is scheduled that only cron would run.
func (s SchedulerStatus) HasJobs() bool {
	return s.CrontabEntries > 0 || s.TeployEntries > 0 || len(s.CronDTeploy) > 0 || len(s.CronDOther) > 0
}

// HasTeployJobs reports whether any of those jobs are teploy's.
func (s SchedulerStatus) HasTeployJobs() bool {
	return s.TeployEntries > 0 || len(s.CronDTeploy) > 0
}

// SchedulerProbe is the read-only probe script. The daemon check scans
// /proc/*/comm with the shell's own read (no procps or systemd needed, so
// it works on minimal images and in containers).
const SchedulerProbe = `sched_daemon=no; for f in /proc/[0-9]*/comm; do { read -r c < "$f"; } 2>/dev/null || continue; case "$c" in cron|crond) sched_daemon=yes; break;; esac; done; ` +
	// /proc hides other users' processes under hidepid=2, and systemd-cron
	// runs jobs without a daemon process: ask systemd too.
	`if [ "$sched_daemon" = no ] && command -v systemctl >/dev/null 2>&1; then for u in cron crond cronie cron.target; do systemctl is-active --quiet "$u" 2>/dev/null && { sched_daemon=yes; break; }; done; fi; echo "daemon=$sched_daemon"; ` +
	`e=unknown; if command -v systemctl >/dev/null 2>&1; then for u in cron crond cronie cron.target; do s=$(systemctl is-enabled "$u" 2>/dev/null); case "$s" in enabled|disabled|masked|static|indirect|alias) e=$s; break;; esac; done; fi; echo "enabled=$e"; ` +
	// Schedule lines only (VAR=value and comment lines are not jobs);
	// teploy's are its backup markers, its binary, or a /deployments script.
	`if command -v crontab >/dev/null 2>&1; then echo crontab=yes; t=$(crontab -l 2>/dev/null | grep '^[[:space:]]*[0-9*@]'); n=$(printf '%s\n' "$t" | grep -c .); m=$(printf '%s\n' "$t" | grep -c -e teploy -e /deployments/); echo "crontab_entries=${n:-0}"; echo "teploy_entries=${m:-0}"; else echo crontab=no; fi; ` +
	`for f in /etc/cron.d/*; do [ -f "$f" ] || continue; b=${f##*/}; case "$b" in .*|e2scrub_all|php|sysstat) continue;; esac; if grep -qi teploy "$f" 2>/dev/null; then echo "crond_teploy=$b"; else echo "crond_other=$b"; fi; done; true`

// ProbeScheduler runs SchedulerProbe.
func ProbeScheduler(ctx context.Context, exec ssh.Executor) (SchedulerStatus, error) {
	out, err := exec.Run(ctx, SchedulerProbe)
	if err != nil {
		return SchedulerStatus{}, fmt.Errorf("probing the cron scheduler: %w", err)
	}
	return parseSchedulerProbe(out)
}

func parseSchedulerProbe(out string) (SchedulerStatus, error) {
	st := SchedulerStatus{Enabled: "unknown"}
	seen := false
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch k {
		case "daemon":
			st.Daemon, seen = v == "yes", true
		case "enabled":
			st.Enabled = v
		case "crontab":
			st.Crontab = v == "yes"
		case "crontab_entries":
			st.CrontabEntries, _ = strconv.Atoi(v)
		case "teploy_entries":
			st.TeployEntries, _ = strconv.Atoi(v)
		case "crond_teploy":
			st.CronDTeploy = append(st.CronDTeploy, v)
		case "crond_other":
			st.CronDOther = append(st.CronDOther, v)
		}
	}
	if !seen {
		return st, fmt.Errorf("probing the cron scheduler: unrecognized output %q", strings.TrimSpace(out))
	}
	return st, nil
}

// schedulerRemedy explains how to give a server a running scheduler.
const schedulerRemedy = "run `teploy setup` against the server (it installs and enables cron), or on the server: apt-get install cron && systemctl enable --now cron"

// RequireScheduler refuses to install a schedule on a server whose cron
// daemon is not running: the crontab line would be written and never run.
func RequireScheduler(ctx context.Context, exec ssh.Executor) error {
	st, err := ProbeScheduler(ctx, exec)
	if err != nil {
		return fmt.Errorf("%w — refusing to install a schedule that might never run", err)
	}
	if !st.Daemon {
		missing := "no cron daemon is running"
		if !st.Crontab {
			missing = "cron is not installed"
		}
		return fmt.Errorf("%s on this server, so a scheduled backup would never run — %s", missing, schedulerRemedy)
	}
	return nil
}

// SchedulerRemedy is the operator remedy for a missing cron daemon.
func SchedulerRemedy() string { return schedulerRemedy }
