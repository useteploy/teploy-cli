package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/useteploy/teploy/internal/confighome"
	"github.com/useteploy/teploy/internal/ssh"
)

// The per-app backup schedule store (D3). Cron lines call only
// `teploy backup run --app X [--accessory Y]`, so the schedule's retention
// half lives beside the app's server data and is read at run time —
// changing retention never rewrites a crontab, and a crontab holds no
// bucket or credentials.

// KindPolicy is the schedule+retention pair for one backup kind.
type KindPolicy struct {
	Cron       string `json:"cron,omitempty"`
	KeepLast   int    `json:"keep_last,omitempty"`
	MaxAgeDays int    `json:"max_age_days,omitempty"`
}

// AppPolicy is the whole per-app schedule store.
type AppPolicy struct {
	Volumes     *KindPolicy            `json:"volumes,omitempty"`
	Accessories map[string]*KindPolicy `json:"accessories,omitempty"`
}

// PolicyPath is the server path of the app's schedule store.
func PolicyPath(app string) string {
	return confighome.Dir(app) + "/backup-schedule.json"
}

// ReadPolicy loads the app's schedule store; an absent file is an empty
// policy, not an error.
func ReadPolicy(ctx context.Context, exec ssh.Executor, app string) (AppPolicy, error) {
	var p AppPolicy
	res := ssh.RunDetailed(ctx, exec, "cat "+ssh.ShellQuote(PolicyPath(app))+" 2>/dev/null || true")
	if res.Err != nil {
		return p, fmt.Errorf("reading the backup schedule store: %w", res.Err)
	}
	raw := strings.TrimSpace(string(res.Stdout))
	if raw == "" {
		return p, nil
	}
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return p, fmt.Errorf("parsing %s: %w", PolicyPath(app), err)
	}
	return p, nil
}

// WritePolicy persists the app's schedule store atomically at 0600 under
// the app's .teploy directory (created if this is the first write).
func WritePolicy(ctx context.Context, exec ssh.Executor, app string, p AppPolicy) error {
	payload, err := json.Marshal(p)
	if err != nil {
		return err
	}
	res := ssh.RunInputDetailed(ctx, exec, writePolicyScript(ssh.ShellQuote(PolicyPath(app))), strings.NewReader(string(payload)))
	if res.Err != nil {
		return fmt.Errorf("writing the backup schedule store: %w", res.Err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("writing the backup schedule store: %s", res.ExitErrorText())
	}
	return nil
}

func writePolicyScript(qPath string) string {
	return fmt.Sprintf(`d=$(dirname %s)
mkdir -p "$d" || exit 1
umask 077
t=$(mktemp "$d/.backup-schedule.XXXXXXXX") || exit 1
cat > "$t" || { rm -f -- "$t"; exit 1; }
chmod 600 "$t" || { rm -f -- "$t"; exit 1; }
mv -f -- "$t" %s || { rm -f -- "$t"; exit 1; }`, qPath, qPath)
}

// RemoveSchedule deletes the cron entry carrying marker (the counterpart
// of SetSchedule), reading the crontab with the same checked discipline.
func (c *Client) RemoveSchedule(ctx context.Context, marker string) error {
	if strings.ContainsAny(marker, "\r\n\x00") {
		return fmt.Errorf("cron marker must be a single line")
	}
	tag := "[" + marker + "]"
	cmd := fmt.Sprintf(
		`raw=$(crontab -l 2>&1); rc=$?; `+
			`if [ "$rc" -ne 0 ]; then case "$raw" in *"no crontab"*) raw="";; *) `+
			`echo "reading crontab failed: $raw" >&2; exit 1;; esac; fi; `+
			`(printf '%%s\n' "$raw" | grep -vF %s; true) | crontab -`,
		ssh.ShellQuote(tag),
	)
	if _, err := c.exec.Run(ctx, cmd); err != nil {
		return fmt.Errorf("removing cron schedule: %w", err)
	}
	return nil
}

// ReadCrontab returns the user's crontab lines (nil when none). The read
// is exit-status-checked (the TCL-46 discipline): only the canonical "no
// crontab for <user>" failure means empty; every other failure is an error.
func ReadCrontab(ctx context.Context, exec ssh.Executor) ([]string, error) {
	res := ssh.RunDetailed(ctx, exec, "crontab -l")
	if res.Err != nil {
		return nil, fmt.Errorf("reading crontab: %w", res.Err)
	}
	if res.ExitCode != 0 {
		if strings.Contains(string(res.Stderr), "no crontab") {
			return nil, nil
		}
		return nil, fmt.Errorf("reading crontab: %s", res.ExitErrorText())
	}
	var lines []string
	for _, line := range strings.Split(strings.TrimSpace(string(res.Stdout)), "\n") {
		if line = strings.TrimRight(line, "\r"); strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	return lines, nil
}
