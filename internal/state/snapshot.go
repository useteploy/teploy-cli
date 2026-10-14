package state

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/useteploy/teploy/internal/ssh"
)

// snapshotChunk is how many apps one ReadAppSnapshots command covers. The
// bundle is one shell loop; chunking bounds argv length and per-command
// output so a fleet of any size costs ceil(n/chunk)+O(1) round trips, never
// O(n) (the per-app `app list` enumeration this replaces measured ~26s on a
// 41-app server — the fleet dashboards probing it time out long before).
const snapshotChunk = 100

// CanonicalStatePath is the authoritative v2 state file for an app.
func CanonicalStatePath(app string) string {
	return fmt.Sprintf("%s/%s/state.json", deploymentsDir, app)
}

// LegacyStatePath is the pre-v2 key=value state file, read only while the
// canonical file does not exist.
func LegacyStatePath(app string) string {
	return fmt.Sprintf("%s/%s/state", deploymentsDir, app)
}

// LockInfoPath is the fenced lock's metadata file (audit F16).
func LockInfoPath(app string) string {
	return fmt.Sprintf("%s/%s/.lock/info", deploymentsDir, app)
}

// AppSnapshot is one app's observed control-plane state from a single
// batched read. Field semantics mirror the per-app readers:
//
//   - State is the parsed canonical or legacy state; nil when absent.
//   - StateErr is a per-app read/parse failure (the state scope of the
//     per-app errors `app list` used to report) — unreadable file,
//     malformed JSON, schema/digest validation, or a truncated bundle.
//     Absence is NOT an error.
//   - Lock follows ReadLock: nil when absent, empty, or unparsable —
//     it never errors.
//   - Maintenance mirrors `test -f .maintenance-block`; HasReleases
//     mirrors `test -d releases`.
type AppSnapshot struct {
	App         string
	State       *AppState
	StateErr    error
	Lock        *LockInfo
	Maintenance bool
	HasReleases bool
}

// ReadAppSnapshots fetches every app's control-plane files in a constant
// number of round trips (one command per snapshotChunk apps). It exists so
// `teploy app list` stops paying 5-6 sequential execs per app.
//
// Error discipline follows ReadRemoteFile (audit F15): a confirmed-missing
// file is recorded as absence, an unreadable file surfaces as that app's
// StateErr, and a wholesale failure (transport died, output framing not
// understood) is returned as the error — the caller cannot attribute it to
// one app and must not pretend to.
func ReadAppSnapshots(ctx context.Context, exec ssh.Executor, apps []string) ([]AppSnapshot, error) {
	if len(apps) == 0 {
		return nil, nil
	}
	byApp := make(map[string]AppSnapshot, len(apps))
	for start := 0; start < len(apps); start += snapshotChunk {
		end := start + snapshotChunk
		if end > len(apps) {
			end = len(apps)
		}
		chunk := apps[start:end]
		out, err := exec.Run(ctx, snapshotCommand(chunk))
		if err != nil {
			return nil, fmt.Errorf("reading app snapshots: %w", err)
		}
		if err := collectSnapshots(out, chunk, byApp); err != nil {
			return nil, err
		}
	}
	names := make([]string, 0, len(byApp))
	for name := range byApp {
		names = append(names, name)
	}
	sort.Strings(names)
	result := make([]AppSnapshot, 0, len(names))
	for _, name := range names {
		result = append(result, byApp[name])
	}
	return result, nil
}

// snapshotCommand builds the one-shot bundle for a chunk of validated app
// names. Output is one framing line per field, base64-encoded so JSON
// content can never collide with the framing:
//
//	A <app>                   — record boundary
//	S absent | S <rc> <b64>   — canonical state.json (rc is cat's exit status)
//	G absent | G <rc> <b64>   — legacy state, fetched only when S is absent
//	L <b64>                   — .lock/info (empty encoding = no lock)
//	M 0|1                     — .maintenance-block present
//	R 0|1                     — releases/ directory present
//
// The `[ ! -e ]` guards mirror ReadRemoteFile exactly: absence is confirmed
// by the remote test, never inferred from an empty read; a present-but-
// unreadable file keeps cat's non-zero rc so the client can tell the two
// apart. base64 output is piped through tr -d '\n' because GNU and busybox
// base64 wrap at different widths — the client must not depend on either.
func snapshotCommand(apps []string) string {
	var b strings.Builder
	b.WriteString("for app in")
	for _, app := range apps {
		b.WriteString(" ")
		b.WriteString(ssh.ShellQuote(app))
	}
	b.WriteString(`; do d="/deployments/$app"; printf 'A %s\n' "$app"; `)
	b.WriteString(`if [ ! -e "$d/state.json" ]; then printf 'S absent\n'; `)
	b.WriteString(`if [ ! -e "$d/state" ]; then printf 'G absent\n'; `)
	b.WriteString(`else g=$(cat -- "$d/state" 2>/dev/null); grc=$?; printf 'G %s %s\n' "$grc" "$(printf '%s' "$g" | base64 | tr -d '\n')"; fi; `)
	b.WriteString(`else s=$(cat -- "$d/state.json" 2>/dev/null); src=$?; printf 'S %s %s\n' "$src" "$(printf '%s' "$s" | base64 | tr -d '\n')"; printf 'G absent\n'; fi; `)
	b.WriteString(`l=$(cat "$d/.lock/info" 2>/dev/null); printf 'L %s\n' "$(printf '%s' "$l" | base64 | tr -d '\n')"; `)
	b.WriteString(`if [ -f "$d/.maintenance-block" ]; then printf 'M 1\n'; else printf 'M 0\n'; fi; `)
	b.WriteString(`if [ -d "$d/releases" ]; then printf 'R 1\n'; else printf 'R 0\n'; fi; `)
	b.WriteString("done")
	return b.String()
}

// snapshotRecord accumulates one app's framing lines until the next A.
type snapshotRecord struct {
	app          string
	stateRead    bool
	stateAbsent  bool
	stateRC      string
	stateData    string
	legacyRead   bool
	legacyAbsent bool
	legacyRC     string
	legacyData   string
	lockData     string
	maintenance  bool
	hasReleases  bool
}

func (r *snapshotRecord) complete() bool {
	return r.stateRead && r.legacyRead
}

func collectSnapshots(output string, chunk []string, byApp map[string]AppSnapshot) error {
	requested := make(map[string]bool, len(chunk))
	for _, app := range chunk {
		requested[app] = true
	}
	var current *snapshotRecord
	flush := func() {
		if current == nil {
			return
		}
		byApp[current.app] = snapshotFromRecord(current)
		current = nil
	}
	for _, raw := range strings.Split(output, "\n") {
		line := strings.TrimRight(raw, "\r")
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, " ", 3)
		switch fields[0] {
		case "A":
			if len(fields) < 2 || !requested[fields[1]] {
				return fmt.Errorf("unrecognized app snapshot framing: %q", line)
			}
			if _, dup := byApp[fields[1]]; dup {
				return fmt.Errorf("duplicate app snapshot record for %s", fields[1])
			}
			flush()
			current = &snapshotRecord{app: fields[1]}
		case "S", "G", "L", "M", "R":
			if current == nil {
				return fmt.Errorf("unrecognized app snapshot framing: %q", line)
			}
			if err := current.apply(fields[0], line); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unrecognized app snapshot framing: %q", line)
		}
	}
	flush()
	// Apps whose records were cut short (transport truncated mid-loop) or
	// never started keep their identity but never fabricated data.
	for _, app := range chunk {
		if _, ok := byApp[app]; ok {
			continue
		}
		byApp[app] = AppSnapshot{App: app, StateErr: fmt.Errorf("app snapshot for %s was truncated", app)}
	}
	return nil
}

func (r *snapshotRecord) apply(kind, line string) error {
	switch kind {
	case "S":
		if r.stateRead {
			return fmt.Errorf("duplicate S line in app snapshot: %q", line)
		}
		r.stateRead = true
		fields := strings.SplitN(line, " ", 3)
		if len(fields) >= 2 && fields[1] == "absent" {
			r.stateAbsent = true
			return nil
		}
		if len(fields) < 3 {
			return fmt.Errorf("malformed S line in app snapshot: %q", line)
		}
		r.stateRC, r.stateData = fields[1], fields[2]
	case "G":
		if r.legacyRead {
			return fmt.Errorf("duplicate G line in app snapshot: %q", line)
		}
		r.legacyRead = true
		fields := strings.SplitN(line, " ", 3)
		if len(fields) >= 2 && fields[1] == "absent" {
			r.legacyAbsent = true
			return nil
		}
		if len(fields) < 3 {
			return fmt.Errorf("malformed G line in app snapshot: %q", line)
		}
		r.legacyRC, r.legacyData = fields[1], fields[2]
	case "L":
		fields := strings.SplitN(line, " ", 2)
		if len(fields) == 2 {
			r.lockData = fields[1]
		}
	case "M":
		r.maintenance = strings.HasSuffix(line, " 1")
	case "R":
		r.hasReleases = strings.HasSuffix(line, " 1")
	}
	return nil
}

// snapshotFromRecord converts framing into an AppSnapshot using the same
// parsers as Read/ReadLock — the bundle must not fork state semantics.
func snapshotFromRecord(r *snapshotRecord) AppSnapshot {
	snap := AppSnapshot{App: r.app, Maintenance: r.maintenance, HasReleases: r.hasReleases}
	if !r.complete() {
		snap.StateErr = fmt.Errorf("app snapshot for %s was truncated", r.app)
		return snap
	}
	if lock, err := decodeSnapshotField(r.lockData); err == nil {
		snap.Lock = ParseLockInfo(string(lock))
	}
	if r.stateAbsent {
		// Legacy state is consulted only on confirmed-absent canonical
		// state (Read's rule); an unreadable legacy file is the same
		// error here that it is there.
		if !r.legacyAbsent {
			if r.legacyRC != "0" {
				snap.StateErr = fmt.Errorf("reading legacy state for %s: cat exited %s", r.app, r.legacyRC)
				return snap
			}
			data, err := decodeSnapshotField(r.legacyData)
			if err != nil {
				snap.StateErr = fmt.Errorf("reading legacy state for %s: %w", r.app, err)
				return snap
			}
			snap.State = ParseLegacy(data)
		}
		return snap
	}
	if r.stateRC != "0" {
		snap.StateErr = fmt.Errorf("reading canonical state for %s: cat exited %s", r.app, r.stateRC)
		return snap
	}
	data, err := decodeSnapshotField(r.stateData)
	if err != nil {
		snap.StateErr = fmt.Errorf("reading canonical state for %s: %w", r.app, err)
		return snap
	}
	snap.State, snap.StateErr = ParseCanonical(r.app, data)
	return snap
}

func decodeSnapshotField(encoded string) ([]byte, error) {
	if encoded == "" {
		return nil, nil
	}
	return base64.StdEncoding.DecodeString(encoded)
}

// ParseCanonical validates and parses canonical v2 state bytes exactly as
// Read does for a present file. Exported so the batched snapshot reader
// shares one parser with the single-app path instead of forking semantics.
func ParseCanonical(app string, data []byte) (*AppState, error) {
	v2Path := CanonicalStatePath(app)
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, fmt.Errorf("canonical state %s exists but is empty — refusing to treat an existing state file as absent; inspect and repair it before deploying", v2Path)
	}
	var s AppState
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parsing canonical state for %s: %w", app, err)
	}
	if s.SchemaVersion != SchemaVersionV2 {
		return nil, fmt.Errorf("unsupported state schema version %d for %s", s.SchemaVersion, app)
	}
	if err := validateManifestDigest(&s); err != nil {
		return nil, fmt.Errorf("validating canonical state for %s: %w", app, err)
	}
	if err := validateReleaseDigest(s.PreviousRelease); err != nil {
		return nil, fmt.Errorf("validating previous release for %s: %w", app, err)
	}
	normalizePorts(&s)
	return &s, nil
}

// ParseLegacy parses pre-v2 key=value state bytes (lenient, like Read:
// unknown lines are skipped; empty input yields nil).
func ParseLegacy(data []byte) *AppState {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	s := &AppState{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		switch parts[0] {
		case "current_port":
			s.CurrentPort, _ = strconv.Atoi(parts[1])
		case "current_hash":
			s.CurrentHash = parts[1]
		case "previous_port":
			s.PreviousPort, _ = strconv.Atoi(parts[1])
		case "previous_hash":
			s.PreviousHash = parts[1]
		case "domain":
			s.Domain = parts[1]
		case "current_ports":
			s.CurrentPorts = parsePorts(parts[1])
		case "previous_ports":
			s.PreviousPorts = parsePorts(parts[1])
		}
	}
	s.SchemaVersion = 1
	s.legacyImported = true
	normalizePorts(s)
	return s
}

// ParseLockInfo parses .lock/info bytes with ReadLock's forgiving contract:
// empty or unparsable output is "no lock", never an error.
func ParseLockInfo(output string) *LockInfo {
	trimmed := strings.TrimSpace(output)
	if trimmed == "" {
		return nil
	}
	var info LockInfo
	if err := json.Unmarshal([]byte(trimmed), &info); err != nil {
		return nil
	}
	return &info
}
