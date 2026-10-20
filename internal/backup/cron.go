package backup

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Cron-marker discovery (D3): teploy-managed backup cron lines end in a
// bracketed marker comment; their schedule, form and baked-in retention
// are parsed for `backup list --json`, `backup-target set`'s rewrite and
// `teploy doctor`.

const (
	CronFormRun    = "run"    // `<bin> backup run --app X [--accessory Y]`
	CronFormLegacy = "legacy" // pre-D3 generated forms (bucket/creds embedded)
	CronFormCustom = "custom" // hand-written command under a teploy marker
)

// CronMarker is one parsed teploy backup cron line.
type CronMarker struct {
	Kind       string // volumes | accessory
	App        string
	Accessory  string
	Cron       string // the five schedule fields
	Form       string
	Tag        string // the bare marker (teploy-backup:app or teploy-accessory-backup:app:name)
	KeepLast   int    // legacy lines only
	MaxAgeDays int
	Command    string
}

var cronMarkerRE = regexp.MustCompile(`\[(teploy-backup:[A-Za-z0-9._-]+|teploy-accessory-backup:[A-Za-z0-9._-]+:[A-Za-z0-9._-]+)\]\s*$`)

var legacyKeepLastRE = regexp.MustCompile(`head -n -([0-9]+)`)

// FindCronMarker parses one crontab line; nil when the line carries no
// teploy backup marker.
func FindCronMarker(line string) *CronMarker {
	m := cronMarkerRE.FindStringSubmatch(line)
	if m == nil {
		return nil
	}
	tag := m[1]
	fields := strings.Fields(line)
	if len(fields) < 6 {
		return nil
	}
	mk := &CronMarker{Tag: tag, Cron: strings.Join(fields[:5], " "), Command: strings.Join(fields[5:], " ")}
	if rest, ok := strings.CutPrefix(tag, "teploy-accessory-backup:"); ok {
		app, acc, found := strings.Cut(rest, ":")
		if !found {
			return nil
		}
		mk.Kind, mk.App, mk.Accessory = KindAccessory, app, acc
	} else {
		mk.Kind, mk.App = KindVolumes, strings.TrimPrefix(tag, "teploy-backup:")
	}
	switch {
	case strings.Contains(mk.Command, " backup run --app "):
		mk.Form = CronFormRun
	case isLegacyGeneratedBackupCmd(mk.Command):
		mk.Form = CronFormLegacy
		if kb := legacyKeepLastRE.FindStringSubmatch(mk.Command); kb != nil {
			mk.KeepLast = atoiSafe(kb[1])
		}
	default:
		mk.Form = CronFormCustom
	}
	return mk
}

// isLegacyGeneratedBackupCmd recognizes the command shapes this CLI
// generated before D3: the volumes shell chain (tar + aws), the accessory
// invocation, or anything carrying a --bucket flag. A hand-written script
// under a teploy marker (the nucleus backup pattern) matches none of
// these and is reported as custom instead of rewritten.
func isLegacyGeneratedBackupCmd(cmd string) bool {
	return strings.Contains(cmd, "aws s3 cp") ||
		strings.Contains(cmd, " accessory backup ") ||
		strings.Contains(cmd, " --bucket ")
}

// IsRecoveryIncomplete reports whether err (or anything it wraps) is the
// restore failure class whose recovery artifacts are intentionally kept
// on the server — callers use it to decide whether restarting the app
// after a failed restore would serve data of unknown integrity.
func IsRecoveryIncomplete(err error) bool {
	return errors.Is(err, errRecoveryIncomplete)
}

// ParseIDTimestamp renders the timestamp part of a backup id as RFC3339
// UTC.
func ParseIDTimestamp(stamp string) (string, error) {
	t, err := time.Parse("20060102-150405", stamp)
	if err != nil {
		return "", fmt.Errorf("invalid backup timestamp %q", stamp)
	}
	return t.UTC().Format(time.RFC3339), nil
}

// EnsureAWSCLIPublic is the exported form of the aws-CLI presence check.
func (c *Client) EnsureAWSCLIPublic(ctx context.Context) error {
	return c.ensureAWSCLI(ctx)
}
