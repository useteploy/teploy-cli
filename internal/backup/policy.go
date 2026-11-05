package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/useteploy/teploy/internal/ssh"
)

// The server backup policy (G4): /etc/teploy/backup-policy.json, read at run
// time by the one cron line `teploy backup run --all`, so a database deployed
// tomorrow is covered with no command. Mode 0600: heartbeat URLs carry tokens.
// A non-root user that cannot write /etc/teploy falls back to
// ~/.config/teploy/backup-policy.json, like the backup target.

// Policy paths.
const (
	PolicyFileEtc  = "/etc/teploy/backup-policy.json"
	PolicyFileHome = ".config/teploy/backup-policy.json"
)

// FleetRetention is the policy's retention half (see RetentionPolicy).
type FleetRetention struct {
	KeepLast    int `json:"keep_last,omitempty"`
	KeepDaily   int `json:"keep_daily,omitempty"`
	KeepWeekly  int `json:"keep_weekly,omitempty"`
	KeepMonthly int `json:"keep_monthly,omitempty"`
	MaxAgeDays  int `json:"max_age_days,omitempty"`
}

// Policy converts to the prune selection's form.
func (r FleetRetention) Policy() RetentionPolicy {
	return RetentionPolicy{KeepLast: r.KeepLast, KeepDaily: r.KeepDaily, KeepWeekly: r.KeepWeekly, KeepMonthly: r.KeepMonthly, MaxAgeDays: r.MaxAgeDays}
}

// DatabaseSelector is an adopted entry's databases: "all", or a list of names.
type DatabaseSelector struct {
	All   bool
	Names []string
}

// MarshalJSON renders "all" or the list.
func (d DatabaseSelector) MarshalJSON() ([]byte, error) {
	if d.All {
		return json.Marshal("all")
	}
	if d.Names == nil {
		return json.Marshal([]string{})
	}
	return json.Marshal(d.Names)
}

// UnmarshalJSON accepts "all" or a list of names.
func (d *DatabaseSelector) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		if s != "all" {
			return fmt.Errorf(`databases must be "all" or a list of names (got %q)`, s)
		}
		*d = DatabaseSelector{All: true}
		return nil
	}
	var names []string
	if err := json.Unmarshal(b, &names); err != nil {
		return fmt.Errorf(`databases must be "all" or a list of names`)
	}
	*d = DatabaseSelector{Names: names}
	return nil
}

// AdoptedEntry brings a container with no teploy labels under the policy.
type AdoptedEntry struct {
	Container string           `json:"container"`
	Engine    string           `json:"engine,omitempty"`
	Databases DatabaseSelector `json:"databases"`
}

// FleetPolicy is the server's backup policy.
type FleetPolicy struct {
	V string `json:"v"`
	// Interval is how often `backup run --all` fires. Empty: no cron line (the
	// policy still configures encryption, retention and manual runs).
	Interval  string         `json:"interval,omitempty"`
	Retention FleetRetention `json:"retention"`
	// Excludes are targets the run leaves out, visibly: "<app>" (everything of
	// it), "<app>/<accessory>", "<app>/volumes", or a container name.
	Excludes []string       `json:"excludes,omitempty"`
	Adopted  []AdoptedEntry `json:"adopted,omitempty"`
	// Volumes turns on app volume archives (each one a full copy).
	Volumes bool `json:"volumes"`
	// VolumeMaxGB caps one archive: a bigger tree is warned about and excluded.
	VolumeMaxGB float64 `json:"volume_max_gb,omitempty"`
	// HeartbeatURLs are pinged after every run. They carry tokens: never logged,
	// never shown back.
	HeartbeatURLs []string `json:"heartbeat_urls,omitempty"`
	// Recipient is the owner's recovery public key (`teploy backup keygen`).
	// With one, every object is encrypted to it and the server's own identity.
	Recipient string `json:"recipient,omitempty"`

	// Path is where the policy was read from (diagnostics only).
	Path string `json:"-"`
}

// Configured reports whether the server has a policy at all.
func (p FleetPolicy) Configured() bool { return p.V != "" }

// Validate checks everything the run later interpolates or depends on. It is
// also the gate on what reaches the server's disk.
func (p FleetPolicy) Validate() error {
	if p.Interval != "" {
		if _, err := IntervalCron(p.Interval, "x"); err != nil {
			return err
		}
	}
	r := p.Retention
	if r.KeepLast < 0 || r.KeepDaily < 0 || r.KeepWeekly < 0 || r.KeepMonthly < 0 || r.MaxAgeDays < 0 {
		return fmt.Errorf("retention values must be >= 0")
	}
	if p.VolumeMaxGB < 0 {
		return fmt.Errorf("volume_max_gb must be >= 0")
	}
	if p.Recipient != "" {
		if err := ValidateRecipient(p.Recipient); err != nil {
			return err
		}
	}
	for _, e := range p.Excludes {
		if err := validateExclude(e); err != nil {
			return err
		}
	}
	seen := map[string]bool{}
	for _, a := range p.Adopted {
		if err := ValidateContainerName(a.Container); err != nil {
			return err
		}
		if seen[a.Container] {
			return fmt.Errorf("container %s is adopted twice", a.Container)
		}
		seen[a.Container] = true
		if a.Engine != "" && (!knownEngines[a.Engine] || a.Engine == EngineGeneric || a.Engine == EngineNucleus) {
			return fmt.Errorf("adopted %s: engine %q cannot be adopted (postgres, mysql, mariadb, redis, valkey, dragonfly, mongo)", a.Container, a.Engine)
		}
		if !a.Databases.All && len(a.Databases.Names) == 0 {
			return fmt.Errorf(`adopted %s: databases must be "all" or at least one name`, a.Container)
		}
		for _, db := range a.Databases.Names {
			if !addressableDB(db) {
				return fmt.Errorf("adopted %s: database name %q cannot be addressed safely", a.Container, db)
			}
		}
	}
	for _, u := range p.HeartbeatURLs {
		if err := validateHeartbeatURL(u); err != nil {
			return err
		}
	}
	return nil
}

var containerNameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,254}$`)

// ValidateContainerName applies docker's own name grammar.
func ValidateContainerName(name string) error {
	if !containerNameRE.MatchString(name) {
		return fmt.Errorf("invalid container name %q", name)
	}
	return nil
}

func validateExclude(e string) error {
	for _, part := range strings.Split(e, "/") {
		if !containerNameRE.MatchString(part) {
			return fmt.Errorf("invalid exclude %q: expected <app>, <app>/<accessory>, <app>/volumes or a container name", e)
		}
	}
	if strings.Count(e, "/") > 1 {
		return fmt.Errorf("invalid exclude %q: expected <app>, <app>/<accessory>, <app>/volumes or a container name", e)
	}
	return nil
}

func validateHeartbeatURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		// Never the value: the URL is a secret and this reaches logs.
		return fmt.Errorf("heartbeat URLs must be http(s) URLs")
	}
	return nil
}

// Excluded reports whether the policy leaves a target out.
func (p FleetPolicy) Excluded(app, name, container string) bool {
	for _, e := range p.Excludes {
		switch {
		case e == container && container != "":
			return true
		case app != "" && app != AdoptedApp && e == app:
			return true
		case app != "" && e == app+"/"+name:
			return true
		}
	}
	return false
}

var intervalMinutes = map[string]int{
	"15m": 15, "30m": 30, "1h": 60, "hourly": 60, "2h": 120, "3h": 180, "4h": 240,
	"6h": 360, "8h": 480, "12h": 720, "24h": 1440, "1d": 1440, "daily": 1440,
	"7d": 10080, "1w": 10080, "weekly": 10080,
}

// IntervalDuration is the policy interval as a duration (0 when unset).
func (p FleetPolicy) IntervalDuration() time.Duration {
	return time.Duration(intervalMinutes[p.Interval]) * time.Minute
}

// IntervalCron renders an interval as five cron fields. seed (the server's
// identity) picks the minute, so a fleet does not hit its bucket in one second.
func IntervalCron(interval, seed string) (string, error) {
	mins, ok := intervalMinutes[interval]
	if !ok {
		return "", fmt.Errorf("invalid interval %q: use 15m, 30m, 1h, 2h, 3h, 4h, 6h, 8h, 12h, 24h, 7d (or hourly, daily, weekly)", interval)
	}
	h := fnv.New32a()
	h.Write([]byte(seed))
	v := h.Sum32()
	m := int(v % 60)
	hour := int((v >> 8) % 6) // the small hours
	switch {
	case mins < 60:
		return fmt.Sprintf("%d-59/%d * * * *", m%mins, mins), nil
	case mins == 60:
		return fmt.Sprintf("%d * * * *", m), nil
	case mins < 1440:
		return fmt.Sprintf("%d %d-23/%d * * *", m, hour%(mins/60), mins/60), nil
	case mins == 1440:
		return fmt.Sprintf("%d %d * * *", m, hour), nil
	}
	return fmt.Sprintf("%d %d * * 0", m, hour), nil
}

// readPolicyScript prints "absent" or "path <p>\n<contents>". A file that exists
// and cannot be read is a failure, not an absence: a non-root CLI user reading
// "no policy" from a root-owned file would upload plaintext.
const readPolicyScript = `for p in /etc/teploy/backup-policy.json "$HOME"/.config/teploy/backup-policy.json; do
if [ -e "$p" ] && [ ! -r "$p" ]; then echo "cannot read $p: run as its owner" >&2; exit 3; fi
if [ -r "$p" ]; then printf 'path %s\n' "$p"; cat "$p"; exit 0; fi
done
echo absent`

// ReadFleetPolicy loads the server's policy. No policy is (FleetPolicy{}, nil).
func ReadFleetPolicy(ctx context.Context, exec ssh.Executor) (FleetPolicy, error) {
	res := ssh.RunDetailed(ctx, exec, readPolicyScript)
	if res.Err != nil {
		return FleetPolicy{}, fmt.Errorf("reading the backup policy: %w", res.Err)
	}
	if res.ExitCode != 0 {
		return FleetPolicy{}, fmt.Errorf("reading the backup policy: %s", res.ExitErrorText())
	}
	out := strings.TrimSpace(string(res.Stdout))
	if out == "absent" {
		return FleetPolicy{}, nil
	}
	line, rest, _ := strings.Cut(out, "\n")
	if !strings.HasPrefix(line, "path ") {
		return FleetPolicy{}, fmt.Errorf("reading the backup policy: unrecognized output framing")
	}
	var p FleetPolicy
	p.Path = strings.TrimSpace(strings.TrimPrefix(line, "path "))
	if err := json.Unmarshal([]byte(rest), &p); err != nil {
		return FleetPolicy{}, fmt.Errorf("parsing %s: %w", p.Path, err)
	}
	if p.V != "1" {
		return FleetPolicy{}, fmt.Errorf("%s: unsupported backup policy version %q", p.Path, p.V)
	}
	return p, nil
}

const writeFleetPolicyScript = `d=
if [ "$(id -u)" = 0 ]; then
  mkdir -p /etc/teploy && chmod 700 /etc/teploy || exit 1
  d=/etc/teploy/backup-policy.json
elif [ -w /etc/teploy ] 2>/dev/null; then
  d=/etc/teploy/backup-policy.json
else
  dir=${XDG_CONFIG_HOME:-$HOME/.config}/teploy
  mkdir -p "$dir" && chmod 700 "$dir" || exit 1
  d=$dir/backup-policy.json
  echo fallback >&2
fi
umask 077
t=$(mktemp "$(dirname "$d")/.backup-policy.XXXXXXXX") || exit 1
cat > "$t" || { rm -f -- "$t"; exit 1; }
chmod 600 "$t" || { rm -f -- "$t"; exit 1; }
mv -f -- "$t" "$d" || { rm -f -- "$t"; exit 1; }
printf 'written %s\n' "$d"`

// WriteFleetPolicy stores the policy atomically at 0600 and returns the path and
// whether the non-root fallback was used.
func WriteFleetPolicy(ctx context.Context, exec ssh.Executor, p FleetPolicy) (string, bool, error) {
	if err := p.Validate(); err != nil {
		return "", false, err
	}
	p.V = "1"
	payload, err := json.Marshal(p)
	if err != nil {
		return "", false, err
	}
	res := ssh.RunInputDetailed(ctx, exec, writeFleetPolicyScript, strings.NewReader(string(payload)))
	if res.Err != nil {
		return "", false, fmt.Errorf("writing the backup policy: %w", res.Err)
	}
	if res.ExitCode != 0 {
		return "", false, fmt.Errorf("writing the backup policy: %s", res.ExitErrorText())
	}
	f := strings.Fields(strings.TrimSpace(string(res.Stdout)))
	if len(f) != 2 || f[0] != "written" {
		return "", false, fmt.Errorf("writing the backup policy: unrecognized output framing")
	}
	return f[1], strings.Contains(string(res.Stderr), "fallback"), nil
}

const unsetFleetPolicyScript = `for p in /etc/teploy/backup-policy.json "$HOME"/.config/teploy/backup-policy.json; do
if [ -e "$p" ]; then rm -f -- "$p" || exit 1; echo "removed $p"; fi
done`

// UnsetFleetPolicy removes the policy file(s), returning what it removed.
func UnsetFleetPolicy(ctx context.Context, exec ssh.Executor) ([]string, error) {
	res := ssh.RunDetailed(ctx, exec, unsetFleetPolicyScript)
	if res.Err != nil {
		return nil, fmt.Errorf("removing the backup policy: %w", res.Err)
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("removing the backup policy: %s", res.ExitErrorText())
	}
	var removed []string
	for _, line := range splitLines(string(res.Stdout)) {
		if v, ok := strings.CutPrefix(line, "removed "); ok {
			removed = append(removed, v)
		}
	}
	return removed, nil
}

// ApplyPolicyEncryption reads the policy and arms cfg with its recovery key, so
// every object written with cfg is encrypted. An unreadable policy fails closed:
// better no backup now than a plaintext one on a server configured to encrypt.
func ApplyPolicyEncryption(ctx context.Context, exec ssh.Executor, cfg *S3Config) error {
	p, err := ReadFleetPolicy(ctx, exec)
	if err != nil {
		return err
	}
	cfg.OwnerRecipient = p.Recipient
	return nil
}

// MaskedHeartbeats renders heartbeat URLs without their tokens: scheme and host.
func MaskedHeartbeats(urls []string) []string {
	out := []string{}
	for _, raw := range urls {
		u, err := url.Parse(raw)
		if err != nil {
			out = append(out, "(unparseable)")
			continue
		}
		out = append(out, u.Scheme+"://"+u.Host+"/...")
	}
	return out
}
